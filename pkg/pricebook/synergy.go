package pricebook

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
)

// Auto-fill the Synergy Wholesale price list from what the organisation
// already has:
//
//   - hosting plans: the monthly price and disk limit of every plan learned
//     from the uploaded Synergy statements and usage reports
//     (expenses.LearnPlans — AUD as charged, GST included);
//   - domain prices: register / renew / transfer per TLD from the Synergy
//     API (getDomainPricing) when the account is connected; otherwise the
//     renewal prices actually charged on the statements.
//
// pkg/expenses is only read.

// commonTLDs are pulled from the Synergy price list even when the
// organisation has no domain on them yet.
var commonTLDs = []string{".com", ".com.au", ".au", ".net", ".org", ".co.ke", ".ke", ".net.au", ".org.au", ".co", ".io", ".africa", ".co.za", ".co.uk", ".info", ".biz", ".online"}

// twoLabel are second-level suffixes under which domains are registered.
var twoLabel = map[string]bool{}

func init() {
	for _, cc := range []string{"au", "ke", "uk", "za", "nz", "ug", "tz", "ng", "gh", "in", "sg", "my", "jp", "br", "rw", "zm", "id", "hk"} {
		for _, sl := range []string{"com", "co", "net", "org", "or", "ne", "ac", "go", "gov", "edu", "id", "info", "me", "sc", "asn", "mobi", "ltd", "plc"} {
			twoLabel[sl+"."+cc] = true
		}
	}
}

// TLDOfDomain returns the ending a domain is registered under
// ("shop.example.co.ke" → ".co.ke", "example.com" → ".com").
func TLDOfDomain(domain string) string {
	d := strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	parts := strings.Split(d, ".")
	if len(parts) < 2 {
		return ""
	}
	if len(parts) >= 3 {
		if two := strings.Join(parts[len(parts)-2:], "."); twoLabel[two] {
			return "." + two
		}
	}
	return "." + parts[len(parts)-1]
}

// DomainPrice is one TLD's prices on the Synergy list (AUD).
type DomainPrice struct {
	TLD      string
	Register *float64
	Renew    *float64
	Transfer *float64
}

func soapList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		if it, ok := t["item"]; ok {
			return soapList(it)
		}
		return []any{t}
	}
	return nil
}

func soapNum(m map[string]any, names ...string) *float64 {
	for _, n := range names {
		for k, v := range m {
			if !strings.EqualFold(k, n) {
				continue
			}
			s := strings.ReplaceAll(strings.TrimSpace(fmt.Sprint(v)), ",", "")
			if s == "" || v == nil {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimPrefix(s, "$"), 64); err == nil && f > 0 && !math.IsInf(f, 0) {
				return &f
			}
		}
	}
	return nil
}

// ParseDomainPricing reads a getDomainPricing reply.
func ParseDomainPricing(m map[string]any) []DomainPrice {
	out := []DomainPrice{}
	for _, it := range soapList(m["pricing"]) {
		p, _ := it.(map[string]any)
		if p == nil {
			continue
		}
		tld := NormTLD(fmt.Sprint(p["tld"]))
		if tld == "" {
			continue
		}
		dp := DomainPrice{TLD: tld,
			Register: soapNum(p, "register_1_year", "register_1_years", "register1Year", "register_1year", "register", "registration"),
			Renew:    soapNum(p, "renew", "renew_1_year", "renewal"),
			Transfer: soapNum(p, "transfer", "transfer_1_year")}
		if dp.Register == nil && dp.Renew == nil && dp.Transfer == nil {
			continue
		}
		out = append(out, dp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TLD < out[j].TLD })
	return out
}

// SynergyPricing fetches the domain price list (tests replace it).
type SynergyPricing func(ctx context.Context, db *gorm.DB, org string) ([]DomainPrice, error)

// ErrNotConnected means the Synergy API has no credentials for this
// organisation.
var ErrNotConnected = errors.New("Synergy Wholesale is not connected")

