package server

import (
	"context"
	"log"
	"time"

	"github.com/takumanakagame/ccmanage/internal/settings"
)

// maybeAutoArchive archives sessions idle longer than auto_archive_days,
// at most once per local calendar day: the collector's first start of the
// day runs it (discoveryLoop's first tick), and a collector that stays up
// runs it again on its first tick after midnight. The day it last ran is
// kept in the settings table so restarts within a day don't repeat it.
func (s *Server) maybeAutoArchive(ctx context.Context, now time.Time) {
	cfg, err := settings.Load(ctx, s.db)
	if err != nil || cfg.AutoArchiveDays <= 0 {
		return
	}
	today := now.Local().Format("2006-01-02")
	if last, _ := s.db.GetSetting(ctx, settings.KeyAutoArchiveLast); last == today {
		return
	}
	n, err := s.db.AutoArchive(ctx, now.AddDate(0, 0, -cfg.AutoArchiveDays))
	if err != nil {
		log.Printf("auto-archive: %v", err)
		return
	}
	if err := s.db.SetSetting(ctx, settings.KeyAutoArchiveLast, today); err != nil {
		log.Printf("auto-archive: %v", err)
	}
	if n > 0 {
		log.Printf("auto-archive: archived %d sessions idle > %d days", n, cfg.AutoArchiveDays)
	}
}
