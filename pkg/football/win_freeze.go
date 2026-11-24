package football

import (
	"fmt"
	"time"
)

// Frozen (locked) verdicts (fb_verdicts): since v2.19.1 a fixture's pick is
// locked early (every target fixture within the lookahead days, lock.go) and
// refreshed whenever its inputs change; at kickoff it is final.
// After full time it is graded once (marks stored on the row so the
// scorecard is a query); nothing is regraded after the fact.

// GradeFallbackAfter: when API-Football still has no result this long after
// the expected end (kickoff + 2 h), openfootball / football-data.org grade.
const GradeFallbackAfter = 6 * time.Hour

// verdictTargets are fixtures whose verdict is frozen: enabled competitions,
// games of open jackpots and watched fixtures.
func (s *Service) verdictTargets(from, to time.Time) []Fixture {
	var fx []Fixture
	s.DB.Raw(`SELECT f.* FROM fb_fixtures f LEFT JOIN fb_competitions c ON c.id = f.competition_id
		WHERE f.kickoff > ? AND f.kickoff <= ? AND f.status IN ('NS','TBD') AND (c.enabled
			OR EXISTS (SELECT 1 FROM fb_jackpot_games g JOIN fb_jackpots j ON j.id = g.jackpot_id WHERE g.fixture_id = f.id AND j.status <> 'graded')
			OR EXISTS (SELECT 1 FROM fb_watchlist w WHERE w.kind = 'fixture' AND w.ref_id = f.id))
		ORDER BY f.kickoff, f.id`, from, to).Scan(&fx)
	return fx
}

func (s *Service) compOf(id int64) Competition {
	var c Competition
	s.DB.Where("id = ?", id).Limit(1).Find(&c)
	return c
}

// predictionFor returns the frozen model prediction when there is one, else
// the stored current one (nil when neither exists).
func (s *Service) predictionFor(f Fixture) *Prediction {
	var p Prediction
	if s.DB.Where("fixture_id = ? AND kind = 'frozen'", f.ID).Limit(1).Find(&p).RowsAffected > 0 {
		return &p
	}
	if s.DB.Where("fixture_id = ? AND kind = 'current'", f.ID).Limit(1).Find(&p).RowsAffected > 0 {
		return &p
	}
	return nil
}

// FreezeVerdicts locks (or refreshes) the picks of fixtures within the
// lookahead window (v2.19.1: lock.go; until v2.19.0 only within the last
// FreezeWindow before kickoff). It returns how many picks were stored or
// refreshed.
func (s *Service) FreezeVerdicts() int {
	n, _ := s.LockVerdicts()
	return n
}

// fixtureScore is the 90-minute score of a finished fixture.
func fixtureScore(f Fixture) (int, int, bool) {
	if !IsFinal(f.Status) {
		return 0, 0, false
	}
	h, a := f.FTHome, f.FTAway
	if h == nil || a == nil {
		h, a = f.HomeGoals, f.AwayGoals
	}
	if h == nil || a == nil {
		return 0, 0, false
	}
	return *h, *a, true
}

// fallbackScore looks for a result at openfootball, then football-data.org.
func (s *Service) fallbackScore(f Fixture) (int, int, string, bool) {
	var of OFMatch
	if s.DB.Where("fixture_id = ? AND hg IS NOT NULL AND ag IS NOT NULL", f.ID).Limit(1).Find(&of).RowsAffected > 0 {
		return *of.HG, *of.AG, ProviderOpenFootball, true
	}
	var fo FDOrgMatch
	if s.DB.Where("fixture_id = ? AND status = 'FINISHED' AND hg IS NOT NULL AND ag IS NOT NULL", f.ID).Limit(1).Find(&fo).RowsAffected > 0 {
		return *fo.HG, *fo.AG, ProviderFDOrg, true
	}
	return 0, 0, "", false
}

// GradeVerdicts grades frozen verdicts of finished (or void) fixtures.
func (s *Service) GradeVerdicts() int {
	now := s.Now()
	var rows []VerdictRow
	s.DB.Where("graded_at IS NULL AND COALESCE(status, '') <> 'no_data' AND kickoff < ?", now.Add(-90*time.Minute)).Order("kickoff").Limit(500).Find(&rows)
	n := 0
	for _, row := range rows {
		var f Fixture
		if s.DB.Where("id = ?", row.FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
			continue
		}
		var rc *ResultCheck
		switch {
		case voidStatuses[f.Status]:
			rc = VoidCheck(&row.Verdict, voidWhy(f.Status), now, ProviderAPIFootball)
		case f.Status == "PST" && now.After(row.Kickoff.Add(3*time.Hour)):
			rc = VoidCheck(&row.Verdict, "the match was postponed after the pick was frozen", now, ProviderAPIFootball)
		default:
			if hg, ag, ok := fixtureScore(f); ok {
				rc = GradeVerdict(row.Verdict, hg, ag, s.teamName(f.HomeID), s.teamName(f.AwayID), now, ProviderAPIFootball)
			} else if now.After(row.Kickoff.Add(2*time.Hour + GradeFallbackAfter)) {
				if hg, ag, src, ok := s.fallbackScore(f); ok {
					rc = GradeVerdict(row.Verdict, hg, ag, s.teamName(f.HomeID), s.teamName(f.AwayID), now, src)
				}
			}
		}
		if rc == nil {
			continue
		}
		if s.saveMarks(row, rc, now) == nil {
			n++
		}
	}
	if n > 0 {
		resetOwnCache()
	}
	return n
}

