package football

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// football-data.co.uk (free CSV files, no key; please attribute the source —
// https://www.football-data.co.uk/notes.txt explains the columns):
//   main leagues, one file per season:  /mmz4281/<yyYY>/<code>.csv   e.g. /mmz4281/2526/E0.csv
//     Div,Date,Time,HomeTeam,AwayTeam,FTHG,FTAG,FTR,HTHG,HTAG,HTR,…,PSH,PSD,PSA,…,AvgH,…,PSCH,PSCD,PSCA,…,AvgCH,AvgCD,AvgCA,MaxCH,…
//   "new leagues", all seasons in one file:  /new/<CTRY>.csv   e.g. /new/POL.csv
//     Country,League,Season,Date,Time,Home,Away,HG,AG,Res,PSCH,PSCD,PSCA,MaxCH,MaxCD,MaxCA,AvgCH,AvgCD,AvgCA,…
// Dates are dd/mm/yyyy (older files dd/mm/yy), times UK local time.
// Used for: results history to fit the model (saves API-Football calls:
// a previous season seeded here is not fetched again), closing odds
// (Pinnacle where present) for grading and backtests, and the source check.

// FDMainLeagues maps football-data.co.uk division codes to API-Football league ids.
var FDMainLeagues = map[string]int64{"E0": 39, "E1": 40, "E2": 41, "E3": 42, "EC": 43, "SC0": 179, "SC1": 180, "D1": 78, "D2": 79, "I1": 135, "I2": 136,
	"SP1": 140, "SP2": 141, "F1": 61, "F2": 62, "N1": 88, "B1": 144, "P1": 94, "T1": 203, "G1": 197}

// FDNewLeagues maps the "new leagues" files (country code → league name → API-Football id).
var FDNewLeagues = map[string]struct {
	Country string
	League  int64
}{"POL": {"Poland", 106}, "AUT": {"Austria", 218}, "DNK": {"Denmark", 119}, "ROU": {"Romania", 283}, "IRL": {"Ireland", 357}, "SWZ": {"Switzerland", 207},
	"NOR": {"Norway", 103}, "SWE": {"Sweden", 113}, "FIN": {"Finland", 244}, "USA": {"USA", 253}, "BRA": {"Brazil", 71}, "ARG": {"Argentina", 128},
	"MEX": {"Mexico", 262}, "JPN": {"Japan", 98}, "CHN": {"China", 169}, "RUS": {"Russia", 235}}

// FDRow is a parsed football-data.co.uk match.
type FDRow struct {
	Div, Country, League string
	Season               int
	Date                 time.Time
	Home, Away           string
	HG, AG               int
	HTHG, HTAG           *int
	PSCH, PSCD, PSCA     *float64
	AvgCH, AvgCD, AvgCA  *float64
	MaxCH, MaxCD, MaxCA  *float64
}

var ukLoc = func() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		return time.UTC
	}
	return l
}()

func parseFDDate(d, t string) (time.Time, bool) {
	d = strings.TrimSpace(d)
	if d == "" {
		return time.Time{}, false
	}
	hh, mm := 15, 0 // unknown kick-off time: mid-afternoon UK
	if p := strings.Split(strings.TrimSpace(t), ":"); len(p) == 2 {
		if h, err := strconv.Atoi(p[0]); err == nil {
			hh = h
		}
		if m, err := strconv.Atoi(p[1]); err == nil {
			mm = m
		}
	}
	for _, f := range []string{"02/01/2006", "02/01/06", "2/1/2006", "2006-01-02"} {
		if x, err := time.ParseInLocation(f, d, ukLoc); err == nil {
			return time.Date(x.Year(), x.Month(), x.Day(), hh, mm, 0, 0, ukLoc).UTC(), true
		}
	}
	return time.Time{}, false
}

func intCol(rec []string, idx map[string]int, names ...string) (int, bool) {
	for _, n := range names {
		if v := col(rec, idx, n); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return int(f), true
			}
		}
	}
	return 0, false
}

