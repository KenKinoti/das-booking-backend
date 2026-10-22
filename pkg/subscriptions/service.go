package subscriptions

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
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

// Service runs subscriptions, billing, dunning and suspension.
type Service struct {
	DB   *gorm.DB
	FX   *fxrates.Service
	Exp  *expenses.Service // optional: Synergy hosted sites, their costs and domain expiry
	Mail *mail.Service
	Inv  Invoicer
	// Supplier suspends / unsuspends hosting at the provider (Synergy
	// Wholesale through its API, see synergy.go). nil = every suspension is
	// a task for an administrator.
	Supplier SupplierActions
	Now      func() time.Time
	// AppURL is the web app origin (PUBLIC_APP_URL) for links in emails.
	AppURL string
	// LogoURL returns the organisation's absolute logo URL ("" for none).
	LogoURL func(org string) string
	// Secret signs the public "Your services" links.
	Secret []byte
	// TestClock enables the per-organisation test clock (SUBSCRIPTIONS_TEST_CLOCK).
	TestClock bool
	// SitesFn replaces the Synergy hosted sites report (tests).
	SitesFn func(org string) []expenses.SynSite
}

// New builds a service.
func New(db *gorm.DB, fx *fxrates.Service, exp *expenses.Service) *Service {
	if fx == nil {
		fx = &fxrates.Service{DB: db}
	}
	return &Service{DB: db, FX: fx, Exp: exp, Mail: mail.Shared(db), Now: time.Now,
		TestClock: strings.TrimSpace(os.Getenv("SUBSCRIPTIONS_TEST_CLOCK")) != "" && os.Getenv("SUBSCRIPTIONS_TEST_CLOCK") != "0"}
}

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

// Today is the organisation's local date (moved by the test clock when it is on).
func (s *Service) Today(org string) time.Time {
	d := LocalDay(s.now(), s.Loc(org))
	if s.TestClock {
		var off []int
		s.DB.Raw(`SELECT test_offset_days FROM subscription_settings WHERE organization_id = ?`, org).Scan(&off)
		if len(off) > 0 && off[0] != 0 {
			d = d.AddDate(0, 0, off[0])
		}
	}
	return d
}

func (s *Service) home(org string) string { return orgprefs.Currency(s.DB, org) }

func clip(v string, n int) string {
	v = strings.TrimSpace(v)
	if len([]rune(v)) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

func hasTable(db *gorm.DB, name string) bool { return db.Migrator().HasTable(name) }

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

func nonEmpty(v ...string) []string {
	out := []string{}
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			out = append(out, strings.TrimSpace(x))
		}
	}
	return out
}

// ---------------------------------------------------------------- settings

// SettingsFor returns the organisation's settings (defaults when none saved).
func (s *Service) SettingsFor(org string) Settings {
	var st Settings
	if err := s.DB.Where("organization_id = ?", org).First(&st).Error; err != nil {
		return DefaultSettings(org)
	}
	if st.ClientDays == nil {
		st.ClientDays = []int{}
	}
	if st.AdminDays == nil || len(st.AdminDays) == 0 {
		st.AdminDays = []int{MinNoticeDays}
	}
	if st.DomainDays == nil {
		st.DomainDays = []int{}
	}
	if st.Recipients == nil {
		st.Recipients = []string{}
	}
	if st.NumberPrefix == "" {
		st.NumberPrefix = "SUB-"
	}
	return st
}

func (s *Service) ensureSettings(db *gorm.DB, org string) {
	st := DefaultSettings(org)
	st.UpdatedAt = s.now()
	db.Clauses(clause.OnConflict{DoNothing: true}).Create(&st)
}

