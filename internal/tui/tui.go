package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/takumanakagame/ccmanage/internal/usage"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/accounts"
	"github.com/takumanakagame/ccmanage/internal/attach"
	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/selfupdate"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/skills"
	"github.com/takumanakagame/ccmanage/internal/store"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// ServerMode describes how the ccdash daemon was acquired for this session.
type ServerMode string

const (
	// ServerModeExisting means a server was already listening when the TUI started.
	ServerModeExisting ServerMode = "existing"
	// ServerModeSpawned means the TUI started the daemon itself on this launch.
	ServerModeSpawned ServerMode = "spawned"
	// ServerModeRemote means the Store talks HTTP to a collector on another
	// host (--remote); there is no local daemon at all.
	ServerModeRemote ServerMode = "remote"
)

// RemoteInfo carries what the TUI needs to know when its Store is backed by
// a remote collector rather than the local DB/filesystem: whether remote
// mode is on at all, and the ssh target used for attach / new-session
// spawns. The server-side PTY machinery reached via /pty/* only serves the
// LOCAL daemon's clients today, so remote mode goes through `ssh -t`
// instead — see attachRemote / spawnNewSessionRemote.
type RemoteInfo struct {
	Enabled   bool
	SSHTarget string
}

func Run(ctx context.Context, st store.Store, lockGroup string, srvMode ServerMode, remote RemoteInfo) error {
	m := newModel(ctx, st, remote)
	m.serverMode = srvMode
	if s, err := settings.Load(ctx, st); err == nil {
		m.settings = s
	}
	if lockGroup != "" {
		m.groupFilter = lockGroup
		m.groupLocked = true
	}
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		return err
	}
	if m.restartRequested {
		return ErrRestart
	}
	return nil
}

type pane int

const (
	paneSessions pane = iota
	paneTranscript
	paneSettings
	paneReleaseNotes
)

type model struct {
	ctx         context.Context
	store       store.Store
	remote      RemoteInfo
	width       int
	height      int
	allSessions []mdl.Session // unfiltered, latest from DB
	sessions    []mdl.Session // post-project-filter — what the list actually renders
	events      []mdl.Event
	approvals   []mdl.Approval
	selSess     int
	selAppr     int
	sessScroll  int
	pane        pane
	err         error
	flash       string
	lastTick    time.Time

	// pending-approval alert state
	lastPendingTotal int
	bellPrimed       bool // skip bell on the very first refresh

	// inline transcript tail for the right pane
	tailMessages []transcript.Message
	tailPath     string
	tailMtime    time.Time
	// tailScroll offsets the right-pane view from the latest line. 0 means
	// "auto-scroll to newest"; positive means "show this many lines older".
	tailScroll int

	// list mode + inline editor state
	showArchived  bool
	editingTitle  bool
	editingGroup  bool
	editingSearch bool
	titleBuffer   string
	groupCandIdx  int    // index into filteredGroupCandidates(); -1 == "no pick yet"
	groupFilter   string // "" = All; otherwise repo / cwd basename
	// groupEditProject switches the editingGroup prompt (input + picker
	// of existing names) from the tab group (T) to the project (p).
	groupEditProject bool
	// projectCands are the projects offered by the p prompt, fetched when
	// it opens (ListProjects: includes projects whose sessions are all
	// archived).
	projectCands []mdl.Project
	// groupSel remembers the selected session ID per group so switching
	// tabs returns the cursor to where the operator left it.
	groupSel map[string]string
	liveSel  liveSelection // drag selection in the live pane
	// spawn tracks an `n` spawn until its real session row shows up.
	// Without ccdash hooks a fresh claude has no row until its first
	// prompt writes a transcript, so injectSpawnRow shows a placeholder
	// row keyed by the PTY (pid-<N>) that the live pane can mirror.
	spawn struct {
		pty   string    // ptyKey; "" = nothing pending
		tag   int       // server hook tag (matches WrapperPID when hooks exist)
		sid   string    // session ID once the server aliased the PTY
		cwd   string    // for the placeholder row
		group string    // tab the placeholder lives in
		at    time.Time // placeholder LastSeen
	}
	// removedSel remembers where the cursor was when x took the selected
	// session out of the current view, so the refresh that drops it keeps
	// the cursor at that row instead of jumping to defaultSelectionIdx.
	removedSel struct {
		sid string
		idx int
	}
	searchQuery   string // "" = no filter; case-insensitive substring search
	accountFilter string // "" = all accounts; otherwise account name (from accounts.json)
	// attentionOnly (!) narrows the list to sessions that need the
	// operator or finished a turn nobody has looked at yet.
	attentionOnly bool
	// usageToday is today's API-price estimate across sessions, refreshed
	// every usageEvery ticks (header).
	usageToday *usage.Totals
	usageTicks int
	// seenSent remembers the unread "done" marks this TUI already cleared,
	// so a slow refresh doesn't re-send MarkSeen every tick.
	seenSent map[string]time.Time

	settings settings.Settings

	// awaitGroupArchiveConfirm is true after the operator pressed ctrl+x
	// to bulk-archive the current tab; the next keystroke is treated as
	// the y/n confirmation rather than a normal shortcut.
	awaitGroupArchiveConfirm bool
	// awaitRestartSessionConfirm gates ctrl+r (restart the hosted claude).
	awaitRestartSessionConfirm bool
	// awaitTitleGenConfirm is the ctrl+t banner: y titles titleGenSel,
	// a titles the titleGenRecent batch (see titlegen.go).
	awaitTitleGenConfirm bool
	titleGenSel          string
	titleGenRecent       []string
	// titleWatch holds the sessions of ctrl+t batches this TUI started (watchTitles).
	titleWatch map[string]struct{}

	// awaitMkdirConfirm guards the mkdir step when the operator hits
	// Enter on a path that doesn't exist. y → create + start; anything
	// else cancels. pendingMkdirPath holds the absolute path we'd
	// create if the operator confirms.
	awaitMkdirConfirm bool
	pendingMkdirPath  string

	// groupLocked is set when the operator launched ccdash with --tab
	// <name>. The tab strip is hidden, h/l/Tab/Shift+Tab become no-ops,
	// and the groupFilter never changes.
	groupLocked bool

	// settings page state
	settingsSel    int
	settingsScroll int    // first visible line of the settings page
	settingsEdit   bool   // editing an int / string value inline
	settingsBuffer string // edit buffer (digits only for ints)
	settingsPick   int    // highlighted path suggestion while editing a Path setting; -1 = none

	// transcript view state
	transcriptMessages []transcript.Message
	transcriptScroll   int
	transcriptTitle    string
	transcriptPath     string
	// transcriptSession is the full session row behind the modal viewer —
	// needed (not just the path) so 'r' reload can re-fetch through the
	// Store interface, which takes a model.Session rather than a bare path.
	transcriptSession mdl.Session

	// pendingPTYKeys maps child PID → server ptyKey for new sessions we
	// spawned via 'n'. The sessionID only becomes known after discovery
	// picks up the JSONL; until then we key by PID. promotePTYKeys
	// registers the alias via POST /pty/{key}/register once the row appears.
	pendingPTYKeys map[int]string

	// serverMode indicates whether the ccdash daemon was already running
	// (ServerModeExisting) or was spawned by this TUI launch (ServerModeSpawned).
	serverMode ServerMode

	// animTick advances on every animTickMsg (~150 ms). Drives the spinner
	// frame for active rows so the operator can tell at a glance which
	// sessions are doing work right now. Wraps naturally — we mod into the
	// frame slice on read.
	animTick int

	// updateAvailable is the GitHub release tag of a newer ccdash, set
	// asynchronously a few seconds after startup. Empty when we're either
	// already on the latest tag, the check is still in flight, or the
	// network probe failed (we stay quiet in that case rather than nag).
	updateAvailable string
	// updateRunning is true while selfupdate.Run is executing in a
	// background goroutine. Drives the in-progress flash and prevents
	// double-fires.
	updateRunning bool
	// updateChecking is set while a settings-triggered release check is in
	// flight, so its result is reported even when there's nothing new.
	updateChecking bool
	// notesReturn is the pane the release-notes view goes back to.
	notesReturn pane
	// restartNote is an extra first line for the restart confirmation
	// (e.g. "Updated to v0.5.1"); empty when opened from settings.
	restartNote string
	// updateNotes / updateNotesScroll back the paneReleaseNotes view.
	// Notes are fetched lazily after 'u' triggers the modal; until the
	// fetch returns we render a "loading…" stub.
	updateNotes       string
	updateNotesErr    error
	updateNotesScroll int

	// editingNewSession is true while the operator is typing a directory
	// path in the new-session footer prompt (entered via 'n').
	editingNewSession bool
	newSessionBuffer  string
	// pickSel is the highlighted row of the `n` directory picker (-1 =
	// none, so Enter starts in the typed directory); pickScroll is the
	// first visible row. See dirpicker.go.
	pickSel    int
	pickScroll int
	// spawnPrompt is the first message for the next `n` spawn — set by
	// the skill picker (`S`, "/<skill>") and consumed by spawnNewSession.
	spawnPrompt string
	// spawnProject is the project the next `n` spawn joins: the selected
	// session's project when n was pressed. The server assigns it once the
	// new claude's session ID is known (ptyStartReq.Project).
	spawnProject string
	// restartConfirm shows the "Restart ccdash" confirmation window;
	// restartRequested makes Run return ErrRestart (see restart.go).
	// listLineSess maps each visible line of the session list (from its
	// top) to a session index, -1 for date headers; written by
	// renderSessionsList, read by clickSessionList. lastClick* detect a
	// double click on the same row.
	listLineSess []int
	// lastNavAt is when j/k last moved the list (see moveWrap).
	lastNavAt    time.Time
	lastClickIdx int
	lastClickAt  time.Time

	// dupConfirm shows the "already running" confirmation for
	// dupSessionID (see dupconfirm.go).
	dupConfirm   bool
	dupSessionID string
	// modalBtns are the on-screen buttons of the open confirmation,
	// recorded by View (see modalbtn.go).
	modalBtns []modalButton
	// modalFocus is the keyboard-focused button of the open confirmation.
	modalFocus int

	restartConfirm   bool
	restartRequested bool
	// helpOpen shows the `?` key list (help.go).
	helpOpen   bool
	helpScroll int
	// Skill picker state (see skillpicker.go).
	editingSkill bool
	skillBuffer  string
	skillList    []skills.Skill
	skillSel     int
	skillScroll  int
	skillTabbed  skills.Skill // last Tab-completed entry, for its Dir

	// Live right pane (see live.go). live is the active screen stream for
	// the selected session; nil when that session isn't hosted by the
	// ccdash server. liveFocus routes keystrokes to claude instead of the
	// dashboard.
	live             *liveScreen
	liveFocus        bool
	liveConnecting   string          // ptyKey whose dial is in flight
	liveFocusPending string          // focus the pane as soon as this key connects
	ptyAlive         map[string]bool // ptyKey → child alive, from GET /pty
	// liveCache keeps the last screen of each stream we closed so that
	// moving the selection back shows it at once while the new stream
	// dials, instead of flashing the transcript view.
	liveCache     map[string]*liveScreen
	ptyListWarned bool // flashed once about a server without /pty
}

func newModel(ctx context.Context, st store.Store, remote RemoteInfo) *model {
	return &model{
		ctx:            ctx,
		store:          st,
		remote:         remote,
		settings:       settings.Defaults(),
		pendingPTYKeys: map[int]string{},
		titleWatch:     map[string]struct{}{},
		ptyAlive:       map[string]bool{},
		liveCache:      map[string]*liveScreen{},
	}
}

func (m *model) quit() tea.Cmd {
	m.closeLive()
	return tea.Quit
}

// promotePTYKeys walks freshly-loaded session rows and registers any pending
// PID-based ptyKeys (from newly spawned sessions) with their real sessionID.
// Called from the sessionsMsg handler so registration happens as soon as
// discovery resolves a new session's id.
func (m *model) promotePTYKeys() {
	if len(m.pendingPTYKeys) == 0 {
		return
	}
	addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
	tok, _ := auth.Load()
	for _, s := range m.allSessions {
		if s.ProcPID == 0 || s.SessionID == "" {
			continue
		}
		ptyKey, ok := m.pendingPTYKeys[s.ProcPID]
		if !ok {
			continue
		}
		delete(m.pendingPTYKeys, s.ProcPID)
		sid := s.SessionID
		go func() {
			body, _ := json.Marshal(map[string]string{"sessionId": sid})
			url := "http://" + addr + "/pty/" + ptyKey + "/register"
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.HeaderName, tok)
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
}

// startNewSession is the front door for the `n` flow's final Enter.
// It expands the path, checks whether the directory exists, and either:
//   - immediately spawns claude (if the dir is fine), or
//   - parks the path on awaitMkdirConfirm so the next keystroke can
//     decide whether to create it.
//
// Real mkdir + spawn live in spawnNewSession, which is also reused
// directly when the confirmation gate fires.
func (m *model) startNewSession(dir string) tea.Cmd {
	if dir == "" {
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("path required")} }
	}
	if m.remote.Enabled {
		// No local filesystem to stat/mkdir against — the path only means
		// something on the remote host, so we hand it to the remote shell
		// as-is and let a typo fail there instead of catching it up front.
		return m.spawnNewSessionRemote(dir)
	}
	expanded, err := expandPath(dir)
	if err != nil {
		return func() tea.Msg { return attachDoneMsg{err: err} }
	}
	fi, err := os.Stat(expanded)
	switch {
	case err == nil && !fi.IsDir():
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("not a directory: %s", expanded)} }
	case err == nil:
		return m.spawnNewSession(expanded, false)
	case os.IsNotExist(err):
		// Don't mkdir silently — a typo on the last segment would create
		// a junk dir without the operator noticing. Park the path and
		// surface a confirmation banner; the next keystroke handler
		// decides whether to commit.
		m.awaitMkdirConfirm = true
		m.pendingMkdirPath = expanded
		m.flash = fmt.Sprintf("'%s' does not exist — press 'y' to create + start, any other key to cancel", expanded)
		return nil
	default:
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("stat %s: %w", expanded, err)} }
	}
}

// spawnNewSession does the actual mkdir-if-needed-and-claude-launch dance.
// `created` controls the post-detach flash so the operator knows whether
// ccdash made a new directory on their behalf.
func (m *model) spawnNewSession(expanded string, created bool) tea.Cmd {
	if created {
		if mkErr := os.MkdirAll(expanded, 0o755); mkErr != nil {
			return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("mkdir %s: %w", expanded, mkErr)} }
		}
	}
	cwd := expanded
	createdNote := ""
	if created {
		createdNote = " (created)"
	}
	cols, rows := 0, 0
	if g, ok := m.liveScreenGeom(); ok {
		cols, rows = g.w, g.h
	}
	prompt, project := m.spawnPrompt, m.spawnProject
	m.spawnPrompt, m.spawnProject = "", ""
	return func() tea.Msg {
		ptyKey, tag, err := postPTYStart("", "", cwd, prompt, project, cols, rows)
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return ptyStartedMsg{ptyKey: ptyKey, cwd: cwd, createdNote: createdNote, live: true, tag: tag}
	}
}

// parsePIDFromPTYKey extracts the PID from a ptyKey of the form "pid-<N>".
// Returns 0 if the key is not in that format.
func parsePIDFromPTYKey(key string) int {
	s := strings.TrimPrefix(key, "pid-")
	if s == key {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// expandPath replaces a leading ~ with the operator's home dir and
// resolves to an absolute path. Empty input is rejected upstream.
func expandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tickCmd(m.tickInterval()), animTickCmd(), m.checkUpdateCmd())
}

// checkUpdateCmd asks GitHub for the latest release tag in the background
// and reports back via updateCheckMsg. Skipped on dev builds (no
// version to compare) and when ccdash is the development binary running
// against a still-publishable tag — the message simply doesn't arrive in
// those cases. We give it a short timeout so a stalled network doesn't
// keep a goroutine pinned for the whole session.
func (m *model) checkUpdateCmd() tea.Cmd {
	if buildinfo.IsDev() {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 8*time.Second)
		defer cancel()
		tag, err := selfupdate.LatestTag(ctx, selfupdate.ChannelStable)
		return updateCheckMsg{tag: tag, err: err}
	}
}

// fetchReleaseNotesCmd asks GitHub for the release body of the given
// tag and emits updateNotesMsg when it returns. The TUI renders that in
// paneReleaseNotes — the operator reads the changelog and then chooses
// whether to install.
func (m *model) fetchReleaseNotesCmd(tag string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		notes, err := selfupdate.ReleaseInfo(ctx, tag)
		return updateNotesMsg{notes: notes, err: err}
	}
}

// runUpdateCmd kicks off selfupdate.Run in a background goroutine and
// emits updateDoneMsg when it returns. Used by the 'u' key handler after
// the y/n confirm fires.
func (m *model) runUpdateCmd() tea.Cmd {
	current := buildinfo.Version
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		res, err := selfupdate.Run(ctx, current, selfupdate.ChannelStable)
		return updateDoneMsg{res: res, err: err}
	}
}

type tickMsg time.Time

type usageMsg struct{ today usage.Totals }

// usageEvery is how many refresh ticks pass between usage refreshes.
const usageEvery = 30

func (m *model) usageCmd() tea.Cmd {
	return func() tea.Msg {
		sum, err := m.store.UsageSummary(m.ctx, 1)
		if err != nil {
			return nil
		}
		return usageMsg{today: sum.Today}
	}
}

type animTickMsg time.Time

// updateCheckMsg is the result of the startup release-tag probe. tag is
// the latest published tag (e.g. "v0.3.1"); err is set on a failed
// probe and we just stay quiet instead of nagging.
type updateCheckMsg struct {
	tag string
	err error
}

// updateDoneMsg fires when the operator-triggered selfupdate.Run finishes.
// Whether it succeeded, was a no-op, or failed comes through Result/err.
type updateDoneMsg struct {
	res selfupdate.Result
	err error
}