// ParseFDCSV reads a main-league season file or a "new leagues" file.
// season is the main file's season start year (0 for new/ files: taken from
// the Season column, "2025/2026" → 2025, "2025" → 2025).
func ParseFDCSV(body []byte, div string, season int) ([]FDRow, error) {
	rows, idx, err := readCSV(body)
	if err != nil {
		return nil, err
	}
	_, main := idx["HomeTeam"]
	_, isNew := idx["Home"]
	if !main && !isNew {
		return nil, fmt.Errorf("not a football-data.co.uk CSV (no HomeTeam / Home column)")
	}
	out := []FDRow{}
	for _, r := range rows {
		var row FDRow
		var ok bool
		row.Date, ok = parseFDDate(col(r, idx, "Date"), col(r, idx, "Time"))
		if !ok {
			continue
		}
		if main {
			row.Div, row.Home, row.Away, row.Season = firstNonEmpty(col(r, idx, "Div"), div), col(r, idx, "HomeTeam"), col(r, idx, "AwayTeam"), season
			hg, ok1 := intCol(r, idx, "FTHG", "HG")
			ag, ok2 := intCol(r, idx, "FTAG", "AG")
			if !ok1 || !ok2 {
				continue
			}
			row.HG, row.AG = hg, ag
			if v, ok := intCol(r, idx, "HTHG"); ok {
				row.HTHG = &v
			}
			if v, ok := intCol(r, idx, "HTAG"); ok {
				row.HTAG = &v
			}
		} else {
			row.Country, row.League, row.Home, row.Away = col(r, idx, "Country"), col(r, idx, "League"), col(r, idx, "Home"), col(r, idx, "Away")
			row.Div = div
			if row.League != "" {
				row.Div = div + " " + row.League
			}
			hg, ok1 := intCol(r, idx, "HG", "FTHG")
			ag, ok2 := intCol(r, idx, "AG", "FTAG")
			if !ok1 || !ok2 {
				continue
			}
			row.HG, row.AG = hg, ag
			sv := col(r, idx, "Season")
			if i := strings.IndexAny(sv, "/-"); i > 0 {
				sv = sv[:i]
			}
			row.Season, _ = strconv.Atoi(strings.TrimSpace(sv))
			if row.Season == 0 {
				row.Season = row.Date.Year()
			}
		}
		if row.Home == "" || row.Away == "" {
			continue
		}
		row.PSCH, row.PSCD, row.PSCA = colF(r, idx, "PSCH"), colF(r, idx, "PSCD"), colF(r, idx, "PSCA")
		row.AvgCH, row.AvgCD, row.AvgCA = colF(r, idx, "AvgCH", "AvgH"), colF(r, idx, "AvgCD", "AvgD"), colF(r, idx, "AvgCA", "AvgA")
		row.MaxCH, row.MaxCD, row.MaxCA = colF(r, idx, "MaxCH", "MaxH"), colF(r, idx, "MaxCD", "MaxD"), colF(r, idx, "MaxCA", "MaxA")
		out = append(out, row)
	}
	return out, nil
}

// seasonCode is football-data.co.uk's season folder: 2025 → "2526".
func seasonCode(start int) string { return fmt.Sprintf("%02d%02d", start%100, (start+1)%100) }

// fetchFDFile downloads and stores one file.
func (s *Service) fetchFDFile(ctx context.Context, path, div string, season int, comp int64, prio int, task string, minSeason int) (FDSeedResult, error) {
	var res FDSeedResult
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderFDCOUK, Path: path, Prio: prio, Task: task, Accept: "text/csv"})
	if err != nil {
		return res, err
	}
	rows, err := ParseFDCSV(out.Body, div, season)
	if err != nil {
		return res, err
	}
	if minSeason > 0 {
		keep := rows[:0]
		for _, r := range rows {
			if r.Season >= minSeason {
				keep = append(keep, r)
			}
		}
		rows = keep
	}
	s.setResults(ProviderFDCOUK, task, len(rows))
	res.Files = 1
	r2, err := s.storeFDRows(rows, comp)
	res.Matches, res.Inserted, res.Merged, res.Odds = r2.Matches, r2.Inserted, r2.Merged, r2.Odds
	return res, err
}