// SettingsInput updates settings (nil = unchanged).
type SettingsInput struct {
	Enabled         *bool     `json:"enabled"`
	LeadMonthly     *int      `json:"lead_monthly"`
	LeadQuarterly   *int      `json:"lead_quarterly"`
	LeadYearly      *int      `json:"lead_yearly"`
	AutoSend        *bool     `json:"auto_send"`
	GraceDays       *int      `json:"grace_days"`
	AutoSuspend     *bool     `json:"auto_suspend"`
	ClientDays      *[]int    `json:"client_days"`
	FinalNoticeDays *int      `json:"final_notice_days"`
	AdminDays       *[]int    `json:"admin_days"`
	Digest          *bool     `json:"digest"`
	DigestHour      *int      `json:"digest_hour"`
	SendHour        *int      `json:"send_hour"`
	InApp           *bool     `json:"in_app"`
	DomainDays      *[]int    `json:"domain_days"`
	Recipients      *[]string `json:"recipients"`
	UseSynergyAPI   *bool     `json:"use_synergy_api"`
	TargetMargin    *float64  `json:"target_margin"`
	NumberPrefix    *string   `json:"number_prefix"`
}

func cleanDays(l []int, min, max, most int, what string) ([]int, error) {
	seen := map[int]bool{}
	out := []int{}
	for _, d := range l {
		if d < min || d > max {
			return nil, invalid("%s must be between %d and %d days", what, min, max)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) > most {
		return nil, invalid("use at most %d %s", most, what)
	}
	sort.Ints(out)
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
	setB(&st.AutoSend, in.AutoSend)
	setB(&st.AutoSuspend, in.AutoSuspend)
	setB(&st.Digest, in.Digest)
	setB(&st.InApp, in.InApp)
	setB(&st.UseSynergyAPI, in.UseSynergyAPI)
	intIn := func(dst *int, v *int, min, max int, msg string) error {
		if v == nil {
			return nil
		}
		if *v < min || *v > max {
			return invalid("%s", msg)
		}
		*dst = *v
		return nil
	}
	for _, e := range []error{
		intIn(&st.LeadMonthly, in.LeadMonthly, 0, 27, "create monthly invoices 0 to 27 days before the period starts"),
		intIn(&st.LeadQuarterly, in.LeadQuarterly, 0, 60, "create quarterly invoices 0 to 60 days before the period starts"),
		intIn(&st.LeadYearly, in.LeadYearly, 0, 90, "create yearly invoices 0 to 90 days before the period starts"),
		intIn(&st.GraceDays, in.GraceDays, 0, 60, "the grace period is 0 to 60 days after the due date"),
		intIn(&st.FinalNoticeDays, in.FinalNoticeDays, 0, 14, "send the final notice 0 to 14 days before suspension"),
		intIn(&st.DigestHour, in.DigestHour, 0, 23, "choose an hour between 0 and 23"),
		intIn(&st.SendHour, in.SendHour, 0, 23, "choose an hour between 0 and 23"),
	} {
		if e != nil {
			return st, e
		}
	}
	if in.ClientDays != nil {
		l, err := cleanDays(*in.ClientDays, -30, 30, 8, "client reminders")
		if err != nil {
			return st, err
		}
		st.ClientDays = l
	}
	if in.AdminDays != nil {
		l, err := cleanDays(*in.AdminDays, 1, 30, 4, "admin alerts")
		if err != nil {
			return st, err
		}
		if len(l) == 0 || l[len(l)-1] < MinNoticeDays {
			return st, invalid("the first admin alert must be at least %d days before the suspension, so there is time to call the client", MinNoticeDays)
		}
		st.AdminDays = l
	}
	if in.DomainDays != nil {
		l, err := cleanDays(*in.DomainDays, 1, 120, 5, "domain reminders")
		if err != nil {
			return st, err
		}
		st.DomainDays = l
	}
	if in.TargetMargin != nil {
		if *in.TargetMargin < 0 || *in.TargetMargin > 90 || math.IsNaN(*in.TargetMargin) {
			return st, invalid("the target margin is 0 to 90%%")
		}
		st.TargetMargin = *in.TargetMargin
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
	if in.NumberPrefix != nil {
		p := strings.TrimSpace(*in.NumberPrefix)
		if p == "" || len(p) > 10 {
			return st, invalid("the number prefix is 1 to 10 characters")
		}
		st.NumberPrefix = p
	}
	st.OrganizationID, st.UpdatedAt = org, s.now()
	if err := s.DB.Omit("test_offset_days", "next_number", "last_run_at", "last_digest").Save(&st).Error; err != nil {
		return st, err
	}
	return s.SettingsFor(org), nil
}

// SetTestClock moves the organisation's subscription clock (test environments only).
func (s *Service) SetTestClock(org string, date string, days *int, reset bool) (time.Time, int, error) {
	if !s.TestClock {
		return time.Time{}, 0, ErrNotFound
	}
	s.ensureSettings(s.DB, org)
	real := LocalDay(s.now(), s.Loc(org))
	off := s.SettingsFor(org).TestOffsetDays
	switch {
	case reset:
		off = 0
	case date != "":
		d, ok := ParseDay(date)
		if !ok {
			return time.Time{}, 0, invalid("date must be YYYY-MM-DD")
		}
		off = DaysBetween(real, d)
	case days != nil:
		off += *days
	}
	if off < -3650 || off > 3650 {
		return time.Time{}, 0, invalid("the test clock moves at most 10 years")
	}
	s.DB.Model(&Settings{}).Where("organization_id = ?", org).Update("test_offset_days", off)
	return real.AddDate(0, 0, off), off, nil
}

// nextNumber reserves the next subscription number (SUB-0001).
func (s *Service) nextNumber(tx *gorm.DB, org string) (string, error) {
	s.ensureSettings(tx, org)
	var row struct {
		N      int
		Prefix string
	}
	if err := tx.Raw(`UPDATE subscription_settings SET next_number = COALESCE(next_number, 1) + 1 WHERE organization_id = ?
		RETURNING next_number - 1 AS n, COALESCE(NULLIF(number_prefix, ''), 'SUB-') AS prefix`, org).Scan(&row).Error; err != nil {
		return "", err
	}
	if row.N < 1 {
		row.N = 1
	}
	for i := 0; i < 50; i++ {
		num := fmt.Sprintf("%s%04d", row.Prefix, row.N+i)
		var n int64
		tx.Model(&Subscription{}).Where("organization_id = ? AND number = ?", org, num).Count(&n)
		if n == 0 {
			if i > 0 {
				tx.Exec(`UPDATE subscription_settings SET next_number = ? WHERE organization_id = ?`, row.N+i+1, org)
			}
			return num, nil
		}
	}
	return "", errors.New("could not number the subscription")
}

// ---------------------------------------------------------------- customers

type customerInfo struct {
	ID, Name, Contact, Email, Phone, Address, Currency, CountryCode, Country, Website string
	Terms                                                                             *int
}

func (s *Service) customers(org string, ids []string) map[string]customerInfo {
	out := map[string]customerInfo{}
	if len(ids) == 0 || !hasTable(s.DB, "customers") {
		return out
	}
	var rows []struct {
		ID, FirstName, LastName, CompanyName, Email, Phone, AddressStreet, AddressSuburb, AddressState, AddressPostcode, AddressCountry, CountryCode, Currency, Website string
		PaymentTermsDays                                                                                                                                                *int
	}
	s.DB.Raw(`SELECT id, first_name, last_name, COALESCE(company_name,'') AS company_name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone,
		COALESCE(address_street,'') AS address_street, COALESCE(address_suburb,'') AS address_suburb, COALESCE(address_state,'') AS address_state,
		COALESCE(address_postcode,'') AS address_postcode, COALESCE(address_country,'') AS address_country, COALESCE(country_code,'') AS country_code,
		COALESCE(currency,'') AS currency, COALESCE(website,'') AS website, payment_terms_days
		FROM customers WHERE organization_id = ? AND id IN ? AND deleted_at IS NULL`, org, ids).Scan(&rows)
	for _, r := range rows {
		person := strings.TrimSpace(r.FirstName + " " + r.LastName)
		name := person
		if c := strings.TrimSpace(r.CompanyName); c != "" {
			name = c // the business is the client; the person is the contact
		}
		addr := strings.Join(nonEmpty(r.AddressStreet, strings.TrimSpace(strings.Join(nonEmpty(r.AddressSuburb, r.AddressState, r.AddressPostcode), " ")), r.AddressCountry), "\n")
		out[r.ID] = customerInfo{ID: r.ID, Name: name, Contact: person, Email: strings.TrimSpace(r.Email), Phone: r.Phone, Address: addr, Currency: r.Currency,
			CountryCode: r.CountryCode, Country: r.AddressCountry, Website: r.Website, Terms: r.PaymentTermsDays}
	}
	return out
}

func (s *Service) customer(org, id string) (customerInfo, bool) {
	m := s.customers(org, []string{id})
	c, ok := m[id]
	return c, ok
}

// ---------------------------------------------------------------- catalogue

// CatalogService is a service from the catalogue (Settings → Services),
// with the billing period and kind read from its name.
type CatalogService struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Label            string  `json:"label"` // the name without a trailing period ("Starter hosting")
	Category         string  `json:"category"`
	Description      string  `json:"description"`
	Price            float64 `json:"price"`
	Currency         string  `json:"currency"`
	PriceIncludesTax bool    `json:"price_includes_tax"`
	Period           string  `json:"period"` // monthly | quarterly | yearly | "" (unknown: the price is per billing period)
	Kind             string  `json:"kind"`
	Active           bool    `json:"active"`
}

var (
	reYearly    = regexp.MustCompile(`(?i)\b(yearly|annual(ly)?|per year|a year|1 ?year|12 ?months?)\b|/ ?(yr|year)\b`)
	reQuarterly = regexp.MustCompile(`(?i)\b(quarterly|per quarter|3 ?months?)\b|/ ?(qtr|quarter)\b`)
	reMonthly   = regexp.MustCompile(`(?i)\b(monthly|per month|a month|1 ?month)\b|/ ?(mo|mth|month)\b`)
	reSuffix    = regexp.MustCompile(`(?i)\s*(?:[—–-]\s*|\()(?:monthly|quarterly|yearly|annual(?:ly)?|per (?:month|year|quarter))\)?\s*$`)
)

// InferPeriod reads a billing period from a service's name (then its
// description): "Starter hosting — yearly" → yearly ("" when it says none).
func InferPeriod(name, description string) string {
	for _, t := range []string{name, description} {
		switch {
		case reYearly.MatchString(t):
			return PeriodYearly
		case reQuarterly.MatchString(t):
			return PeriodQuarterly
		case reMonthly.MatchString(t):
			return PeriodMonthly
		}
	}
	return ""
}

// InferKind reads what a service is from its name and category.
func InferKind(name, category string) string {
	t := strings.ToLower(name)
	c := strings.ToLower(category)
	hosting := strings.Contains(t, "hosting") || strings.Contains(t, "cpanel") || strings.Contains(t, " env") || strings.Contains(t, "vps") || strings.Contains(t, "server")
	switch {
	case hosting:
		return KindHosting
	case strings.Contains(t, "domain") || strings.HasPrefix(strings.TrimSpace(t), "."):
		return KindDomain
	case strings.Contains(t, "email") || strings.Contains(t, "mailbox") || strings.Contains(t, "workspace"):
		return KindEmail
	case strings.Contains(t, "maintenance") || strings.Contains(t, "support") || strings.Contains(t, "care plan"):
		return KindMaintenance
	case strings.Contains(c, "hosting"):
		return KindHosting
	}
	return KindOther
}

// ServiceLabel is a service name without a trailing billing period.
func ServiceLabel(name string) string {
	l := strings.TrimSpace(reSuffix.ReplaceAllString(name, ""))
	if l == "" {
		return strings.TrimSpace(name)
	}
	return l
}

// catalog loads catalogue services (ids nil = every active one).
func (s *Service) catalog(org string, ids []string) map[string]CatalogService {
	out := map[string]CatalogService{}
	if !hasTable(s.DB, "services") {
		return out
	}
	var rows []struct {
		ID, Name, Category, Description, Currency string
		Price                                     float64
		PriceIncludesTax, IsActive                bool
	}
	q := `SELECT id, name, COALESCE(category,'') AS category, COALESCE(description,'') AS description, price, COALESCE(currency,'') AS currency,
		COALESCE(price_includes_tax, false) AS price_includes_tax, COALESCE(is_active, true) AS is_active FROM services WHERE organization_id = ? AND deleted_at IS NULL`
	args := []any{org}
	if ids != nil {
		if len(ids) == 0 {
			return out
		}
		q += ` AND id IN ?`
		args = append(args, ids)
	} else {
		q += ` AND COALESCE(is_active, true)`
	}
	s.DB.Raw(q+` ORDER BY LOWER(name)`, args...).Scan(&rows)
	home := s.home(org)
	for _, r := range rows {
		cur := strings.ToUpper(strings.TrimSpace(r.Currency))
		if cur == "" {
			cur = home
		}
		out[r.ID] = CatalogService{ID: r.ID, Name: r.Name, Label: ServiceLabel(r.Name), Category: r.Category, Description: r.Description, Price: r.Price,
			Currency: cur, PriceIncludesTax: r.PriceIncludesTax, Period: InferPeriod(r.Name, r.Description), Kind: InferKind(r.Name, r.Category), Active: r.IsActive}
	}
	return out
}

// Catalog lists the active catalogue services, by name.
func (s *Service) Catalog(org string) []CatalogService {
	m := s.catalog(org, nil)
	out := make([]CatalogService, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// TaxDefaults are the invoicing settings that decide line prices.
type TaxDefaults struct {
	Rate             float64 `json:"default_tax_rate"`
	PricesIncludeTax bool    `json:"prices_include_tax"`
	Label            string  `json:"tax_label"`
}

// taxDefaults reads the invoicing settings (no row: no tax, prices before tax).
func (s *Service) taxDefaults(org string) TaxDefaults {
	t := TaxDefaults{Label: "Tax"}
	if !hasTable(s.DB, "billing_settings") {
		return t
	}
	var rows []struct {
		DefaultTaxRate   float64
		PricesIncludeTax bool
		TaxLabel         string
	}
	s.DB.Raw(`SELECT COALESCE(default_tax_rate, 0) AS default_tax_rate, COALESCE(prices_include_tax, false) AS prices_include_tax, COALESCE(tax_label, '') AS tax_label
		FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&rows)
	if len(rows) > 0 {
		t.Rate, t.PricesIncludeTax, t.Label = rows[0].DefaultTaxRate, rows[0].PricesIncludeTax, firstNonEmpty(rows[0].TaxLabel, "Tax")
	}
	return t
}

// rate is home units per 1 unit of cur (0 when no rate is known).
func (s *Service) rate(org, cur, home string) float64 {
	if cur == "" || cur == home {
		return 1
	}
	q := s.FX.Quote(org, cur, home)
	if !q.OK() {
		return 0
	}
	return q.Rate
}

// convert converts v from one currency to another at today's rate.
func (s *Service) convert(org string, v float64, from, to string) (float64, bool) {
	if from == to || from == "" {
		return v, true
	}
	q := s.FX.Quote(org, from, to)
	if !q.OK() {
		return 0, false
	}
	return v * q.Rate, true
}

// found loads the first row of q into dest. A miss is not an error (and, unlike
// First, is not logged as one): most lookups here expect to miss sometimes.
func found(q *gorm.DB, dest any) bool {
	r := q.Limit(1).Find(dest)
	return r.Error == nil && r.RowsAffected > 0
}
