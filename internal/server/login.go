package server

// Logging Claude Code in again from the portal (docs/decisions/0010-relogin.md).
//
// Claude Code keeps one login per machine (~/.claude/.credentials.json, the
// keychain on macOS). When it stops working a turn ends with "Login expired
// · Please run /login" — the StopFailure hook with error
// "authentication_failed", and an API-error entry in the transcript. The
// collector flags those sessions (model.LoginReason) and asks `claude auth
// status` about the machine's login every loginCheckEvery.
//
// The fix is one login per machine: running claudes re-read the credentials
// file when it changes. POST /api/login/start runs `claude auth login`
// here, which prints a sign-in URL and reads the code from stdin; the
// portal shows the URL (GET /api/login/flow), the operator signs in on any
// browser and pastes the code back (POST /api/login/code), which goes to
// that process's stdin. Afterwards the hosted sessions stopped on the error
// get a short "go on" prompt (typeBlocked rules, like a message); the
// others keep a mark telling the operator to send one.
//
// The URL and the code never reach a log, the DB or a notification: the
// URL lives on the flow in memory until the flow ends, the code only passes
// through to the process. The routes are a person's (hubAllowed: attach on,
// never a peer).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/attach"
	"github.com/TakumaNakagame/ccdash/internal/discovery"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/redact"
)

const (
	loginCheckEvery = 10 * time.Minute
	loginRecheckGap = time.Minute      // a new mark asks again, at most this often
	loginFlowMax    = 10 * time.Minute // a sign-in nobody finishes is dropped
	loginKeepEnded  = 10 * time.Minute // an ended flow stays visible this long
	loginNudgeMax   = 5 * time.Minute  // a hosted session that stays busy keeps a mark instead
	loginMarkWindow = 24 * time.Hour   // older login errors in transcripts are left alone
	loginWaitURL    = 30 * time.Second
	loginWaitDone   = 30 * time.Second

	keyLoginOKAt    = "login_ok_at"   // Unix s of the last login ccdash saw
	keyLoginExpires = "login_expires" // RFC 3339: the expiry `claude auth status` last said
)

// newAuthCmd builds `claude auth …`. A variable so tests can swap it.
var newAuthCmd = attach.ViaShell

type loginRunner struct {
	mu        sync.Mutex
	checked   bool
	loggedIn  bool
	expiresAt time.Time
	checkedAt time.Time
	checkErr  string
	lastOK    time.Time
	lastAsk   time.Time
	flow      *loginProc
	nudge     map[string]time.Time // session id → give up at
	recheck   chan struct{}
}

type loginProc struct {
	info   model.LoginFlow
	stdin  io.WriteCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	// changed is closed and replaced on every state change, so handlers
	// can wait for "URL known" / "ended".
	changed chan struct{}
	tail    []string // the process's last lines, URLs removed (error text)
}

func (l *loginRunner) init() {
	if l.recheck == nil {
		l.recheck = make(chan struct{}, 1)
		l.nudge = map[string]time.Time{}
	}
}

// askLoginCheck asks loginLoop for a `claude auth status` soon.
func (s *Server) askLoginCheck() {
	s.login.mu.Lock()
	s.login.init()
	ch := s.login.recheck
	s.login.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// loginLoop keeps the machine's login state and hands the "go on" prompts.
func (s *Server) loginLoop(ctx context.Context) {
	s.login.mu.Lock()
	s.login.init()
	ch := s.login.recheck
	s.login.mu.Unlock()
	if v, _ := s.db.GetSetting(ctx, keyLoginOKAt); v != "" {
		var sec int64
		if _, err := fmt.Sscan(v, &sec); err == nil {
			s.login.mu.Lock()
			s.login.lastOK = time.Unix(sec, 0)
			s.login.mu.Unlock()
		}
	}
	s.checkLogin(ctx)
	check := time.NewTicker(loginCheckEvery)
	defer check.Stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.cancelLogin("")
			return
		case <-check.C:
			s.checkLogin(ctx)
		case <-ch:
			s.login.mu.Lock()
			wait := loginRecheckGap - time.Since(s.login.lastAsk)
			s.login.mu.Unlock()
			if wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			s.checkLogin(ctx)
		case <-tick.C:
			s.nudgeSessions(ctx)
		}
	}
}