// updateNotesMsg carries the release-body text fetched for the
// paneReleaseNotes view. err is set when the fetch failed; both empty
// is "still loading" but in practice we only emit it on completion.
type updateNotesMsg struct {
	notes string
	err   error
}

type sessionsMsg []mdl.Session
type eventsMsg []mdl.Event
type approvalsMsg []mdl.Approval
type errMsg struct{ err error }

func tickCmd(every time.Duration) tea.Cmd {
	if every <= 0 {
		every = time.Second
	}
	return tea.Tick(every, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// animTickCmd schedules the next spinner frame. 150 ms feels lively without
// drowning the terminal in repaints — Bubble Tea coalesces renders, but
// every tick also rebuilds the view tree, so we don't want to push it
// faster than the eye actually resolves.
func animTickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}

func (m *model) tickInterval() time.Duration {
	if ms := m.settings.RefreshIntervalMs; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return time.Second
}

func (m *model) refresh() tea.Cmd {
	archived := m.showArchived
	cmds := []tea.Cmd{
		func() tea.Msg {
			ss, err := m.store.ListSessions(m.ctx, archived)
			if err != nil {
				return errMsg{err}
			}
			return sessionsMsg(ss)
		},
		func() tea.Msg {
			as, err := m.store.ListPendingApprovals(m.ctx)
			if err != nil {
				return errMsg{err}
			}
			return approvalsMsg(as)
		},
	}
	if cmd := m.loadTailCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if !m.remote.Enabled {
		cmds = append(cmds, fetchPTYListCmd())
	}
	return tea.Batch(cmds...)
}

// loadTailCmd returns a Cmd that (re)loads the inline transcript for the
// currently selected session. It avoids reparsing if the file's mtime hasn't
// advanced since the last load — most ticks land on an unchanged file.
func (m *model) loadTailCmd() tea.Cmd {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	s := m.sessions[m.selSess]
	path := s.TranscriptPath
	if path == "" {
		return nil
	}
	prevPath := m.tailPath
	prevMtime := m.tailMtime
	budgetKB := m.settings.TailBudgetKB
	if budgetKB <= 0 {
		budgetKB = 256
	}
	st := m.store
	ctx := m.ctx
	return func() tea.Msg {
		statCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		mtime, _, err := st.TranscriptStat(statCtx, s)
		cancel()
		if err != nil {
			return tailMsg{path: path, err: err}
		}
		if path == prevPath && !mtime.After(prevMtime) {
			return tailMsg{path: path, mtime: mtime, unchanged: true}
		}
		// Tail-read keeps session-switching fast even for transcripts in
		// the tens of megabytes; we only need the recent end for the
		// inline pane (the modal viewer pulls the full file).
		tailCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
		defer cancel2()
		tail, err := st.TranscriptTail(tailCtx, s, budgetKB*1024)
		if err != nil {
			return tailMsg{path: path, mtime: mtime, err: err}
		}
		return tailMsg{path: path, mtime: tail.Mtime, messages: tail.Messages}
	}
}

type tailMsg struct {
	path      string
	mtime     time.Time
	messages  []transcript.Message
	unchanged bool
	err       error
}

func (m *model) currentSessionID() string {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return ""
	}
	return m.sessions[m.selSess].SessionID
}

// Update is a thin wrapper around update that reconciles the live right
// pane after every message: selection moves, resizes, and PTY-list polls
// all end up needing the same connect / resize / teardown decisions.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	mm, cmd := m.update(msg)
	if sync := m.syncLive(); sync != nil {
		cmd = tea.Batch(cmd, sync)
	}
	return mm, cmd
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		m.lastTick = time.Time(msg)
		m.usageTicks++
		if m.usageTicks%usageEvery == 1 {
			return m, tea.Batch(m.refresh(), tickCmd(m.tickInterval()), m.usageCmd())
		}
		return m, tea.Batch(m.refresh(), tickCmd(m.tickInterval()))
	case usageMsg:
		t := msg.today
		m.usageToday = &t
	case animTickMsg:
		m.animTick++
		return m, animTickCmd()
	case updateCheckMsg:
		if m.updateChecking {
			// The operator asked (settings → Update ccdash): always answer.
			return m, m.manualUpdateChecked(msg)
		}
		// Stay quiet on errors (probe failure shouldn't nag) and on
		// "already latest" matches. Anything else is a real new tag.
		if msg.err != nil || msg.tag == "" {
			return m, nil
		}
		if msg.tag == buildinfo.Version {
			return m, nil
		}
		m.updateAvailable = msg.tag
		m.flash = "update available: " + msg.tag + " — press 'u' to install"
		return m, nil
	case updateDoneMsg:
		m.updateRunning = false
		if msg.err != nil {
			m.err = fmt.Errorf("update failed: %w", msg.err)
			return m, nil
		}
		if msg.res.NoOp {
			m.flash = "update: " + msg.res.Reason
			m.updateAvailable = ""
			return m, nil
		}
		m.updateAvailable = ""
		// The new binary only runs after a restart: offer it right away.
		m.openRestartConfirm("Updated to " + msg.res.NewVersion + ". Restart now to run it.")
		return m, nil
	case updateNotesMsg:
		m.updateNotes = msg.notes
		m.updateNotesErr = msg.err
		m.updateNotesScroll = 0
		return m, nil
	case sessionsMsg:
		prev := m.currentSessionID()
		m.allSessions = []mdl.Session(msg)
		m.injectSpawnRow()
		// Promote any newly-discovered sessions whose PID matches a
		// freshly-spawned PTY so the server can alias the ptyKey to the
		// real sessionID, enabling reattach by sessionID on next Enter.
		m.promotePTYKeys()
		m.watchTitles()
		// If the tab the operator was looking at vanished (its last
		// session got archived, removed, or moved to a different tab),
		// step onto the next available tab so the body isn't blank.
		// --tab pinned operators stay put; that's their explicit ask.
		if !m.groupLocked && m.groupFilter != "" {
			projects := m.uniqueGroups()
			present := false
			for _, p := range projects {
				if p == m.groupFilter {
					present = true
					break
				}
			}
			if !present {
				from := m.groupFilter
				next := ""
				if len(projects) > 1 {
					next = projects[1]
				}
				m.groupFilter = next
				if next == "" {
					m.flash = fmt.Sprintf("'%s' tab emptied → All", from)
				} else {
					m.flash = fmt.Sprintf("'%s' tab emptied → '%s'", from, next)
				}
				m.tailPath = ""
				m.tailMtime = time.Time{}
			}
		}
		m.applyGroupFilter()
		// Try to keep the cursor on the same session as before; only
		// fall back to defaultSelectionIdx (newest) when the previous
		// session is no longer present (filter change, archive toggle,
		// session vanished, etc.).
		found := false
		if prev != "" {
			for i, s := range m.sessions {
				if s.SessionID == prev {
					m.selSess = i
					found = true
					break
				}
			}
		}
		if !found {
			if prev != "" && prev == m.removedSel.sid {
				// x just removed it: stay on the same row (clamped below).
				m.selSess = m.removedSel.idx
			} else {
				m.selSess = m.defaultSelectionIdx()
			}
			m.removedSel.sid = ""
		}
		m.selectSpawned()
		if m.selSess >= len(m.sessions) {
			m.selSess = len(m.sessions) - 1
		}
		if m.selSess < 0 {
			m.selSess = 0
		}
		seen := m.markSelectedSeen()
		if cur := m.currentSessionID(); cur != "" && cur != prev {
			return m, tea.Batch(m.loadTailCmd(), seen)
		}
		if seen != nil {
			return m, seen
		}
	case eventsMsg:
		m.events = []mdl.Event(msg)
	case tailMsg:
		if msg.err != nil {
			m.err = msg.err
			break
		}
		m.tailPath = msg.path
		m.tailMtime = msg.mtime
		if !msg.unchanged {
			m.tailMessages = msg.messages
		}
	case approvalsMsg:
		m.approvals = []mdl.Approval(msg)
		if m.selAppr >= len(m.approvals) {
			m.selAppr = len(m.approvals) - 1
		}
		if m.selAppr < 0 {
			m.selAppr = 0
		}
		// Ring the terminal bell once when pending count goes from zero to
		// non-zero. We emit \a as part of the rendered View so it goes
		// through Bubble Tea's writer rather than competing with the alt
		// screen via stderr.
		total := len(m.approvals)
		ring := m.settings.BellOnPending && m.bellPrimed && m.lastPendingTotal == 0 && total > 0
		m.lastPendingTotal = total
		m.bellPrimed = true
		if ring {
			return m, ringBellCmd()
		}
	case errMsg:
		m.err = msg.err
	case ptyListMsg:
		if msg.err == nil {
			m.ptyAlive = msg.alive
			m.ptyListWarned = false
			m.resolveSpawnSID(msg.pids)
		} else if !m.ptyListWarned {
			// Usually an older ccdash server (no /pty list route) still
			// holding the port — e.g. a previous TUI build left running.
			m.ptyListWarned = true
			m.flash = "live pane unavailable: " + msg.err.Error() + " (older ccdash still running?)"
		}
	case liveConnectedMsg:
		return m, m.handleLiveConnected(msg)
	case liveFramesMsg:
		return m, m.handleLiveFrames(msg)
	case tea.MouseWheelMsg:
		return m.handleMouse(msg)
	case tea.MouseClickMsg:
		// Clicking the emulator area focuses it; clicking anywhere else
		// gives the keyboard back to the dashboard.
		// A left press in the emulator area also anchors a drag
		// selection (see livesel.go).
		m.clearLiveSelection()
		if msg.Button == tea.MouseLeft {
			if cmd, ok := m.clickModalButton(msg.Mouse()); ok {
				return m, cmd
			}
		}
		if m.modalOpen() && m.pane == paneSessions {
			return m, nil // a picker / confirmation owns input
		}
		if msg.Button == tea.MouseLeft {
			mm := msg.Mouse()
			if live := m.liveForCurrent(); live != nil && m.mouseInLiveScreen(mm) {
				m.setLiveFocus(true)
				m.startLiveSelection(live, mm)
			} else {
				m.setLiveFocus(false)
				return m, m.clickSessionList(mm)
			}
		}
		return m, nil
	case tea.MouseMotionMsg:
		m.dragLiveSelection(msg.Mouse())
		return m, nil
	case tea.MouseReleaseMsg:
		return m, m.finishLiveSelection(msg.Mouse())
	case tea.PasteMsg:
		return m.handlePaste(msg.Content)
	case ptyStartedMsg:
		// Track PID-based ptyKeys for new sessions so promotePTYKeys can
		// register the alias once discovery resolves the sessionID.
		if msg.sessionID == "" {
			// ptyKey format for new sessions is "pid-<N>"
			if pid := parsePIDFromPTYKey(msg.ptyKey); pid > 0 {
				m.pendingPTYKeys[pid] = msg.ptyKey
			}
		}
		m.ptyAlive[msg.ptyKey] = true
		if msg.live && msg.sessionID == "" {
			// Fresh `n` spawn: there's no row to select until claude's
			// first hook lands. sessionsMsg picks the row by its tag
			// (selectSpawned), and the live pane focuses once it streams.
			m.spawn.pty, m.spawn.tag, m.spawn.sid = msg.ptyKey, msg.tag, ""
			m.spawn.cwd, m.spawn.group, m.spawn.at = msg.cwd, m.groupFilter, time.Now()
			m.injectSpawnRow()
			m.applyGroupFilter()
			for i, s := range m.sessions {
				if s.SessionID == msg.ptyKey {
					m.selSess = i
				}
			}
			m.liveFocusPending = msg.ptyKey
			m.flash = "claude started in " + msg.cwd + msg.createdNote + " — ctrl+] returns to the dashboard"
			return m, nil
		}
		if msg.live {
			// Resume path: the server now hosts the session; syncLive (run
			// after this Update) dials the screen stream and we focus it
			// as soon as the first frame lands.
			m.liveFocusPending = msg.ptyKey
			m.flash = "claude running in the right pane — ctrl+] returns to the dashboard"
			return m, nil
		}
		tok, _ := auth.Load()
		addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
		cwd := msg.cwd
		createdNote := msg.createdNote
		sc := &attach.StreamClient{
			SessionID: msg.ptyKey,
			Addr:      addr,
			Token:     tok,
		}
		return m, tea.Exec(sc, func(err error) tea.Msg {
			if err != nil {
				return attachDoneMsg{err: err}
			}
			switch {
			case sc.Result.Detached && cwd != "":
				return attachDoneMsg{msg: "detached — new claude session running in " + cwd + createdNote + " (Enter on the row to reattach)"}
			case sc.Result.Detached:
				return attachDoneMsg{msg: "detached — claude still running (Enter to reattach)"}
			case sc.Result.ExitErr != nil:
				return attachDoneMsg{err: sc.Result.ExitErr, msg: "claude session ended"}
			default:
				return attachDoneMsg{msg: "claude session ended"}
			}
		})
	case sessionRestartedMsg:
		return m, m.handleSessionRestarted(msg)
	case attachDoneMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.flash = msg.msg
		}
		if m.live != nil {
			// A fullscreen attach resized the PTY to the whole terminal;
			// force syncLive to push the pane geometry back.
			m.live.sentCols, m.live.sentRows = 0, 0
		}
		return m, tea.Batch(tea.ClearScreen, m.refresh())
	case titlesKickedMsg:
		for _, id := range msg.sessionIDs {
			m.titleWatch[id] = struct{}{}
		}
		m.flash = fmt.Sprintf("generating %d title(s)…", len(msg.sessionIDs))
		return m, m.refresh()
	case projectsMsg:
		m.projectCands = []mdl.Project(msg)
		return m, nil
	case transcriptLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.transcriptMessages = msg.messages
		m.transcriptPath = msg.path
		m.transcriptTitle = msg.title
		m.transcriptScroll = m.maxTranscriptScroll(m.transcriptVisibleHeight())
		m.pane = paneTranscript
		m.err = nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handlePaste routes bracketed-paste text: into the focused live session,
// or into whichever inline editor is open. Newlines are flattened for the
// single-line editors.
func (m *model) handlePaste(content string) (tea.Model, tea.Cmd) {
	if m.pane == paneSessions && m.liveFocus {
		if live := m.liveForCurrent(); live != nil {
			m.handleLivePaste(content, live)
			return m, nil
		}
	}
	flat := strings.ReplaceAll(strings.ReplaceAll(content, "\r", ""), "\n", " ")
	switch {
	case m.editingSearch, m.editingTitle, m.editingGroup:
		m.titleBuffer += flat
		m.groupCandIdx = -1
	case m.editingNewSession:
		m.newSessionBuffer += flat
		m.pickSel = -1
	case m.editingSkill:
		m.skillBuffer += flat
		m.skillSel, m.skillScroll = 0, 0
	case m.pane == paneSettings && m.settingsEdit:
		m.settingsBuffer += filterSettingsInput(settings.AllSpecs()[m.settingsSel], flat)
	}
	return m, nil
}

