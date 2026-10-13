package cli

// Running `ccdash web` in the background: `ccdash web` starts a detached
// `ccdash web --foreground` and returns; stop / restart / status find it
// through web.json in the state dir.
//
// Stopping and restarting each have one meaning:
//
//   - stop: the portal process ends (SIGTERM). A collector it runs ends
//     with it, and with that collector every claude it hosts. --resume
//     saves those sessions first (POST /shutdown?resume=1 to the
//     collector, which also ends the portal) so the next start resumes
//     them.
//   - restart: with its own collector, POST /api/restart — the sessions are
//     saved, every hosted claude is closed, and the process re-execs itself
//     (same PID, same log) and resumes them. Linked to another collector,
//     only the portal is stopped and started again; the sessions live in
//     that collector and are untouched.
//
// A collector that stops is never restarted inside the same process
// (linkCollector returns errCollectorStopped), so no resumed claude can run
// next to the one it replaces.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/takumanakagame/ccmanage/internal/paths"
)

// How `ccdash web` reaches its collector (web.json "collector").
const (
	webEmbedded = "embedded" // runs it in this process
	webLinked   = "linked"   // linked to a collector running elsewhere
)

// errCollectorStopped: the collector `ccdash web` ran was shut down
// (POST /shutdown); the portal exits with it.
var errCollectorStopped = errors.New("ccdash web: collector shut down")

// webLogMax is the size past which web.log is rotated to web.log.1 when a
// background portal starts.
const webLogMax = 10 << 20

// webState is web.json: the running portal, written by the foreground
// process (the background one included) and removed when it exits.
type webState struct {
	PID           int       `json:"pid"`
	Port          int       `json:"port"`
	URL           string    `json:"url"`
	CollectorAddr string    `json:"collectorAddr"`
	Collector     string    `json:"collector,omitempty"` // webEmbedded / webLinked
	Started       time.Time `json:"started"`

	mu sync.Mutex
}

func webStateFile() (string, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "web.json"), nil
}

func webLogPath() (string, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "web.log"), nil
}

func newWebState(port int, url, collectorAddr string) *webState {
	return &webState{PID: os.Getpid(), Port: port, URL: url, CollectorAddr: collectorAddr, Started: time.Now()}
}