// FetchSynergyPricing calls getDomainPricing with the organisation's stored
// reseller ID and API key (Settings → Expense sources).
func FetchSynergyPricing(ctx context.Context, db *gorm.DB, org string) ([]DomainPrice, error) {
	if !hasTable(db, "expense_connections") {
		return nil, ErrNotConnected
	}
	var c expenses.Connection
	if err := db.Where("organization_id = ? AND kind = ?", org, "synergy").Take(&c).Error; err != nil || c.Account == "" || c.SecretEnc == "" || !c.Enabled {
		return nil, ErrNotConnected
	}
	key, err := encryption.GlobalEncryption.Decrypt(c.SecretEnc)
	if err != nil || key == "" {
		return nil, errors.New("the stored Synergy API key could not be read — enter it again in Settings → Expense sources")
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	m, err := expenses.NewSynergyClient(c.Account, key).Call(ctx, "getDomainPricing")
	if err != nil {
		return nil, err
	}
	return ParseDomainPricing(m), nil
}

// AutofillOptions narrows the auto-fill.
type AutofillOptions struct {
	TLDs    []string `json:"tlds"`     // extra TLDs to pull from the API
	AllTLDs bool     `json:"all_tlds"` // every TLD on the Synergy list
}

// AutofillResult says what the auto-fill did.
type AutofillResult struct {
	SupplierID    string   `json:"supplier_id"`
	Supplier      string   `json:"supplier"`
	Plans         int      `json:"plans"`   // hosting plans found on the statements
	Domains       int      `json:"domains"` // domain prices found
	Created       int      `json:"created"`
	Updated       int      `json:"updated"`
	Unchanged     int      `json:"unchanged"`
	DomainSource  string   `json:"domain_source"` // api | statements | none
	APIConnected  bool     `json:"api_connected"`
	APIError      string   `json:"api_error,omitempty"`
	HasStatements bool     `json:"has_statements"`
	Notes         []string `json:"notes"`
	Message       string   `json:"message"`
}

// synergySupplier finds (or creates) the Synergy Wholesale supplier.
func (s *Service) synergySupplier(org string) (*Supplier, error) {
	var sp Supplier
	err := s.DB.Where("organization_id = ? AND system = ?", org, "synergy").Order("created_at").Take(&sp).Error
	if err != nil {
		err = s.DB.Where("organization_id = ? AND name_key LIKE ? AND type = ?", org, "%synergy%", TypeWholesale).Order("created_at").Take(&sp).Error
	}
	if err == nil {
		return &sp, nil
	}
	name, typ, cur := "Synergy Wholesale", TypeWholesale, "AUD"
	return s.SaveSupplier(org, "", SupplierInput{Name: &name, Type: &typ, Currency: &cur})
}

// upsert saves one learned price; it never overwrites a row someone
// de-activated, and counts what happened.
func (s *Service) autofillItem(org, user string, sp *Supplier, in ItemInput, source string, today time.Time, res *AutofillResult) {
	var cur Item
	key := NameKey(in.Name.V)
	if IsDomainKind(in.Kind.V) {
		key = NameKey(DomainItemName(in.Kind.V, in.TLD.V))
	}
	found := s.DB.Where("organization_id = ? AND supplier_id = ? AND kind = ? AND name_key = ? AND tld = ? AND period = ?", org, sp.ID, in.Kind.V, key, in.TLD.V, in.Period.V).Take(&cur).Error == nil
	id := ""
	if found {
		id = cur.ID
		sameDisk := !in.DiskGB.Set || samePtr(cur.DiskGB, &in.DiskGB.V)
		if abs(cur.Price-in.Price.V) < 0.005 && sameDisk {
			// Same price: only confirm it is still current.
			if cur.AsOf == nil || Day(*cur.AsOf).Before(Day(today)) {
				d := Day(today)
				s.DB.Model(&Item{}).Where("id = ?", cur.ID).Updates(map[string]any{"as_of": d, "updated_at": s.now()})
			}
			res.Unchanged++
			return
		}
		// Keep the notes and link someone typed.
		in.Notes, in.SourceURL = Opt[string]{}, Opt[string]{}
	}
	if _, err := s.SaveItem(org, user, id, in, source, today); err != nil {
		res.Notes = append(res.Notes, in.Name.V+in.TLD.V+": "+err.Error())
		return
	}
	if found {
		res.Updated++
	} else {
		res.Created++
	}
}

// AutofillSynergy fills the Synergy Wholesale price list from the
// statements (hosting plans) and the API or statements (domains).
func (s *Service) AutofillSynergy(ctx context.Context, org, user string, opts AutofillOptions, pricing SynergyPricing, today time.Time) (*AutofillResult, error) {
	s.EnsureSeed(org)
	sp, err := s.synergySupplier(org)
	if err != nil {
		return nil, err
	}
	res := &AutofillResult{SupplierID: sp.ID, Supplier: sp.Name, Notes: []string{}, DomainSource: "none"}
	asOf := Day(today).Format("2006-01-02")

	// ---- hosting plans learned from the statements
	var txns []expenses.SynergyTxn
	var snaps []expenses.SynergyUsage
	var manual []expenses.SynergyPlan
	if hasTable(s.DB, "synergy_transactions") {
		s.DB.Where("organization_id = ?", org).Order("date, seq").Find(&txns)
		s.DB.Where("organization_id = ?", org).Order("snapshot_date, domain").Find(&snaps)
		s.DB.Where("organization_id = ?", org).Find(&manual)
	}
	res.HasStatements = len(txns) > 0 || len(snaps) > 0
	if res.HasStatements {
		plans, _ := expenses.LearnPlans(txns, snaps, manual)
		keys := []string{}
		for k := range plans {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := plans[k]
			if p.MonthlyPrice == nil || *p.MonthlyPrice <= 0 {
				res.Notes = append(res.Notes, p.Name+": no monthly price on the statements yet")
				continue
			}
			res.Plans++
			note := "Learned from your Synergy statements"
			switch {
			case p.Manual:
				note = "Price set by you in Expenses → Synergy → Plan prices"
			case p.PriceSource == "plan change":
				note = "Estimated from a plan-change pro-rata on your Synergy statements"
			case p.Evidence != "":
				note += " (" + p.Evidence + ")"
			}
			note += " · AUD as charged, GST included"
			in := ItemInput{SupplierID: Of(sp.ID), Kind: Of(KindHosting), Name: Of(p.Name), Period: Of(PeriodMonth), Price: Of(math.Round(*p.MonthlyPrice*100) / 100),
				Currency: Of("AUD"), Sites: Of(1), AsOf: Of(asOf), Notes: Of(note), Active: Of(true)}
			if p.LimitMB != nil && *p.LimitMB > 0 {
				in.DiskGB = Of(math.Round(*p.LimitMB/1024*1000) / 1000)
			}
			s.autofillItem(org, user, sp, in, "synergy_statements", today, res)
		}
	}

	// ---- domain prices: the API first, then the statements
	inUse := map[string]bool{}
	for _, t := range txns {
		if t.Domain != "" {
			if tld := TLDOfDomain(t.Domain); tld != "" {
				inUse[tld] = true
			}
		}
	}
	for _, u := range snaps {
		if tld := TLDOfDomain(u.Domain); tld != "" {
			inUse[tld] = true
		}
	}
	if hasTable(s.DB, "expense_renewals") {
		var names []string
		s.DB.Raw(`SELECT name FROM expense_renewals WHERE organization_id = ? AND kind = 'domain' AND active`, org).Scan(&names)
		for _, n := range names {
			if tld := TLDOfDomain(n); tld != "" {
				inUse[tld] = true
			}
		}
	}
	if pricing == nil {
		pricing = FetchSynergyPricing
	}
	list, perr := pricing(ctx, s.DB, org)
	switch {
	case perr == nil:
		res.APIConnected = true
		want := map[string]bool{}
		for t := range inUse {
			want[t] = true
		}
		for _, t := range commonTLDs {
			want[t] = true
		}
		for _, t := range opts.TLDs {
			if n := NormTLD(t); n != "" {
				want[n] = true
			}
		}
		var existing []string
		s.DB.Model(&Item{}).Where("organization_id = ? AND supplier_id = ? AND tld <> ''", org, sp.ID).Distinct().Pluck("tld", &existing)
		for _, t := range existing {
			want[t] = true
		}
		for _, dp := range list {
			if !opts.AllTLDs && !want[dp.TLD] {
				continue
			}
			for _, x := range []struct {
				kind  string
				price *float64
			}{{KindRegister, dp.Register}, {KindRenew, dp.Renew}, {KindTransfer, dp.Transfer}} {
				if x.price == nil {
					continue
				}
				res.Domains++
				s.autofillItem(org, user, sp, ItemInput{SupplierID: Of(sp.ID), Kind: Of(x.kind), Name: Of(""), TLD: Of(dp.TLD), Period: Of(PeriodYear), Price: Of(*x.price),
					Currency: Of("AUD"), AsOf: Of(asOf), Notes: Of("From the Synergy Wholesale API price list"), Active: Of(true)}, "synergy_api", today, res)
			}
		}
		if res.Domains > 0 {
			res.DomainSource = "api"
		} else {
			res.Notes = append(res.Notes, "The Synergy API returned no domain prices for your TLDs.")
		}
	case errors.Is(perr, ErrNotConnected):
	default:
		res.APIConnected = true
		res.APIError = perr.Error()
		var se *expenses.SynergyError
		if errors.As(perr, &se) {
			res.APIError = se.Message
			if se.Hint != "" {
				res.APIError += " — " + se.Hint
			}
		}
	}
	if res.DomainSource == "none" {
		// Renewals actually charged: the latest charge per TLD.
		type obs struct {
			at     time.Time
			amount float64
			domain string
		}
		latest := map[string]obs{}
		for _, t := range txns {
			if t.Kind != expenses.SynDomainRenewal || t.Debit <= 0 || t.Domain == "" {
				continue
			}
			d := strings.ToLower(t.Description)
			if !strings.Contains(d, "renew") || !(strings.Contains(d, "1 year") || strings.Contains(d, "1 yr")) {
				continue
			}
			tld := TLDOfDomain(t.Domain)
			if tld == "" {
				continue
			}
			if o, ok := latest[tld]; !ok || t.Date.After(o.at) {
				latest[tld] = obs{t.Date, t.Debit, t.Domain}
			}
		}
		tlds := []string{}
		for t := range latest {
			tlds = append(tlds, t)
		}
		sort.Strings(tlds)
		for _, tld := range tlds {
			o := latest[tld]
			res.Domains++
			s.autofillItem(org, user, sp, ItemInput{SupplierID: Of(sp.ID), Kind: Of(KindRenew), Name: Of(""), TLD: Of(tld), Period: Of(PeriodYear), Price: Of(o.amount), Currency: Of("AUD"),
				AsOf: Of(Day(o.at).Format("2006-01-02")), Notes: Of(fmt.Sprintf("Learned from your Synergy statement: renewal of %s on %s · AUD as charged, GST included", o.domain, o.at.Format("2 Jan 2006"))), Active: Of(true)},
				"synergy_statements", today, res)
		}
		if len(tlds) > 0 {
			res.DomainSource = "statements"
		}
	}

	// ---- what happened, in words
	parts := []string{}
	if res.Plans > 0 {
		parts = append(parts, fmt.Sprintf("%d hosting plan%s from your statements", res.Plans, plural(res.Plans)))
	}
	switch res.DomainSource {
	case "api":
		parts = append(parts, fmt.Sprintf("%d domain price%s from the Synergy API", res.Domains, plural(res.Domains)))
	case "statements":
		parts = append(parts, fmt.Sprintf("%d renewal price%s from your statements", res.Domains, plural(res.Domains)))
	}
	switch {
	case len(parts) == 0 && !res.HasStatements && !res.APIConnected:
		res.Message = "Nothing to fill in yet: upload your Synergy statements (Expenses → Synergy) or connect the Synergy API (Settings → Expense sources)."
	case len(parts) == 0:
		res.Message = "No prices found on your Synergy statements yet."
	default:
		res.Message = "Synergy Wholesale: " + strings.Join(parts, " and ") + fmt.Sprintf(" — %d added, %d updated, %d unchanged.", res.Created, res.Updated, res.Unchanged)
	}
	switch {
	case res.APIError != "":
		res.Notes = append(res.Notes, "Synergy API: "+res.APIError)
	case !res.APIConnected:
		res.Notes = append(res.Notes, "The Synergy API is not connected, so registration and transfer prices were not pulled — connect it in Settings → Expense sources, or add them by hand or CSV.")
	}
	return res, nil
}
