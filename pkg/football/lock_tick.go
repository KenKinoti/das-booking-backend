package football

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Work that happens while nobody is looking (v2.19.1). Production runs on
// Cloud Run without minimum instances: the 30-second scheduler ticker only
// runs while an instance is awake. So:
//   - GET|POST /api/v1/public/football/tick runs one scheduler pass now
//     (budgets and the advisory lock as always), at most once per
//     TickEvery — an hourly Cloud Scheduler job wakes the service;
//     FOOTBALL_TICK_TOKEN (optional) must then match X-Tick-Token / ?token=;
//   - a catch-up pass runs soon after an instance starts;
//   - any football request when the last pass is older than SelfHealAfter
//     starts one in the background (single flight).
// Locked picks no longer depend on a pass running in the last 30 minutes
// before kickoff (lock.go).

// Timings.
const (
	TickEvery     = 5 * time.Minute
	SelfHealAfter = 30 * time.Minute
	TickTimeout   = 10 * time.Minute
)

// DBReady reports whether start-up migrations finished (cmd/production sets
// it to dbstatus.Ready); the catch-up pass waits for it.
var DBReady = func() bool { return true }

// lockRuntime is per-service, in-memory state (single flight, last pass).
type lockRuntime struct {
	armed    atomic.Bool // StartScheduler ran (background passes allowed)
	healing  atomic.Bool
	lastSeen atomic.Int64 // module-clock unix ns of the newest pass known
	readAt   atomic.Int64 // wall-clock unix ns of the last read of fb_lock_state
	heals    atomic.Int64
}

var runtimes sync.Map // *Service → *lockRuntime

func (s *Service) rt() *lockRuntime {
	v, _ := runtimes.LoadOrStore(s, &lockRuntime{})
	return v.(*lockRuntime)
}

// isAutoTrigger: passes started without a person (they run the free
// sources even when no paid key is saved, like the schedule trigger).
func isAutoTrigger(t string) bool {
	switch t {
	case "schedule", "tick", "catchup", "selfheal":
		return true
	}
	return false
}

// schedulerEnvOff: FOOTBALL_SCHEDULER=off (tests / development) also turns
// off the catch-up and self-heal passes (the tick endpoint still answers).
func schedulerEnvOff() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("FOOTBALL_SCHEDULER")), "off")
}

// passOf condenses a run summary.
func passOf(sum RunSummary) PassSummary {
	notes := sum.Notes
	if notes == nil {
		notes = []string{}
	}
	return PassSummary{At: sum.At, Trigger: sum.Trigger, DurationMS: sum.DurationMS, Planned: sum.Planned, Done: sum.Done, Skipped: sum.Skipped,
		Failed: sum.Failed, Fitted: sum.Fitted, Predicted: sum.Predicted, Locked: sum.Locked, NoData: sum.NoData, PredLocked: sum.Frozen,
		Graded: sum.VGraded, Settled: sum.Settled, Notes: notes}
}

// recordPass remembers the last pass (every trigger).
func (s *Service) recordPass(sum RunSummary) {
	ps := passOf(sum)
	at := sum.At
	s.lockState()
	s.DB.Model(&LockState{}).Where("id = 1").Updates(map[string]any{"last_pass_at": at, "last_trigger": clip(sum.Trigger, 20), "last_summary": mustJSON(ps),
		"updated_at": time.Now().UTC()})
	s.rt().lastSeen.Store(at.UnixNano())
}

// TickResult is the tick endpoint's answer (counts only).
type TickResult struct {
	Ran           bool         `json:"ran"`
	Status        string       `json:"status"` // ran | ran_recently | busy | off
	Message       string       `json:"message"`
	Pass          *PassSummary `json:"pass"` // this pass, or the last one
	LastPassAt    *time.Time   `json:"last_pass_at"`
	NextAllowedAt *time.Time   `json:"next_allowed_at"`
}

