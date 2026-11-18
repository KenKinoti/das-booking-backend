package brief

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var templateName = regexp.MustCompile(`^[a-z0-9_]{1,120}$`)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// DueAt is the instant of a person's brief on local date (midnight UTC of
// that date) in loc.
func DueAt(hhmm string, loc *time.Location, date time.Time) time.Time {
	h, m := 8, 0
	if t, ok := NormalTime(hhmm); ok {
		h, _ = strconv.Atoi(t[:2])
		m, _ = strconv.Atoi(t[3:])
	}
	return time.Date(date.Year(), date.Month(), date.Day(), h, m, 0, 0, loc)
}

// DayAllowed reports whether the days rule includes date.
func DayAllowed(days string, date time.Time) bool {
	if days == DaysWeekdays {
		wd := date.Weekday()
		return wd != time.Saturday && wd != time.Sunday
	}
	return true
}

// Decision is what the scheduler does about one person and one local date.
type Decision string

const (
	Wait   Decision = "wait"   // not time yet
	Skip   Decision = "skip"   // not a brief day, or the schedule changed after that time
	Send   Decision = "send"   // due (late within the catch-up window)
	Missed Decision = "missed" // later than the catch-up window
)

// Decide works out what to do for date (local, midnight UTC) at now.
func Decide(st Settings, loc *time.Location, date, now time.Time, catchUp time.Duration) (Decision, time.Time) {
	if !st.Enabled || !DayAllowed(st.Days, date) {
		return Skip, time.Time{}
	}
	at := DueAt(st.Time, loc, date)
	if now.Before(at) {
		return Wait, at
	}
	if !st.ScheduleFrom.IsZero() && at.Before(st.ScheduleFrom) {
		return Skip, at
	}
	if now.Sub(at) <= catchUp {
		return Send, at
	}
	return Missed, at
}

// RunResult reports one scheduler run.
type RunResult struct {
	At      time.Time `json:"at"`
	Checked int       `json:"checked"`
	Sent    []string  `json:"sent"`   // snapshot ids
	Missed  []string  `json:"missed"` // snapshot ids
	Retried []string  `json:"retried"`
	Leader  bool      `json:"leader"`
}

// claim records a scheduled brief once per person and local date.
func (s *Service) claim(st Settings, tz, date string, at time.Time, status string, late bool) (*Snapshot, bool) {
	now := s.now()
	snap := &Snapshot{ID: uuid.NewString(), OrganizationID: st.OrganizationID, UserID: st.UserID, Date: date, Kind: KindScheduled, Timezone: tz,
		ScheduledFor: &at, Status: status, Late: late, CreatedAt: now, UpdatedAt: time.Now()}
	tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(snap)
	if tx.Error != nil || tx.RowsAffected != 1 {
		return nil, false
	}
	return snap, true
}

// RunDue sends every brief that is due now (and records missed ones), then
// retries channels that failed with a temporary error. Only one instance
// runs at a time (Postgres advisory lock).
func (s *Service) RunDue(ctx context.Context) RunResult {
	return s.RunDueWait(ctx, 0)
}