func (st *webState) save() {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, err := webStateFile()
	if err != nil {
		return
	}
	b, _ := json.Marshal(st)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// setMode records how the portal reaches its collector.
func (st *webState) setMode(m string) {
	st.mu.Lock()
	same := st.Collector == m
	st.Collector = m
	st.mu.Unlock()
	if !same {
		st.save()
	}
}

// remove deletes web.json if it is still ours.
func (st *webState) remove() {
	p, err := webStateFile()
	if err != nil {
		return
	}
	if cur, err := loadWebState(); err == nil && cur.PID == st.PID {
		_ = os.Remove(p)
	}
}

func loadWebState() (*webState, error) {
	p, err := webStateFile()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var st webState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// runningWeb returns the portal web.json describes when its process is
// alive and its port answers. A web.json left by a crash is removed.
func runningWeb() (*webState, bool) {
	st, err := loadWebState()
	if err != nil {
		return nil, false
	}
	if pidAlive(st.PID) && webAnswers(st.Port) {
		return st, true
	}
	if !pidAlive(st.PID) {
		if p, err := webStateFile(); err == nil {
			_ = os.Remove(p)
		}
	}
	return st, false
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func webAnswers(port int) bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/healthz", paths.DefaultHost, port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// underSupervisor: started by launchd (parent PID 1) or a systemd unit —
// a service manager that expects the process to stay in the foreground.
func underSupervisor() bool {
	return os.Getppid() == 1 || os.Getenv("INVOCATION_ID") != ""
}

// startWebDaemon starts `ccdash web --foreground` detached from this
// terminal and returns once it answers (or opens the login link of the
// one already running).
func startWebDaemon(port int, collector string, openIt bool) error {
	if st, ok := runningWeb(); ok {
		fmt.Printf("ccdash web is already running: %s (pid %d)\n", st.URL, st.PID)
		if !openIt {
			return nil
		}
		return openWebLogin(false)
	}
	if webAnswers(port) {
		// A portal without web.json (an older ccdash web in a terminal).
		fmt.Printf("ccdash web is already running on port %d (not started in the background; stop it where it runs)\n", port)
		if !openIt {
			return nil
		}
		return openWebLogin(false)
	}
	pid, logPath, err := spawnWeb(port, collector)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s:%d", paths.DefaultHost, port)
	fmt.Printf("ccdash web: running in the background on %s (pid %d)\n", url, pid)
	fmt.Printf("  log: %s\n  stop: ccdash web stop   restart: ccdash web restart   log in: ccdash web open\n", logPath)
	if openIt {
		if err := openWebLogin(false); err != nil {
			fmt.Println("ccdash web: could not open a browser:", err, "— run 'ccdash web open --print'")
		}
	}
	return nil
}

// spawnWeb launches the detached portal (its own session, stdin
// /dev/null, output appended to web.log) and waits for its port. The child
// inherits this environment unchanged, so hosted claude sessions start as
// they would from a foreground `ccdash web`.
func spawnWeb(port int, collector string) (int, string, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	logPath, err := webLogPath()
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, "", err
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > webLogMax {
		_ = os.Rename(logPath, logPath+".1")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer logFile.Close()
	cmd := exec.Command(self, "web", "--foreground", "--no-open",
		"--port", strconv.Itoa(port), "--collector-addr", collector)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return 0, logPath, fmt.Errorf("ccdash web exited at start:\n%s", logTail(logPath, 10))
		default:
		}
		if webAnswers(port) {
			return cmd.Process.Pid, logPath, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cmd.Process.Pid, logPath, fmt.Errorf("ccdash web (pid %d) didn't answer within 10s; see %s", cmd.Process.Pid, logPath)
}

// logTail returns the last n lines of the file at p.
func logTail(p string, n int) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 8192 {
		_, _ = f.Seek(-8192, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func webStopCmd() *cobra.Command {
	var resume bool
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running 'ccdash web' (and the collector + claude sessions it runs)",
		Long: `Stop the running 'ccdash web'.

When the portal runs the collector itself, the collector and every claude
session it hosts stop too. --resume saves those sessions first, and the
next 'ccdash web' (within 10 minutes) resumes them. A portal linked to a
collector that runs elsewhere (ccdash server, ccdash -k) only stops the
portal; that collector and its sessions keep running.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, ok := runningWeb()
			if !ok {
				fmt.Println("ccdash web is not running")
				return nil
			}
			return stopWeb(st, resume)
		},
	}
	c.Flags().BoolVar(&resume, "resume", false, "save the hosted claude sessions so the next 'ccdash web' resumes them")
	return c
}

// stopWeb ends the portal st describes and waits for its process to go.
func stopWeb(st *webState, resume bool) error {
	switch {
	case resume && st.Collector == webEmbedded:
		// The collector saves its sessions and shuts down, which ends the
		// portal (errCollectorStopped).
		if _, err := collectorPost(st.CollectorAddr, "/shutdown?resume=1"); err != nil {
			return fmt.Errorf("ask the collector to save its sessions: %w", err)
		}
	case resume:
		fmt.Printf("ccdash web: the collector on %s isn't this portal's; its sessions keep running\n", st.CollectorAddr)
		fallthrough
	default:
		if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop pid %d: %w", st.PID, err)
		}
	}
	if !waitPIDGone(st.PID, 15*time.Second) {
		log.Printf("ccdash web (pid %d) still running after 15s; killing it", st.PID)
		_ = syscall.Kill(st.PID, syscall.SIGKILL)
		if !waitPIDGone(st.PID, 3*time.Second) {
			return fmt.Errorf("ccdash web (pid %d) would not stop", st.PID)
		}
	}
	if p, err := webStateFile(); err == nil {
		_ = os.Remove(p)
	}
	if resume && st.Collector == webEmbedded {
		fmt.Println("ccdash web stopped; its claude sessions are saved — the next 'ccdash web' resumes them (within 10 minutes)")
	} else {
		fmt.Println("ccdash web stopped")
	}
	return nil
}

func waitPIDGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// collectorPost POSTs path to the collector at addr with the local token.
func collectorPost(addr, path string) ([]byte, error) {
	tok, err := loadToken()
	if err != nil {
		return nil, fmt.Errorf("read the collector token: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Ccdash-Token", tok)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return b, fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func webRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the running 'ccdash web'; the claude sessions it hosts are resumed",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, ok := runningWeb()
			if !ok {
				return fmt.Errorf("ccdash web is not running — start it with 'ccdash web'")
			}
			if st.Collector != webEmbedded {
				// Only the portal restarts; the sessions live in the other
				// collector.
				if err := stopWeb(st, false); err != nil {
					return err
				}
				pid, logPath, err := spawnWeb(st.Port, st.CollectorAddr)
				if err != nil {
					return err
				}
				fmt.Printf("ccdash web restarted on %s (pid %d, log %s)\n", st.URL, pid, logPath)
				return nil
			}
			b, err := collectorPost(st.CollectorAddr, "/api/restart")
			if err != nil {
				return err
			}
			var r struct {
				Resuming int `json:"resuming"`
			}
			_ = json.Unmarshal(b, &r)
			// The process re-execs itself (same PID); wait for it to answer
			// again with its collector.
			time.Sleep(time.Second)
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if pidAlive(st.PID) && webAnswers(st.Port) && pingServer(st.CollectorAddr) {
					fmt.Printf("ccdash web restarted on %s (pid %d); resuming %d claude session(s)\n", st.URL, st.PID, r.Resuming)
					return nil
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !pidAlive(st.PID) {
				lp, _ := webLogPath()
				return fmt.Errorf("ccdash web exited while restarting:\n%s", logTail(lp, 10))
			}
			return fmt.Errorf("ccdash web (pid %d) didn't come back within 20s", st.PID)
		},
	}
}

func webStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether 'ccdash web' is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, ok := runningWeb()
			if !ok {
				fmt.Println("ccdash web is not running")
				return nil
			}
			coll := st.CollectorAddr
			switch st.Collector {
			case webEmbedded:
				coll += " (run by this portal)"
			case webLinked:
				coll += " (another process's)"
			}
			lp, _ := webLogPath()
			fmt.Printf("ccdash web is running: %s\n  pid:       %d (since %s)\n  collector: %s\n  log:       %s (when started in the background)\n",
				st.URL, st.PID, st.Started.Format("2006-01-02 15:04"), coll, lp)
			return nil
		},
	}
}
