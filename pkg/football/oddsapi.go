package football

// The Odds API v4 client shapes.
//
// Docs: https://the-odds-api.com/liveapi/guides/v4/
//   - Base URL https://api.the-odds-api.com, key in the apiKey query parameter.
//   - GET /v4/sports (free) → [{key, group, title, description, active, has_outrights}];
//     soccer keys are listed at https://the-odds-api.com/sports-odds-data/sports-apis.html
//     (no Kenyan league; Africa only soccer_africa_cup_of_nations).
//   - GET /v4/sports/{sport}/odds?regions=uk,eu&markets=h2h,totals,spreads&oddsFormat=decimal
//     → [{id, sport_key, commence_time, home_team, away_team, bookmakers:[{key, title,
//     last_update, markets:[{key, last_update, outcomes:[{name, price, point?}]}]}]}].
//     Soccer h2h is 3-way (an outcome named "Draw"); spreads are handicaps per
//     team (point), totals are Over/Under with point.
//   - Cost = markets × regions per call; calls returning no events cost
//     nothing; headers x-requests-remaining, x-requests-used, x-requests-last.
//   - Errors: https://the-odds-api.com/liveapi/guides/v4/api-error-codes.html
//     (OUT_OF_USAGE_CREDITS, EXCEEDED_FREQ_LIMIT = 429, INVALID_KEY …).
//   - Bookmakers by region: https://the-odds-api.com/sports-odds-data/bookmaker-apis.html
//     — Pinnacle is in "eu" only; 1xBet (onexbet) in "eu", Betway in "uk";
//     SportPesa, Betika, Odibets and Mozzartbet are not covered (manual entry).

import (
	"strings"
	"time"
)

// OASport is an entry of GET /v4/sports.
type OASport struct {
	Key          string `json:"key"`
	Group        string `json:"group"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Active       bool   `json:"active"`
	HasOutrights bool   `json:"has_outrights"`
}

// OAEvent is an event of GET /v4/sports/{sport}/odds.
type OAEvent struct {
	ID           string `json:"id"`
	SportKey     string `json:"sport_key"`
	CommenceTime string `json:"commence_time"`
	HomeTeam     string `json:"home_team"`
	AwayTeam     string `json:"away_team"`
	Bookmakers   []struct {
		Key        string `json:"key"`
		Title      string `json:"title"`
		LastUpdate string `json:"last_update"`
		Markets    []struct {
			Key        string `json:"key"`
			LastUpdate string `json:"last_update"`
			Outcomes   []struct {
				Name  string   `json:"name"`
				Price float64  `json:"price"`
				Point *float64 `json:"point"`
			} `json:"outcomes"`
		} `json:"markets"`
	} `json:"bookmakers"`
}

// oddsRowsFromOA converts an event's prices into odds rows for fixture id.
func oddsRowsFromOA(fixtureID int64, ev OAEvent, now time.Time) []Odd {
	out := []Odd{}
	for _, b := range ev.Bookmakers {
		bk := bookmakerSlug(b.Title)
		if bk == "" {
			bk = bookmakerSlug(b.Key)
		}
		for _, m := range b.Markets {
			upd, ok := parseTime(m.LastUpdate)
			if !ok {
				upd, ok = parseTime(b.LastUpdate)
			}
			var updp *time.Time
			if ok {
				u := upd
				updp = &u
			}
			for _, o := range m.Outcomes {
				if o.Price <= 1 {
					continue
				}
				r := Odd{FixtureID: fixtureID, Source: ProviderOddsAPI, Bookmaker: bk, BookmakerTitle: b.Title, Price: o.Price,
					ProviderUpdatedAt: updp, FetchedAt: now, CheckedAt: now}
				name := strings.TrimSpace(o.Name)
				switch m.Key {
				case "h2h":
					r.Market = Mkt1X2
					switch {
					case strings.EqualFold(name, "Draw"):
						r.Selection = SelDraw
					case name == ev.HomeTeam:
						r.Selection = SelHome
					case name == ev.AwayTeam:
						r.Selection = SelAway
					default:
						continue
					}
				case "totals":
					if o.Point == nil {
						continue
					}
					r.Market, r.Line = MktOU, ptr(*o.Point)
					switch strings.ToLower(name) {
					case "over":
						r.Selection = SelOver
					case "under":
						r.Selection = SelUnder
					default:
						continue
					}
				case "spreads":
					if o.Point == nil {
						continue
					}
					r.Market, r.Line = MktAH, ptr(*o.Point)
					switch name {
					case ev.HomeTeam:
						r.Selection = SelHome
					case ev.AwayTeam:
						r.Selection = SelAway
					default:
						continue
					}
				default:
					continue
				}
				out = append(out, r)
			}
		}
	}
	return out
}

func ptr[T any](v T) *T { return &v }
