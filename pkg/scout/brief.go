package scout

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// The morning brief's "Market scout" section. The brief (another package)
// calls BriefSection; nothing here imports it — the types are plain values
// it can render in an email, WhatsApp text or the app.

// BriefBlock is the section.
type BriefBlock struct {
	Key       string      `json:"key"`      // "market_scout"
	Title     string      `json:"title"`    // "Market scout"
	Headline  string      `json:"headline"` // one sentence
	Items     []BriefItem `json:"items"`
	Link      string      `json:"link"` // absolute URL of the tab (path when PUBLIC_APP_URL is not set)
	LinkLabel string      `json:"link_label"`
	New       int         `json:"new"`          // new since `since` (all of them, not only the items listed)
	Closing   int         `json:"closing_soon"` // saved / new with a deadline in the next 7 days
	Reminders int         `json:"reminders"`    // the user's reminders due today
}

// BriefItem is one opportunity in the section.
type BriefItem struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Type      string  `json:"type"`       // tender | grant | …
	TypeLabel string  `json:"type_label"` // "Tender"
	Value     string  `json:"value"`      // "KES 2,500,000" or ""
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Deadline  string  `json:"deadline"`  // YYYY-MM-DD or ""
	DaysLeft  *int    `json:"days_left"` // nil without a deadline
	When      string  `json:"when"`      // "closes in 12 days" / "no fixed deadline"
	Country   string  `json:"country"`   // display name
	Fit       int     `json:"fit"`       // 0–100
	Why       string  `json:"why"`       // new | closing_soon | reminder
	URL       string  `json:"url"`       // the card in the app
	SourceURL string  `json:"source_url"`
}

// BriefSection returns the section for one person's brief: the user's
// reminders due today, then opportunities first seen since `since`, then
// saved / new ones closing within 7 days — best fit first, at most limit
// items. ok is false when the scout was never switched on or there is
// nothing to say.
func BriefSection(ctx context.Context, db *gorm.DB, org, userID string, since time.Time, limit int) (BriefBlock, bool) {
	if db == nil || org == "" {
		return BriefBlock{}, false
	}
	db = db.WithContext(ctx).Session(&gorm.Session{NewDB: true, Logger: logger.Default.LogMode(logger.Silent)})
	var st Settings
	if db.Where("organization_id = ?", org).Limit(1).Find(&st).Error != nil || st.OrganizationID == "" || (st.EnabledAt == nil && !st.Enabled) {
		return BriefBlock{}, false
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	loc := time.UTC
	if l, err := time.LoadLocation(orgprefs.Get(db, org).Timezone); err == nil {
		loc = l
	}
	today := localDay(clockNow(), loc)
	ds := today.Format("2006-01-02")
	// Cards whose deadline passed are not news.
	db.Model(&Opportunity{}).Where("organization_id = ? AND status IN ? AND deadline IS NOT NULL AND deadline < ?", org, []string{StatusNew, StatusSaved}, ds).Update("status", StatusExpired)

	var reminders, fresh, closing []Opportunity
	if userID != "" {
		db.Where("organization_id = ? AND remind_user_id = ? AND remind_on = ? AND status NOT IN ?", org, userID, ds, []string{StatusDismissed, StatusExpired}).
			Order("fit DESC").Limit(limit).Find(&reminders)
	}
	db.Where("organization_id = ? AND status IN ? AND first_seen_at >= ?", org, []string{StatusNew, StatusSaved}, since).Order("fit DESC").Limit(50).Find(&fresh)
	db.Where("organization_id = ? AND status IN ? AND deadline IS NOT NULL AND deadline >= ? AND deadline <= ?", org, []string{StatusSaved, StatusNew}, ds, today.AddDate(0, 0, 7).Format("2006-01-02")).
		Order("deadline ASC").Order("fit DESC").Limit(20).Find(&closing)

	b := BriefBlock{Key: "market_scout", Title: "Market scout", Items: []BriefItem{}, LinkLabel: "Open Market scout", New: len(fresh), Reminders: len(reminders)}
	appURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_APP_URL")), "/")
	b.Link = appURL + TabPath
	seen := map[string]bool{}
	add := func(o Opportunity, why string) {
		if seen[o.ID] || len(b.Items) >= limit {
			return
		}
		seen[o.ID] = true
		it := BriefItem{ID: o.ID, Title: o.Title, Type: o.Type, TypeLabel: TypeLabel(o.Type), Currency: o.Currency, Fit: o.Fit, Why: why, URL: appURL + CardPath(o.ID)}
		if o.EstValue != nil {
			it.Amount, it.Value = *o.EstValue, money(*o.EstValue, o.Currency)
		}
		if o.Country != "" {
			it.Country = currency.CountryName(o.Country)
		}
		if o.Deadline != nil {
			d := int(o.Deadline.Sub(today).Hours() / 24)
			it.Deadline, it.DaysLeft = o.Deadline.Format("2006-01-02"), &d
			it.When = "closes " + daysText(d)
		} else {
			it.When = "no fixed deadline"
		}
		if len(o.Sources) > 0 {
			it.SourceURL = o.Sources[0].URL
		}
		b.Items = append(b.Items, it)
	}
	for _, o := range reminders {
		add(o, "reminder")
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Fit > fresh[j].Fit })
	for _, o := range fresh {
		add(o, "new")
	}
	for _, o := range closing {
		if !seen[o.ID] {
			b.Closing++
		}
		add(o, "closing_soon")
	}
	if len(b.Items) == 0 {
		return b, false
	}
	parts := []string{}
	if b.New > 0 {
		parts = append(parts, fmt.Sprintf("%d new opportunit%s", b.New, map[bool]string{true: "y", false: "ies"}[b.New == 1]))
	}
	if b.Closing > 0 {
		parts = append(parts, fmt.Sprintf("%d closing this week", b.Closing))
	}
	if b.Reminders > 0 {
		parts = append(parts, fmt.Sprintf("%d reminder%s for today", b.Reminders, plural(b.Reminders)))
	}
	best := b.Items[0]
	for _, it := range b.Items {
		if it.Fit > best.Fit {
			best = it
		}
	}
	b.Headline = strings.Join(parts, ", ") + fmt.Sprintf(" — best fit: %s (%d/100).", best.Title, best.Fit)
	if len(parts) == 0 {
		b.Headline = fmt.Sprintf("Best fit: %s (%d/100).", best.Title, best.Fit)
	} else {
		b.Headline = strings.ToUpper(b.Headline[:1]) + b.Headline[1:]
	}
	return b, true
}
