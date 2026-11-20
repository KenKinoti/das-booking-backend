package football

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// ClubElo (http://clubelo.com/API, free, no key) serves CSV:
//   GET /YYYY-MM-DD  → Rank,Club,Country,Level,Elo,From,To   (every club's rating that day)
//   GET /Fixtures    → Date,Country,Home,Away,GD<-5,GD=-5,…,GD=0,…,GD=5,GD>5,R:0-0,R:0-1,…
//                      (probabilities of each goal difference for upcoming matches)
// Ratings are an independent free model shown next to DASYIN's (and used,
// with a weight, by the jackpot analyser). Clubs are mapped to API-Football
// teams through the alias table (provider "clubelo") and name rules.

// ClubEloCountry maps API-Football country names to ClubElo's codes.
var ClubEloCountry = map[string]string{"England": "ENG", "Spain": "ESP", "Italy": "ITA", "Germany": "GER", "France": "FRA", "Netherlands": "NED",
	"Portugal": "POR", "Belgium": "BEL", "Scotland": "SCO", "Austria": "AUT", "Switzerland": "SUI", "Denmark": "DEN", "Sweden": "SWE", "Norway": "NOR",
	"Poland": "POL", "Czech-Republic": "CZE", "Czech Republic": "CZE", "Romania": "ROM", "Greece": "GRE", "Turkey": "TUR", "Ukraine": "UKR", "Russia": "RUS",
	"Croatia": "CRO", "Serbia": "SRB", "Hungary": "HUN", "Bulgaria": "BUL", "Slovakia": "SVK", "Slovenia": "SVN", "Finland": "FIN", "Ireland": "IRL",
	"Northern-Ireland": "NIR", "Northern Ireland": "NIR", "Wales": "WAL", "Cyprus": "CYP", "Israel": "ISR", "Iceland": "ISL", "Bosnia": "BIH",
	"Belarus": "BLR", "Kazakhstan": "KAZ", "Azerbaijan": "AZE", "Georgia": "GEO", "Armenia": "ARM", "Albania": "ALB", "Lithuania": "LTU", "Latvia": "LVA",
	"Estonia": "EST", "Luxembourg": "LUX", "Malta": "MLT", "Moldova": "MDA", "North-Macedonia": "MKD", "Montenegro": "MNE", "Faroe-Islands": "FRO"}

// ClubEloRow is a parsed ratings row.
type ClubEloRow struct {
	Rank    *int
	Club    string
	Country string
	Level   int
	Elo     float64
	From    time.Time
	To      time.Time
}

// ClubEloFix is a parsed fixtures row.
type ClubEloFix struct {
	Date                time.Time
	Country             string
	Home, Away          string
	PHome, PDraw, PAway float64
}

func readCSV(body []byte) ([][]string, map[string]int, error) {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(body))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	head, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("empty CSV")
	}
	idx := map[string]int{}
	for i, h := range head {
		idx[strings.TrimSpace(h)] = i
	}
	rows := [][]string{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		rows = append(rows, rec)
	}
	return rows, idx, nil
}

func col(rec []string, idx map[string]int, name string) string {
	if i, ok := idx[name]; ok && i < len(rec) {
		return strings.TrimSpace(rec[i])
	}
	return ""
}

func colF(rec []string, idx map[string]int, names ...string) *float64 {
	for _, n := range names {
		if v := col(rec, idx, n); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(f) {
				return &f
			}
		}
	}
	return nil
}

// ParseClubEloRatings reads GET /YYYY-MM-DD.
func ParseClubEloRatings(body []byte) ([]ClubEloRow, error) {
	rows, idx, err := readCSV(body)
	if err != nil {
		return nil, err
	}
	if _, ok := idx["Club"]; !ok {
		return nil, fmt.Errorf("not a ClubElo ratings CSV (no Club column)")
	}
	out := []ClubEloRow{}
	for _, r := range rows {
		e := colF(r, idx, "Elo")
		if e == nil || col(r, idx, "Club") == "" {
			continue
		}
		row := ClubEloRow{Club: col(r, idx, "Club"), Country: col(r, idx, "Country"), Elo: *e}
		if v, err := strconv.Atoi(col(r, idx, "Rank")); err == nil {
			row.Rank = &v
		}
		row.Level, _ = strconv.Atoi(col(r, idx, "Level"))
		row.From, _ = time.Parse("2006-01-02", col(r, idx, "From"))
		row.To, _ = time.Parse("2006-01-02", col(r, idx, "To"))
		out = append(out, row)
	}
	return out, nil
}

