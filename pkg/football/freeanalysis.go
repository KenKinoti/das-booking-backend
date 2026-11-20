package football

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Match page additions (v2.17): team news, news, source check, ClubElo and
// Kenyan bookmakers. When this organisation's managers confirmed or
// dismissed team news that changes the missing players, the model section
// is recomputed for this organisation's view (frozen predictions and the
// track record never change).

// TeamNewsSection is the analysis' team news.
type TeamNewsSection struct {
	Home   []TeamNewsOut `json:"home"`
	Away   []TeamNewsOut `json:"away"`
	Counts struct {
		Confirmed   int `json:"confirmed"`
		Reported    int `json:"reported"`
		UsedInModel int `json:"used_in_model"`
	} `json:"counts"`
	CanReview  bool   `json:"can_review"`
	ExpiryDays int    `json:"expiry_days"`
	RulesNote  string `json:"rules_note"`
}

// NewsSection is the analysis' news list.
type NewsSection struct {
	Items     []NewsOut      `json:"items"`
	Total     int64          `json:"total"`
	Providers map[string]int `json:"providers"`
	FetchedAt *time.Time     `json:"fetched_at"`
	Note      string         `json:"note"`
}

// TeamNewsCount is the fixtures list's team-news flag: confirmed, not
// dismissed, unexpired injured / suspended facts seen in the last 7 days
// per side (this organisation's view).
type TeamNewsCount struct {
	Home int `json:"home"`
	Away int `json:"away"`
}

// addTeamNewsCounts fills the flag on fixture rows (one query to find the
// teams with recent facts; confidence only for those).
func (s *Service) addTeamNewsCounts(rows []FixtureRow, org string) {
	if len(rows) == 0 {
		return
	}
	now := s.Now()
	teams := []int64{}
	for _, r := range rows {
		teams = append(teams, r.Home.ID, r.Away.ID)
	}
	var with []int64
	s.DB.Model(&TeamNewsFact{}).Where("team_id IN ? AND expires_at > ? AND last_seen > ? AND status IN ?", teams, now, now.AddDate(0, 0, -7), []string{TNInjured, TNSuspended}).
		Distinct("team_id").Pluck("team_id", &with)
	n := map[int64]int{}
	for _, t := range with {
		for _, f := range s.activeFacts(t, org) {
			if f.confirmed && !f.dismissed && (f.Status == TNInjured || f.Status == TNSuspended) && f.LastSeen.After(now.AddDate(0, 0, -7)) {
				n[t]++
			}
		}
	}
	for i := range rows {
		rows[i].TeamNews = &TeamNewsCount{Home: n[rows[i].Home.ID], Away: n[rows[i].Away.ID]}
	}
}

// TeamNewsFor builds a fixture's team news for an organisation's view.
func (s *Service) TeamNewsFor(f Fixture, org string) *TeamNewsSection {
	st := s.FreeSettingsGet()
	out := &TeamNewsSection{Home: []TeamNewsOut{}, Away: []TeamNewsOut{}, ExpiryDays: st.NewsExpiryDays,
		RulesNote: "Found with keyword rules (no AI). Only confirmed facts — in API-Football's injury list, reported by 2 independent sites within 7 days, or confirmed by a manager — change the model."}
	rel := s.reliabilityMap()
	for _, sd := range []struct {
		side string
		team int64
	}{{"home", f.HomeID}, {"away", f.AwayID}} {
		for _, fs := range s.activeFacts(sd.team, org) {
			o := s.factOut(fs, sd.side, rel)
			if sd.side == "home" {
				out.Home = append(out.Home, o)
			} else {
				out.Away = append(out.Away, o)
			}
			if o.Dismissed {
				continue
			}
			if o.Confidence == "confirmed" {
				out.Counts.Confirmed++
			} else {
				out.Counts.Reported++
			}
			if o.UsedInModel {
				out.Counts.UsedInModel++
			}
		}
	}
	order := func(xs []TeamNewsOut) {
		sort.SliceStable(xs, func(i, j int) bool {
			if xs[i].Dismissed != xs[j].Dismissed {
				return !xs[i].Dismissed
			}
			if (xs[i].Confidence == "confirmed") != (xs[j].Confidence == "confirmed") {
				return xs[i].Confidence == "confirmed"
			}
			return xs[i].LastSeen.After(xs[j].LastSeen)
		})
	}
	order(out.Home)
	order(out.Away)
	return out
}

