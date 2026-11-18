package aitools

import (
	"net/url"
	"strings"
)

// Morning brief tool: reads /brief/today (or /brief/day/:date) — the same
// brief the person gets by email / WhatsApp, limited to what they may see.
func init() {
	Register(Tool{
		Name:  "brief_today",
		Title: "Morning brief",
		Description: "The person's morning brief for today (or a past day): a one-line headline ('3 bills due this week (KES 41,200), 2 Jira issues " +
			"overdue, 1 new opportunity worth ~KES 60,000') and the sections they may see — Bills & payables (new supplier bills incl. Synergy " +
			"invoices in the Expenses inbox, due today / this week, overdue, the Synergy balance and top-up, hosting / domain renewals in 14 days), " +
			"Money in (payments received yesterday, top overdue invoices, client subscriptions past due and suspensions in the next 3 days), " +
			"Projects (progress per active Jira / local project, done since the last brief, overdue, due in 7 days, high priority or blocked), " +
			"Opportunities (the top opportunities with estimated values, new industry-watch headlines) and Today (bookings, meetings, reminders " +
			"due). Each item has a link into the app. Also says whether today's brief was delivered by email / WhatsApp. Use for 'what's my " +
			"brief?', 'what needs my attention today?', 'what bills are due this week?'.",
		InputSchema: tObj(map[string]any{
			"date": tDate("A past day's brief (YYYY-MM-DD, last 30 days); default today"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  briefToday,
	})
}

func briefToday(c *Ctx, a map[string]any) (any, error) {
	path := "/brief/today"
	if d := strings.TrimSpace(argStr(a, "date")); d != "" && d != c.today() {
		if !validDate(d) {
			return nil, tErr("date must be YYYY-MM-DD")
		}
		path = "/brief/day/" + url.PathEscape(d)
	}
	v, err := c.get(path, nil)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	b, _ := d["brief"].(map[string]any)
	out := map[string]any{"live": d["live"], "url": d["url"]}
	if b == nil {
		out["note"] = "No brief was written for that day (it was missed or failed)."
		out["delivery"] = briefDelivery(d["delivery"])
		return out, nil
	}
	for _, k := range []string{"date", "date_label", "timezone", "headline", "currency"} {
		out[k] = b[k]
	}
	secs := []map[string]any{}
	list, _ := b["sections"].([]any)
	for _, x := range list {
		s, ok := x.(map[string]any)
		if !ok {
			continue
		}
		sec := tPick(s, "key", "title", "summary", "notes")
		groups := []map[string]any{}
		gl, _ := s["groups"].([]any)
		for _, gx := range gl {
			g, ok := gx.(map[string]any)
			if !ok {
				continue
			}
			items := []map[string]any{}
			il, _ := g["items"].([]any)
			for _, ix := range il {
				it, ok := ix.(map[string]any)
				if !ok {
					continue
				}
				m := tPick(it, "title", "detail", "value", "tag", "external")
				if l, _ := it["link"].(string); l != "" {
					m["url"] = tURL(l)
				}
				items = append(items, m)
			}
			gm := map[string]any{"title": g["title"], "items": items}
			if more, ok := g["more"].(float64); ok && more > 0 {
				gm["more"] = more
			}
			groups = append(groups, gm)
		}
		sec["groups"] = groups
		if l, _ := s["link"].(string); l != "" {
			sec["url"] = tURL(l)
		}
		secs = append(secs, sec)
	}
	out["sections"] = secs
	if del := briefDelivery(d["delivery"]); del != nil {
		out["delivery"] = del
	}
	return out, nil
}

func briefDelivery(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	return tPick(m, "status", "email_status", "email_error", "whatsapp_status", "whatsapp_error", "whatsapp_kind", "late", "created_at")
}
