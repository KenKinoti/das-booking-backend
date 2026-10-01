package expenses

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

// ensureSchedule turns the daily automatic sync on (06:00 local) for an
// organisation that has no schedule yet. A schedule switched off by the user
// stays off.
func (s *Service) ensureSchedule(org string) {
	var n int64
	s.DB.Model(&Connection{}).Where("organization_id = ? AND kind = ?", org, "schedule").Count(&n)
	if n > 0 {
		return
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&Connection{ID: uuid.NewString(), OrganizationID: org, Kind: "schedule", Enabled: true,
		Settings: Settings{AutoSync: true, Hour: 6}, CreatedAt: now, UpdatedAt: now})
}

// AutoApproveItems applies the vendors' auto-approve rules to new items:
// an invoice is approved when its amount is within ±pct % of the vendor's
// last approved one in the same currency. Returns how many were approved.
func (s *Service) AutoApproveItems(org string, ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	var list []Item
	s.DB.Where("organization_id = ? AND id IN ? AND status = ?", org, ids, StatusPending).Order("invoice_date").Find(&list)
	n := 0
	for _, it := range list {
		if it.VendorID == "" || it.Total <= 0 || it.InvoiceDate == nil || (it.Kind != KindInvoice && it.Kind != KindReceipt) {
			continue
		}
		v := s.vendorByID(org, it.VendorID)
		if v == nil || !v.AutoApprove {
			continue
		}
		pct := v.AutoApprovePct
		if pct <= 0 {
			pct = 20
		}
		var last Item
		if s.DB.Where("organization_id = ? AND vendor_id = ? AND status = ? AND currency = ? AND id <> ? AND invoice_date <= ?", org, v.ID, StatusApproved, it.Currency, it.ID, *it.InvoiceDate).
			Order("invoice_date DESC").First(&last).Error != nil || last.Total <= 0 {
			s.addWarning(it.ID, fmt.Sprintf("Not approved automatically: this is the first %s invoice — approve it once, later ones within ±%.0f%% are approved automatically", v.Name, pct))
			continue
		}
		change := (it.Total - last.Total) / last.Total * 100
		if math.Abs(change) > pct+1e-9 {
			s.addWarning(it.ID, fmt.Sprintf("Not approved automatically: %s is %+.0f%% vs the last invoice (%s) — the rule allows ±%.0f%%", fmtMoney(it.Total, it.Currency), change, fmtMoney(last.Total, last.Currency), pct))
			continue
		}
		if _, err := s.Approve(org, "", it.ID, ApproveInput{}); err != nil {
			s.addWarning(it.ID, "Not approved automatically: "+err.Error())
			continue
		}
		note := fmt.Sprintf("Approved automatically: %s is within ±%.0f%% of the last invoice (%s on %s).", fmtMoney(it.Total, it.Currency), pct, fmtMoney(last.Total, last.Currency), last.InvoiceDate.Format("2 Jan 2006"))
		s.DB.Model(&Item{}).Where("id = ?", it.ID).Updates(map[string]any{"notes": strings.TrimSpace(it.Notes + "\n" + note), "approved_by": "auto-approve rule"})
		n++
	}
	return n
}

func (s *Service) addWarning(id, w string) {
	var it Item
	if s.DB.Where("id = ?", id).First(&it).Error != nil {
		return
	}
	for _, x := range it.Warnings {
		if x == w {
			return
		}
	}
	it.Warnings = append(dropWarn(it.Warnings, "Not approved automatically"), w)
	wj, _ := json.Marshal(it.Warnings)
	s.DB.Model(&Item{}).Where("id = ?", id).Update("warnings", string(wj))
}

// SourceAutomation says, per source, what is automatic and what is not.
type SourceAutomation struct {
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Level       string     `json:"level"` // automatic | partly | manual | off
	How         string     `json:"how"`
	Manual      string     `json:"manual,omitempty"` // what still needs doing by hand
	LastFetched *time.Time `json:"last_fetched"`
	Status      string     `json:"status,omitempty"`
}

