package football

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Every store function is idempotent: the same provider response stored
// twice leaves the same rows (upserts on the provider ids; per-fixture
// child rows are replaced as a set).

// upsert updates every column on conflict (primary key); upsertCols only
// the given ones (conflict on "id").
func upsert() clause.OnConflict { return clause.OnConflict{UpdateAll: true} }

func upsertCols(cols ...string) clause.OnConflict {
	return clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns(cols)}
}

// storeLeagues updates the competition list from GET /leagues: names,
// logos, the current season and its coverage flags. Leagues not seen before
// are added switched off (the super admin's picker lists them).
func (s *Service) storeLeagues(entries []afLeagueEntry) (int, error) {
	now := s.Now()
	byID := map[int64]afLeagueEntry{}
	rows := []Competition{}
	for _, e := range entries {
		if e.League.ID == 0 {
			continue
		}
		byID[e.League.ID] = e
		c := Competition{ID: e.League.ID, Name: e.League.Name, Country: e.Country.Name, CountryCode: strp(e.Country.Code), Type: e.League.Type,
			Logo: e.League.Logo, Flag: strp(e.Country.Flag), Known: true, FetchedAt: &now, UpdatedAt: now, Priority: 50,
			OddsSportKey: KnownSportKeys[e.League.ID], Group: "other"}
		// Current season, else the latest.
		for _, se := range e.Seasons {
			if se.Current || se.Year > c.Season && !anyCurrent(e) {
				c.Season = se.Year
				if t, ok := parseTime(se.Start); ok {
					c.SeasonStart = &t
				}
				if t, ok := parseTime(se.End); ok {
					c.SeasonEnd = &t
				}
				cv := se.Coverage
				c.Coverage = Coverage{Events: cv.Fixtures.Events, Lineups: cv.Fixtures.Lineups, StatisticsFixture: cv.Fixtures.StatisticsFixture,
					StatisticsPlayers: cv.Fixtures.StatisticsPlayers, Standings: cv.Standings, Players: cv.Players, TopScorers: cv.TopScorers,
					Injuries: cv.Injuries, Predictions: cv.Predictions, Odds: cv.Odds}
			}
		}
		rows = append(rows, c)
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		for i := 0; i < len(rows); i += 200 {
			j := i + 200
			if j > len(rows) {
				j = len(rows)
			}
			if err := tx.Clauses(upsertCols("name", "country", "country_code", "type", "logo", "flag", "season", "season_start", "season_end",
				"coverage", "known", "fetched_at", "updated_at")).Create(rows[i:j]).Error; err != nil {
				return err
			}
		}
		// Check the default competitions' ids against the provider's list.
		for _, d := range DefaultCompetitions {
			e, ok := byID[d.ID]
			if ok && sameLeague(e, d) {
				tx.Model(&Competition{}).Where("id = ?", d.ID).Update("note", "")
				continue
			}
			var found *afLeagueEntry
			for _, x := range entries {
				x := x
				if sameLeague(x, d) {
					found = &x
					break
				}
			}
			note := fmt.Sprintf("API-Football does not list league %d as %s (%s).", d.ID, d.Name, d.Country)
			if found != nil {
				note += fmt.Sprintf(" It is league %d there: that one is used instead.", found.League.ID)
				var was Competition
				if tx.Where("id = ?", d.ID).Limit(1).Find(&was).RowsAffected > 0 && was.Enabled {
					tx.Model(&Competition{}).Where("id = ?", found.League.ID).Updates(map[string]any{"enabled": true, "priority": d.Priority,
						"odds_sport_key": d.SportKey, "grp": d.Group})
					tx.Model(&Competition{}).Where("id = ?", d.ID).Updates(map[string]any{"enabled": false, "note": note})
				}
			} else {
				tx.Model(&Competition{}).Where("id = ?", d.ID).Update("note", note)
			}
		}
		return nil
	})
	return len(rows), err
}

