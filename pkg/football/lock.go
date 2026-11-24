package football

import (
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Locked picks (v2.19.1). Until v2.19.0 a verdict was frozen only in the 30
// minutes before kickoff and only when a scheduler pass ran then; on Cloud
// Run without minimum instances that rarely happened, so finished matches
// said "no pick". Now:
//   - every target fixture within the lookahead days gets its pick locked the
//     first time it can be computed (or "no pick — not enough data");
//   - the lock is refreshed whenever its inputs change (scheduler passes, and
//     any API path that shows the match: lock on view, at most once per
//     fixture every LockViewEvery);
//   - at kickoff the last refresh is final: never changed, graded once.
// The model's frozen prediction (fb_predictions kind 'frozen') follows the
// same rule (LockPredictions).

// LockViewEvery throttles lock on view per fixture.
const LockViewEvery = 10 * time.Minute

// lockWindow is how far ahead picks are locked (Settings.LookaheadDays).
func (s *Service) lockWindow() time.Duration {
	st, _ := s.Settings()
	d := st.LookaheadDays
	if d < 1 {
		d = 3
	}
	return time.Duration(d) * 24 * time.Hour
}

func upcomingFixture(f Fixture, now time.Time) bool {
	return f.Kickoff.After(now) && (f.Status == "NS" || f.Status == "TBD" || f.Status == "")
}

// lockOutcome is what one lock attempt did.
type lockOutcome int

const (
	lockSkipped   lockOutcome = iota // after kickoff, graded, or nothing to do
	lockStored                       // first pick locked
	lockRefreshed                    // pick refreshed (inputs changed)
	lockSame                         // checked, unchanged
	lockNoData                       // locked / refreshed as "not enough data"
	lockKept                         // had a pick, now no data: the pick is kept
)

// LockVerdicts locks (or refreshes) the picks of every target fixture within
// the lookahead window. It returns the picks stored or refreshed and the
// matches locked as "not enough data".
func (s *Service) LockVerdicts() (picks, noData int) {
	now := s.Now()
	// Rescheduled before kickoff: forget the old lock (a new one is made).
	var moved []int64
	s.DB.Raw(`SELECT v.fixture_id FROM fb_verdicts v JOIN fb_fixtures f ON f.id = v.fixture_id
		WHERE v.graded_at IS NULL AND f.status IN ('NS','TBD') AND f.kickoff > v.kickoff + interval '3 hours'`).Scan(&moved)
	if len(moved) > 0 {
		s.DB.Where("fixture_id IN ?", moved).Delete(&VerdictRow{})
	}
	targets := s.verdictTargets(now, now.Add(s.lockWindow()))
	if len(targets) == 0 {
		return 0, 0
	}
	ids := make([]int64, len(targets))
	for i, f := range targets {
		ids[i] = f.ID
	}
	rows := s.verdictRows(ids)
	own := s.ownRecords()
	comps := map[int64]Competition{}
	for _, f := range targets {
		c, ok := comps[f.CompetitionID]
		if !ok {
			c = s.compOf(f.CompetitionID)
			comps[f.CompetitionID] = c
		}
		var row *VerdictRow
		if r, ok := rows[f.ID]; ok {
			row = &r
		}
		switch s.lockOne(f, c, s.marketView(f, nil, ""), row, own) {
		case lockStored, lockRefreshed:
			picks++
		case lockNoData:
			noData++
		}
	}
	return picks, noData
}

// lockOne computes a fixture's verdict now and stores / refreshes its lock.
func (s *Service) lockOne(f Fixture, comp Competition, mv MarketView, row *VerdictRow, own map[string]*OwnRecord) lockOutcome {
	now := s.Now()
	if !upcomingFixture(f, now) || (row != nil && row.GradedAt != nil) {
		return lockSkipped
	}
	pred := s.predictionFor(f)
	in := s.consensusInputs(f, mv, pred, nil)
	v, g := s.verdictGated(f, comp, in, own)
	if v == nil {
		return s.storeNoData(f, comp, g, row, now)
	}
	if row != nil && row.Status != "no_data" && row.Pick != "" && row.InputsAt != nil && !v.AsOf.After(*row.InputsAt) &&
		samePick(row.Verdict, *v) {
		s.touchLock(f.ID, now)
		return lockSame
	}
	if s.storeLock(f, comp, *v, g, in, row, now) != nil {
		return lockSkipped
	}
	if row != nil && row.Status != "no_data" && row.Pick != "" {
		return lockRefreshed
	}
	return lockStored
}

// samePick: the stored verdict says the same (pick and chances).
func samePick(a, b Verdict) bool {
	if a.Pick != b.Pick {
		return false
	}
	for _, k := range []string{"1", "X", "2"} {
		if math.Abs(a.Consensus.Probs[k]-b.Consensus.Probs[k]) > 0.00005 {
			return false
		}
	}
	return true
}

// touchLock records that a lock was checked (lock on view's throttle).
func (s *Service) touchLock(fixture int64, now time.Time) {
	s.DB.Model(&VerdictRow{}).Where("fixture_id = ? AND graded_at IS NULL AND kickoff > ?", fixture, now).Update("checked_at", now)
}

// storeLock writes (or refreshes) a locked pick — only ever before kickoff.
func (s *Service) storeLock(f Fixture, comp Competition, v Verdict, g GateOut, in consInputs, row *VerdictRow, now time.Time) error {
	if !f.Kickoff.After(now) {
		return inputErr("too late: the match has kicked off")
	}
	t := now
	asOf := v.AsOf
	v.Kind, v.FrozenAt, v.LockedAt = "frozen", &t, &t
	v.LockedLabel = LockedLabel(t, f.Kickoff)
	nr := VerdictRow{FixtureID: f.ID, CompetitionID: f.CompetitionID, Kickoff: f.Kickoff, Grp: groupOf(comp), FrozenAt: now, InputsAt: &asOf, Verdict: v,
		Pick: v.Pick, P: v.P, Tier: v.Tier.Key, Safer: v.Safer.Selection, SaferP: v.Safer.P, NoMarket: !v.Consensus.MarketUsed, UpdatedAt: now,
		FirstLockedAt: &t, CheckedAt: &t, Gate: &g}
	if v.Goals != nil {
		nr.GoalsPick, nr.GoalsP = v.Goals.Pick, ptr(v.Goals.P)
	}
	if v.BTTS != nil {
		nr.BTTSPick, nr.BTTSP = v.BTTS.Pick, ptr(v.BTTS.P)
	}
	if len(v.TopScores) > 0 {
		nr.TopScore, nr.TopScoreP = v.TopScores[0].Score, v.TopScores[0].P
	}
	if in.Fav != nil {
		k := MostLikely([]P3{*in.Fav})[0]
		nr.FavPick, nr.FavP = selKeys[k], ptr(round4(in.Fav[k]))
	}
	if row == nil {
		return s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&nr).Error
	}
	up := map[string]any{"competition_id": nr.CompetitionID, "kickoff": nr.Kickoff, "grp": nr.Grp, "frozen_at": now, "inputs_at": asOf, "verdict": mustJSON(v),
		"pick": nr.Pick, "p": nr.P, "tier": nr.Tier, "safer": nr.Safer, "safer_p": nr.SaferP, "goals_pick": nr.GoalsPick, "goals_p": nr.GoalsP,
		"btts_pick": nr.BTTSPick, "btts_p": nr.BTTSP, "top_score": nr.TopScore, "top_score_p": nr.TopScoreP, "fav_pick": nr.FavPick, "fav_p": nr.FavP,
		"no_market": nr.NoMarket, "status": "", "reason": "", "gate_json": mustJSON(g), "checked_at": now, "updated_at": now}
	if row.FirstLockedAt == nil {
		up["first_locked_at"] = row.FrozenAt
	}
	if row.Status != "no_data" && row.Pick != "" {
		up["refreshes"] = gorm.Expr("refreshes + 1")
	}
	return s.DB.Model(&VerdictRow{}).Where("fixture_id = ? AND graded_at IS NULL AND kickoff > ?", f.ID, now).Updates(up).Error
}

