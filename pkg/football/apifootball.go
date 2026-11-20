package football

// API-Football v3 (api-sports.io) client and response shapes.
//
// Docs: https://www.api-football.com/documentation-v3
//   - Base URL https://v3.football.api-sports.io, GET only, header
//     x-apisports-key (no other headers needed).
//   - Every response is {get, parameters, errors, results, paging{current,total}, response}.
//     errors is [] on success or an object such as {"token": "..."},
//     {"requests": "You have reached the request limit for the day..."},
//     {"rateLimit": "Too many requests..."} or {"plan": "Free plans do not have access to this season..."}.
//   - Headers: x-ratelimit-requests-limit / x-ratelimit-requests-remaining
//     (daily quota) and X-RateLimit-Limit / X-RateLimit-Remaining (per minute).
//   - GET /status does not count towards the quota.
//   - Update frequencies (docs, "How to get started", 2026):
//     https://www.api-football.com/news/post/how-to-get-started-with-api-football-the-complete-beginners-guide
//     live fixtures & events every 15 s, statistics / fixture players every
//     minute in play, standings and predictions hourly, pre-match odds every
//     3 hours, injuries every 4 hours, line-ups 20–40 minutes before kickoff.
//
// Endpoints used: /status, /leagues, /teams, /fixtures (date, league+season,
// ids — up to 20 fixtures with events, lineups, statistics and players
// embedded — and live), /fixtures/headtohead, /fixtures/statistics (incl.
// "expected_goals" where covered), /fixtures/events, /fixtures/lineups,
// /fixtures/players, /standings, /injuries, /players, /players/topscorers,
// /predictions (stored as a reference only), /odds (pre-match, paginated),
// /odds/live, /odds/bookmakers.

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// afEnvelope is the common response wrapper.
type afEnvelope struct {
	Get      string                       `json:"get"`
	Errors   json.RawMessage              `json:"errors"`
	Results  int                          `json:"results"`
	Paging   struct{ Current, Total int } `json:"paging"`
	Response json.RawMessage              `json:"response"`
}

// afErrors reads the errors member ([] or {key: message}).
func afErrors(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 || raw[0] != '{' {
		return out
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	for k, v := range m {
		out[k] = strings.TrimSpace(strings.TrimSpace(asString(v)))
	}
	return out
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// ---------------------------------------------------------------- shapes

type afStatus struct {
	Account struct {
		Firstname string `json:"firstname"`
		Lastname  string `json:"lastname"`
		Email     string `json:"email"`
	} `json:"account"`
	Subscription struct {
		Plan   string `json:"plan"`
		End    string `json:"end"`
		Active bool   `json:"active"`
	} `json:"subscription"`
	Requests struct {
		Current  int `json:"current"`
		LimitDay int `json:"limit_day"`
	} `json:"requests"`
}

type afLeagueEntry struct {
	League struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
		Logo string `json:"logo"`
	} `json:"league"`
	Country struct {
		Name string  `json:"name"`
		Code *string `json:"code"`
		Flag *string `json:"flag"`
	} `json:"country"`
	Seasons []struct {
		Year     int    `json:"year"`
		Start    string `json:"start"`
		End      string `json:"end"`
		Current  bool   `json:"current"`
		Coverage struct {
			Fixtures struct {
				Events            bool `json:"events"`
				Lineups           bool `json:"lineups"`
				StatisticsFixture bool `json:"statistics_fixtures"`
				StatisticsPlayers bool `json:"statistics_players"`
			} `json:"fixtures"`
			Standings   bool `json:"standings"`
			Players     bool `json:"players"`
			TopScorers  bool `json:"top_scorers"`
			TopAssists  bool `json:"top_assists"`
			TopCards    bool `json:"top_cards"`
			Injuries    bool `json:"injuries"`
			Predictions bool `json:"predictions"`
			Odds        bool `json:"odds"`
		} `json:"coverage"`
	} `json:"seasons"`
}

type afTeamEntry struct {
	Team struct {
		ID       int64   `json:"id"`
		Name     string  `json:"name"`
		Code     *string `json:"code"`
		Country  string  `json:"country"`
		Founded  *int    `json:"founded"`
		National bool    `json:"national"`
		Logo     string  `json:"logo"`
	} `json:"team"`
	Venue struct {
		Name *string `json:"name"`
		City *string `json:"city"`
	} `json:"venue"`
}

type afTeamRef struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Logo   string `json:"logo"`
	Winner *bool  `json:"winner"`
}

type afPair struct {
	Home *int `json:"home"`
	Away *int `json:"away"`
}