// FDSeedResult counts what a seeding run did.
type FDSeedResult struct {
	Files    int `json:"files"`
	Matches  int `json:"matches"`
	Inserted int `json:"inserted"` // seeded fixtures (no API-Football fixture yet)
	Merged   int `json:"merged"`   // matched to an existing API-Football fixture
	Odds     int `json:"odds"`     // closing prices stored
}

// teamsOfComp lists a competition's teams (from its fixtures).
func (s *Service) teamsOfComp(comp int64) []struct {
	ID   int64
	Name string
} {
	var rows []struct {
		ID   int64
		Name string
	}
	s.DB.Raw(`SELECT DISTINCT t.id, t.name FROM fb_teams t JOIN fb_fixtures f ON (f.home_id = t.id OR f.away_id = t.id) WHERE f.competition_id = ? AND f.id > 0`, comp).Scan(&rows)
	return rows
}

// mapNames maps a provider's names to a competition's teams (alias first,
// then a unique best name ≥ 0.8); unmapped names are recorded.
func (s *Service) mapNames(provider string, names []string, comp int64, country string) map[string]int64 {
	out := map[string]int64{}
	teams := s.teamsOfComp(comp)
	aliases := s.aliasMap()
	for _, n := range names {
		if _, done := out[n]; done {
			continue
		}
		var id int64
		for _, k := range []string{normFree(n), NormTeam(n)} {
			if t, ok := aliases[k]; ok && k != "" {
				id = t
				break
			}
		}
		if id == 0 {
			bestV, secondV := 0.0, 0.0
			var best int64
			bestName := ""
			for _, t := range teams {
				v := TeamSim(n, t.Name)
				if v > bestV {
					secondV, bestV, best, bestName = bestV, v, t.ID, t.Name
				} else if v > secondV {
					secondV = v
				}
			}
			if bestV >= 0.8 && secondV < bestV-0.05 {
				id = best
				s.learnAliasFrom(n, id, provider)
			} else if len(teams) > 0 {
				s.noteUnmatched(provider, n, country, bestName, best)
			}
		}
		out[n] = id
	}
	return out
}

// seededID is the deterministic negative id of a fixture seeded from
// football-data.co.uk (never collides with API-Football's positive ids).
func seededID(comp int64, date time.Time, home, away int64) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "fdcouk|%d|%s|%d|%d", comp, date.Format("2006-01-02"), home, away)
	return -int64(h.Sum64() & 0x1fffffffffffff)
}

