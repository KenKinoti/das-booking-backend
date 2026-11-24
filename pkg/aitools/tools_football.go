package aitools

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Football match analysis & bet tracker (pkg/football, /football/…).
// Probabilities are model estimates with their inputs and track record; the
// tools must never present a bet as certain. DASYIN does not take bets:
// football_record_bet only records a bet the person already placed with
// their bookmaker. It is marked destructive so the assistant always asks the
// user to confirm it first (even with auto-approve) and MCP clients see
// destructiveHint; the API also refuses bets over the person's own limits.
func init() {
	Register(Tool{
		Name:  "football_fixtures",
		Title: "Football fixtures",
		Description: "Lists football matches (enabled competitions: top European leagues, UEFA, Kenyan and African football, AFCON / World Cup qualifiers) " +
			"with kickoff, status and score, the model's win/draw/win probabilities, the best odds per outcome with the bookmaker, the de-margined market " +
			"probabilities and value flags (selections where the model's estimate is clearly above the market), DASYIN's one-line verdict (pick · chance · " +
			"tier, safer pick, draw alert) and, after full time, whether the pick frozen before kickoff was correct or wrong. Use for 'what games are on today?', " +
			"'Arsenal's next match', 'any value this weekend?'. Probabilities are estimates, never certainties. Then use football_match_analysis for one match.",
		InputSchema: tObj(map[string]any{
			"date":        tDate("Matches on this day (business time zone), YYYY-MM-DD; default the next days"),
			"competition": tInt("API-Football league id (see the competition ids in results)", 1, 100000),
			"status":      tEnum("Which matches (default all)", "upcoming", "live", "finished", "all"),
			"q":           tStr("Team or competition name contains"),
			"limit":       tLimit(),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballFixtures,
	})
	Register(Tool{
		Name:  "football_match_analysis",
		Title: "Football match analysis",
		Description: "Everything about one football match: league positions and position history, last 10 results of each team with xG, form (last 5/10, " +
			"home team at home, away team away, season), head-to-head, key players with availability (injured, suspended, doubtful, expected in the " +
			"line-up) and the missing-player impact, line-ups when published, the model (Dixon–Coles + Elo blend, expected goals, most likely scores, " +
			"over/under, both teams to score, Asian handicap), bookmaker odds with the de-margined consensus and best prices, value / expected value / " +
			"fractional Kelly stake per selection, live score and events when in play, the model's measured track record in this competition (Brier, " +
			"RPS, log loss vs the market) and the data sources with fetch times; also team news found in the news by keyword rules (each fact " +
			"'confirmed' or only 'reported (1 source)' — only confirmed injured/suspended players change the model), the latest headlines with " +
			"links, a cross-source stats check (agree / minor difference / conflict per stat), ClubElo's independent ratings and probabilities, and " +
			"Kenyan bookmakers' prices (e.g. Betika via OddsPapi) against the sharp fair price. Always mention 'data as of' and the sources, say " +
			"probabilities are estimates, say whether team news is confirmed or only reported, and never call a bet sure or guaranteed. Give " +
			"fixture_id (from football_fixtures) or a search like 'Arsenal v Chelsea'.",
		InputSchema: tObj(map[string]any{
			"fixture_id": tInt("Fixture id from football_fixtures", 1, 1<<53-1),
			"q":          tStr("Find the next match by team names, e.g. 'Gor Mahia' or 'Arsenal v Chelsea'"),
			"detail":     tEnum("How much to return (default summary)", "summary", "full"),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballAnalysis,
	})
	Register(Tool{
		Name:  "football_value_bets",
		Title: "Football value bets",
		Description: "Selections where the model's probability is at least the minimum edge (default 3 points) above the bookmakers' de-margined " +
			"probability (Pinnacle when available) and the best price has positive expected value, with the fractional Kelly stake (¼ Kelly, capped at " +
			"2% of the person's bankroll). These are model estimates against the market — show the edge, EV, odds and bookmaker, the model's track " +
			"record, and remind that value bets lose often; never present one as certain.",
		InputSchema: tObj(map[string]any{
			"date":     tDate("Only matches on this day (YYYY-MM-DD); default the next days"),
			"min_edge": tNum("Minimum edge as a probability difference, e.g. 0.05 for 5 points (default from settings, 0.03)"),
			"limit":    tLimit(),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballValue,
	})
	Register(Tool{
		Name:  "football_bets_summary",
		Title: "Football bets summary",
		Description: "The person's own recorded football bets: profit and loss, yield (profit ÷ staked), ROI, strike rate, average odds, closing-line " +
			"value (their odds vs the closing price, the best sign of skill), breakdowns by market, bookmaker, competition and month, bankroll and " +
			"drawdown, open bets, and their limits / break status. Use for 'how are my bets doing?', 'am I beating the closing line?'.",
		InputSchema: tObj(map[string]any{
			"from": tDate("Bets placed from this day (YYYY-MM-DD)"),
			"to":   tDate("Bets placed before this day (YYYY-MM-DD)"),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballSummary,
	})
	Register(Tool{
		Name:  "football_record_bet",
		Title: "Record a football bet",
		Description: "Records a bet the person ALREADY placed with their own bookmaker (DASYIN never places bets or takes money). Markets: 1x2 " +
			"(home/draw/away), dc double chance (1x/12/x2), dnb draw no bet (home/away), ou total goals (over/under + line, e.g. 2.5 or 2.25), btts " +
			"(yes/no), ah Asian handicap (home/away + the line for that team, quarter lines allowed). Odds are decimal; the stake is in the " +
			"organisation's currency. Always read the details back to the user and get their confirmation first. Refused with a clear message when " +
			"it would break the person's own stake or loss limit, or during a break they chose. It settles itself after full time.",
		InputSchema: tObj(map[string]any{
			"fixture_id": tInt("Fixture id from football_fixtures", 1, 1<<53-1),
			"market":     tEnum("Market", "1x2", "dc", "dnb", "ou", "btts", "ah"),
			"selection":  tEnum("Selection", "home", "draw", "away", "1x", "12", "x2", "over", "under", "yes", "no"),
			"line":       tNum("Line for ou (goals, e.g. 2.5) or ah (handicap for the selected team, e.g. -0.25)"),
			"odds":       tNum("Decimal odds taken, e.g. 2.10"),
			"stake":      tNum("Stake in the organisation's currency"),
			"bookmaker":  tStr("Where it was placed, e.g. SportPesa, Betika, Odibets, Betway, 1xBet"),
			"placed_at":  tStr("When it was placed (RFC3339 or local YYYY-MM-DDTHH:MM); default now"),
			"notes":      tStr("Optional notes"),
		}, "fixture_id", "market", "selection", "odds", "stake", "bookmaker"),
		Destructive: true,
		Module:      "football",
		Handler:     footballRecordBet,
	})
}

// fbID formats a JSON id (decoded as float64) without an exponent.
func fbID(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func fbFixtureCard(f map[string]any) map[string]any {
	home, away := tObjOf(f["home"]), tObjOf(f["away"])
	comp := tObjOf(f["competition"])
	m := map[string]any{"fixture_id": f["id"], "kickoff": f["kickoff"], "status": f["status"], "competition": tS(comp, "name"), "competition_id": comp["id"],
		"home": tS(home, "name"), "away": tS(away, "name"), "url": tURL("/football/match/" + fbID(f["id"]))}
	if sc := tObjOf(f["score"]); sc != nil && sc["home"] != nil {
		m["score"] = fmt.Sprintf("%v-%v", sc["home"], sc["away"])
	}
	if f["elapsed"] != nil {
		m["minute"] = f["elapsed"]
	}
	if mo := tObjOf(f["model"]); mo != nil {
		m["model_p"] = tPick(mo, "home", "draw", "away")
	}
	if mk := tObjOf(f["market_1x2"]); len(mk) > 0 {
		m["market_p"] = mk
		m["market_source"] = f["market_source"]
	}
	best := map[string]any{}
	for k, v := range tObjOf(f["best_odds"]) {
		if b := tObjOf(v); b != nil {
			best[k] = fmt.Sprintf("%v (%s)", b["price"], tS(b, "title"))
		}
	}
	if len(best) > 0 {
		m["best_odds"] = best
	}
	if n := tF(f, "value_count"); n > 0 {
		edges := []map[string]any{}
		for _, e := range tList(f["edges"]) {
			edges = append(edges, tPick(e, "label", "model_p", "market_p", "edge", "best_price", "bookmaker", "ev"))
		}
		m["value"] = edges
	}
	// v2.18: the one-line verdict and the ✓/✗ (tools_football_win.go).
	if v := tObjOf(f["verdict"]); v != nil {
		m["verdict"] = fbVerdictLine(v)
	}
	if rc := tObjOf(f["result_check"]); rc != nil {
		m["result_check"] = fbCheckLine(rc)
	}
	// v2.19.1: no pick without real data (and why); when the pick was locked.
	if np := tObjOf(f["no_pick"]); np != nil {
		m["no_pick"] = tS(np, "label") + ": " + tS(np, "reason")
	}
	if lk := tObjOf(f["lock"]); lk != nil && tS(lk, "label") != "" {
		m["locked"] = tS(lk, "label")
	}
	if n := tS(f, "model_note"); n != "" {
		m["model_note"] = n
	}
	return m
}

func footballFixtures(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"date", "status", "q"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	if n := argInt(a, "competition", 0); n > 0 {
		q.Set("competition", strconv.Itoa(n))
	}
	lim := argLimit(a)
	q.Set("limit", strconv.Itoa(lim))
	v, err := c.get("/football/fixtures", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	items := []map[string]any{}
	for _, f := range tList(d["fixtures"]) {
		items = append(items, fbFixtureCard(f))
	}
	return map[string]any{"fixtures": items, "count": len(items), "timezone": d["timezone"], "data_as_of": d["last_run_at"], "url": tURL("/football"),
		"note": "Model probabilities are estimates (Dixon–Coles + Elo) — check football_match_analysis for inputs, sources and track record. " + tS(d, "disclaimer")}, nil
}

// findFixture resolves q ("Team A v Team B" or a team) to the next match.
func findFixture(c *Ctx, q string) (string, error) {
	parts := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return r == ',' })
	q = strings.TrimSpace(strings.Join(parts, " "))
	sides := []string{q}
	for _, sep := range []string{" vs. ", " vs ", " v ", " - ", " against "} {
		if i := strings.Index(q, sep); i > 0 {
			sides = []string{strings.TrimSpace(q[:i]), strings.TrimSpace(q[i+len(sep):])}
			break
		}
	}
	v, err := c.get("/football/fixtures", url.Values{"q": {sides[0]}, "limit": {"60"}})
	if err != nil {
		return "", err
	}
	for _, f := range tList(tObjOf(v)["fixtures"]) {
		h, aw := strings.ToLower(tS(tObjOf(f["home"]), "name")), strings.ToLower(tS(tObjOf(f["away"]), "name"))
		if len(sides) == 2 && !(strings.Contains(h+" "+aw, sides[1])) {
			continue
		}
		if st := tS(f, "status"); st == "FT" || st == "AET" || st == "PEN" {
			continue
		}
		return fbID(f["id"]), nil
	}
	return "", tErr("No upcoming match found for %q. Use football_fixtures to find it.", q)
}

func footballAnalysis(c *Ctx, a map[string]any) (any, error) {
	id := ""
	if n := argInt(a, "fixture_id", 0); n > 0 {
		id = strconv.Itoa(n)
	} else if q := strings.TrimSpace(argStr(a, "q")); q != "" {
		var err error
		if id, err = findFixture(c, q); err != nil {
			return nil, err
		}
	} else {
		return nil, tErr("Give fixture_id or q (team names).")
	}
	v, err := c.get("/football/fixtures/"+id+"/analysis", nil)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	if argStr(a, "detail") == "full" {
		d["url"] = tURL("/football/match/" + id)
		return d, nil
	}
	fx := tObjOf(d["fixture"])
	res := map[string]any{"fixture": map[string]any{"id": fx["id"], "kickoff": fx["kickoff"], "status": fx["status"], "elapsed": fx["elapsed"],
		"competition": tS(tObjOf(fx["competition"]), "name"), "round": tObjOf(fx["competition"])["round"], "venue": fx["venue"],
		"home": tS(tObjOf(fx["home"]), "name"), "away": tS(tObjOf(fx["away"]), "name"), "score": fx["score"]},
		"as_of": d["as_of"], "stale": d["stale"], "stale_items": d["stale_items"], "disclaimer": d["disclaimer"], "url": tURL("/football/match/" + id)}
	if st := tObjOf(d["standings"]); st != nil {
		res["standings"] = map[string]any{"home": tPick(tObjOf(st["home"]), "rank", "points", "played", "gd", "form", "description"),
			"away": tPick(tObjOf(st["away"]), "rank", "points", "played", "gd", "form", "description"), "table_size": st["table_size"]}
	}
	form := tObjOf(d["form"])
	pick := func(m map[string]any, keys ...string) map[string]any {
		out := map[string]any{}
		for _, k := range keys {
			if x := tObjOf(m[k]); x != nil {
				out[k] = tPick(x, "n", "w", "d", "l", "ppg", "gf", "ga", "xgf", "xga", "sequence", "win_rate")
			}
		}
		return out
	}
	res["form"] = map[string]any{"home": pick(tObjOf(form["home"]), "last5", "last10", "home", "season"), "away": pick(tObjOf(form["away"]), "last5", "last10", "away", "season")}
	if h := tObjOf(d["h2h"]); h != nil {
		res["h2h"] = h["summary"]
	}
	pl := tObjOf(d["players"])
	missing := func(side string) []map[string]any {
		out := []map[string]any{}
		for _, p := range tList(tObjOf(pl[side])["missing"]) {
			out = append(out, tPick(p, "name", "position", "availability", "reason", "season.goals", "season.assists"))
		}
		return out
	}
	res["missing_players"] = map[string]any{"home": missing("home"), "away": missing("away"),
		"home_impact": tObjOf(pl["home"])["impact"], "away_impact": tObjOf(pl["away"])["impact"]}
	if lu := tObjOf(d["lineups"]); lu != nil {
		res["lineups_published"] = lu["published"]
	}
	if m := tObjOf(d["model"]); m != nil {
		res["model"] = tPick(m, "kind", "frozen_at", "blend", "weights", "dc.lambda_home", "dc.lambda_away", "elo.rating_home", "elo.rating_away", "top_scores",
			"over_under", "btts", "inputs", "note")
	}
	mk := tObjOf(d["market"])
	sel := []map[string]any{}
	for _, s := range tList(mk["selections"]) {
		mkt := tS(s, "market")
		line := fmt.Sprint(s["line"])
		if mkt == "1x2" || (mkt == "ou" && line == "2.5") || mkt == "btts" {
			x := tPick(s, "label", "fair_p", "fair_source", "books")
			if b := tObjOf(s["best"]); b != nil {
				x["best"] = fmt.Sprintf("%v (%s)", b["price"], tS(b, "title"))
			}
			sel = append(sel, x)
		}
	}
	res["market"] = map[string]any{"selections": sel, "books": mk["books"], "fair_source": mk["fair_source"], "as_of": mk["as_of"]}
	vals := []map[string]any{}
	for _, v := range tList(d["value"]) {
		if v["value"] == true {
			vals = append(vals, tPick(v, "label", "model_p", "market_p", "edge", "best_price", "bookmaker", "ev", "kelly_fraction", "kelly_stake", "currency"))
		}
	}
	res["value"] = vals
	if lv := tObjOf(d["live"]); lv != nil {
		ev := []map[string]any{}
		for _, e := range tList(lv["events"]) {
			ev = append(ev, tPick(e, "minute", "side", "type", "detail", "player"))
		}
		res["live"] = map[string]any{"status": lv["status"], "minute": lv["minute"], "score": lv["score"], "events": ev}
	}
	res["track_record"] = tPick(tObjOf(d["track_record"]), "n", "brier", "rps", "log_loss", "market_brier", "market_rps", "market_log_loss", "note")
	srcs := []map[string]any{}
	for _, s := range tList(d["sources"]) {
		srcs = append(srcs, tPick(s, "provider", "object", "fetched_at", "updated_at", "stale"))
	}
	res["sources"] = srcs
	footballFreeSummary(d, res)
	return res, nil
}

func footballValue(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := strings.TrimSpace(argStr(a, "date")); s != "" {
		q.Set("date", s)
	}
	if n, ok := argNum(a, "min_edge"); ok && n > 0 {
		q.Set("min_edge", strconv.FormatFloat(n, 'f', -1, 64))
	}
	v, err := c.get("/football/value", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	items := []map[string]any{}
	for i, x := range tList(d["value"]) {
		if i >= argLimit(a) {
			break
		}
		m := tPick(x, "fixture_id", "kickoff", "competition", "home", "away", "label", "model_p", "market_p", "market_source", "edge", "best_price", "bookmaker",
			"ev", "kelly_fraction", "kelly_stake", "currency")
		m["url"] = tURL("/football/match/" + fbID(x["fixture_id"]))
		items = append(items, m)
	}
	perf, _ := c.get("/football/model/performance", nil)
	return map[string]any{"value": items, "count": d["count"], "min_edge": d["min_edge"], "note": d["note"], "disclaimer": d["disclaimer"],
		"model_track_record": tPick(tObjOf(perf), "n", "rps", "market_rps", "brier", "market_brier", "note")}, nil
}

func footballSummary(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"from", "to"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	v, err := c.get("/football/bets/summary", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	res := tPick(d, "currency", "bets", "open", "open_stake", "settled", "won", "lost", "void", "half_won", "half_lost", "cashed_out", "staked", "returned",
		"pnl", "yield", "roi", "strike_rate", "avg_odds", "avg_clv", "beat_closing_rate", "model_expected_pnl", "note")
	for _, k := range []string{"by_market", "by_bookmaker", "by_competition"} {
		rows := []map[string]any{}
		for _, r := range tList(d[k]) {
			rows = append(rows, tPick(r, "label", "bets", "staked", "pnl", "yield", "strike_rate", "avg_clv"))
		}
		res[k] = rows
	}
	br := tObjOf(d["bankroll"])
	res["bankroll"] = tPick(br, "balance", "available", "start", "deposits", "withdrawals", "max_drawdown", "max_drawdown_pct", "current_drawdown")
	lim := tObjOf(d["limits"])
	res["limits"] = map[string]any{"on_break": lim["on_break"], "cool_off_until": lim["cool_off_until"]}
	open, err := c.get("/football/bets", url.Values{"status": {"open"}, "limit": {"10"}})
	if err == nil {
		ob := []map[string]any{}
		for _, b := range tList(tObjOf(open)["bets"]) {
			ob = append(ob, tPick(b, "id", "label", "home.name:home", "away.name:away", "kickoff", "bookmaker", "odds", "stake", "potential_return"))
		}
		res["open_bets"] = ob
	}
	res["url"] = tURL("/football/bets")
	return res, nil
}

func footballRecordBet(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "fixture_id", "market", "selection", "odds", "stake", "bookmaker"); err != nil {
		return nil, err
	}
	body := map[string]any{"fixture_id": argInt(a, "fixture_id", 0), "market": argStr(a, "market"), "selection": argStr(a, "selection"),
		"bookmaker": argStr(a, "bookmaker"), "notes": argStr(a, "notes")}
	for _, k := range []string{"odds", "stake", "line"} {
		if n, ok := argNum(a, k); ok {
			body[k] = n
		}
	}
	if s := strings.TrimSpace(argStr(a, "placed_at")); s != "" {
		t, err := c.parseWhen(s)
		if err != nil {
			return nil, err
		}
		body["placed_at"] = t
	}
	v, err := c.api("POST", "/football/bets", nil, body)
	if err != nil {
		return nil, err
	}
	b := tObjOf(v)
	out := tPick(b, "id", "label", "market_label", "home.name:home", "away.name:away", "kickoff", "bookmaker", "odds", "stake", "currency", "potential_return",
		"model_p", "market_p", "status")
	out["url"] = tURL("/football/bets")
	out["note"] = "Recorded. It settles automatically after full time; closing-line value is added at kickoff."
	return out, nil
}
