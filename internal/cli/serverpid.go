package cli

// server.pid: which process is the collector `ccdash server` runs, so the
// TUI can stop one that no longer answers /shutdown. A PID alone can be
// stale — the server may be long gone and its number given to some other
// process — so the file also records the process's start time and the
// address it serves, and nothing is signalled unless the process with that
// PID still started at that time and still runs `ccdash … server`.
//
// Start time and arguments come from ps (`-o lstart=` / `-o args=`), which
// both macOS's BSD ps and Linux's procps understand.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/TakumaNakagame/ccdash/internal/paths"
)

// serverPIDRecord is server.pid. Files written before this format hold just
// the number (Start and Addr empty).
type serverPIDRecord struct {
	PID   int    `json:"pid"`
	Addr  string `json:"addr,omitempty"`
	Start string `json:"start,omitempty"` // ps lstart of the process
}

func serverPIDFile() (string, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "server.pid"), nil
}

// saveServerPID records pid as the collector serving addr.
func saveServerPID(pid int, addr string) {
	p, err := serverPIDFile()
	if err != nil {
		return
	}
	start, _ := procStart(pid)
	b, _ := json.Marshal(serverPIDRecord{PID: pid, Addr: addr, Start: start})
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// removeServerPID deletes server.pid if it still names pid.
func removeServerPID(pid int) {
	p, err := serverPIDFile()
	if err != nil {
		return
	}
	if rec, err := loadServerPID(); err == nil && rec.PID == pid {
		_ = os.Remove(p)
	}
}

func loadServerPID() (serverPIDRecord, error) {
	p, err := serverPIDFile()
	if err != nil {
		return serverPIDRecord{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return serverPIDRecord{}, err
	}
	return parseServerPID(b)
}

func parseServerPID(b []byte) (serverPIDRecord, error) {
	var rec serverPIDRecord
	s := strings.TrimSpace(string(b))
	if pid, err := strconv.Atoi(s); err == nil {
		rec.PID = pid // the old format
	} else if err := json.Unmarshal([]byte(s), &rec); err != nil {
		return rec, fmt.Errorf("server.pid: %w", err)
	}
	if rec.PID <= 0 {
		return rec, errors.New("server.pid: no pid")
	}
	return rec, nil
}

// verifyServerPID reports whether rec still names the collector serving
// addr: alive, started when recorded (when the record has a start time),
// serving the same port (when it has an address), and running
// `ccdash … server`. The error says why not.
func verifyServerPID(rec serverPIDRecord, addr string) error {
	if !pidAlive(rec.PID) {
		return fmt.Errorf("pid %d is not running", rec.PID)
	}
	if rec.Addr != "" && portOf(rec.Addr) != portOf(addr) {
		return fmt.Errorf("pid %d serves %s, not %s", rec.PID, rec.Addr, addr)
	}
	if rec.Start != "" {
		start, err := procStart(rec.PID)
		if err != nil {
			return fmt.Errorf("pid %d: read start time: %w", rec.PID, err)
		}
		if start != rec.Start {
			return fmt.Errorf("pid %d started %s, not %s: the number now belongs to another process", rec.PID, start, rec.Start)
		}
	}
	args, err := procArgs(rec.PID)
	if err != nil {
		return fmt.Errorf("pid %d: read arguments: %w", rec.PID, err)
	}
	if !isServerArgs(args) {
		return fmt.Errorf("pid %d is not a ccdash server (%q)", rec.PID, args)
	}
	return nil
}

// isServerArgs: a ps args line for `ccdash server` — an argument whose
// base name is ccdash (the binary, wherever it sits) directly followed by
// the word server.
func isServerArgs(args string) bool {
	fs := strings.Fields(args)
	for i := 0; i+1 < len(fs); i++ {
		if filepath.Base(fs[i]) == "ccdash" && fs[i+1] == "server" {
			return true
		}
	}
	return false
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return addr
}

// signalServerPID sends SIGTERM to the collector server.pid names, only
// after verifyServerPID agrees it is that collector. A record that no
// longer fits is removed.
func signalServerPID(addr string) bool {
	rec, err := loadServerPID()
	if err != nil {
		return false
	}
	if err := verifyServerPID(rec, addr); err != nil {
		log.Printf("ccdash: not stopping server.pid's process: %v", err)
		removeServerPID(rec.PID)
		return false
	}
	if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil {
		log.Printf("ccdash: stop server pid %d: %v", rec.PID, err)
		return false
	}
	return true
}

func procStart(pid int) (string, error) { return psField(pid, "lstart") }
func procArgs(pid int) (string, error)  { return psField(pid, "args") }

// psField returns `ps -o <field>= -p <pid>` in the C locale (so lstart
// reads the same whichever language the operator uses).
func psField(pid int, field string) (string, error) {
	cmd := exec.Command("ps", "-o", field+"=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("ps: no %s for pid %d", field, pid)
	}
	return s, nil
}
