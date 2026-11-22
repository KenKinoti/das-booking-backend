package football

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Jackpot analyser: per organisation and person (nobody else sees a
// person's jackpots). Probabilities per game blend the jackpot's own odds
// (de-margined) with the sharp/consensus market, ClubElo and DASYIN's model
// where those exist; see jackpot_math.go for the maths.

// JackpotDisclaimer is shown with every analysis.
const JackpotDisclaimer = "18+ only. Probabilities are estimates from bookmaker prices and ratings, never certainties — no ticket is a sure win, " +
	"and almost every jackpot ticket wins nothing. DASYIN does not sell tickets or take bets. Set limits and stop when it isn't fun."

// JackpotBookmakers are suggested in the form.
var JackpotBookmakers = []string{"SportPesa", "Betika", "Odibets", "Mozzartbet", "Betway", "Shabiki", "SportyBet", "Betin"}

// GameMatch is how a jackpot game was matched to a fixture.
type GameMatch struct {
	Status        string           `json:"status"` // matched | suggested | unmatched | manual
	FixtureID     *int64           `json:"fixture_id"`
	Confidence    float64          `json:"confidence"`
	Home          string           `json:"home"`
	Away          string           `json:"away"`
	Kickoff       *time.Time       `json:"kickoff"`
	Competition   string           `json:"competition"`
	CompetitionID int64            `json:"competition_id"`
	Via           string           `json:"via"` // alias | name | manual
	Candidates    []MatchCandidate `json:"candidates"`
}

// MatchCandidate is a fixture a game may be.
type MatchCandidate struct {
	FixtureID     int64     `json:"fixture_id"`
	Home          string    `json:"home"`
	Away          string    `json:"away"`
	Kickoff       time.Time `json:"kickoff"`
	Competition   string    `json:"competition"`
	CompetitionID int64     `json:"competition_id"`
	Confidence    float64   `json:"confidence"`
	homeID        int64
	awayID        int64
}

// Grading is a jackpot's results and how each ticket did.
type Grading struct {
	GradedAt          time.Time     `json:"graded_at"`
	Complete          bool          `json:"complete"`
	Results           []*string     `json:"results"`
	Void              []bool        `json:"void"`
	CorrectMostLikely *int          `json:"correct_most_likely"`
	ExpectedCorrect   *float64      `json:"expected_correct"` // of the most likely ticket (sum of its picks\' probabilities)
	Tickets           []TicketGrade `json:"tickets"`
	BestTopN          *struct {
		Rank    int `json:"rank"`
		Correct int `json:"correct"`
	} `json:"best_top_n"`
}

// TicketGrade is one ticket's grade.
type TicketGrade struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Picks      []string   `json:"picks"`
	Correct    int        `json:"correct"`
	Of         int        `json:"of"`
	Bonus      *BonusTier `json:"bonus"`
	WonJackpot bool       `json:"won_jackpot"`
}

// GameInput is a game typed in or picked from fixtures.
type GameInput struct {
	Home      string     `json:"home"`
	Away      string     `json:"away"`
	Odds      []float64  `json:"odds"`
	Kickoff   *time.Time `json:"kickoff"`
	FixtureID *int64     `json:"fixture_id"`
	Raw       string     `json:"raw"`
}

// JackpotInput creates or changes a jackpot (nil = unchanged on update).
type JackpotInput struct {
	Name        *string            `json:"name"`
	Bookmaker   *string            `json:"bookmaker"`
	Prize       *float64           `json:"prize"`
	Currency    *string            `json:"currency"`
	TicketPrice *float64           `json:"ticket_price"`
	Bonuses     []BonusTier        `json:"bonuses"`
	ClosesAt    *time.Time         `json:"closes_at"`
	Status      *string            `json:"status"`
	Text        *string            `json:"text"`
	Date        string             `json:"date"`
	Games       []GameInput        `json:"games"`
	Weights     map[string]float64 `json:"weights"`
	Sharing     *SharingOpts       `json:"sharing"`
	TopN        *int               `json:"top_n"`
	Notes       *string            `json:"notes"`
}

// ---------------------------------------------------------------- views

// GameView is a jackpot game in a payload.
type GameView struct {
	N            int                 `json:"n"`
	Home         string              `json:"home"`
	Away         string              `json:"away"`
	Raw          string              `json:"raw"`
	Odds         map[string]*float64 `json:"odds"` // null when not typed yet
	Kickoff      *time.Time          `json:"kickoff"`
	FixtureID    *int64              `json:"fixture_id"`
	Match        GameMatch           `json:"match"`
	Result       *string             `json:"result"`
	ResultSource *string             `json:"result_source"`
	Void         bool                `json:"void"`
	Score        *string             `json:"score"`
	ResultCheck  *JackpotCheck       `json:"result_check"` // v2.18: ✓/✗ against the frozen "Most likely" ticket
}

// TicketView is a saved ticket.
type TicketView struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Source       string     `json:"source"`
	Picks        []string   `json:"picks"`
	Combinations int        `json:"combinations"`
	Cost         float64    `json:"cost"`
	PAll         float64    `json:"p_all"`
	OneIn        float64    `json:"one_in"`
	Correct      *int       `json:"correct"`
	Of           int        `json:"of"`
	Bonus        *BonusTier `json:"bonus"`
	WonJackpot   *bool      `json:"won_jackpot"`
	CreatedAt    time.Time  `json:"created_at"`
}

// JackpotView is a jackpot with its games, tickets, analysis and grading.
type JackpotView struct {
	Jackpot  Jackpot          `json:"jackpot"`
	Games    []GameView       `json:"games"`
	Tickets  []TicketView     `json:"tickets"`
	Analysis *JackpotAnalysis `json:"analysis"`
	Grading  *Grading         `json:"grading"`
	Frozen   *JackpotFrozen   `json:"frozen"` // v2.18 (jackpot_win.go): chances stored at close
	Review   *JackpotReview   `json:"review"` // v2.18: plain-English review after grading
}

// SourceP is one source of a game's probabilities.
type SourceP struct {
	Key    string             `json:"key"`
	Label  string             `json:"label"`
	Probs  map[string]float64 `json:"probs"`
	Weight float64            `json:"weight"`
	Used   bool               `json:"used"`
	Detail string             `json:"detail,omitempty"`
	Why    string             `json:"why,omitempty"`
}

// Evidence is a note behind a game's probabilities. Status: team_news
// "confirmed" | "reported"; source_check "agree" | "minor" | "conflict".
type Evidence struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Ref    string `json:"ref,omitempty"`
	Status string `json:"status,omitempty"`
}

// GameSourceCheck is a matched game's source check (the match page's shape).
type GameSourceCheck struct {
	Status string   `json:"status"`
	Items  []SCItem `json:"items"`
}

// GameAnalysis is one game's analysis.
type GameAnalysis struct {
	N           int                 `json:"n"`
	Home        string              `json:"home"`
	Away        string              `json:"away"`
	Kickoff     *time.Time          `json:"kickoff"`
	FixtureID   *int64              `json:"fixture_id"`
	Odds        map[string]*float64 `json:"odds"` // null when the jackpot's odds aren't typed yet
	Margin      float64             `json:"margin"`
	Sources     []SourceP           `json:"sources"`
	SourceCheck *GameSourceCheck    `json:"source_check,omitempty"`
	Probs       map[string]float64  `json:"probs"`
	Pick        string              `json:"pick"`
	PickP       float64             `json:"pick_p"`
	Second      string              `json:"second"`
	SecondP     float64             `json:"second_p"`
	Certainty   float64             `json:"certainty"`
	DrawLean    bool                `json:"draw_lean"`
	Public      map[string]float64  `json:"public"`
	Evidence    []Evidence          `json:"evidence"`
	Verdict     *Verdict            `json:"verdict,omitempty"` // v2.18: tier, draw alert, pick or draw
	p           P3
}

// PickChange is a game whose pick differs from the most likely ticket.
type PickChange struct {
	N    int    `json:"n"`
	From string `json:"from"`
	To   string `json:"to"`
}

// TicketOut is a ranked ticket.
type TicketOut struct {
	Rank    int          `json:"rank"`
	Picks   []string     `json:"picks"`
	P       float64      `json:"p"`
	OneIn   float64      `json:"one_in"`
	CumP    float64      `json:"cum_p"`
	Draws   int          `json:"draws"`
	Changes []PickChange `json:"changes"`
}

// CoverOut is a cover step with its cost.
type CoverOut struct {
	Step int `json:"step"`
	CoverStep
	Cost  float64 `json:"cost"`
	OneIn float64 `json:"one_in"`
}

// CoverPlan is the ticket after some cover steps.
type CoverPlan struct {
	Label        string   `json:"label"`
	Doubles      int      `json:"doubles"`
	Triples      int      `json:"triples"`
	Combinations int      `json:"combinations"`
	Cost         float64  `json:"cost"`
	PAll         float64  `json:"p_all"`
	OneIn        float64  `json:"one_in"`
	Picks        []string `json:"picks"`
	PerCombo     float64  `json:"per_combination_p"`
}

// KP is P(k).
type KP struct {
	K int     `json:"k"`
	P float64 `json:"p"`
}

// Variant is a contrarian ticket.
type Variant struct {
	Label     string       `json:"label"`
	Picks     []string     `json:"picks"`
	Changes   []PickChange `json:"changes"`
	P         float64      `json:"p"`
	OneIn     float64      `json:"one_in"`
	PRatio    float64      `json:"p_ratio"`
	CoWinners float64      `json:"co_winners_expected"`
	EVTotal   float64      `json:"ev_total"`
	Why       string       `json:"why"`
}