func anyCurrent(e afLeagueEntry) bool {
	for _, se := range e.Seasons {
		if se.Current {
			return true
		}
	}
	return false
}

func normName(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("-", " ", ".", " ", "  ", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

func sameLeague(e afLeagueEntry, d DefaultComp) bool {
	if !strings.EqualFold(strings.TrimSpace(e.Country.Name), d.Country) {
		return false
	}
	a, b := normName(e.League.Name), normName(d.Name)
	return a == b || strings.Contains(a, b) || strings.Contains(b, a)
}

// ensureTeams makes sure team rows exist (from fixtures: name and logo).
func (s *Service) ensureTeams(tx *gorm.DB, ts map[int64]Team) error {
	if len(ts) == 0 {
		return nil
	}
	rows := make([]Team, 0, len(ts))
	for _, t := range ts {
		rows = append(rows, t)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return tx.Clauses(upsertCols("name", "logo", "fetched_at")).Create(&rows).Error
}

// storeTeams stores GET /teams entries (venue, country, code).
func (s *Service) storeTeams(entries []afTeamEntry) error {
	now := s.Now()
	rows := []Team{}
	for _, e := range entries {
		f := 0
		if e.Team.Founded != nil {
			f = *e.Team.Founded
		}
		rows = append(rows, Team{ID: e.Team.ID, Name: e.Team.Name, Code: strp(e.Team.Code), Country: e.Team.Country, Logo: e.Team.Logo,
			National: e.Team.National, Founded: f, Venue: strp(e.Venue.Name), City: strp(e.Venue.City), FetchedAt: now})
	}
	if len(rows) == 0 {
		return nil
	}
	return s.DB.Clauses(upsert()).Create(&rows).Error
}

// fixtureCols are updated on every fixture upsert (fetch markers, the Odds
// API event id and the reference prediction are kept).
var fixtureCols = []string{"competition_id", "season", "round", "kickoff", "status", "status_long", "elapsed", "elapsed_extra", "home_id", "away_id",
	"home_goals", "away_goals", "ht_home", "ht_away", "ft_home", "ft_away", "et_home", "et_away", "pen_home", "pen_away", "venue", "city",
	"referee", "fetched_at", "updated_at"}

// StoreResult counts what a store changed.
type StoreResult struct {
	Fixtures, Finished, Details, Lineups int
	NewlyFinal                           []int64
}

// storeFixtures upserts fixtures and any embedded events, line-ups,
// statistics and players (fixtures?id= / ids=).
func (s *Service) storeFixtures(entries []afFixtureEntry, details bool) (StoreResult, error) {
	var res StoreResult
	now := s.Now()
	if len(entries) == 0 {
		return res, nil
	}
	ids := []int64{}
	for _, e := range entries {
		ids = append(ids, e.Fixture.ID)
	}
	prev := map[int64]string{}
	var old []Fixture
	s.DB.Select("id, status").Where("id IN ?", ids).Find(&old)
	for _, f := range old {
		prev[f.ID] = f.Status
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		teams := map[int64]Team{}
		comps := map[int64]Competition{}
		rows := []Fixture{}
		for _, e := range entries {
			if e.Fixture.ID == 0 {
				continue
			}
			teams[e.Teams.Home.ID] = Team{ID: e.Teams.Home.ID, Name: e.Teams.Home.Name, Logo: e.Teams.Home.Logo, FetchedAt: now}
			teams[e.Teams.Away.ID] = Team{ID: e.Teams.Away.ID, Name: e.Teams.Away.Name, Logo: e.Teams.Away.Logo, FetchedAt: now}
			comps[e.League.ID] = Competition{ID: e.League.ID, Name: e.League.Name, Country: e.League.Country, Logo: e.League.Logo,
				Flag: strp(e.League.Flag), Season: e.League.Season, Priority: 50, Group: "other", OddsSportKey: KnownSportKeys[e.League.ID], UpdatedAt: now}
			rows = append(rows, fixtureRow(e, now))
		}
		if err := s.ensureTeams(tx, teams); err != nil {
			return err
		}
		for _, c := range comps {
			c := c
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error; err != nil {
				return err
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		if err := tx.Clauses(upsertCols(fixtureCols...)).Create(&rows).Error; err != nil {
			return err
		}
		res.Fixtures = len(rows)
		for _, f := range rows {
			if IsFinal(f.Status) {
				res.Finished++
				if !IsFinal(prev[f.ID]) {
					res.NewlyFinal = append(res.NewlyFinal, f.ID)
					tx.Model(&Fixture{}).Where("id = ? AND finished_at IS NULL", f.ID).Update("finished_at", now)
				}
			}
		}
		if !details {
			return nil
		}
		for _, e := range entries {
			fid := e.Fixture.ID
			if len(e.Statistics) > 0 {
				if err := storeStats(tx, fid, e.Statistics, now); err != nil {
					return err
				}
			}
			if err := storeEvents(tx, fid, e.Events, now); err != nil {
				return err
			}
			if len(e.Lineups) > 0 {
				if err := storeLineups(tx, fid, e.Lineups, now); err != nil {
					return err
				}
				res.Lineups++
			}
			if len(e.Players) > 0 {
				if err := storeFixturePlayers(tx, fid, e.Players, now); err != nil {
					return err
				}
			}
			up := map[string]any{"details_at": now}
			if len(e.Lineups) > 0 {
				up["lineups_at"] = now
			}
			tx.Model(&Fixture{}).Where("id = ?", fid).Updates(up)
			res.Details++
		}
		return nil
	})
	return res, err
}

func storeStats(tx *gorm.DB, fid int64, st []afTeamStats, now time.Time) error {
	rows := []FixtureStat{}
	for _, t := range st {
		if t.Team.ID != 0 {
			rows = append(rows, statRow(fid, t, now))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(upsert()).Create(&rows).Error
}

func storeEvents(tx *gorm.DB, fid int64, evs []afEvent, now time.Time) error {
	if evs == nil {
		return nil
	}
	if err := tx.Where("fixture_id = ?", fid).Delete(&FixtureEvent{}).Error; err != nil {
		return err
	}
	rows := []FixtureEvent{}
	for i, e := range evs {
		r := FixtureEvent{FixtureID: fid, Seq: i + 1, Minute: e.Time.Elapsed, Extra: e.Time.Extra, TeamID: e.Team.ID,
			PlayerName: strp(e.Player.Name), AssistName: strp(e.Assist.Name), Type: e.Type, Detail: e.Detail, Comments: clip(strp(e.Comments), 160), FetchedAt: now}
		if e.Player.ID != nil {
			r.PlayerID = *e.Player.ID
		}
		if e.Assist.ID != nil {
			r.AssistID = *e.Assist.ID
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Create(&rows).Error
}

func lineupPlayers(ps []afLineupPlayer) []LineupPlayer {
	out := []LineupPlayer{}
	for _, p := range ps {
		out = append(out, LineupPlayer{ID: p.Player.ID, Name: p.Player.Name, Number: intv(p.Player.Number), Pos: strp(p.Player.Pos), Grid: strp(p.Player.Grid)})
	}
	return out
}

func storeLineups(tx *gorm.DB, fid int64, ls []afLineup, now time.Time) error {
	rows := []Lineup{}
	for _, l := range ls {
		if l.Team.ID == 0 {
			continue
		}
		rows = append(rows, Lineup{FixtureID: fid, TeamID: l.Team.ID, Formation: strp(l.Formation), Coach: strp(l.Coach.Name),
			StartXI: lineupPlayers(l.StartXI), Substitutes: lineupPlayers(l.Substitutes), FetchedAt: now})
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(upsert()).Create(&rows).Error
}

func storeFixturePlayers(tx *gorm.DB, fid int64, teams []afFixturePlayerTeam, now time.Time) error {
	rows := []FixturePlayer{}
	players := []Player{}
	for _, t := range teams {
		for _, p := range t.Players {
			if p.Player.ID == 0 || len(p.Statistics) == 0 {
				continue
			}
			st := p.Statistics[0]
			rows = append(rows, FixturePlayer{FixtureID: fid, PlayerID: p.Player.ID, TeamID: t.Team.ID, Name: p.Player.Name,
				Minutes: intv(st.Games.Minutes), Rating: ratingOf(st.Games.Rating), Position: strp(st.Games.Position), Substitute: st.Games.Substitute,
				Captain: st.Games.Captain, Goals: intv(st.Goals.Total), Assists: intv(st.Goals.Assists), Shots: intv(st.Shots.Total), ShotsOn: intv(st.Shots.On),
				KeyPasses: intv(st.Passes.Key), Yellow: intv(st.Cards.Yellow), Red: intv(st.Cards.Red), FetchedAt: now})
			players = append(players, Player{ID: p.Player.ID, Name: p.Player.Name, Photo: p.Player.Photo, FetchedAt: now})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&players).Error; err != nil {
		return err
	}
	return tx.Clauses(upsert()).Create(&rows).Error
}

// storeFixtureDetailParts stores the per-endpoint responses
// (fixtures/statistics, /events, /lineups, /players).
func (s *Service) storeFixtureDetailParts(fid int64, st []afTeamStats, ev []afEvent, ln []afLineup, pl []afFixturePlayerTeam) error {
	now := s.Now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if st != nil {
			if err := storeStats(tx, fid, st, now); err != nil {
				return err
			}
		}
		if ev != nil {
			if err := storeEvents(tx, fid, ev, now); err != nil {
				return err
			}
		}
		if len(ln) > 0 {
			if err := storeLineups(tx, fid, ln, now); err != nil {
				return err
			}
			tx.Model(&Fixture{}).Where("id = ?", fid).Update("lineups_at", now)
		}
		if pl != nil {
			if err := storeFixturePlayers(tx, fid, pl, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// storeStandings stores a table snapshot dated today (one snapshot per
// competition, season and day; a later fetch the same day replaces it).
func (s *Service) storeStandings(entries []afStandingsEntry) (int, error) {
	now := s.Now()
	day := now.Truncate(24 * time.Hour)
	n := 0
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		teams := map[int64]Team{}
		for _, e := range entries {
			rows := []Standing{}
			for _, group := range e.League.Standings {
				for _, r := range group {
					teams[r.Team.ID] = Team{ID: r.Team.ID, Name: r.Team.Name, Logo: r.Team.Logo, FetchedAt: now}
					gname := r.Group
					if gname == "" {
						gname = e.League.Name
					}
					rows = append(rows, Standing{CompetitionID: e.League.ID, Season: e.League.Season, AsOf: day, GroupName: clip(gname, 80), TeamID: r.Team.ID,
						Rank: r.Rank, Points: r.Points, Played: r.All.Played, Won: r.All.Win, Drawn: r.All.Draw, Lost: r.All.Lose, GF: r.All.Goals.For,
						GA: r.All.Goals.Against, GD: r.GoalsDiff, Form: clip(strp(r.Form), 10), Description: clip(strp(r.Description), 160),
						Home:   Record{r.Home.Played, r.Home.Win, r.Home.Draw, r.Home.Lose, r.Home.Goals.For, r.Home.Goals.Against},
						Away:   Record{r.Away.Played, r.Away.Win, r.Away.Draw, r.Away.Lose, r.Away.Goals.For, r.Away.Goals.Against},
						Source: ProviderAPIFootball, FetchedAt: now})
				}
			}
			if len(rows) == 0 {
				continue
			}
			if err := s.ensureTeams(tx, teams); err != nil {
				return err
			}
			if err := tx.Where("competition_id = ? AND season = ? AND as_of = ?", e.League.ID, e.League.Season, day).Delete(&Standing{}).Error; err != nil {
				return err
			}
			if err := tx.Create(&rows).Error; err != nil {
				return err
			}
			n += len(rows)
		}
		return nil
	})
	return n, err
}

// storeInjuries replaces the injury list of the fixtures in the response
// (and of fixtures asked for that came back empty).
func (s *Service) storeInjuries(list []afInjury, fixtures []int64) (int, error) {
	now := s.Now()
	byFix := map[int64][]Injury{}
	for _, f := range fixtures {
		byFix[f] = []Injury{}
	}
	for _, i := range list {
		if i.Fixture.ID == 0 || i.Player.ID == 0 {
			continue
		}
		reason := strp(i.Player.Reason)
		byFix[i.Fixture.ID] = append(byFix[i.Fixture.ID], Injury{FixtureID: i.Fixture.ID, PlayerID: i.Player.ID, TeamID: i.Team.ID,
			CompetitionID: i.League.ID, Season: i.League.Season, PlayerName: clip(i.Player.Name, 120), Type: clip(i.Player.Type, 40),
			Reason: clip(reason, 120), Status: injuryStatus(i.Player.Type, reason), FetchedAt: now})
	}
	n := 0
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		for fid, rows := range byFix {
			if err := tx.Where("fixture_id = ?", fid).Delete(&Injury{}).Error; err != nil {
				return err
			}
			dedup := map[int64]Injury{}
			for _, r := range rows {
				dedup[r.PlayerID] = r
			}
			rows = rows[:0]
			for _, r := range dedup {
				rows = append(rows, r)
			}
			if len(rows) > 0 {
				if err := tx.Create(&rows).Error; err != nil {
					return err
				}
				n += len(rows)
			}
			tx.Model(&Fixture{}).Where("id = ?", fid).Update("injuries_at", now)
		}
		return nil
	})
	return n, err
}

// storePlayers stores GET /players and /players/topscorers entries.
func (s *Service) storePlayers(entries []afPlayerEntry) (int, error) {
	now := s.Now()
	players := map[int64]Player{}
	seasons := map[string]PlayerSeason{}
	for _, e := range entries {
		if e.Player.ID == 0 {
			continue
		}
		players[e.Player.ID] = Player{ID: e.Player.ID, Name: e.Player.Name, Firstname: clip(e.Player.Firstname, 80), Lastname: clip(e.Player.Lastname, 80),
			Age: intv(e.Player.Age), Nationality: e.Player.Nationality, Photo: e.Player.Photo, Injured: e.Player.Injured, FetchedAt: now}
		for _, st := range e.Statistics {
			if st.Team.ID == 0 || st.League.ID == 0 {
				continue
			}
			k := fmt.Sprintf("%d-%d-%d-%d", e.Player.ID, st.Team.ID, st.League.ID, st.League.Season)
			seasons[k] = PlayerSeason{PlayerID: e.Player.ID, TeamID: st.Team.ID, CompetitionID: st.League.ID, Season: st.League.Season,
				Position: strp(st.Games.Position), Appearances: intv(st.Games.Appearences), Lineups: intv(st.Games.Lineups), Minutes: intv(st.Games.Minutes),
				Rating: ratingOf(st.Games.Rating), Goals: intv(st.Goals.Total), Assists: intv(st.Goals.Assists), Shots: intv(st.Shots.Total),
				ShotsOn: intv(st.Shots.On), KeyPasses: intv(st.Passes.Key), Yellow: intv(st.Cards.Yellow), Red: intv(st.Cards.Red),
				PenScored: intv(st.Penalty.Scored), FetchedAt: now}
		}
	}
	if len(players) == 0 {
		return 0, nil
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		pr := make([]Player, 0, len(players))
		for _, p := range players {
			pr = append(pr, p)
		}
		sort.Slice(pr, func(i, j int) bool { return pr[i].ID < pr[j].ID })
		if err := tx.Clauses(upsert()).Create(&pr).Error; err != nil {
			return err
		}
		if len(seasons) == 0 {
			return nil
		}
		sr := make([]PlayerSeason, 0, len(seasons))
		for _, p := range seasons {
			sr = append(sr, p)
		}
		sort.Slice(sr, func(i, j int) bool {
			return sr[i].PlayerID < sr[j].PlayerID || sr[i].PlayerID == sr[j].PlayerID && sr[i].TeamID < sr[j].TeamID
		})
		return tx.Clauses(upsert()).Create(&sr).Error
	})
	return len(players), err
}

// storePredictionRef keeps API-Football's prediction as a reference.
func (s *Service) storePredictionRef(fid int64, ps []afPrediction) error {
	now := s.Now()
	if len(ps) == 0 {
		return s.DB.Model(&Fixture{}).Where("id = ?", fid).Update("pred_ref_at", now).Error
	}
	p := ps[0].Predictions
	ref := &RefPrediction{Home: parsePct(p.Percent.Home), Draw: parsePct(p.Percent.Draw), Away: parsePct(p.Percent.Away),
		Advice: clip(strp(p.Advice), 200), UnderOver: strp(p.UnderOver), FetchedAt: now}
	if p.Winner != nil {
		ref.Winner = strp(p.Winner.Name)
	}
	b, _ := json.Marshal(ref)
	return s.DB.Model(&Fixture{}).Where("id = ?", fid).Updates(map[string]any{"prediction_ref": string(b), "pred_ref_at": now}).Error
}

// storeH2H stores the head-to-head fixtures and remembers the pair.
func (s *Service) storeH2H(a, b int64, entries []afFixtureEntry) error {
	if _, err := s.storeFixtures(entries, false); err != nil {
		return err
	}
	if a > b {
		a, b = b, a
	}
	ids := []int64{}
	for _, e := range entries {
		ids = append(ids, e.Fixture.ID)
	}
	return s.DB.Clauses(upsert()).Create(&H2H{TeamA: a, TeamB: b, FixtureIDs: ids, FetchedAt: s.Now()}).Error
}

// oddKey identifies a price line for change detection.
func oddKey(o Odd) string {
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s|%t", o.FixtureID, o.Source, o.Bookmaker, o.Market, o.Selection, lineKey(o.Line), o.Live)
}

// storeOdds writes prices: a new row when the price moved, otherwise the
// last row's checked_at is updated (so history stays small but complete).
func (s *Service) storeOdds(rows []Odd) (inserted, unchanged int, err error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	fids := map[int64]bool{}
	for _, r := range rows {
		fids[r.FixtureID] = true
	}
	ids := []int64{}
	for f := range fids {
		ids = append(ids, f)
	}
	var existing []Odd
	s.DB.Raw(`SELECT DISTINCT ON (fixture_id, source, bookmaker, market, selection, COALESCE(line, -999), live) *
		FROM fb_odds WHERE fixture_id IN ? ORDER BY fixture_id, source, bookmaker, market, selection, COALESCE(line, -999), live, fetched_at DESC, id DESC`, ids).Scan(&existing)
	last := map[string]Odd{}
	for _, o := range existing {
		last[oddKey(o)] = o
	}
	seen := map[string]bool{}
	ins := []Odd{}
	touch := []int64{}
	now := s.Now()
	for _, r := range rows {
		k := oddKey(r)
		if seen[k] {
			continue
		}
		seen[k] = true
		if o, ok := last[k]; ok && math.Abs(o.Price-r.Price) < 1e-9 {
			touch = append(touch, o.ID)
			continue
		}
		ins = append(ins, r)
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if len(touch) > 0 {
			if err := tx.Model(&Odd{}).Where("id IN ?", touch).Update("checked_at", now).Error; err != nil {
				return err
			}
		}
		for i := 0; i < len(ins); i += 500 {
			j := i + 500
			if j > len(ins) {
				j = len(ins)
			}
			if err := tx.Create(ins[i:j]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return len(ins), len(touch), err
}