// Automation describes how automatic each expense source is right now.
type Automation struct {
	DailySync bool               `json:"daily_sync"`
	Hour      int                `json:"hour"`
	NextRun   *time.Time         `json:"next_run"`
	Sources   []SourceAutomation `json:"sources"`
}

// AutomationFor builds the "How automatic is this?" panel.
func (s *Service) AutomationFor(org string) *Automation {
	a := &Automation{Sources: []SourceAutomation{}}
	sched := s.connection(org, "schedule")
	if sched != nil && sched.Enabled && sched.Settings.AutoSync {
		a.DailySync, a.Hour = true, sched.Settings.Hour
		loc := locOf(s.DB, org)
		now := s.now().In(loc)
		next := time.Date(now.Year(), now.Month(), now.Day(), a.Hour, 0, 0, 0, loc)
		if !next.After(now) || (sched.LastSyncAt != nil && sched.LastSyncAt.In(loc).Format(dayFmt) == now.Format(dayFmt)) {
			next = next.AddDate(0, 0, 1)
		}
		a.NextRun = &next
	}
	gm := s.connection(org, "gmail")
	gmOn := gm != nil && gm.Enabled && gm.SecretEnc != ""
	sy := s.connection(org, "synergy")
	syOn := sy != nil && sy.Enabled && sy.SecretEnc != ""
	daily := map[bool]string{true: "daily", false: "when you press Sync now (turn on the daily sync)"}[a.DailySync]
	gw := s.vendorByKey(org, "google_workspace")
	rule := "approve each one yourself (no auto-approve rule)"
	if gw != nil && gw.AutoApprove {
		rule = fmt.Sprintf("approved automatically when within ±%.0f%% of the last invoice", gw.AutoApprovePct)
	}
	row := SourceAutomation{Key: "google_workspace", Label: "Google Workspace invoices", Level: "off",
		How:    "Google has no billing API for Workspace customers who pay Google directly — the Reseller API only covers resellers' customers and the Cloud Billing API only Google Cloud. So invoices come from your Gmail: the monthly “Your invoice is available” email and its PDF.",
		Manual: "Connect Gmail in Sources."}
	if gmOn {
		row.Level, row.LastFetched, row.Status = "automatic", gm.LastSyncAt, gm.LastStatus
		row.How = "Read from Gmail " + daily + ": amount and VAT from the invoice PDF (“Total in EUR”), converted to your currency at the rate of the invoice month; " + rule + ". Google has no billing API for customers who pay Google directly (the Reseller API and the Cloud Billing API don't cover a direct Workspace subscription), so Gmail is the automatic path."
		row.Manual = ""
		if !a.DailySync {
			row.Level = "partly"
			row.Manual = "Turn on the daily sync."
		}
	}
	a.Sources = append(a.Sources, row)

	var lastUp SynergyUpload
	s.DB.Where("organization_id = ? AND file_type = ?", org, SynFileStatement).Order("created_at DESC").First(&lastUp)
	p := s.prefs(org)
	mode := map[string]string{ModeCharges: "service charges", ModeTopups: "top-ups from your bank"}[p.SynergyMode]
	appr := map[bool]string{true: "created and approved automatically", false: "created automatically and left for your approval"}[p.autoApprove()]
	st := SourceAutomation{Key: "synergy_statements", Label: "Synergy statements (expenses)", Level: "manual",
		How:    "Synergy Wholesale's API has no statement or invoice list. Its monthly “transaction statement” email has the CSV attached: with Gmail connected it is imported automatically. Expenses (" + mode + ") are " + appr + ".",
		Manual: "Upload the monthly statement CSV (Expenses → Synergy → Upload), or connect Gmail so the statement email is imported for you."}
	if lastUp.ID != "" {
		t := lastUp.CreatedAt
		st.LastFetched = &t
	}
	if gmOn {
		st.Level, st.Manual = "automatic", ""
		if !a.DailySync {
			st.Level, st.Manual = "partly", "Turn on the daily sync."
		}
		if gm.LastSyncAt != nil && (st.LastFetched == nil || gm.LastSyncAt.After(*st.LastFetched)) {
			st.LastFetched = gm.LastSyncAt
		}
	}
	a.Sources = append(a.Sources, st)

	api := SourceAutomation{Key: "synergy_api", Label: "Synergy balance, domains & hosting", Level: "off",
		How:    "The Synergy Wholesale API (balanceQuery, listDomains, listHosting) gives the prepaid balance, domain expiry dates and hosting plans / renewal dates.",
		Manual: "Add your reseller ID and API key in Sources, and whitelist the server IP in Synergy."}
	if syOn {
		api.Level, api.LastFetched, api.Status, api.Manual = "automatic", sy.LastSyncAt, sy.LastStatus, ""
		api.How = "Fetched " + daily + " from the Synergy Wholesale API: balance, domain expiry dates, hosting plans and renewal dates."
		if !a.DailySync {
			api.Level, api.Manual = "partly", "Turn on the daily sync."
		}
	}
	a.Sources = append(a.Sources, api)

	var lastUse SynergyUpload
	s.DB.Where("organization_id = ? AND file_type = ?", org, SynFileUsage).Order("created_at DESC").First(&lastUse)
	use := SourceAutomation{Key: "synergy_usage", Label: "Synergy disk usage", Level: "manual",
		How:    "The hosting usage report (Identifier, Plan, Usage, Limit …) is a CSV you export from the Synergy portal. The public API documentation does not list disk usage for hosting services; if your account's listHosting returns usage and limit, snapshots are stored automatically on each sync.",
		Manual: "Export “Hosting account usage” from Synergy monthly and upload it (or email it to yourself from Synergy so the Gmail sync imports it)."}
	if lastUse.ID != "" {
		t := lastUse.CreatedAt
		use.LastFetched = &t
	}
	var apiSnap int64
	s.DB.Model(&SynergyUsage{}).Where("organization_id = ? AND upload_id = ?", org, "api").Count(&apiSnap)
	if apiSnap > 0 && syOn {
		use.Level, use.Manual = "automatic", ""
		use.How = "Disk usage comes from the Synergy API (listHosting) on every sync."
		use.LastFetched = sy.LastSyncAt
	}
	a.Sources = append(a.Sources, use)
	return a
}