type afFixtureEntry struct {
	Fixture struct {
		ID        int64   `json:"id"`
		Referee   *string `json:"referee"`
		Timezone  string  `json:"timezone"`
		Date      string  `json:"date"`
		Timestamp int64   `json:"timestamp"`
		Venue     struct {
			ID   *int64  `json:"id"`
			Name *string `json:"name"`
			City *string `json:"city"`
		} `json:"venue"`
		Status struct {
			Long    string `json:"long"`
			Short   string `json:"short"`
			Elapsed *int   `json:"elapsed"`
			Extra   *int   `json:"extra"`
		} `json:"status"`
	} `json:"fixture"`
	League struct {
		ID      int64   `json:"id"`
		Name    string  `json:"name"`
		Country string  `json:"country"`
		Logo    string  `json:"logo"`
		Flag    *string `json:"flag"`
		Season  int     `json:"season"`
		Round   string  `json:"round"`
	} `json:"league"`
	Teams struct {
		Home afTeamRef `json:"home"`
		Away afTeamRef `json:"away"`
	} `json:"teams"`
	Goals afPair `json:"goals"`
	Score struct {
		Halftime  afPair `json:"halftime"`
		Fulltime  afPair `json:"fulltime"`
		Extratime afPair `json:"extratime"`
		Penalty   afPair `json:"penalty"`
	} `json:"score"`
	// Embedded with fixtures?id= / ids= (absent otherwise).
	Events     []afEvent             `json:"events"`
	Lineups    []afLineup            `json:"lineups"`
	Statistics []afTeamStats         `json:"statistics"`
	Players    []afFixturePlayerTeam `json:"players"`
}

type afEvent struct {
	Time struct {
		Elapsed int  `json:"elapsed"`
		Extra   *int `json:"extra"`
	} `json:"time"`
	Team struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Player struct {
		ID   *int64  `json:"id"`
		Name *string `json:"name"`
	} `json:"player"`
	Assist struct {
		ID   *int64  `json:"id"`
		Name *string `json:"name"`
	} `json:"assist"`
	Type     string  `json:"type"`
	Detail   string  `json:"detail"`
	Comments *string `json:"comments"`
}

type afLineupPlayer struct {
	Player struct {
		ID     int64   `json:"id"`
		Name   string  `json:"name"`
		Number *int    `json:"number"`
		Pos    *string `json:"pos"`
		Grid   *string `json:"grid"`
	} `json:"player"`
}

type afLineup struct {
	Team struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Formation   *string          `json:"formation"`
	StartXI     []afLineupPlayer `json:"startXI"`
	Substitutes []afLineupPlayer `json:"substitutes"`
	Coach       struct {
		ID   *int64  `json:"id"`
		Name *string `json:"name"`
	} `json:"coach"`
}

type afTeamStats struct {
	Team struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Statistics []struct {
		Type  string `json:"type"`
		Value any    `json:"value"`
	} `json:"statistics"`
}

type afPlayerGameStats struct {
	Games struct {
		Minutes    *int    `json:"minutes"`
		Number     *int    `json:"number"`
		Position   *string `json:"position"`
		Rating     any     `json:"rating"`
		Captain    bool    `json:"captain"`
		Substitute bool    `json:"substitute"`
	} `json:"games"`
	Shots struct {
		Total *int `json:"total"`
		On    *int `json:"on"`
	} `json:"shots"`
	Goals struct {
		Total    *int `json:"total"`
		Conceded *int `json:"conceded"`
		Assists  *int `json:"assists"`
		Saves    *int `json:"saves"`
	} `json:"goals"`
	Passes struct {
		Total *int `json:"total"`
		Key   *int `json:"key"`
	} `json:"passes"`
	Cards struct {
		Yellow *int `json:"yellow"`
		Red    *int `json:"red"`
	} `json:"cards"`
}

type afFixturePlayerTeam struct {
	Team struct {
		ID int64 `json:"id"`
	} `json:"team"`
	Players []struct {
		Player struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Photo string `json:"photo"`
		} `json:"player"`
		Statistics []afPlayerGameStats `json:"statistics"`
	} `json:"players"`
}

type afStandingRecord struct {
	Played int `json:"played"`
	Win    int `json:"win"`
	Draw   int `json:"draw"`
	Lose   int `json:"lose"`
	Goals  struct {
		For     int `json:"for"`
		Against int `json:"against"`
	} `json:"goals"`
}

type afStandingRow struct {
	Rank int `json:"rank"`
	Team struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Logo string `json:"logo"`
	} `json:"team"`
	Points      int              `json:"points"`
	GoalsDiff   int              `json:"goalsDiff"`
	Group       string           `json:"group"`
	Form        *string          `json:"form"`
	Status      *string          `json:"status"`
	Description *string          `json:"description"`
	All         afStandingRecord `json:"all"`
	Home        afStandingRecord `json:"home"`
	Away        afStandingRecord `json:"away"`
	Update      string           `json:"update"`
}

