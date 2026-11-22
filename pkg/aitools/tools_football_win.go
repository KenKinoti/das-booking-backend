package aitools

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Football v2.18 tools: the scorecard (how DASYIN's frozen picks did, with
// the normal range their own chances implied) and the upcoming picks
// strongest first. Wording: estimates, 18+, never "sure", "banker",
// "guaranteed" or "fixed"; DASYIN takes no bets.
func init() {
	Register(Tool{
		Name:  "football_scorecard",
		Title: "Football picks scorecard",
		Description: "How DASYIN's football picks did: each match's pick is frozen before kickoff and marked ✓/✗ after full time. Returns how many " +
			"picks were right, how many their own chances said (expected, with the normal 10–90% range), a plain-English verdict (bad luck / " +
			"below the normal range …), the safer double-chance pick, goals over/under 2.5, both teams to score and exact scores, the bookmakers' " +
			"favourite on the same matches, results by tier (Strong / Good / Close call / Coin-flip, with the open-history hit rate of each tier), " +
			"by competition, per week, the person's graded jackpots and the latest rows. Period: weekend (the latest Fri–Mon, default), 7d, 30d, " +
			"season, or custom from/to. Always say these are estimates and that coin-flip games are coin-flips.",
		InputSchema: tObj(map[string]any{
			"period":      tEnum("Which period (default weekend)", "weekend", "7d", "30d", "season", "custom"),
			"from":        tDate("Custom period start (YYYY-MM-DD)"),
			"to":          tDate("Custom period end, included (YYYY-MM-DD)"),
			"competition": tInt("Only this competition (API-Football league id)", 1, 100000),
			"tier":        tEnum("Only this tier", "strong", "good", "close", "coinflip"),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballScorecard,
	})
	Register(Tool{
		Name:  "football_picks",
		Title: "Football picks (strongest first)",
		Description: "Upcoming football matches with DASYIN's one-line verdict, strongest first: the pick ('Arsenal win · 64% · Strong'), its tier " +
			"and how often that tier's pick won in open history (Strong ≈ 72%, Good ≈ 56%, Close call ≈ 46%, Coin-flip ≈ 38%) and in DASYIN's own " +
			"graded record, the safer double chance, a draw alert, goals and both-teams-to-score leans and the top scores. For a combo, multiply " +
			"the chances (games assumed independent). Estimates only — never call a pick sure.",
		InputSchema: tObj(map[string]any{
			"from":     tDate("From this day (YYYY-MM-DD); default now"),
			"to":       tDate("Up to this day, included (YYYY-MM-DD); default the next days"),
			"min_tier": tEnum("Only picks at least this strong", "strong", "good", "close", "coinflip"),
			"limit":    tLimit(),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballPicks,
	})
}

// fbVerdictLine condenses a verdict ("Arsenal win · 64% · Strong").
func fbVerdictLine(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	tier := tObjOf(v["tier"])
	out := map[string]any{"pick": v["pick"], "line": fmt.Sprintf("%s · %.0f%% · %s", tS(v, "pick_label"), tF(v, "p")*100, tS(tier, "label")), "p": v["p"],
		"tier": tS(tier, "key")}
	if h := tObjOf(tier["history"]); h != nil {
		out["tier_history_hit_rate"] = h["hit_rate"]
	}
	if own := tObjOf(tier["own"]); own != nil {
		out["tier_own_hit_rate"] = own["hit_rate"]
		out["tier_own_n"] = own["n"]
	}
	if n := tS(tier, "note"); n != "" {
		out["note"] = n
	}
	if sf := tObjOf(v["safer"]); sf != nil {
		out["safer"] = fmt.Sprintf("%s · %.0f%%", tS(sf, "label"), tF(sf, "p")*100)
	}
	if da := tObjOf(v["draw_alert"]); da != nil && da["on"] == true {
		out["draw_alert"] = tS(da, "why")
	}
	if g := tObjOf(v["goals"]); g != nil {
		out["goals"] = fmt.Sprintf("%s %.1f · %.0f%%", tS(g, "pick"), tF(g, "line"), tF(g, "p")*100)
	}
	if b := tObjOf(v["btts"]); b != nil {
		out["btts"] = fmt.Sprintf("%s · %.0f%%", tS(b, "pick"), tF(b, "p")*100)
	}
	scores := []string{}
	for _, s := range tList(v["top_scores"]) {
		scores = append(scores, fmt.Sprintf("%s (%.0f%%)", tS(s, "score"), tF(s, "p")*100))
	}
	if len(scores) > 0 {
		out["top_scores"] = strings.Join(scores, ", ")
	}
	out["kind"] = v["kind"]
	return out
}

// fbCheckLine condenses a result check.
func fbCheckLine(rc map[string]any) map[string]any {
	if rc == nil {
		return nil
	}
	return tPick(rc, "label", "score", "summary")
}

func footballScorecard(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"period", "from", "to", "tier"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	if n := argInt(a, "competition", 0); n > 0 {
		q.Set("competition", strconv.Itoa(n))
	}
	v, err := c.get("/football/scorecard", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	rows := []map[string]any{}
	for i, r := range tList(d["rows"]) {
		if i >= 20 {
			break
		}
		x := tPick(r, "fixture_id", "kickoff", "home", "away", "pick_label", "p", "tier", "score", "status", "summary")
		x["url"] = tURL("/football/match/" + fbID(r["fixture_id"]))
		rows = append(rows, x)
	}
	tiers := []map[string]any{}
	for _, t := range tList(d["by_tier"]) {
		tiers = append(tiers, tPick(t, "label", "graded", "correct", "hit_rate", "expected_rate", "history_hit_rate"))
	}
	return map[string]any{"period": d["period"], "totals": d["totals"], "safer": d["safer"], "goals": d["goals"], "btts": d["btts"],
		"exact_scores": d["exact_scores"], "favourite_baseline": d["favourite_baseline"], "by_tier": tiers, "jackpots": d["jackpots"], "rows": rows,
		"history": d["history"], "note": d["note"], "url": tURL("/football/scorecard"),
		"disclaimer": "Estimates, not certainties: picks are frozen before kickoff and never regraded. 18+; DASYIN takes no bets."}, nil
}

func footballPicks(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"limit": {strconv.Itoa(argLimit(a))}}
	for _, k := range []string{"from", "to", "min_tier"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	v, err := c.get("/football/picks", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	items := []map[string]any{}
	for _, p := range tList(d["picks"]) {
		comp := tObjOf(p["competition"])
		items = append(items, map[string]any{"fixture_id": p["fixture_id"], "kickoff": p["kickoff"], "competition": tS(comp, "name"),
			"home": tS(tObjOf(p["home"]), "name"), "away": tS(tObjOf(p["away"]), "name"), "verdict": fbVerdictLine(tObjOf(p["verdict"])),
			"url": tURL("/football/match/" + fbID(p["fixture_id"]))})
	}
	return map[string]any{"picks": items, "count": len(items), "accumulator": d["accumulator"], "url": tURL("/football"),
		"disclaimer": "Estimates, not certainties — even Strong picks lose about 3 times in 10. 18+; DASYIN takes no bets."}, nil
}
