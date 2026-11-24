package football

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// GET /football/results — the match centre's Results view: matches that
// kicked off on a chosen day (or in the last N days) with DASYIN's locked
// pick, ✓ Correct / ✗ Wrong / no pick (and why), and a one-line tally such
// as "Yesterday: 7 ✓ · 5 ✗ · 3 no pick".

// ResultTally counts a period's results.
type ResultTally struct {
	Correct       int      `json:"correct"`
	Wrong         int      `json:"wrong"`
	NoPick        int      `json:"no_pick"`         // nothing locked before kickoff
	NotEnoughData int      `json:"not_enough_data"` // locked as "no pick — not enough data"
	Void          int      `json:"void"`
	Pending       int      `json:"pending"` // kicked off, no result yet
	Total         int      `json:"total"`
	HitRate       *float64 `json:"hit_rate"` // correct / (correct + wrong)
	Line          string   `json:"line"`     // "Yesterday: 7 ✓ · 5 ✗ · 3 no pick"
}

func (t *ResultTally) add(status string) {
	t.Total++
	switch status {
	case "correct":
		t.Correct++
	case "wrong":
		t.Wrong++
	case "no_pick":
		t.NoPick++
	case "not_enough_data":
		t.NotEnoughData++
	case "void":
		t.Void++
	default:
		t.Pending++
	}
}

func (t *ResultTally) finish(label string) {
	t.HitRate = rate(t.Correct, t.Correct+t.Wrong)
	parts := []string{fmt.Sprintf("%d ✓", t.Correct), fmt.Sprintf("%d ✗", t.Wrong), fmt.Sprintf("%d no pick", t.NoPick+t.NotEnoughData)}
	if t.Void > 0 {
		parts = append(parts, fmt.Sprintf("%d void", t.Void))
	}
	if t.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting for the result", t.Pending))
	}
	t.Line = label + ": " + strings.Join(parts, " · ")
	if t.Total == 0 {
		t.Line = label + ": no matches"
	}
}

// ResultPick is the locked pick on a results row.
type ResultPick struct {
	Pick      string  `json:"pick"`
	PickLabel string  `json:"pick_label"`
	P         float64 `json:"p"`
	Tier      string  `json:"tier"`
	TierLabel string  `json:"tier_label"`
	Note      string  `json:"note,omitempty"`
}

// ResultRow is one match of the Results view.
type ResultRow struct {
	FixtureID   int64        `json:"fixture_id"`
	Kickoff     time.Time    `json:"kickoff"`
	Status      string       `json:"status"`
	Competition CompRef      `json:"competition"`
	Home        TeamRef      `json:"home"`
	Away        TeamRef      `json:"away"`
	Score       ScoreOut     `json:"score"`
	Pick        *ResultPick  `json:"pick"`
	Lock        *LockOut     `json:"lock"`
	ResultCheck *ResultCheck `json:"result_check"`
	Mark        string       `json:"mark"` // correct | wrong | no_pick | not_enough_data | void | pending
}

// ResultDay is one day of the view.
type ResultDay struct {
	Date  string      `json:"date"`
	Label string      `json:"label"`
	Tally ResultTally `json:"tally"`
	Rows  []ResultRow `json:"rows"`
}

// Results is GET /football/results.
type Results struct {
	Period struct {
		From     time.Time `json:"from"`
		To       time.Time `json:"to"`
		Label    string    `json:"label"`
		Timezone string    `json:"timezone"`
		Date     string    `json:"date,omitempty"`
		Days     int       `json:"days"`
	} `json:"period"`
	Tally        ResultTally `json:"tally"`
	Days         []ResultDay `json:"days"` // newest first
	Scorecard    string      `json:"scorecard"`
	Note         string      `json:"note"`
	AsOf         time.Time   `json:"as_of"`
	LastPassAt   *time.Time  `json:"last_pass_at"`
	LastResultAt *time.Time  `json:"last_result_at"` // the newest result fetched (stale data shows here)
}