// storeFDRows upserts history rows; matches with both teams mapped are
// merged into the API-Football fixture of the same day and teams, or
// seeded as finished fixtures (negative ids) for the model; Pinnacle /
// average closing prices are stored as odds (source fdcouk).
func (s *Service) storeFDRows(rows []FDRow, comp int64) (FDSeedResult, error) {
	var res FDSeedResult
	if len(rows) == 0 {
		return res, nil
	}
	now := s.Now()
	var compRow Competition
	if comp != 0 {
		s.DB.Where("id = ?", comp).Limit(1).Find(&compRow)
	}
	names := []string{}
	for _, r := range rows {
		names = append(names, r.Home, r.Away)
	}
	var ids map[string]int64
	if comp != 0 {
		ids = s.mapNames(ProviderFDCOUK, names, comp, compRow.Country)
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			hm := HistMatch{Source: ProviderFDCOUK, Div: clip(r.Div, 40), Date: r.Date, Home: clip(r.Home, 80), Away: clip(r.Away, 80), Country: clip(r.Country, 40),
				League: clip(r.League, 80), Season: r.Season, HG: r.HG, AG: r.AG, HTHG: r.HTHG, HTAG: r.HTAG, PSCH: r.PSCH, PSCD: r.PSCD, PSCA: r.PSCA,
				AvgCH: r.AvgCH, AvgCD: r.AvgCD, AvgCA: r.AvgCA, MaxCH: r.MaxCH, MaxCD: r.MaxCD, MaxCA: r.MaxCA, CompetitionID: comp, FetchedAt: now}
			if ids != nil {
				hm.HomeID, hm.AwayID = ids[r.Home], ids[r.Away]
			}
			if hm.HomeID != 0 && hm.AwayID != 0 && hm.HomeID != hm.AwayID {
				var fid int64
				tx.Model(&Fixture{}).Where("home_id = ? AND away_id = ? AND kickoff BETWEEN ? AND ? AND id > 0", hm.HomeID, hm.AwayID, r.Date.Add(-36*time.Hour), r.Date.Add(36*time.Hour)).
					Limit(1).Pluck("id", &fid)
				if fid != 0 {
					hm.FixtureID = fid
					res.Merged++
				} else if id := seededID(comp, r.Date, hm.HomeID, hm.AwayID); r.Date.Before(now.Add(-3*time.Hour)) && tx.Model(&Fixture{}).Where("id = ?", id).Limit(1).Find(&Fixture{}).RowsAffected > 0 {
					hm.FixtureID = id // seeded before
				} else if r.Date.Before(now.Add(-3 * time.Hour)) {
					// Seed a finished fixture for the model.
					hg, ag := r.HG, r.AG
					f := Fixture{ID: id, CompetitionID: comp, Season: r.Season, Round: "football-data.co.uk", Kickoff: r.Date, Status: "FT", StatusLong: "Match Finished",
						HomeID: hm.HomeID, AwayID: hm.AwayID, HomeGoals: &hg, AwayGoals: &ag, FTHome: &hg, FTAway: &ag, HTHome: r.HTHG, HTAway: r.HTAG, FetchedAt: now,
						FinishedAt: &r.Date, UpdatedAt: now}
					if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&f).Error; err != nil {
						return err
					}
					hm.FixtureID = id
					res.Inserted++
				}
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "source"}, {Name: "div"}, {Name: "date"}, {Name: "home"}, {Name: "away"}},
				DoUpdates: clause.AssignmentColumns([]string{"hg", "ag", "hthg", "htag", "psch", "pscd", "psca", "avgch", "avgcd", "avgca", "maxch", "maxcd", "maxca",
					"competition_id", "home_id", "away_id", "fixture_id", "season", "fetched_at"})}).Create(&hm).Error; err != nil {
				return err
			}
			res.Matches++
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Odds = s.storeFDClosingOdds(comp)
	return res, nil
}