// storeAPIUsage stores today's disk usage snapshot from listHosting (only
// when the API returned usage and limit). Returns how many sites were stored.
func (s *Service) storeAPIUsage(org string, hosting []SynergyHosting) int {
	day := Day(s.now())
	n := 0
	for _, h := range hosting {
		if h.UsageMB == nil || h.LimitMB == nil || *h.LimitMB <= 0 || h.Domain == "" {
			continue
		}
		pct := math.Round(*h.UsageMB / *h.LimitMB * 10000) / 100
		u := SynergyUsage{ID: uuid.NewString(), OrganizationID: org, SnapshotDate: day, Domain: clip(h.Domain, 250), Status: clip(h.Status, 38), Plan: clip(h.Plan, 118),
			UsageMB: *h.UsageMB, LimitMB: *h.LimitMB, PctUsed: pct, At75: pct >= 75, At100MB: *h.LimitMB-*h.UsageMB <= 100, RemainingMB: r2(*h.LimitMB - *h.UsageMB),
			UploadID: "api", CreatedAt: s.now(), UpdatedAt: s.now()}
		res := s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}, {Name: "snapshot_date"}, {Name: "domain"}},
			DoUpdates: clause.AssignmentColumns([]string{"status", "plan", "usage_mb", "limit_mb", "pct_used", "at_75", "at_100mb", "remaining_mb", "updated_at"})}).Create(&u)
		if res.Error == nil {
			n++
		}
	}
	return n
}
