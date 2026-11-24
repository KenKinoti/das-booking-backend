package outbox

import (
	"context"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"
)

const lockKey = "outbox:engine"

// RunResult summarises one engine pass.
type RunResult struct {
	Ran         bool     `json:"ran"`
	Released    int      `json:"released"`
	Sent        int      `json:"sent"`
	Failed      int      `json:"failed"`
	Superseded  int      `json:"superseded"`
	Stale       int      `json:"stale"`
	Interrupted int      `json:"interrupted"`
	SoonEmails  int      `json:"soon_emails"`
	DailyEmails int      `json:"daily_emails"`
	Notes       []string `json:"notes"`
}

// withLock runs fn while holding the advisory lock (one leader).
func (s *Service) withLock(ctx context.Context, wait time.Duration, fn func()) (bool, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	got := false
	err := s.db().WithContext(ctx).Connection(func(tx *gorm.DB) error {
		end := time.Now().Add(wait)
		for {
			if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", lockKey).Scan(&got).Error; err != nil {
				return err
			}
			if got || time.Now().After(end) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", lockKey)
		fn()
		return nil
	})
	return got, err
}

// RunOnce does one pass under the lock.
func (s *Service) RunOnce(ctx context.Context, wait time.Duration) (RunResult, error) {
	r := RunResult{Notes: []string{}}
	ok, err := s.withLock(ctx, wait, func() {
		r.Ran = true
		s.interrupted(&r)
		s.releaseDue(ctx, &r)
		s.recheck(ctx, &r)
		s.markStale(&r)
		s.digests(ctx, &r)
	})
	if err == nil && !ok {
		err = errors.New("another server is running Pending review right now — try again in a moment")
	}
	return r, err
}

// Start runs the engine every interval.
func (s *Service) Start(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("outbox: run failed: %v", rec)
					}
				}()
				r, err := s.RunOnce(ctx, 0)
				if err == nil && (r.Released+r.Superseded+r.SoonEmails+r.DailyEmails+r.Interrupted) > 0 {
					log.Printf("outbox: %d released (%d sent, %d failed), %d superseded, %d stale, %d update email(s)", r.Released, r.Sent, r.Failed,
						r.Superseded, r.Stale, r.SoonEmails+r.DailyEmails)
				}
			}()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// interrupted: an item left "sending" by a server that stopped is failed
// (never re-sent automatically — it may have gone).
func (s *Service) interrupted(r *RunResult) {
	now := s.now()
	var stuck []Item
	s.db().Where("status = ? AND claimed_at < ?", StatusSending, now.Add(-StaleSending)).Limit(200).Find(&stuck)
	for i := range stuck {
		it := &stuck[i]
		res := s.db().Model(&Item{}).Where("id = ? AND status = ?", it.ID, StatusSending).Updates(map[string]any{"status": StatusFailed,
			"error": "Interrupted while sending (the server restarted) — check the record before trying again, it may already have gone.", "claimed_at": nil, "updated_at": now})
		if res.RowsAffected > 0 {
			r.Interrupted++
			s.event(it, "failed", nil, "Interrupted while sending — check before trying again", nil, "")
		}
	}
}

// releaseDue sends approved items whose time has come.
func (s *Service) releaseDue(ctx context.Context, r *RunResult) {
	now := s.now()
	var due []string
	s.db().Model(&Item{}).Where("status = ? AND (send_at IS NULL OR send_at <= ?)", StatusApproved, now).Order("send_at NULLS FIRST, created_at").Limit(100).Pluck("id", &due)
	for _, id := range due {
		if ctx.Err() != nil {
			return
		}
		it, _ := s.release(ctx, id, []string{StatusApproved}, nil)
		if it == nil {
			continue
		}
		r.Released++
		switch it.Status {
		case StatusSent:
			r.Sent++
		case StatusFailed:
			r.Failed++
		case StatusSuperseded:
			r.Superseded++
		}
	}
}

// recheck asks the modules whether waiting items are still needed.
func (s *Service) recheck(ctx context.Context, r *RunResult) {
	now := s.now()
	var list []Item
	s.db().Where("status IN ? AND (checked_at IS NULL OR checked_at < ?)", []string{StatusPending, StatusFailed, StatusApproved}, now.Add(-RecheckEvery)).
		Order("checked_at NULLS FIRST, created_at").Limit(200).Find(&list)
	for i := range list {
		if ctx.Err() != nil {
			return
		}
		it := &list[i]
		s.db().Model(&Item{}).Where("id = ?", it.ID).Update("checked_at", now)
		rel := releaserOf(it.Module)
		if rel == nil {
			continue
		}
		if why := safeRecheck(ctx, rel, it); why != "" {
			if s.supersede(ctx, it, why, []string{StatusPending, StatusFailed, StatusApproved}) {
				r.Superseded++
			}
		}
	}
}

// markStale writes the "stale" event once per item (the flag itself is computed).
func (s *Service) markStale(r *RunResult) {
	now := s.now()
	var orgs []string
	s.db().Model(&Item{}).Distinct("organization_id").Where("status IN ? AND dated AND stale_at IS NULL", []string{StatusPending, StatusFailed}).Pluck("organization_id", &orgs)
	for _, org := range orgs {
		st := s.SettingsFor(org)
		cut := now.Add(-time.Duration(st.StaleDays) * 24 * time.Hour)
		var list []Item
		s.db().Where("organization_id = ? AND status IN ? AND dated AND stale_at IS NULL AND created_at <= ?", org, []string{StatusPending, StatusFailed}, cut).Limit(500).Find(&list)
		for i := range list {
			it := &list[i]
			if s.db().Model(&Item{}).Where("id = ? AND stale_at IS NULL", it.ID).Update("stale_at", now).RowsAffected > 0 {
				r.Stale++
				s.event(it, "stale", nil, "Waiting more than "+plural(st.StaleDays, "day", "days")+" — it may be out of date; approve it or reject it", nil, "")
			}
		}
	}
}
