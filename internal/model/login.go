package model

import "time"

// LoginReason is the attention reason of a session whose turn ended
// because Claude Code's login stopped working ("Login expired · Please run
// /login"). Wire text (Japanese), like the other reasons.
const LoginReason = "ログイン切れ"

// LoginDoneReason replaces LoginReason after a new login on a session
// ccdash couldn't tell to go on (one it doesn't run, or one that stayed
// busy): the operator sends the next prompt.
const LoginDoneReason = "ログインし直しました: 続きを送ってください"

// LoginNudge is what ccdash types into a hosted session stopped on the
// login error once the machine is logged in again.
const LoginNudge = "ログインし直しました。止まったところから続けてください。"

// LoginState is this machine's Claude Code login as the collector sees it
// (GET /api/login). Claude Code keeps one login per machine (per config
// directory), so logging in again fixes every session here. The hub
// carries it per device (/api/devices → login); the portal, the Android app
// and MCP draw their notice from it.
//
// Nothing in here is secret: the sign-in URL lives only in LoginFlow.URL,
// which only GET /api/login/flow returns, and the code a person pastes back
// is never kept.
type LoginState struct {
	// Expired: the login doesn't work — `claude auth status` says not
	// logged in or past its expiry, or a session hit the login error since
	// the last login ccdash saw.
	Expired bool `json:"expired"`
	// ExpiresAt is when the login reaches its time limit (from `claude auth
	// status`); zero when unknown.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// Soon: ExpiresAt is less than LoginSoon away.
	Soon bool `json:"soon,omitempty"`
	// Sessions are the sessions stopped on the login error.
	Sessions []LoginSession `json:"sessions"`
	// CheckedAt: the last `claude auth status`; CheckErr why it failed.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	CheckErr  string    `json:"check_err,omitempty"`
	// Flow is the sign-in ccdash runs (POST /api/login/start), while one
	// runs or just ended; without its URL.
	Flow *LoginFlow `json:"flow,omitempty"`
}

// LoginSession is one session stopped on the login error.
type LoginSession struct {
	SessionID string `json:"session_id"`
	Num       int64  `json:"num,omitempty"`
	Title     string `json:"title,omitempty"`
	// Hosted: ccdash runs it, so it is told to go on after the login.
	Hosted bool `json:"hosted"`
}

// LoginFlow is one `claude auth login` the collector runs for the portal.
type LoginFlow struct {
	ID string `json:"id"`
	// State: starting → url (waiting for the code) → exchanging → done |
	// failed | cancelled.
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	// URL: the sign-in page, only from GET /api/login/flow and the start
	// reply, only while State is "url".
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
	// After "done": the account now signed in (`claude auth status`), and
	// the sessions told to go on / left for the operator.
	Account string   `json:"account,omitempty"`
	Resumed []string `json:"resumed,omitempty"`
	Left    []string `json:"left,omitempty"`
}

// Login flow states.
const (
	LoginStarting   = "starting"
	LoginURL        = "url"
	LoginExchanging = "exchanging"
	LoginDone       = "done"
	LoginFailed     = "failed"
	LoginCancelled  = "cancelled"
)

// LoginSoon is how close the login's time limit must be for a warning.
const LoginSoon = 3 * 24 * time.Hour