// storeFDClosingOdds writes closing prices of history rows linked to a
// fixture that has no football-data.co.uk prices yet. They are stamped one
// minute before kickoff so every "before kickoff" view treats them as the
// closing line.
func (s *Service) storeFDClosingOdds(comp int64) int {
	var hs []HistMatch
	q := s.DB.Where("fixture_id <> 0 AND (psch IS NOT NULL OR avgch IS NOT NULL)").
		Where("NOT EXISTS (SELECT 1 FROM fb_odds o WHERE o.fixture_id = fb_hist_matches.fixture_id AND o.source = ?)", ProviderFDCOUK)
	if comp != 0 {
		q = q.Where("competition_id = ?", comp)
	}
	q.Limit(20000).Find(&hs)
	kick := map[int64]time.Time{}
	if len(hs) > 0 {
		ids := []int64{}
		for _, h := range hs {
			ids = append(ids, h.FixtureID)
		}
		var ks []struct {
			ID      int64
			Kickoff time.Time
		}
		s.DB.Raw(`SELECT id, kickoff FROM fb_fixtures WHERE id IN ?`, ids).Scan(&ks)
		for _, k := range ks {
			kick[k.ID] = k.Kickoff
		}
	}
	rows := []Odd{}
	for _, h := range hs {
		// Stamped just before the earlier of the two kickoff times (the
		// files' UK times can differ from API-Football's).
		ref := h.Date
		if k, ok := kick[h.FixtureID]; ok && k.Before(ref) {
			ref = k
		}
		at := ref.Add(-time.Minute)
		add := func(book, title string, ph, pd, pa *float64) {
			if ph == nil || pd == nil || pa == nil || *ph <= 1 || *pd <= 1 || *pa <= 1 {
				return
			}
			for i, sel := range []string{SelHome, SelDraw, SelAway} {
				p := []*float64{ph, pd, pa}[i]
				rows = append(rows, Odd{FixtureID: h.FixtureID, Source: ProviderFDCOUK, Bookmaker: book, BookmakerTitle: title, Market: Mkt1X2, Selection: sel,
					Price: *p, FetchedAt: at, CheckedAt: at})
			}
		}
		add("pinnacle", "Pinnacle (closing, football-data.co.uk)", h.PSCH, h.PSCD, h.PSCA)
		add("market_avg", "Market average (closing, football-data.co.uk)", h.AvgCH, h.AvgCD, h.AvgCA)
	}
	n, _, _ := s.storeOdds(rows)
	return n
}

// dedupeSeeded removes seeded fixtures once API-Football has the same match
// (same teams within 36 h), moving history links and odds to the real one.
func (s *Service) dedupeSeeded() int {
	var pairs []struct {
		Seeded int64
		Real   int64
	}
	s.DB.Raw(`SELECT s.id AS seeded, r.id AS real FROM fb_fixtures s JOIN fb_fixtures r ON r.home_id = s.home_id AND r.away_id = s.away_id AND r.id > 0
		AND r.kickoff BETWEEN s.kickoff - interval '36 hours' AND s.kickoff + interval '36 hours' WHERE s.id < 0`).Scan(&pairs)
	for _, p := range pairs {
		s.DB.Transaction(func(tx *gorm.DB) error {
			tx.Model(&HistMatch{}).Where("fixture_id = ?", p.Seeded).Update("fixture_id", p.Real)
			tx.Exec(`DELETE FROM fb_odds WHERE fixture_id = ? AND source = ? AND EXISTS (SELECT 1 FROM fb_odds o WHERE o.fixture_id = ? AND o.source = ?)`, p.Seeded, ProviderFDCOUK, p.Real, ProviderFDCOUK)
			tx.Model(&Odd{}).Where("fixture_id = ?", p.Seeded).Update("fixture_id", p.Real)
			tx.Where("fixture_id = ?", p.Seeded).Delete(&Prediction{})
			return tx.Where("id = ?", p.Seeded).Delete(&Fixture{}).Error
		})
	}
	return len(pairs)
}

// fdTargets lists the files to fetch: main leagues of enabled (or known)
// competitions and the configured "new leagues" countries.
type fdTarget struct {
	Path      string
	Div       string
	Season    int
	Comp      int64
	Key       string
	Weekly    bool // the current season (refreshed weekly); past seasons once
	MinSeason int
}