type afStandingsEntry struct {
	League struct {
		ID        int64             `json:"id"`
		Name      string            `json:"name"`
		Country   string            `json:"country"`
		Season    int               `json:"season"`
		Standings [][]afStandingRow `json:"standings"`
	} `json:"league"`
}

type afInjury struct {
	Player struct {
		ID     int64   `json:"id"`
		Name   string  `json:"name"`
		Photo  string  `json:"photo"`
		Type   string  `json:"type"`
		Reason *string `json:"reason"`
	} `json:"player"`
	Team struct {
		ID int64 `json:"id"`
	} `json:"team"`
	Fixture struct {
		ID   int64  `json:"id"`
		Date string `json:"date"`
	} `json:"fixture"`
	League struct {
		ID     int64 `json:"id"`
		Season int   `json:"season"`
	} `json:"league"`
}

type afPlayerEntry struct {
	Player struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Firstname   string `json:"firstname"`
		Lastname    string `json:"lastname"`
		Age         *int   `json:"age"`
		Nationality string `json:"nationality"`
		Injured     bool   `json:"injured"`
		Photo       string `json:"photo"`
	} `json:"player"`
	Statistics []struct {
		Team struct {
			ID int64 `json:"id"`
		} `json:"team"`
		League struct {
			ID     int64 `json:"id"`
			Season int   `json:"season"`
		} `json:"league"`
		Games struct {
			Appearences *int    `json:"appearences"`
			Lineups     *int    `json:"lineups"`
			Minutes     *int    `json:"minutes"`
			Position    *string `json:"position"`
			Rating      any     `json:"rating"`
		} `json:"games"`
		Shots struct {
			Total *int `json:"total"`
			On    *int `json:"on"`
		} `json:"shots"`
		Goals struct {
			Total   *int `json:"total"`
			Assists *int `json:"assists"`
		} `json:"goals"`
		Passes struct {
			Key *int `json:"key"`
		} `json:"passes"`
		Cards struct {
			Yellow *int `json:"yellow"`
			Red    *int `json:"red"`
		} `json:"cards"`
		Penalty struct {
			Scored *int `json:"scored"`
		} `json:"penalty"`
	} `json:"statistics"`
}

type afPrediction struct {
	Predictions struct {
		Winner *struct {
			ID      *int64  `json:"id"`
			Name    *string `json:"name"`
			Comment *string `json:"comment"`
		} `json:"winner"`
		UnderOver *string `json:"under_over"`
		Advice    *string `json:"advice"`
		Percent   struct {
			Home string `json:"home"`
			Draw string `json:"draw"`
			Away string `json:"away"`
		} `json:"percent"`
	} `json:"predictions"`
}

type afOddValue struct {
	Value     any     `json:"value"`
	Odd       any     `json:"odd"`
	Handicap  *string `json:"handicap"`
	Main      *bool   `json:"main"`
	Suspended bool    `json:"suspended"`
}

type afOddsEntry struct {
	Fixture struct {
		ID int64 `json:"id"`
	} `json:"fixture"`
	Update     string `json:"update"`
	Bookmakers []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Bets []struct {
			ID     int64        `json:"id"`
			Name   string       `json:"name"`
			Values []afOddValue `json:"values"`
		} `json:"bets"`
	} `json:"bookmakers"`
}

// afLiveOdds is one fixture of GET /odds/live (no bookmakers: API-Football's
// own in-play prices).
type afLiveOdds struct {
	Fixture struct {
		ID     int64 `json:"id"`
		Status struct {
			Long    string `json:"long"`
			Elapsed *int   `json:"elapsed"`
		} `json:"status"`
	} `json:"fixture"`
	Status struct {
		Stopped  bool `json:"stopped"`
		Blocked  bool `json:"blocked"`
		Finished bool `json:"finished"`
	} `json:"status"`
	Update string `json:"update"`
	Odds   []struct {
		ID     int64        `json:"id"`
		Name   string       `json:"name"`
		Values []afOddValue `json:"values"`
	} `json:"odds"`
}

// ---------------------------------------------------------------- helpers

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, f := range []string{time.RFC3339, "2006-01-02T15:04:05-07:00", "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func strp(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func intv(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// statNumber reads a statistic value: 12, "55%", "1.42", null.
func statNumber(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case string:
		s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "%"))
		if s == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return &f
		}
	}
	return nil
}

func statInt(v any) *int {
	if f := statNumber(v); f != nil {
		i := int(*f + 0.5)
		return &i
	}
	return nil
}

// parsePct reads "45%" → 0.45.
func parsePct(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
	if err != nil {
		return 0
	}
	return f / 100
}

