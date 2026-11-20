package football

import (
	"log"
	"math"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The Odds API names teams its own way ("Manchester Utd" vs "Manchester
// United", "Inter" vs "Inter Milan") and has its own event ids. An event is
// matched to an API-Football fixture of the same competition when the kickoff
// is within ±15 minutes and both team names are similar enough; the alias
// table (fb_team_aliases, editable by super admins) is checked first and
// learns from every confident match. Unmatched events are logged
// (fb_unmatched_events) so an alias can be added.

// MatchWindow is how far kickoff times may differ.
const MatchWindow = 15 * time.Minute

// stopWords are dropped before comparing names.
var stopWords = map[string]bool{"fc": true, "afc": true, "cf": true, "sc": true, "ac": true, "as": true, "ss": true, "club": true, "de": true,
	"the": true, "cd": true, "sd": true, "ud": true, "fk": true, "sk": true, "bk": true, "if": true, "calcio": true, "football": true, "sv": true}

var abbreviations = map[string]string{"utd": "united", "st": "saint", "ath": "athletic", "atl": "atletico", "dep": "deportivo", "spa": "sporting",
	"int": "internazionale", "inter": "internazionale", "wolves": "wolverhampton", "spurs": "tottenham", "man": "manchester", "nott'm": "nottingham",
	"nottm": "nottingham", "psg": "paris saint germain", "fcsb": "steaua", "rb": "rasenballsport", "gor": "gor", "afc": ""}

// NormTeam normalises a team name: lower case, accents and punctuation
// removed, common abbreviations expanded, club suffixes dropped.
func NormTeam(s string) string {
	s = norm.NFD.String(strings.ToLower(s))
	b := strings.Builder{}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'':
			continue
		default:
			b.WriteRune(' ')
		}
	}
	out := []string{}
	for _, w := range strings.Fields(b.String()) {
		if x, ok := abbreviations[w]; ok {
			w = x
		}
		if w == "" || stopWords[w] {
			continue
		}
		out = append(out, strings.Fields(w)...)
	}
	return strings.Join(out, " ")
}

