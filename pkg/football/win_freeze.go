package football

import (
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// Frozen verdicts (fb_verdicts): a fixture's verdict is frozen together with
// the model's frozen prediction (FreezeWindow before kickoff) and refreshed
// at every later pre-kickoff refresh of its inputs; at kickoff it is final.
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

// liveVerdict computes a fixture's verdict now from its current inputs.
func (s *Service) liveVerdict(f Fixture, comp Competition, mv MarketView, pred *Prediction) (*Verdict, consInputs) {
	in := s.consensusInputs(f, mv, pred, nil)
	return s.verdictFromInputs(f, comp, in, s.ownRecords()), in
}

// FreezeVerdicts freezes (or refreshes) the verdicts of fixtures kicking off
// within FreezeWindow. It returns how many were stored or refreshed.
func (s *Service) FreezeVerdicts() int {
	now := s.Now()
	n := 0
	// Rescheduled before kickoff: forget the old pick (a new one is frozen later).
	var moved []int64
	s.DB.Raw(`SELECT v.fixture_id FROM fb_verdicts v JOIN fb_fixtures f ON f.id = v.fixture_id
		WHERE v.graded_at IS NULL AND f.status IN ('NS','TBD') AND f.kickoff > v.kickoff + interval '3 hours'`).Scan(&moved)
	if len(moved) > 0 {
		s.DB.Where("fixture_id IN ?", moved).Delete(&VerdictRow{})
	}
	for _, f := range s.verdictTargets(now, now.Add(FreezeWindow)) {
		var row VerdictRow
		have := s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&row).RowsAffected > 0
		if have && row.GradedAt != nil {
			continue
		}
		comp := s.compOf(f.CompetitionID)
		mv := s.marketView(f, nil, "")
		pred := s.predictionFor(f)
		in := s.consensusInputs(f, mv, pred, nil)
		v := s.verdictFromInputs(f, comp, in, s.ownRecords())
		if v == nil {
			continue
		}
		// Refresh only when an input changed since the last freeze.
		if have && row.InputsAt != nil && !v.AsOf.After(*row.InputsAt) && row.Verdict.Consensus.Probs["1"] == v.Consensus.Probs["1"] &&
			row.Verdict.Consensus.Probs["X"] == v.Consensus.Probs["X"] {
			continue
		}
		if s.storeFrozen(f, comp, *v, in, now) == nil {
			n++
		}
	}
	return n
}

// storeFrozen writes a frozen verdict row.
func (s *Service) storeFrozen(f Fixture, comp Competition, v Verdict, in consInputs, now time.Time) error {
	if !f.Kickoff.After(now) {
		return inputErr("too late: the match has kicked off")
	}
	t := now
	asOf := v.AsOf
	v.Kind, v.FrozenAt = "frozen", &t
	row := VerdictRow{FixtureID: f.ID, CompetitionID: f.CompetitionID, Kickoff: f.Kickoff, Grp: groupOf(comp), FrozenAt: now, InputsAt: &asOf, Verdict: v,
		Pick: v.Pick, P: v.P, Tier: v.Tier.Key, Safer: v.Safer.Selection, SaferP: v.Safer.P, NoMarket: !v.Consensus.MarketUsed, UpdatedAt: now}
	if v.Goals != nil {
		row.GoalsPick, row.GoalsP = v.Goals.Pick, ptr(v.Goals.P)
	}
	if v.BTTS != nil {
		row.BTTSPick, row.BTTSP = v.BTTS.Pick, ptr(v.BTTS.P)
	}
	if len(v.TopScores) > 0 {
		row.TopScore, row.TopScoreP = v.TopScores[0].Score, v.TopScores[0].P
	}
	if in.Fav != nil {
		k := MostLikely([]P3{*in.Fav})[0]
		row.FavPick, row.FavP = selKeys[k], ptr(round4(in.Fav[k]))
	}
	return s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
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
	s.DB.Where("graded_at IS NULL AND kickoff < ?", now.Add(-90*time.Minute)).Order("kickoff").Limit(500).Find(&rows)
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

// fixtureWin returns what a fixture shows: the frozen verdict when there is
// one, else a live verdict before kickoff (nil after kickoff), and the ✓/✗
// once finished or void.
func (s *Service) fixtureWin(f Fixture, comp Competition, mv MarketView, pred *Prediction, row *VerdictRow) (*Verdict, *ResultCheck) {
	now := s.Now()
	var v *Verdict
	if row != nil {
		vv := row.Verdict
		v = &vv
	} else if f.Kickoff.After(now) && notStartedStats[f.Status] && f.Status != "PST" {
		v, _ = s.liveVerdict(f, comp, mv, pred)
	}
	return v, s.fixtureCheck(f, row)
}

// fixtureCheck is a fixture's ✓/✗ (nil until it is finished or void).
func (s *Service) fixtureCheck(f Fixture, row *VerdictRow) *ResultCheck {
	now := s.Now()
	if row != nil && row.Check != nil {
		return row.Check
	}
	if voidStatuses[f.Status] {
		var v *Verdict
		if row != nil {
			v = &row.Verdict
		}
		return VoidCheck(v, voidWhy(f.Status), now, ProviderAPIFootball)
	}
	hg, ag, ok := fixtureScore(f)
	if !ok {
		return nil
	}
	if row == nil {
		return NoPickCheck(itoa(hg) + "-" + itoa(ag))
	}
	// Finished but not graded yet by the scheduler: the same pure grading.
	return GradeVerdict(row.Verdict, hg, ag, s.teamName(f.HomeID), s.teamName(f.AwayID), now, ProviderAPIFootball)
}

// winPass is the scheduler's v2.18 work (counts go to the run's notes so
// the v2.16 frozen / graded counters keep their meaning).
func (s *Service) winPass(sum *RunSummary, step string) {
	switch step {
	case "freeze":
		if n := s.FreezeVerdicts(); n > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d verdict(s) frozen or refreshed before kickoff", n))
		}
		if n := s.FreezeJackpots(); n > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d jackpot(s) frozen at close", n))
		}
	case "grade":
		if n := s.GradeVerdicts(); n > 0 {
			sum.Notes = append(sum.Notes, fmt.Sprintf("%d verdict(s) marked ✓/✗", n))
		}
		if s.FitConsensusDue() {
			sum.Notes = append(sum.Notes, "consensus weights refitted (weekly)")
		}
	}
}

// winForFixture is the match page's verdict and check.
func (s *Service) winForFixture(f Fixture, mv MarketView, pred *Prediction) (*Verdict, *ResultCheck) {
	rows := s.verdictRows([]int64{f.ID})
	var row *VerdictRow
	if r, ok := rows[f.ID]; ok {
		row = &r
	}
	return s.fixtureWin(f, s.compOf(f.CompetitionID), mv, pred, row)
}