// authStatus is what `claude auth status --json` says (only the fields
// ccdash reads).
type authStatus struct {
	LoggedIn       bool   `json:"loggedIn"`
	LoginExpiresAt string `json:"loginExpiresAt"`
	Email          string `json:"email"`
}

var runAuthStatus = func(ctx context.Context) (authStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := newAuthCmd("auth", "status", "--json")
	cmd.Env = attach.SafeEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = os.TempDir()
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := runKilling(ctx, cmd)
	// Not logged in exits non-zero but still prints the JSON.
	st, perr := parseAuthStatus(out.String())
	if perr != nil {
		if err != nil {
			return st, err
		}
		return st, perr
	}
	return st, nil
}

// parseAuthStatus finds the JSON object in the output (a login shell's rc
// files may print around it).
func parseAuthStatus(out string) (authStatus, error) {
	var st authStatus
	i := strings.Index(out, `"loggedIn"`)
	if i < 0 {
		return st, errors.New("claude auth status: no status in the output")
	}
	j := strings.LastIndex(out[:i], "{")
	if j < 0 {
		return st, errors.New("claude auth status: no status in the output")
	}
	if err := json.NewDecoder(strings.NewReader(out[j:])).Decode(&st); err != nil {
		return st, fmt.Errorf("claude auth status: %w", err)
	}
	return st, nil
}

// runKilling runs cmd and kills its process group when ctx ends.
func runKilling(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// checkLogin asks `claude auth status`. A later expiry than the last one
// seen means somebody logged in (a /login inside a session, a terminal):
// that counts like a login through the portal.
func (s *Server) checkLogin(ctx context.Context) {
	s.login.mu.Lock()
	s.login.lastAsk = time.Now()
	s.login.mu.Unlock()
	st, err := runAuthStatus(ctx)
	if ctx.Err() != nil {
		return
	}
	var exp time.Time
	if st.LoginExpiresAt != "" {
		exp, _ = time.Parse(time.RFC3339Nano, st.LoginExpiresAt)
	}
	s.login.mu.Lock()
	s.login.checkedAt = time.Now()
	if err != nil {
		s.login.checkErr = truncate(redact.String(err.Error()), 200)
		s.login.mu.Unlock()
		return
	}
	s.login.checked, s.login.checkErr = true, ""
	s.login.loggedIn, s.login.expiresAt = st.LoggedIn, exp
	s.login.mu.Unlock()

	prev, _ := s.db.GetSetting(ctx, keyLoginExpires)
	if !exp.IsZero() {
		_ = s.db.SetSetting(ctx, keyLoginExpires, exp.UTC().Format(time.RFC3339))
	}
	if p, perr := time.Parse(time.RFC3339, prev); perr == nil && st.LoggedIn && exp.After(p.Add(time.Minute)) {
		log.Printf("login: the Claude Code login was renewed")
		s.loggedInAgain(ctx, nil)
	}
}

// noteLoginFailure flags a session whose turn ended on the login error at
// `at` (zero = now: the StopFailure hook). Errors from before the last
// login, or older than loginMarkWindow, are history.
func (s *Server) noteLoginFailure(ctx context.Context, sid string, at time.Time) {
	s.login.mu.Lock()
	lastOK := s.login.lastOK
	s.login.mu.Unlock()
	if !at.IsZero() && (!at.After(lastOK) || time.Since(at) > loginMarkWindow) {
		return
	}
	changed, err := s.db.MarkLoginExpired(ctx, sid)
	if err != nil {
		log.Printf("login mark %s: %v", sid, err)
		return
	}
	if changed {
		log.Printf("login: session %s stopped on the login error", sid)
		s.askLoginCheck()
	}
}

// loginState is GET /api/login.
func (s *Server) loginState(ctx context.Context) model.LoginState {
	out := model.LoginState{Sessions: []model.LoginSession{}}
	ids, err := s.db.LoginExpiredIDs(ctx)
	if err != nil {
		log.Printf("login state: %v", err)
	}
	s.ptyMu.Lock()
	hosted := map[string]bool{}
	for _, id := range ids {
		if e := s.ptyMap[id]; e != nil && e.sess.Alive() {
			hosted[id] = true
		}
	}
	s.ptyMu.Unlock()
	for _, id := range ids {
		x, ok, err := s.db.GetSession(ctx, id)
		if err != nil || !ok {
			continue
		}
		out.Sessions = append(out.Sessions, model.LoginSession{SessionID: id, Num: x.Num, Title: truncate(x.DisplayTitle(), 80), Hosted: hosted[id]})
	}
	s.login.mu.Lock()
	defer s.login.mu.Unlock()
	out.ExpiresAt, out.CheckedAt, out.CheckErr = s.login.expiresAt, s.login.checkedAt, s.login.checkErr
	now := time.Now()
	out.Expired = len(out.Sessions) > 0 ||
		(s.login.checked && (!s.login.loggedIn || (!s.login.expiresAt.IsZero() && now.After(s.login.expiresAt))))
	out.Soon = !out.Expired && !s.login.expiresAt.IsZero() && s.login.expiresAt.Sub(now) < model.LoginSoon
	if f := s.login.flow; f != nil {
		info := f.info
		info.URL = ""
		if info.EndedAt.IsZero() || now.Sub(info.EndedAt) < loginKeepEnded {
			out.Flow = &info
		}
	}
	return out
}

func (s *Server) handleLoginState(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.loginState(r.Context()))
}