func voidWhy(status string) string {
	switch status {
	case "CANC":
		return "the match was cancelled"
	case "ABD":
		return "the match was abandoned"
	case "AWD":
		return "the result was awarded off the pitch"
	case "WO":
		return "it was a walkover"
	}
	return "the match wasn't played"
}

func (s *Service) saveMarks(row VerdictRow, rc *ResultCheck, now time.Time) error {
	up := map[string]any{"graded_at": now, "status": rc.Status, "check_json": mustJSON(rc), "updated_at": now}
	if rc.Score != nil {
		up["score"] = *rc.Score
	}
	if rc.Outcome != nil {
		up["outcome"] = *rc.Outcome
		if row.FavPick != "" {
			up["fav_ok"] = row.FavPick == *rc.Outcome
		}
	}
	if rc.ResultSource != nil {
		up["result_source"] = *rc.ResultSource
	}
	set := func(col string, m *Mark) {
		if m != nil && m.Correct != nil {
			up[col] = *m.Correct
		}
	}
	set("pick_ok", rc.Pick)
	set("safer_ok", rc.Safer)
	set("goals_ok", rc.Goals)
	set("btts_ok", rc.BTTS)
	set("score_ok", rc.ExactScore)
	return s.DB.Model(&VerdictRow{}).Where("fixture_id = ? AND graded_at IS NULL", row.FixtureID).Updates(up).Error
}

// verdictRows loads frozen verdicts of fixtures.
func (s *Service) verdictRows(ids []int64) map[int64]VerdictRow {
	out := map[int64]VerdictRow{}
	if len(ids) == 0 {
		return out
	}
	var rows []VerdictRow
	s.DB.Where("fixture_id IN ?", ids).Find(&rows)
	for _, r := range rows {
		out[r.FixtureID] = r
	}
	return out
}

// fixtureWin returns what a fixture shows: the locked pick (refreshed now
// when it's upcoming — lock on view) and the ✓/✗ once finished or void
// (v2.19.1: fixtureLock in lock.go also gives the lock and no-pick reason).
func (s *Service) fixtureWin(f Fixture, comp Competition, mv MarketView, pred *Prediction, row *VerdictRow) (*Verdict, *ResultCheck) {
	lv := s.fixtureLock(f, comp, mv, pred, row)
	return lv.Verdict, lv.Check
}

// liveVerdict computes a fixture's verdict now from its current inputs.
func (s *Service) liveVerdict(f Fixture, comp Competition, mv MarketView, pred *Prediction) (*Verdict, consInputs) {
	in := s.consensusInputs(f, mv, pred, nil)
	return s.verdictFromInputs(f, comp, in, s.ownRecords()), in
}

// fixtureCheck is a fixture's ✓/✗ (nil until it is finished or void).
func (s *Service) fixtureCheck(f Fixture, row *VerdictRow) *ResultCheck {
	now := s.Now()
	if row != nil && row.Check != nil {
		return decorateCheck(row.Check, row, f)
	}
	noData := row != nil && row.Status == "no_data"
	if voidStatuses[f.Status] {
		var v *Verdict
		if row != nil && !noData {
			v = &row.Verdict
		}
		return decorateCheck(VoidCheck(v, voidWhy(f.Status), now, ProviderAPIFootball), row, f)
	}
	hg, ag, ok := fixtureScore(f)
	if !ok {
		return nil
	}
	if row == nil {
		return NoPickCheck(itoa(hg) + "-" + itoa(ag))
	}
	if noData {
		return NoDataCheck(itoa(hg)+"-"+itoa(ag), row.Reason)
	}
	// Finished but not graded yet by the scheduler: the same pure grading.
	return decorateCheck(GradeVerdict(row.Verdict, hg, ag, s.teamName(f.HomeID), s.teamName(f.AwayID), now, ProviderAPIFootball), row, f)
}

// winPass is the scheduler's v2.18 work (counts go to the run's notes so
// the v2.16 frozen / graded counters keep their meaning).
func (s *Service) winPass(sum *RunSummary, step string) {
	switch step {
	case "freeze":
		n, nd := s.LockVerdicts() // v2.19.1: locked early, refreshed until kickoff (lock.go)
		if n > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d verdict(s) frozen or refreshed before kickoff", n))
		}
		if nd > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d match(es) locked as \"no pick — not enough data\"", nd))
		}
		sum.Locked, sum.NoData = sum.Locked+n, sum.NoData+nd
		if n := s.FreezeJackpots(); n > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d jackpot(s) frozen at close", n))
		}
	case "grade":
		n := s.GradeVerdicts()
		if n > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d verdict(s) marked ✓/✗", n))
		}
		sum.VGraded += n
		if s.FitConsensusDue() {
			sum.Notes = append(sum.Notes, "consensus weights refitted (weekly)")
		}
	}
}

// winForFixture is the match page's verdict and check.
func (s *Service) winForFixture(f Fixture, mv MarketView, pred *Prediction) (*Verdict, *ResultCheck) {
	lv := s.lockForFixture(f, mv, pred)
	return lv.Verdict, lv.Check
}

// lockForFixture is the match page's verdict, check, lock and no-pick reason.
func (s *Service) lockForFixture(f Fixture, mv MarketView, pred *Prediction) LockView {
	rows := s.verdictRows([]int64{f.ID})
	var row *VerdictRow
	if r, ok := rows[f.ID]; ok {
		row = &r
	}
	return s.fixtureLock(f, s.compOf(f.CompetitionID), mv, pred, row)
}