// RunDueWait is RunDue that waits up to wait for another instance's run to
// finish (the test clock's "run now").
func (s *Service) RunDueWait(ctx context.Context, wait time.Duration) RunResult {
	res := RunResult{At: s.now(), Sent: []string{}, Missed: []string{}, Retried: []string{}}
	deadline := time.Now().Add(wait)
	for {
		if s.withLock(ctx, "brief:scheduler", func() {
			res.Leader = true
			res.At = s.now()
			s.runDue(ctx, &res)
		}) || time.Now().After(deadline) || ctx.Err() != nil {
			return res
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (s *Service) runDue(ctx context.Context, res *RunResult) {
	catchUp := s.CatchUp
	if catchUp <= 0 {
		catchUp = 3 * time.Hour
	}
	s.prune()
	var list []Settings
	s.DB.Where("enabled AND user_id IN (SELECT id FROM users WHERE deleted_at IS NULL AND (is_active IS NULL OR is_active))").Find(&list)
	for _, st := range list {
		if ctx.Err() != nil {
			return
		}
		res.Checked++
		tz, loc := s.zoneOf(st)
		now := s.now()
		today := localDay(now, loc)
		for _, date := range []time.Time{today.AddDate(0, 0, -1), today} {
			d, at := Decide(st, loc, date, now, catchUp)
			if d != Send && d != Missed {
				continue
			}
			var n int64
			s.DB.Model(&Snapshot{}).Where("user_id = ? AND date = ? AND kind = ?", st.UserID, dayStr(date), KindScheduled).Count(&n)
			if n > 0 {
				continue
			}
			if d == Missed {
				if snap, ok := s.claim(st, tz, dayStr(date), at, StatusMissed, true); ok {
					res.Missed = append(res.Missed, snap.ID)
					log.Printf("brief: %s's brief for %s was missed (due %s)", st.UserID, dayStr(date), at.Format(time.RFC3339))
				}
				continue
			}
			snap, ok := s.claim(st, tz, dayStr(date), at, StatusSending, now.Sub(at) > 5*time.Minute)
			if !ok {
				continue
			}
			s.deliverScheduled(ctx, snap)
			res.Sent = append(res.Sent, snap.ID)
		}
	}
	var retries []Snapshot
	s.DB.Where("status = ? AND next_retry_at <= ?", StatusRetry, s.now()).Order("next_retry_at").Limit(100).Find(&retries)
	for i := range retries {
		if ctx.Err() != nil {
			return
		}
		s.retry(ctx, &retries[i])
		res.Retried = append(res.Retried, retries[i].ID)
	}
}

// KeepDays is how long briefs are kept (the history shows 30 days).
const KeepDays = 90

// prune removes briefs older than KeepDays (at most once an hour).
func (s *Service) prune() {
	s.mu.Lock()
	due := time.Since(s.lastPrune) > time.Hour
	if due {
		s.lastPrune = time.Now()
	}
	s.mu.Unlock()
	if !due {
		return
	}
	cut := dayStr(s.now().AddDate(0, 0, -KeepDays))
	if res := s.DB.Where("date < ?", cut).Delete(&Snapshot{}); res.RowsAffected > 0 {
		log.Printf("brief: removed %d briefs older than %d days", res.RowsAffected, KeepDays)
	}
}

func (s *Service) withLock(ctx context.Context, key string, fn func()) bool {
	got := false
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", key)
		fn()
		return nil
	})
	if err != nil {
		log.Printf("brief: lock %s: %v", key, err)
		return false
	}
	return got
}

// StartScheduler runs RunDue every `every` (BRIEF_SCHEDULER=off disables it).
func (s *Service) StartScheduler(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("brief: scheduler stopped: %v", r)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			r := s.RunDue(ctx)
			if len(r.Sent)+len(r.Missed)+len(r.Retried) > 0 {
				log.Printf("brief: %d sent, %d missed, %d retried", len(r.Sent), len(r.Missed), len(r.Retried))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// NextSend is when a person's next brief goes out (zero when it is off).
func (s *Service) NextSend(st Settings) time.Time {
	if !st.Enabled {
		return time.Time{}
	}
	_, loc := s.zoneOf(st)
	now := s.now()
	today := localDay(now, loc)
	for i := 0; i < 8; i++ {
		date := today.AddDate(0, 0, i)
		if !DayAllowed(st.Days, date) {
			continue
		}
		at := DueAt(st.Time, loc, date)
		if at.After(now) {
			var n int64
			s.DB.Model(&Snapshot{}).Where("user_id = ? AND date = ? AND kind = ?", st.UserID, dayStr(date), KindScheduled).Count(&n)
			if n == 0 {
				return at
			}
		}
	}
	return time.Time{}
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
