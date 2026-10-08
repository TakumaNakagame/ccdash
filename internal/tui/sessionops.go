package tui

// Per-session operations that talk to the local collector directly:
//
//   - c / C: next palette color / a random different one for the session;
//     the color lives on the session row, so the portal shows it too.
//   - ctrl+r: restart the session's hosted claude (POST /pty/{key}/restart:
//     kill + `claude --resume`), after a y confirmation.

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/takumanakagame/ccmanage/internal/auth"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
)

// sessionRestartedMsg reports POST /pty/{key}/restart.
type sessionRestartedMsg struct {
	sessionID string
	err       error
}

// stepColorCurrent moves the selected session's color dir steps through the
// palette, starting from the color it shows now; dir 0 picks a random other.
func (m *model) stepColorCurrent(dir int) tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	if s.SessionID == "" {
		return nil
	}
	n := len(mdl.SessionPalette)
	i := 0
	for j, c := range mdl.SessionPalette {
		if strings.EqualFold(c, s.Color()) {
			i = j
			break
		}
	}
	next := mdl.SessionPalette[((i+dir)%n+n)%n]
	if dir == 0 { // random, but never the current color
		next = mdl.SessionPalette[(i+1+rand.IntN(n-1))%n]
	}
	// Show it right away; the next poll brings the stored value back.
	m.sessions[m.selSess].ColorOverride = next
	for k := range m.allSessions {
		if m.allSessions[k].SessionID == s.SessionID {
			m.allSessions[k].ColorOverride = next
		}
	}
	sid := s.SessionID
	return func() tea.Msg {
		if err := m.store.SetColor(m.ctx, sid, next); err != nil {
			return attachDoneMsg{err: err}
		}
		return nil
	}
}

// askRestartSession arms the y/n confirmation for ctrl+r.
func (m *model) askRestartSession() {
	if len(m.sessions) == 0 {
		return
	}
	s := m.sessions[m.selSess]
	if !m.ptyAlive[s.SessionID] {
		m.flash = "this session isn't running under ccdash — enter starts it"
		return
	}
	title := s.DisplayTitle()
	if title == "" {
		title = shortID(s.SessionID)
	}
	m.awaitRestartSessionConfirm = true
	m.flash = fmt.Sprintf("restart claude in '%s' (kill + resume; a reply in progress is cut off)? press 'y' to confirm", shorten(title, 50))
}

// restartSessionCurrent restarts the selected session's hosted claude.
func (m *model) restartSessionCurrent() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	sid := m.sessions[m.selSess].SessionID
	m.flash = "restarting " + shortID(sid) + "…"
	return func() tea.Msg {
		addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/pty/"+sid+"/restart", nil)
		if err != nil {
			return sessionRestartedMsg{sessionID: sid, err: err}
		}
		if tok, err := auth.Load(); err == nil {
			req.Header.Set(auth.HeaderName, tok)
		}
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return sessionRestartedMsg{sessionID: sid, err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return sessionRestartedMsg{sessionID: sid, err: fmt.Errorf("restart: %s: %s", resp.Status, strings.TrimSpace(string(b)))}
		}
		return sessionRestartedMsg{sessionID: sid}
	}
}

// handleSessionRestarted drops the live pane's stream to the old claude so
// syncLive dials the new one under the same key.
func (m *model) handleSessionRestarted(msg sessionRestartedMsg) tea.Cmd {
	if msg.err != nil {
		m.err = msg.err
		return nil
	}
	if m.live != nil && m.live.key == msg.sessionID {
		m.closeLive()
	}
	m.flash = "restarted " + shortID(msg.sessionID) + " (claude --resume)"
	return tea.Batch(fetchPTYListCmd(), m.refresh())
}