// handleMouse routes wheel events. In the modal transcript view, scrolling
// always moves through that buffer. In the sessions view, the wheel zone
// is decided by Y in vertical layout (top = sessions, bottom = transcript)
// or by X in horizontal layout (left = sessions, right = transcript).
func (m *model) handleMouse(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	const wheelStep = 3
	if m.pane == paneTranscript {
		switch msg.Button {
		case tea.MouseWheelUp:
			bodyHeight := m.transcriptVisibleHeight()
			m.transcriptScroll = clamp(m.transcriptScroll-wheelStep, 0, m.maxTranscriptScroll(bodyHeight))
		case tea.MouseWheelDown:
			bodyHeight := m.transcriptVisibleHeight()
			m.transcriptScroll = clamp(m.transcriptScroll+wheelStep, 0, m.maxTranscriptScroll(bodyHeight))
		}
		return m, nil
	}
	mm := msg.Mouse()
	if m.forwardLiveWheel(mm) {
		return m, nil
	}
	inRight := m.mouseInRightPane(mm)
	listStep := 1
	if m.settings.InvertListScroll {
		listStep = -1
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		if inRight {
			m.tailScroll += wheelStep
			return m, nil
		}
		return m, m.move(-listStep)
	case tea.MouseWheelDown:
		if inRight {
			m.tailScroll -= wheelStep
			if m.tailScroll < 0 {
				m.tailScroll = 0
			}
			return m, nil
		}
		return m, m.move(listStep)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.clearLiveSelection()
	if m.restartConfirm {
		return m.handleKeyRestartConfirm(msg)
	}
	if m.helpOpen {
		return m.handleKeyHelp(msg)
	}
	if m.dupConfirm {
		return m.handleKeyDupConfirm(msg)
	}
	if m.pane == paneSessions && m.liveFocus {
		if live := m.liveForCurrent(); live != nil {
			return m.handleLiveKey(msg, live)
		}
		m.liveFocus = false
	}
	if m.pane == paneTranscript {
		return m.handleKeyTranscript(msg)
	}
	if m.pane == paneSettings {
		return m.handleKeySettings(msg)
	}
	if m.pane == paneReleaseNotes {
		return m.handleKeyReleaseNotes(msg)
	}
	if m.editingSearch {
		return m.handleKeySearchEdit(msg)
	}
	if m.editingNewSession {
		return m.handleKeyNewSessionEdit(msg)
	}
	if m.editingSkill {
		return m.handleKeySkillEdit(msg)
	}
	if m.editingTitle || m.editingGroup {
		return m.handleKeyTitleEdit(msg)
	}
	if m.awaitGroupArchiveConfirm {
		m.awaitGroupArchiveConfirm = false
		switch msg.String() {
		case "y", "Y":
			return m, m.archiveCurrentGroup()
		default:
			m.flash = "group archive cancelled"
			return m, nil
		}
	}
	if m.awaitTitleGenConfirm {
		return m.handleKeyTitleGenConfirm(msg)
	}
	if m.awaitRestartSessionConfirm {
		m.awaitRestartSessionConfirm = false
		switch msg.String() {
		case "y", "Y":
			return m, m.restartSessionCurrent()
		default:
			m.flash = "restart cancelled"
			return m, nil
		}
	}
	if m.awaitMkdirConfirm {
		m.awaitMkdirConfirm = false
		path := m.pendingMkdirPath
		m.pendingMkdirPath = ""
		switch msg.String() {
		case "y", "Y":
			m.flash = ""
			return m, m.spawnNewSession(path, true)
		default:
			m.spawnPrompt = ""
			m.flash = "new session cancelled"
			return m, nil
		}
	}
	switch msg.String() {
	case "ctrl+c", "q":
		return m, m.quit()
	case "j", "down":
		return m, m.moveWrap(1, msg.IsRepeat)
	case "k", "up":
		return m, m.moveWrap(-1, msg.IsRepeat)
	case "g", "home":
		return m, m.jumpTo(0)
	case "G", "end":
		return m, m.jumpTo(-1)
	case "J":
		// 1-line scroll of the right-pane transcript toward the newest.
		// tailScroll==0 means "auto-tail at the bottom"; we never go
		// negative.
		if m.tailScroll > 0 {
			m.tailScroll--
		}
		return m, nil
	case "K":
		// 1-line scroll toward older content.
		m.tailScroll++
		return m, nil
	case "pgdown":
		step := m.tailHalfPage()
		m.tailScroll -= step
		if m.tailScroll < 0 {
			m.tailScroll = 0
		}
		return m, nil
	case "pgup":
		m.tailScroll += m.tailHalfPage()
		return m, nil
	case "r":
		return m, m.refresh()
	case "enter":
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		return m, m.attachCurrent()
	case "ctrl+]":
		if live := m.liveForCurrent(); live != nil && !live.exited {
			m.setLiveFocus(true)
			return m, nil
		}
		m.flash = "no live screen for this session — enter starts one"
		return m, nil
	case "F":
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		return m, m.attachFullscreen()
	case "o":
		return m, m.openTranscript()
	case "a":
		if !m.settings.ApproveEnabled {
			m.flash = "approval blocking is OFF (settings ',')"
			return m, nil
		}
		return m, m.decideApproval("allow", false)
	case "A":
		if !m.settings.ApproveEnabled {
			m.flash = "approval blocking is OFF (settings ',')"
			return m, nil
		}
		return m, m.decideApproval("allow", true)
	case "d":
		if !m.settings.ApproveEnabled {
			m.flash = "approval blocking is OFF (settings ',')"
			return m, nil
		}
		return m, m.decideApproval("deny", false)
	case "x":
		return m, m.toggleArchiveCurrent()
	case "ctrl+x":
		return m, m.promptArchiveCurrentGroup()
	case "X":
		m.showArchived = !m.showArchived
		m.selSess = 0
		m.tailScroll = 0
		// The fresh sessionsMsg from refresh() will reset selSess to
		// defaultSelectionIdx() when the previous selection is gone.
		return m, m.refresh()
	case "tab", "l":
		if m.groupLocked {
			m.flash = "group is locked via --group flag"
			return m, nil
		}
		return m, m.cycleGroup(1)
	case "shift+tab", "h":
		if m.groupLocked {
			m.flash = "group is locked via --group flag"
			return m, nil
		}
		return m, m.cycleGroup(-1)
	case "R":
		next, err := settings.Set(m.ctx, m.store, m.settings, "auto_repo_tabs", !m.settings.AutoRepoTabs)
		if err != nil {
			m.err = err
			return m, nil
		}
		m.settings = next
		// If we just turned auto repos off and the current filter is one
		// of them, drop it back to All so the operator isn't stuck with
		// a filter that isn't reachable from the cycle anymore.
		if !m.settings.AutoRepoTabs && m.groupFilter != "" {
			projs := m.uniqueGroups()
			present := false
			for _, p := range projs {
				if p == m.groupFilter {
					present = true
					break
				}
			}
			if !present {
				m.groupFilter = ""
				m.applyGroupFilter()
			}
		}
		if m.settings.AutoRepoTabs {
			m.flash = "auto repo tabs ON"
		} else {
			m.flash = "auto repo tabs OFF (user-named only)"
		}
		return m, nil
	case "f":
		return m, m.toggleFavoriteCurrent()
	case "c":
		return m, m.stepColorCurrent(1)
	case "C":
		return m, m.stepColorCurrent(0)
	case "ctrl+r":
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		m.askRestartSession()
		return m, nil
	case "!":
		m.attentionOnly = !m.attentionOnly
		m.applyGroupFilter()
		if m.selSess >= len(m.sessions) {
			m.selSess = max(len(m.sessions)-1, 0)
		}
		if m.attentionOnly {
			m.flash = "要対応・未確認だけ表示（! で解除）"
		} else {
			m.flash = "すべて表示"
		}
		return m, m.loadTailCmd()
	case "t":
		return m, m.startTitleEdit()
	case "T":
		return m, m.startGroupEdit()
	case "p":
		return m, m.startProjectEdit()
	case "P":
		return m, m.rerollProjectColor()
	case "ctrl+t":
		m.startTitleGen()
		return m, nil
	case "?":
		m.helpOpen = true
		m.helpScroll = 0
		return m, nil
	case ",":
		m.pane = paneSettings
		m.settingsSel = 0
		m.settingsEdit = false
		return m, nil
	case "<":
		m.adjustPaneSplit(-5)
		return m, nil
	case ">":
		m.adjustPaneSplit(5)
		return m, nil
	case "@":
		m.cycleAccountFilter()
		m.applyGroupFilter()
		m.selSess = 0
		return m, nil
	case "/":
		m.editingSearch = true
		m.titleBuffer = m.searchQuery
		return m, nil
	case "u":
		// Trigger the release-notes preview modal. The actual install
		// kicks off from inside paneReleaseNotes when the operator hits
		// 'y' — that way they see the changelog before committing.
		if m.updateAvailable == "" || m.updateRunning {
			return m, nil
		}
		return m, m.openReleaseNotes()
	case "n":
		// Start a new claude session: open the directory picker window.
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		m.spawnPrompt = ""
		m.spawnProject = ""
		if len(m.sessions) > 0 {
			m.spawnProject = m.sessions[m.selSess].Project
		}
		m.openDirPicker()
		return m, nil
	case "S":
		// Run a skill in a throwaway session: pick a skill, then a dir.
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		m.openSkillPicker()
		return m, nil
	case "esc":
		cleared := false
		if m.searchQuery != "" {
			m.searchQuery = ""
			cleared = true
		}
		if m.accountFilter != "" {
			m.accountFilter = ""
			cleared = true
		}
		if cleared {
			m.applyGroupFilter()
			m.selSess = 0
			m.flash = "filters cleared"
		}
		return m, nil
	}
	return m, nil
}

// applyGroupFilter recomputes m.sessions from m.allSessions using the
// current groupFilter, accountFilter, and searchQuery. Filters compose by
// intersection.
func (m *model) applyGroupFilter() {
	src := m.allSessions
	if m.accountFilter != "" {
		out := make([]mdl.Session, 0, len(src))
		for _, s := range src {
			if s.Account == m.accountFilter {
				out = append(out, s)
			}
		}
		src = out
	}
	if m.groupFilter != "" {
		out := make([]mdl.Session, 0, len(src))
		for _, s := range src {
			if groupOf(s) == m.groupFilter {
				out = append(out, s)
			}
		}
		src = out
	}
	if m.attentionOnly {
		out := make([]mdl.Session, 0, len(src))
		for _, s := range src {
			if s.Attention != "" {
				out = append(out, s)
			}
		}
		src = out
	}
	if m.searchQuery != "" {
		q := strings.ToLower(m.searchQuery)
		out := make([]mdl.Session, 0, len(src))
		for _, s := range src {
			if sessionMatchesQuery(s, q) {
				out = append(out, s)
			}
		}
		src = out
	}
	src = projectsFirst(src)
	if m.settings.NewestAtBottom {
		// Reverse a copy: with no filter active src still aliases
		// m.allSessions, and reversing that in place would flip the
		// order back on the next applyGroupFilter without a refresh.
		src = append([]mdl.Session(nil), src...)
		for i, j := 0, len(src)-1; i < j; i, j = i+1, j-1 {
			src[i], src[j] = src[j], src[i]
		}
	}
	m.sessions = src
}

// projectsFirst moves the sessions that belong to a project to the newest
// end of the list (the top; the bottom with newest_at_bottom, since the
// caller reverses afterwards), one block per project: the project with the
// most recent activity first, its sessions newest first. Everything else
// keeps its order (favorites, then by date). Returns a new slice when it
// reorders anything.
func projectsFirst(src []mdl.Session) []mdl.Session {
	latest := map[string]time.Time{}
	for _, s := range src {
		if s.Project == "" {
			continue
		}
		if t, ok := latest[s.Project]; !ok || s.LastSeen.After(t) {
			latest[s.Project] = s.LastSeen
		}
	}
	if len(latest) == 0 {
		return src
	}
	var in, rest []mdl.Session
	for _, s := range src {
		if s.Project != "" {
			in = append(in, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Project != b.Project {
			la, lb := latest[a.Project], latest[b.Project]
			if !la.Equal(lb) {
				return la.After(lb)
			}
			return a.Project < b.Project
		}
		return a.LastSeen.After(b.LastSeen)
	})
	return append(in, rest...)
}

// sessionMatchesQuery returns true when q (lower-cased) appears in any of
// the human-readable fields of s. We don't search payloads or transcripts
// to keep the per-keystroke cost predictable.
func sessionMatchesQuery(s mdl.Session, q string) bool {
	if ref := s.Ref(); ref != "" && (q == strings.ToLower(ref) || q == ref[1:]) {
		return true
	}
	for _, f := range []string{
		s.DisplayTitle(), s.UserGroup, s.Repo, s.Cwd, s.Branch,
		s.Project, s.SessionID,
	} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// cycleAccountFilter advances m.accountFilter through ["", account1, account2, ...]
// and updates m.flash to show the active filter.
func (m *model) cycleAccountFilter() {
	accs, err := accounts.Load()
	if err != nil || len(accs) == 0 {
		m.flash = "no accounts configured"
		return
	}
	// Build ordered list: "" (All) followed by each account name.
	names := make([]string, 0, len(accs)+1)
	names = append(names, "")
	for _, a := range accs {
		names = append(names, a.Name)
	}
	// Find current position and advance.
	next := names[0]
	for i, n := range names {
		if n == m.accountFilter {
			next = names[(i+1)%len(names)]
			break
		}
	}
	m.accountFilter = next
	if next == "" {
		m.flash = "account filter: all"
	} else {
		m.flash = "account filter: " + next
	}
}

// handleKeySearchEdit processes keys while the / search input is active.
func (m *model) handleKeySearchEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Code == tea.KeyEnter:
		m.searchQuery = strings.TrimSpace(m.titleBuffer)
		m.editingSearch = false
		m.titleBuffer = ""
		m.applyGroupFilter()
		m.selSess = 0
		m.sessScroll = 0
		return m, m.loadTailCmd()
	case msg.Code == tea.KeyEscape || msg.String() == "ctrl+c":
		m.editingSearch = false
		m.titleBuffer = ""
		return m, nil
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.titleBuffer); len(r) > 0 {
			m.titleBuffer = string(r[:len(r)-1])
		}
	case msg.Text != "":
		m.titleBuffer += msg.Text
	}
	return m, nil
}

// handleKeyNewSessionEdit drives the footer prompt for `n`. The buffer is
// a directory path. UX:
//   - Tab          : cycle highlight through live candidates (preview only,
//     buffer stays put so the operator can keep typing).
//   - / or Enter   : when a candidate is highlighted, commit it into the
//     buffer and reset the highlight — Enter then "descends"
//     one more level on the next press.
//   - Enter        : with no highlight, starts a `claude` session in the
//     buffer path (creates the dir if missing).
//   - Esc          : cancel.
//
// groupOf names the group a session belongs to. Operator-set user_group
// wins; otherwise we fall back to the repo basename, then the cwd
// basename, so sessions still bucket sensibly without any explicit
// labeling.
func groupOf(s mdl.Session) string {
	if s.UserGroup != "" {
		return s.UserGroup
	}
	if s.Repo != "" {
		return s.Repo
	}
	if s.Cwd != "" {
		return filepath.Base(s.Cwd)
	}
	return ""
}

// uniqueGroups returns the labels available for the tab-strip cycle.
// User-named groups (sessions.user_group) always appear; the auto-derived
// repo / cwd names appear only when includeAutoRepo is true. The
// ""-at-front sentinel represents "All / no filter".
func (m *model) uniqueGroups() []string {
	user := map[string]struct{}{}
	auto := map[string]struct{}{}
	for _, s := range m.allSessions {
		if s.UserGroup != "" {
			user[s.UserGroup] = struct{}{}
			continue
		}
		if !m.settings.AutoRepoTabs {
			continue
		}
		switch {
		case s.Repo != "":
			auto[s.Repo] = struct{}{}
		case s.Cwd != "":
			auto[filepath.Base(s.Cwd)] = struct{}{}
		}
	}
	out := []string{""}
	for k := range user {
		out = append(out, k)
	}
	for k := range auto {
		if _, dup := user[k]; dup {
			continue
		}
		out = append(out, k)
	}
	tail := out[1:]
	for i := 0; i < len(tail); i++ {
		for j := i + 1; j < len(tail); j++ {
			if tail[j] < tail[i] {
				tail[i], tail[j] = tail[j], tail[i]
			}
		}
	}
	return out
}

func (m *model) cycleGroup(delta int) tea.Cmd {
	projects := m.uniqueGroups()
	if len(projects) <= 1 {
		m.flash = "no other projects to filter to"
		return nil
	}
	idx := 0
	for i, p := range projects {
		if p == m.groupFilter {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(projects)) % len(projects)
	m.rememberSelection()
	m.groupFilter = projects[idx]
	m.applyGroupFilter()
	if !m.restoreSelection() {
		m.selSess = m.defaultSelectionIdx()
	}
	m.sessScroll = 0
	m.tailScroll = 0
	m.tailPath = ""
	m.tailMtime = time.Time{}
	return m.loadTailCmd()
}

// injectSpawnRow adds the placeholder row for a pending `n` spawn to
// m.allSessions (rebuilt from the store on every refresh), or forgets the
// spawn once its PTY is gone.
func (m *model) injectSpawnRow() {
	if m.spawn.pty == "" {
		return
	}
	if !m.ptyAlive[m.spawn.pty] {
		m.spawn.pty = ""
		return
	}
	for _, s := range m.allSessions {
		if s.SessionID == m.spawn.pty {
			return
		}
	}
	m.allSessions = append([]mdl.Session{{
		SessionID: m.spawn.pty,
		Cwd:       m.spawn.cwd,
		Title:     "new claude session (starting…)",
		UserGroup: m.spawn.group,
		LastSeen:  m.spawn.at,
		Status:    mdl.StatusActive,
	}}, m.allSessions...)
}

// resolveSpawnSID learns the pending spawn's session ID from the /pty
// list: once the server aliases the PTY, the session ID shows up as a
// second key with the same shell pid.
func (m *model) resolveSpawnSID(pids map[string]int) {
	if m.spawn.pty == "" || m.spawn.sid != "" {
		return
	}
	pid, ok := pids[m.spawn.pty]
	if !ok {
		return
	}
	for k, p := range pids {
		if p == pid && k != m.spawn.pty && !strings.HasPrefix(k, "pid-") {
			m.spawn.sid = k
			return
		}
	}
}

// selectSpawned moves the cursor from the placeholder onto the spawned
// session's real row once it shows up — matched by session ID (learned
// from the /pty aliases) or by the server's hook tag in WrapperPID —
// switching to its tab if needed, and keeps the live pane focused.
func (m *model) selectSpawned() {
	if m.spawn.pty == "" {
		return
	}
	var target *mdl.Session
	for i := range m.allSessions {
		s := &m.allSessions[i]
		if s.SessionID == m.spawn.pty {
			continue
		}
		if (m.spawn.sid != "" && s.SessionID == m.spawn.sid) || (m.spawn.tag != 0 && s.WrapperPID == m.spawn.tag) {
			target = s
			break
		}
	}
	if target == nil {
		return
	}
	tgt := *target
	sid := tgt.SessionID
	onPlaceholder := m.currentSessionID() == m.spawn.pty
	// Carry the screen over so the pane doesn't blank while the stream
	// re-dials under the session ID.
	if m.live != nil && m.live.key == m.spawn.pty && len(m.live.rows) > 0 {
		c := *m.live
		c.client, c.key = nil, sid
		m.liveCache[sid] = &c
	}
	pty := m.spawn.pty
	m.spawn.pty = ""
	kept := m.allSessions[:0:0]
	for _, s := range m.allSessions {
		if s.SessionID != pty {
			kept = append(kept, s)
		}
	}
	m.allSessions = kept
	if !onPlaceholder {
		return // the operator moved on; don't yank the cursor
	}
	if !m.groupLocked && m.groupFilter != "" && groupOf(tgt) != m.groupFilter {
		m.rememberSelection()
		m.groupFilter = groupOf(tgt)
	}
	if m.searchQuery != "" && !sessionMatchesQuery(tgt, strings.ToLower(m.searchQuery)) {
		m.searchQuery = ""
	}
	m.applyGroupFilter()
	for i, s := range m.sessions {
		if s.SessionID == sid {
			m.selSess = i
			m.liveFocusPending = sid
			break
		}
	}
}

// rememberSelection records the current session as the cursor position
// for the active group, so cycleGroup can come back to it later.
func (m *model) rememberSelection() {
	id := m.currentSessionID()
	if id == "" {
		return
	}
	if m.groupSel == nil {
		m.groupSel = map[string]string{}
	}
	m.groupSel[m.groupFilter] = id
}

// restoreSelection moves the cursor to the session remembered for the
// active group. Returns false when nothing is remembered or the session
// is no longer in the list.
func (m *model) restoreSelection() bool {
	id, ok := m.groupSel[m.groupFilter]
	if !ok {
		return false
	}
	for i, s := range m.sessions {
		if s.SessionID == id {
			m.selSess = i
			return true
		}
	}
	return false
}

// defaultSelectionIdx returns the cursor position to land on when the
// session set has just changed (tab switch, archive view toggle, fresh
// startup). Newest is always preferable; with NewestAtBottom enabled
// that's the LAST entry in m.sessions.
func (m *model) defaultSelectionIdx() int {
	if len(m.sessions) == 0 {
		return 0
	}
	if m.settings.NewestAtBottom {
		return len(m.sessions) - 1
	}
	return 0
}

// mouseInRightPane decides whether a wheel event lands on the transcript
// pane (vs the session list) based on the active layout. We recompute the
// pane geometry on demand instead of caching it because View runs every
// frame and the terminal can resize between events.
func (m *model) mouseInRightPane(mm tea.Mouse) bool {
	bodyTop, bodyHeight := m.bodyLayout()
	if m.useVerticalLayout() {
		listH, _ := m.verticalSplit(bodyHeight)
		// +1 for the separator row between the list and the transcript.
		return mm.Y >= bodyTop+listH+1
	}
	return mm.X >= m.leftPaneWidth()+3 // 3-col vertical separator
}

// tailHalfPage approximates half the right pane's visible height. We don't
// know the exact pane size at key-handler time (it depends on the approval
// section and header), so we use half the terminal
// height as a reasonable upper bound. The scroll is clamped at render
// time so over-shooting is harmless.
func (m *model) tailHalfPage() int {
	step := m.height / 2
	if step < 1 {
		step = 1
	}
	return step
}

// handleKeyTitleEdit consumes keystrokes while the rename input is active.
// We avoid the larger key map here so typed letters land in the buffer
// rather than triggering global shortcuts.
func (m *model) handleKeyTitleEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Code == tea.KeyEnter:
		var cmd tea.Cmd
		if m.editingGroup && m.groupEditProject {
			cmd = m.commitProjectEdit()
			m.editingGroup = false
		} else if m.editingGroup {
			cmd = m.commitGroupEdit()
			m.editingGroup = false
		} else {
			cmd = m.commitTitleEdit()
			m.editingTitle = false
		}
		m.titleBuffer = ""
		return m, cmd
	case msg.Code == tea.KeyEscape || msg.String() == "ctrl+c":
		m.editingTitle = false
		m.editingGroup = false
		m.titleBuffer = ""
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.editingGroup {
			m.pickTabCandidate(-1)
			return m, nil
		}
	case msg.Code == tea.KeyDown:
		if m.editingGroup {
			m.pickTabCandidate(1)
			return m, nil
		}
	case msg.Code == tea.KeyBackspace:
		runes := []rune(m.titleBuffer)
		if len(runes) > 0 {
			m.titleBuffer = string(runes[:len(runes)-1])
		}
		m.groupCandIdx = -1 // typing breaks the picker selection
	case msg.Text != "":
		m.titleBuffer += msg.Text
		m.groupCandIdx = -1
	}
	return m, nil
}