// storeNoData locks "no pick — not enough data" (or keeps an earlier pick:
// a pick made from real data is never replaced by "no pick").
func (s *Service) storeNoData(f Fixture, comp Competition, g GateOut, row *VerdictRow, now time.Time) lockOutcome {
	if !f.Kickoff.After(now) {
		return lockSkipped
	}
	if row != nil && row.Status != "no_data" && row.Pick != "" {
		s.touchLock(f.ID, now)
		return lockKept
	}
	reason := clip(firstNonEmpty(g.Reason, "Not enough data"), 300)
	t := now
	if row == nil {
		nr := VerdictRow{FixtureID: f.ID, CompetitionID: f.CompetitionID, Kickoff: f.Kickoff, Grp: groupOf(comp), FrozenAt: now, Status: "no_data", Reason: reason,
			Gate: &g, FirstLockedAt: &t, CheckedAt: &t, NoMarket: true, UpdatedAt: now}
		if s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&nr).Error != nil {
			return lockSkipped
		}
		return lockNoData
	}
	if row.Reason == reason && row.Kickoff.Equal(f.Kickoff) {
		s.touchLock(f.ID, now)
		return lockSame
	}
	s.DB.Model(&VerdictRow{}).Where("fixture_id = ? AND graded_at IS NULL AND kickoff > ?", f.ID, now).
		Updates(map[string]any{"reason": reason, "gate_json": mustJSON(g), "kickoff": f.Kickoff, "frozen_at": now, "checked_at": now, "updated_at": now})
	return lockNoData
}

