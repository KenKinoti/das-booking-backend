package football

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Cross-source stats check ("establish accurate stats"): for a match,
// compare league position, points, played, goals for/against, last-5
// results, head-to-head, kickoff and team identity across API-Football,
// football-data.org, football-data.co.uk, ClubElo, OddsPapi and news. Each
// stat gets agree / minor / conflict / single with the sources behind every
// value. Conflicts never block the page.

// SCValue is one source's value.
type SCValue struct {
	Source    string     `json:"source"`
	Value     any        `json:"value"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	URL       string     `json:"url,omitempty"`
}

// SCItem is one checked stat.
type SCItem struct {
	Key             string    `json:"key"`
	Section         string    `json:"section"`
	Label           string    `json:"label"`
	Side            string    `json:"side"`
	Value           any       `json:"value"`
	Status          string    `json:"status"`
	SourcesAgreeing []string  `json:"sources_agreeing"`
	Values          []SCValue `json:"values"`
	Disagreeing     []SCValue `json:"disagreeing"`
	Note            string    `json:"note"`
}

// SourceCheck is the match page's source check.
type SourceCheck struct {
	Summary struct {
		Status    string    `json:"status"`
		Agree     int       `json:"agree"`
		Minor     int       `json:"minor"`
		Conflict  int       `json:"conflict"`
		Single    int       `json:"single"`
		Sources   []string  `json:"sources"`
		CheckedAt time.Time `json:"checked_at"`
		Note      string    `json:"note"`
	} `json:"summary"`
	Sections map[string]string `json:"sections"`
	Items    []SCItem          `json:"items"`
}

// scKind is how values are compared.
type scKind int

const (
	scNumber scKind = iota // ≤ 1 apart = minor
	scForm                 // differ only in the newest result = minor
	scTime                 // ≤ 15 min = minor (dates only: same day = agree)
	scText                 // exact
	scName                 // similarity
)

// EvalItem decides an item's status from its values (pure; the first value
// is the reference shown).
func EvalItem(it *SCItem, kind scKind) {
	it.SourcesAgreeing, it.Disagreeing = []string{}, []SCValue{}
	if len(it.Values) == 0 {
		it.Status = "single"
		return
	}
	ref := it.Values[0]
	it.Value = ref.Value
	if len(it.Values) == 1 {
		it.Status, it.SourcesAgreeing = "single", []string{ref.Source}
		if it.Note == "" {
			it.Note = "only one source has this"
		}
		return
	}
	worst := 0 // 0 agree 1 minor 2 conflict
	it.SourcesAgreeing = append(it.SourcesAgreeing, ref.Source)
	notes := []string{}
	for _, v := range it.Values[1:] {
		lvl, note := compareSC(ref.Value, v.Value, kind)
		if lvl == 0 {
			it.SourcesAgreeing = append(it.SourcesAgreeing, v.Source)
			continue
		}
		it.Disagreeing = append(it.Disagreeing, v)
		if note != "" {
			notes = append(notes, note)
		}
		if lvl > worst {
			worst = lvl
		}
	}
	it.Status = []string{"agree", "minor", "conflict"}[worst]
	if len(notes) > 0 && it.Note == "" {
		it.Note = strings.Join(uniqStrings(notes), "; ")
	}
}

func uniqStrings(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func toF(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func compareSC(a, b any, kind scKind) (int, string) {
	switch kind {
	case scNumber:
		x, ok1 := toF(a)
		y, ok2 := toF(b)
		if !ok1 || !ok2 {
			return 2, "values not comparable"
		}
		d := math.Abs(x - y)
		switch {
		case d < 1e-9:
			return 0, ""
		case d <= 1:
			return 1, "one apart: sources updated at different times"
		}
		return 2, fmt.Sprintf("%.0f apart", d)
	case scForm:
		x, y := fmt.Sprint(a), fmt.Sprint(b)
		if x == y {
			return 0, ""
		}
		// One source one match behind: "WDLWW" vs "DLWWL"-style shift.
		if len(x) == len(y) && len(x) > 1 && (x[1:] == y[:len(y)-1] || y[1:] == x[:len(x)-1]) {
			return 1, "one source is one match behind"
		}
		if len(x) > 1 && len(y) > 1 && (x[:len(x)-1] == y[:len(y)-1]) {
			return 1, "differs only in the newest result"
		}
		return 2, "different results"
	case scTime:
		ta, ok1 := a.(time.Time)
		tb, ok2 := b.(time.Time)
		if !ok1 || !ok2 {
			sa, sb := fmt.Sprint(a), fmt.Sprint(b)
			if ok1 {
				sa = ta.UTC().Format(time.RFC3339)
			}
			if ok2 {
				sb = tb.UTC().Format(time.RFC3339)
			}
			if sa == sb {
				return 0, ""
			}
			// A date-only source (ClubElo) agrees when the day matches.
			if len(sb) == 10 && strings.HasPrefix(sa, sb) || len(sa) == 10 && strings.HasPrefix(sb, sa) {
				return 0, ""
			}
			return 2, "different dates"
		}
		d := math.Abs(ta.Sub(tb).Minutes())
		switch {
		case d < 1:
			return 0, ""
		case d <= 15:
			return 1, fmt.Sprintf("%.0f minutes apart", d)
		}
		return 2, fmt.Sprintf("kickoff differs by %s", time.Duration(d*float64(time.Minute)).Round(time.Minute))
	case scName:
		x, y := fmt.Sprint(a), fmt.Sprint(b)
		v := TeamSim(x, y)
		switch {
		case v >= 0.6:
			return 0, ""
		case v >= 0.4:
			return 1, "names spelled quite differently (check the alias)"
		}
		return 2, "names don't look like the same team — check the alias"
	}
	if fmt.Sprint(a) == fmt.Sprint(b) {
		return 0, ""
	}
	return 2, "different"
}

// afLast5: API-Football's last five results of a team (provider fixtures only).
func (s *Service) afLast5(team int64, before time.Time) (string, *time.Time) {
	var rows []resultRow
	s.DB.Raw(`SELECT id, kickoff, competition_id, season, home_id, away_id, ft_home, ft_away FROM fb_fixtures
		WHERE (home_id = ? OR away_id = ?) AND id > 0 AND status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND kickoff < ? ORDER BY kickoff DESC LIMIT 5`, team, team, before).Scan(&rows)
	return formString(rows, team), nil
}

func formString(rows []resultRow, team int64) string {
	b := []byte{}
	for i := len(rows) - 1; i >= 0; i-- {
		_, gf, ga, _, _, _ := side(rows[i], team)
		switch {
		case gf > ga:
			b = append(b, 'W')
		case gf == ga:
			b = append(b, 'D')
		default:
			b = append(b, 'L')
		}
	}
	return string(b)
}

func (s *Service) fdLast5(team int64, before time.Time) string {
	var hs []HistMatch
	s.DB.Where("(home_id = ? OR away_id = ?) AND date < ?", team, team, before).Order("date DESC").Limit(5).Find(&hs)
	rows := []resultRow{}
	for _, h := range hs {
		rows = append(rows, resultRow{HomeID: h.HomeID, AwayID: h.AwayID, FTHome: h.HG, FTAway: h.AG, Kickoff: h.Date})
	}
	if len(rows) < 5 {
		return ""
	}
	return formString(rows, team)
}

func (s *Service) fdoLast5(team int64, before time.Time) string {
	var ms []FDOrgMatch
	s.DB.Where("(home_id = ? OR away_id = ?) AND utc_date < ? AND status = 'FINISHED' AND hg IS NOT NULL", team, team, before).Order("utc_date DESC").Limit(5).Find(&ms)
	if len(ms) < 5 {
		return ""
	}
	rows := []resultRow{}
	for _, m := range ms {
		rows = append(rows, resultRow{HomeID: m.HomeID, AwayID: m.AwayID, FTHome: *m.HG, FTAway: *m.AG, Kickoff: m.UTCDate})
	}
	return formString(rows, team)
}

// h2hString: last 5 meetings from the home team's view, e.g. "H2-1, D1-1".
func h2hString(rows []resultRow, home int64) string {
	parts := []string{}
	for _, r := range rows {
		hg, ag := r.FTHome, r.FTAway
		gf, ga := hg, ag
		if r.HomeID != home {
			gf, ga = ag, hg
		}
		c := "D"
		if gf > ga {
			c = "W"
		} else if gf < ga {
			c = "L"
		}
		parts = append(parts, fmt.Sprintf("%s%d-%d %s", c, gf, ga, r.Kickoff.Format("2006-01-02")))
	}
	return strings.Join(parts, ", ")
}

// SourceCheckFor builds the source check of a fixture.
func (s *Service) SourceCheckFor(f Fixture) SourceCheck {
	now := s.Now()
	sc := SourceCheck{Sections: map[string]string{}, Items: []SCItem{}}
	sc.Summary.Note = "Conflicts never block the page; they are flagged."
	sc.Summary.CheckedAt = now
	srcSeen := map[string]bool{}
	add := func(it SCItem, kind scKind) {
		if len(it.Values) == 0 {
			return
		}
		EvalItem(&it, kind)
		for _, v := range it.Values {
			srcSeen[v.Source] = true
		}
		sc.Items = append(sc.Items, it)
	}
	before := minTime(f.Kickoff, now)
	hn, an := s.teamName(f.HomeID), s.teamName(f.AwayID)
	// Standings: API-Football snapshot vs football-data.org vs a table from football-data.co.uk results.
	var afRows []Standing
	s.DB.Raw(`SELECT * FROM fb_standings WHERE competition_id = ? AND season = ? AND source = ? AND team_id IN ? AND as_of = (SELECT MAX(as_of) FROM fb_standings WHERE competition_id = ? AND season = ? AND source = ?)`,
		f.CompetitionID, f.Season, ProviderAPIFootball, []int64{f.HomeID, f.AwayID}, f.CompetitionID, f.Season, ProviderAPIFootball).Scan(&afRows)
	af := map[int64]Standing{}
	for _, r := range afRows {
		af[r.TeamID] = r
	}
	fdo := map[int64]FDOrgStanding{}
	if code := fdOrgCodeOf(f.CompetitionID); code != "" {
		var rs []FDOrgStanding
		s.DB.Where("competition = ? AND team_id IN ?", code, []int64{f.HomeID, f.AwayID}).Find(&rs)
		for _, r := range rs {
			fdo[r.TeamID] = r
		}
	}
	hist := s.histTable(f.CompetitionID, f.Season, before)
	histRank := map[int64]int{}
	for i, r := range rankRows(hist) {
		histRank[r.team] = i + 1
	}
	// The football-data.co.uk table is only compared when it covers the season so far.
	histOK := len(hist) >= 6
	for _, sd := range []struct {
		side string
		team int64
		name string
	}{{"home", f.HomeID, hn}, {"away", f.AwayID, an}} {
		type stat struct {
			key, label string
			af         func(Standing) int
			fo         func(FDOrgStanding) int
			fd         func(*tableRow) int
		}
		stats := []stat{
			{"position", "league position", func(r Standing) int { return r.Rank }, func(r FDOrgStanding) int { return r.Position }, nil},
			{"points", "points", func(r Standing) int { return r.Points }, func(r FDOrgStanding) int { return r.Points }, func(r *tableRow) int { return 3*r.w + r.d }},
			{"played", "played", func(r Standing) int { return r.Played }, func(r FDOrgStanding) int { return r.Played }, func(r *tableRow) int { return r.p }},
			{"goals_for", "goals for", func(r Standing) int { return r.GF }, func(r FDOrgStanding) int { return r.GF }, func(r *tableRow) int { return r.gf }},
			{"goals_against", "goals against", func(r Standing) int { return r.GA }, func(r FDOrgStanding) int { return r.GA }, func(r *tableRow) int { return r.ga }},
		}
		for _, stt := range stats {
			it := SCItem{Key: sd.side + "." + stt.key, Section: "standings", Label: sd.name + " — " + stt.label, Side: sd.side}
			if r, ok := af[sd.team]; ok {
				t := r.FetchedAt
				it.Values = append(it.Values, SCValue{Source: ProviderAPIFootball, Value: stt.af(r), FetchedAt: &t, URL: docAF + "#tag/Standings"})
			}
			if r, ok := fdo[sd.team]; ok {
				t := r.FetchedAt
				it.Values = append(it.Values, SCValue{Source: ProviderFDOrg, Value: stt.fo(r), FetchedAt: &t, URL: "https://docs.football-data.org/general/v4/standings.html"})
			}
			// football-data.co.uk's table only counts when it has (nearly) every
			// match the provider table has (otherwise it's just incomplete).
			complete := true
			if a, ok := af[sd.team]; ok {
				if r, ok := hist[sd.team]; ok && r.p < a.Played-1 {
					complete = false
				}
			}
			if histOK && complete {
				if r, ok := hist[sd.team]; ok {
					v := 0
					if stt.fd != nil {
						v = stt.fd(r)
					} else {
						v = histRank[sd.team]
					}
					it.Values = append(it.Values, SCValue{Source: ProviderFDCOUK, Value: v, URL: "https://www.football-data.co.uk/data.php"})
				}
			}
			add(it, scNumber)
		}
		// Last five.
		it := SCItem{Key: sd.side + ".last5", Section: "results", Label: sd.name + " — last 5", Side: sd.side}
		if v, _ := s.afLast5(sd.team, before); len(v) == 5 {
			it.Values = append(it.Values, SCValue{Source: ProviderAPIFootball, Value: v})
		}
		if v := s.fdLast5(sd.team, before); v != "" {
			it.Values = append(it.Values, SCValue{Source: ProviderFDCOUK, Value: v})
		}
		if v := s.fdoLast5(sd.team, before); v != "" {
			it.Values = append(it.Values, SCValue{Source: ProviderFDOrg, Value: v})
		}
		add(it, scForm)
		// Identity: the names each source uses for this team.
		idt := SCItem{Key: sd.side + ".identity", Section: "identity", Label: sd.name + " — same team in every source", Side: sd.side,
			Values: []SCValue{{Source: ProviderAPIFootball, Value: sd.name}}}
		if c, _ := s.clubEloSide(sd.team); c != nil {
			idt.Values = append(idt.Values, SCValue{Source: ProviderClubElo, Value: c.Club})
		}
		var opName string
		if sd.side == "home" {
			s.DB.Model(&OPFixture{}).Where("fixture_id = ?", f.ID).Limit(1).Pluck("home", &opName)
		} else {
			s.DB.Model(&OPFixture{}).Where("fixture_id = ?", f.ID).Limit(1).Pluck("away", &opName)
		}
		if opName != "" {
			idt.Values = append(idt.Values, SCValue{Source: ProviderOddsPapi, Value: opName})
		}
		var fdName string
		s.DB.Model(&HistMatch{}).Where("home_id = ?", sd.team).Order("date DESC").Limit(1).Pluck("home", &fdName)
		if fdName != "" {
			idt.Values = append(idt.Values, SCValue{Source: ProviderFDCOUK, Value: fdName})
		}
		var foName string
		s.DB.Model(&FDOrgStanding{}).Where("team_id = ?", sd.team).Limit(1).Pluck("team", &foName)
		if foName != "" {
			idt.Values = append(idt.Values, SCValue{Source: ProviderFDOrg, Value: foName})
		}
		if len(idt.Values) > 1 {
			add(idt, scName)
		}
	}
	// Head-to-head: API-Football vs football-data.co.uk (last 5 meetings).
	var afH []resultRow
	s.DB.Raw(`SELECT id, kickoff, competition_id, season, home_id, away_id, ft_home, ft_away FROM fb_fixtures WHERE ((home_id = ? AND away_id = ?) OR (home_id = ? AND away_id = ?))
		AND id > 0 AND status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND kickoff < ? ORDER BY kickoff DESC LIMIT 5`, f.HomeID, f.AwayID, f.AwayID, f.HomeID, before).Scan(&afH)
	var fdH []HistMatch
	s.DB.Where("((home_id = ? AND away_id = ?) OR (home_id = ? AND away_id = ?)) AND date < ?", f.HomeID, f.AwayID, f.AwayID, f.HomeID, before).Order("date DESC").Limit(5).Find(&fdH)
	h2h := SCItem{Key: "h2h.results", Section: "h2h", Label: "Head-to-head (last 5)"}
	if len(afH) > 0 {
		h2h.Values = append(h2h.Values, SCValue{Source: ProviderAPIFootball, Value: h2hString(afH, f.HomeID)})
	}
	if len(fdH) > 0 {
		rows := []resultRow{}
		for _, h := range fdH {
			rows = append(rows, resultRow{HomeID: h.HomeID, AwayID: h.AwayID, FTHome: h.HG, FTAway: h.AG, Kickoff: h.Date})
		}
		// Only compare meetings in both sources' date range.
		if len(afH) > 0 && len(rows) > len(afH) {
			rows = rows[:len(afH)]
		}
		h2h.Values = append(h2h.Values, SCValue{Source: ProviderFDCOUK, Value: h2hDays(rows, f.HomeID, afH)})
		if len(afH) > 0 {
			h2h.Values[0].Value = h2hDays(afH, f.HomeID, afH)
		}
	}
	add(h2h, scText)
	// Kickoff: API-Football vs OddsPapi vs football-data.org vs ClubElo (date).
	ko := SCItem{Key: "kickoff", Section: "kickoff", Label: "Kickoff", Values: []SCValue{{Source: ProviderAPIFootball, Value: f.Kickoff}}}
	var op OPFixture
	if s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&op).RowsAffected > 0 {
		ko.Values = append(ko.Values, SCValue{Source: ProviderOddsPapi, Value: op.Start})
	}
	var fm FDOrgMatch
	if s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&fm).RowsAffected > 0 {
		ko.Values = append(ko.Values, SCValue{Source: ProviderFDOrg, Value: fm.UTCDate})
	}
	var cf ClubEloFixture
	if s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&cf).RowsAffected > 0 {
		ko.Values = append(ko.Values, SCValue{Source: ProviderClubElo, Value: cf.Date.Format("2006-01-02")})
	}
	ko.Values = append(ko.Values, s.ofKickoffValues(f)...) // v2.18: openfootball (openfootball.go)
	// News mentioning a postponement in the last 3 days.
	var pn []NewsItem
	s.DB.Where("postponed AND published_at > ? AND id IN (SELECT news_id FROM fb_news_teams WHERE team_id IN ?)", now.AddDate(0, 0, -3), []int64{f.HomeID, f.AwayID}).
		Order("published_at DESC").Limit(3).Find(&pn)
	add(ko, scTime)
	for _, it := range s.ofResultItems(f) { // v2.18: full-time score at API-Football v openfootball / football-data.org
		add(it, scText)
	}
	if len(pn) > 0 {
		it := SCItem{Key: "kickoff.news", Section: "kickoff", Label: "News about the date or venue", Status: "conflict", SourcesAgreeing: []string{},
			Values: []SCValue{}, Disagreeing: []SCValue{}}
		for _, n := range pn {
			t := n.PublishedAt
			it.Values = append(it.Values, SCValue{Source: "news", Value: n.Title, FetchedAt: &t, URL: n.URL})
		}
		it.Value = pn[0].Title
		it.Disagreeing = it.Values
		it.Note = "news mentions a postponement or change — check the kickoff"
		srcSeen["news"] = true
		sc.Items = append(sc.Items, it)
	}
	// Summary.
	rank := map[string]int{"agree": 0, "single": 0, "minor": 1, "conflict": 2}
	secWorst := map[string]string{}
	for _, it := range sc.Items {
		switch it.Status {
		case "agree":
			sc.Summary.Agree++
		case "minor":
			sc.Summary.Minor++
		case "conflict":
			sc.Summary.Conflict++
		default:
			sc.Summary.Single++
		}
		cur, ok := secWorst[it.Section]
		if !ok || rank[it.Status] > rank[cur] || (cur == "single" && it.Status == "agree") {
			secWorst[it.Section] = it.Status
		}
	}
	for _, sec := range []string{"standings", "results", "h2h", "kickoff", "identity"} {
		if v, ok := secWorst[sec]; ok {
			sc.Sections[sec] = v
		} else {
			sc.Sections[sec] = "unchecked"
		}
	}
	switch {
	case sc.Summary.Conflict > 0:
		sc.Summary.Status = "conflict"
	case sc.Summary.Minor > 0:
		sc.Summary.Status = "minor"
	case sc.Summary.Agree > 0:
		sc.Summary.Status = "agree"
	default:
		sc.Summary.Status = "unchecked"
	}
	for k := range srcSeen {
		sc.Summary.Sources = append(sc.Summary.Sources, k)
	}
	sort.Strings(sc.Summary.Sources)
	if sc.Summary.Sources == nil {
		sc.Summary.Sources = []string{}
	}
	return sc
}

// h2hDays formats meetings on the dates both sources have.
func h2hDays(rows []resultRow, home int64, ref []resultRow) string {
	return h2hString(rows, home)
}