// JackpotAnalysis is the analyser's output.
type JackpotAnalysis struct {
	NGames          int                `json:"n_games"`
	CombinationsAll float64            `json:"combinations_full"`
	AsOf            time.Time          `json:"as_of"`
	Currency        string             `json:"currency"`
	Weights         map[string]float64 `json:"weights"`
	Games           []GameAnalysis     `json:"games"`
	MostLikely      struct {
		Picks           []string `json:"picks"`
		P               float64  `json:"p"`
		OneIn           float64  `json:"one_in"`
		ExpectedCorrect float64  `json:"expected_correct"`
		Draws           int      `json:"draws"`
	} `json:"most_likely"`
	TopTickets      []TicketOut `json:"top_tickets"`
	Cover           []CoverOut  `json:"cover"`
	CoverPlans      []CoverPlan `json:"cover_plans"`
	AtLeast         []KP        `json:"at_least"`
	CorrectDist     []KP        `json:"correct_dist"`
	ExpectedCorrect float64     `json:"expected_correct"`
	ExpectedDraws   float64     `json:"expected_draws"`
	DrawDist        []KP        `json:"draw_dist"`
	EV              EVResult    `json:"ev"`
	Contrarian      []Variant   `json:"contrarian"`
	Sharing         struct {
		SharingOpts
		Explain string `json:"explain"`
	} `json:"sharing"`
	Notes      []string `json:"notes"`
	Disclaimer string   `json:"disclaimer"`
}

// SummaryGrade is a list row's grading.
type SummaryGrade struct {
	CorrectMostLikely *int     `json:"correct_most_likely"`
	ExpectedCorrect   *float64 `json:"expected_correct"`
	BestTicketCorrect *int     `json:"best_ticket_correct"`
	Complete          bool     `json:"complete"`
	ReviewCorrect     *int     `json:"review_correct"`  // v2.18: the frozen "Most likely" ticket
	ReviewExpected    *float64 `json:"review_expected"` // v2.18
	SaferHits         *int     `json:"safer_hits"`      // v2.18: pick or draw on every game
}

