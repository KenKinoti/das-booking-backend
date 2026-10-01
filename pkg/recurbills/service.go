package recurbills

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// ValidationError is a 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...interface{}) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// ErrNotFound is a 404.
var ErrNotFound = errors.New("not found")

// MaxCatchUp caps how many missed periods one run generates per schedule.
const MaxCatchUp = 24

// Service runs recurring bills and reminders.
type Service struct {
	DB   *gorm.DB
	Fin  *finance.Service
	FX   *fxrates.Service
	Exp  *expenses.Service // optional (subscriptions, Synergy balance)
	Mail *mail.Service
	Now  func() time.Time
	// AppURL is the web app origin used in email links (PUBLIC_APP_URL).
	AppURL string
	// LogoURL returns the organisation's absolute logo URL ("" for none).
	LogoURL func(org string) string
}

// New builds a service.
func New(db *gorm.DB, fx *fxrates.Service, exp *expenses.Service) *Service {
	return &Service{DB: db, Fin: finance.NewService(db), FX: fx, Exp: exp, Mail: mail.Shared(db), Now: time.Now}
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Loc is the organisation's time zone.
func (s *Service) Loc(org string) *time.Location {
	if tz := orgprefs.Get(s.DB, org).Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.UTC
}

// Today is the organisation's local calendar date.
func (s *Service) Today(org string) time.Time {
	return Day(localDate(s.now().In(s.Loc(org))))
}

func localDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---------------------------------------------------------------- settings

// SettingsFor returns the organisation's reminder settings (defaults when none saved).
func (s *Service) SettingsFor(org string) Settings {
	var st Settings
	if err := s.DB.Where("organization_id = ?", org).First(&st).Error; err != nil {
		return DefaultSettings(org)
	}
	if st.LeadDays == nil {
		st.LeadDays = []int{}
	}
	if st.Recipients == nil {
		st.Recipients = []string{}
	}
	return st
}

// SettingsInput updates settings (nil = unchanged).
type SettingsInput struct {
	Enabled       *bool     `json:"enabled"`
	LeadDays      *[]int    `json:"lead_days"`
	OnDue         *bool     `json:"on_due"`
	OverdueEvery  *int      `json:"overdue_every"`
	OverdueMax    *int      `json:"overdue_max"`
	IncludeOneOff *bool     `json:"include_one_off"`
	Digest        *bool     `json:"digest"`
	DigestWeekday *int      `json:"digest_weekday"`
	DigestHour    *int      `json:"digest_hour"`
	SendHour      *int      `json:"send_hour"`
	NotifyAdmins  *bool     `json:"notify_admins"`
	UseNotifyList *bool     `json:"use_notification_recipients"`
	Recipients    *[]string `json:"recipients"`
	InApp         *bool     `json:"in_app"`
}

func cleanLeads(l []int) ([]int, error) {
	seen := map[int]bool{}
	out := []int{}
	for _, d := range l {
		if d < 1 || d > 60 {
			return nil, invalid("reminder lead times must be 1 to 60 days before the due date")
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) > 5 {
		return nil, invalid("use at most 5 lead times")
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out, nil
}

// SaveSettings validates and stores settings.
func (s *Service) SaveSettings(org string, in SettingsInput) (Settings, error) {
	st := s.SettingsFor(org)
	setB := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	setB(&st.Enabled, in.Enabled)
	setB(&st.OnDue, in.OnDue)
	setB(&st.IncludeOneOff, in.IncludeOneOff)
	setB(&st.Digest, in.Digest)
	setB(&st.NotifyAdmins, in.NotifyAdmins)
	setB(&st.UseNotifyList, in.UseNotifyList)
	setB(&st.InApp, in.InApp)
	if in.LeadDays != nil {
		l, err := cleanLeads(*in.LeadDays)
		if err != nil {
			return st, err
		}
		st.LeadDays = l
	}
	if in.OverdueEvery != nil {
		if *in.OverdueEvery < 0 || *in.OverdueEvery > 30 {
			return st, invalid("overdue reminders: every 1 to 30 days (0 = off)")
		}
		st.OverdueEvery = *in.OverdueEvery
	}
	if in.OverdueMax != nil {
		if *in.OverdueMax < 0 || *in.OverdueMax > 10 {
			return st, invalid("send at most 10 overdue reminders")
		}
		st.OverdueMax = *in.OverdueMax
	}
	if in.DigestWeekday != nil {
		if *in.DigestWeekday < 0 || *in.DigestWeekday > 6 {
			return st, invalid("choose a weekday for the digest")
		}
		st.DigestWeekday = *in.DigestWeekday
	}
	if in.DigestHour != nil {
		if *in.DigestHour < 0 || *in.DigestHour > 23 {
			return st, invalid("choose an hour between 0 and 23")
		}
		st.DigestHour = *in.DigestHour
	}
	if in.SendHour != nil {
		if *in.SendHour < 0 || *in.SendHour > 23 {
			return st, invalid("choose an hour between 0 and 23")
		}
		st.SendHour = *in.SendHour
	}
	if in.Recipients != nil {
		v, bad := mail.ParseList(*in.Recipients...)
		if len(bad) > 0 {
			return st, invalid("not a valid email address: %s", strings.Join(bad, ", "))
		}
		if len(v) > 20 {
			return st, invalid("add at most 20 recipients")
		}
		st.Recipients = v
	}
	st.OrganizationID, st.UpdatedAt = org, s.now()
	return st, s.DB.Save(&st).Error
}

// ---------------------------------------------------------------- schedules

// Input creates or updates a schedule.
type Input struct {
	Name            string   `json:"name"`
	VendorID        string   `json:"vendor_id"`
	AccountID       string   `json:"account_id"`
	Description     string   `json:"description"`
	Reference       string   `json:"reference"`
	Notes           string   `json:"notes"`
	Amount          float64  `json:"amount"`
	TaxRate         float64  `json:"tax_rate"`
	Currency        string   `json:"currency"`
	AmountMode      string   `json:"amount_mode"`
	Frequency       string   `json:"frequency"`
	Interval        int      `json:"interval"`
	StartDate       string   `json:"start_date"`
	EndMode         string   `json:"end_mode"`
	EndAfter        int      `json:"end_after"`
	EndDate         string   `json:"end_date"`
	CreateAs        string   `json:"create_as"`
	DueDays         int      `json:"due_days"`
	AdvanceDays     *int     `json:"advance_days"`
	AutoPay         bool     `json:"auto_pay"`
	Mode            string   `json:"mode"`
	Service         string   `json:"service"`
	SynergyDomain   string   `json:"synergy_domain"`
	ExpenseVendorID string   `json:"expense_vendor_id"`
	SubscriptionKey string   `json:"subscription_key"`
	ReminderLeads   *[]int   `json:"reminder_leads"` // null = organisation default
	ExtraRecipients []string `json:"extra_recipients"`
	RemindersOff    bool     `json:"reminders_off"`
	// SourceBillID (create only): an existing bill that is the first
	// occurrence (Bills → New bill → Repeat).
	SourceBillID string `json:"source_bill_id"`
}

func (s *Service) home(org string) string { return orgprefs.Currency(s.DB, org) }

func (s *Service) apply(org string, r *RecurringBill, in Input) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.VendorID == "" {
		return invalid("choose a vendor")
	}
	var v finance.Vendor
	if s.DB.Where("organization_id = ? AND id = ?", org, in.VendorID).First(&v).Error != nil {
		return invalid("choose a vendor")
	}
	if in.Name == "" {
		in.Name = v.Name
	}
	if in.AccountID != "" {
		var a finance.ChartOfAccount
		if s.DB.Where("organization_id = ? AND id = ?", org, in.AccountID).First(&a).Error != nil {
			return invalid("choose a category account")
		}
		if a.AccountType == finance.TypeRevenue {
			return invalid("bills should be coded to expense or asset accounts (not %s)", a.Name)
		}
	}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = s.home(org)
	}
	if !currency.IsSupported(cur) {
		return invalid("choose a supported currency")
	}
	if !(in.Amount > 0) || in.Amount > 1e10 {
		return invalid("enter the amount")
	}
	if in.TaxRate < 0 || in.TaxRate > 100 {
		return invalid("tax rate must be between 0 and 100")
	}
	switch in.AmountMode {
	case "":
		in.AmountMode = AmountFixed
	case AmountFixed, AmountEstimate:
	default:
		return invalid("amount must be fixed or an estimate")
	}
	switch in.CreateAs {
	case "":
		in.CreateAs = "draft"
	case "draft", "approved":
	default:
		return invalid("create the next bill as a draft or approved")
	}
	switch in.Mode {
	case "":
		in.Mode = ModeCreate
	case ModeCreate, ModeTrack:
	default:
		return invalid("choose whether the schedule creates bills")
	}
	if in.DueDays < 0 || in.DueDays > 365 {
		return invalid("due date: 0 to 365 days after the bill date")
	}
	adv := 7
	if r.ID != "" {
		adv = r.AdvanceDays
	}
	if in.AdvanceDays != nil {
		adv = *in.AdvanceDays
	}
	if adv < 0 || adv > 60 {
		return invalid("create the bill 0 to 60 days ahead")
	}
	start, ok := ParseDay(in.StartDate)
	if !ok {
		return invalid("choose a start date")
	}
	if in.EndMode == "" {
		in.EndMode = EndNever
	}
	var end *time.Time
	if in.EndMode == EndOn {
		if d, ok := ParseDay(in.EndDate); ok {
			end = &d
		}
	}
	rule := Rule{Frequency: in.Frequency, Interval: in.Interval, Start: start, EndMode: in.EndMode, EndAfter: in.EndAfter, EndDate: end}
	if err := rule.Validate(); err != nil {
		return invalid("%s", err.Error())
	}
	var leads []int
	if in.ReminderLeads != nil {
		l, err := cleanLeads(*in.ReminderLeads)
		if err != nil {
			return err
		}
		leads = l
	}
	extra, bad := mail.ParseList(in.ExtraRecipients...)
	if len(bad) > 0 {
		return invalid("not a valid email address: %s", strings.Join(bad, ", "))
	}
	if len(extra) > 10 {
		return invalid("add at most 10 extra recipients")
	}
	interval := in.Interval
	if in.Frequency != FreqCustom {
		interval = 1
	}
	r.Name, r.VendorID, r.AccountID = clip(in.Name, 250), v.ID, in.AccountID
	r.Description, r.Reference, r.Notes = clip(in.Description, 480), clip(in.Reference, 250), strings.TrimSpace(in.Notes)
	r.Amount, r.TaxRate, r.Currency, r.AmountMode = fxrates.Round(in.Amount, cur), in.TaxRate, cur, in.AmountMode
	r.Frequency, r.Interval, r.StartDate, r.EndMode, r.EndAfter, r.EndDate = in.Frequency, interval, start, in.EndMode, in.EndAfter, end
	if in.EndMode != EndAfter {
		r.EndAfter = 0
	}
	r.CreateAs, r.DueDays, r.AdvanceDays, r.AutoPay, r.Mode = in.CreateAs, in.DueDays, adv, in.AutoPay, in.Mode
	r.Service, r.SynergyDomain = clip(in.Service, 250), clip(strings.ToLower(in.SynergyDomain), 250)
	r.ExpenseVendorID, r.SubscriptionKey = clip(in.ExpenseVendorID, 64), clip(in.SubscriptionKey, 500)
	r.ReminderLeads, r.ExtraRecipients, r.RemindersOff = leads, extra, in.RemindersOff
	return nil
}

// Create stores a new schedule.
func (s *Service) Create(org, user string, in Input) (*RecurringBill, error) {
	var src *finance.Bill
	if in.SourceBillID != "" {
		b, err := s.Fin.GetBill(org, in.SourceBillID)
		if err != nil {
			return nil, invalid("the bill to repeat was not found")
		}
		var linked int64
		s.DB.Model(&Occurrence{}).Where("organization_id = ? AND bill_id = ?", org, b.ID).Count(&linked)
		if linked > 0 {
			return nil, invalid("this bill is already part of a recurring schedule")
		}
		src = b
		in.StartDate = b.BillDate.Format(dayFmt)
		if in.VendorID == "" {
			in.VendorID = b.VendorID
		}
		if in.Amount == 0 {
			in.Amount = b.Total
		}
		if in.DueDays == 0 {
			in.DueDays = int(Day(b.DueDate).Sub(Day(b.BillDate)).Hours() / 24)
		}
		if len(b.LineItems) > 0 {
			if in.AccountID == "" {
				in.AccountID = b.LineItems[0].AccountID
			}
			if in.Description == "" {
				in.Description = b.LineItems[0].Description
			}
			if in.TaxRate == 0 {
				in.TaxRate = b.LineItems[0].TaxRate
			}
		}
		in.Currency = s.home(org)
	}
	now := s.now()
	r := &RecurringBill{ID: uuid.NewString(), OrganizationID: org, Status: StatusActive, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
	if err := s.apply(org, r, in); err != nil {
		return nil, err
	}
	// Periods before today are never caught up for a brand-new schedule
	// (except the source bill's own period, which already exists).
	today := s.Today(org)
	if Day(r.StartDate).Before(today) {
		t := today
		r.ResumedAt = &t
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(r).Error; err != nil {
			return err
		}
		if src != nil {
			o := Occurrence{ID: uuid.NewString(), OrganizationID: org, RecurringID: r.ID, Period: Day(src.BillDate), Seq: 1, DueDate: Day(src.DueDate),
				Status: OccGenerated, BillID: src.ID, Amount: src.Total, Currency: r.Currency, HomeAmount: src.Total, FxRate: 1, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&o).Error; err != nil {
				return err
			}
			lf := Day(src.BillDate)
			return tx.Model(&RecurringBill{}).Where("id = ?", r.ID).Update("last_generated_for", lf).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(org, r.ID)
}

// Update edits a schedule. Bills already created are not changed.
func (s *Service) Update(org, id string, in Input) (*RecurringBill, error) {
	r, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	oldStart := r.StartDate
	if err := s.apply(org, r, in); err != nil {
		return nil, err
	}
	// Moving the start earlier must not back-fill the past.
	today := s.Today(org)
	if !Day(r.StartDate).Equal(Day(oldStart)) && Day(r.StartDate).Before(today) && (r.ResumedAt == nil || r.ResumedAt.Before(today)) {
		t := today
		r.ResumedAt = &t
	}
	r.UpdatedAt = s.now()
	if r.Status == StatusEnded && !s.ended(r, today) {
		r.Status = StatusActive
	}
	if err := s.DB.Save(r).Error; err != nil {
		return nil, err
	}
	return s.Get(org, id)
}

// Delete removes a schedule; bills it created stay.
func (s *Service) Delete(org, id string) error {
	r, err := s.load(org, id)
	if err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("recurring_id = ?", r.ID).Delete(&Occurrence{}).Error; err != nil {
			return err
		}
		return tx.Delete(r).Error
	})
}

// SetPaused pauses or resumes a schedule. Periods while paused are not
// caught up on resume.
func (s *Service) SetPaused(org, id string, paused bool) (*RecurringBill, error) {
	r, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if paused {
		if r.Status != StatusActive {
			return nil, invalid("only an active schedule can be paused")
		}
		r.Status, r.PausedAt = StatusPaused, &now
	} else {
		if r.Status != StatusPaused {
			return nil, invalid("this schedule is not paused")
		}
		t := s.Today(org)
		r.Status, r.PausedAt, r.ResumedAt = StatusActive, nil, &t
	}
	r.UpdatedAt = now
	if err := s.DB.Save(r).Error; err != nil {
		return nil, err
	}
	return s.Get(org, id)
}

func (s *Service) load(org, id string) (*RecurringBill, error) {
	var r RecurringBill
	if err := s.DB.Where("organization_id = ? AND id = ?", org, id).First(&r).Error; err != nil {
		return nil, ErrNotFound
	}
	return &r, nil
}

// ended: the rule has no occurrence on or after today.
func (s *Service) ended(r *RecurringBill, today time.Time) bool {
	_, ok := RuleOf(r).Next(today)
	return !ok
}

// Get returns one schedule, decorated.
func (s *Service) Get(org, id string) (*RecurringBill, error) {
	r, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	l := []RecurringBill{*r}
	s.decorate(org, l)
	return &l[0], nil
}

// List returns every schedule, decorated.
func (s *Service) List(org string) []RecurringBill {
	out := []RecurringBill{}
	s.DB.Where("organization_id = ?", org).Order("status, name").Find(&out)
	s.decorate(org, out)
	return out
}

func (s *Service) vendorNames(org string) map[string]string {
	var vs []finance.Vendor
	s.DB.Select("id, name").Where("organization_id = ?", org).Find(&vs)
	m := map[string]string{}
	for _, v := range vs {
		m[v.ID] = v.Name
	}
	return m
}

func (s *Service) decorate(org string, list []RecurringBill) {
	if len(list) == 0 {
		return
	}
	names := s.vendorNames(org)
	var accts []finance.ChartOfAccount
	s.DB.Select("id, name, code").Where("organization_id = ?", org).Find(&accts)
	an := map[string]string{}
	for _, a := range accts {
		an[a.ID] = a.Code + " · " + a.Name
	}
	home := s.home(org)
	today := s.Today(org)
	type cnt struct {
		RecurringID string
		N           int
		LastPaid    *time.Time
	}
	var counts []cnt
	s.DB.Raw(`SELECT o.recurring_id, COUNT(*) AS n, MAX(CASE WHEN b.status = 'paid' THEN b.paid_at WHEN o.status = 'paid' THEN o.paid_at END) AS last_paid
		FROM finance_recurring_occurrences o LEFT JOIN bills b ON b.id = o.bill_id
		WHERE o.organization_id = ? AND o.status <> 'skipped' GROUP BY o.recurring_id`, org).Scan(&counts)
	cm := map[string]cnt{}
	for _, c := range counts {
		cm[c.RecurringID] = c
	}
	for i := range list {
		r := &list[i]
		r.VendorName = names[r.VendorID]
		r.AccountName = an[r.AccountID]
		rule := RuleOf(r)
		r.Summary = rule.Describe()
		if r.Status != StatusEnded {
			if o, ok := s.nextOpen(r, today); ok {
				r.NextDate = o.Format(dayFmt)
				r.NextDue = o.AddDate(0, 0, r.DueDays).Format(dayFmt)
			}
		}
		if r.Currency != home {
			if q := s.FX.Quote(org, r.Currency, home); q.OK() {
				v := fxrates.Convert(r.Amount, q.Rate, home)
				r.HomeAmount = &v
			}
		} else {
			v := r.Amount
			r.HomeAmount = &v
		}
		r.HomeCurr = home
		if c, ok := cm[r.ID]; ok {
			r.BillCount = c.N
			if c.LastPaid != nil {
				r.LastPaid = c.LastPaid.Format(dayFmt)
			}
		}
		if r.ReminderLeads == nil {
			r.ReminderLeads = nil
		}
		if r.ExtraRecipients == nil {
			r.ExtraRecipients = []string{}
		}
	}
}

// nextOpen is the next period on or after today that hasn't been handled.
func (s *Service) nextOpen(r *RecurringBill, today time.Time) (time.Time, bool) {
	var done []time.Time
	s.DB.Model(&Occurrence{}).Where("recurring_id = ? AND period >= ?", r.ID, today).Pluck("period", &done)
	dm := map[string]bool{}
	for _, d := range done {
		dm[Day(d).Format(dayFmt)] = true
	}
	for _, o := range RuleOf(r).Between(today, today.AddDate(6, 0, 0), 40) {
		if !dm[o.Date.Format(dayFmt)] {
			return o.Date, true
		}
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------- generation

// GenResult reports one run of the generator.
type GenResult struct {
	Created   int      `json:"created"`
	Matched   int      `json:"matched"`
	Errors    []string `json:"errors"`
	BillIDs   []string `json:"bill_ids"`
	Schedules int      `json:"schedules"`
}

// Generate creates every bill that is due for the organisation's active
// schedules (catching up missed runs, never duplicating a period).
func (s *Service) Generate(org string) GenResult {
	res := GenResult{Errors: []string{}, BillIDs: []string{}}
	var list []RecurringBill
	s.DB.Where("organization_id = ? AND status = ?", org, StatusActive).Find(&list)
	st := s.SettingsFor(org)
	today := s.Today(org)
	for i := range list {
		r := &list[i]
		res.Schedules++
		leads := r.ReminderLeads
		if leads == nil {
			leads = st.LeadDays
		}
		occs := Due(RuleOf(r), today, r.DueDays, r.AdvanceDays, leads, r.ResumedAt, MaxCatchUp)
		for _, o := range occs {
			bill, matched, err := s.generateOne(org, r, o, "system")
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s (%s): %v", r.Name, o.Date.Format(dayFmt), err))
				continue
			}
			if bill != "" {
				if matched {
					res.Matched++
				} else {
					res.Created++
					res.BillIDs = append(res.BillIDs, bill)
				}
			}
		}
		// Nothing on or after today any more: the schedule has ended.
		if s.ended(r, today) {
			s.DB.Model(&RecurringBill{}).Where("id = ?", r.ID).Updates(map[string]any{"status": StatusEnded, "updated_at": s.now()})
		}
	}
	return res
}

// generateOne handles one period: create mode creates the bill; track mode
// matches an imported bill. Returns the bill id ("" when nothing happened).
func (s *Service) generateOne(org string, r *RecurringBill, o Occ, user string) (string, bool, error) {
	period := Day(o.Date)
	var existing Occurrence
	snoozedRow := false
	if s.DB.Where("recurring_id = ? AND period = ?", r.ID, period).First(&existing).Error == nil {
		switch {
		case existing.Status == "generating" && s.now().Sub(existing.UpdatedAt) > 10*time.Minute:
			// A run died half-way: release the claim and retry.
			s.DB.Where("id = ? AND status = ?", existing.ID, "generating").Delete(&Occurrence{})
		case existing.Status == "snoozed":
			snoozedRow = true // only a snooze marker: the period is still open
		default:
			return "", false, nil // already handled (generated, skipped, matched or paid)
		}
	}
	if r.Mode == ModeTrack {
		id := s.findImported(org, r, period)
		if id == "" {
			return "", false, nil
		}
		var b finance.Bill
		s.DB.Where("id = ?", id).First(&b)
		now := s.now()
		if snoozedRow {
			err := s.DB.Model(&Occurrence{}).Where("id = ? AND status = ?", existing.ID, "snoozed").Updates(map[string]any{"status": OccMatched, "bill_id": id,
				"amount": b.Total, "currency": s.home(org), "home_amount": b.Total, "fx_rate": 1, "updated_at": now}).Error
			return id, true, err
		}
		occ := Occurrence{ID: uuid.NewString(), OrganizationID: org, RecurringID: r.ID, Period: period, Seq: o.Index + 1, DueDate: period.AddDate(0, 0, r.DueDays),
			Status: OccMatched, BillID: id, Amount: b.Total, Currency: s.home(org), HomeAmount: b.Total, FxRate: 1, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
		if tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&occ); tx.Error != nil || tx.RowsAffected == 0 {
			return "", false, tx.Error
		}
		return id, true, nil
	}
	return s.createBill(org, r, o, user, snoozedRow)
}

// findImported finds a bill for the schedule's vendor dated near the period
// that no other occurrence claims (track mode).
func (s *Service) findImported(org string, r *RecurringBill, period time.Time) string {
	vendors := []string{r.VendorID}
	if r.ExpenseVendorID != "" && s.Exp != nil {
		if v := s.Exp.FinanceVendorOf(org, r.ExpenseVendorID); v != "" && v != r.VendorID {
			vendors = append(vendors, v)
		}
	}
	win := 10
	if RuleOf(r).Frequency == FreqWeekly {
		win = 3
	}
	var ids []string
	s.DB.Raw(`SELECT b.id FROM bills b WHERE b.organization_id = ? AND b.vendor_id IN ? AND b.bill_date::date BETWEEN ? AND ?
		AND NOT EXISTS (SELECT 1 FROM finance_recurring_occurrences o WHERE o.bill_id = b.id)
		ORDER BY ABS(b.bill_date::date - ?::date), b.created_at LIMIT 1`,
		org, vendors, period.AddDate(0, 0, -win), period.AddDate(0, 0, win), period.Format(dayFmt)).Scan(&ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func periodLabel(r *RecurringBill, d time.Time) string {
	switch r.Frequency {
	case FreqWeekly:
		return "week of " + d.Format("2 Jan 2006")
	case FreqYearly:
		return d.Format("2006") + "/" + d.AddDate(1, 0, 0).Format("06")
	}
	return d.Format("Jan 2006")
}

// createBill creates the finance bill for one period. The occurrence row is
// claimed first (unique per schedule + period), so concurrent runs can never
// create two bills for the same period.
func (s *Service) createBill(org string, r *RecurringBill, o Occ, user string, fromSnoozed bool) (string, bool, error) {
	period := Day(o.Date)
	home := s.home(org)
	rate := 1.0
	amount := r.Amount
	fxNote := ""
	if r.Currency != home {
		q := s.FX.Quote(org, r.Currency, home)
		if !q.OK() {
			return "", false, invalid("no exchange rate for %s → %s — set a manual rate in Invoice settings → Exchange rates", r.Currency, home)
		}
		rate = q.Rate
		amount = fxrates.Convert(r.Amount, rate, home)
		fxNote = fmt.Sprintf(" Original: %s at %s %s per %s (%s).", fmtMoney(r.Amount, r.Currency), trimFloat(rate), home, r.Currency, strings.ToLower(q.Provider))
	}
	amount = r2(amount)
	now := s.now()
	occ := Occurrence{ID: uuid.NewString(), OrganizationID: org, RecurringID: r.ID, Period: period, Seq: o.Index + 1, DueDate: period.AddDate(0, 0, r.DueDays),
		Status: "generating", Amount: r.Amount, Currency: r.Currency, HomeAmount: amount, FxRate: rate, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
	if fromSnoozed {
		// Claim the snooze marker row (keeps its snoozed_until).
		var ex Occurrence
		if s.DB.Where("recurring_id = ? AND period = ?", r.ID, period).First(&ex).Error != nil {
			return "", false, nil
		}
		tx := s.DB.Model(&Occurrence{}).Where("id = ? AND status = ?", ex.ID, "snoozed").Updates(map[string]any{"status": "generating", "amount": r.Amount,
			"currency": r.Currency, "home_amount": amount, "fx_rate": rate, "updated_at": now})
		if tx.Error != nil || tx.RowsAffected == 0 {
			return "", false, tx.Error
		}
		occ.ID = ex.ID
	} else if tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&occ); tx.Error != nil || tx.RowsAffected == 0 {
		return "", false, tx.Error
	}
	unit := amount
	if r.TaxRate > 0 {
		unit = r2(amount / (1 + r.TaxRate/100))
	}
	desc := r.Description
	if desc == "" {
		desc = r.Name
	}
	desc = clip(desc+" — "+periodLabel(r, period), 250)
	status := ""
	if r.CreateAs == "draft" || r.AmountMode == AmountEstimate {
		status = "draft"
	}
	notes := "Recurring bill: " + r.Name + " (" + RuleOf(r).Describe() + ")."
	if r.AmountMode == AmountEstimate {
		notes += " Estimated amount — confirm it and approve the bill."
	}
	if r.AutoPay {
		notes += " Paid automatically by card / direct debit."
	}
	notes += fxNote
	if r.Notes != "" {
		notes += "\n" + r.Notes
	}
	bill, err := s.Fin.CreateBill(org, nz(user, "system"), finance.BillInput{VendorID: r.VendorID, Reference: clip(r.Reference, 120), BillDate: period.Format(dayFmt),
		DueDate: period.AddDate(0, 0, r.DueDays).Format(dayFmt), Notes: notes, Status: status,
		LineItems: []finance.BillLineInput{{Description: desc, AccountID: r.AccountID, Quantity: 1, UnitPrice: unit, TaxRate: r.TaxRate}}})
	if err != nil {
		if fromSnoozed {
			s.DB.Model(&Occurrence{}).Where("id = ?", occ.ID).Update("status", "snoozed")
		} else {
			s.DB.Where("id = ?", occ.ID).Delete(&Occurrence{}) // retried next run
		}
		return "", false, err
	}
	s.DB.Model(&Occurrence{}).Where("id = ?", occ.ID).Updates(map[string]any{"status": OccGenerated, "bill_id": bill.ID, "home_amount": bill.Total, "updated_at": s.now()})
	s.DB.Model(&RecurringBill{}).Where("id = ? AND (last_generated_for IS NULL OR last_generated_for < ?)", r.ID, period).Update("last_generated_for", period)
	return bill.ID, false, nil
}

func nz(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.6f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// ---------------------------------------------------------------- occurrence actions

func (s *Service) periodOf(r *RecurringBill, period string) (Occ, error) {
	d, ok := ParseDay(period)
	if !ok {
		return Occ{}, invalid("invalid period")
	}
	for _, o := range RuleOf(r).Between(d, d, 1) {
		return o, nil
	}
	return Occ{}, invalid("%s is not a date of this schedule", period)
}

// GenerateNow creates (or returns) the bill for one period now.
func (s *Service) GenerateNow(org, user, id, period string) (*finance.Bill, error) {
	r, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	o, err := s.periodOf(r, period)
	if err != nil {
		return nil, err
	}
	var ex Occurrence
	snoozed := false
	if s.DB.Where("recurring_id = ? AND period = ?", r.ID, Day(o.Date)).First(&ex).Error == nil {
		switch {
		case ex.BillID != "":
			return s.Fin.GetBill(org, ex.BillID)
		case ex.Status == OccSkipped:
			return nil, invalid("this period was skipped — restore it first")
		case ex.Status == "snoozed":
			snoozed = true
		default:
			return nil, invalid("this period is already marked paid")
		}
	}
	if r.Mode == ModeTrack {
		return nil, invalid("bills for this schedule come from Expenses imports — mark the period paid instead")
	}
	billID, _, err := s.createBill(org, r, o, user, snoozed)
	if err != nil {
		return nil, err
	}
	if billID == "" {
		return nil, invalid("the bill is being created — try again")
	}
	return s.Fin.GetBill(org, billID)
}

// Skip skips one period (no bill, no reminders).
func (s *Service) Skip(org, user, id, period, note string) error {
	r, err := s.load(org, id)
	if err != nil {
		return err
	}
	o, err := s.periodOf(r, period)
	if err != nil {
		return err
	}
	var ex Occurrence
	if s.DB.Where("recurring_id = ? AND period = ?", r.ID, Day(o.Date)).First(&ex).Error == nil {
		if ex.Status == OccSkipped {
			return nil
		}
		if ex.BillID != "" && ex.Status == OccGenerated {
			var b finance.Bill
			if s.DB.Where("id = ?", ex.BillID).First(&b).Error == nil && b.PaidAmount > 0 {
				return invalid("this period's bill has payments — it can't be skipped")
			}
			if err := s.Fin.DeleteBill(org, nz(user, "system"), ex.BillID); err != nil && !errors.Is(err, finance.ErrNotFound) {
				return err
			}
		}
		return s.DB.Model(&Occurrence{}).Where("id = ?", ex.ID).Updates(map[string]any{"status": OccSkipped, "bill_id": "", "note": clip(note, 480), "updated_at": s.now()}).Error
	}
	now := s.now()
	return s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&Occurrence{ID: uuid.NewString(), OrganizationID: org, RecurringID: r.ID, Period: Day(o.Date), Seq: o.Index + 1,
		DueDate: Day(o.Date).AddDate(0, 0, r.DueDays), Status: OccSkipped, Amount: r.Amount, Currency: r.Currency, Note: clip(note, 480), CreatedBy: user, CreatedAt: now, UpdatedAt: now}).Error
}

// Unskip restores a skipped period (it is generated on the next run).
func (s *Service) Unskip(org, id, period string) error {
	r, err := s.load(org, id)
	if err != nil {
		return err
	}
	d, ok := ParseDay(period)
	if !ok {
		return invalid("invalid period")
	}
	res := s.DB.Where("recurring_id = ? AND period = ? AND status = ?", r.ID, d, OccSkipped).Delete(&Occurrence{})
	if res.RowsAffected == 0 {
		return invalid("this period is not skipped")
	}
	// A restored past period must be generated even if it is before the
	// catch-up boundary.
	if r.ResumedAt != nil && d.Before(Day(*r.ResumedAt)) {
		s.DB.Model(&RecurringBill{}).Where("id = ?", r.ID).Update("resumed_at", d)
	}
	return nil
}

// MarkPaid marks a period paid when there is no bill to pay (track mode, or
// a bill handled outside the system).
func (s *Service) MarkPaid(org, user, id, period, note string) error {
	r, err := s.load(org, id)
	if err != nil {
		return err
	}
	o, err := s.periodOf(r, period)
	if err != nil {
		return err
	}
	var ex Occurrence
	now := s.now()
	if s.DB.Where("recurring_id = ? AND period = ?", r.ID, Day(o.Date)).First(&ex).Error == nil {
		if ex.BillID != "" {
			return invalid("record the payment on the bill")
		}
		return s.DB.Model(&Occurrence{}).Where("id = ?", ex.ID).Updates(map[string]any{"status": OccPaid, "paid_at": now, "note": clip(note, 480), "updated_at": now}).Error
	}
	if r.Mode == ModeCreate {
		return invalid("create the bill and record the payment on it")
	}
	return s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&Occurrence{ID: uuid.NewString(), OrganizationID: org, RecurringID: r.ID, Period: Day(o.Date), Seq: o.Index + 1,
		DueDate: Day(o.Date).AddDate(0, 0, r.DueDays), Status: OccPaid, PaidAt: &now, Amount: r.Amount, Currency: r.Currency, Note: clip(note, 480), CreatedBy: user, CreatedAt: now, UpdatedAt: now}).Error
}

// Snooze stops reminders until a date, for a bill or a (track-mode) period.
func (s *Service) Snooze(org, billID, recurringID, period string, days int, until string) (time.Time, error) {
	today := s.Today(org)
	u, ok := ParseDay(until)
	if !ok {
		if days <= 0 {
			days = 3
		}
		u = today.AddDate(0, 0, days)
	}
	if !u.After(today) || u.After(today.AddDate(0, 0, 90)) {
		return u, invalid("snooze for 1 to 90 days")
	}
	if billID != "" {
		var n int64
		s.DB.Model(&finance.Bill{}).Where("organization_id = ? AND id = ?", org, billID).Count(&n)
		if n == 0 {
			return u, ErrNotFound
		}
		// A bill created by a schedule: snooze its occurrence too.
		s.DB.Model(&Occurrence{}).Where("organization_id = ? AND bill_id = ?", org, billID).Update("snoozed_until", u)
		return u, s.DB.Save(&BillSnooze{BillID: billID, OrganizationID: org, Until: u, UpdatedAt: s.now()}).Error
	}
	r, err := s.load(org, recurringID)
	if err != nil {
		return u, err
	}
	o, err := s.periodOf(r, period)
	if err != nil {
		return u, err
	}
	var ex Occurrence
	if s.DB.Where("recurring_id = ? AND period = ?", r.ID, Day(o.Date)).First(&ex).Error == nil {
		if ex.BillID != "" {
			s.DB.Save(&BillSnooze{BillID: ex.BillID, OrganizationID: org, Until: u, UpdatedAt: s.now()})
		}
		return u, s.DB.Model(&Occurrence{}).Where("id = ?", ex.ID).Update("snoozed_until", u).Error
	}
	now := s.now()
	// No bill yet: keep a marker row that only carries the snooze (status
	// "snoozed" is not "handled" — the bill is still created / matched).
	return u, s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&Occurrence{ID: uuid.NewString(), OrganizationID: org, RecurringID: r.ID, Period: Day(o.Date), Seq: o.Index + 1,
		DueDate: Day(o.Date).AddDate(0, 0, r.DueDays), Status: "snoozed", SnoozedUntil: &u, Amount: r.Amount, Currency: r.Currency, CreatedAt: now, UpdatedAt: now}).Error
}

// ---------------------------------------------------------------- subscriptions

// SubscriptionView is a detected subscription with its link state.
type SubscriptionView struct {
	expenses.Subscription
	LinkedID   string `json:"linked_recurring_id,omitempty"`
	LinkedName string `json:"linked_recurring_name,omitempty"`
}

// Subscriptions lists Expenses' detected subscriptions with the recurring
// bill each is linked to.
func (s *Service) Subscriptions(org string) []SubscriptionView {
	out := []SubscriptionView{}
	if s.Exp == nil {
		return out
	}
	links := map[string]RecurringBill{}
	var list []RecurringBill
	s.DB.Where("organization_id = ? AND subscription_key <> ''", org).Find(&list)
	for _, r := range list {
		links[r.SubscriptionKey] = r
	}
	for _, sb := range s.Exp.SubscriptionsFor(org, s.Today(org)) {
		v := SubscriptionView{Subscription: sb}
		if r, ok := links[sb.Key]; ok {
			v.LinkedID, v.LinkedName = r.ID, r.Name
		}
		out = append(out, v)
	}
	return out
}

// FromSubscription converts a detected subscription into a recurring bill
// in track mode (Expenses keeps creating the bills, so nothing is counted
// twice); the caller may override fields with in.
func (s *Service) FromSubscription(org, user, key string, in *Input) (*RecurringBill, error) {
	if s.Exp == nil {
		return nil, invalid("Expenses is not available")
	}
	var sub *expenses.Subscription
	for _, sb := range s.Exp.SubscriptionsFor(org, s.Today(org)) {
		if sb.Key == key {
			x := sb
			sub = &x
			break
		}
	}
	if sub == nil {
		return nil, invalid("that subscription is no longer detected")
	}
	var n int64
	s.DB.Model(&RecurringBill{}).Where("organization_id = ? AND subscription_key = ?", org, key).Count(&n)
	if n > 0 {
		return nil, invalid("this subscription is already a recurring bill")
	}
	vendorID, accountID, err := s.Exp.SubscriptionTarget(org, *sub)
	if err != nil {
		return nil, err
	}
	freq, interval := FreqMonthly, 1
	switch sub.Months {
	case 1:
	case 3:
		freq = FreqQuarterly
	case 12:
		freq = FreqYearly
	default:
		if sub.Months > 1 {
			freq, interval = FreqCustom, sub.Months
		}
	}
	start := sub.NextDate
	if start == "" {
		start = s.Today(org).Format(dayFmt)
	}
	name := strings.TrimSpace(sub.Vendor)
	if sub.Service != "" && !strings.EqualFold(sub.Service, sub.Vendor) {
		name += " — " + sub.Service
	}
	base := Input{Name: name, VendorID: vendorID, AccountID: accountID, Description: nz(sub.Service, sub.Vendor), Amount: sub.LastOrig, Currency: sub.Currency,
		AmountMode: AmountEstimate, Frequency: freq, Interval: interval, StartDate: start, EndMode: EndNever, CreateAs: "draft", Mode: ModeTrack,
		Service: sub.Service, ExpenseVendorID: sub.VendorID, SubscriptionKey: sub.Key, AutoPay: true}
	if in != nil {
		if in.Name != "" {
			base.Name = in.Name
		}
		if in.Mode != "" {
			base.Mode = in.Mode
		}
		if in.StartDate != "" {
			base.StartDate = in.StartDate
		}
		if in.Amount > 0 {
			base.Amount = in.Amount
		}
		if in.ExtraRecipients != nil {
			base.ExtraRecipients = in.ExtraRecipients
		}
		base.AutoPay = in.AutoPay
		base.DueDays = in.DueDays
		base.SynergyDomain = in.SynergyDomain
	}
	if base.Amount <= 0 {
		return nil, invalid("the subscription has no amount — create the recurring bill by hand")
	}
	return s.Create(org, user, base)
}