// NameSimilarity is a 0..1 similarity of two normalised names: the best of
// token overlap, containment and character similarity.
func NameSimilarity(a, b string) float64 {
	a, b = NormTeam(a), NormTeam(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	ta, tb := strings.Fields(a), strings.Fields(b)
	set := map[string]bool{}
	for _, t := range ta {
		set[t] = true
	}
	inter := 0
	for _, t := range tb {
		if set[t] {
			inter++
		} else {
			// prefix tokens ("ath" vs "athletic" already expanded; "kingsp" vs "kingsport")
			for _, u := range ta {
				if len(t) >= 3 && len(u) >= 3 && (strings.HasPrefix(u, t) || strings.HasPrefix(t, u)) {
					inter++
					break
				}
			}
		}
	}
	jac := float64(inter) / math.Max(float64(len(ta)), float64(len(tb)))
	cont := 0.0
	if strings.Contains(a, b) || strings.Contains(b, a) {
		cont = 0.85
	}
	lev := 1 - float64(levenshtein(a, b))/math.Max(float64(len(a)), float64(len(b)))
	return math.Max(jac, math.Max(cont, lev))
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			c := 1
			if ra[i-1] == rb[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Candidate is a fixture an event may belong to.
type Candidate struct {
	FixtureID int64
	Kickoff   time.Time
	HomeID    int64
	AwayID    int64
	Home      string
	Away      string
}

// MatchResult says how an event was matched.
type MatchResult struct {
	FixtureID int64
	Score     float64
	ByAlias   bool
	Guess     string
}

// MatchEvent picks the fixture for an event: kickoff within ±15 min, then
// alias hits (exact team id) or name similarity; both sides must pass
// (≥ 0.6 each, ≥ 1.4 together). aliases maps normalised names to team ids.
func MatchEvent(home, away string, commence time.Time, cands []Candidate, aliases map[string]int64) MatchResult {
	best := MatchResult{}
	nh, na := NormTeam(home), NormTeam(away)
	for _, c := range cands {
		d := c.Kickoff.Sub(commence)
		if d < -MatchWindow || d > MatchWindow {
			continue
		}
		side := func(name, norm string, id int64, fixName string) (float64, bool) {
			if t, ok := aliases[norm]; ok {
				if t == id {
					return 1, true
				}
				return 0, true // the alias points elsewhere: not this team
			}
			return NameSimilarity(name, fixName), false
		}
		sh, ah := side(home, nh, c.HomeID, c.Home)
		sa, aa := side(away, na, c.AwayID, c.Away)
		score := sh + sa
		// Prefer the closer kickoff on ties.
		score -= math.Abs(d.Minutes()) / 1000
		if sh >= 0.6 && sa >= 0.6 && score >= 1.4 && score > best.Score {
			best = MatchResult{FixtureID: c.FixtureID, Score: score, ByAlias: ah && aa}
		} else if best.FixtureID == 0 && score > 0.8 {
			best.Guess = c.Home + " v " + c.Away
		}
	}
	return best
}

// matchOddsEvents maps a sport's events to fixtures, learns aliases and
// records unmatched events.
func (s *Service) matchOddsEvents(sportKey string, comps []int64, events []OAEvent) map[string]int64 {
	out := map[string]int64{}
	if len(events) == 0 {
		return out
	}
	minT, maxT := time.Time{}, time.Time{}
	for _, e := range events {
		t, ok := parseTime(e.CommenceTime)
		if !ok {
			continue
		}
		if minT.IsZero() || t.Before(minT) {
			minT = t
		}
		if t.After(maxT) {
			maxT = t
		}
	}
	var rows []struct {
		ID          int64
		Kickoff     time.Time
		HomeID      int64
		AwayID      int64
		Home        string
		Away        string
		OddsEventID string
	}
	s.DB.Raw(`SELECT f.id, f.kickoff, f.home_id, f.away_id, h.name AS home, a.name AS away, COALESCE(f.odds_event_id,'') AS odds_event_id
		FROM fb_fixtures f JOIN fb_teams h ON h.id = f.home_id JOIN fb_teams a ON a.id = f.away_id
		WHERE f.competition_id IN ? AND f.kickoff BETWEEN ? AND ?`, comps, minT.Add(-MatchWindow), maxT.Add(MatchWindow)).Scan(&rows)
	cands := []Candidate{}
	byEvent := map[string]int64{}
	for _, r := range rows {
		cands = append(cands, Candidate{FixtureID: r.ID, Kickoff: r.Kickoff, HomeID: r.HomeID, AwayID: r.AwayID, Home: r.Home, Away: r.Away})
		if r.OddsEventID != "" {
			byEvent[r.OddsEventID] = r.ID
		}
	}
	aliases := s.aliasMap()
	now := s.Now()
	for _, e := range events {
		if f, ok := byEvent[e.ID]; ok {
			out[e.ID] = f
			continue
		}
		t, ok := parseTime(e.CommenceTime)
		if !ok {
			continue
		}
		m := MatchEvent(e.HomeTeam, e.AwayTeam, t, cands, aliases)
		if m.FixtureID == 0 {
			u := UnmatchedEvent{EventID: e.ID, SportKey: sportKey, Home: clip(e.HomeTeam, 160), Away: clip(e.AwayTeam, 160), Commence: t,
				BestGuess: clip(m.Guess, 300), Seen: 1, LastSeen: now}
			s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}},
				DoUpdates: clause.Assignments(map[string]any{"seen": gorm.Expr("fb_unmatched_events.seen + 1"), "last_seen": now, "best_guess": u.BestGuess})}).Create(&u)
			log.Printf("football: unmatched odds event %s %q v %q at %s (%s)", sportKey, e.HomeTeam, e.AwayTeam, t.Format(time.RFC3339), m.Guess)
			continue
		}
		out[e.ID] = m.FixtureID
		s.DB.Model(&Fixture{}).Where("id = ?", m.FixtureID).Update("odds_event_id", e.ID)
		s.DB.Where("event_id = ?", e.ID).Delete(&UnmatchedEvent{})
		// Learn the provider's spelling.
		for _, c := range cands {
			if c.FixtureID != m.FixtureID {
				continue
			}
			s.learnAlias(e.HomeTeam, c.HomeID)
			s.learnAlias(e.AwayTeam, c.AwayID)
		}
	}
	return out
}

func (s *Service) aliasMap() map[string]int64 {
	var rows []TeamAlias
	s.DB.Find(&rows)
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Alias] = r.TeamID
	}
	return out
}

func (s *Service) learnAlias(name string, team int64) {
	a := NormTeam(name)
	if a == "" {
		return
	}
	s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&TeamAlias{Alias: clip(a, 160), TeamID: team, Source: "auto", Name: clip(name, 160), CreatedAt: s.Now()})
}

// SetAlias adds or changes an alias by hand (super admin).
func (s *Service) SetAlias(name string, team int64) (TeamAlias, error) {
	a := NormTeam(name)
	if a == "" {
		return TeamAlias{}, inputErr("Type the team name as the odds provider writes it.")
	}
	var t Team
	if s.DB.Where("id = ?", team).Limit(1).Find(&t).RowsAffected == 0 {
		return TeamAlias{}, inputErr("Unknown team %d.", team)
	}
	row := TeamAlias{Alias: clip(a, 160), TeamID: team, Source: "manual", Name: clip(name, 160), CreatedAt: s.Now()}
	err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
	return row, err
}