// JackpotSummary is a list row.
type JackpotSummary struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Bookmaker    string     `json:"bookmaker"`
	Prize        float64    `json:"prize"`
	Currency     string     `json:"currency"`
	TicketPrice  float64    `json:"ticket_price"`
	ClosesAt     *time.Time `json:"closes_at"`
	Status       string     `json:"status"`
	NGames       int        `json:"n_games"`
	MatchedGames int        `json:"matched_games"`
	MostLikely   struct {
		Picks []string `json:"picks"`
		OneIn float64  `json:"one_in"`
	} `json:"most_likely"`
	Tickets   int           `json:"tickets"`
	Graded    *SummaryGrade `json:"graded"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// ---------------------------------------------------------------- matching

type fixCand struct {
	ID       int64
	Kickoff  time.Time
	CompID   int64
	HomeID   int64
	AwayID   int64
	Home     string
	Away     string
	CompName string
	Status   string
}

func (s *Service) fixtureCands(from, to time.Time) []fixCand {
	var rows []fixCand
	s.DB.Raw(`SELECT f.id, f.kickoff, f.competition_id AS comp_id, f.home_id, f.away_id, h.name AS home, a.name AS away, COALESCE(c.name,'') AS comp_name, f.status
		FROM fb_fixtures f JOIN fb_teams h ON h.id = f.home_id JOIN fb_teams a ON a.id = f.away_id LEFT JOIN fb_competitions c ON c.id = f.competition_id
		WHERE f.kickoff BETWEEN ? AND ? ORDER BY f.kickoff LIMIT 20000`, from, to).Scan(&rows)
	return rows
}

// MatchGame scores candidate fixtures for a pasted game (pure: aliases maps
// a folded name key to a team id).
func MatchGame(home, away string, kickoff *time.Time, cands []fixCand, aliases map[string]int64) GameMatch {
	gm := GameMatch{Status: "unmatched", Candidates: []MatchCandidate{}}
	side := func(name string, id int64, fixName string) (float64, bool) {
		for _, k := range []string{normFree(name), NormTeam(name)} {
			if t, ok := aliases[k]; ok && k != "" {
				if t == id {
					return 1, true
				}
				return 0, true
			}
		}
		return TeamSim(name, fixName), false
	}
	type scored struct {
		MatchCandidate
		alias bool
	}
	all := []scored{}
	for _, c := range cands {
		sh, ah := side(home, c.HomeID, c.Home)
		sa, aa := side(away, c.AwayID, c.Away)
		if sh < 0.55 || sa < 0.55 {
			continue
		}
		conf := (sh + sa) / 2
		if kickoff != nil {
			d := math.Abs(c.Kickoff.Sub(*kickoff).Minutes())
			switch {
			case d > 36*60:
				conf -= 0.3
			case d > 180:
				conf -= 0.15
			case d > 15:
				conf -= 0.03
			}
		}
		all = append(all, scored{MatchCandidate{FixtureID: c.ID, Home: c.Home, Away: c.Away, Kickoff: c.Kickoff, Competition: c.CompName, CompetitionID: c.CompID,
			Confidence: round4(math.Max(0, conf)), homeID: c.HomeID, awayID: c.AwayID}, ah && aa})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Confidence > all[j].Confidence })
	for i, c := range all {
		if i >= 5 {
			break
		}
		gm.Candidates = append(gm.Candidates, c.MatchCandidate)
	}
	if len(all) == 0 {
		return gm
	}
	b := all[0]
	gm.Confidence = b.Confidence
	ambiguous := len(all) > 1 && all[1].Confidence > b.Confidence-0.03 && all[1].FixtureID != b.FixtureID
	switch {
	case b.Confidence >= 0.80 && !ambiguous:
		id := b.FixtureID
		k := b.Kickoff
		gm.Status, gm.FixtureID, gm.Home, gm.Away, gm.Kickoff, gm.Competition, gm.CompetitionID = "matched", &id, b.Home, b.Away, &k, b.Competition, b.CompetitionID
		gm.Via = "name"
		if b.alias {
			gm.Via = "alias"
		}
	case b.Confidence >= 0.55:
		gm.Status = "suggested"
		gm.Via = "name"
	}
	return gm
}

// matchWindow is where a game's fixture is looked for.
func (s *Service) matchWindow(kickoff, closes *time.Time) (time.Time, time.Time) {
	now := s.Now()
	switch {
	case kickoff != nil:
		return kickoff.Add(-30 * time.Hour), kickoff.Add(30 * time.Hour)
	case closes != nil:
		return closes.Add(-24 * time.Hour), closes.Add(4 * 24 * time.Hour)
	}
	return now.Add(-6 * time.Hour), now.Add(8 * 24 * time.Hour)
}

func (s *Service) matchGames(games []JackpotGame, closes *time.Time) {
	if len(games) == 0 {
		return
	}
	aliases := s.aliasMap()
	cache := map[string][]fixCand{}
	for i := range games {
		g := &games[i]
		if g.Match.Status == "manual" {
			continue
		}
		from, to := s.matchWindow(g.Kickoff, closes)
		key := from.Format(time.RFC3339) + to.Format(time.RFC3339)
		if _, ok := cache[key]; !ok {
			cache[key] = s.fixtureCands(from, to)
		}
		m := MatchGame(g.Home, g.Away, g.Kickoff, cache[key], aliases)
		if g.FixtureID != nil && *g.FixtureID != 0 {
			// Chosen in the form: keep it as a manual match.
			if c, ok := s.candidateOf(*g.FixtureID); ok {
				id := c.FixtureID
				k := c.Kickoff
				m = GameMatch{Status: "manual", FixtureID: &id, Confidence: 1, Home: c.Home, Away: c.Away, Kickoff: &k, Competition: c.Competition,
					CompetitionID: c.CompetitionID, Via: "manual", Candidates: m.Candidates}
			}
		}
		g.Match = m
		g.FixtureID = m.FixtureID
		if m.Kickoff != nil && (m.Status == "matched" || m.Status == "manual") {
			k := *m.Kickoff
			g.Kickoff = &k
		}
	}
}

func (s *Service) candidateOf(fid int64) (MatchCandidate, bool) {
	var rows []fixCand
	s.DB.Raw(`SELECT f.id, f.kickoff, f.competition_id AS comp_id, f.home_id, f.away_id, h.name AS home, a.name AS away, COALESCE(c.name,'') AS comp_name, f.status
		FROM fb_fixtures f JOIN fb_teams h ON h.id = f.home_id JOIN fb_teams a ON a.id = f.away_id LEFT JOIN fb_competitions c ON c.id = f.competition_id
		WHERE f.id = ?`, fid).Scan(&rows)
	if len(rows) == 0 {
		return MatchCandidate{}, false
	}
	r := rows[0]
	return MatchCandidate{FixtureID: r.ID, Home: r.Home, Away: r.Away, Kickoff: r.Kickoff, Competition: r.CompName, CompetitionID: r.CompID, Confidence: 1, homeID: r.HomeID, awayID: r.AwayID}, true
}

// Candidates searches fixtures for manual matching.
func (s *Service) JackpotCandidates(home, away, q, date string, loc *time.Location) []MatchCandidate {
	from, to := s.Now().Add(-24*time.Hour), s.Now().Add(8*24*time.Hour)
	if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), loc); err == nil {
		from, to = d.Add(-12*time.Hour).UTC(), d.Add(36*time.Hour).UTC()
	}
	cands := s.fixtureCands(from, to)
	out := []MatchCandidate{}
	if strings.TrimSpace(home) != "" || strings.TrimSpace(away) != "" {
		for _, c := range cands {
			sh, sa := 1.0, 1.0
			if strings.TrimSpace(home) != "" {
				sh = TeamSim(home, c.Home)
			}
			if strings.TrimSpace(away) != "" {
				sa = TeamSim(away, c.Away)
			}
			if sh >= 0.45 && sa >= 0.45 {
				out = append(out, MatchCandidate{FixtureID: c.ID, Home: c.Home, Away: c.Away, Kickoff: c.Kickoff, Competition: c.CompName, CompetitionID: c.CompID, Confidence: round4((sh + sa) / 2)})
			}
		}
	} else if t := Fold(strings.TrimSpace(q)); t != "" {
		for _, c := range cands {
			if strings.Contains(Fold(c.Home+" "+c.Away+" "+c.CompName), t) {
				out = append(out, MatchCandidate{FixtureID: c.ID, Home: c.Home, Away: c.Away, Kickoff: c.Kickoff, Competition: c.CompName, CompetitionID: c.CompID, Confidence: 1})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

// ParseAndMatch parses pasted text and matches every game (nothing saved).
func (s *Service) ParseAndMatch(text, date string, loc *time.Location) ParseResult {
	base := s.Now()
	if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), loc); err == nil {
		base = d
	}
	res := ParseJackpotText(text, base, loc)
	games := make([]JackpotGame, len(res.Games))
	var closes *time.Time
	if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), loc); err == nil {
		u := d.UTC()
		closes = &u
	}
	for i, g := range res.Games {
		games[i] = JackpotGame{Home: g.Home, Away: g.Away, Kickoff: g.Kickoff}
	}
	s.matchGames(games, closes)
	for i := range res.Games {
		m := games[i].Match
		res.Games[i].Match = &m
	}
	return res
}

// ---------------------------------------------------------------- CRUD

func validGames(in []GameInput) error {
	if len(in) < 2 {
		return inputErr("A jackpot needs at least 2 games.")
	}
	if len(in) > 30 {
		return inputErr("At most 30 games in a jackpot.")
	}
	for i, g := range in {
		if strings.TrimSpace(g.Home) == "" || strings.TrimSpace(g.Away) == "" {
			return inputErr("Game %d needs a home and an away team.", i+1)
		}
		if len(g.Odds) == 0 && g.FixtureID != nil && *g.FixtureID > 0 {
			continue // added from the match centre: odds typed later
		}
		if len(g.Odds) != 3 {
			return inputErr("Game %d needs the three 1X2 odds.", i+1)
		}
		for _, o := range g.Odds {
			if o < 1.01 || o > 1000 || math.IsNaN(o) {
				return inputErr("Game %d: odds must be between 1.01 and 1000.", i+1)
			}
		}
	}
	return nil
}

func (s *Service) gamesFromInput(org string, in JackpotInput, loc *time.Location) ([]GameInput, []string, error) {
	notes := []string{}
	games := in.Games
	if in.Text != nil && strings.TrimSpace(*in.Text) != "" {
		base := s.Now()
		if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(in.Date), loc); err == nil {
			base = d
		} else if in.ClosesAt != nil {
			base = *in.ClosesAt
		}
		pr := ParseJackpotText(*in.Text, base, loc)
		games = nil
		for _, g := range pr.Games {
			games = append(games, GameInput{Home: g.Home, Away: g.Away, Odds: g.Odds, Kickoff: g.Kickoff, Raw: g.Raw})
		}
		for _, sk := range pr.Skipped {
			notes = append(notes, fmt.Sprintf("Line %d skipped (%s): %s", sk.Line, sk.Why, clip(sk.Raw, 80)))
		}
		notes = append(notes, pr.Warnings...)
	}
	if err := validGames(games); err != nil {
		return nil, notes, err
	}
	return games, notes, nil
}

func (s *Service) applyJackpotInput(j *Jackpot, in JackpotInput, creating bool) error {
	st := s.FreeSettingsGet()
	if in.Name != nil {
		j.Name = clip(*in.Name, 160)
	}
	if strings.TrimSpace(j.Name) == "" {
		j.Name = "Jackpot " + s.Now().Format("2 Jan")
	}
	if in.Bookmaker != nil {
		j.Bookmaker = clip(strings.TrimSpace(*in.Bookmaker), 80)
	}
	if in.Prize != nil {
		if *in.Prize < 0 || *in.Prize > 1e12 {
			return inputErr("The prize must be between 0 and 1,000,000,000,000.")
		}
		j.Prize = *in.Prize
	}
	if in.Currency != nil && len(strings.TrimSpace(*in.Currency)) == 3 {
		j.Currency = strings.ToUpper(strings.TrimSpace(*in.Currency))
	}
	if in.TicketPrice != nil {
		if *in.TicketPrice <= 0 || *in.TicketPrice > 1e7 {
			return inputErr("The ticket price must be above 0.")
		}
		j.TicketPrice = *in.TicketPrice
	}
	if creating && j.TicketPrice <= 0 {
		j.TicketPrice = 99
	}
	if in.Bonuses != nil {
		bs := []BonusTier{}
		for _, b := range in.Bonuses {
			if b.Correct <= 0 || b.Amount < 0 {
				return inputErr("Bonus tiers need a number of correct games and an amount.")
			}
			bs = append(bs, b)
		}
		j.Bonuses = bs
	}
	if j.Bonuses == nil {
		j.Bonuses = []BonusTier{}
	}
	if in.ClosesAt != nil {
		t := in.ClosesAt.UTC()
		j.ClosesAt = &t
	}
	if in.Status != nil {
		switch *in.Status {
		case "draft", "open", "closed":
			j.Status = *in.Status
		default:
			return inputErr("Status must be draft, open or closed.")
		}
	}
	if j.Status == "" {
		j.Status = "open"
	}
	if in.Weights != nil {
		w := map[string]float64{}
		tot := 0.0
		for _, k := range []string{"jackpot", "market", "clubelo", "model"} {
			v := in.Weights[k]
			if v < 0 || v > 10 {
				return inputErr("Weights must be between 0 and 10.")
			}
			w[k] = v
			tot += v
		}
		if tot <= 0 {
			return inputErr("At least one weight must be above 0.")
		}
		j.Weights = w
	}
	if len(j.Weights) == 0 {
		j.Weights = st.JackpotWeights
	}
	if in.Sharing != nil {
		sh := *in.Sharing
		if sh.Mode != "fixed" {
			sh.Mode = "model"
		}
		if sh.Mode == "fixed" && (sh.CoWinners == nil || *sh.CoWinners < 0 || *sh.CoWinners > 1e6) {
			return inputErr("With a fixed sharing assumption, give the expected number of other winners (0 or more).")
		}
		if sh.TicketsSold <= 0 {
			sh.TicketsSold = st.JackpotSharing.TicketsSold
		}
		if sh.Popularity <= 0 || sh.Popularity > 4 {
			sh.Popularity = st.JackpotSharing.Popularity
		}
		j.Sharing = sh
	}
	if j.Sharing.Mode == "" {
		j.Sharing = st.JackpotSharing
	}
	if in.TopN != nil {
		if *in.TopN < 1 || *in.TopN > 200 {
			return inputErr("Top tickets: 1 to 200.")
		}
		j.TopN = *in.TopN
	}
	if j.TopN <= 0 {
		j.TopN = 10
	}
	if in.Notes != nil {
		j.Notes = clip(*in.Notes, 4000)
	}
	return nil
}

func gameRows(j *Jackpot, gs []GameInput) []JackpotGame {
	out := make([]JackpotGame, len(gs))
	for i, g := range gs {
		raw := g.Raw
		o := [3]float64{}
		if len(g.Odds) == 3 {
			o = [3]float64{g.Odds[0], g.Odds[1], g.Odds[2]}
		}
		if raw == "" {
			raw = fmt.Sprintf("%s v %s", g.Home, g.Away)
			if len(g.Odds) == 3 {
				raw += fmt.Sprintf(" %.2f %.2f %.2f", g.Odds[0], g.Odds[1], g.Odds[2])
			}
		}
		out[i] = JackpotGame{JackpotID: j.ID, N: i + 1, OrganizationID: j.OrganizationID, Home: clip(g.Home, 160), Away: clip(g.Away, 160), Raw: clip(raw, 400),
			Odds1: o[0], OddsX: o[1], Odds2: o[2], Kickoff: g.Kickoff, FixtureID: g.FixtureID, Match: GameMatch{Candidates: []MatchCandidate{}}}
	}
	return out
}

// CreateJackpot saves a jackpot.
func (s *Service) CreateJackpot(org, user string, in JackpotInput) (*JackpotView, error) {
	loc := s.zoneOf(org)
	games, notes, err := s.gamesFromInput(org, in, loc)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	j := Jackpot{ID: uuid.NewString(), OrganizationID: org, UserID: user, Currency: s.currencyOf(org), CreatedAt: now, UpdatedAt: now}
	if err := s.applyJackpotInput(&j, in, true); err != nil {
		return nil, err
	}
	var n int64
	s.DB.Model(&Jackpot{}).Where("organization_id = ? AND user_id = ?", org, user).Count(&n)
	if n >= 500 {
		return nil, inputErr("You have 500 saved jackpots: delete old ones first.")
	}
	rows := gameRows(&j, games)
	s.matchGames(rows, j.ClosesAt)
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&j).Error; err != nil {
			return err
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		return nil, err
	}
	v, err := s.JackpotViewOf(org, user, j.ID, 0)
	if v != nil && v.Analysis != nil {
		v.Analysis.Notes = append(notes, v.Analysis.Notes...)
	}
	return v, err
}

// AnalyseJackpot analyses an unsaved jackpot (the AI tool and "preview").
func (s *Service) AnalyseJackpot(org string, in JackpotInput) (*JackpotView, error) {
	loc := s.zoneOf(org)
	games, notes, err := s.gamesFromInput(org, in, loc)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	j := Jackpot{OrganizationID: org, Currency: s.currencyOf(org), CreatedAt: now, UpdatedAt: now}
	if err := s.applyJackpotInput(&j, in, true); err != nil {
		return nil, err
	}
	rows := gameRows(&j, games)
	s.matchGames(rows, j.ClosesAt)
	a := s.analyseJackpot(j, rows, org)
	a.Notes = append(notes, a.Notes...)
	return &JackpotView{Jackpot: j, Games: gameViews(rows), Tickets: []TicketView{}, Analysis: a}, nil
}

func (s *Service) loadJackpot(org, user, id string) (Jackpot, []JackpotGame, error) {
	var j Jackpot
	if s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Limit(1).Find(&j).RowsAffected == 0 {
		return j, nil, ErrNotFound
	}
	var gs []JackpotGame
	s.DB.Where("jackpot_id = ?", j.ID).Order("n").Find(&gs)
	return j, gs, nil
}

// UpdateJackpot changes a jackpot (games replaced when games/text are given).
func (s *Service) UpdateJackpot(org, user, id string, in JackpotInput) (*JackpotView, error) {
	j, _, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	if err := s.applyJackpotInput(&j, in, false); err != nil {
		return nil, err
	}
	j.UpdatedAt = s.Now()
	replace := in.Games != nil || (in.Text != nil && strings.TrimSpace(*in.Text) != "")
	var rows []JackpotGame
	if replace {
		games, _, err := s.gamesFromInput(org, in, s.zoneOf(org))
		if err != nil {
			return nil, err
		}
		rows = gameRows(&j, games)
		s.matchGames(rows, j.ClosesAt)
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&j).Error; err != nil {
			return err
		}
		if replace {
			if err := tx.Where("jackpot_id = ?", j.ID).Delete(&JackpotGame{}).Error; err != nil {
				return err
			}
			return tx.Create(&rows).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.JackpotViewOf(org, user, j.ID, 0)
}

// DeleteJackpot removes a jackpot with its games and tickets.
func (s *Service) DeleteJackpot(org, user, id string) error {
	j, _, err := s.loadJackpot(org, user, id)
	if err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		tx.Where("jackpot_id = ?", j.ID).Delete(&JackpotTicket{})
		tx.Where("jackpot_id = ?", j.ID).Delete(&JackpotGame{})
		return tx.Where("id = ?", j.ID).Delete(&Jackpot{}).Error
	})
}

// DuplicateJackpot copies a jackpot (games and settings, no tickets or grading).
func (s *Service) DuplicateJackpot(org, user, id, name string) (*JackpotView, error) {
	j, gs, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	c := j
	c.ID, c.CreatedAt, c.UpdatedAt, c.Grading, c.GradedAt, c.Status = uuid.NewString(), now, now, nil, nil, "draft"
	c.Frozen = nil
	c.Name = clip(strings.TrimSpace(name), 160)
	if c.Name == "" {
		c.Name = clip(j.Name+" (copy)", 160)
	}
	rows := make([]JackpotGame, len(gs))
	for i, g := range gs {
		g.JackpotID, g.Result, g.ResultSource, g.Void, g.Score = c.ID, "", "", false, ""
		rows[i] = g
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&c).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			return tx.Create(&rows).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.JackpotViewOf(org, user, c.ID, 0)
}

// SetGameMatch sets (or clears) a game's fixture by hand and learns the
// spellings as aliases (provider "jackpot").
func (s *Service) SetGameMatch(org, user, id string, n int, fixtureID *int64) (*JackpotView, error) {
	j, gs, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	var g *JackpotGame
	for i := range gs {
		if gs[i].N == n {
			g = &gs[i]
		}
	}
	if g == nil {
		return nil, ErrNotFound
	}
	if fixtureID == nil || *fixtureID == 0 {
		g.FixtureID = nil
		g.Match = GameMatch{Status: "unmatched", Candidates: g.Match.Candidates, Via: "manual"}
	} else {
		c, ok := s.candidateOf(*fixtureID)
		if !ok {
			return nil, inputErr("Unknown fixture %d.", *fixtureID)
		}
		fid, k := c.FixtureID, c.Kickoff
		g.FixtureID = &fid
		g.Kickoff = &k
		g.Match = GameMatch{Status: "manual", FixtureID: &fid, Confidence: 1, Home: c.Home, Away: c.Away, Kickoff: &k, Competition: c.Competition,
			CompetitionID: c.CompetitionID, Via: "manual", Candidates: g.Match.Candidates}
		s.learnAliasFrom(g.Home, c.homeID, ProviderJackpotAlias)
		s.learnAliasFrom(g.Away, c.awayID, ProviderJackpotAlias)
	}
	if err := s.DB.Save(g).Error; err != nil {
		return nil, err
	}
	s.DB.Model(&Jackpot{}).Where("id = ?", j.ID).Update("updated_at", s.Now())
	return s.JackpotViewOf(org, user, j.ID, 0)
}

// ProviderJackpotAlias marks aliases learned from jackpot matching.
const ProviderJackpotAlias = "jackpot"

// learnAliasFrom stores a provider's spelling of a team (never overwriting a manual alias).
func (s *Service) learnAliasFrom(name string, team int64, provider string) {
	if team == 0 {
		return
	}
	for _, a := range []string{normFree(name), NormTeam(name)} {
		if a == "" {
			continue
		}
		var cur TeamAlias
		if s.DB.Where("alias = ?", a).Limit(1).Find(&cur).RowsAffected > 0 {
			if cur.Source == "manual" || cur.TeamID == team {
				continue
			}
		}
		row := TeamAlias{Alias: clip(a, 160), TeamID: team, Source: "auto", Name: clip(name, 160), Provider: provider, CreatedAt: s.Now()}
		s.DB.Save(&row)
	}
}

// SetGameResult sets a game's result by hand ("" clears it).
func (s *Service) SetGameResult(org, user, id string, n int, result *string, void bool) (*JackpotView, error) {
	j, gs, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range gs {
		if gs[i].N != n {
			continue
		}
		found = true
		g := &gs[i]
		g.Void = void
		switch {
		case result == nil || *result == "":
			g.Result, g.ResultSource = "", ""
		default:
			r := strings.ToUpper(strings.TrimSpace(*result))
			if r != "1" && r != "X" && r != "2" {
				return nil, inputErr("Result must be 1, X or 2.")
			}
			g.Result, g.ResultSource = r, "manual"
		}
		if void {
			g.ResultSource = "manual"
		}
		if err := s.DB.Save(g).Error; err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, ErrNotFound
	}
	s.gradeJackpot(j.ID)
	return s.JackpotViewOf(org, user, j.ID, 0)
}

// AddTicket saves a ticket.
func (s *Service) AddTicket(org, user, id, name, source string, picks []string) (*JackpotView, error) {
	j, gs, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	if len(picks) != len(gs) {
		return nil, inputErr("Give one pick per game (%d games).", len(gs))
	}
	norm := make([]string, len(picks))
	for i, p := range picks {
		ks, ok := ParsePick(p)
		if !ok {
			return nil, inputErr("Game %d: picks are 1, X, 2 or a cover like 1X, X2, 12, 1X2.", i+1)
		}
		norm[i] = pickString(ks)
	}
	switch source {
	case "custom", "most_likely", "top", "cover", "contrarian":
	default:
		source = "custom"
	}
	var n int64
	s.DB.Model(&JackpotTicket{}).Where("jackpot_id = ?", j.ID).Count(&n)
	if n >= 50 {
		return nil, inputErr("At most 50 tickets per jackpot.")
	}
	t := JackpotTicket{ID: uuid.NewString(), JackpotID: j.ID, OrganizationID: org, Name: clip(strings.TrimSpace(name), 120), Source: source, Picks: norm, CreatedAt: s.Now()}
	if t.Name == "" {
		t.Name = fmt.Sprintf("Ticket %d", n+1)
	}
	if err := s.DB.Create(&t).Error; err != nil {
		return nil, err
	}
	s.gradeJackpot(j.ID)
	return s.JackpotViewOf(org, user, j.ID, 0)
}

// DeleteTicket removes a ticket.
func (s *Service) DeleteTicket(org, user, id, ticketID string) (*JackpotView, error) {
	j, _, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	if s.DB.Where("id = ? AND jackpot_id = ?", ticketID, j.ID).Delete(&JackpotTicket{}).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	s.gradeJackpot(j.ID)
	return s.JackpotViewOf(org, user, j.ID, 0)
}

// ---------------------------------------------------------------- views

func gameViews(gs []JackpotGame) []GameView {
	out := make([]GameView, len(gs))
	for i, g := range gs {
		v := GameView{N: g.N, Home: g.Home, Away: g.Away, Raw: g.Raw, Odds: oddsMap(g),
			Kickoff: g.Kickoff, FixtureID: g.FixtureID, Match: g.Match, Void: g.Void}
		if v.Match.Candidates == nil {
			v.Match.Candidates = []MatchCandidate{}
		}
		if v.Match.Status == "" {
			v.Match.Status = "unmatched"
		}
		if g.Result != "" {
			r := g.Result
			v.Result = &r
		}
		if g.ResultSource != "" {
			r := g.ResultSource
			v.ResultSource = &r
		}
		if g.Score != "" {
			sc := g.Score
			v.Score = &sc
		}
		out[i] = v
	}
	return out
}

func (s *Service) effectiveStatus(j Jackpot) string {
	if j.Status == "graded" || j.Status == "draft" {
		return j.Status
	}
	if j.ClosesAt != nil && !j.ClosesAt.After(s.Now()) {
		return "closed"
	}
	return j.Status
}

// JackpotViewOf builds a jackpot's full view (topN 0 = the saved setting).
func (s *Service) JackpotViewOf(org, user, id string, topN int) (*JackpotView, error) {
	j, gs, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	if topN > 0 && topN <= 200 {
		j.TopN = topN
	}
	j.Status = s.effectiveStatus(j)
	a := s.analyseJackpot(j, gs, org)
	s.rememberMostLikely(&j, a)
	// Keep the automatic "Most likely" ticket current until the jackpot closes.
	if j.Status == "open" || j.Status == "draft" {
		s.syncMostLikelyTicket(j, a.MostLikely.Picks)
	}
	var ts []JackpotTicket
	s.DB.Where("jackpot_id = ?", j.ID).Order("created_at, id").Find(&ts)
	ps := make([]P3, len(a.Games))
	for i, g := range a.Games {
		ps[i] = g.p
	}
	v := &JackpotView{Jackpot: j, Games: gameViews(gs), Tickets: []TicketView{}, Analysis: a, Grading: j.Grading}
	for _, t := range ts {
		v.Tickets = append(v.Tickets, s.ticketView(j, t, ps))
	}
	s.jackpotWin(v, &j, gs) // v2.18: frozen chances, review, per-game ✓/✗ (jackpot_win.go)
	return v, nil
}

// rememberMostLikely stores the analysis' most likely ticket on the jackpot
// (for the list) when it changed.
func (s *Service) rememberMostLikely(j *Jackpot, a *JackpotAnalysis) {
	if j.ID == "" || a == nil || len(a.MostLikely.Picks) == 0 {
		return
	}
	if strings.Join(j.LastML, ",") == strings.Join(a.MostLikely.Picks, ",") && math.Abs(j.LastMLP-a.MostLikely.P) <= 1e-15 {
		return
	}
	now := s.Now()
	j.LastML, j.LastMLP, j.LastAnalysedAt = a.MostLikely.Picks, a.MostLikely.P, &now
	s.DB.Model(&Jackpot{}).Where("id = ?", j.ID).Updates(map[string]any{"last_ml": mustJSON(a.MostLikely.Picks), "last_ml_p": a.MostLikely.P, "last_analysed_at": now})
}

func (s *Service) syncMostLikelyTicket(j Jackpot, picks []string) {
	if len(picks) == 0 {
		return
	}
	var t JackpotTicket
	if s.DB.Where("jackpot_id = ? AND source = 'most_likely' AND name = 'Most likely'", j.ID).Limit(1).Find(&t).RowsAffected == 0 {
		t = JackpotTicket{ID: uuid.NewString(), JackpotID: j.ID, OrganizationID: j.OrganizationID, Name: "Most likely", Source: "most_likely", Picks: picks, CreatedAt: s.Now()}
		s.DB.Create(&t)
		return
	}
	if strings.Join(t.Picks, ",") != strings.Join(picks, ",") {
		s.DB.Model(&JackpotTicket{}).Where("id = ?", t.ID).Update("picks", mustJSON(picks))
	}
}

func (s *Service) ticketView(j Jackpot, t JackpotTicket, ps []P3) TicketView {
	v := TicketView{ID: t.ID, Name: t.Name, Source: t.Source, Picks: t.Picks, Correct: t.Correct, Of: len(t.Picks), CreatedAt: t.CreatedAt}
	if len(ps) == len(t.Picks) {
		sel := make([][]int, len(t.Picks))
		for i, p := range t.Picks {
			sel[i], _ = ParsePick(p)
		}
		pa, n := CoveredProb(ps, sel)
		v.PAll, v.Combinations, v.Cost = pa, n, round2(float64(n)*j.TicketPrice)
		if pa > 0 {
			v.OneIn = math.Round(1 / pa)
		}
	}
	if j.Grading != nil {
		for _, tg := range j.Grading.Tickets {
			if tg.ID == t.ID {
				c := tg.Correct
				v.Correct, v.Of, v.Bonus = &c, tg.Of, tg.Bonus
				if j.Grading.Complete {
					w := tg.WonJackpot
					v.WonJackpot = &w
				}
			}
		}
	}
	return v
}

// ListJackpots lists a person's jackpots.
func (s *Service) ListJackpots(org, user, status string, limit, offset int) ([]JackpotSummary, int64) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := s.DB.Model(&Jackpot{}).Where("organization_id = ? AND user_id = ?", org, user)
	switch status {
	case "graded", "draft":
		q = q.Where("status = ?", status)
	case "open":
		q = q.Where("status = 'open' AND (closes_at IS NULL OR closes_at > ?)", s.Now())
	case "closed":
		q = q.Where("status IN ('open','closed') AND closes_at IS NOT NULL AND closes_at <= ?", s.Now())
	}
	var n int64
	q.Count(&n)
	var js []Jackpot
	q.Order("COALESCE(closes_at, created_at) DESC, created_at DESC").Limit(limit).Offset(offset).Find(&js)
	out := []JackpotSummary{}
	for _, j := range js {
		out = append(out, s.summaryOf(j))
	}
	return out, n
}

func (s *Service) summaryOf(j Jackpot) JackpotSummary {
	var gs []JackpotGame
	s.DB.Where("jackpot_id = ?", j.ID).Order("n").Find(&gs)
	sm := JackpotSummary{ID: j.ID, Name: j.Name, Bookmaker: j.Bookmaker, Prize: j.Prize, Currency: j.Currency, TicketPrice: j.TicketPrice, ClosesAt: j.ClosesAt,
		Status: s.effectiveStatus(j), NGames: len(gs), CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt}
	ps := make([]P3, len(gs))
	for i, g := range gs {
		if g.Match.Status == "matched" || g.Match.Status == "manual" {
			sm.MatchedGames++
		}
		ps[i] = P3{1.0 / 3, 1.0 / 3, 1.0 / 3}
		if hasOdds(g) {
			ps[i], _ = DeMarginProportional([3]float64{g.Odds1, g.OddsX, g.Odds2})
		}
	}
	// The list shows the last analysis' most likely ticket and chance (as on
	// the jackpot's page); before any analysis, the jackpot odds alone.
	if len(j.LastML) == len(gs) && j.LastMLP > 0 {
		sm.MostLikely.Picks, sm.MostLikely.OneIn = j.LastML, math.Round(1/j.LastMLP)
	}
	var t JackpotTicket
	if sm.MostLikely.Picks != nil {
		// done
	} else if s.DB.Where("jackpot_id = ? AND source = 'most_likely' AND name = 'Most likely'", j.ID).Limit(1).Find(&t).RowsAffected > 0 && len(t.Picks) == len(gs) {
		sm.MostLikely.Picks = t.Picks
	} else {
		ml := MostLikely(ps)
		sm.MostLikely.Picks = ticketLabels(ml)
	}
	if sm.MostLikely.OneIn == 0 {
		sel := make([][]int, len(sm.MostLikely.Picks))
		for i, p := range sm.MostLikely.Picks {
			sel[i], _ = ParsePick(p)
		}
		if pa, _ := CoveredProb(ps, sel); pa > 0 {
			sm.MostLikely.OneIn = math.Round(1 / pa)
		}
	}
	var nt int64
	s.DB.Model(&JackpotTicket{}).Where("jackpot_id = ?", j.ID).Count(&nt)
	sm.Tickets = int(nt)
	if j.Grading != nil {
		sm.Graded = &SummaryGrade{CorrectMostLikely: j.Grading.CorrectMostLikely, ExpectedCorrect: j.Grading.ExpectedCorrect, Complete: j.Grading.Complete}
		for _, tg := range j.Grading.Tickets {
			c := tg.Correct
			if sm.Graded.BestTicketCorrect == nil || c > *sm.Graded.BestTicketCorrect {
				sm.Graded.BestTicketCorrect = &c
			}
		}
	}
	if sm.MostLikely.Picks == nil {
		sm.MostLikely.Picks = []string{}
	}
	s.reviewSummary(&sm, j, gs) // v2.18
	return sm
}

// oddsMap shows a game's jackpot odds (null when not typed yet).
func oddsMap(g JackpotGame) map[string]*float64 {
	m := map[string]*float64{"1": nil, "X": nil, "2": nil}
	if hasOdds(g) {
		a, b, c := g.Odds1, g.OddsX, g.Odds2
		m["1"], m["X"], m["2"] = &a, &b, &c
	}
	return m
}

func hasOdds(g JackpotGame) bool { return g.Odds1 > 1 && g.OddsX > 1 && g.Odds2 > 1 }

func ticketLabels(t Ticket) []string {
	out := make([]string, len(t))
	for i, k := range t {
		out[i] = jpLabels[k]
	}
	return out
}

// JackpotHistory lists graded jackpots with simple stats.
func (s *Service) JackpotHistory(org, user string, limit int) map[string]any {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var js []Jackpot
	s.DB.Where("organization_id = ? AND user_id = ? AND grading IS NOT NULL", org, user).Order("graded_at DESC NULLS LAST, created_at DESC").Limit(limit).Find(&js)
	out := []JackpotSummary{}
	sum, n, best := 0.0, 0, 0
	byBook := map[string]map[string]float64{}
	for _, j := range js {
		sm := s.summaryOf(j)
		out = append(out, sm)
		if j.Grading != nil && j.Grading.CorrectMostLikely != nil && j.Grading.Complete {
			sum += float64(*j.Grading.CorrectMostLikely)
			n++
			b := j.Bookmaker
			if byBook[b] == nil {
				byBook[b] = map[string]float64{"jackpots": 0, "avg_correct_most_likely": 0}
			}
			byBook[b]["jackpots"]++
			byBook[b]["avg_correct_most_likely"] += float64(*j.Grading.CorrectMostLikely)
		}
		if sm.Graded != nil && sm.Graded.BestTicketCorrect != nil && *sm.Graded.BestTicketCorrect > best {
			best = *sm.Graded.BestTicketCorrect
		}
	}
	for _, v := range byBook {
		if v["jackpots"] > 0 {
			v["avg_correct_most_likely"] = round2(v["avg_correct_most_likely"] / v["jackpots"])
		}
	}
	stats := map[string]any{"jackpots": len(js), "graded_complete": n, "best_correct": best, "by_bookmaker": byBook, "avg_correct_most_likely": nil}
	if n > 0 {
		stats["avg_correct_most_likely"] = round2(sum / float64(n))
	}
	return map[string]any{"history": out, "stats": stats}
}

// ---------------------------------------------------------------- analysis

func p3map(p P3) map[string]float64 {
	return map[string]float64{"1": round4(p[0]), "X": round4(p[1]), "2": round4(p[2])}
}

// gameSources collects a matched game's other sources (market, ClubElo, model).
func (s *Service) gameSources(fid *int64) (market, elo, mdl *P3, mDetail, eDetail, mdDetail, mWhy, eWhy, mdWhy string, ev []Evidence) {
	mWhy, eWhy, mdWhy = "no fixture matched", "no fixture matched", "no fixture matched"
	if fid == nil {
		return
	}
	var f Fixture
	if s.DB.Where("id = ?", *fid).Limit(1).Find(&f).RowsAffected == 0 {
		return
	}
	mv := s.marketView(f, nil, "")
	if len(mv.Fair1X2) == 3 {
		p := P3{mv.Fair1X2[SelHome], mv.Fair1X2[SelDraw], mv.Fair1X2[SelAway]}
		market, mWhy = &p, ""
		mDetail = mv.FairSource
		if mv.AsOf != nil {
			mDetail += ", " + mv.AsOf.Format("2006-01-02 15:04") + " UTC"
		}
	} else {
		mWhy = "no bookmaker prices stored for this fixture"
	}
	if ce := s.clubEloFor(f); ce.Available && ce.Probs != nil {
		p := P3{ce.Probs["home"], ce.Probs["draw"], ce.Probs["away"]}
		elo, eWhy = &p, ""
		eDetail = fmt.Sprintf("%s %.0f v %s %.0f (%s)", ce.Home.Club, ce.Home.Elo, ce.Away.Club, ce.Away.Elo, strings.ReplaceAll(ce.ProbsSource, "_", " "))
	} else {
		eWhy = "ClubElo has no rating for one of the teams"
		if ce.Note != "" {
			eWhy = ce.Note
		}
	}
	if ok, why := s.modelUsable(f); ok {
		if pr, have := s.CurrentPrediction(f); have {
			p := P3{pr.PHome, pr.PDraw, pr.PAway}
			mdl, mdWhy = &p, ""
			mdDetail = fmt.Sprintf("computed %s", pr.ComputedAt.Format("2006-01-02 15:04"))
		} else {
			mdWhy = "no prediction"
		}
	} else {
		mdWhy = why
	}
	// Team news evidence.
	for _, side := range []struct {
		team int64
		name string
	}{{f.HomeID, s.teamName(f.HomeID)}, {f.AwayID, s.teamName(f.AwayID)}} {
		facts := s.activeFacts(side.team, "")
		conf, rep := 0, 0
		names := []string{}
		for _, fc := range facts {
			if fc.dismissed {
				continue
			}
			if fc.confirmed {
				conf++
			} else {
				rep++
			}
			if fc.PlayerName != "" && len(names) < 3 && (fc.Status == TNInjured || fc.Status == TNSuspended || fc.Status == TNDoubtful) {
				names = append(names, fc.PlayerName+" ("+fc.Status+")")
			}
		}
		if conf+rep > 0 {
			t := fmt.Sprintf("%s: %d team-news fact(s), %d confirmed", side.name, conf+rep, conf)
			if len(names) > 0 {
				t += " — " + strings.Join(names, ", ")
			}
			st := "reported"
			if conf > 0 {
				st = "confirmed"
			}
			ev = append(ev, Evidence{Kind: "team_news", Text: t, Status: st})
		}
	}
	return
}

// modelUsable: DASYIN's model is only used for a jackpot game when its
// competition has a Dixon–Coles fit or both teams have 10+ stored results.
func (s *Service) modelUsable(f Fixture) (bool, string) {
	var n []int64
	s.DB.Raw(`SELECT COUNT(*) FROM fb_fixtures WHERE status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND (home_id = ? OR away_id = ?) AND kickoff < ?
		UNION ALL SELECT COUNT(*) FROM fb_fixtures WHERE status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND (home_id = ? OR away_id = ?) AND kickoff < ?`,
		f.HomeID, f.HomeID, s.Now(), f.AwayID, f.AwayID, s.Now()).Scan(&n)
	if len(n) == 2 && n[0] >= 10 && n[1] >= 10 {
		return true, ""
	}
	cacheMu.Lock()
	e, ok := dcCache[f.CompetitionID]
	cacheMu.Unlock()
	if ok && e.p != nil {
		return true, ""
	}
	return false, "too few stored results for DASYIN's model in this league"
}

// analyseJackpot computes the analysis of a jackpot's games.
func (s *Service) analyseJackpot(j Jackpot, gs []JackpotGame, org string) *JackpotAnalysis {
	now := s.Now()
	a := &JackpotAnalysis{NGames: len(gs), CombinationsAll: math.Pow(3, float64(len(gs))), AsOf: now, Currency: j.Currency, Weights: j.Weights,
		Games: []GameAnalysis{}, TopTickets: []TicketOut{}, Cover: []CoverOut{}, CoverPlans: []CoverPlan{}, AtLeast: []KP{}, CorrectDist: []KP{},
		DrawDist: []KP{}, Contrarian: []Variant{}, Notes: []string{}, Disclaimer: JackpotDisclaimer}
	if len(gs) == 0 {
		return a
	}
	w := j.Weights
	if len(w) == 0 {
		w = DefaultJackpotWeights
	}
	ps := make([]P3, len(gs))
	unmatched := []string{}
	noOdds := []string{}
	for i, g := range gs {
		jp, margin := DeMarginProportional([3]float64{g.Odds1, g.OddsX, g.Odds2})
		ga := GameAnalysis{N: g.N, Home: g.Home, Away: g.Away, Kickoff: g.Kickoff, FixtureID: g.FixtureID,
			Odds: oddsMap(g), Margin: round4(margin), Evidence: []Evidence{}}
		if g.Match.Home != "" && (g.Match.Status == "matched" || g.Match.Status == "manual") {
			ga.Home, ga.Away = g.Match.Home, g.Match.Away
		}
		srcs := []P3{}
		wts := []float64{}
		labels := []SourceP{}
		if hasOdds(g) {
			srcs, wts = append(srcs, jp), append(wts, w["jackpot"])
			labels = append(labels, SourceP{Key: "jackpot", Label: "Jackpot odds (de-margined)", Probs: p3map(jp), Used: w["jackpot"] > 0,
				Detail: fmt.Sprintf("%s odds %.2f / %.2f / %.2f, margin %.1f%%", firstNonEmpty(j.Bookmaker, "jackpot"), g.Odds1, g.OddsX, g.Odds2, margin*100)})
		} else {
			jp, margin, ga.Margin = P3{1.0 / 3, 1.0 / 3, 1.0 / 3}, 0, 0
			labels = append(labels, SourceP{Key: "jackpot", Label: "Jackpot odds (de-margined)", Why: "the jackpot's odds aren't typed yet"})
			noOdds = append(noOdds, strconv.Itoa(g.N))
		}
		var market, elo, mdl *P3
		var md, ed, mdd, mw, ew, mdw string
		var ev []Evidence
		if g.Match.Status == "matched" || g.Match.Status == "manual" {
			market, elo, mdl, md, ed, mdd, mw, ew, mdw, ev = s.gameSources(g.FixtureID)
		} else {
			mw, ew, mdw = "no fixture matched", "no fixture matched", "no fixture matched"
			unmatched = append(unmatched, fmt.Sprintf("%d (%s v %s)", g.N, g.Home, g.Away))
		}
		add := func(key, label string, p *P3, detail, why string) {
			sp := SourceP{Key: key, Label: label, Detail: detail, Why: why}
			if p != nil && w[key] > 0 {
				srcs = append(srcs, *p)
				wts = append(wts, w[key])
				sp.Probs, sp.Used = p3map(*p), true
			} else if p != nil {
				sp.Probs = p3map(*p)
				sp.Why = "weight 0"
			}
			labels = append(labels, sp)
		}
		mlabel := "Sharp / consensus market (de-margined)"
		if strings.Contains(md, "Pinnacle") {
			mlabel = "Pinnacle (de-margined)"
		}
		add("market", mlabel, market, md, mw)
		add("clubelo", "ClubElo", elo, ed, ew)
		add("model", "DASYIN model", mdl, mdd, mdw)
		blend, eff := BlendP3(srcs, wts)
		if blend[0]+blend[1]+blend[2] == 0 {
			blend = jp // nothing else known: the jackpot odds (or 1/3 each when not typed)
		}
		// Effective weights back onto the labels (in the order used).
		ui := 0
		for li := range labels {
			if labels[li].Used {
				if ui < len(eff) {
					labels[li].Weight = round4(eff[ui])
				}
				ui++
			}
		}
		ga.Sources, ga.p = labels, blend
		ga.Probs = p3map(blend)
		order := []int{0, 1, 2}
		sort.SliceStable(order, func(x, y int) bool { return blend[order[x]] > blend[order[y]] })
		ga.Pick, ga.PickP, ga.Second, ga.SecondP = jpLabels[order[0]], round4(blend[order[0]]), jpLabels[order[1]], round4(blend[order[1]])
		ga.Certainty = round4(blend[order[0]] - blend[order[1]])
		// Evidence leans draw: another source rates the draw ≥ 30% and the
		// blended draw is within 6 points of the favourite.
		for _, p := range []*P3{market, elo, mdl} {
			if p != nil && p[1] >= 0.30 && blend[order[0]]-blend[1] <= 0.06 {
				ga.DrawLean = true
			}
		}
		if ga.Pick == "X" {
			ga.DrawLean = true
		}
		ga.Public = p3map(PublicShares(blend, j.Sharing.Popularity))
		ga.Evidence = append(ga.Evidence, ev...)
		if market != nil {
			for k := 0; k < 3; k++ {
				if d := jp[k] - market[k]; math.Abs(d) >= 0.06 {
					dir := "higher"
					if d < 0 {
						dir = "lower"
					}
					ga.Evidence = append(ga.Evidence, Evidence{Kind: "market", Text: fmt.Sprintf("The jackpot's odds rate %s %.0f points %s than the sharp market.", outcomeName(k, ga.Home, ga.Away), math.Abs(d)*100, dir)})
				}
			}
		}
		if (g.Match.Status == "matched" || g.Match.Status == "manual") && g.FixtureID != nil {
			var fx Fixture
			if s.DB.Where("id = ?", *g.FixtureID).Limit(1).Find(&fx).RowsAffected > 0 {
				sc := s.SourceCheckFor(fx)
				ga.SourceCheck = &GameSourceCheck{Status: sc.Summary.Status, Items: sc.Items}
				if sc.Summary.Status == "minor" || sc.Summary.Status == "conflict" {
					parts := []string{}
					for _, it := range sc.Items {
						if it.Status == "minor" || it.Status == "conflict" {
							parts = append(parts, it.Label+" ("+it.Status+")")
						}
					}
					ga.Evidence = append(ga.Evidence, Evidence{Kind: "source_check", Status: sc.Summary.Status, Text: "Sources differ: " + strings.Join(parts, "; ")})
				}
			}
		}
		if elo != nil && market != nil {
			for k := 0; k < 3; k++ {
				if d := elo[k] - market[k]; math.Abs(d) >= 0.10 {
					ga.Evidence = append(ga.Evidence, Evidence{Kind: "clubelo", Text: fmt.Sprintf("ClubElo gives %s %.0f%% against the market's %.0f%%.", outcomeName(k, ga.Home, ga.Away), elo[k]*100, market[k]*100)})
				}
			}
		}
		ps[i] = blend
		a.Games = append(a.Games, ga)
	}
	if len(noOdds) > 0 {
		a.Notes = append(a.Notes, "No jackpot odds typed yet for game "+strings.Join(noOdds, ", ")+": those games use the other sources only (1/3 each when there are none).")
	}
	if len(unmatched) > 0 {
		a.Notes = append(a.Notes, "Not matched to a fixture: game "+strings.Join(unmatched, ", ")+" — only the jackpot's own odds are used there. Match them by hand for the market, ClubElo and the model.")
	}
	ml := MostLikely(ps)
	pml := TicketProb(ps, ml)
	a.MostLikely.Picks = ticketLabels(ml)
	a.MostLikely.P = pml
	if pml > 0 {
		a.MostLikely.OneIn = math.Round(1 / pml)
	}
	qs := make([]float64, len(ml))
	for i, k := range ml {
		qs[i] = ps[i][k]
		a.MostLikely.ExpectedCorrect += qs[i]
		if k == 1 {
			a.MostLikely.Draws++
		}
	}
	a.MostLikely.ExpectedCorrect = round2(a.MostLikely.ExpectedCorrect)
	a.ExpectedCorrect = a.MostLikely.ExpectedCorrect
	// Top-N.
	cum := 0.0
	for r, t := range TopTickets(ps, j.TopN) {
		p := TicketProb(ps, t)
		cum += p
		to := TicketOut{Rank: r + 1, Picks: ticketLabels(t), P: p, CumP: cum, Changes: []PickChange{}}
		if p > 0 {
			to.OneIn = math.Round(1 / p)
		}
		for i, k := range t {
			if k == 1 {
				to.Draws++
			}
			if k != ml[i] {
				to.Changes = append(to.Changes, PickChange{N: i + 1, From: jpLabels[ml[i]], To: jpLabels[k]})
			}
		}
		a.TopTickets = append(a.TopTickets, to)
	}
	// Doubles / triples priority.
	steps, _ := CoverPriority(ps, minInt(10, 2*len(ps)))
	sel := make([][]int, len(ps))
	for i := range sel {
		sel[i] = []int{ml[i]}
	}
	for i, st := range steps {
		co := CoverOut{Step: i + 1, CoverStep: st, Cost: round2(float64(st.Combinations) * j.TicketPrice)}
		if st.PAll > 0 {
			co.OneIn = math.Round(1 / st.PAll)
		}
		a.Cover = append(a.Cover, co)
		ks, _ := ParsePick(st.Picks)
		sel[st.Game-1] = ks
		nd, nt := 0, 0
		picks := make([]string, len(sel))
		for gi, s2 := range sel {
			switch len(s2) {
			case 2:
				nd++
			case 3:
				nt++
			}
			picks[gi] = pickString(s2)
		}
		label := fmt.Sprintf("%d double%s", nd, plural(nd))
		if nt > 0 {
			label += fmt.Sprintf(", %d triple%s", nt, plural(nt))
		}
		a.CoverPlans = append(a.CoverPlans, CoverPlan{Label: label, Doubles: nd, Triples: nt, Combinations: st.Combinations, Cost: co.Cost, PAll: st.PAll, OneIn: co.OneIn,
			Picks: picks, PerCombo: st.PAll / float64(st.Combinations)})
	}
	// P(≥k) and distributions.
	dist := PoissonBinomial(qs)
	n := len(ps)
	for k := n; k >= maxInt(0, n-3); k-- {
		a.AtLeast = append(a.AtLeast, KP{K: k, P: AtLeast(dist, k)})
	}
	for k, p := range dist {
		a.CorrectDist = append(a.CorrectDist, KP{K: k, P: p})
	}
	ed, dd := ExpectedDraws(ps)
	a.ExpectedDraws = round2(ed)
	for k, p := range dd {
		a.DrawDist = append(a.DrawDist, KP{K: k, P: p})
	}
	// Expected value.
	a.EV = TicketEV(ps, ml, j.Prize, j.TicketPrice, j.Bonuses, j.Sharing)
	a.EV.Explain = evExplain(a.EV, j)
	a.Sharing.SharingOpts = j.Sharing
	if j.Sharing.Mode == "fixed" {
		a.Sharing.Explain = "A fixed assumption: a winning ticket is expected to share the prize with the number of other winners you set."
	} else {
		a.Sharing.Explain = fmt.Sprintf("Assumes about %s tickets are sold and that people pick favourites more often than the odds justify (each outcome's share of tickets ∝ probability^%.1f). "+
			"Change it if you know the bookmaker's numbers.", fmtThousands(j.Sharing.TicketsSold), j.Sharing.Popularity)
	}
	a.Contrarian = contrarianVariants(ps, ml, a.Games, j)
	if pml > 0 {
		a.Notes = append(a.Notes, fmt.Sprintf("Even the most likely ticket loses about %s times out of %s.", fmtThousands(math.Round(1/pml)-1), fmtThousands(math.Round(1/pml))))
	}
	s.jackpotVerdicts(a) // v2.18 (jackpot_win.go)
	return a
}

func outcomeName(k int, home, away string) string {
	switch k {
	case 0:
		return home + " to win"
	case 1:
		return "the draw"
	}
	return away + " to win"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

func fmtThousands(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	out := []byte{}
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func evExplain(e EVResult, j Jackpot) string {
	cur := j.Currency
	if cur == "" {
		cur = "KES"
	}
	share := ""
	if j.Sharing.Mode == "fixed" {
		share = fmt.Sprintf("you set %.1f other winners, so the expected share is %.0f%% of the prize", e.CoWinners, e.ShareFactor*100)
	} else {
		share = fmt.Sprintf("with about %s tickets and favourites over-picked, a winning ticket like this one is expected to share with %.1f other winner(s), so the expected share is %.0f%% of the prize",
			fmtThousands(e.TicketsSold), e.CoWinners, e.ShareFactor*100)
	}
	return fmt.Sprintf("Expected value of one %s %s ticket with the most likely picks: jackpot part %s %.2f (%s) plus bonuses %s %.2f, minus the ticket = %s %.2f. "+
		"Bonuses are not shared. This is an average over very many tickets — almost every ticket wins nothing, and a positive number is not a promise.",
		cur, fmtThousands(j.TicketPrice), cur, e.EVJackpot, share, cur, e.EVBonuses, cur, e.EVTotal)
}

// contrarianVariants: draws where evidence leans draw, and the cheapest
// single changes, each with what it costs in probability and its EV (fewer
// co-winners when the public avoids the pick).
func contrarianVariants(ps []P3, ml Ticket, games []GameAnalysis, j Jackpot) []Variant {
	pml := TicketProb(ps, ml)
	out := []Variant{}
	if pml <= 0 {
		return out
	}
	mk := func(label string, t Ticket, why string) Variant {
		p := TicketProb(ps, t)
		e := TicketEV(ps, t, j.Prize, j.TicketPrice, j.Bonuses, j.Sharing)
		v := Variant{Label: label, Picks: ticketLabels(t), Changes: []PickChange{}, P: p, PRatio: round4(p / pml), CoWinners: e.CoWinners, EVTotal: e.EVTotal, Why: why}
		if p > 0 {
			v.OneIn = math.Round(1 / p)
		}
		for i := range t {
			if t[i] != ml[i] {
				v.Changes = append(v.Changes, PickChange{N: i + 1, From: jpLabels[ml[i]], To: jpLabels[t[i]]})
			}
		}
		return v
	}
	seen := map[string]bool{}
	draws := []int{}
	for i, g := range games {
		if g.DrawLean && ml[i] != 1 {
			draws = append(draws, i)
		}
	}
	for _, i := range draws {
		t := append(Ticket(nil), ml...)
		t[i] = 1
		v := mk(fmt.Sprintf("Draw in game %d instead of %s", i+1, map[int]string{0: "home", 2: "away"}[ml[i]]), t,
			fmt.Sprintf("Another source rates the draw at 30%% or more and it is close to the favourite. Draws are under-picked, so a winning ticket with this draw is shared with fewer people; it keeps %.0f%% of the ticket's chance.", (TicketProb(ps, t)/pml)*100))
		out = append(out, v)
		seen[strings.Join(v.Picks, "")] = true
	}
	if len(draws) >= 2 {
		t := append(Ticket(nil), ml...)
		for _, i := range draws {
			t[i] = 1
		}
		v := mk(fmt.Sprintf("All %d draw leans together", len(draws)), t, "Every draw the evidence leans to, together: far fewer co-winners, but a much smaller chance.")
		if !seen[strings.Join(v.Picks, "")] {
			out = append(out, v)
			seen[strings.Join(v.Picks, "")] = true
		}
	}
	// Cheapest single changes.
	type alt struct {
		i, k  int
		ratio float64
	}
	alts := []alt{}
	for i, p := range ps {
		for k := 0; k < 3; k++ {
			if k != ml[i] && p[ml[i]] > 0 {
				alts = append(alts, alt{i, k, p[k] / p[ml[i]]})
			}
		}
	}
	sort.SliceStable(alts, func(a, b int) bool { return alts[a].ratio > alts[b].ratio })
	for _, x := range alts {
		if len(out) >= 5 {
			break
		}
		t := append(Ticket(nil), ml...)
		t[x.i] = x.k
		key := ""
		for _, k := range t {
			key += jpLabels[k]
		}
		if seen[key] {
			continue
		}
		v := mk(fmt.Sprintf("Game %d: %s instead of %s", x.i+1, jpLabels[x.k], jpLabels[ml[x.i]]), t,
			fmt.Sprintf("The cheapest single change: it keeps %.0f%% of the ticket's chance and is picked by fewer people.", x.ratio*100))
		out = append(out, v)
		seen[key] = true
	}
	return out
}

// ---------------------------------------------------------------- grading

// fixtureOutcome reads a fixture's 1X2 result on the 90-minute score.
func fixtureOutcome(f Fixture) (res string, void bool, score string, final bool) {
	if voidStatuses[f.Status] {
		return "", true, "", true
	}
	if !IsFinal(f.Status) {
		return "", false, "", false
	}
	h, a := f.FTHome, f.FTAway
	if h == nil || a == nil {
		h, a = f.HomeGoals, f.AwayGoals
	}
	if h == nil || a == nil {
		return "", false, "", false
	}
	score = fmt.Sprintf("%d-%d", *h, *a)
	switch {
	case *h > *a:
		return "1", false, score, true
	case *h < *a:
		return "2", false, score, true
	}
	return "X", false, score, true
}

// GradeJackpots refreshes results of open jackpots from their fixtures and
// grades them (scheduler pass). It returns how many jackpots changed.
func (s *Service) GradeJackpots() int {
	var ids []string
	s.DB.Raw(`SELECT DISTINCT j.id FROM fb_jackpots j JOIN fb_jackpot_games g ON g.jackpot_id = j.id
		WHERE j.status <> 'graded' AND g.fixture_id IS NOT NULL AND g.result = '' AND NOT g.void AND g.kickoff < ?`, s.Now().Add(-90*time.Minute)).Scan(&ids)
	n := 0
	for _, id := range ids {
		if s.gradeJackpot(id) {
			n++
		}
	}
	return n
}

// gradeJackpot pulls results from fixtures and grades the tickets.
func (s *Service) gradeJackpot(id string) bool {
	var j Jackpot
	if s.DB.Where("id = ?", id).Limit(1).Find(&j).RowsAffected == 0 {
		return false
	}
	var gs []JackpotGame
	s.DB.Where("jackpot_id = ?", id).Order("n").Find(&gs)
	changed := false
	for i := range gs {
		g := &gs[i]
		if g.ResultSource == "manual" || g.FixtureID == nil {
			continue
		}
		var f Fixture
		if s.DB.Where("id = ?", *g.FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
			continue
		}
		res, void, score, final := fixtureOutcome(f)
		if !final {
			continue
		}
		if g.Result != res || g.Void != void || g.Score != score {
			g.Result, g.Void, g.Score, g.ResultSource = res, void, score, ProviderAPIFootball
			s.DB.Model(&JackpotGame{}).Where("jackpot_id = ? AND n = ?", id, g.N).Updates(map[string]any{"result": res, "void": void, "score": score, "result_source": ProviderAPIFootball})
			changed = true
		}
	}
	gr := &Grading{GradedAt: s.Now(), Complete: true, Results: make([]*string, len(gs)), Void: make([]bool, len(gs)), Tickets: []TicketGrade{}}
	anyRes := false
	for i, g := range gs {
		gr.Void[i] = g.Void
		if g.Result != "" {
			r := g.Result
			gr.Results[i] = &r
			anyRes = true
		} else if !g.Void {
			gr.Complete = false
		}
		if g.Void {
			anyRes = true
		}
	}
	if !anyRes {
		if j.Grading != nil {
			s.DB.Model(&Jackpot{}).Where("id = ?", id).Updates(map[string]any{"grading": nil, "graded_at": nil})
			return true
		}
		return changed
	}
	countCorrect := func(picks []string) (int, int) {
		c, of := 0, 0
		for i, g := range gs {
			if g.Void || i >= len(picks) {
				continue
			}
			of++
			if g.Result != "" && strings.Contains(picks[i], g.Result) {
				c++
			}
		}
		return c, of
	}
	var ts []JackpotTicket
	s.DB.Where("jackpot_id = ?", id).Order("created_at, id").Find(&ts)
	now := s.Now()
	for _, t := range ts {
		c, of := countCorrect(t.Picks)
		tg := TicketGrade{ID: t.ID, Name: t.Name, Picks: t.Picks, Correct: c, Of: of, WonJackpot: gr.Complete && c == of && of > 0}
		for _, b := range j.Bonuses {
			if b.Correct == c && gr.Complete {
				bb := b
				tg.Bonus = &bb
			}
		}
		gr.Tickets = append(gr.Tickets, tg)
		cc := c
		s.DB.Model(&JackpotTicket{}).Where("id = ?", t.ID).Updates(map[string]any{"correct": cc, "graded_at": now})
		if t.Source == "most_likely" && t.Name == "Most likely" {
			gr.CorrectMostLikely = &cc
		}
	}
	// Best of the current top-N.
	if gr.Complete {
		a := s.analyseJackpot(j, gs, j.OrganizationID)
		s.rememberMostLikely(&j, a)
		for _, t := range a.TopTickets {
			c, _ := countCorrect(t.Picks)
			if gr.BestTopN == nil || c > gr.BestTopN.Correct {
				gr.BestTopN = &struct {
					Rank    int `json:"rank"`
					Correct int `json:"correct"`
				}{t.Rank, c}
			}
		}
		if gr.CorrectMostLikely == nil {
			c, _ := countCorrect(a.MostLikely.Picks)
			gr.CorrectMostLikely = &c
		}
		// Expected correct of the most likely ticket (non-void games).
		picks := a.MostLikely.Picks
		for _, t := range ts {
			if t.Source == "most_likely" && t.Name == "Most likely" && len(t.Picks) == len(gs) {
				picks = t.Picks
			}
		}
		e := 0.0
		for i, ga := range a.Games {
			if i < len(picks) && !gs[i].Void {
				for _, k := range strings.Split(picks[i], "") {
					e += ga.Probs[k]
				}
			}
		}
		e = round2(e)
		gr.ExpectedCorrect = &e
	}
	up := map[string]any{"grading": mustJSON(gr), "updated_at": now}
	if gr.Complete {
		up["status"], up["graded_at"] = "graded", now
	}
	s.DB.Model(&Jackpot{}).Where("id = ?", id).Updates(up)
	return true
}

// GradeNow grades one jackpot on request.
func (s *Service) GradeNow(org, user, id string) (*JackpotView, error) {
	j, _, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	s.gradeJackpot(j.ID)
	return s.JackpotViewOf(org, user, j.ID, 0)
}