// Tick runs one scheduler pass now unless one ran in the last TickEvery.
func (s *Service) Tick(ctx context.Context) TickResult {
	now := s.Now()
	s.lockState()
	s.DB.Model(&LockState{}).Where("id = 1").Updates(map[string]any{"last_tick_at": now, "ticks": gorm.Expr("ticks + 1")})
	ls := s.lockState()
	res := TickResult{Pass: ls.LastSummary, LastPassAt: ls.LastPassAt}
	st, err := s.Settings()
	if err != nil {
		res.Status, res.Message = "busy", "Settings not readable yet — try again shortly."
		return res
	}
	if !st.SchedulerOn {
		res.Status, res.Message = "off", "The football scheduler is switched off in System settings → Football data."
		return res
	}
	if ls.LastPassAt != nil {
		if d := now.Sub(*ls.LastPassAt); d >= 0 && d < TickEvery {
			next := ls.LastPassAt.Add(TickEvery)
			res.Status, res.NextAllowedAt = "ran_recently", &next
			res.Message = "A pass ran " + durationWords(d) + " ago; nothing to do (at most one pass every 5 minutes)."
			return res
		}
	}
	af, oa := s.Keys()
	sum, err := s.RunOnce(ctx, RunOpts{Trigger: "tick", NoFetch: af == "" && oa == ""})
	if errors.Is(err, ErrBusy) {
		res.Status, res.Message = "busy", "A pass is running right now."
		return res
	}
	if err != nil {
		log.Printf("football: tick: %v", err)
		res.Status, res.Message = "busy", "The pass stopped early; the next tick tries again."
		return res
	}
	ps := passOf(sum)
	at := sum.At
	res.Ran, res.Status, res.Pass, res.LastPassAt = true, "ran", &ps, &at
	res.Message = "Pass done: " + strconv.Itoa(ps.Locked) + " pick(s) locked or refreshed, " + strconv.Itoa(ps.Graded) + " graded."
	return res
}

// startCatchUp runs a pass soon after the instance starts (once migrations
// finished), unless one ran in the last TickEvery, and arms the self-heal.
func (s *Service) startCatchUp(ctx context.Context) {
	s.rt().armed.Store(true)
	delay := 20 * time.Second
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("FOOTBALL_CATCHUP_DELAY_SEC"))); err == nil && v >= 0 {
		delay = time.Duration(v) * time.Second
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("football: catch-up pass failed: %v", r)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		for i := 0; i < 180 && !DBReady(); i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		s.CatchUp(ctx)
	}()
}

// CatchUp is the start-up pass: grade what finished, lock what's upcoming,
// refresh stale fixtures (a normal pass), unless one ran in the last TickEvery.
func (s *Service) CatchUp(ctx context.Context) bool {
	st, err := s.Settings()
	if err != nil || !st.SchedulerOn {
		return false
	}
	if ls := s.lockState(); ls.LastPassAt != nil {
		if d := s.Now().Sub(*ls.LastPassAt); d >= 0 && d < TickEvery {
			return false
		}
	}
	af, oa := s.Keys()
	_, err = s.RunOnce(ctx, RunOpts{Trigger: "catchup", NoFetch: af == "" && oa == ""})
	if err != nil && !errors.Is(err, ErrBusy) {
		log.Printf("football: catch-up: %v", err)
	}
	return err == nil
}

// maybeSelfHeal starts a background pass when the last one is older than
// SelfHealAfter (single flight; never blocks the request). It reports
// whether one was started.
func (s *Service) maybeSelfHeal() bool {
	rt := s.rt()
	if schedulerEnvOff() || !rt.armed.Load() {
		return false
	}
	if time.Since(time.Unix(0, rt.readAt.Load())) > time.Minute {
		rt.readAt.Store(time.Now().UnixNano())
		if ls := s.lockState(); ls.LastPassAt != nil && ls.LastPassAt.UnixNano() > rt.lastSeen.Load() {
			rt.lastSeen.Store(ls.LastPassAt.UnixNano())
		}
	}
	if last := rt.lastSeen.Load(); last != 0 {
		if d := s.Now().Sub(time.Unix(0, last)); d >= 0 && d < SelfHealAfter {
			return false
		}
	}
	if !rt.healing.CompareAndSwap(false, true) {
		return false
	}
	rt.heals.Add(1)
	go func() {
		defer rt.healing.Store(false)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("football: self-heal pass failed: %v", r)
			}
		}()
		st, err := s.Settings()
		if err != nil || !st.SchedulerOn {
			return
		}
		af, oa := s.Keys()
		ctx, cancel := context.WithTimeout(context.Background(), TickTimeout)
		defer cancel()
		if _, err := s.RunOnce(ctx, RunOpts{Trigger: "selfheal", NoFetch: af == "" && oa == ""}); err != nil && !errors.Is(err, ErrBusy) {
			log.Printf("football: self-heal: %v", err)
		}
	}()
	return true
}

// ---------------------------------------------------------------- HTTP

// RegisterPublic mounts GET|POST /public/football/tick (no sign-in: it is
// idempotent, rate-limited and returns counts only).
func (h *Handler) RegisterPublic(public *gin.RouterGroup) {
	public.GET("/football/tick", h.tick)
	public.POST("/football/tick", h.tick)
}

