package main

import (
	"context"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/brief"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/scout"
)

// The market scout's opportunities as a morning-brief section (between the
// own-data opportunities and Today). pkg/brief and pkg/scout don't know each
// other; this adapter joins them.
func init() {
	brief.Register("market_scout", "Market scout", 45, func(ctx context.Context, in *brief.Input) (*brief.Section, error) {
		if in.Viewer == nil {
			return nil, nil
		}
		b, ok := scout.BriefSection(ctx, in.DB, in.Org, in.Viewer.UserID, in.Since, 5)
		if !ok || len(b.Items) == 0 {
			return nil, nil
		}
		sec := &brief.Section{Key: "market_scout", Title: b.Title, Summary: b.Headline, Link: b.Link}
		items := make([]brief.Item, 0, len(b.Items))
		var newVal float64
		cur := ""
		for _, it := range b.Items {
			tag, tone := "", "info"
			switch it.Why {
			case "new":
				tag = "New"
			case "closing_soon":
				tag, tone = "Closing soon", "warn"
			case "reminder":
				tag, tone = "Reminder", "warn"
			}
			detail := strings.TrimSpace(strings.Join(nonEmpty(it.TypeLabel, it.Country, it.When), " · "))
			items = append(items, brief.Item{Key: it.ID, Title: it.Title, Detail: detail, Amount: it.Amount, Currency: it.Currency,
				Value: it.Value, Link: it.URL, External: it.SourceURL, Tag: tag, Tone: tone})
			if it.Why == "new" && it.Amount > 0 && (cur == "" || cur == it.Currency) {
				newVal += it.Amount
				cur = it.Currency
			}
		}
		sec.Add("scout", "Opportunities to go for", items, len(items), b.Link)
		if b.New > 0 {
			if newVal > 0 && cur != "" {
				sec.Say(42, "%d new from the market scout (%s)", b.New, brief.Approx(newVal, cur))
			} else {
				sec.Say(42, "%d new from the market scout", b.New)
			}
		}
		if b.Closing > 0 {
			sec.Say(48, "%d scout opportunit%s closing within a week", b.Closing, map[bool]string{true: "y", false: "ies"}[b.Closing == 1])
		}
		return sec, nil
	}, brief.Icon("fa-solid fa-binoculars"),
		brief.Describe("Tenders, grants, leads and new markets the AI market scout found on the web, best fit first"),
		brief.Requires(func(v *brief.Viewer) bool { return v.Managing }, "For owners, admins and managers"))
}

func nonEmpty(s ...string) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			out = append(out, x)
		}
	}
	return out
}