// ParseClubEloFixtures reads GET /Fixtures: P(home) = Σ P(GD ≥ 1), P(draw) =
// P(GD = 0), P(away) = Σ P(GD ≤ −1).
func ParseClubEloFixtures(body []byte) ([]ClubEloFix, error) {
	rows, idx, err := readCSV(body)
	if err != nil {
		return nil, err
	}
	if _, ok := idx["Home"]; !ok {
		return nil, fmt.Errorf("not a ClubElo fixtures CSV (no Home column)")
	}
	out := []ClubEloFix{}
	for _, r := range rows {
		d, err := time.Parse("2006-01-02", col(r, idx, "Date"))
		if err != nil {
			continue
		}
		f := ClubEloFix{Date: d, Country: col(r, idx, "Country"), Home: col(r, idx, "Home"), Away: col(r, idx, "Away")}
		for name, i := range idx {
			if !strings.HasPrefix(name, "GD") || i >= len(r) {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(r[i]), 64)
			if err != nil {
				continue
			}
			sp := strings.TrimPrefix(name, "GD")
			switch {
			case sp == "<-5":
				f.PAway += v
			case sp == ">5":
				f.PHome += v
			case strings.HasPrefix(sp, "="):
				n, err := strconv.Atoi(strings.TrimPrefix(sp, "="))
				if err != nil {
					continue
				}
				switch {
				case n > 0:
					f.PHome += v
				case n < 0:
					f.PAway += v
				default:
					f.PDraw += v
				}
			}
		}
		t := f.PHome + f.PDraw + f.PAway
		if t <= 0 || f.Home == "" || f.Away == "" {
			continue
		}
		f.PHome, f.PDraw, f.PAway = f.PHome/t, f.PDraw/t, f.PAway/t
		out = append(out, f)
	}
	return out, nil
}

// EloFormula turns two ClubElo ratings into 1X2 probabilities: the expected
// score We = 1/(10^(−(dr+HFA)/400)+1) (Elo; ClubElo's home advantage is
// about 60 points on average), with a draw share that shrinks as the match
// gets more one-sided (≈29% for equal teams), so that pH + pD/2 = We.
func EloFormula(home, away float64, neutral bool) P3 {
	hfa := 60.0
	if neutral {
		hfa = 0
	}
	we := 1 / (math.Pow(10, -(home-away+hfa)/400) + 1)
	d := 0.29 - 0.24*math.Pow(2*we-1, 2)
	d = math.Max(0.08, math.Min(0.32, d))
	ph := we - d/2
	pa := 1 - we - d/2
	if ph < 0.01 {
		ph = 0.01
	}
	if pa < 0.01 {
		pa = 0.01
	}
	t := ph + d + pa
	return P3{ph / t, d / t, pa / t}
}

// fetchClubElo stores the day's ratings (one request) and maps clubs.
func (s *Service) fetchClubEloRatings(ctx context.Context, day time.Time, prio int, task string) (int, error) {
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderClubElo, Path: "/" + day.Format("2006-01-02"), Prio: prio, Task: task, Accept: "text/csv"})
	if err != nil {
		return 0, err
	}
	rows, err := ParseClubEloRatings(out.Body)
	if err != nil {
		return 0, err
	}
	s.setResults(ProviderClubElo, task, len(rows))
	now := s.Now()
	d := day.Truncate(24 * time.Hour)
	recs := make([]ClubEloRating, 0, len(rows))
	for _, r := range rows {
		recs = append(recs, ClubEloRating{Club: clip(r.Club, 80), Date: d, Country: r.Country, Level: r.Level, Elo: r.Elo, Rank: r.Rank, FetchedAt: now})
	}
	for i := 0; i < len(recs); i += 500 {
		j := minInt(i+500, len(recs))
		if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(recs[i:j]).Error; err != nil {
			return 0, err
		}
	}
	s.mapClubEloClubs(d)
	s.linkClubEloFixtures()
	// Keep 14 days of ratings.
	s.DB.Where("date < ?", d.AddDate(0, 0, -14)).Delete(&ClubEloRating{})
	return len(recs), nil
}