// NewsSectionFor lists a fixture's news.
func (s *Service) NewsSectionFor(f Fixture) *NewsSection {
	items, total := s.NewsFor([]int64{f.HomeID, f.AwayID}, "", 40)
	out := &NewsSection{Items: items, Total: total, Providers: map[string]int{ProviderGNews: 0, ProviderGDELT: 0, "feed": 0},
		Note: "Headlines, short snippets and links only — open the link to read the article on the publisher's site."}
	for _, it := range items {
		k := it.Provider
		if k == ProviderFeeds {
			k = "feed"
		}
		out.Providers[k]++
		t := it.FetchedAt
		out.FetchedAt = latestTime(out.FetchedAt, &t)
	}
	for i := range out.Items {
		if out.Items[i].Provider == ProviderFeeds {
			out.Items[i].Provider = "feed"
		}
	}
	return out
}

func (s *Service) freeSections(a *Analysis, f Fixture, org, user string, upcoming bool, addSrc func(SourceOut)) {
	now := s.Now()
	// Team news and news.
	a.TeamNews = s.TeamNewsFor(f, org)
	a.News = s.NewsSectionFor(f)
	for _, it := range a.News.Items {
		t := it.PublishedAt
		prov := it.Provider
		if prov == "feed" {
			prov = ProviderFeeds
		}
		addSrc(SourceOut{Provider: prov, Object: "news: " + clip(it.Title, 90), ID: it.Domain, FetchedAt: &it.FetchedAt, UpdatedAt: &t, URL: it.URL, Note: it.Source})
	}
	// Organisation-adjusted model.
	if a.Model != nil && org != "" && upcoming && s.orgChangesMissing(f, org) {
		if p, err := s.computeOrg(f, org); err == nil {
			m := modelOut(p)
			m.OrgAdjusted = true
			m.OrgNote = "Recomputed for your organisation with the team news your managers confirmed or dismissed. The shared prediction and its track record are unchanged."
			a.Model = m
			st, _ := s.Settings()
			bal := 0.0
			if a.Bankroll.Balance != nil {
				bal = *a.Bankroll.Balance
			}
			a.Value = valueRows(p, a.Market, st.MinEdge, st.KellyFraction, st.KellyCap, bal, a.Bankroll.Currency)
		}
	}
	// Source check.
	sc := s.SourceCheckFor(f)
	a.SourceCheck = &sc
	// ClubElo.
	ce := s.clubEloFor(f)
	a.ClubElo = &ce
	if ce.Available {
		addSrc(SourceOut{Provider: ProviderClubElo, Object: "ClubElo ratings" + map[bool]string{true: " + fixture probabilities", false: ""}[ce.ProbsSource == "clubelo_fixtures"],
			Endpoint: "GET /YYYY-MM-DD, /Fixtures", FetchedAt: ce.FetchedAt, URL: ce.URL, Stale: upcoming && ce.FetchedAt != nil && now.Sub(*ce.FetchedAt) > 3*24*time.Hour})
	}
	// Kenyan bookmakers.
	ko := s.kenyanOddsFor(f, a.Market)
	a.KenyanOdds = &ko
	// Other free sources behind the source check.
	var fo FDOrgStanding
	if code := fdOrgCodeOf(f.CompetitionID); code != "" && s.DB.Where("competition = ?", code).Order("fetched_at DESC").Limit(1).Find(&fo).RowsAffected > 0 {
		t := fo.FetchedAt
		addSrc(SourceOut{Provider: ProviderFDOrg, Object: "football-data.org table and matches (cross-check)", ID: code, Endpoint: "GET /v4/competitions/" + code + "/standings",
			FetchedAt: &t, URL: docFDOrg})
	}
	var hm HistMatch
	if s.DB.Where("(home_id IN ? OR away_id IN ?)", []int64{f.HomeID, f.AwayID}, []int64{f.HomeID, f.AwayID}).Order("fetched_at DESC").Limit(1).Find(&hm).RowsAffected > 0 {
		t := hm.FetchedAt
		addSrc(SourceOut{Provider: ProviderFDCOUK, Object: "results history and closing odds", ID: hm.Div, Endpoint: "CSV " + hm.Div, FetchedAt: &t,
			URL: "https://www.football-data.co.uk/data.php", Note: "Data: football-data.co.uk"})
	}
}

// orgChangesMissing: does this organisation's review change the missing
// players of either team?
func (s *Service) orgChangesMissing(f Fixture, org string) bool {
	var n int64
	s.DB.Raw(`SELECT COUNT(*) FROM fb_team_news_reviews r JOIN fb_team_news t ON t.id = r.fact_id WHERE r.organization_id = ? AND t.team_id IN ? AND t.expires_at > ?`,
		org, []int64{f.HomeID, f.AwayID}, s.Now()).Scan(&n)
	if n == 0 {
		return false
	}
	for _, team := range []int64{f.HomeID, f.AwayID} {
		a, b := s.newsMissing(f, team, ""), s.newsMissing(f, team, org)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			return true
		}
	}
	return false
}

// canReviewRole: owners, admins and managers review team news (and super admins).
func canReviewRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "super_admin", "admin", "owner", "manager":
		return true
	}
	return false
}