// Final and live statuses (https://www.api-football.com/documentation-v3#tag/Fixtures).
var (
	finalStatuses   = map[string]bool{"FT": true, "AET": true, "PEN": true}
	voidStatuses    = map[string]bool{"CANC": true, "ABD": true, "AWD": true, "WO": true}
	liveStatuses    = map[string]bool{"1H": true, "HT": true, "2H": true, "ET": true, "BT": true, "P": true, "LIVE": true, "SUSP": true, "INT": true}
	notStartedStats = map[string]bool{"NS": true, "TBD": true, "PST": true}
)

// IsFinal: the result is known.
func IsFinal(status string) bool { return finalStatuses[status] }

// IsLive: in play.
func IsLive(status string) bool { return liveStatuses[status] }

// fixtureRow converts a fixtures response entry.
func fixtureRow(e afFixtureEntry, now time.Time) Fixture {
	ko, ok := parseTime(e.Fixture.Date)
	if !ok && e.Fixture.Timestamp > 0 {
		ko = time.Unix(e.Fixture.Timestamp, 0).UTC()
	}
	f := Fixture{ID: e.Fixture.ID, CompetitionID: e.League.ID, Season: e.League.Season, Round: e.League.Round, Kickoff: ko,
		Status: e.Fixture.Status.Short, StatusLong: e.Fixture.Status.Long, Elapsed: e.Fixture.Status.Elapsed, ElapsedExtra: e.Fixture.Status.Extra,
		HomeID: e.Teams.Home.ID, AwayID: e.Teams.Away.ID, HomeGoals: e.Goals.Home, AwayGoals: e.Goals.Away,
		HTHome: e.Score.Halftime.Home, HTAway: e.Score.Halftime.Away, FTHome: e.Score.Fulltime.Home, FTAway: e.Score.Fulltime.Away,
		ETHome: e.Score.Extratime.Home, ETAway: e.Score.Extratime.Away, PenHome: e.Score.Penalty.Home, PenAway: e.Score.Penalty.Away,
		Venue: strp(e.Fixture.Venue.Name), City: strp(e.Fixture.Venue.City), Referee: strp(e.Fixture.Referee), FetchedAt: now, UpdatedAt: now}
	// Some competitions only fill goals: use them as the 90-minute score
	// when the match ended in normal time.
	if f.Status == "FT" && f.FTHome == nil && f.HomeGoals != nil {
		f.FTHome, f.FTAway = f.HomeGoals, f.AwayGoals
	}
	return f
}

// statRow converts one team's statistics.
func statRow(fixture int64, s afTeamStats, now time.Time) FixtureStat {
	r := FixtureStat{FixtureID: fixture, TeamID: s.Team.ID, FetchedAt: now}
	for _, st := range s.Statistics {
		switch strings.ToLower(strings.TrimSpace(st.Type)) {
		case "shots on goal":
			r.ShotsOn = statInt(st.Value)
		case "shots off goal":
			r.ShotsOff = statInt(st.Value)
		case "total shots":
			r.ShotsTotal = statInt(st.Value)
		case "blocked shots":
			r.ShotsBlocked = statInt(st.Value)
		case "shots insidebox":
			r.ShotsInside = statInt(st.Value)
		case "shots outsidebox":
			r.ShotsOutside = statInt(st.Value)
		case "fouls":
			r.Fouls = statInt(st.Value)
		case "corner kicks":
			r.Corners = statInt(st.Value)
		case "offsides":
			r.Offsides = statInt(st.Value)
		case "ball possession":
			r.Possession = statNumber(st.Value)
		case "yellow cards":
			r.Yellow = statInt(st.Value)
		case "red cards":
			r.Red = statInt(st.Value)
		case "goalkeeper saves":
			r.Saves = statInt(st.Value)
		case "total passes":
			r.Passes = statInt(st.Value)
		case "passes accurate":
			r.PassesAccurate = statInt(st.Value)
		case "passes %":
			r.PassPct = statNumber(st.Value)
		case "expected_goals", "expected goals":
			r.XG = statNumber(st.Value)
		}
	}
	return r
}

func ratingOf(v any) *float64 {
	f := statNumber(v)
	if f == nil || *f <= 0 {
		return nil
	}
	return f
}

// injuryStatus maps API-Football's injury type/reason to
// injured | suspended | doubtful.
func injuryStatus(typ, reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "suspend") || strings.Contains(r, "red card") || strings.Contains(r, "yellow card"):
		return "suspended"
	case strings.EqualFold(strings.TrimSpace(typ), "Questionable"):
		return "doubtful"
	}
	return "injured"
}

// positionName maps line-up letters to the season position names.
func positionName(p string) string {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "G":
		return "Goalkeeper"
	case "D":
		return "Defender"
	case "M":
		return "Midfielder"
	case "F":
		return "Attacker"
	}
	return p
}