// lockView is lock on view: an API path showing an upcoming fixture stores
// or refreshes its lock (at most once per fixture every LockViewEvery). It
// returns the current row.
func (s *Service) lockView(f Fixture, comp Competition, mv MarketView, row *VerdictRow) *VerdictRow {
	now := s.Now()
	if !upcomingFixture(f, now) {
		return row
	}
	if row != nil && row.CheckedAt != nil {
		if d := now.Sub(*row.CheckedAt); d >= 0 && d < LockViewEvery {
			return row
		}
	}
	if s.lockOne(f, comp, mv, row, s.ownRecords()) == lockSkipped {
		return row
	}
	var r VerdictRow
	if s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&r).RowsAffected == 0 {
		return row
	}
	return &r
}

// LockFixtures is lock on view for a list of fixture ids (jackpot games).
func (s *Service) LockFixtures(ids []int64) int {
	if len(ids) == 0 {
		return 0
	}
	now := s.Now()
	var fx []Fixture
	s.DB.Where("id IN ? AND kickoff > ? AND status IN ('NS','TBD')", ids, now).Find(&fx)
	rows := s.verdictRows(ids)
	n := 0
	for _, f := range fx {
		var row *VerdictRow
		if r, ok := rows[f.ID]; ok {
			row = &r
		}
		if nr := s.lockView(f, s.compOf(f.CompetitionID), s.marketView(f, nil, ""), row); nr != nil {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- what rows show

// LockView is a fixture's verdict, ✓ / ✗, lock and no-pick reason.
type LockView struct {
	Verdict *Verdict
	Check   *ResultCheck
	Lock    *LockOut
	NoPick  *NoPickOut
}

// fixtureLock is what a fixture shows (lists, the match page): the locked
// pick (stored or refreshed now when it's upcoming — lock on view), the
// "not enough data" reason, and the ✓ / ✗ once finished.
func (s *Service) fixtureLock(f Fixture, comp Competition, mv MarketView, pred *Prediction, row *VerdictRow) LockView {
	now := s.Now()
	upcoming := upcomingFixture(f, now)
	if upcoming {
		row = s.lockView(f, comp, mv, row)
	}
	var lv LockView
	switch {
	case row != nil && row.Status == "no_data":
		lv.Lock = lockOutOf(*row, f, now)
		if upcoming || !IsFinal(f.Status) {
			lv.NoPick = noPickOf(row.Reason, row.Gate)
		}
	case row != nil:
		v := row.Verdict
		decorateLocked(&v, *row, f)
		lv.Verdict = &v
		lv.Lock = lockOutOf(*row, f, now)
	default:
		if upcoming {
			// The lock couldn't be stored (e.g. a race): show the live verdict.
			in := s.consensusInputs(f, mv, pred, nil)
			v, g := s.verdictGated(f, comp, in, s.ownRecords())
			lv.Verdict = v
			if v == nil {
				lv.NoPick = noPickOf(g.Reason, &g)
			}
			lv.Lock = &LockOut{Status: "not_locked", Label: "Not locked yet"}
		} else if notStartedStats[f.Status] && f.Status != "PST" || IsLive(f.Status) || IsFinal(f.Status) {
			lv.Lock = &LockOut{Status: "not_locked", Label: "No pick — not locked before kick-off", Final: true}
		}
	}
	lv.Check = s.fixtureCheck(f, row)
	return lv
}

func noPickOf(reason string, g *GateOut) *NoPickOut {
	np := &NoPickOut{Status: "not_enough_data", Label: "No pick — not enough data", Reason: firstNonEmpty(reason, "Not enough data"), Checks: []GateCheck{}}
	if g != nil && g.Checks != nil {
		np.Checks = g.Checks
	}
	return np
}

// lockOutOf describes a stored lock.
func lockOutOf(r VerdictRow, f Fixture, now time.Time) *LockOut {
	lo := &LockOut{Status: "locked", CheckedAt: r.CheckedAt, Refreshes: r.Refreshes, Final: !f.Kickoff.After(now) || !upcomingFixture(f, now)}
	at := r.FrozenAt
	lo.LockedAt = &at
	lo.FirstLockedAt = r.FirstLockedAt
	if lo.FirstLockedAt == nil {
		lo.FirstLockedAt = &at
	}
	ko := f.Kickoff
	if ko.IsZero() {
		ko = r.Kickoff
	}
	m := int((ko.Sub(at) + 30*time.Second) / time.Minute)
	if m < 0 {
		m = 0
	}
	lo.BeforeKickoffMin = &m
	if r.Status == "no_data" {
		lo.Status, lo.Reason = "not_enough_data", r.Reason
		lo.Label = "No pick — not enough data"
		return lo
	}
	lo.Label = LockedLabel(at, ko)
	return lo
}

// decorateLocked adds the lock fields to a stored verdict.
func decorateLocked(v *Verdict, r VerdictRow, f Fixture) {
	at := r.FrozenAt
	v.LockedAt = &at
	ko := f.Kickoff
	if ko.IsZero() {
		ko = r.Kickoff
	}
	v.LockedLabel = LockedLabel(at, ko)
	if v.FrozenAt == nil {
		v.FrozenAt = &at
	}
	if v.Data == nil && r.Gate != nil {
		g := *r.Gate
		v.Data = &g
	}
}

// LockedLabel is "Locked 2 h 10 min before kick-off".
func LockedLabel(lockedAt, kickoff time.Time) string {
	return "Locked " + durationWords(kickoff.Sub(lockedAt)) + " before kick-off"
}

// durationWords: "2 h 10 min", "45 min", "1 day 3 h", "less than a minute"
// (to the nearest minute).
func durationWords(d time.Duration) string {
	m := int((d + 30*time.Second) / time.Minute)
	if m < 1 {
		return "less than a minute"
	}
	days, h, mins := m/1440, (m%1440)/60, m%60
	switch {
	case days > 0:
		w := fmt.Sprintf("%d day", days)
		if days > 1 {
			w += "s"
		}
		if h > 0 {
			w += fmt.Sprintf(" %d h", h)
		}
		return w
	case h > 0 && mins > 0:
		return fmt.Sprintf("%d h %d min", h, mins)
	case h > 0:
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d min", mins)
}

// NoDataCheck is a finished match that was locked as "not enough data".
func NoDataCheck(score, reason string) *ResultCheck {
	rc := NoPickCheck(score)
	rc.Status = "not_enough_data"
	rc.Reason = firstNonEmpty(reason, "Not enough data")
	rc.Summary = "No pick — not enough data (" + strings.TrimSuffix(lowerFirst(rc.Reason), ".") + ")."
	return rc
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// decorateCheck adds when the graded pick was locked.
func decorateCheck(rc *ResultCheck, r *VerdictRow, f Fixture) *ResultCheck {
	if rc == nil || r == nil || r.Status == "no_data" {
		return rc
	}
	c := *rc
	at := r.FrozenAt
	c.LockedAt = &at
	ko := f.Kickoff
	if ko.IsZero() {
		ko = r.Kickoff
	}
	c.LockedLabel = LockedLabel(at, ko)
	return &c
}

// ---------------------------------------------------------------- the model's frozen prediction

// LockPredictions stores the model's frozen prediction early (every fixture
// of an enabled competition within the lookahead window) and refreshes it
// whenever the model's numbers change; at kickoff it is final (the closing
// market is added and it is graded after full time). Fixtures the model
// knows nothing about (both teams unrated) get none: an unfitted default
// isn't a prediction.
func (s *Service) LockPredictions() int {
	now := s.Now()
	var fx []Fixture
	s.DB.Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id AND c.enabled").
		Where("fb_fixtures.kickoff > ? AND fb_fixtures.kickoff <= ? AND fb_fixtures.status IN ?", now, now.Add(s.lockWindow()), []string{"NS", "TBD"}).Find(&fx)
	n := 0
	for _, f := range fx {
		var cur, fr Prediction
		haveCur := s.DB.Where("fixture_id = ? AND kind = 'current'", f.ID).Limit(1).Find(&cur).RowsAffected > 0
		haveFr := s.DB.Where("fixture_id = ? AND kind = 'frozen'", f.ID).Limit(1).Find(&fr).RowsAffected > 0
		if !haveCur {
			p, err := s.compute(f)
			if err != nil {
				continue
			}
			cur = p
		}
		md := s.modelDataFor(f, &cur)
		if !md.HasData {
			continue
		}
		if haveFr && math.Abs(fr.PHome-cur.PHome) < 1e-9 && math.Abs(fr.PDraw-cur.PDraw) < 1e-9 && math.Abs(fr.PAway-cur.PAway) < 1e-9 {
			continue
		}
		p := cur
		t := now
		p.Kind, p.FrozenAt = "frozen", &t
		p.MarketHome, p.MarketDraw, p.MarketAway, p.MarketSource = nil, nil, nil, ""
		if mv := s.marketView(f, nil, ""); mv.Fair1X2 != nil {
			p.MarketHome, p.MarketDraw, p.MarketAway = ptr(mv.Fair1X2[SelHome]), ptr(mv.Fair1X2[SelDraw]), ptr(mv.Fair1X2[SelAway])
			p.MarketSource = "at freeze: " + mv.FairSource
		}
		if !f.Kickoff.After(s.Now()) {
			continue
		}
		var err error
		if haveFr {
			err = s.DB.Model(&Prediction{}).Where("fixture_id = ? AND kind = 'frozen' AND graded_at IS NULL AND kickoff > ?", f.ID, now).
				Updates(map[string]any{"p_home": p.PHome, "p_draw": p.PDraw, "p_away": p.PAway, "detail": mustJSON(p.Detail), "computed_at": p.ComputedAt,
					"frozen_at": now, "market_home": p.MarketHome, "market_draw": p.MarketDraw, "market_away": p.MarketAway, "market_source": p.MarketSource,
					"model_version": p.ModelVersion, "kickoff": f.Kickoff}).Error
		} else {
			err = s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error
		}
		if err == nil {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- bets

// lockedPickFor is DASYIN's locked pick of a bet's match and whether it was
// right (My bets).
func (s *Service) lockedPickFor(f Fixture) *LockedPick {
	if f.ID == 0 {
		return nil
	}
	now := s.Now()
	var r VerdictRow
	have := s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&r).RowsAffected > 0
	lp := &LockedPick{Final: !upcomingFixture(f, now)}
	if !have {
		if upcomingFixture(f, now) {
			lp.Status, lp.Label = "not_locked", "Not locked yet"
		} else {
			lp.Status, lp.Label = "not_locked", "No pick — not locked before kick-off"
		}
		return lp
	}
	at := r.FrozenAt
	lp.LockedAt = &at
	if r.Status == "no_data" {
		lp.Status, lp.Label, lp.Reason = "not_enough_data", "No pick — not enough data", r.Reason
		return lp
	}
	lp.Pick, lp.PickLabel, lp.P, lp.Tier, lp.TierLabel = r.Pick, r.Verdict.PickLabel, r.P, r.Tier, History.Tier(r.Tier).Label
	lp.LockedLabel = LockedLabel(at, f.Kickoff)
	rc := s.fixtureCheck(f, &r)
	switch {
	case rc == nil:
		lp.Status, lp.Label = "pending", "Not graded yet"
	default:
		lp.Status, lp.Label, lp.Summary = rc.Status, rc.Label, rc.Summary
		if rc.Score != nil {
			lp.Score = *rc.Score
		}
	}
	return lp
}