func (s *Service) fetchClubEloFixtures(ctx context.Context, prio int, task string) (int, error) {
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderClubElo, Path: "/Fixtures", Prio: prio, Task: task, Accept: "text/csv"})
	if err != nil {
		return 0, err
	}
	fs, err := ParseClubEloFixtures(out.Body)
	if err != nil {
		return 0, err
	}
	s.setResults(ProviderClubElo, task, len(fs))
	now := s.Now()
	clubTeam := s.clubEloTeamMap()
	for _, f := range fs {
		row := ClubEloFixture{Date: f.Date, Home: clip(f.Home, 80), Away: clip(f.Away, 80), Country: f.Country, PHome: round4(f.PHome), PDraw: round4(f.PDraw),
			PAway: round4(f.PAway), FetchedAt: now}
		h, a := clubTeam[f.Home], clubTeam[f.Away]
		if h != 0 && a != 0 {
			var fid int64
			s.DB.Model(&Fixture{}).Where("home_id = ? AND away_id = ? AND kickoff BETWEEN ? AND ?", h, a, f.Date.Add(-12*time.Hour), f.Date.Add(36*time.Hour)).
				Limit(1).Pluck("id", &fid)
			row.FixtureID = fid
		}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row)
	}
	s.DB.Where("date < ?", now.AddDate(0, 0, -3)).Delete(&ClubEloFixture{})
	return len(fs), nil
}

// linkClubEloFixtures links ClubElo fixture rows to fixtures once both
// clubs are mapped (also after a new alias).
func (s *Service) linkClubEloFixtures() {
	var rows []ClubEloFixture
	s.DB.Where("fixture_id = 0 AND date >= ?", s.Now().AddDate(0, 0, -2)).Find(&rows)
	if len(rows) == 0 {
		return
	}
	m := s.clubEloTeamMap()
	for _, r := range rows {
		h, a := m[r.Home], m[r.Away]
		if h == 0 || a == 0 {
			continue
		}
		var fid int64
		s.DB.Model(&Fixture{}).Where("home_id = ? AND away_id = ? AND kickoff BETWEEN ? AND ?", h, a, r.Date.Add(-12*time.Hour), r.Date.Add(36*time.Hour)).Limit(1).Pluck("id", &fid)
		if fid != 0 {
			s.DB.Model(&ClubEloFixture{}).Where("date = ? AND home = ? AND away = ?", r.Date, r.Home, r.Away).Update("fixture_id", fid)
		}
	}
}

// clubEloTeamMap: ClubElo club → team id (latest mapping).
func (s *Service) clubEloTeamMap() map[string]int64 {
	var rows []struct {
		Club   string
		TeamID int64
	}
	s.DB.Raw(`SELECT DISTINCT ON (club) club, team_id FROM fb_clubelo_ratings WHERE team_id <> 0 ORDER BY club, date DESC`).Scan(&rows)
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Club] = r.TeamID
	}
	return out
}

// mapClubEloClubs maps the day's clubs to teams of competitions in the same
// country (alias first, then name similarity ≥ 0.8 with a unique best).
func (s *Service) mapClubEloClubs(day time.Time) {
	var clubs []ClubEloRating
	s.DB.Where("date = ?", day).Find(&clubs)
	if len(clubs) == 0 {
		return
	}
	// Teams per ClubElo country code (teams that played in a competition of that country).
	var trs []struct {
		ID      int64
		Name    string
		Country string
	}
	s.DB.Raw(`SELECT DISTINCT t.id, t.name, COALESCE(NULLIF(t.country,''), c.country) AS country FROM fb_teams t
		JOIN fb_fixtures f ON f.home_id = t.id OR f.away_id = t.id JOIN fb_competitions c ON c.id = f.competition_id
		WHERE c.type = 'League' OR t.country <> ''`).Scan(&trs)
	byCountry := map[string][]struct {
		ID   int64
		Name string
	}{}
	for _, t := range trs {
		code := ClubEloCountry[t.Country]
		if code == "" {
			continue
		}
		byCountry[code] = append(byCountry[code], struct {
			ID   int64
			Name string
		}{t.ID, t.Name})
	}
	aliases := s.aliasMap()
	used := map[int64]string{}
	for _, c := range clubs {
		if c.TeamID != 0 {
			used[c.TeamID] = c.Club
		}
	}
	for _, c := range clubs {
		if c.TeamID != 0 {
			continue
		}
		var team int64
		for _, k := range []string{normFree(c.Club), NormTeam(c.Club)} {
			if t, ok := aliases[k]; ok && k != "" {
				team = t
				break
			}
		}
		cands := byCountry[c.Country]
		if team == 0 && len(cands) > 0 {
			type sc struct {
				id   int64
				name string
				v    float64
			}
			best := []sc{}
			for _, t := range cands {
				v := TeamSim(c.Club, t.Name)
				if v >= 0.6 {
					best = append(best, sc{t.ID, t.Name, v})
				}
			}
			sort.Slice(best, func(i, j int) bool { return best[i].v > best[j].v })
			switch {
			case len(best) > 0 && best[0].v >= 0.8 && (len(best) == 1 || best[1].v < best[0].v-0.05):
				team = best[0].id
				s.learnAliasFrom(c.Club, team, ProviderClubElo)
			case len(best) > 0:
				s.noteUnmatched(ProviderClubElo, c.Club, c.Country, best[0].name, best[0].id)
			}
		}
		if team != 0 {
			if other, ok := used[team]; ok && other != c.Club {
				continue // two clubs on one team: leave for a manual alias
			}
			used[team] = c.Club
			s.DB.Model(&ClubEloRating{}).Where("club = ? AND team_id = 0", c.Club).Update("team_id", team)
		}
	}
}