// pickTabCandidate moves the candidate cursor by delta and copies the
// chosen value into the buffer. Wraps at both ends; idle (-1) goes to the
// first candidate on KeyDown / last on KeyUp so the operator can start
// browsing immediately on Tᴜ edit open.
func (m *model) pickTabCandidate(delta int) {
	cands := m.filteredGroupCandidates()
	if len(cands) == 0 {
		return
	}
	switch {
	case m.groupCandIdx < 0 && delta > 0:
		m.groupCandIdx = 0
	case m.groupCandIdx < 0 && delta < 0:
		m.groupCandIdx = len(cands) - 1
	default:
		m.groupCandIdx = (m.groupCandIdx + delta + len(cands)) % len(cands)
	}
	m.titleBuffer = cands[m.groupCandIdx]
}

// promptArchiveCurrentGroup prepares a bulk archive (or unarchive, when in
// the archive view) of every session that matches the current filter. We
// require an explicit tab — running it on the All bucket would torch the
// entire dashboard at once. The next keystroke confirms or cancels via
// the awaitGroupArchiveConfirm gate.
func (m *model) promptArchiveCurrentGroup() tea.Cmd {
	if m.groupFilter == "" {
		m.flash = "switch to a specific group first (h/l cycles)"
		return nil
	}
	if len(m.sessions) == 0 {
		m.flash = "group is empty"
		return nil
	}
	verb := "archive"
	if m.showArchived {
		verb = "unarchive"
	}
	m.awaitGroupArchiveConfirm = true
	m.flash = fmt.Sprintf("%s all %d sessions in '%s'? press 'y' to confirm", verb, len(m.sessions), m.groupFilter)
	return nil
}

// archiveCurrentGroup applies SetArchived to every session in m.sessions.
// In the archive view we reverse the action so this same shortcut also
// pulls a group back out of archive in one keystroke. The current group
// is about to go empty, so cycle to the next one synchronously — the
// operator shouldn't have to stare at a blank list while the DB writes
// settle.
func (m *model) archiveCurrentGroup() tea.Cmd {
	group := m.groupFilter
	want := !m.showArchived
	sids := make([]string, 0, len(m.sessions))
	for _, s := range m.sessions {
		sids = append(sids, s.SessionID)
	}
	verb := "archived"
	if !want {
		verb = "unarchived"
	}
	// Move forward in the cycle BEFORE the DB ops kick off so the next
	// render shows the new group populated. cycleGroup computes the
	// next entry from the still-current m.allSessions (which still
	// includes the soon-to-be-archived rows), then applyGroupFilter
	// scopes m.sessions to the new group so they aren't visible there.
	// We skip the cycle when --group pinned the operator: they
	// explicitly asked to stay on this group.
	var cycleCmd tea.Cmd
	if !m.groupLocked {
		cycleCmd = m.cycleGroup(1)
	}
	return tea.Batch(
		cycleCmd,
		func() tea.Msg {
			for _, sid := range sids {
				_ = m.store.SetArchived(m.ctx, sid, want)
			}
			return attachDoneMsg{msg: fmt.Sprintf("%s %d sessions in '%s'", verb, len(sids), group)}
		},
		m.refresh(),
	)
}

func (m *model) toggleArchiveCurrent() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	want := !s.Archived
	sid := s.SessionID
	// Either way the session leaves the list being shown (active view
	// loses it on archive, archived view on unarchive).
	m.removedSel.sid, m.removedSel.idx = sid, m.selSess
	verb := "archived"
	if !want {
		verb = "unarchived"
	}
	return func() tea.Msg {
		if err := m.store.SetArchived(m.ctx, sid, want); err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: verb + " " + shortID(sid)}
	}
}

// markSelectedSeen clears the unread "done" mark of the session under the
// cursor: it is on screen (the right pane shows it), so it has been seen.
func (m *model) markSelectedSeen() tea.Cmd {
	if m.selSess < 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	s := m.sessions[m.selSess]
	if s.Attention != mdl.AttentionDone {
		return nil
	}
	if m.seenSent == nil {
		m.seenSent = map[string]time.Time{}
	}
	if t, ok := m.seenSent[s.SessionID]; ok && !s.AttentionAt.After(t) {
		return nil
	}
	m.seenSent[s.SessionID] = time.Now()
	sid := s.SessionID
	return func() tea.Msg {
		_ = m.store.MarkSeen(m.ctx, sid)
		return nil
	}
}

func (m *model) toggleFavoriteCurrent() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	want := !s.Favorite
	sid := s.SessionID
	verb := "favorited"
	if !want {
		verb = "unfavorited"
	}
	return func() tea.Msg {
		if err := m.store.SetFavorite(m.ctx, sid, want); err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: verb + " " + shortID(sid)}
	}
}

func (m *model) startTitleEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	m.editingTitle = true
	m.titleBuffer = s.CustomTitle
	if m.titleBuffer == "" {
		m.titleBuffer = s.Title
	}
	return nil
}

func (m *model) startGroupEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	m.editingGroup = true
	m.groupEditProject = false
	m.titleBuffer = m.sessions[m.selSess].UserGroup
	m.groupCandIdx = -1
	return nil
}

// startProjectEdit opens the project prompt (p) for the selected session:
// type a new name or pick an existing project; empty takes it out.
func (m *model) startProjectEdit() tea.Cmd {
	if len(m.sessions) == 0 || m.sessions[m.selSess].Num <= 0 {
		return nil
	}
	m.editingGroup = true
	m.groupEditProject = true
	m.titleBuffer = ""
	m.groupCandIdx = -1
	m.projectCands = nil
	st, ctx := m.store, m.ctx
	return func() tea.Msg {
		ps, err := st.ListProjects(ctx)
		if err != nil {
			return projectsMsg(nil)
		}
		return projectsMsg(ps)
	}
}

// removeProjectLabel is the p picker's last entry for a session already in
// a project: choosing it takes the session out. (An empty input means
// "no change", so a stray enter never drops a session from its project.)
func removeProjectLabel(cur string) string { return "✕ remove from " + cur }

// projectsMsg carries the p prompt's candidates.
type projectsMsg []mdl.Project

// projectColorByName finds a candidate's color ("" when unknown).
func (m *model) projectColorByName(name string) string {
	for _, p := range m.projectCands {
		if p.Name == name {
			return mdl.ProjectColorOf(p.Name, p.Color)
		}
	}
	for _, s := range m.allSessions {
		if s.Project == name {
			return mdl.ProjectColorOf(s.Project, s.ProjectColor)
		}
	}
	return ""
}

func (m *model) commitProjectEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	cur := m.sessions[m.selSess]
	sid := cur.SessionID
	project := strings.TrimSpace(m.titleBuffer)
	switch {
	case project == "":
		m.flash = "project unchanged"
		return nil
	case project == removeProjectLabel(cur.Project):
		project = ""
	}
	return func() tea.Msg {
		if err := m.store.SetProject(m.ctx, sid, project); err != nil {
			return attachDoneMsg{err: err}
		}
		if project == "" {
			return attachDoneMsg{msg: "removed " + shortID(sid) + " from its project"}
		}
		return attachDoneMsg{msg: "project: " + project}
	}
}

// rerollProjectColor (P) gives the selected session's project a new color
// that keeps apart from the other projects'.
func (m *model) rerollProjectColor() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	project := m.sessions[m.selSess].Project
	if project == "" {
		m.flash = "not in a project — p puts it in one"
		return nil
	}
	return func() tea.Msg {
		if err := m.store.SetProjectColor(m.ctx, project, "random"); err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: "new color for project " + project}
	}
}

// filteredGroupCandidates returns the unique user_group (or, for the
// project prompt, project) values currently
// in use across the unfiltered session set, narrowed by case-insensitive
// prefix match against the input buffer.
func (m *model) filteredGroupCandidates() []string {
	seen := map[string]struct{}{}
	var all []string
	if m.groupEditProject {
		return m.filteredProjectCandidates()
	}
	for _, s := range m.allSessions {
		v := s.UserGroup
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		all = append(all, v)
	}
	// Stable sort so the picker doesn't jiggle between renders.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j] < all[i] {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if m.titleBuffer == "" {
		return all
	}
	prefix := strings.ToLower(m.titleBuffer)
	out := all[:0]
	for _, c := range all {
		if strings.HasPrefix(strings.ToLower(c), prefix) {
			out = append(out, c)
		}
	}
	return out
}

// filteredProjectCandidates lists the projects for the p prompt: the
// fetched ones (newest first) plus any only seen on session rows, narrowed
// by a case-insensitive substring of the input. The selected session's
// current project is left out (choosing it would change nothing).
func (m *model) filteredProjectCandidates() []string {
	cur := ""
	if len(m.sessions) > 0 {
		cur = m.sessions[m.selSess].Project
	}
	seen := map[string]bool{cur: true, "": true}
	var all []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			all = append(all, n)
		}
	}
	for _, p := range m.projectCands {
		add(p.Name)
	}
	for _, s := range m.allSessions {
		add(s.Project)
	}
	if cur != "" {
		all = append(all, removeProjectLabel(cur))
	}
	q := strings.ToLower(strings.TrimSpace(m.titleBuffer))
	if q == "" || m.groupCandIdx >= 0 {
		return all
	}
	out := all[:0]
	for _, c := range all {
		if strings.Contains(strings.ToLower(c), q) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) commitGroupEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	sid := s.SessionID
	group := strings.TrimSpace(m.titleBuffer)
	return func() tea.Msg {
		if err := m.store.SetUserGroup(m.ctx, sid, group); err != nil {
			return attachDoneMsg{err: err}
		}
		if group == "" {
			return attachDoneMsg{msg: "cleared group for " + shortID(sid)}
		}
		return attachDoneMsg{msg: "group: " + group}
	}
}

func (m *model) commitTitleEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	sid := s.SessionID
	title := strings.TrimSpace(m.titleBuffer)
	// If the buffer matches the auto-derived title we treat it as "clear
	// the override" so the auto title takes over again.
	if title == strings.TrimSpace(s.Title) {
		title = ""
	}
	return func() tea.Msg {
		if err := m.store.SetCustomTitle(m.ctx, sid, title); err != nil {
			return attachDoneMsg{err: err}
		}
		if title == "" {
			return attachDoneMsg{msg: "cleared custom title for " + shortID(sid)}
		}
		return attachDoneMsg{msg: "set title: " + shorten(title, 60)}
	}
}

// decideApproval sends the operator's allow/deny choice to the embedded
// server. With keep=true on an allow decision, the server adds an
// updatedPermissions block so Claude remembers the rule for the rest of
// the session — the next equivalent call won't pop a permission prompt.
func (m *model) decideApproval(behavior string, keep bool) tea.Cmd {
	pending := m.approvalsForSelected()
	if len(pending) == 0 {
		m.flash = "no pending approvals to " + behavior
		return nil
	}
	a := pending[0]
	id := a.ID
	tool := a.Tool
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		if err := m.store.DecideApproval(ctx, id, behavior, "", keep); err != nil {
			return attachDoneMsg{err: err}
		}
		verb := "allowed"
		if behavior == "deny" {
			verb = "denied"
		} else if keep {
			verb = "allowed (kept for session)"
		}
		return attachDoneMsg{msg: fmt.Sprintf("%s %s approval (#%d)", verb, tool, id)}
	}
}

// handleKeyReleaseNotes drives paneReleaseNotes: scroll the changelog,
// 'y' to install, esc / q to back out.
func (m *model) handleKeyReleaseNotes(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	body := m.releaseNotesVisibleHeight()
	max := m.maxReleaseNotesScroll(body)
	switch msg.String() {
	case "ctrl+c":
		return m, m.quit()
	case "q", "esc", "tab":
		m.pane = m.notesReturn
		return m, nil
	case "y", "Y":
		if m.updateRunning {
			return m, nil
		}
		m.updateRunning = true
		m.flash = "updating to " + m.updateAvailable + "…"
		m.pane = m.notesReturn
		return m, m.runUpdateCmd()
	case "j", "down":
		m.updateNotesScroll = clamp(m.updateNotesScroll+1, 0, max)
	case "k", "up":
		m.updateNotesScroll = clamp(m.updateNotesScroll-1, 0, max)
	case "ctrl+d", "pgdown", "space":
		m.updateNotesScroll = clamp(m.updateNotesScroll+body/2, 0, max)
	case "ctrl+u", "pgup":
		m.updateNotesScroll = clamp(m.updateNotesScroll-body/2, 0, max)
	case "g", "home":
		m.updateNotesScroll = 0
	case "G", "end":
		m.updateNotesScroll = max
	}
	return m, nil
}

// releaseNotesVisibleHeight is the number of body rows we get to fill
// inside paneReleaseNotes — same arithmetic as transcriptVisibleHeight,
// minus a header row.
func (m *model) releaseNotesVisibleHeight() int {
	headerH := countLines(m.renderHeader())
	footerH := countLines(m.renderFooter()) + 1
	h := m.height - headerH - footerH - 1 // -1 for the title row
	if h < 5 {
		h = 5
	}
	return h
}

// maxReleaseNotesScroll caps scrolling at "last line is on screen".
func (m *model) maxReleaseNotesScroll(visible int) int {
	total := strings.Count(m.updateNotes, "\n") + 1
	max := total - visible
	if max < 0 {
		return 0
	}
	return max
}

func (m *model) handleKeyTranscript(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	bodyHeight := m.transcriptVisibleHeight()
	switch msg.String() {
	case "ctrl+c":
		return m, m.quit()
	case "q", "esc", "tab":
		m.pane = paneSessions
		return m, nil
	case "j", "down":
		m.transcriptScroll = clamp(m.transcriptScroll+1, 0, m.maxTranscriptScroll(bodyHeight))
	case "k", "up":
		m.transcriptScroll = clamp(m.transcriptScroll-1, 0, m.maxTranscriptScroll(bodyHeight))
	case "ctrl+d", "pgdown", "space":
		m.transcriptScroll = clamp(m.transcriptScroll+bodyHeight/2, 0, m.maxTranscriptScroll(bodyHeight))
	case "ctrl+u", "pgup":
		m.transcriptScroll = clamp(m.transcriptScroll-bodyHeight/2, 0, m.maxTranscriptScroll(bodyHeight))
	case "g", "home":
		m.transcriptScroll = 0
	case "G", "end":
		m.transcriptScroll = m.maxTranscriptScroll(bodyHeight)
	case "r":
		return m, m.reloadTranscript()
	}
	return m, nil
}

type transcriptLoadedMsg struct {
	path     string
	title    string
	messages []transcript.Message
	err      error
}

func (m *model) openTranscript() tea.Cmd {
	if m.pane != paneSessions || len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	if s.TranscriptPath == "" {
		m.flash = "no transcript path recorded for this session"
		return nil
	}
	path := s.TranscriptPath
	title := s.DisplayTitle()
	if title == "" {
		title = shortID(s.SessionID)
	}
	m.transcriptSession = s
	st := m.store
	ctx := m.ctx
	return func() tea.Msg {
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		msgs, err := st.TranscriptFull(tctx, s)
		return transcriptLoadedMsg{path: path, title: title, messages: msgs, err: err}
	}
}

