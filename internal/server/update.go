package server

// Updating the collector's own ccdash (docs/decisions/0011-auto-update.md).
//
// GET /api/update reports the running version, the newest release in the
// channel (checked at most every updateCheckEvery, so the hub's 4 s watch
// never reaches GitHub) and how the last update went. POST /api/update
// (the portal's / the app's Update button) and — with the auto_update
// setting on — a nightly run in updateLoop download the release with
// selfupdate.Run (fixed repository, sha256 checked), replace the binary
// and schedule a restart for when no hosted session is busy (the same
// wait as POST /api/restart {"when":"idle"}). Claude takes no part, so a
// machine whose Claude Code login has expired still updates.
//
// The request can only say "update now" and pick stable / dev: never a
// URL, a version or a file. It is a person's (hubAllowed: attach on,
// never a peer).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/buildinfo"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/redact"
	"github.com/TakumaNakagame/ccdash/internal/selfupdate"
	"github.com/TakumaNakagame/ccdash/internal/settings"
)

const (
	updateCheckEvery = 6 * time.Hour
	updateLoopEvery  = time.Hour
	updateTimeout    = 5 * time.Minute
	// The nightly window (local time) for auto_update.
	updateNightFrom, updateNightTo = 3, 5
	keyAutoUpdateLast              = "auto_update_last" // local date of the last nightly attempt
)

// Swapped by tests.
var (
	runSelfUpdate = selfupdate.Run
	latestRelease = selfupdate.LatestTag
	updateNow     = time.Now
)

type updater struct {
	mu        sync.Mutex
	latest    string
	channel   string // the channel latest was checked in
	checkedAt time.Time
	checkErr  string
	running   bool
	installed string
	last      *model.UpdateResult
}

// updateChannel is the channel to follow: auto_update's when it is on,
// else update_channel.
func updateChannel(cfg settings.Settings) selfupdate.Channel {
	name := cfg.UpdateChannel
	if cfg.AutoUpdate == "stable" || cfg.AutoUpdate == "dev" {
		name = cfg.AutoUpdate
	}
	ch, err := selfupdate.ParseChannel(name)
	if err != nil {
		return selfupdate.ChannelStable
	}
	return ch
}

// checkUpdate refreshes the newest release when the last look is stale
// (or was in another channel).
func (s *Server) checkUpdate(ctx context.Context, ch selfupdate.Channel, force bool) {
	s.upd.mu.Lock()
	fresh := s.upd.channel == string(ch) && updateNow().Sub(s.upd.checkedAt) < updateCheckEvery
	s.upd.mu.Unlock()
	if fresh && !force {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tag, err := latestRelease(ctx, ch)
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	s.upd.checkedAt, s.upd.channel = updateNow(), string(ch)
	if err != nil {
		s.upd.checkErr = truncate(redact.String(err.Error()), 200)
		return
	}
	s.upd.latest, s.upd.checkErr = tag, ""
}

func (s *Server) updateState(ctx context.Context) model.UpdateState {
	cfg, _ := settings.Load(ctx, s.db)
	ch := updateChannel(cfg)
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	st := model.UpdateState{
		Current: buildinfo.Version, Channel: string(ch), Auto: cfg.AutoUpdate,
		CheckedAt: s.upd.checkedAt, CheckErr: s.upd.checkErr, Running: s.upd.running,
		Installed: s.upd.installed, Last: s.upd.last,
	}
	if s.upd.channel == string(ch) {
		st.Latest = s.upd.latest
	}
	have := buildinfo.Version
	if s.upd.installed != "" {
		have = s.upd.installed
	}
	st.Available = st.Latest != "" && selfupdate.Newer(have, st.Latest)
	st.RestartNeeded = s.upd.installed != "" && !s.canRestartSelf()
	return st
}

func (s *Server) canRestartSelf() bool { return s.CanRestart && s.cancelFn != nil }

// handleUpdateState is GET /api/update. A stale look is refreshed in the
// background; the reply never waits on GitHub.
func (s *Server) handleUpdateState(w http.ResponseWriter, r *http.Request) {
	cfg, _ := settings.Load(r.Context(), s.db)
	go s.checkUpdate(context.Background(), updateChannel(cfg), false)
	writeOK(w, s.updateState(r.Context()))
}

// handleUpdate is POST /api/update {"channel"?: "stable" | "dev"}: start
// an update in the background and answer at once (the client polls
// GET /api/update). 409 while one runs.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel string `json:"channel"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad body: %w", err))
			return
		}
	}
	cfg, _ := settings.Load(r.Context(), s.db)
	ch := updateChannel(cfg)
	if body.Channel != "" {
		c, err := selfupdate.ParseChannel(body.Channel)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		ch = c
	}
	if !s.beginUpdate() {
		writeErr(w, http.StatusConflict, errors.New("an update is already running"))
		return
	}
	go s.runUpdate(ch, "portal")
	writeOK(w, s.updateState(r.Context()))
}