func (s *Service) fdTargets(now time.Time) []fdTarget {
	st := s.FreeSettingsGet()
	comps := map[int64]Competition{}
	for _, c := range s.Competitions(false) {
		comps[c.ID] = c
	}
	out := []fdTarget{}
	codes := make([]string, 0, len(FDMainLeagues))
	for c := range FDMainLeagues {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, code := range codes {
		id := FDMainLeagues[code]
		c, ok := comps[id]
		if !ok || !c.Enabled {
			continue
		}
		cur := guessSeason(c, now)
		for k := 0; k < st.FDCOUKSeasons; k++ {
			season := cur - k
			out = append(out, fdTarget{Path: "/mmz4281/" + seasonCode(season) + "/" + code + ".csv", Div: code, Season: season, Comp: id,
				Key: fmt.Sprintf("fd:main:%s:%d", code, season), Weekly: k == 0})
		}
	}
	for _, ctry := range splitList(strings.ToUpper(st.FDCOUKNew), nil) {
		ctry = strings.ToUpper(ctry)
		nl, ok := FDNewLeagues[ctry]
		if !ok {
			continue
		}
		var comp int64
		if c, ok := comps[nl.League]; ok && (c.Enabled || c.Known) {
			comp = c.ID
		}
		out = append(out, fdTarget{Path: "/new/" + ctry + ".csv", Div: ctry, Comp: comp, Key: "fd:new:" + ctry, Weekly: true, MinSeason: now.Year() - st.FDCOUKSeasons})
	}
	return out
}

// SeedHistory downloads every due football-data.co.uk file now (super admin
// "seed history"; budgets apply). force refetches current-season files.
func (s *Service) SeedHistory(ctx context.Context, comp int64, seasons int) (FDSeedResult, error) {
	var tot FDSeedResult
	now := s.Now()
	st := s.FreeSettingsGet()
	if seasons > 0 && seasons <= 10 {
		st.FDCOUKSeasons = seasons
		s.DB.Model(&FreeSettings{}).Where("id = 1").Update("fdcouk_seasons", seasons)
	}
	for _, t := range s.fdTargets(now) {
		if comp != 0 && t.Comp != comp {
			continue
		}
		r, err := s.fetchFDFile(ctx, t.Path, t.Div, t.Season, t.Comp, PrioLow, t.Key, t.MinSeason)
		if err != nil {
			if IsKind(err, "not_found") {
				s.saveTaskState(t.Key, true, "no file")
				continue
			}
			if IsKind(err, "budget") || IsKind(err, "backoff") {
				return tot, err
			}
			s.saveTaskState(t.Key, false, err.Error())
			continue
		}
		s.saveTaskState(t.Key, true, fmt.Sprintf("%d matches", r.Matches))
		s.markSeasonSeeded(t)
		tot.Files += r.Files
		tot.Matches += r.Matches
		tot.Inserted += r.Inserted
		tot.Merged += r.Merged
		tot.Odds += r.Odds
	}
	s.dedupeSeeded()
	s.DB.Model(&FreeSettings{}).Where("id = 1").Update("last_seed_at", now)
	ResetModelCache()
	return tot, nil
}

// markSeasonSeeded tells the API-Football planner a past season is already
// in the database (its "previous season" backfill is then skipped for 120 days).
func (s *Service) markSeasonSeeded(t fdTarget) {
	if t.Comp == 0 || t.Weekly || t.Season == 0 {
		return
	}
	var n int64
	s.DB.Model(&HistMatch{}).Where("competition_id = ? AND season = ? AND fixture_id <> 0", t.Comp, t.Season).Count(&n)
	if n >= 30 {
		s.saveTaskState(fmt.Sprintf("af:season:%d:%d", t.Comp, t.Season), true, fmt.Sprintf("seeded from football-data.co.uk (%d matches)", n))
	}
}

// histTable computes a season table from football-data.co.uk results (the
// source check's independent table for main leagues).
func (s *Service) histTable(comp int64, season int, before time.Time) map[int64]*tableRow {
	var hs []HistMatch
	s.DB.Where("competition_id = ? AND season = ? AND date < ? AND home_id <> 0 AND away_id <> 0", comp, season, before).Order("date").Find(&hs)
	m := map[int64]*tableRow{}
	for _, h := range hs {
		addResult(m, resultRow{ID: h.FixtureID, Kickoff: h.Date, CompetitionID: comp, Season: season, HomeID: h.HomeID, AwayID: h.AwayID, FTHome: h.HG, FTAway: h.AG})
	}
	return m
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