func dayLabel(d, today time.Time) string {
	switch int(today.Sub(d).Hours()/24 + 0.5) {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	return d.Format("Mon 2 Jan")
}

// ResultsFor builds the Results view.
func (s *Service) ResultsFor(date string, days int, comp int64, loc *time.Location) (*Results, error) {
	if loc == nil {
		loc = time.UTC
	}
	now := s.Now()
	lt := now.In(loc)
	today := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	out := &Results{Days: []ResultDay{}, AsOf: now,
		Note: "Each pick is locked before kick-off (refreshed until then) and graded once after full time — never changed afterwards."}
	var from, to time.Time
	if strings.TrimSpace(date) != "" {
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), loc)
		if err != nil {
			return nil, inputErr("Use a date like 2026-11-23.")
		}
		from, to = d, d.AddDate(0, 0, 1)
		out.Period.Date, out.Period.Days, out.Period.Label = d.Format("2006-01-02"), 1, dayLabel(d, today)
		out.Scorecard = "/football/scorecard?period=custom&from=" + d.Format("2006-01-02") + "&to=" + d.Format("2006-01-02")
	} else {
		if days <= 0 {
			days = 7
		}
		if days > 14 {
			return nil, inputErr("days: 1 to 14.")
		}
		from, to = today.AddDate(0, 0, -(days-1)), today.AddDate(0, 0, 1)
		out.Period.Days = days
		out.Period.Label = "Last " + strconv.Itoa(days) + " days"
		if days == 1 {
			out.Period.Label = "Today"
		}
		out.Scorecard = "/football/scorecard?period=custom&from=" + from.Format("2006-01-02") + "&to=" + today.Format("2006-01-02")
		if days == 7 {
			out.Scorecard = "/football/scorecard?period=7d"
		}
	}
	if to.After(now) {
		to = now
	}
	out.Period.From, out.Period.To, out.Period.Timezone = from.UTC(), to.UTC(), loc.String()
	q := s.DB.Model(&Fixture{}).Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id").
		Where("fb_fixtures.kickoff >= ? AND fb_fixtures.kickoff < ? AND fb_fixtures.id > 0", from.UTC(), to.UTC())
	if comp != 0 {
		q = q.Where("fb_fixtures.competition_id = ?", comp)
	} else {
		q = q.Where("(c.enabled OR EXISTS (SELECT 1 FROM fb_verdicts v WHERE v.fixture_id = fb_fixtures.id))")
	}
	var fx []Fixture
	if err := q.Select("fb_fixtures.*").Order("fb_fixtures.kickoff DESC, c.priority, fb_fixtures.id").Limit(600).Find(&fx).Error; err != nil {
		return nil, err
	}
	ids := make([]int64, len(fx))
	for i, f := range fx {
		ids[i] = f.ID
	}
	rows := s.verdictRows(ids)
	comps := map[int64]Competition{}
	byDay := map[string]*ResultDay{}
	order := []string{}
	for _, f := range fx {
		if f.Status == "PST" || (notStartedStats[f.Status] && f.Kickoff.After(now)) {
			continue
		}
		c, okc := comps[f.CompetitionID]
		if !okc {
			c = s.compOf(f.CompetitionID)
			comps[f.CompetitionID] = c
		}
		var row *VerdictRow
		if r, ok := rows[f.ID]; ok {
			row = &r
		}
		rr := ResultRow{FixtureID: f.ID, Kickoff: f.Kickoff, Status: f.Status, Competition: CompRef{ID: c.ID, Name: c.Name, Country: c.Country, Logo: c.Logo, Type: c.Type,
			Season: f.Season, Round: f.Round}, Home: s.teamRef(f.HomeID), Away: s.teamRef(f.AwayID), Score: scoreOf(f)}
		if row != nil {
			rr.Lock = lockOutOf(*row, f, now)
			if row.Status != "no_data" && row.Pick != "" {
				rr.Pick = &ResultPick{Pick: row.Pick, PickLabel: row.Verdict.PickLabel, P: row.P, Tier: row.Tier, TierLabel: History.Tier(row.Tier).Label, Note: row.Verdict.Tier.Note}
			}
		} else {
			rr.Lock = &LockOut{Status: "not_locked", Label: "No pick — not locked before kick-off", Final: true}
		}
		rr.ResultCheck = s.fixtureCheck(f, row)
		rr.Mark = "pending"
		if rr.ResultCheck != nil {
			rr.Mark = rr.ResultCheck.Status
		}
		lk := f.Kickoff.In(loc)
		key := lk.Format("2006-01-02")
		d := byDay[key]
		if d == nil {
			dd := time.Date(lk.Year(), lk.Month(), lk.Day(), 0, 0, 0, 0, loc)
			d = &ResultDay{Date: key, Label: dayLabel(dd, today), Rows: []ResultRow{}}
			byDay[key] = d
			order = append(order, key)
		}
		d.Rows = append(d.Rows, rr)
		d.Tally.add(rr.Mark)
		out.Tally.add(rr.Mark)
	}
	for _, k := range order {
		d := byDay[k]
		d.Tally.finish(d.Label)
		out.Days = append(out.Days, *d)
	}
	out.Tally.finish(out.Period.Label)
	ls := s.lockState()
	out.LastPassAt = ls.LastPassAt
	var last *time.Time
	s.DB.Model(&Fixture{}).Where("finished_at IS NOT NULL AND id > 0").Select("MAX(fetched_at)").Scan(&last)
	out.LastResultAt = last
	return out, nil
}

func (h *Handler) results(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	days, _ := strconv.Atoi(strings.TrimSpace(c.Query("days")))
	r, err := h.S.ResultsFor(c.Query("date"), days, qInt(c, "competition"), h.S.zoneOf(org))
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, r)
}