func (s *Server) beginUpdate() bool {
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	if s.upd.running {
		return false
	}
	s.upd.running = true
	return true
}

// runUpdate downloads and installs the newest release in ch, then
// schedules the restart into it. Call after beginUpdate.
func (s *Server) runUpdate(ch selfupdate.Channel, by string) model.UpdateResult {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	s.upd.mu.Lock()
	have := buildinfo.Version
	if s.upd.installed != "" {
		have = s.upd.installed
	}
	s.upd.mu.Unlock()
	log.Printf("update: looking for a newer ccdash than %s (channel %s, by %s)", have, ch, by)
	res, err := runSelfUpdate(ctx, have, ch)
	out := model.UpdateResult{At: updateNow(), From: buildinfo.Version, To: res.NewVersion, By: by, OK: err == nil}
	if err != nil {
		out.Error = truncate(redact.String(err.Error()), 200)
		log.Printf("update: %s", out.Error)
	}
	s.upd.mu.Lock()
	s.upd.running = false
	s.upd.last = &out
	if res.NewVersion != "" {
		s.upd.latest, s.upd.channel, s.upd.checkedAt, s.upd.checkErr = res.NewVersion, string(ch), updateNow(), ""
	}
	installed := err == nil && !res.NoOp
	if installed {
		s.upd.installed = res.NewVersion
	}
	pending := s.upd.installed != ""
	s.upd.mu.Unlock()
	switch {
	case err != nil:
	case installed:
		log.Printf("update: installed %s at %s", res.NewVersion, res.BinaryPath)
	default:
		log.Printf("update: %s", res.Reason)
	}
	// Restart into what is on disk — also when this run found nothing new
	// but an earlier one installed a version not running yet.
	if err == nil && pending && s.canRestartSelf() {
		s.scheduleRestart(restartReq{When: model.RestartWhenIdle, By: "update"})
	}
	return out
}

// updateLoop keeps the newest-release look fresh and, with auto_update
// on, updates once a night.
func (s *Server) updateLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute): // let the collector settle first
	}
	t := time.NewTicker(updateLoopEvery)
	defer t.Stop()
	for {
		s.updateTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) updateTick(ctx context.Context) {
	cfg, err := settings.Load(ctx, s.db)
	if err != nil {
		return
	}
	ch := updateChannel(cfg)
	s.checkUpdate(ctx, ch, false)
	if cfg.AutoUpdate != "stable" && cfg.AutoUpdate != "dev" {
		return
	}
	now := updateNow()
	if h := now.Hour(); h < updateNightFrom || h >= updateNightTo {
		return
	}
	today := now.Format("2006-01-02")
	if v, _ := s.db.GetSetting(ctx, keyAutoUpdateLast); v == today {
		return
	}
	if !s.updateState(ctx).Available {
		return
	}
	_ = s.db.SetSetting(ctx, keyAutoUpdateLast, today) // one try a night, success or not
	if !s.beginUpdate() {
		return
	}
	s.runUpdate(ch, "auto")
}