// handleLoginFlow is GET /api/login/flow: the current sign-in with its URL
// (while it waits for the code).
func (s *Server) handleLoginFlow(w http.ResponseWriter, r *http.Request) {
	s.login.mu.Lock()
	defer s.login.mu.Unlock()
	if s.login.flow == nil {
		writeOK(w, map[string]any{"flow": nil})
		return
	}
	writeOK(w, map[string]any{"flow": s.flowView(s.login.flow)})
}

// flowView is the flow as a person sees it; the URL only while it waits.
// Call with login.mu held.
func (s *Server) flowView(f *loginProc) model.LoginFlow {
	info := f.info
	if info.State != model.LoginURL {
		info.URL = ""
	}
	return info
}

// handleLoginStart is POST /api/login/start: (re)start `claude auth login`
// and answer once its URL is known (or it ended).
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	s.cancelLogin("")
	id := randomID()
	ctx, cancel := context.WithTimeout(context.Background(), loginFlowMax)
	cmd := newAuthCmd("auth", "login", "--claudeai")
	cmd.Env = append(attach.SafeEnv(), "BROWSER=true") // nothing to open on this machine
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		http.Error(w, "claude auth login: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		http.Error(w, "claude auth login: "+err.Error(), http.StatusBadGateway)
		return
	}
	p := &loginProc{
		info:    model.LoginFlow{ID: id, State: model.LoginStarting, StartedAt: time.Now()},
		stdin:   stdin,
		cmd:     cmd,
		cancel:  cancel,
		changed: make(chan struct{}),
	}
	s.login.mu.Lock()
	s.login.init()
	s.login.flow = p
	s.login.mu.Unlock()
	log.Printf("login: sign-in started")

	go s.readLoginOutput(p, pr)
	go func() {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-done
		}
		_ = pw.Close()
		s.loginEnded(p, err, ctx.Err())
		cancel()
	}()

	s.waitFlow(r.Context(), p, loginWaitURL, func(f model.LoginFlow) bool { return f.State != model.LoginStarting })
	s.login.mu.Lock()
	v := s.flowView(p)
	s.login.mu.Unlock()
	writeOK(w, v)
}

// loginURL picks the sign-in URL out of `claude auth login`'s output.
var loginURL = regexp.MustCompile(`https://[^\s"'<>]*oauth[^\s"'<>]*`)

func (s *Server) readLoginOutput(p *loginProc, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(ansiStrip(sc.Text()))
		// The code prompt has no newline: it leads the next line.
		line = strings.TrimSpace(strings.TrimPrefix(line, "Paste code here if prompted >"))
		if line == "" {
			continue
		}
		s.login.mu.Lock()
		if u := loginURL.FindString(line); u != "" && p.info.URL == "" {
			p.info.URL = u
			if p.info.State == model.LoginStarting {
				p.info.State = model.LoginURL
			}
			p.signal()
		} else if !loginURL.MatchString(line) {
			p.tail = append(p.tail, truncate(redact.String(line), 200))
			if len(p.tail) > 6 {
				p.tail = p.tail[len(p.tail)-6:]
			}
		}
		s.login.mu.Unlock()
	}
	_, _ = io.Copy(io.Discard, r)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