func (m *model) reloadTranscript() tea.Cmd {
	if m.transcriptPath == "" {
		return nil
	}
	path := m.transcriptPath
	title := m.transcriptTitle
	s := m.transcriptSession
	st := m.store
	ctx := m.ctx
	return func() tea.Msg {
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		msgs, err := st.TranscriptFull(tctx, s)
		return transcriptLoadedMsg{path: path, title: title, messages: msgs, err: err}
	}
}

type attachDoneMsg struct {
	err error
	msg string
}

// ptyStartedMsg is returned by a Cmd after POST /pty/start succeeds. The
// Update handler receives it, optionally stores the PID→ptyKey mapping for
// promotion, then launches a StreamClient via tea.Exec.
type ptyStartedMsg struct {
	ptyKey      string
	sessionID   string // empty for new sessions (no id yet)
	cwd         string
	createdNote string
	// live is true when the caller wants the session in the right pane
	// (resume via Enter, or a fresh `n` spawn) instead of a fullscreen
	// attach.
	live bool
	// tag is the server's hook tag for a fresh spawn: the new session's
	// row carries it as WrapperPID, which is how we find and select it.
	tag int
}

// postPTYStart calls POST /pty/start on the local server and returns the ptyKey.
// sessionId and resumeId may be empty for brand-new sessions.
func postPTYStart(sessionID, resumeID, cwd, prompt, project string, cols, rows int) (string, int, error) {
	addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
	body, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"resumeId":  resumeID,
		"cwd":       cwd,
		"prompt":    prompt,
		"project":   project,
		"cols":      cols,
		"rows":      rows,
	})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/pty/start", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok, err := auth.Load(); err == nil {
		req.Header.Set(auth.HeaderName, tok)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("pty/start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("pty/start: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		PtyKey string `json:"ptyKey"`
		Tag    int    `json:"tag"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("pty/start decode: %w", err)
	}
	return out.PtyKey, out.Tag, nil
}

// attachCurrent decides how to attach to the selected session and returns the
// appropriate command. It prefers switching to the existing tmux pane when we
// know one; otherwise it asks the server to start/resume the PTY session and
// connects to it via StreamClient.
func (m *model) attachCurrent() tea.Cmd {
	return m.attachSession(false)
}

// attachSession opens the selected session. Unless force is set, a session
// already running in another terminal opens the "already running"
// confirmation instead of resuming a second claude (see dupconfirm.go).
func (m *model) attachSession(force bool) tea.Cmd {
	if m.pane != paneSessions || len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	if !force && m.liveForCurrent() == nil && m.runningElsewhere(s) {
		m.openDupConfirm(s)
		return nil
	}
	if m.remote.Enabled {
		return m.attachRemote(s)
	}
	if s.Pane != "" {
		c := exec.Command("tmux", "switch-client", "-t", s.Pane)
		return tea.ExecProcess(c, func(err error) tea.Msg {
			return attachDoneMsg{err: err, msg: "switched to " + s.Pane}
		})
	}
	// Already hosted here: just hand the keyboard over.
	if live := m.liveForCurrent(); live != nil && !live.exited {
		m.setLiveFocus(true)
		return nil
	}
	sid := s.SessionID
	cwd := s.Cwd
	cols, rows := 0, 0
	if g, ok := m.liveScreenGeom(); ok {
		cols, rows = g.w, g.h
	}
	m.flash = "starting claude --resume " + shortID(sid) + "…"
	return func() tea.Msg {
		ptyKey, _, err := postPTYStart(sid, sid, cwd, "", "", cols, rows)
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return ptyStartedMsg{ptyKey: ptyKey, sessionID: sid, live: true}
	}
}

// attachRemote hands the whole terminal to `ssh -t <target> <script>` for
// the selected session. internal/attach's PTY machinery only manages a
// claude child on THIS host, so it can't attach anything remote; instead we
// shell out to ssh and let the remote host's own terminal drive claude,
// exactly like an operator would type it by hand after ssh-ing in.
func (m *model) attachRemote(s mdl.Session) tea.Cmd {
	target := m.remote.SSHTarget
	if target == "" {
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("no ssh target configured (--ssh-target)")} }
	}
	var script string
	if s.Pane != "" {
		// A remote ssh session attaches its own tmux client to the pane —
		// there's no local client to switch, unlike the same-host case.
		script = "tmux attach -t " + shellQuote(s.Pane)
	} else {
		if s.Cwd == "" {
			return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("no cwd recorded for this session")} }
		}
		script = remoteCD(s.Cwd) + " && exec claude --resume " + shellQuote(s.SessionID)
	}
	c := sshCommand(target, script)
	sid := s.SessionID
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: "ssh session ended (id " + shortID(sid) + ")"}
	})
}

// spawnNewSessionRemote is the remote-mode counterpart of spawnNewSession:
// there's no local directory to mkdir or stat, so we just ssh in and run
// claude in the operator-typed directory, letting the remote shell's own
// `cd` fail (and print an error the operator sees, since -t gives them a
// real interactive terminal) if the path doesn't exist there.
func (m *model) spawnNewSessionRemote(dir string) tea.Cmd {
	target := m.remote.SSHTarget
	if target == "" {
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("no ssh target configured (--ssh-target)")} }
	}
	script := remoteCD(dir) + " && exec claude"
	if m.spawnPrompt != "" {
		script += " " + shellQuote(m.spawnPrompt)
		m.spawnPrompt = ""
	}
	c := sshCommand(target, script)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: "ssh session ended"}
	})
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes,
// so operator-typed values can be embedded safely in an ssh script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshCommand builds the `ssh -t <target> bash -lc '<script>'` invocation
// remote attach / new-session use. The script runs under `bash -l` — a
// LOGIN shell — because ssh's plain remote-command path is non-interactive
// and skips rc files, so a claude installed via PATH additions in
// ~/.profile / ~/.bash_profile (e.g. ~/.local/bin) would be "command not
// found". The script is single-quoted (via shellQuote) so the remote
// user's own login shell passes it to bash verbatim regardless of what
// that shell is.
func sshCommand(target, script string) *exec.Cmd {
	return exec.Command("ssh", sshArgs(target, script)...)
}

// sshArgs is the testable core of sshCommand.
func sshArgs(target, script string) []string {
	return []string{"-t", target, "bash -lc " + shellQuote(script)}
}

// remoteCD builds a `cd <dir>` shell fragment for an ssh script. A leading
// `~` is deliberately left unquoted so the remote shell still performs
// tilde expansion — quoting it (`cd '~/foo'`) would make the remote shell
// look for a directory literally named "~", which almost never exists.
// Everything else is single-quoted against injection from the
// operator-typed path.
func remoteCD(dir string) string {
	switch {
	case dir == "~":
		return "cd ~"
	case strings.HasPrefix(dir, "~/"):
		return "cd ~/" + shellQuote(strings.TrimPrefix(dir, "~/"))
	default:
		return "cd " + shellQuote(dir)
	}
}

// attachFullscreen hands the whole terminal to the live session (raw PTY
// relay via tea.Exec) until Ctrl+D. The right-pane stream stays connected
// underneath; syncLive pushes the pane geometry back afterwards.
func (m *model) attachFullscreen() tea.Cmd {
	live := m.liveForCurrent()
	if live == nil || live.exited {
		m.flash = "no live screen for this session — enter starts one"
		return nil
	}
	tok, _ := auth.Load()
	addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
	sc := &attach.StreamClient{
		SessionID: live.key,
		Addr:      addr,
		Token:     tok,
	}
	m.setLiveFocus(false)
	return tea.Exec(sc, func(err error) tea.Msg {
		if err != nil {
			return attachDoneMsg{err: err}
		}
		if sc.Result.Detached {
			return attachDoneMsg{msg: "back from fullscreen — claude keeps running in the right pane"}
		}
		return attachDoneMsg{msg: "claude session ended"}
	})
}

// move shifts the selection. Returns a Cmd that refreshes the right pane
// (transcript tail) immediately for the new session, instead of waiting for
// the next tick.
func (m *model) move(delta int) tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	prev := m.selSess
	m.selSess = clamp(m.selSess+delta, 0, len(m.sessions)-1)
	if m.selSess != prev {
		m.tailPath = ""
		m.tailMtime = time.Time{}
		m.tailScroll = 0 // reset scroll when changing sessions
		return m.loadTailCmd()
	}
	return nil
}

// navRepeatGap is the longest gap between two navigation presses that
// still counts as one continuous run (a held key or fast tapping). OS key
// repeat fires every ~30-80 ms, well under it.
const navRepeatGap = 200 * time.Millisecond

// moveWrap is move for keyboard navigation: stepping past either end
// wraps around to the other — but only on a fresh press. While the key is
// held (repeat) or presses come in a fast run, the selection stops at the
// end, so racing down the list doesn't fly over the edge; pressing again
// after a pause wraps. Mouse wheel keeps the clamping move so a long
// scroll doesn't spin through the list.
func (m *model) moveWrap(delta int, repeat bool) tea.Cmd {
	n := len(m.sessions)
	now := time.Now()
	run := repeat || now.Sub(m.lastNavAt) < navRepeatGap
	m.lastNavAt = now
	if n <= 1 {
		return nil
	}
	next := m.selSess + delta
	if next < 0 || next >= n {
		if run {
			m.flash = "end of list — press again to wrap"
			return nil
		}
		next = (next%n + n) % n
	}
	return m.jumpTo(next)
}

func (m *model) jumpTo(idx int) tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	if idx < 0 {
		idx = len(m.sessions) - 1
	}
	m.selSess = clamp(idx, 0, len(m.sessions)-1)
	m.tailPath = ""
	m.tailMtime = time.Time{}
	m.tailScroll = 0
	return m.loadTailCmd()
}

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	if m.width == 0 {
		v.SetContent("loading...")
		return v
	}
	header := m.renderHeader()
	tabBar := m.renderTabBar()
	footer := m.renderFooter()
	headerH := countLines(header)
	tabH := 0
	if tabBar != "" {
		// Tab strip + a blank "breathing room" line below it so the body
		// doesn't visually butt right up against the tabs.
		tabH = 2
	}
	// Match the tab strip's breathing-room line above the footer so the
	// help row doesn't feel pasted onto the body.
	const footerSpacer = 1
	footerH := countLines(footer) + footerSpacer
	bodyHeight := m.height - headerH - tabH - footerH
	if bodyHeight < 5 {
		bodyHeight = 5
	}
	body := clampLines(m.renderBody(bodyHeight), bodyHeight)
	parts := []string{header}
	if tabBar != "" {
		parts = append(parts, tabBar, "")
	}
	parts = append(parts, body, "", footer)
	out := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// Final safety net: never emit more lines than the terminal can show, or
	// the alt-screen scrolls and the title bar disappears off the top.
	out = clampLines(out, m.height)
	// Place the terminal's real cursor inside the live pane while it has
	// focus — that's what lets an OS-level IME anchor its pre-edit text.
	v.Cursor = m.liveCursor()
	if m.editingNewSession {
		box, cx, cy := m.dirPickerBox()
		bw, bh := lipgloss.Width(box), lipgloss.Height(box)
		x, y := max(0, (m.width-bw)/2), max(1, (m.height-bh)/3)
		out = overlay(out, box, x, y)
		// The caret doubles as the IME anchor while typing a path.
		v.Cursor = tea.NewCursor(x+cx, y+cy)
	}
	m.modalBtns = nil
	confirmBox := func(box string, btns []modalButton) {
		bw, bh := lipgloss.Width(box), lipgloss.Height(box)
		x, y := max(0, (m.width-bw)/2), max(1, (m.height-bh)/3)
		out = overlay(out, box, x, y)
		v.Cursor = nil
		for _, b := range btns {
			m.modalBtns = append(m.modalBtns, modalButton{x0: x + b.x0, x1: x + b.x1, y: y + b.y, key: b.key})
		}
	}
	if m.dupConfirm {
		confirmBox(m.dupConfirmBox())
	}
	if m.restartConfirm {
		confirmBox(m.restartBox())
	}
	if m.helpOpen {
		box := m.helpBox()
		bw, bh := lipgloss.Width(box), lipgloss.Height(box)
		out = overlay(out, box, max(0, (m.width-bw)/2), max(1, (m.height-bh)/2))
		v.Cursor = nil
	}
	if m.editingSkill {
		box, cx, cy := m.skillPickerBox()
		bw, bh := lipgloss.Width(box), lipgloss.Height(box)
		x, y := max(0, (m.width-bw)/2), max(1, (m.height-bh)/3)
		out = overlay(out, box, x, y)
		v.Cursor = tea.NewCursor(x+cx, y+cy)
	}
	v.SetContent(out)
	return v
}

// countLines returns the number of '\n'-separated lines in s, treating an
// empty string as zero lines.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// clampLines truncates s so it has at most n lines.
func clampLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if strings.Count(s, "\n")+1 <= n {
		return s
	}
	lines := strings.SplitN(s, "\n", n+1)
	return strings.Join(lines[:n], "\n")
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	subtitleStyle = lipgloss.NewStyle().Faint(true)
	// refStyle colors the "#N" session handle in front of each title.
	refStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("109"))
	// Black on bright orange — meant to be unmissable so a stale dev build
	// stands out against a release binary's clean header.
	devBadgeStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("208")).Padding(0, 1)
	srvExistingBadgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Faint(true) // bright green, dim
	srvSpawnedBadgeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Faint(true) // yellow, dim
	srvRemoteBadgeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Faint(true) // cyan, dim
	// Bright cyan on black so it doesn't clash with the orange DEV chip
	// (a dev binary can also have a newer release available, in which
	// case both show side by side).
	updateBadgeStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("33")).Padding(0, 1)
	selectedRow        = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	pendingStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	pendingRowStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	groupHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
	tabActiveStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("12"))
	confirmBannerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11")).Padding(0, 1)
	// Inactive tabs were on bg 236 + fg 8 — both grays, basically illegible.
	// Drop the background so inactive labels read against the terminal
	// default and bump the fg to 250 (light gray) for solid contrast.
	tabInactiveStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	approvalRowStyle   = lipgloss.NewStyle().Background(lipgloss.Color("58")).Foreground(lipgloss.Color("15"))
	approvalLabelStyle = lipgloss.NewStyle().Background(lipgloss.Color("11")).Foreground(lipgloss.Color("0")).Bold(true)
	statusActive       = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // bright green: busy
	statusIdle         = lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // bright cyan: alive idle
	statusRecent       = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))  // yellow: dead but <6h
	statusStop         = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // dim gray: long-dead
	footerStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	paneTitle          = lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(lipgloss.Color("237")).Foreground(lipgloss.Color("15"))
	paneTitleDim       = lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color("8"))
	errStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

func (m *model) renderHeader() string {
	left := titleStyle.Render("ccdash")
	pendingTotal := 0
	for _, s := range m.sessions {
		pendingTotal += s.PendingCount
	}
	pendingPart := subtitleStyle.Render(fmt.Sprintf("pending: %d", pendingTotal))
	if pendingTotal > 0 {
		// Plain bold-yellow text (no background/padding) so lipgloss.Width
		// matches the actual rendered width and the bar can't wrap to a
		// second visible line — that wrap was eating the tabs row.
		pendingPart = pendingStyle.Render(fmt.Sprintf("⚠ pending: %d", pendingTotal))
	}
	needs, unread := 0, 0
	for _, s := range m.allSessions {
		switch s.Attention {
		case mdl.AttentionNeedsYou:
			needs++
		case mdl.AttentionDone:
			unread++
		}
	}
	attPart := ""
	if needs > 0 {
		attPart += pendingStyle.Render(fmt.Sprintf("? 要対応 %d", needs)) + "  "
	}
	if unread > 0 {
		attPart += statusIdle.Render(fmt.Sprintf("✓ 未確認 %d", unread)) + "  "
	}
	if m.attentionOnly {
		attPart += subtitleStyle.Render("[!]") + "  "
	}
	if m.usageToday != nil && m.usageToday.Messages > 0 {
		attPart += subtitleStyle.Render(fmt.Sprintf("今日 $%.2f", m.usageToday.Cost)) + "  "
	}
	right := subtitleStyle.Render(fmt.Sprintf("sessions: %d  ", len(m.sessions))) + attPart +
		pendingPart +
		subtitleStyle.Render("  "+m.lastTick.Format("15:04:05"))
	switch m.serverMode {
	case ServerModeExisting:
		right += "  " + srvExistingBadgeStyle.Render("⬡ server")
	case ServerModeSpawned:
		right += "  " + srvSpawnedBadgeStyle.Render("⬡ spawned")
	case ServerModeRemote:
		right += "  " + srvRemoteBadgeStyle.Render("⬡ remote")
	}
	if buildinfo.IsDev() {
		// On dev builds: bright orange "DEV", a content-derived hash so we
		// can spot stale binaries at a glance, and the binary's mtime as
		// the closest stand-in for "build time".
		parts := []string{"DEV"}
		if h := buildinfo.Hash(); h != "" {
			parts = append(parts, h)
		}
		if t := buildinfo.BuiltAt(); !t.IsZero() {
			parts = append(parts, t.Local().Format("2006-01-02 15:04"))
		}
		right += "  " + devBadgeStyle.Render(strings.Join(parts, " "))
	}
	if m.updateAvailable != "" {
		// Reuse the dev badge style — operator-facing meaning differs
		// (offer to upgrade vs. flag a dev build) but the visual weight
		// is the same: an unmissable orange chip in the corner.
		right += "  " + updateBadgeStyle.Render("↑ "+m.updateAvailable+" · u")
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	leftLabel := titleStyle.Render("ccdash")
	if m.showArchived {
		leftLabel += " " + subtitleStyle.Render("[archive]")
	}
	if m.groupFilter != "" {
		leftLabel += " " + subtitleStyle.Render("· "+m.groupFilter)
	}
	if m.accountFilter != "" {
		leftLabel += " " + subtitleStyle.Render("[@"+m.accountFilter+"]")
	}
	if m.searchQuery != "" {
		leftLabel += " " + pendingStyle.Render("🔍 "+shorten(m.searchQuery, 30))
	}
	if m.groupLocked {
		leftLabel += " " + subtitleStyle.Render("(locked)")
	}
	if m.showArchived || m.groupFilter != "" || m.accountFilter != "" || m.searchQuery != "" {
		left = leftLabel
		gap = m.width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
	}
	bar := left + strings.Repeat(" ", gap) + right
	// Single-line rule under the title bar to separate header from body.
	// Count is computed from the rune's display width because '─' is East
	// Asian "ambiguous" and reports as 2 cols under CJK locales.
	charW := runewidth.RuneWidth('─')
	if charW < 1 {
		charW = 1
	}
	count := m.width / charW
	if count < 1 {
		count = 1
	}
	rule := subtitleStyle.Render(strings.Repeat("─", count))
	return lipgloss.JoinVertical(lipgloss.Left, bar, rule)
}

func (m *model) renderFooter() string {
	if m.awaitGroupArchiveConfirm || m.awaitMkdirConfirm || m.awaitTitleGenConfirm || m.awaitRestartSessionConfirm {
		// y/n confirmation lands in a full-width yellow banner instead
		// of the dim flash so operators don't miss the cue.
		banner := confirmBannerStyle.Width(m.width).Render(m.flash)
		legend := "y confirm · any other key cancels"
		if m.awaitTitleGenConfirm {
			legend = m.titleGenFooterHint()
		}
		hint := footerStyle.Render(legend)
		return banner + "\n" + hint
	}
	if m.editingSearch {
		prompt := "/" + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("enter apply · esc cancel · empty=clear")
		return pendingStyle.Render(prompt) + "  " + hint
	}
	if m.editingTitle {
		prompt := "rename: " + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("enter save · esc cancel")
		return pendingStyle.Render(prompt) + "  " + hint
	}
	if m.editingGroup {
		prompt := "tab: " + m.titleBuffer + "▏"
		if m.groupEditProject {
			return m.projectPromptFooter()
		}
		hint := subtitleStyle.Render("↑↓ pick · enter assign · esc cancel · empty=clear")
		cands := m.filteredGroupCandidates()
		if len(cands) == 0 {
			return pendingStyle.Render(prompt) + "  " + hint
		}
		labels := make([]string, len(cands))
		for i, c := range cands {
			if i == m.groupCandIdx {
				labels[i] = pendingStyle.Render("▶ " + c)
			} else {
				labels[i] = subtitleStyle.Render(c)
			}
		}
		candLine := subtitleStyle.Render("existing: ") + strings.Join(labels, "  ")
		return candLine + "\n" + pendingStyle.Render(prompt) + "  " + hint
	}
	keys := "↑/↓ sel  h/l tabs  / search  enter open  n new  a/A/d allow/keep/deny  p project  t rename  ctrl+t titles  f fav  x/X arch  ! needs-you  , settings  ? help  q quit"
	if m.pane == paneSessions {
		if live := m.liveForCurrent(); live != nil && !live.exited {
			if m.liveFocus {
				keys = "keys → claude  ctrl+] / ctrl+d back to dashboard  (mouse wheel scrolls claude)"
			} else {
				keys = "↑/↓ sel  enter/ctrl+] type into claude  F fullscreen  a/A/d allow/keep/deny  p project  t rename  ctrl+t titles  x arch  , settings  ? help  q quit"
			}
		}
	}
	if m.pane == paneSettings {
		keys = "↑/↓ select · space toggle · enter edit · esc back"
		if m.settingsEdit {
			keys = "enter save · esc cancel"
			if settings.AllSpecs()[m.settingsSel].Path {
				keys = "↑↓ pick · tab complete · enter save · esc cancel"
			}
		}
	}
	if m.showArchived {
		keys = "↑/↓ select  enter attach  x unarchive  X back to active  o transcript  ? help  q quit"
	}
	if m.pane == paneTranscript {
		keys = "↑/↓ scroll  pgup/pgdn page  g/G top/end  r reload  esc/q back"
	}
	if m.pane == paneReleaseNotes {
		keys = "↑/↓ scroll  pgup/pgdn page  g/G top/end  y install  esc/q back"
	}
	keys = fitKeys(keys, m.width)
	if m.err != nil {
		return errStyle.Render("error: "+m.err.Error()) + "\n" + footerStyle.Render(keys)
	}
	if m.flash != "" {
		return subtitleStyle.Render(m.flash) + "\n" + footerStyle.Render(keys)
	}
	return footerStyle.Render(keys)
}

// projectPromptFooter is the p prompt: the existing projects as chips in
// their colors (↑↓ picks one), then the input. Typing a name that isn't
// listed creates that project on enter.
func (m *model) projectPromptFooter() string {
	cands := m.filteredProjectCandidates()
	cur := ""
	if len(m.sessions) > 0 {
		cur = m.sessions[m.selSess].Project
	}
	var chips []string
	for i, c := range cands {
		if c == removeProjectLabel(cur) {
			st := subtitleStyle
			if i == m.groupCandIdx {
				st = pendingStyle
			}
			chips = append(chips, st.Render(c))
			continue
		}
		bg := m.projectColorByName(c)
		st := lipgloss.NewStyle().Background(lipgloss.Color(bg)).Foreground(lipgloss.Color(mdl.InkOn(bg)))
		label := " " + c + " "
		if i == m.groupCandIdx {
			label = "▶" + c + " "
			st = st.Bold(true).Underline(true)
		}
		chips = append(chips, st.Render(label))
	}
	line1 := subtitleStyle.Render("projects: ")
	switch {
	case len(cands) > 0:
		line1 += strings.Join(chips, " ")
	case len(m.projectCands) == 0 && strings.TrimSpace(m.titleBuffer) == "":
		line1 += subtitleStyle.Render("(none yet — type a name to create one)")
	default:
		line1 += subtitleStyle.Render("(no match)")
	}
	line1 = ansi.Truncate(line1, m.width, "…")
	name := strings.TrimSpace(m.titleBuffer)
	hint := "↑↓ pick · enter put in project · esc cancel"
	if name != "" && m.groupCandIdx < 0 && !m.projectExists(name) {
		hint = "enter: create project \"" + name + "\" · ↑↓ pick · esc cancel"
	}
	prompt := "project: " + m.titleBuffer + "▏"
	if cur != "" {
		prompt = "project (now " + cur + "): " + m.titleBuffer + "▏"
	}
	return line1 + "\n" + pendingStyle.Render(prompt) + "  " + subtitleStyle.Render(hint)
}

// projectExists reports whether name is an existing project.
func (m *model) projectExists(name string) bool {
	return m.projectColorByName(name) != ""
}

// renderTabBar lays out a browser-style strip of project / user-tab labels
// below the header, with the active filter highlighted. When the total
// width exceeds the terminal we center on the active label and surface
// ‹ / › arrows so the operator knows there's more off-screen.
func (m *model) renderTabBar() string {
	if m.pane != paneSessions {
		return ""
	}
	if m.groupLocked {
		// --tab pinned the operator to a single bucket; the strip would
		// be a one-button row that does nothing useful.
		return ""
	}
	tabs := m.uniqueGroups()
	if len(tabs) <= 1 {
		return ""
	}
	display := make([]string, len(tabs))
	activeIdx := 0
	for i, t := range tabs {
		label := t
		if label == "" {
			label = "All"
		}
		styled := " " + label + " "
		if t == m.groupFilter {
			display[i] = tabActiveStyle.Render(styled)
			activeIdx = i
		} else {
			display[i] = tabInactiveStyle.Render(styled)
		}
	}
	return slideTabs(display, activeIdx, m.width)
}

// slideTabs picks a window of the items list that fits in maxW, centered
// on the active index. Overflow on either side is announced with arrow
// markers; both sides reserve space even when not overflowing so the row
// width stays stable across selections.
func slideTabs(items []string, active, maxW int) string {
	widths := make([]int, len(items))
	for i, s := range items {
		widths[i] = lipgloss.Width(s)
	}
	leftMark := subtitleStyle.Render("‹ ")
	rightMark := subtitleStyle.Render(" ›")
	leftPad := strings.Repeat(" ", lipgloss.Width(leftMark))
	rightPad := strings.Repeat(" ", lipgloss.Width(rightMark))
	budget := maxW - lipgloss.Width(leftMark) - lipgloss.Width(rightMark)
	if budget < 1 {
		budget = 1
	}
	used := widths[active]
	lo, hi := active, active
	for {
		expanded := false
		if hi+1 < len(items) && used+widths[hi+1] <= budget {
			hi++
			used += widths[hi]
			expanded = true
		}
		if lo > 0 && used+widths[lo-1] <= budget {
			lo--
			used += widths[lo]
			expanded = true
		}
		if !expanded {
			break
		}
	}
	var b strings.Builder
	if lo > 0 {
		b.WriteString(leftMark)
	} else {
		b.WriteString(leftPad)
	}
	for i := lo; i <= hi; i++ {
		b.WriteString(items[i])
	}
	if hi < len(items)-1 {
		b.WriteString(rightMark)
	} else {
		b.WriteString(rightPad)
	}
	return b.String()
}

func (m *model) renderBody(height int) string {
	switch m.pane {
	case paneTranscript:
		return m.renderTranscriptBody(height)
	case paneSettings:
		return m.renderSettingsBody(height)
	case paneReleaseNotes:
		return m.renderReleaseNotesBody(height)
	default:
		return m.renderSessionsBody(height)
	}
}

// renderReleaseNotesBody renders the changelog modal: a heading row that
// names the version diff (current → target), the GitHub release body
// scrolled into view, and a footer hint reminding the operator how to
// install or back out.
func (m *model) renderReleaseNotesBody(height int) string {
	title := pendingStyle.Render(fmt.Sprintf("update %s → %s", buildinfo.Version, m.updateAvailable))
	bodyH := height - 1
	if bodyH < 1 {
		bodyH = 1
	}
	var body string
	switch {
	case m.updateNotesErr != nil:
		body = errStyle.Render("failed to fetch release notes: " + m.updateNotesErr.Error() +
			"\n\npress 'y' to install anyway, esc to cancel")
	case m.updateNotes == "":
		body = subtitleStyle.Render("loading release notes from github.com/" + selfupdate.Repo + " …")
	default:
		// The body is markdown. We display it as plain text — preserving
		// blank lines and indentation — and let the width clip take care
		// of overflow. A pretty Markdown renderer is a future polish.
		lines := strings.Split(strings.ReplaceAll(m.updateNotes, "\r\n", "\n"), "\n")
		from := m.updateNotesScroll
		if from > len(lines) {
			from = len(lines)
		}
		to := from + bodyH
		if to > len(lines) {
			to = len(lines)
		}
		body = strings.Join(lines[from:to], "\n")
	}
	return title + "\n" + body
}

// filterSettingsInput keeps the characters an inline settings edit
// accepts: digits for KindInt, anything printable for KindString.
func filterSettingsInput(spec settings.Spec, text string) string {
	if spec.Kind == settings.KindString {
		return strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, text)
	}
	var b strings.Builder
	for _, r := range text {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *model) handleKeySettings(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	specs := settings.AllSpecs()
	if m.settingsEdit {
		switch {
		case msg.Code == tea.KeyEnter && specs[m.settingsSel].Kind == settings.KindString && !(specs[m.settingsSel].Path && m.settingsPick >= 0):
			cur := specs[m.settingsSel]
			next, err := settings.Set(m.ctx, m.store, m.settings, cur.Key, strings.TrimSpace(m.settingsBuffer))
			if err == nil {
				m.settings = next
			} else {
				m.err = err
			}
			m.settingsEdit = false
			m.settingsBuffer = ""
			return m, nil
		case msg.Code == tea.KeyEnter && specs[m.settingsSel].Kind != settings.KindString:
			cur := specs[m.settingsSel]
			n, err := strconv.Atoi(m.settingsBuffer)
			if err == nil {
				if cur.Min > 0 && n < cur.Min {
					n = cur.Min
				}
				if cur.Max > 0 && n > cur.Max {
					n = cur.Max
				}
				next, err := settings.Set(m.ctx, m.store, m.settings, cur.Key, n)
				if err == nil {
					m.settings = next
				} else {
					m.err = err
				}
			}
			m.settingsEdit = false
			m.settingsBuffer = ""
			return m, nil
		case msg.Code == tea.KeyEscape || msg.String() == "ctrl+c":
			m.settingsEdit = false
			m.settingsBuffer = ""
			return m, nil
		case specs[m.settingsSel].Path && (msg.String() == "down" || msg.String() == "up"):
			if n := len(pathMatches(m.settingsBuffer)); n > 0 {
				d := 1
				if msg.String() == "up" {
					d = -1
				}
				if m.settingsPick < 0 && d < 0 {
					m.settingsPick = n - 1
				} else {
					m.settingsPick = ((m.settingsPick+d)%n + n) % n
				}
			}
		case specs[m.settingsSel].Path && (msg.String() == "tab" || (msg.Code == tea.KeyEnter && m.settingsPick >= 0)):
			if cands := pathMatches(m.settingsBuffer); m.settingsPick >= 0 && m.settingsPick < len(cands) {
				m.settingsBuffer = cands[m.settingsPick].path
			} else {
				m.settingsBuffer, _ = completePath(m.settingsBuffer)
			}
			m.settingsPick = -1
		case msg.Code == tea.KeyBackspace:
			if r := []rune(m.settingsBuffer); len(r) > 0 {
				m.settingsBuffer = string(r[:len(r)-1])
			}
			m.settingsPick = -1
		case msg.Text != "":
			m.settingsBuffer += filterSettingsInput(specs[m.settingsSel], msg.Text)
			m.settingsPick = -1
		}
		return m, nil
	}
	switch msg.String() {
	case "esc", "q", ",":
		m.pane = paneSessions
		return m, nil
	case "j", "down":
		if m.settingsSel+1 < len(specs) {
			m.settingsSel++
		}
		return m, nil
	case "k", "up":
		if m.settingsSel > 0 {
			m.settingsSel--
		}
		return m, nil
	case "g", "home":
		m.settingsSel = 0
		return m, nil
	case "G", "end":
		m.settingsSel = len(specs) - 1
		return m, nil
	case "space", "enter":
		cur := specs[m.settingsSel]
		switch cur.Kind {
		case settings.KindBool:
			old := settings.Get(m.settings, cur.Key).(bool)
			next, err := settings.Set(m.ctx, m.store, m.settings, cur.Key, !old)
			if err == nil {
				m.settings = next
			} else {
				m.err = err
			}
		case settings.KindInt:
			m.settingsEdit = true
			v := settings.Get(m.settings, cur.Key).(int)
			m.settingsBuffer = strconv.Itoa(v)
		case settings.KindString:
			m.settingsEdit = true
			m.settingsBuffer = settings.Get(m.settings, cur.Key).(string)
			m.settingsPick = -1
			if cur.Path && m.settingsBuffer == "" {
				// Start from ~/ so suggestions show right away.
				m.settingsBuffer = "~/"
			}
		case settings.KindAction:
			if cur.Key == settings.KeyRestart {
				m.openRestartConfirm("")
				return m, nil
			}
			if cur.Key == settings.KeyUpdate {
				return m, m.startUpdateFromSettings()
			}
			if cur.Apply != nil {
				next, err := cur.Apply(m.ctx, m.store, m.settings)
				if err == nil {
					m.settings = next
					m.flash = "applied: " + cur.Label
				} else {
					m.err = err
				}
			}
		case settings.KindEnum:
			if len(cur.Options) > 0 {
				curVal, _ := settings.Get(m.settings, cur.Key).(string)
				idx := 0
				for i, o := range cur.Options {
					if o == curVal {
						idx = i
						break
					}
				}
				next := cur.Options[(idx+1)%len(cur.Options)]
				updated, err := settings.Set(m.ctx, m.store, m.settings, cur.Key, next)
				if err == nil {
					m.settings = updated
				} else {
					m.err = err
				}
			}
		}
	}
	return m, nil
}

// settingsSystemHeader opens the settings page's last section: a rule,
// the running version, and any pending update, above "Restart ccdash".
func (m *model) settingsSystemHeader() []string {
	version := buildinfo.Version
	if buildinfo.IsDev() {
		if h := buildinfo.Hash(); h != "" {
			version += " · " + h
		}
		if t := buildinfo.BuiltAt(); !t.IsZero() {
			version += " · built " + t.Local().Format("01/02 15:04")
		}
	}
	rule := "-- system " + strings.Repeat("-", max(0, min(m.width, 80)-10))
	lines := []string{
		subtitleStyle.Render(rule),
		"",
		fmt.Sprintf("  %-32s  %s", "Version", statusActive.Render(version)),
	}
	if m.updateAvailable != "" {
		lines = append(lines, "  "+strings.Repeat(" ", 34)+pendingStyle.Render("update available: "+m.updateAvailable+" (Update ccdash below)"))
	}
	return append(lines, "")
}

// settingsValueCol is where a settings row's value starts: marker (2) +
// %-32s label + 2 spaces.
const settingsValueCol = 36

func (m *model) renderSettingsBody(height int) string {
	specs := settings.AllSpecs()
	header := titleStyle.Render("settings") + "  " + subtitleStyle.Render("(persists across runs)")
	var rows []string
	selStart, selEnd := 0, 0 // rows span of the selected setting (label … help)
	for i, s := range specs {
		marker := "  "
		if i == m.settingsSel {
			marker = "▶ "
		}
		if s.Key == settings.KeyUpdate {
			rows = append(rows, m.settingsSystemHeader()...)
		}
		if i == m.settingsSel {
			selStart = len(rows)
		}
		valStr := ""
		switch s.Kind {
		case settings.KindBool:
			// Render as "on · off" with the active option highlighted, so
			// every toggleable row reads the same way as the layout enum
			// below — operators don't have to context-switch between
			// "ON/OFF" labels and inline option lists.
			cur := settings.Get(m.settings, s.Key).(bool)
			activeIdx := 1
			if cur {
				activeIdx = 0
			}
			parts := []string{"on", "off"}
			rendered := make([]string, len(parts))
			for j, p := range parts {
				if j == activeIdx {
					rendered[j] = statusActive.Render(p)
				} else {
					rendered[j] = subtitleStyle.Render(p)
				}
			}
			valStr = strings.Join(rendered, " · ")
		case settings.KindInt:
			cur := settings.Get(m.settings, s.Key).(int)
			if m.settingsEdit && i == m.settingsSel {
				valStr = pendingStyle.Render(m.settingsBuffer + "▏")
			} else {
				valStr = fmt.Sprintf("%d", cur)
			}
			// Show the live terminal width next to the auto-vertical
			// threshold so the operator can pick a value relative to
			// their current window.
			if s.Key == settings.KeyVerticalAutoCols {
				marker := "≥ threshold"
				if m.width < cur {
					marker = "< threshold ⇒ vertical"
				}
				valStr += "  " + subtitleStyle.Render(fmt.Sprintf("(now: %d cols, %s)", m.width, marker))
			}
		case settings.KindString:
			cur := settings.Get(m.settings, s.Key).(string)
			switch {
			case m.settingsEdit && i == m.settingsSel:
				valStr = pendingStyle.Render(m.settingsBuffer + "▏")
			case cur == "":
				valStr = subtitleStyle.Render("(home)")
			default:
				valStr = cur
			}
		case settings.KindAction:
			valStr = pendingStyle.Render("[run]")
		case settings.KindEnum:
			cur, _ := settings.Get(m.settings, s.Key).(string)
			parts := make([]string, 0, len(s.Options))
			for _, o := range s.Options {
				if o == cur {
					parts = append(parts, statusActive.Render(o))
				} else {
					parts = append(parts, subtitleStyle.Render(o))
				}
			}
			valStr = strings.Join(parts, " · ")
		}
		labelLine := fmt.Sprintf("%s%-32s  %s", marker, s.Label, valStr)
		if i == m.settingsSel {
			labelLine = selectedRow.Render(padRight(labelLine, m.width))
		}
		rows = append(rows, labelLine)
		if s.Path && m.settingsEdit && i == m.settingsSel {
			// Live suggestions right under the input, in the value column.
			indent := strings.Repeat(" ", settingsValueCol)
			box := pathSuggestBox(pathMatches(m.settingsBuffer), m.settingsPick, max(20, min(60, m.width-settingsValueCol-4)))
			for _, l := range strings.Split(box, "\n") {
				rows = append(rows, indent+l)
			}
		}
		rows = append(rows, subtitleStyle.Render("    "+s.Help))
		if i == m.settingsSel {
			selEnd = len(rows)
		}
		rows = append(rows, "")
	}
	// Key help lives in the shared footer (renderFooter), not here.
	lines := append([]string{header, ""}, rows...)
	selStart, selEnd = selStart+2, selEnd+2
	// Scroll only when the selection leaves the window, like a list
	// view: moving back up from the bottom moves the cursor first and
	// scrolls once it reaches the top edge.
	if height <= 0 || len(lines) <= height {
		m.settingsScroll = 0
		return strings.Join(lines, "\n")
	}
	if m.settingsSel == 0 {
		selStart = 0 // show the page header with the first row
	}
	if selStart < m.settingsScroll {
		m.settingsScroll = selStart
	}
	if selEnd > m.settingsScroll+height {
		m.settingsScroll = selEnd - height
	}
	m.settingsScroll = clamp(m.settingsScroll, 0, len(lines)-height)
	return strings.Join(lines[m.settingsScroll:m.settingsScroll+height], "\n")
}

// leftPaneWidth is the session list's width in the horizontal layout,
// per the list-size setting. Every geometry consumer (render, mouse
// zoning, live pane sizing) must use this so they agree to the cell.
// The list keeps 30 cols and the right pane 20 (after the 3-col
// separator); on a terminal too narrow for both, the list wins.
func (m *model) leftPaneWidth() int {
	w := m.width * settings.ClampPaneListPct(m.settings.PaneListPct) / 100
	w = min(w, m.width-3-20)
	return max(w, 30)
}

// adjustPaneSplit grows (+) or shrinks (-) the session list by step
// percent and persists it — the `<` / `>` dashboard keys.
func (m *model) adjustPaneSplit(step int) {
	pct := settings.ClampPaneListPct(m.settings.PaneListPct + step)
	next, err := settings.Set(m.ctx, m.store, m.settings, "pane_list_pct", pct)
	if err != nil {
		m.err = err
		return
	}
	m.settings = next
	m.flash = fmt.Sprintf("session list %d%% · work pane %d%%", pct, 100-pct)
}

func (m *model) renderSessionsBody(height int) string {
	if m.useVerticalLayout() {
		return m.renderSessionsBodyVertical(height)
	}
	// Reserve 3 cols for " │ " separator.
	const sepW = 3
	leftWidth := m.leftPaneWidth()
	rightWidth := m.width - leftWidth - sepW
	if rightWidth < 20 {
		rightWidth = 20
	}
	left := m.renderSessionsList(leftWidth, height)
	right := m.renderEventsList(rightWidth, height)
	// Build a vertical separator: " │ " repeated per row, joined with \n.
	sepLine := " " + subtitleStyle.Render("│") + " "
	sepLines := make([]string, height)
	for i := range sepLines {
		sepLines[i] = sepLine
	}
	sep := strings.Join(sepLines, "\n")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, sep, right)
}

// useVerticalLayout returns true when the body should stack vertically.
// "auto" flips on narrow terminals so a 4K monitor split vertically into
// a tall column gets the stacked layout for free; "vertical" forces it
// regardless; "horizontal" never stacks even on narrow displays.
func (m *model) useVerticalLayout() bool {
	switch m.settings.LayoutMode {
	case "vertical":
		return true
	case "horizontal":
		return false
	default:
		threshold := m.settings.VerticalAutoCols
		if threshold <= 0 {
			threshold = 100
		}
		return m.width < threshold
	}
}

// renderSessionsBodyVertical stacks the list above the transcript pane.
// The split is 1/2 each minus a single separator line. Useful for narrow
// or tall terminals where horizontal width is the scarce resource.
func (m *model) renderSessionsBodyVertical(height int) string {
	listH, rightH := m.verticalSplit(height)
	list := m.renderSessionsList(m.width, listH)
	right := m.renderEventsList(m.width, rightH)
	// Use ASCII '-' rather than the box-drawing '─' so we don't get bitten
	// by terminals that disagree with runewidth on the East-Asian
	// "ambiguous" rendering. With '-' every cell is unambiguously one
	// column, so the rule reliably spans the full body width.
	sep := subtitleStyle.Render(strings.Repeat("-", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, list, sep, right)
}

// verticalSplit returns the line allotments for the list and transcript
// panes in vertical layout. Extracted so the mouse handler can compute
// the same top/bottom boundary without re-rendering anything.
func (m *model) verticalSplit(height int) (listH, rightH int) {
	listH = height * settings.ClampPaneListPct(m.settings.PaneListPct) / 100
	if listH < 5 {
		listH = 5
	}
	rightH = height - listH - 1
	if rightH < 5 {
		rightH = 5
		listH = height - rightH - 1
		if listH < 5 {
			listH = 5
		}
	}
	return listH, rightH
}

func (m *model) renderSessionsList(width, height int) string {
	m.listLineSess = nil
	if len(m.sessions) == 0 {
		return lipgloss.NewStyle().Width(width).Height(height).
			Render(subtitleStyle.Render("no sessions yet"))
	}

	// Build a flat list of "rows" — header rows (1 line each) and session
	// rows (2 lines each) interleaved by date bucket. We render everything
	// to a single line slice and then pick a window that keeps the selected
	// session visible. This is simpler than tracking variable-height
	// blocks individually.
	type rowEntry struct {
		lines      []string
		sessionIdx int // -1 for headers
	}

	now := time.Now()
	var rows []rowEntry
	// Project members carry a one-cell gutter in the project's color; the
	// column is reserved on every row while any project is in view so the
	// rows stay aligned.
	stats := projectStats(m.sessions)
	rowW := width
	if len(stats) > 0 {
		rowW--
	}
	prevBucket := ""
	for i, s := range m.sessions {
		bucket := bucketFor(s, now)
		if bucket != prevBucket {
			// A blank line around each project block keeps neighbouring
			// projects (and the date groups after them) apart.
			if len(rows) > 0 && (s.Project != "" || strings.HasPrefix(prevBucket, projectBucketPrefix)) {
				rows = append(rows, rowEntry{lines: []string{""}, sessionIdx: -1})
			}
			var header string
			switch {
			case s.Project != "":
				header = renderProjectHeader(s, stats[s.Project], width)
			case bucket == bucketFavorites:
				header = groupHeaderStyle.Render(padRight("★ "+bucket, width))
			default:
				header = groupHeaderStyle.Render(padRight(bucket, width))
			}
			rows = append(rows, rowEntry{lines: []string{header}, sessionIdx: -1})
			prevBucket = bucket
		}
		row := m.renderSessionRow(s, i == m.selSess, rowW)
		// renderSessionRow returns a 2-line block ending with "\n"; split.
		lines := strings.Split(strings.TrimRight(row, "\n"), "\n")
		if len(stats) > 0 {
			gutter := " "
			if s.Project != "" {
				gutter = lipgloss.NewStyle().Background(lipgloss.Color(mdl.ProjectColorOf(s.Project, s.ProjectColor))).Render(" ")
			}
			for k := range lines {
				lines[k] = gutter + lines[k]
			}
		}
		rows = append(rows, rowEntry{lines: lines, sessionIdx: i})
	}

	// Flatten rows into a line slice and remember where each session lands.
	var allLines []string
	var lineSess []int // session index per line, -1 for headers
	sessionLineStart := make([]int, len(m.sessions))
	for _, r := range rows {
		if r.sessionIdx >= 0 {
			sessionLineStart[r.sessionIdx] = len(allLines)
		}
		allLines = append(allLines, r.lines...)
		for range r.lines {
			lineSess = append(lineSess, r.sessionIdx)
		}
	}

	// Reserve one line at the bottom for the "n/N" indicator.
	visibleH := height - 1
	if visibleH < 2 {
		visibleH = 2
	}

	// Adjust scroll so the selected session's lines (2 of them) stay in view.
	selStart := sessionLineStart[m.selSess]
	selEnd := selStart + 2 // session rows are always 2 lines
	if m.sessScroll > selStart {
		m.sessScroll = selStart
		// Pull in the preceding header if there is one and it fits.
		if m.sessScroll > 0 {
			m.sessScroll--
		}
	}
	if selEnd > m.sessScroll+visibleH {
		m.sessScroll = selEnd - visibleH
	}
	if m.sessScroll < 0 {
		m.sessScroll = 0
	}
	maxScroll := len(allLines) - visibleH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.sessScroll > maxScroll {
		m.sessScroll = maxScroll
	}

	end := m.sessScroll + visibleH
	if end > len(allLines) {
		end = len(allLines)
	}

	body := strings.Join(allLines[m.sessScroll:end], "\n")
	// Mouse clicks map a list row back to its session through this.
	m.listLineSess = lineSess[m.sessScroll:end]
	indicator := fmt.Sprintf("%d / %d sessions", m.selSess+1, len(m.sessions))
	if m.sessScroll > 0 || end < len(allLines) {
		indicator += "  ↑↓ to scroll"
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(body + "\n" + subtitleStyle.Render(indicator))
}

const bucketFavorites = "Favorites"

// projectBucketPrefix marks bucketFor labels that are projects (a NUL can't
// collide with a date label).
const projectBucketPrefix = "\x00project:"

// projectStat is what a project header shows about its sessions in view.
type projectStat struct {
	total, running, needsYou int
	latest                   time.Time
}

// projectStats tallies the sessions of each project in ss.
func projectStats(ss []mdl.Session) map[string]projectStat {
	out := map[string]projectStat{}
	for _, s := range ss {
		if s.Project == "" {
			continue
		}
		st := out[s.Project]
		st.total++
		if s.Status == mdl.StatusActive || s.Status == mdl.StatusIdle {
			st.running++
		}
		if s.Attention == mdl.AttentionNeedsYou {
			st.needsYou++
		}
		if s.LastSeen.After(st.latest) {
			st.latest = s.LastSeen
		}
		out[s.Project] = st
	}
	return out
}

// renderProjectHeader draws a project's heading as a full-width band in the
// project's color — louder than the gray date headings and the sessions'
// one-cell bars, so the project blocks read as one unit.
func renderProjectHeader(s mdl.Session, st projectStat, width int) string {
	bg := mdl.ProjectColorOf(s.Project, s.ProjectColor)
	ink := mdl.InkOn(bg)
	band := lipgloss.NewStyle().Background(lipgloss.Color(bg)).Foreground(lipgloss.Color(ink))
	meta := fmt.Sprintf("%d sessions", st.total)
	if st.running > 0 {
		meta += fmt.Sprintf(" · %d running", st.running)
	}
	if st.needsYou > 0 {
		meta += fmt.Sprintf(" · %d needs you", st.needsYou)
	}
	name := " PROJECT  " + s.Project + "  "
	room := width - runewidth.StringWidth(name)
	if room < 0 {
		return band.Bold(true).Render(runewidth.Truncate(name, width, "…"))
	}
	meta = runewidth.Truncate(meta, room, "…")
	return band.Bold(true).Render(name) + band.Render(padRight(meta, room))
}

// bucketFor returns the group label that a session belongs to. Project
// members are grouped by project (projectsFirst puts them at the newest
// end); favorites go to the top regardless of date; everything else is
// bucketed by last_seen.
func bucketFor(s mdl.Session, now time.Time) string {
	if s.Project != "" {
		return projectBucketPrefix + s.Project
	}
	if s.Favorite {
		return bucketFavorites
	}
	if s.LastSeen.IsZero() {
		return "Unknown"
	}
	t := s.LastSeen.Local()
	today := now.Local()
	if sameYMD(t, today) {
		return "Today"
	}
	if sameYMD(t, today.AddDate(0, 0, -1)) {
		return "Yesterday"
	}
	if t.After(today.AddDate(0, 0, -7)) {
		return "This week"
	}
	if t.After(today.AddDate(0, -1, 0)) {
		return "Earlier this month"
	}
	return t.Format("January 2006")
}

func sameYMD(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// renderSessionRow renders one session as a 2-line block:
//
//	▶ ● 1m   @task.md の内容から、具体的な作業内容を確認して
//	         ccmanage:main · a574854b · ⚠3
//
// Line 1 leads with status, age, and the title (the most useful identifier
// for the operator). Line 2 carries supporting metadata in dim text.
func (m *model) renderSessionRow(s mdl.Session, selected bool, width int) string {
	// A one-cell bar in the session's color (Session.Color, shared with the
	// portal) leads both lines. A background-colored space rather than a
	// block glyph: those are East-Asian ambiguous width.
	bar := " "
	if c := s.Color(); c != "" {
		bar = lipgloss.NewStyle().Background(lipgloss.Color(c)).Render(" ")
	}
	width--
	age := m.sessionTime(s.LastSeen)
	// Line-2 indent aligns with where the title starts on line 1:
	// marker(1) + " "(1) + dot(1) + " "(1) + age + " "(1).
	indent := strings.Repeat(" ", 5+runewidth.StringWidth(age))

	marker := " "
	if selected {
		marker = "▶"
	}
	statusDot := renderStatusDot(s.Status, m.animTick)
	switch {
	case s.Attention == mdl.AttentionNeedsYou && s.Status != mdl.StatusActive:
		statusDot = pendingStyle.Render("?")
	case s.Attention == mdl.AttentionNeedsYou:
		statusDot = pendingStyle.Bold(true).Render("?")
	case s.Attention == mdl.AttentionDone:
		statusDot = statusIdle.Bold(true).Render("✓")
	}

	title := s.DisplayTitle()
	if title == "" {
		title = "(no prompt yet)"
	}
	if s.Favorite {
		title = "★ " + title
	}
	if s.PendingCount > 0 {
		title = "⚠ " + title
	}
	ref := s.Ref()
	titleBudget := width - runewidth.StringWidth(indent)
	if ref != "" {
		titleBudget -= runewidth.StringWidth(ref) + 1
	}
	if titleBudget < 10 {
		titleBudget = 10
	}
	titleText := shorten(title, titleBudget)
	titleStyled := titleText
	switch {
	case selected:
		titleStyled = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Render(titleText)
	case s.PendingCount > 0:
		titleStyled = pendingRowStyle.Render(titleText)
	case s.DisplayTitle() == "":
		titleStyled = subtitleStyle.Render(titleText)
	}
	if ref != "" {
		titleStyled = refStyle.Render(ref) + " " + titleStyled
	}
	line1 := fmt.Sprintf("%s %s %s %s", marker, statusDot, subtitleStyle.Render(age), titleStyled)

	// Line 2: repo:branch · session-id · pending
	repo := baseLast(s.Cwd)
	if s.Branch != "" && s.Branch != "HEAD" {
		repo = repo + ":" + s.Branch
	}
	parts := []string{repo, shortID(s.SessionID)}
	if s.Pane != "" {
		parts = append(parts, "tmux:"+s.Pane)
	} else if s.ProcPID != 0 {
		parts = append(parts, fmt.Sprintf("pid:%d", s.ProcPID))
	}
	if s.PendingCount > 0 {
		parts = append(parts, pendingStyle.Render(fmt.Sprintf("⚠%d pending", s.PendingCount)))
	} else if s.Attention == mdl.AttentionNeedsYou {
		reason := s.AttentionReason
		if reason == "" {
			reason = "要対応"
		}
		parts = append(parts, pendingStyle.Render("? "+shorten(reason, 60)))
	}
	switch s.TitleStatus {
	case "running":
		parts = append(parts, pendingStyle.Render("⏳ titling"))
	case "error":
		parts = append(parts, statusStop.Render("✗ title error"))
	}
	meta := strings.Join(parts, " · ")
	metaBudget := width - runewidth.StringWidth(indent)
	if metaBudget < 10 {
		metaBudget = 10
	}
	line2 := indent + subtitleStyle.Render(shorten(meta, metaBudget))

	if selected {
		line1 = selectedRow.Render(padRight(line1, width))
		line2 = selectedRow.Render(padRight(line2, width))
	}
	return bar + line1 + "\n" + bar + line2 + "\n"
}

func padRight(s string, width int) string {
	visible := lipgloss.Width(s)
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

// renderEventsList renders the right pane: a live transcript tail and a
// pending-approval section pinned to the bottom whenever the selected
// session has any approvals waiting.
func (m *model) renderEventsList(width, height int) string {
	if m.currentSessionID() == "" {
		return ""
	}

	if live := m.liveForCurrent(); live != nil {
		return m.renderLivePane(live, width, height)
	}
	if key := m.liveWantKey(); key != "" {
		// The stream for this session is still dialing / hasn't sent its
		// first frame: keep the live layout (with the cached screen if we
		// have one) rather than flashing the transcript for a frame.
		return m.renderLivePane(m.livePlaceholder(key), width, height)
	}

	hdrID := shortID(m.currentSessionID())
	if len(m.sessions) > 0 {
		if ref := m.sessions[m.selSess].Ref(); ref != "" {
			hdrID = ref + " · " + hdrID
		}
	}
	header := subtitleStyle.Render(fmt.Sprintf("transcript  (%s)", hdrID))

	approvalSection, approvalH := m.approvalBlock(width, height)

	transcriptH := height - 1 // header
	if approvalH > 0 {
		transcriptH -= approvalH
	}
	if transcriptH < 1 {
		transcriptH = 1
	}

	transcriptBody := m.renderTranscriptTail(width, transcriptH)
	out := header + "\n" + transcriptBody
	if approvalSection != "" {
		out += "\n" + approvalSection
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(out)
}

// approvalBlock renders the pinned approval section for the selected
// session, capped at half the pane so the transcript / live screen stays
// readable. Returns "" and 0 when nothing is pending.
func (m *model) approvalBlock(width, height int) (section string, lines int) {
	approvals := m.approvalsForSelected()
	if len(approvals) == 0 {
		return "", 0
	}
	section = m.renderApprovalSection(approvals, width)
	lines = strings.Count(section, "\n") + 1
	if lines > height/2 {
		lines = height / 2
		section = clampLines(section, lines)
	}
	return section, lines
}

// renderTranscriptTail builds the transcript line stream and returns the
// visible window for the given height, honoring the operator's tailScroll
// offset.
func (m *model) renderTranscriptTail(width, height int) string {
	if len(m.tailMessages) == 0 {
		return subtitleStyle.Render("(no messages yet)")
	}
	bodyWidth := width - 1
	if bodyWidth < 20 {
		bodyWidth = 20
	}

	var lines []string
	addBlock := func(block []string, leadBlank bool) {
		if len(block) == 0 {
			return
		}
		if leadBlank && len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, block...)
	}

	for i, msg := range m.tailMessages {
		// Tool calls and their results render as one line each; keep a
		// run of them together as a single block.
		leadBlank := i > 0 && msg.Kind != transcript.KindToolResult &&
			!(msg.Kind == transcript.KindToolUse && isToolKind(m.tailMessages[i-1].Kind))
		addBlock(renderPreviewMessage(msg, bodyWidth), leadBlank)
	}
	if height < 1 {
		height = 1
	}
	// Clamp scroll: 0 = newest at bottom; max = top of buffer.
	maxScroll := len(lines) - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.tailScroll > maxScroll {
		m.tailScroll = maxScroll
	}
	end := len(lines) - m.tailScroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 1 {
		end = 1
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end], "\n")
}

// approvalsForSelected returns pending approvals for the currently selected
// session, in oldest-first order.
func (m *model) approvalsForSelected() []mdl.Approval {
	sid := m.currentSessionID()
	if sid == "" {
		return nil
	}
	out := make([]mdl.Approval, 0)
	for _, a := range m.approvals {
		if a.SessionID == sid {
			out = append(out, a)
		}
	}
	return out
}

// renderApprovalSection draws a compact "needs your decision" panel pinned
// to the bottom of the right pane. We use a yellow rule above so it's
// visually distinct from the transcript text.
func (m *model) renderApprovalSection(approvals []mdl.Approval, width int) string {
	if len(approvals) == 0 {
		return ""
	}
	charW := runewidth.RuneWidth('─')
	if charW < 1 {
		charW = 1
	}
	rule := pendingStyle.Render(strings.Repeat("─", width/charW))
	title := pendingStyle.Render(fmt.Sprintf("⚠ %d pending — 'a' allow · 'A' keep-allow · 'd' deny (oldest first)", len(approvals)))

	var blocks []string
	blocks = append(blocks, rule, title)
	bodyWidth := width - 4
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	for _, a := range approvals {
		header := approvalLabelStyle.Render(fmt.Sprintf(" %s ", a.Tool)) +
			approvalRowStyle.Render(strings.Repeat(" ", maxInt(0, width-runewidth.StringWidth(" "+a.Tool+" "))))
		blocks = append(blocks, header)
		input := summarizeApprovalInput(a)
		for _, chunk := range wrapToWidth(input, bodyWidth) {
			content := "  " + chunk
			pad := width - runewidth.StringWidth(content)
			if pad < 0 {
				pad = 0
			}
			blocks = append(blocks, approvalRowStyle.Render(content+strings.Repeat(" ", pad)))
		}
	}
	return strings.Join(blocks, "\n")
}

// summarizeApprovalInput pulls a one-or-two-line summary out of the JSON
// blob the hook captured. Falls back to the raw JSON for tool kinds we
// don't have a special-case for.
func summarizeApprovalInput(a mdl.Approval) string {
	if len(a.ToolInput) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(a.ToolInput, &m); err != nil {
		return string(a.ToolInput)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	switch a.Tool {
	case "Bash":
		return pick("command")
	case "Edit", "Write", "Read":
		return pick("file_path")
	case "Glob", "Grep":
		return pick("pattern", "query")
	case "WebFetch", "WebSearch":
		return pick("url", "query")
	}
	if s := pick("file_path", "command", "url", "query", "pattern"); s != "" {
		return s
	}
	return string(a.ToolInput)
}

func (m *model) unusedRenderApprovalsBody(height int) string {
	if len(m.approvals) == 0 {
		return lipgloss.NewStyle().Width(m.width).Height(height).
			Render(subtitleStyle.Render("no pending approvals"))
	}
	var b strings.Builder
	for i, a := range m.approvals {
		marker := "  "
		if i == m.selAppr {
			marker = "▶ "
		}
		row := fmt.Sprintf("%s%s  %-12s  %-7s  %s",
			marker,
			a.Timestamp.Local().Format("15:04:05"),
			shortID(a.SessionID),
			a.Tool,
			shorten(string(a.ToolInput), m.width-50),
		)
		if i == m.selAppr {
			row = selectedRow.Render(row)
		}
		b.WriteString(row + "\n")
	}
	return lipgloss.NewStyle().Width(m.width).Render(b.String())
}

func (m *model) transcriptVisibleHeight() int {
	h := m.height - 4 // header(2) + footer(1) + title bar(1)
	if h < 5 {
		h = 5
	}
	return h
}

func (m *model) renderTranscriptBody(height int) string {
	if len(m.transcriptMessages) == 0 {
		return lipgloss.NewStyle().Width(m.width).Height(height).Render(
			subtitleStyle.Render("(empty transcript)"))
	}
	rendered := m.renderedTranscriptLines()
	max := m.maxTranscriptScroll(height)
	if m.transcriptScroll > max {
		m.transcriptScroll = max
	}
	end := m.transcriptScroll + height
	if end > len(rendered) {
		end = len(rendered)
	}
	body := strings.Join(rendered[m.transcriptScroll:end], "\n")

	titleBar := titleStyle.Render(shorten(m.transcriptTitle, m.width-30)) + "  " +
		subtitleStyle.Render(fmt.Sprintf("%d-%d / %d lines", m.transcriptScroll+1, end, len(rendered)))

	return lipgloss.JoinVertical(lipgloss.Left, titleBar, body)
}

// renderedTranscriptLines flattens the parsed messages into wrapped display
// lines so scroll math works in line units.
func (m *model) renderedTranscriptLines() []string {
	width := m.width - 2
	if width < 30 {
		width = 30
	}
	var out []string
	for i, msg := range m.transcriptMessages {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, renderTranscriptMessage(msg, width)...)
	}
	return out
}

func (m *model) maxTranscriptScroll(height int) int {
	total := len(m.renderedTranscriptLines())
	max := total - height
	if max < 0 {
		return 0
	}
	return max
}

// Per-role styles. Each row in the transcript pane is padded to the pane
// width and rendered with the background color so the color extends across
// the full row, making message boundaries obvious.
var (
	userRowStyle        = lipgloss.NewStyle().Background(lipgloss.Color("17")).Foreground(lipgloss.Color("15")) // dark blue
	userLabelStyle      = lipgloss.NewStyle().Background(lipgloss.Color("12")).Foreground(lipgloss.Color("15")).Bold(true)
	assistantRowStyle   = lipgloss.NewStyle().Background(lipgloss.Color("22")).Foreground(lipgloss.Color("15")) // dark green
	assistantLabelStyle = lipgloss.NewStyle().Background(lipgloss.Color("10")).Foreground(lipgloss.Color("0")).Bold(true)
	toolRowStyle        = lipgloss.NewStyle().Background(lipgloss.Color("23")).Foreground(lipgloss.Color("15")) // teal
	toolLabelStyle      = lipgloss.NewStyle().Background(lipgloss.Color("14")).Foreground(lipgloss.Color("0")).Bold(true)
	resultRowStyle      = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("7")) // dark gray
	resultLabelStyle    = lipgloss.NewStyle().Background(lipgloss.Color("242")).Foreground(lipgloss.Color("0")).Bold(true)
	errorRowStyle       = lipgloss.NewStyle().Background(lipgloss.Color("52")).Foreground(lipgloss.Color("15")) // dark red
	errorLabelStyle     = lipgloss.NewStyle().Background(lipgloss.Color("9")).Foreground(lipgloss.Color("15")).Bold(true)
	thinkingRowStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	thinkingLabelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true).Bold(true)
	systemRowStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	systemLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)
)

// isToolKind reports tool_use / tool_result, which render compactly.
func isToolKind(k transcript.Kind) bool {
	return k == transcript.KindToolUse || k == transcript.KindToolResult
}

// renderToolLine renders a tool call or its result as a single line, the
// way Claude Code's own console does: "⏺ Bash  git status" and
// "  ⎿ first line of output  (+12 lines)".
func renderToolLine(msg transcript.Message, width int) []string {
	var text string
	var style lipgloss.Style
	if msg.Kind == transcript.KindToolUse {
		text = "⏺ " + msg.Tool
		if in := firstLine(msg.ToolInput); in != "" {
			text += "  " + in
		}
		style = toolRowStyle
	} else {
		body := strings.TrimSpace(msg.Text)
		first := firstLine(body)
		if first == "" {
			first = "(no output)"
		}
		if msg.IsError {
			first = "error: " + first
		}
		text = "  ⎿ " + first
		switch n := strings.Count(body, "\n"); {
		case n == 1:
			text += "  (+1 line)"
		case n > 1:
			text += fmt.Sprintf("  (+%d lines)", n)
		}
		style = resultRowStyle
		if msg.IsError {
			style = errorRowStyle
		}
	}
	text = runewidth.Truncate(text, width, "…")
	return []string{style.Render(runewidth.FillRight(text, width))}
}

// firstLine returns the first non-blank line of s, whitespace-trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// renderPreviewMessage is renderTranscriptMessage for the right-pane
// preview: tool traffic collapses to one line each. The full viewer (o)
// keeps the expanded rendering.
func renderPreviewMessage(msg transcript.Message, width int) []string {
	if isToolKind(msg.Kind) {
		return renderToolLine(msg, width)
	}
	return renderTranscriptMessage(msg, width)
}

func renderTranscriptMessage(msg transcript.Message, width int) []string {
	var label string
	var rowStyle, labelStyle lipgloss.Style
	body := msg.Text
	// labelIndent shifts the label-row to the right; bodyIndent does the same
	// for body rows. Tool results indent deeper so they read as "belonging"
	// to the tool_use just above them.
	labelIndent := ""
	bodyIndent := "  "

	switch msg.Kind {
	case transcript.KindUser:
		label, rowStyle, labelStyle = "USER", userRowStyle, userLabelStyle
	case transcript.KindAssistant:
		label, rowStyle, labelStyle = "CLAUDE", assistantRowStyle, assistantLabelStyle
	case transcript.KindThinking:
		label, rowStyle, labelStyle = "thinking", thinkingRowStyle, thinkingLabelStyle
	case transcript.KindToolUse:
		label, rowStyle, labelStyle = "TOOL "+msg.Tool, toolRowStyle, toolLabelStyle
		body = msg.ToolInput
	case transcript.KindToolResult:
		labelIndent = "  "
		bodyIndent = "      "
		if msg.IsError {
			label, rowStyle, labelStyle = "↳ ERROR", errorRowStyle, errorLabelStyle
		} else {
			label, rowStyle, labelStyle = "↳ result", resultRowStyle, resultLabelStyle
		}
	case transcript.KindSystem:
		label, rowStyle, labelStyle = "system", systemRowStyle, systemLabelStyle
	default:
		return nil
	}

	var out []string
	labelText := labelIndent + " " + label + " "
	pad := width - runewidth.StringWidth(labelText)
	if pad < 0 {
		pad = 0
	}
	// Render label-area background first so the indent on the left is also
	// drawn with the row's bg, then the bright label, then padding.
	labelRow := rowStyle.Render(labelIndent) + labelStyle.Render(" "+label+" ") + rowStyle.Render(strings.Repeat(" ", pad))
	out = append(out, labelRow)

	bodyWidth := width - runewidth.StringWidth(bodyIndent)
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return out
	}
	for _, raw := range strings.Split(body, "\n") {
		// Claude often emits trailing spaces on lines (markdown soft-break
		// convention) and sometimes \r before the \n on transcripts that
		// originated outside Unix. Trim every trailing Unicode whitespace
		// rune so the row's background color doesn't extend past the
		// actual content. \n is impossible here (we just split on it), so
		// IsSpace is the right cut.
		raw = strings.TrimRightFunc(raw, unicode.IsSpace)
		if raw == "" {
			out = append(out, rowStyle.Render(strings.Repeat(" ", width)))
			continue
		}
		for _, chunk := range wrapToWidth(raw, bodyWidth) {
			content := bodyIndent + chunk
			padN := width - runewidth.StringWidth(content)
			if padN < 0 {
				padN = 0
			}
			out = append(out, rowStyle.Render(content+strings.Repeat(" ", padN)))
		}
	}
	return out
}

// wrapToWidth splits s into chunks whose display width is at most width.
// We don't break on word boundaries — for chat content, hard wrapping reads
// fine and is simpler/safer with mixed-width characters.
func wrapToWidth(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var out []string
	var line []rune
	var lineW int
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if lineW+rw > width && lineW > 0 {
			out = append(out, string(line))
			line = line[:0]
			lineW = 0
		}
		line = append(line, r)
		lineW += rw
	}
	if len(line) > 0 {
		out = append(out, string(line))
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// activeSpinnerFrames is the per-tick glyph cycle for "claude is working
// right now" rows. Braille cells render at single-cell width on every
// terminal worth supporting and the rotation is recognisable as motion
// even at small sizes.
var activeSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func renderStatusDot(s mdl.SessionStatus, tick int) string {
	switch s {
	case mdl.StatusActive:
		// Spin only on the active state — idle / recent / stopped get a
		// static dot so motion in the list reads as "this one is doing
		// something" and not visual noise.
		frame := activeSpinnerFrames[((tick%len(activeSpinnerFrames))+len(activeSpinnerFrames))%len(activeSpinnerFrames)]
		return statusActive.Bold(true).Render(frame)
	case mdl.StatusIdle:
		return statusIdle.Render("●")
	case mdl.StatusRecent:
		return statusRecent.Render("●")
	case mdl.StatusStopped:
		return statusStop.Render("●")
	}
	return "·"
}

func eventStyle(t mdl.EventType) lipgloss.Style {
	switch t {
	case mdl.EventPermissionRequest:
		return pendingStyle
	case mdl.EventPostToolFailure:
		return statusStop
	case mdl.EventUserPrompt:
		return titleStyle
	case mdl.EventPreTool, mdl.EventPostTool:
		return statusActive
	}
	return subtitleStyle
}

func pendingTag(n int) string {
	if n == 0 {
		return ""
	}
	return pendingStyle.Render(fmt.Sprintf("●%d", n))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// shorten truncates s by display width, not rune count, so wide characters
// (Japanese, Chinese, emoji) don't cause line wraps even though their rune
// count looks fine.
func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if n <= 0 {
		return ""
	}
	return runewidth.Truncate(s, n, "…")
}

func baseLast(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

func ttyForPID(pid int) string {
	for _, fd := range []string{"0", "1", "2"} {
		t, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd))
		if err != nil {
			continue
		}
		if strings.HasPrefix(t, "/dev/pts/") {
			return t
		}
	}
	return ""
}

// sessionTime renders a session's list time per the time-format
// setting, padded to a fixed width so the titles line up: relative
// ("4m  ", 4 cols) or absolute ("09/30 18:45", 11 cols).
func (m *model) sessionTime(t time.Time) string {
	if m.settings.TimeFormat == "absolute" {
		if t.IsZero() {
			return runewidth.FillRight("-", 11)
		}
		return t.Local().Format("01/02 15:04")
	}
	return runewidth.FillRight(humanDuration(time.Since(t)), 4)
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