func (h *Handler) tick(c *gin.Context) {
	if tok := strings.TrimSpace(os.Getenv("FOOTBALL_TICK_TOKEN")); tok != "" {
		got := strings.TrimSpace(c.GetHeader("X-Tick-Token"))
		if got == "" {
			got = strings.TrimSpace(c.Query("token"))
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			fail(c, http.StatusUnauthorized, "TICK_TOKEN", "This tick needs the X-Tick-Token header (FOOTBALL_TICK_TOKEN).")
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), TickTimeout)
	defer cancel()
	ok(c, h.S.Tick(ctx))
}

// wake is the self-heal middleware of the football routes.
func (h *Handler) wake(c *gin.Context) {
	h.S.maybeSelfHeal()
	c.Next()
}

// registerLock mounts the v2.19.1 routes:
//
//	GET /football/results        ?date=YYYY-MM-DD | days=1..14 &competition — finished matches with ✓ / ✗ and a tally
//	GET /football/lock-status    the last scheduler pass, ticks, the keyless defaults
func (h *Handler) registerLock(g, sa *gin.RouterGroup) {
	g.GET("/results", h.results)
	g.GET("/lock-status", h.lockStatus)
}

func (h *Handler) lockStatus(c *gin.Context) {
	ls := h.S.lockState()
	st, _ := h.S.Settings()
	ok(c, gin.H{"last_pass_at": ls.LastPassAt, "last_trigger": ls.LastTrigger, "last_pass": ls.LastSummary, "last_tick_at": ls.LastTickAt, "ticks": ls.Ticks,
		"scheduler_on": st.SchedulerOn, "lock_window_days": int(h.S.lockWindow() / (24 * time.Hour)), "lock_view_every_min": int(LockViewEvery / time.Minute),
		"tick_every_min": int(TickEvery / time.Minute), "self_heal_after_min": int(SelfHealAfter / time.Minute), "keyless": h.S.keylessView(),
		"tick_token_required": strings.TrimSpace(os.Getenv("FOOTBALL_TICK_TOKEN")) != ""})
}

// ---------------------------------------------------------------- keyless sources on by default

// applyKeylessDefaults switches the keyless free sources on once (v2.19.1)
// where nobody made a choice: a source that was used before and is off now
// was switched off on purpose and stays off.
func applyKeylessDefaults(db *gorm.DB, now time.Time) error {
	var ls LockState
	if db.Where("id = 1").Limit(1).Find(&ls).RowsAffected == 0 || ls.KeylessAt != nil {
		return nil
	}
	var fs FreeSettings
	if db.Where("id = 1").Limit(1).Find(&fs).RowsAffected == 0 {
		return nil
	}
	normalizeFree(&fs)
	on := []string{}
	for _, k := range KeylessDefaults {
		if fs.Enabled[k] {
			continue
		}
		if _, chosen := ls.Chosen[k]; chosen {
			continue
		}
		var used int64
		db.Model(&FetchLog{}).Where("provider = ? AND task <> 'test connection'", k).Count(&used)
		var q int64
		db.Model(&Quota{}).Where("provider = ? AND used > 0", k).Count(&q)
		if used > 0 || q > 0 {
			continue
		}
		fs.Enabled[k] = true
		on = append(on, k)
	}
	if len(on) > 0 {
		if err := db.Model(&FreeSettings{}).Where("id = 1").Update("enabled", mustJSON(fs.Enabled)).Error; err != nil {
			return err
		}
	}
	return db.Model(&LockState{}).Where("id = 1").Updates(map[string]any{"keyless_at": now, "keyless_on": mustJSON(on), "updated_at": now}).Error
}

// noteSourceChoices records which sources a person switched on or off (the
// settings page sends every switch; only changes count as a choice).
func (s *Service) noteSourceChoices(before, patch map[string]bool) {
	ls := s.lockState()
	changed := false
	for k, v := range patch {
		if before[k] != v {
			ls.Chosen[k] = v
			changed = true
		}
	}
	if changed {
		s.DB.Model(&LockState{}).Where("id = 1").Updates(map[string]any{"chosen": mustJSON(ls.Chosen), "updated_at": time.Now().UTC()})
	}
}

// keylessView is the settings page's note on the keyless defaults.
func (s *Service) keylessView() gin.H {
	ls := s.lockState()
	return gin.H{"defaults": KeylessDefaults, "switched_on": ls.KeylessOn, "applied_at": ls.KeylessAt, "chosen": ls.Chosen,
		"note": "ClubElo, football-data.co.uk and openfootball are free and need no key, so they are on by default since 2.19.1 — unless somebody switched them off. Switch any of them off here if you prefer."}
}