func ansiStrip(s string) string { return ansiRe.ReplaceAllString(s, "") }

// signal wakes the waiters. Call with login.mu held.
func (p *loginProc) signal() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// waitFlow waits (up to d) until ok(flow) or the flow ended.
func (s *Server) waitFlow(ctx context.Context, p *loginProc, d time.Duration, ok func(model.LoginFlow) bool) {
	deadline := time.After(d)
	for {
		s.login.mu.Lock()
		info, ch := p.info, p.changed
		s.login.mu.Unlock()
		if ok(info) || !info.EndedAt.IsZero() {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// loginEnded records how `claude auth login` ended and, on success, checks
// the login and tells the stopped sessions to go on.
func (s *Server) loginEnded(p *loginProc, err, ctxErr error) {
	s.login.mu.Lock()
	switch {
	case p.info.State == model.LoginCancelled:
	case err == nil:
		p.info.State = model.LoginExchanging
	case ctxErr != nil:
		p.info.State, p.info.Error = model.LoginFailed, "時間切れ（10 分）"
	default:
		p.info.State = model.LoginFailed
		p.info.Error = "ログインできませんでした"
		if len(p.tail) > 0 { // Claude Code's own words ("Couldn't log in: …")
			p.info.Error = p.tail[len(p.tail)-1]
		}
	}
	p.info.URL = ""
	ok := p.info.State == model.LoginExchanging
	if !ok {
		p.info.EndedAt = time.Now()
	}
	p.signal()
	s.login.mu.Unlock()
	if !ok {
		log.Printf("login: sign-in ended (%s)", p.info.State)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, serr := runAuthStatus(ctx)
	if serr == nil && st.LoggedIn {
		// The new expiry isn't a login "from outside" for checkLogin.
		if exp, perr := time.Parse(time.RFC3339Nano, st.LoginExpiresAt); perr == nil {
			_ = s.db.SetSetting(ctx, keyLoginExpires, exp.UTC().Format(time.RFC3339))
			s.login.mu.Lock()
			s.login.expiresAt = exp
			s.login.mu.Unlock()
		}
	}
	s.login.mu.Lock()
	s.login.checkedAt = time.Now()
	if serr == nil {
		s.login.checked, s.login.loggedIn, s.login.checkErr = true, st.LoggedIn, ""
	}
	s.login.mu.Unlock()
	var resumed, left []string
	if serr == nil && st.LoggedIn {
		resumed, left = s.loggedInAgain(ctx, p)
	}
	s.login.mu.Lock()
	p.info.EndedAt = time.Now()
	if serr != nil || !st.LoggedIn {
		p.info.State, p.info.Error = model.LoginFailed, "ログインの処理は終わりましたが、ログインできていません"
	} else {
		p.info.State, p.info.Account = model.LoginDone, st.Email
		p.info.Resumed, p.info.Left = resumed, left
	}
	p.signal()
	s.login.mu.Unlock()
	log.Printf("login: sign-in ended (%s)", p.info.State)
}

// loggedInAgain is what follows a new login: the marks go, hosted sessions
// stopped on the error get the "go on" prompt (nudgeSessions), the others a
// mark telling the operator to send one. Returns the refs of each.
func (s *Server) loggedInAgain(ctx context.Context, _ *loginProc) (resumed, left []string) {
	now := time.Now()
	s.login.mu.Lock()
	s.login.lastOK = now
	s.login.mu.Unlock()
	_ = s.db.SetSetting(ctx, keyLoginOKAt, fmt.Sprint(now.Unix()))
	ids, err := s.db.LoginExpiredIDs(ctx)
	if err != nil {
		log.Printf("login: %v", err)
		return nil, nil
	}
	for _, id := range ids {
		x, ok, _ := s.db.GetSession(ctx, id)
		if !ok {
			continue
		}
		s.ptyMu.Lock()
		e := s.ptyMap[id]
		s.ptyMu.Unlock()
		if e != nil && e.sess.Alive() {
			_ = s.db.ClearLoginMark(ctx, id, "")
			s.login.mu.Lock()
			s.login.nudge[id] = now.Add(loginNudgeMax)
			s.login.mu.Unlock()
			resumed = append(resumed, x.Ref())
			continue
		}
		_ = s.db.ClearLoginMark(ctx, id, model.LoginDoneReason)
		left = append(left, x.Ref())
	}
	return resumed, left
}

// nudgeSessions types the "go on" prompt into each hosted session waiting
// for one, under the same rules as a message (typeBlocked), and only while
// its transcript still ends on the login error — a session that already
// went on (a /login inside it retries the turn by itself) gets nothing.
func (s *Server) nudgeSessions(ctx context.Context) {
	s.login.mu.Lock()
	ids := make(map[string]time.Time, len(s.login.nudge))
	for id, until := range s.login.nudge {
		ids[id] = until
	}
	s.login.mu.Unlock()
	now := time.Now()
	for id, until := range ids {
		done := func() {
			s.login.mu.Lock()
			delete(s.login.nudge, id)
			s.login.mu.Unlock()
		}
		x, ok, err := s.db.GetSession(ctx, id)
		if err != nil || !ok {
			done()
			continue
		}
		if x.TranscriptPath != "" && discovery.AuthFailedAt(x.TranscriptPath).IsZero() {
			done()
			continue
		}
		s.ptyMu.Lock()
		e := s.ptyMap[id]
		s.ptyMu.Unlock()
		if e == nil || !e.sess.Alive() || now.After(until) {
			_ = s.db.SetAttention(ctx, id, model.AttentionNeedsYou, model.LoginDoneReason)
			done()
			continue
		}
		s.msgMu.Lock()
		why := s.typeBlocked(x, e, now)
		if why == "" && e.typeInto("", model.LoginNudge, false, true) {
			s.msgLast[id] = now
			log.Printf("login: told %s to go on", id)
			s.msgMu.Unlock()
			done()
			continue
		}
		s.msgMu.Unlock()
	}
}

// loginCode is what a person pastes back: the code the sign-in page shows
// (letters, digits and a few separators). Anything else is refused before
// it reaches the process.
var loginCode = regexp.MustCompile(`^[A-Za-z0-9._~#=+/-]{8,512}$`)

// handleLoginCode is POST /api/login/code {"id","code"}: hand the code to
// the waiting `claude auth login`, and answer once it ended (or later).
func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(body.Code)
	if !loginCode.MatchString(code) {
		http.Error(w, "that doesn't look like a sign-in code", http.StatusBadRequest)
		return
	}
	s.login.mu.Lock()
	p := s.login.flow
	if p == nil || p.info.ID != body.ID || p.info.State != model.LoginURL {
		s.login.mu.Unlock()
		http.Error(w, "no sign-in waits for a code (start again)", http.StatusConflict)
		return
	}
	p.info.State = model.LoginExchanging
	p.info.URL = ""
	p.signal()
	stdin := p.stdin
	s.login.mu.Unlock()
	if _, err := io.WriteString(stdin, code+"\n"); err != nil {
		http.Error(w, "the sign-in process is gone (start again)", http.StatusConflict)
		return
	}
	s.waitFlow(r.Context(), p, loginWaitDone, func(f model.LoginFlow) bool { return f.State != model.LoginExchanging })
	s.login.mu.Lock()
	v := s.flowView(p)
	s.login.mu.Unlock()
	writeOK(w, v)
}

// handleLoginCancel is POST /api/login/cancel.
func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.cancelLogin(model.LoginCancelled)
	writeOK(w, nil)
}

// cancelLogin stops a running sign-in; state "" leaves the record as it
// ends (a restart replaces it anyway).
func (s *Server) cancelLogin(state string) {
	s.login.mu.Lock()
	p := s.login.flow
	if p == nil || !p.info.EndedAt.IsZero() {
		s.login.mu.Unlock()
		return
	}
	if state == "" {
		state = model.LoginCancelled
	}
	p.info.State, p.info.URL = state, ""
	p.signal()
	s.login.mu.Unlock()
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	p.cancel()
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