// noteUnmatched records a provider's team name nobody could map.
func (s *Service) noteUnmatched(provider, name, country, guess string, guessID int64) {
	now := s.Now()
	u := UnmatchedName{Provider: provider, Name: clip(name, 160), Country: clip(country, 40), Seen: 1, LastSeen: now, BestGuess: clip(guess, 200), BestTeamID: guessID}
	s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "provider"}, {Name: "name"}},
		DoUpdates: clause.Assignments(map[string]any{"seen": gormExpr("fb_unmatched_names.seen + 1"), "last_seen": now, "best_guess": u.BestGuess, "best_team_id": guessID})}).Create(&u)
}

// ClubEloSide is one club's rating.
type ClubEloSide struct {
	Club    string  `json:"club"`
	Elo     float64 `json:"elo"`
	Rank    *int    `json:"rank"`
	Country string  `json:"country"`
	Level   int     `json:"level"`
	Date    string  `json:"date"`
}

// ClubEloOut is the match page's ClubElo section.
type ClubEloOut struct {
	Available   bool               `json:"available"`
	Home        *ClubEloSide       `json:"home"`
	Away        *ClubEloSide       `json:"away"`
	Probs       map[string]float64 `json:"probs"`
	ProbsSource string             `json:"probs_source"`
	EloDiff     *float64           `json:"elo_diff"`
	FetchedAt   *time.Time         `json:"fetched_at"`
	URL         string             `json:"url"`
	Note        string             `json:"note"`
}

func (s *Service) clubEloSide(team int64) (*ClubEloSide, *time.Time) {
	var r ClubEloRating
	if s.DB.Where("team_id = ? AND date <= ?", team, s.Now()).Order("date DESC").Limit(1).Find(&r).RowsAffected == 0 {
		return nil, nil
	}
	t := r.FetchedAt
	return &ClubEloSide{Club: r.Club, Elo: math.Round(r.Elo*10) / 10, Rank: r.Rank, Country: r.Country, Level: r.Level, Date: r.Date.Format("2006-01-02")}, &t
}

// clubEloFor builds the ClubElo section of a fixture.
func (s *Service) clubEloFor(f Fixture) ClubEloOut {
	out := ClubEloOut{Note: "Independent free model from clubelo.com (daily ratings; fixture probabilities from its own simulation when listed). Shown next to DASYIN's model, not blended into it."}
	h, ht := s.clubEloSide(f.HomeID)
	a, at := s.clubEloSide(f.AwayID)
	out.Home, out.Away = h, a
	out.FetchedAt = latestTime(ht, at)
	if h == nil || a == nil {
		out.Note = "ClubElo has no rating mapped for " + map[bool]string{true: s.teamName(f.AwayID), false: s.teamName(f.HomeID)}[h != nil] +
			" (it covers European top divisions and some second tiers; a super admin can add an alias)."
		return out
	}
	out.Available = true
	out.URL = "http://clubelo.com/" + strings.ReplaceAll(h.Club, " ", "")
	d := math.Round((h.Elo-a.Elo)*10) / 10
	out.EloDiff = &d
	var cf ClubEloFixture
	if s.DB.Where("(fixture_id = ? OR (home = ? AND away = ? AND date BETWEEN ? AND ?))", f.ID, h.Club, a.Club,
		f.Kickoff.Add(-36*time.Hour).Truncate(24*time.Hour), f.Kickoff.Add(36*time.Hour)).Order("fetched_at DESC").Limit(1).Find(&cf).RowsAffected > 0 {
		out.Probs = map[string]float64{"home": cf.PHome, "draw": cf.PDraw, "away": cf.PAway}
		out.ProbsSource = "clubelo_fixtures"
		t := cf.FetchedAt
		out.FetchedAt = latestTime(out.FetchedAt, &t)
		return out
	}
	p := EloFormula(h.Elo, a.Elo, false)
	out.Probs = map[string]float64{"home": round4(p[0]), "draw": round4(p[1]), "away": round4(p[2])}
	out.ProbsSource = "elo_formula"
	return out
}
