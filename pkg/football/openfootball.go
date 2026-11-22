package football

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// openfootball football.json (public domain, CC0 —
// https://github.com/openfootball/football.json): fixtures and results as
// raw GitHub files <season>/<code>.json, e.g. 2026-27/en.1.json, rebuilt
// daily at about 05:00 UTC (README). League codes from the README / repo:
// en.1 Premier League, en.2 Championship, de.1 Bundesliga, de.2
// 2. Bundesliga, es.1 La Liga, es.2 Segunda División, it.1 Serie A, it.2
// Serie B, fr.1 Ligue 1, fr.2 Ligue 2. File: {name, matches: [{round, date
// "2026-08-21", time "20:00" (local) or "19:00 UTC+0", team1, team2, score:
// {ht: [h, a], ft: [h, a], et?, p?} or a bare [h, a] (full time)}]}.
// Used for the Source check (kickoff and result agreement) and as the
// grading fallback; one fetch per file per day.

// OFLeague is a football.json file's competition.
type OFLeague struct {
	Comp    int64
	Name    string
	Country string
	Zone    string // local time zone of times without a UTC offset
}

// OFLeagues maps football.json codes to API-Football competitions.
var OFLeagues = map[string]OFLeague{
	"en.1": {39, "Premier League", "England", "Europe/London"}, "en.2": {40, "Championship", "England", "Europe/London"},
	"de.1": {78, "Bundesliga", "Germany", "Europe/Berlin"}, "de.2": {79, "2. Bundesliga", "Germany", "Europe/Berlin"},
	"es.1": {140, "La Liga", "Spain", "Europe/Madrid"}, "es.2": {141, "Segunda División", "Spain", "Europe/Madrid"},
	"it.1": {135, "Serie A", "Italy", "Europe/Rome"}, "it.2": {136, "Serie B", "Italy", "Europe/Rome"},
	"fr.1": {61, "Ligue 1", "France", "Europe/Paris"}, "fr.2": {62, "Ligue 2", "France", "Europe/Paris"},
}

// OFSeason is the football.json season folder of a date ("2026-27").
func OFSeason(t time.Time) string {
	y := t.Year()
	if t.Month() < time.July {
		y--
	}
	return fmt.Sprintf("%d-%02d", y, (y+1)%100)
}

// OFRow is a parsed football.json match.
type OFRow struct {
	Round       string
	Date        time.Time // the local date (UTC midnight of that day)
	Kickoff     *time.Time
	Team1       string
	Team2       string
	HG, AG      *int
	HasTimeZone bool
}

type ofFile struct {
	Name    string `json:"name"`
	Matches []struct {
		Round string          `json:"round"`
		Date  string          `json:"date"`
		Time  string          `json:"time"`
		Team1 string          `json:"team1"`
		Team2 string          `json:"team2"`
		Score json.RawMessage `json:"score"`
	} `json:"matches"`
}

var ofTimeRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})(?:\s*UTC([+-]\d{1,2})(?::?(\d{2}))?)?$`)

// ofScore reads {ft: [h, a]} or [h, a].
func ofScore(raw json.RawMessage) (*int, *int) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []int
	if raw[0] == '[' {
		if json.Unmarshal(raw, &arr) == nil && len(arr) == 2 {
			return &arr[0], &arr[1]
		}
		return nil, nil
	}
	var obj struct {
		FT []int `json:"ft"`
	}
	if json.Unmarshal(raw, &obj) == nil && len(obj.FT) == 2 {
		return &obj.FT[0], &obj.FT[1]
	}
	return nil, nil
}

// ParseOFFile parses a football.json file; times without an offset are in
// the league's local zone.
func ParseOFFile(body []byte, zone string) (string, []OFRow, error) {
	var f ofFile
	if err := json.Unmarshal(body, &f); err != nil {
		return "", nil, fmt.Errorf("not a football.json file: %w", err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc = time.UTC
	}
	out := []OFRow{}
	for _, m := range f.Matches {
		d, err := time.Parse("2006-01-02", strings.TrimSpace(m.Date))
		if err != nil || m.Team1 == "" || m.Team2 == "" {
			continue
		}
		r := OFRow{Round: m.Round, Date: d, Team1: strings.TrimSpace(m.Team1), Team2: strings.TrimSpace(m.Team2)}
		if g := ofTimeRe.FindStringSubmatch(strings.TrimSpace(m.Time)); g != nil {
			hh, _ := strconv.Atoi(g[1])
			mm, _ := strconv.Atoi(g[2])
			var t time.Time
			if g[3] != "" {
				off, _ := strconv.Atoi(g[3])
				offM, _ := strconv.Atoi(g[4])
				sign := 1
				if off < 0 {
					sign = -1
				}
				z := time.FixedZone("", off*3600+sign*offM*60)
				t = time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, z).UTC()
				r.HasTimeZone = true
			} else {
				t = time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, loc).UTC()
			}
			r.Kickoff = &t
		}
		r.HG, r.AG = ofScore(m.Score)
		out = append(out, r)
	}
	return f.Name, out, nil
}

// ofCodes are the configured files with a known competition.
func ofCodes(s string) []string {
	out := []string{}
	for _, c := range splitList(s, nil) {
		c = strings.ToLower(strings.TrimSpace(c))
		if _, ok := OFLeagues[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

// fetchOFFile gets one file, maps team names and links fixtures.
func (s *Service) fetchOFFile(ctx context.Context, code, season string, prio int, task string) (int, int, error) {
	lg, ok := OFLeagues[code]
	if !ok {
		return 0, 0, fmt.Errorf("unknown football.json code %q", code)
	}
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderOpenFootball, Path: "/" + season + "/" + code + ".json", Prio: prio, Task: task, Accept: "application/json"})
	if err != nil {
		return 0, 0, err
	}
	_, rows, err := ParseOFFile(out.Body, lg.Zone)
	if err != nil {
		return 0, 0, err
	}
	s.setResults(ProviderOpenFootball, task, len(rows))
	names := []string{}
	for _, r := range rows {
		names = append(names, r.Team1, r.Team2)
	}
	ids := s.mapNames(ProviderOpenFootball, names, lg.Comp, lg.Country)
	now := s.Now()
	linked := 0
	for _, r := range rows {
		m := OFMatch{Code: code, Season: season, Date: r.Date, Team1: clip(r.Team1, 120), Team2: clip(r.Team2, 120), Round: clip(r.Round, 60), Kickoff: r.Kickoff,
			HG: r.HG, AG: r.AG, CompetitionID: lg.Comp, HomeID: ids[r.Team1], AwayID: ids[r.Team2], FetchedAt: now}
		if m.HomeID != 0 && m.AwayID != 0 {
			ref := r.Date.Add(12 * time.Hour)
			if r.Kickoff != nil {
				ref = *r.Kickoff
			}
			var fid int64
			s.DB.Model(&Fixture{}).Where("home_id = ? AND away_id = ? AND id > 0 AND kickoff BETWEEN ? AND ?", m.HomeID, m.AwayID, ref.Add(-36*time.Hour), ref.Add(36*time.Hour)).
				Limit(1).Pluck("id", &fid)
			m.FixtureID = fid
			if fid != 0 {
				linked++
			}
		}
		s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "code"}, {Name: "season"}, {Name: "date"}, {Name: "team1"}, {Name: "team2"}},
			DoUpdates: clause.AssignmentColumns([]string{"round", "kickoff", "hg", "ag", "competition_id", "home_id", "away_id", "fixture_id", "fetched_at"})}).Create(&m)
	}
	s.DB.Model(&WinSettings{}).Where("id = 1").Update("last_of_at", now)
	return len(rows), linked, nil
}

// ofKickoffValues adds openfootball's kickoff to the Source check.
func (s *Service) ofKickoffValues(f Fixture) []SCValue {
	var m OFMatch
	if s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&m).RowsAffected == 0 {
		return nil
	}
	t := m.FetchedAt
	if m.Kickoff != nil {
		return []SCValue{{Source: ProviderOpenFootball, Value: *m.Kickoff, FetchedAt: &t, URL: docOpenFootball}}
	}
	return []SCValue{{Source: ProviderOpenFootball, Value: m.Date.Format("2006-01-02"), FetchedAt: &t, URL: docOpenFootball}}
}

// ofResultItems compares the full-time score across API-Football,
// openfootball and football-data.org once a match is over.
func (s *Service) ofResultItems(f Fixture) []SCItem {
	it := SCItem{Key: "result", Section: "results", Label: "Full-time score", Values: []SCValue{}}
	if hg, ag, ok := fixtureScore(f); ok {
		it.Values = append(it.Values, SCValue{Source: ProviderAPIFootball, Value: fmt.Sprintf("%d-%d", hg, ag)})
	}
	var m OFMatch
	if s.DB.Where("fixture_id = ? AND hg IS NOT NULL AND ag IS NOT NULL", f.ID).Limit(1).Find(&m).RowsAffected > 0 {
		t := m.FetchedAt
		it.Values = append(it.Values, SCValue{Source: ProviderOpenFootball, Value: fmt.Sprintf("%d-%d", *m.HG, *m.AG), FetchedAt: &t, URL: docOpenFootball})
	}
	var fo FDOrgMatch
	if s.DB.Where("fixture_id = ? AND status = 'FINISHED' AND hg IS NOT NULL AND ag IS NOT NULL", f.ID).Limit(1).Find(&fo).RowsAffected > 0 {
		t := fo.FetchedAt
		it.Values = append(it.Values, SCValue{Source: ProviderFDOrg, Value: fmt.Sprintf("%d-%d", *fo.HG, *fo.AG), FetchedAt: &t})
	}
	if len(it.Values) < 2 {
		return nil
	}
	return []SCItem{it}
}
