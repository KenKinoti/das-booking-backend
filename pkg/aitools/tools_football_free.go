package aitools

import (
	"fmt"
	"net/url"
	"strings"
)

// Football v2.17 tools: the jackpot analyser (read-only: it analyses pasted
// games or a saved jackpot and saves nothing) and the person's saved
// jackpots, plus the team news / source check / ClubElo / Kenyan odds
// summary added to football_match_analysis. Jackpot chances are estimates;
// the tools must never present a ticket as likely to win or "sure".
func init() {
	Register(Tool{
		Name:  "football_jackpot_analyse",
		Title: "Analyse a football jackpot",
		Description: "Analyses a 13–17 game 1X2 jackpot (SportPesa / Betika / Odibets style: one prize shared by all winners, bonuses for some " +
			"correct, combinations = product of selections per game). Give the pasted games as text (one game per line, e.g. 'Piast Gliwice v " +
			"Korona Kielce 2.44 3.35 2.55'; numbering, dates and capitals are fine) or the id of a saved jackpot. Returns per game the blended " +
			"probabilities (jackpot odds with the margin removed, the sharp market, ClubElo and DASYIN's model where matched, with each source), " +
			"the most likely ticket and its chance ('1 in N'), the next best tickets, which games to double or triple first with the chance " +
			"vs combinations and cost, P(at least k correct), expected draws, the expected value of a ticket with the sharing assumption, and " +
			"contrarian variants. Nothing is saved. Always say these are estimates: even the most likely ticket almost always loses.",
		InputSchema: tObj(map[string]any{
			"text":         tStr("Pasted jackpot games, one per line with the three 1X2 odds"),
			"jackpot_id":   tStr("A saved jackpot's id (from football_jackpots) instead of text"),
			"prize":        tNum("Jackpot prize in the organisation's currency (for the expected value)"),
			"ticket_price": tNum("Price of one ticket / combination (default 99)"),
			"bookmaker":    tStr("Bookmaker, e.g. SportPesa or Betika"),
			"date":         tDate("Day of the games (YYYY-MM-DD) to match them to fixtures; default the next days"),
			"top_n":        tInt("How many best tickets to list (default 5)", 1, 50),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballJackpotAnalyse,
	})
	Register(Tool{
		Name:  "football_jackpots",
		Title: "Saved football jackpots",
		Description: "The person's saved jackpots (open, closed, graded): bookmaker, prize, number of games and how many were matched to fixtures, " +
			"the most likely ticket and its chance, saved tickets, and after the games how many each ticket got right (graded automatically from " +
			"results). Use for 'how did my jackpot go?' or to find a jackpot id for football_jackpot_analyse.",
		InputSchema: tObj(map[string]any{
			"status": tEnum("Which jackpots (default all)", "open", "closed", "graded", "draft", "all"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "football",
		Handler:  footballJackpots,
	})
}

func pctStr(v any) string {
	f, ok := v.(float64)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.1f%%", f*100)
}

func footballJackpotAnalyse(c *Ctx, a map[string]any) (any, error) {
	var d map[string]any
	topN := argInt(a, "top_n", 5)
	if id := strings.TrimSpace(argStr(a, "jackpot_id")); id != "" {
		v, err := c.get("/football/jackpots/"+url.PathEscape(id), url.Values{"top_n": {fmt.Sprint(topN)}})
		if err != nil {
			return nil, err
		}
		d = tObjOf(v)
	} else {
		text := strings.TrimSpace(argStr(a, "text"))
		if text == "" {
			return nil, tErr("Give the jackpot's games as text (one per line with the three odds) or a saved jackpot_id.")
		}
		body := map[string]any{"text": text, "top_n": topN}
		if p, ok := argNum(a, "prize"); ok {
			body["prize"] = p
		}
		if p, ok := argNum(a, "ticket_price"); ok {
			body["ticket_price"] = p
		}
		if b := argStr(a, "bookmaker"); b != "" {
			body["bookmaker"] = b
		}
		if dt := argStr(a, "date"); dt != "" {
			body["date"] = dt
		}
		v, err := c.api("POST", "/football/jackpots/analyse", nil, body)
		if err != nil {
			return nil, err
		}
		d = tObjOf(v)
	}
	an := tObjOf(d["analysis"])
	if an == nil {
		return nil, tErr("No analysis came back.")
	}
	games := []map[string]any{}
	for _, g := range tList(an["games"]) {
		srcs := []string{}
		for _, s := range tList(g["sources"]) {
			if s["used"] == true {
				srcs = append(srcs, fmt.Sprintf("%s %.0f%%", tS(s, "key"), toF64(s["weight"])*100))
			}
		}
		x := tPick(g, "n", "home", "away", "kickoff", "odds", "probs", "pick", "pick_p", "second", "certainty", "draw_lean")
		x["sources_used"] = strings.Join(srcs, ", ")
		ev := []string{}
		for _, e := range tList(g["evidence"]) {
			ev = append(ev, tS(e, "text"))
		}
		if len(ev) > 0 {
			x["evidence"] = ev
		}
		games = append(games, x)
	}
	res := map[string]any{"games": games, "most_likely": an["most_likely"], "at_least": an["at_least"], "expected_draws": an["expected_draws"],
		"ev":    tPick(tObjOf(an["ev"]), "ticket_price", "prize", "p_jackpot", "co_winners_expected", "share_factor", "ev_jackpot", "ev_bonuses", "ev_total", "explain"),
		"notes": an["notes"], "disclaimer": an["disclaimer"], "as_of": an["as_of"]}
	tops := []map[string]any{}
	for i, t := range tList(an["top_tickets"]) {
		if i >= topN {
			break
		}
		tops = append(tops, tPick(t, "rank", "picks", "one_in", "changes"))
	}
	res["top_tickets"] = tops
	cover := []map[string]any{}
	for i, s := range tList(an["cover"]) {
		if i >= 6 {
			break
		}
		cover = append(cover, tPick(s, "step", "game", "picks", "kind", "combinations", "cost", "one_in"))
	}
	res["cover_priority"] = cover
	cv := []map[string]any{}
	for _, v := range tList(an["contrarian"]) {
		cv = append(cv, tPick(v, "label", "changes", "one_in", "p_ratio", "co_winners_expected", "ev_total", "why"))
	}
	res["contrarian"] = cv
	unmatched := []string{}
	for _, g := range tList(d["games"]) {
		m := tObjOf(g["match"])
		if st := tS(m, "status"); st != "matched" && st != "manual" {
			unmatched = append(unmatched, fmt.Sprintf("%v. %s v %s (%s)", g["n"], tS(g, "home"), tS(g, "away"), st))
		}
	}
	if len(unmatched) > 0 {
		res["unmatched_games"] = unmatched
	}
	if j := tObjOf(d["jackpot"]); j != nil && tS(j, "id") != "" {
		res["jackpot"] = tPick(j, "id", "name", "bookmaker", "prize", "currency", "ticket_price", "closes_at", "status")
		res["url"] = tURL("/football/jackpots/" + tS(j, "id"))
		if gr := tObjOf(d["grading"]); gr != nil {
			res["grading"] = gr
		}
	} else {
		res["url"] = tURL("/football/jackpots")
	}
	return res, nil
}

func toF64(v any) float64 {
	f, _ := v.(float64)
	return f
}

func footballJackpots(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"limit": {fmt.Sprint(argInt(a, "limit", 20))}}
	if st := argStr(a, "status"); st != "" && st != "all" {
		q.Set("status", st)
	}
	v, err := c.get("/football/jackpots", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := []map[string]any{}
	for _, j := range tList(d["jackpots"]) {
		x := tPick(j, "id", "name", "bookmaker", "prize", "currency", "ticket_price", "closes_at", "status", "n_games", "matched_games", "most_likely", "tickets", "graded")
		x["url"] = tURL("/football/jackpots/" + tS(j, "id"))
		out = append(out, x)
	}
	return map[string]any{"jackpots": out, "total": d["total"], "note": "Jackpot chances are estimates; even the most likely ticket almost always loses."}, nil
}

// footballFreeSummary adds the v2.17 sections to football_match_analysis.
func footballFreeSummary(d, res map[string]any) {
	if tn := tObjOf(d["team_news"]); tn != nil {
		facts := []map[string]any{}
		for _, side := range []string{"home", "away"} {
			for _, f := range tList(tn[side]) {
				if f["dismissed"] == true {
					continue
				}
				x := tPick(f, "team", "player", "role", "status", "confidence", "confidence_note", "used_in_model", "last_seen")
				srcs := []string{}
				for _, s := range tList(f["sources"]) {
					srcs = append(srcs, fmt.Sprintf("%s (%s)", tS(s, "domain"), tS(s, "url")))
				}
				x["sources"] = srcs
				facts = append(facts, x)
			}
		}
		res["team_news"] = map[string]any{"facts": facts, "counts": tn["counts"], "rule": tn["rules_note"]}
	}
	if nw := tObjOf(d["news"]); nw != nil {
		items := []map[string]any{}
		for i, it := range tList(nw["items"]) {
			if i >= 5 {
				break
			}
			items = append(items, tPick(it, "title", "domain", "published_at", "url", "language", "matched_keywords"))
		}
		res["news"] = items
	}
	if sc := tObjOf(d["source_check"]); sc != nil {
		issues := []map[string]any{}
		for _, it := range tList(sc["items"]) {
			if st := tS(it, "status"); st == "conflict" || st == "minor" {
				issues = append(issues, tPick(it, "label", "status", "value", "disagreeing", "note"))
			}
		}
		res["source_check"] = map[string]any{"summary": tPick(tObjOf(sc["summary"]), "status", "agree", "minor", "conflict", "single", "sources"), "sections": sc["sections"], "issues": issues}
	}
	if ce := tObjOf(d["clubelo"]); ce != nil {
		res["clubelo"] = tPick(ce, "available", "home.club", "home.elo", "away.club", "away.elo", "probs", "probs_source", "note")
	}
	if ko := tObjOf(d["kenyan_odds"]); ko != nil {
		books := []map[string]any{}
		for _, b := range tList(ko["bookmakers"]) {
			books = append(books, tPick(b, "title", "prices", "margin", "vs_fair", "fetched_at"))
		}
		res["kenyan_odds"] = map[string]any{"available": ko["available"], "bookmakers": books, "fair": ko["fair"], "fair_source": ko["fair_source"], "note": ko["note"]}
	}
	if m := tObjOf(d["model"]); m != nil && m["org_adjusted"] == true {
		if mm, ok := res["model"].(map[string]any); ok {
			mm["org_adjusted"], mm["org_note"] = true, m["org_note"]
		}
	}
}

var _ = pctStr
