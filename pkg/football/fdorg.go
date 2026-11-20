package football

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// football-data.org v4 (https://docs.football-data.org/general/v4/index.html):
// header X-Auth-Token; the free tier covers 12 competitions with 10 calls a
// minute (scores delayed). Used only as a cross-check source:
//   GET /v4/competitions/{code}/standings
//   GET /v4/competitions/{code}/matches?dateFrom=YYYY-MM-DD&dateTo=YYYY-MM-DD

// FDOrgCompetitions maps football-data.org codes (free tier) to API-Football league ids.
var FDOrgCompetitions = map[string]int64{"PL": 39, "ELC": 40, "BL1": 78, "PD": 140, "SA": 135, "FL1": 61, "DED": 88, "PPL": 94, "CL": 2, "BSA": 71, "WC": 1, "EC": 4}

type fdoTeam struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ShortName string `json:"shortName"`
	TLA       string `json:"tla"`
}

type fdoMatch struct {
	ID       int64   `json:"id"`
	UTCDate  string  `json:"utcDate"`
	Status   string  `json:"status"`
	Matchday int     `json:"matchday"`
	HomeTeam fdoTeam `json:"homeTeam"`
	AwayTeam fdoTeam `json:"awayTeam"`
	Score    struct {
		Winner   *string `json:"winner"`
		Duration string  `json:"duration"`
		FullTime struct {
			Home *int `json:"home"`
			Away *int `json:"away"`
		} `json:"fullTime"`
		RegularTime *struct {
			Home *int `json:"home"`
			Away *int `json:"away"`
		} `json:"regularTime"`
	} `json:"score"`
}

type fdoMatches struct {
	Competition struct {
		Code string `json:"code"`
	} `json:"competition"`
	Matches []fdoMatch `json:"matches"`
}

type fdoStandings struct {
	Competition struct {
		Code string `json:"code"`
	} `json:"competition"`
	Standings []struct {
		Stage string `json:"stage"`
		Type  string `json:"type"`
		Table []struct {
			Position       int     `json:"position"`
			Team           fdoTeam `json:"team"`
			PlayedGames    int     `json:"playedGames"`
			Form           *string `json:"form"`
			Won            int     `json:"won"`
			Draw           int     `json:"draw"`
			Lost           int     `json:"lost"`
			Points         int     `json:"points"`
			GoalsFor       int     `json:"goalsFor"`
			GoalsAgainst   int     `json:"goalsAgainst"`
			GoalDifference int     `json:"goalDifference"`
		} `json:"table"`
	} `json:"standings"`
}

// ParseFDOrgMatches reads /competitions/{code}/matches.
func ParseFDOrgMatches(body []byte) (string, []fdoMatch, error) {
	var m fdoMatches
	if err := json.Unmarshal(body, &m); err != nil {
		return "", nil, fmt.Errorf("unexpected football-data.org response: %w", err)
	}
	return m.Competition.Code, m.Matches, nil
}

// ParseFDOrgStandings reads /competitions/{code}/standings (TOTAL table).
func ParseFDOrgStandings(body []byte) (string, []FDOrgStanding, error) {
	var m fdoStandings
	if err := json.Unmarshal(body, &m); err != nil {
		return "", nil, fmt.Errorf("unexpected football-data.org response: %w", err)
	}
	out := []FDOrgStanding{}
	for _, st := range m.Standings {
		if st.Type != "" && st.Type != "TOTAL" {
			continue
		}
		for _, r := range st.Table {
			form := ""
			if r.Form != nil {
				form = strings.ReplaceAll(*r.Form, ",", "")
			}
			out = append(out, FDOrgStanding{Competition: m.Competition.Code, FDTeamID: r.Team.ID, Team: r.Team.Name, Position: r.Position, Played: r.PlayedGames,
				Won: r.Won, Draw: r.Draw, Lost: r.Lost, Points: r.Points, GF: r.GoalsFor, GA: r.GoalsAgainst, Form: clip(form, 20)})
		}
	}
	return m.Competition.Code, out, nil
}

func (s *Service) fetchFDOrgStandings(ctx context.Context, code string, comp int64, prio int, task string) (int, error) {
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderFDOrg, Path: "/v4/competitions/" + code + "/standings", Prio: prio, Task: task})
	if err != nil {
		return 0, err
	}
	_, rows, err := ParseFDOrgStandings(out.Body)
	if err != nil {
		return 0, err
	}
	s.setResults(ProviderFDOrg, task, len(rows))
	names := []string{}
	for _, r := range rows {
		names = append(names, r.Team)
	}
	ids := s.mapNames(ProviderFDOrg, names, comp, "")
	now := s.Now()
	s.DB.Where("competition = ?", code).Delete(&FDOrgStanding{})
	for _, r := range rows {
		r.Competition, r.TeamID, r.FetchedAt = code, ids[r.Team], now
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&r)
	}
	return len(rows), nil
}

func (s *Service) fetchFDOrgMatches(ctx context.Context, code string, comp int64, from, to time.Time, prio int, task string) (int, error) {
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderFDOrg, Path: "/v4/competitions/" + code + "/matches", Prio: prio, Task: task,
		Params: url.Values{"dateFrom": {from.Format("2006-01-02")}, "dateTo": {to.Format("2006-01-02")}}})
	if err != nil {
		return 0, err
	}
	_, ms, err := ParseFDOrgMatches(out.Body)
	if err != nil {
		return 0, err
	}
	s.setResults(ProviderFDOrg, task, len(ms))
	names := []string{}
	for _, m := range ms {
		names = append(names, m.HomeTeam.Name, m.AwayTeam.Name)
	}
	ids := s.mapNames(ProviderFDOrg, names, comp, "")
	now := s.Now()
	for _, m := range ms {
		t, ok := parseTime(m.UTCDate)
		if !ok {
			continue
		}
		row := FDOrgMatch{ID: m.ID, Competition: code, UTCDate: t, Status: m.Status, Matchday: m.Matchday, Home: clip(m.HomeTeam.Name, 120), Away: clip(m.AwayTeam.Name, 120),
			HomeFDID: m.HomeTeam.ID, AwayFDID: m.AwayTeam.ID, HomeID: ids[m.HomeTeam.Name], AwayID: ids[m.AwayTeam.Name], FetchedAt: now}
		ft := m.Score.FullTime
		if m.Score.RegularTime != nil && m.Score.RegularTime.Home != nil {
			ft.Home, ft.Away = m.Score.RegularTime.Home, m.Score.RegularTime.Away
		}
		row.HG, row.AG = ft.Home, ft.Away
		if row.HomeID != 0 && row.AwayID != 0 {
			var fid int64
			s.DB.Model(&Fixture{}).Where("home_id = ? AND away_id = ? AND id > 0 AND kickoff BETWEEN ? AND ?", row.HomeID, row.AwayID, t.Add(-36*time.Hour), t.Add(36*time.Hour)).Limit(1).Pluck("id", &fid)
			row.FixtureID = fid
		}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row)
	}
	return len(ms), nil
}

// fdOrgCodeOf returns the football-data.org code of a competition ("" = not covered).
func fdOrgCodeOf(comp int64) string {
	for k, v := range FDOrgCompetitions {
		if v == comp {
			return k
		}
	}
	return ""
}
