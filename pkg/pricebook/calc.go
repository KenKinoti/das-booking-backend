package pricebook

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// ---------------------------------------------------------------- money

// RoundCur rounds v to the currency's minor units (UGX / JPY none, KWD 3,
// everything else 2).
func RoundCur(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	return math.Round(v*p+math.Copysign(1e-9, v)) / p
}

// RateInfo is the rate used to convert one currency to the home currency
// and where it came from.
type RateInfo struct {
	Currency    string     `json:"currency"`
	Rate        float64    `json:"rate"`         // home units per 1 unit of Currency (the one used)
	DefaultRate float64    `json:"default_rate"` // the quoted rate before any what-if override
	Source      string     `json:"source"`       // same | manual | live | fallback | missing | what-if
	Provider    string     `json:"provider,omitempty"`
	AsOf        *time.Time `json:"as_of"`
	Stale       bool       `json:"stale,omitempty"`
	Warning     string     `json:"warning,omitempty"`
	Overridden  bool       `json:"overridden"`
}

// Rates converts amounts between currencies through the home currency.
type Rates struct {
	Home string
	Per  map[string]float64 // home units per 1 unit of the currency
	Info map[string]RateInfo
}

// Rate is home units per 1 unit of cur (1 for the home currency, 0 when
// there is no rate).
func (r Rates) Rate(cur string) float64 {
	cur = strings.ToUpper(strings.TrimSpace(cur))
	if cur == "" || cur == r.Home {
		return 1
	}
	return r.Per[cur]
}

// ToHome converts v to the home currency, rounded to its minor units.
func (r Rates) ToHome(v float64, cur string) float64 {
	return RoundCur(v*r.Rate(cur), r.Home)
}

// Convert converts v from one currency to another through the home
// currency, without rounding. It returns 0 when a rate is missing.
func (r Rates) Convert(v float64, from, to string) float64 {
	from, to = strings.ToUpper(from), strings.ToUpper(to)
	if from == to {
		return v
	}
	t := r.Rate(to)
	if t <= 0 {
		return 0
	}
	return v * r.Rate(from) / t
}

// PromoTrapRatio: an option is flagged "year 1 cheap, renews at …" when a
// year at the regular prices costs more than this × the first year.
const PromoTrapRatio = 1.05

// ---------------------------------------------------------------- one price over time

// Cost is what something costs over time, in its own currency.
type Cost struct {
	FirstYear float64 // paid in the first 12 months (every billing period that starts in them)
	Ongoing   float64 // per year after that, at the regular price
	Term      float64 // paid over the whole term (every billing period that starts in it)
}

// ItemCost works out the cost of a price billed every months months, whose
// first period costs price and every later period regular, over termYears
// years. months 0 is a one-off payment.
//
//	periods in the first year  n1 = ceil(12 / months)
//	first year                 = price + (n1 − 1) × regular
//	ongoing per year           = regular × 12 / months
//	periods in the term        nT = ceil(12 × termYears / months)
//	term total                 = price + (nT − 1) × regular
func ItemCost(price, regular float64, months, termYears int) Cost {
	if termYears < 1 {
		termYears = 1
	}
	if months <= 0 {
		return Cost{FirstYear: price, Ongoing: 0, Term: price}
	}
	if regular <= 0 {
		regular = price
	}
	n1 := int(math.Ceil(12 / float64(months)))
	nT := int(math.Ceil(float64(12*termYears) / float64(months)))
	return Cost{
		FirstYear: price + float64(n1-1)*regular,
		Ongoing:   regular * 12 / float64(months),
		Term:      price + float64(nT-1)*regular,
	}
}

// ---------------------------------------------------------------- comparison input

// CompareInput is what to compare.
type CompareInput struct {
	TLDs         []string `json:"tlds"`          // ".co.ke", ".com" … (none = hosting only)
	Tier         string   `json:"tier"`          // starter | standard | business | "" (use MinDiskGB / MinSites)
	MinDiskGB    float64  `json:"min_disk_gb"`   // disk the customer needs
	MinSites     int      `json:"min_sites"`     // sites the customer's plan must allow
	TermYears    int      `json:"term_years"`    // 1 or 2
	DomainAction string   `json:"domain_action"` // register (default) | transfer
	NoHosting    bool     `json:"no_hosting"`    // domain only
	IncludeSSL   bool     `json:"include_ssl"`
	Mailboxes    int      `json:"mailboxes"` // 0 = no email
	// ShareSites spreads a multi-site plan over this many client sites
	// (1 = one client per plan).
	ShareSites int `json:"share_sites"`
	// SelfHostedBasis: cost per site at the current number of sites
	// ("current") or with the server full ("capacity").
	SelfHostedBasis string `json:"self_hosted_basis"`
	// DomainFrom / HostingFrom pin the mix to a supplier (its id); "" lets
	// the comparison pick the cheapest for that half.
	DomainFrom  string `json:"domain_from"`
	HostingFrom string `json:"hosting_from"`
	// Rates are what-if rates: home units per 1 unit of the currency.
	Rates map[string]float64 `json:"rates"`
}

// Normalize fills the defaults and resolves the tier.
func (in CompareInput) Normalize(tiers []Tier) CompareInput {
	out := in
	seen := map[string]bool{}
	out.TLDs = []string{}
	for _, t := range in.TLDs {
		if n := NormTLD(t); n != "" && !seen[n] {
			seen[n] = true
			out.TLDs = append(out.TLDs, n)
		}
	}
	out.Tier = strings.ToLower(strings.TrimSpace(in.Tier))
	if out.Tier != "" {
		found := false
		for _, t := range tiers {
			if t.Key == out.Tier {
				out.MinDiskGB, out.MinSites, found = t.MinDiskGB, t.MinSites, true
			}
		}
		if !found {
			out.Tier = ""
		}
	}
	if out.MinDiskGB < 0 {
		out.MinDiskGB = 0
	}
	if out.MinSites < 1 {
		out.MinSites = 1
	}
	if out.TermYears != 2 {
		out.TermYears = 1
	}
	if out.DomainAction != "transfer" {
		out.DomainAction = "register"
	}
	if out.Mailboxes < 0 {
		out.Mailboxes = 0
	}
	if out.ShareSites < 1 {
		out.ShareSites = 1
	}
	if out.SelfHostedBasis != "capacity" {
		out.SelfHostedBasis = "current"
	}
	out.DomainFrom, out.HostingFrom = strings.TrimSpace(in.DomainFrom), strings.TrimSpace(in.HostingFrom)
	if len(out.TLDs) == 0 {
		out.NoHosting = false // nothing at all to compare otherwise
	}
	rates := map[string]float64{}
	for k, v := range in.Rates {
		if k = strings.ToUpper(strings.TrimSpace(k)); k != "" && v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
			rates[k] = v
		}
	}
	out.Rates = rates
	return out
}

// ---------------------------------------------------------------- comparison output

// CurShare says which currency a share of a cost is really paid in (a
// self-hosted server's cost per site can mix a USD server with local time).
type CurShare struct {
	Currency string  `json:"currency"`
	Share    float64 `json:"share"`
}

// Part is one line of a combo: the domain, the hosting plan, SSL or email.
type Part struct {
	Role         string     `json:"role"` // domain | hosting | ssl | email
	ItemID       string     `json:"item_id,omitempty"`
	RenewItemID  string     `json:"renew_item_id,omitempty"`
	SupplierID   string     `json:"supplier_id"`
	SupplierName string     `json:"supplier_name"`
	Name         string     `json:"name"`
	Period       string     `json:"period"`
	Months       int        `json:"months"`
	Currency     string     `json:"currency"`
	Quantity     float64    `json:"quantity"`  // mailboxes, sites
	SharedBy     int        `json:"shared_by"` // client sites sharing the plan (1 = not shared)
	Price        float64    `json:"price"`     // first billing period, for this customer
	Regular      float64    `json:"regular"`   // every later billing period
	Monthly      float64    `json:"monthly"`   // regular price per month
	Setup        float64    `json:"setup"`
	FirstYear    float64    `json:"first_year"`
	Ongoing      float64    `json:"ongoing"`
	Term         float64    `json:"term_total"`
	Rate         float64    `json:"rate"`
	FirstYearH   float64    `json:"first_year_home"`
	OngoingH     float64    `json:"ongoing_home"`
	TermH        float64    `json:"term_total_home"`
	SetupH       float64    `json:"setup_home"`
	Promo        bool       `json:"promo"`
	Included     bool       `json:"included"`               // costs nothing extra (in the plan / assumed)
	Assumed      bool       `json:"assumed"`                // a price that was not listed and had to be assumed
	AssumedWhat  string     `json:"assumed_what,omitempty"` // renewal | registration: which domain price was missing
	Note         string     `json:"note,omitempty"`
	AsOf         string     `json:"as_of,omitempty"`
	Stale        bool       `json:"stale"`
	DiskGB       *float64   `json:"disk_gb,omitempty"`
	Split        []CurShare `json:"split,omitempty"`
}

// Flag is something to know about an option.
type Flag struct {
	Type     string `json:"type"`     // promo | assumed_renewal | assumed_registration | stale | shared | no_rate | email_not_priced | ssl_assumed
	Severity string `json:"severity"` // warning | info
	Message  string `json:"message"`
}

// CurAmount is a total in one currency.
type CurAmount struct {
	Currency  string  `json:"currency"`
	FirstYear float64 `json:"first_year"`
	Ongoing   float64 `json:"ongoing"`
	Term      float64 `json:"term_total"`
	Setup     float64 `json:"setup"`
}

// Option is one way to supply the combo: everything from one supplier, or
// the cheapest mix.
type Option struct {
	Key          string   `json:"key"` // s:<supplier id> | mix
	SupplierID   string   `json:"supplier_id,omitempty"`
	SupplierName string   `json:"supplier_name"`
	SupplierType string   `json:"supplier_type"`
	Label        string   `json:"label"`
	Domain       *Part    `json:"domain"`
	Hosting      *Part    `json:"hosting"`
	SSL          *Part    `json:"ssl"`
	Email        *Part    `json:"email"`
	Complete     bool     `json:"complete"`
	Missing      []string `json:"missing"`
	Reasons      []string `json:"reasons,omitempty"` // why something is missing
	// Currency is set when every part is in one currency: the totals below
	// are then also given in it.
	Currency   string      `json:"currency"`
	FirstYear  float64     `json:"first_year"`
	Ongoing    float64     `json:"ongoing"`
	Term       float64     `json:"term_total"`
	Setup      float64     `json:"setup"`
	ByCurrency []CurAmount `json:"by_currency"`
	// Home currency.
	FirstYearH float64 `json:"first_year_home"`
	OngoingH   float64 `json:"ongoing_home"`
	TermH      float64 `json:"term_total_home"`
	SetupH     float64 `json:"setup_home"`
	Flags      []Flag  `json:"flags"`
	PromoTrap  bool    `json:"promo_trap"`
	Cheapest   bool    `json:"cheapest"`
	// LowestOngoing marks the option that is cheapest per year once any
	// introductory prices have ended (set when several options are complete).
	LowestOngoing bool `json:"lowest_ongoing"`
	// Mixes only. Pinned: the user chose where a half comes from.
	// SavingOngoing (the renewal mix): what it saves per year against the
	// single supplier that is cheapest to renew.
	SavingOngoing float64 `json:"saving_ongoing_home,omitempty"`
	Pinned        bool    `json:"pinned,omitempty"`
	SameSupplier  bool    `json:"same_supplier,omitempty"`
	Saving        float64 `json:"saving_home,omitempty"` // vs the cheapest single supplier, over the term
}

// Parts lists the option's parts in display order.
func (o *Option) Parts() []*Part {
	out := []*Part{}
	for _, p := range []*Part{o.Domain, o.Hosting, o.SSL, o.Email} {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Stats are the lowest, middle and highest of a set of prices.
type Stats struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// Market summarises the competitors' retail prices for the combo (home
// currency, per year).
type Market struct {
	FirstYear Stats `json:"first_year"`
	Ongoing   Stats `json:"ongoing"`
}

// ComboResult is the comparison for one TLD.
type ComboResult struct {
	TLD     string   `json:"tld"`
	Label   string   `json:"label"`
	Options []Option `json:"options"`
	Mix     *Option  `json:"mix"`
	MixNote string   `json:"mix_note,omitempty"` // why a pinned mix could not be built
	// MixOngoing is the renewal-optimised mix: the domain that is cheapest
	// to renew + the hosting that costs least per year at the regular
	// prices. nil when it is the same pair as Mix, when both halves come from
	// one supplier (its own option shows it) or when a half is pinned.
	MixOngoing *Option `json:"mix_ongoing"`
	Best       string  `json:"best"` // key of the cheapest complete option
	// BestOngoing is the key of the option that costs least per year at the
	// regular prices ("" when fewer than two options are complete).
	BestOngoing string   `json:"best_ongoing"`
	Benchmarks  []Option `json:"benchmarks"`
	Market      *Market  `json:"market"`
}

// CompareResult is the whole answer.
type CompareResult struct {
	Home     string        `json:"home"`
	Input    CompareInput  `json:"input"`
	Tiers    []Tier        `json:"tiers"`
	Need     string        `json:"need"` // the hosting need in words
	Rates    []RateInfo    `json:"rates"`
	Combos   []ComboResult `json:"combos"`
	Warnings []string      `json:"warnings"`
	Today    string        `json:"today"`
	// MultiSitePlans counts the wholesale hosting plans that allow more than
	// one site (reseller plans): with ShareSites 1 they are costed as if one
	// client used the whole plan.
	MultiSitePlans int `json:"multi_site_plans"`
}

// Data is everything Compare needs.
type Data struct {
	Home      string
	Suppliers []Supplier
	Items     []Item // active items
	Rates     Rates
	Tiers     []Tier
	Today     time.Time
}

// ---------------------------------------------------------------- building parts

func fmtNum(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func money(v float64, cur string) string {
	d := currency.Decimals(cur)
	s := fmt.Sprintf("%.*f", d, math.Abs(v))
	intPart, frac := s, ""
	if i := strings.Index(s, "."); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, ch := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	sign := ""
	if v < 0 {
		sign = "−"
	}
	return sign + cur + " " + b.String() + frac
}

func (d Data) finish(p *Part, c Cost, setup float64) {
	cur := p.Currency
	p.FirstYear, p.Ongoing, p.Term, p.Setup = RoundCur(c.FirstYear, cur), RoundCur(c.Ongoing, cur), RoundCur(c.Term, cur), RoundCur(setup, cur)
	p.Rate = d.Rates.Rate(cur)
	p.FirstYearH, p.OngoingH, p.TermH, p.SetupH = d.Rates.ToHome(p.FirstYear, cur), d.Rates.ToHome(p.Ongoing, cur), d.Rates.ToHome(p.Term, cur), d.Rates.ToHome(p.Setup, cur)
	if len(p.Split) == 0 {
		p.Split = []CurShare{{Currency: cur, Share: 1}}
	}
}

func (d Data) stamp(p *Part, it Item) {
	x := it
	x.Age(d.Today)
	if it.AsOf != nil {
		p.AsOf = it.AsOf.Format("2006-01-02")
	}
	p.Stale = x.Stale
}

// planFit decides whether a hosting plan covers the need and how many
// client sites share it.
func planFit(it Item, in CompareInput) (ok bool, sharedBy int, reason string) {
	months := PeriodMonths(it.Period)
	if it.Period != PeriodOneOff && months > 12*in.TermYears {
		return false, 0, fmt.Sprintf("billed every %d months — longer than the %d-year term", months, in.TermYears)
	}
	need := in.MinSites
	if need < 1 {
		need = 1
	}
	capacity := 1
	unlimitedSites := false
	if it.Sites != nil {
		switch {
		case *it.Sites == Unlimited:
			unlimitedSites = true
		case *it.Sites > 0:
			capacity = *it.Sites
		}
	}
	if !unlimitedSites && capacity < need {
		return false, 0, fmt.Sprintf("allows %d site%s, %d needed", capacity, plural(capacity), need)
	}
	sharedBy = 1
	if in.ShareSites > 1 && (unlimitedSites || capacity/need > 1) {
		sharedBy = in.ShareSites
		if !unlimitedSites && capacity/need < sharedBy {
			sharedBy = capacity / need
		}
	}
	if in.MinDiskGB > 0 {
		switch {
		case it.DiskGB == nil:
			return false, 0, "disk space not listed"
		case *it.DiskGB == Unlimited:
		default:
			if fit := int(math.Floor(*it.DiskGB/in.MinDiskGB + 1e-9)); fit < sharedBy {
				sharedBy = fit
			}
			if sharedBy < 1 {
				return false, 0, fmt.Sprintf("%s GB disk, %s GB needed", fmtNum(*it.DiskGB), fmtNum(in.MinDiskGB))
			}
		}
	}
	return true, sharedBy, ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// hostingPart prices one plan for the customer.
func (d Data) hostingPart(s Supplier, it Item, in CompareInput, sharedBy int) *Part {
	months := PeriodMonths(it.Period)
	div := float64(sharedBy)
	price, regular := it.Price/div, it.Regular()/div
	p := &Part{Role: "hosting", ItemID: it.ID, SupplierID: s.ID, SupplierName: s.Name, Name: it.Name, Period: it.Period, Months: months,
		Currency: it.Currency, Quantity: 1, SharedBy: sharedBy, Price: RoundCur(price, it.Currency), Regular: RoundCur(regular, it.Currency),
		Promo: it.IsPromo(), DiskGB: it.DiskGB}
	if months > 0 {
		p.Monthly = RoundCur(regular/float64(months), it.Currency)
	}
	if sharedBy > 1 {
		p.Note = fmt.Sprintf("%s per period shared by %d client sites", money(it.Price, it.Currency), sharedBy)
	}
	d.stamp(p, it)
	d.finish(p, ItemCost(price, regular, months, in.TermYears), it.SetupFee/div)
	return p
}

// selfHostedPart prices a site on the organisation's own server.
func (d Data) selfHostedPart(s Supplier, in CompareInput) (*Part, string) {
	m := SelfHost(s, d.Rates)
	if len(m.NoRate) > 0 {
		// Leaving a cost out would make the server look cheaper than it is.
		return nil, "no exchange rate for " + strings.Join(m.NoRate, ", ") + " — enter one to cost your server"
	}
	if m.MonthlyTotal <= 0 {
		return nil, "add your server costs to include self-hosting"
	}
	sites := s.CurrentSites
	basis := "at the current " + fmt.Sprintf("%d site%s", sites, plural(sites))
	if in.SelfHostedBasis == "capacity" {
		sites = s.Capacity
		basis = fmt.Sprintf("with the server full (%d sites)", sites)
	}
	if sites < 1 {
		if in.SelfHostedBasis == "capacity" {
			return nil, "set how many sites the server holds"
		}
		sites, basis = 1, "with one site on the server"
	}
	need := in.MinSites
	if need < 1 {
		need = 1
	}
	if s.Capacity > 0 && need > s.Capacity {
		return nil, fmt.Sprintf("the server holds %d sites, %d needed", s.Capacity, need)
	}
	if in.MinDiskGB > 0 && s.DiskGB > 0 && s.Capacity > 0 {
		if per := s.DiskGB / float64(s.Capacity) * float64(need); per+1e-9 < in.MinDiskGB {
			return nil, fmt.Sprintf("%s GB disk per site, %s GB needed", fmtNum(per/float64(need)), fmtNum(in.MinDiskGB))
		}
	}
	perSite := m.MonthlyTotal / float64(sites) * float64(need)
	p := &Part{Role: "hosting", SupplierID: s.ID, SupplierName: s.Name, Name: "Your server · " + basis, Period: PeriodMonth, Months: 1,
		Currency: s.Currency, Quantity: float64(need), SharedBy: sites, Price: RoundCur(perSite, s.Currency), Regular: RoundCur(perSite, s.Currency),
		Monthly: RoundCur(perSite, s.Currency), Split: m.Split,
		Note: fmt.Sprintf("%s a month ÷ %d site%s", money(m.MonthlyTotal, s.Currency), sites, plural(sites))}
	// Monthly × 12 so the line reads "hosting × 12 months" exactly.
	d.finish(p, ItemCost(p.Price, p.Price, 1, in.TermYears), 0)
	return p, ""
}

// cheaper reports whether a costs less than b over the term (then per year
// afterwards) in the home currency.
func cheaper(a, b *Part) bool {
	at, bt := a.TermH+a.SetupH, b.TermH+b.SetupH
	if math.Abs(at-bt) > 0.005 {
		return at < bt
	}
	return a.OngoingH < b.OngoingH
}

// domainPart prices the TLD at a supplier (nil when it is not sold there).
func (d Data) domainPart(s Supplier, items []Item, tld string, in CompareInput) (*Part, string) {
	firstKind := KindRegister
	if in.DomainAction == "transfer" {
		firstKind = KindTransfer
	}
	var renew *Item
	for i := range items {
		it := &items[i]
		if it.Kind != KindRenew || it.TLD != tld || PeriodMonths(it.Period) <= 0 {
			continue
		}
		// The yearly renewal price; the cheapest per year when several are listed.
		if renew == nil || it.Price*12/float64(PeriodMonths(it.Period)) < renew.Price*12/float64(PeriodMonths(renew.Period)) {
			renew = it
		}
	}
	var best *Part
	found, tooLong := false, false
	for _, it := range items {
		if it.Kind != firstKind || it.TLD != tld {
			continue
		}
		found = true
		months := PeriodMonths(it.Period)
		if months <= 0 {
			months = 12
		}
		if months > 12*in.TermYears {
			tooLong = true
			continue
		}
		p := &Part{Role: "domain", ItemID: it.ID, SupplierID: s.ID, SupplierName: s.Name, Name: it.Name, Period: it.Period, Months: months, Currency: it.Currency,
			Quantity: 1, SharedBy: 1, Price: RoundCur(it.Price, it.Currency), Promo: it.IsPromo()}
		var perYear float64
		switch {
		case renew != nil && renew.Currency == it.Currency:
			perYear = renew.Price * 12 / float64(PeriodMonths(renew.Period))
			p.RenewItemID = renew.ID
		case renew != nil:
			perYear = d.Rates.Convert(renew.Price*12/float64(PeriodMonths(renew.Period)), renew.Currency, it.Currency)
			p.RenewItemID = renew.ID
		default:
			perYear = it.Regular() * 12 / float64(months)
			p.Assumed = true
			if it.RegularPrice != nil && *it.RegularPrice > 0 {
				p.Assumed = false
			}
		}
		regular := perYear * float64(months) / 12
		p.Regular = RoundCur(regular, it.Currency)
		p.Monthly = RoundCur(perYear/12, it.Currency)
		if regular > it.Price+0.0001 {
			p.Promo = true
		}
		if p.Assumed {
			p.AssumedWhat = "renewal"
			p.Note = "No renewal price listed — assumed the same as " + map[string]string{KindRegister: "registration", KindTransfer: "the transfer"}[firstKind]
		}
		d.stamp(p, it)
		if renew != nil {
			x := *renew
			x.Age(d.Today)
			p.Stale = p.Stale || x.Stale
		}
		d.finish(p, ItemCost(it.Price, regular, months, in.TermYears), it.SetupFee)
		if best == nil || cheaper(p, best) {
			best = p
		}
	}
	if best != nil {
		return best, ""
	}
	what := map[string]string{KindRegister: "registration", KindTransfer: "transfer"}[firstKind]
	// Only a renewal price is listed (e.g. learned from statements): the
	// first year is assumed to cost the same, and the option says so.
	if !found && renew != nil {
		if months := PeriodMonths(renew.Period); months <= 12*in.TermYears {
			it := *renew
			p := &Part{Role: "domain", ItemID: it.ID, RenewItemID: it.ID, SupplierID: s.ID, SupplierName: s.Name, Name: it.Name, Period: it.Period, Months: months, Currency: it.Currency,
				Quantity: 1, SharedBy: 1, Price: RoundCur(it.Price, it.Currency), Regular: RoundCur(it.Regular(), it.Currency), Monthly: RoundCur(it.Regular()/float64(months), it.Currency),
				Promo: it.IsPromo(), Assumed: true, AssumedWhat: "registration", Note: "No " + what + " price listed — assumed the same as the renewal"}
			d.stamp(p, it)
			d.finish(p, ItemCost(it.Price, it.Regular(), months, in.TermYears), it.SetupFee)
			return p, ""
		}
	}
	switch {
	case found && tooLong:
		return nil, fmt.Sprintf("%s %s is only sold for longer than the %d-year term", tld, what, in.TermYears)
	default:
		return nil, fmt.Sprintf("no %s %s price", tld, what)
	}
}

// addonPart prices SSL or mailboxes from the supplier's list (the cheapest
// over the term); nil when none is listed.
func (d Data) addonPart(s Supplier, items []Item, kind, role string, qty float64, in CompareInput) *Part {
	var best *Part
	for _, it := range items {
		if it.Kind != kind {
			continue
		}
		months := PeriodMonths(it.Period)
		if it.Period != PeriodOneOff && months > 12*in.TermYears {
			continue
		}
		p := &Part{Role: role, ItemID: it.ID, SupplierID: s.ID, SupplierName: s.Name, Name: it.Name, Period: it.Period, Months: months, Currency: it.Currency,
			Quantity: qty, SharedBy: 1, Price: RoundCur(it.Price*qty, it.Currency), Regular: RoundCur(it.Regular()*qty, it.Currency), Promo: it.IsPromo()}
		if months > 0 {
			p.Monthly = RoundCur(it.Regular()*qty/float64(months), it.Currency)
		}
		d.stamp(p, it)
		d.finish(p, ItemCost(it.Price*qty, it.Regular()*qty, months, in.TermYears), it.SetupFee*qty)
		if best == nil || cheaper(p, best) {
			best = p
		}
	}
	return best
}

// bundle is a hosting plan with its SSL and mailboxes.
type bundle struct {
	hosting, ssl, email *Part
	flags               []Flag
	partial             bool // something asked for could not be priced (mailboxes)
}

// betterThan: a bundle that covers everything asked for beats one that does
// not; then the cheapest over the term, then per year afterwards.
func (b *bundle) betterThan(o *bundle) bool {
	if o == nil {
		return true
	}
	if b.partial != o.partial {
		return !b.partial
	}
	if math.Abs(b.termH()-o.termH()) > 0.005 {
		return b.termH() < o.termH()
	}
	return b.ongoingH() < o.ongoingH()
}

// cheaperToRenew: a bundle that covers everything asked for first, then the
// lowest cost per year at the regular prices, then the lowest over the term.
func (b *bundle) cheaperToRenew(o *bundle) bool {
	if o == nil {
		return true
	}
	if b.partial != o.partial {
		return !b.partial
	}
	if math.Abs(b.ongoingH()-o.ongoingH()) > 0.005 {
		return b.ongoingH() < o.ongoingH()
	}
	return b.termH() < o.termH()
}

func (b bundle) termH() float64 {
	t := 0.0
	for _, p := range []*Part{b.hosting, b.ssl, b.email} {
		if p != nil {
			t += p.TermH + p.SetupH
		}
	}
	return t
}

func (b bundle) ongoingH() float64 {
	t := 0.0
	for _, p := range []*Part{b.hosting, b.ssl, b.email} {
		if p != nil {
			t += p.OngoingH
		}
	}
	return t
}

// hostingBundle finds the supplier's cheapest hosting (with SSL and email
// when asked for) that covers the need: cheapest over the term, or — with
// toRenew — cheapest per year at the regular prices.
func (d Data) hostingBundle(s Supplier, items []Item, in CompareInput, toRenew bool) (*bundle, string) {
	type cand struct {
		part *Part
		it   *Item
	}
	cands := []cand{}
	reasons := []string{}
	if s.Type == TypeSelfHosted {
		p, why := d.selfHostedPart(s, in)
		if p == nil {
			return nil, why
		}
		cands = append(cands, cand{part: p})
	} else {
		plans := 0
		for i := range items {
			it := &items[i]
			if it.Kind != KindHosting {
				continue
			}
			plans++
			ok, shared, why := planFit(*it, in)
			if !ok {
				reasons = append(reasons, it.Name+": "+why)
				continue
			}
			cands = append(cands, cand{part: d.hostingPart(s, *it, in, shared), it: it})
		}
		if plans == 0 {
			return nil, "no hosting plans on this price list"
		}
		if len(cands) == 0 {
			if len(reasons) > 3 {
				reasons = append(reasons[:3], fmt.Sprintf("and %d more", len(reasons)-3))
			}
			return nil, "no plan covers the need (" + strings.Join(reasons, "; ") + ")"
		}
	}
	var ssl *Part
	var sslFlag *Flag
	if in.IncludeSSL {
		if ssl = d.addonPart(s, items, KindSSL, "ssl", 1, in); ssl == nil {
			ssl = &Part{Role: "ssl", SupplierID: s.ID, SupplierName: s.Name, Name: "SSL certificate", Currency: s.Currency, Quantity: 1, SharedBy: 1, Included: true, Assumed: true,
				Note: "No SSL price listed — assumed included (e.g. Let's Encrypt)", Split: []CurShare{{Currency: s.Currency, Share: 1}}, Rate: d.Rates.Rate(s.Currency)}
			sslFlag = &Flag{Type: "ssl_assumed", Severity: "info", Message: "No SSL price on " + s.Name + "'s list — assumed included."}
		}
	}
	var best *bundle
	for _, c := range cands {
		b := &bundle{hosting: c.part, ssl: ssl}
		if sslFlag != nil {
			b.flags = append(b.flags, *sslFlag)
		}
		if in.Mailboxes > 0 {
			included := false
			if c.it != nil && c.it.EmailAccounts != nil {
				need := in.Mailboxes * c.part.SharedBy
				included = *c.it.EmailAccounts == Unlimited || *c.it.EmailAccounts >= need
			}
			switch {
			case included:
				b.email = &Part{Role: "email", SupplierID: s.ID, SupplierName: s.Name, Name: fmt.Sprintf("%d mailbox%s", in.Mailboxes, pluralEs(in.Mailboxes)), Currency: c.part.Currency,
					Quantity: float64(in.Mailboxes), SharedBy: 1, Included: true, Note: "Included in " + c.part.Name, Split: []CurShare{{Currency: c.part.Currency, Share: 1}}, Rate: d.Rates.Rate(c.part.Currency)}
			default:
				if b.email = d.addonPart(s, items, KindEmail, "email", float64(in.Mailboxes), in); b.email == nil {
					b.partial = true
					b.flags = append(b.flags, Flag{Type: "email_not_priced", Severity: "warning",
						Message: fmt.Sprintf("%s lists no mailbox price and %s does not include %d mailbox%s — email is not in this total.", s.Name, c.part.Name, in.Mailboxes, pluralEs(in.Mailboxes))})
				}
			}
		}
		if (toRenew && b.cheaperToRenew(best)) || (!toRenew && b.betterThan(best)) {
			best = b
		}
	}
	return best, ""
}

func pluralEs(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// assemble totals an option from its parts and raises the flags.
func (d Data) assemble(o *Option, in CompareInput, wantDomain, wantHosting bool) {
	o.Missing = []string{}
	o.Flags = append([]Flag{}, o.Flags...)
	if wantDomain && o.Domain == nil {
		o.Missing = append(o.Missing, "domain")
	}
	if wantHosting && o.Hosting == nil {
		o.Missing = append(o.Missing, "hosting")
	}
	by := map[string]*CurAmount{}
	order := []string{}
	noRate := map[string]bool{}
	stale := 0
	for _, p := range o.Parts() {
		c := by[p.Currency]
		if c == nil {
			c = &CurAmount{Currency: p.Currency}
			by[p.Currency] = c
			order = append(order, p.Currency)
		}
		c.FirstYear += p.FirstYear + p.Setup
		c.Ongoing += p.Ongoing
		c.Term += p.Term + p.Setup
		c.Setup += p.Setup
		o.FirstYearH += p.FirstYearH + p.SetupH
		o.OngoingH += p.OngoingH
		o.TermH += p.TermH + p.SetupH
		o.SetupH += p.SetupH
		if p.Rate <= 0 && !p.Included {
			noRate[p.Currency] = true
		}
		if p.Stale {
			stale++
		}
		if p.Role == "domain" && p.Assumed {
			typ := "assumed_renewal"
			if p.AssumedWhat == "registration" {
				typ = "assumed_registration"
			}
			o.Flags = append(o.Flags, Flag{Type: typ, Severity: "info", Message: p.Note + "."})
		}
		if p.Role == "hosting" && p.SharedBy > 1 && p.ItemID != "" {
			o.Flags = append(o.Flags, Flag{Type: "shared", Severity: "info", Message: fmt.Sprintf("%s is shared by %d client sites: the cost per site only holds while that many sites are on it.", p.Name, p.SharedBy)})
		}
	}
	o.ByCurrency = []CurAmount{}
	for _, k := range order {
		c := by[k]
		c.FirstYear, c.Ongoing, c.Term, c.Setup = RoundCur(c.FirstYear, k), RoundCur(c.Ongoing, k), RoundCur(c.Term, k), RoundCur(c.Setup, k)
		o.ByCurrency = append(o.ByCurrency, *c)
	}
	if len(o.ByCurrency) == 1 {
		c := o.ByCurrency[0]
		o.Currency, o.FirstYear, o.Ongoing, o.Term, o.Setup = c.Currency, c.FirstYear, c.Ongoing, c.Term, c.Setup
	}
	home := d.Home
	o.FirstYearH, o.OngoingH, o.TermH, o.SetupH = RoundCur(o.FirstYearH, home), RoundCur(o.OngoingH, home), RoundCur(o.TermH, home), RoundCur(o.SetupH, home)
	for cur := range noRate {
		o.Flags = append(o.Flags, Flag{Type: "no_rate", Severity: "warning", Message: "No exchange rate for " + cur + " → " + home + ": enter one to compare this option."})
	}
	o.Complete = len(o.Missing) == 0 && len(noRate) == 0 && len(o.Parts()) > 0
	// Promo trap: year 1 is clearly cheaper (5% or more) than every year after it.
	if firstExSetup := o.FirstYearH - o.SetupH; o.OngoingH > firstExSetup*PromoTrapRatio+0.005 && firstExSetup > 0 {
		o.PromoTrap = true
		names := []string{}
		for _, p := range o.Parts() {
			if p.Promo && p.OngoingH > p.FirstYearH+0.005 {
				names = append(names, p.Name)
			}
		}
		msg := fmt.Sprintf("Year 1 is cheap, then it renews at %s a year (+%.0f%%)", money(o.OngoingH, home), (o.OngoingH/firstExSetup-1)*100)
		if len(names) > 0 {
			msg += " — introductory price on " + strings.Join(names, ", ")
		}
		o.Flags = append(o.Flags, Flag{Type: "promo", Severity: "warning", Message: msg + "."})
	}
	if stale > 0 {
		o.Flags = append(o.Flags, Flag{Type: "stale", Severity: "warning", Message: fmt.Sprintf("%d price%s older than %d days — check before quoting.", stale, plural(stale), StaleDays)})
	}
}

func stats(vals []float64, cur string) Stats {
	s := Stats{Count: len(vals)}
	if len(vals) == 0 {
		return s
	}
	v := append([]float64{}, vals...)
	sort.Float64s(v)
	s.Min, s.Max = v[0], v[len(v)-1]
	if n := len(v); n%2 == 1 {
		s.Median = v[n/2]
	} else {
		s.Median = RoundCur((v[n/2-1]+v[n/2])/2, cur)
	}
	return s
}

// optionLess orders complete options: cheapest over the term first, then
// cheapest per year afterwards, then by name.
func optionLess(a, b Option) bool {
	if a.Complete != b.Complete {
		return a.Complete
	}
	if math.Abs(a.TermH-b.TermH) > 0.005 {
		return a.TermH < b.TermH
	}
	if math.Abs(a.OngoingH-b.OngoingH) > 0.005 {
		return a.OngoingH < b.OngoingH
	}
	return a.SupplierName < b.SupplierName
}

// NeedText describes the hosting need.
func NeedText(in CompareInput, tiers []Tier) string {
	if in.NoHosting {
		return "Domain only"
	}
	parts := []string{}
	for _, t := range tiers {
		if t.Key == in.Tier {
			parts = append(parts, t.Label+" hosting")
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "Hosting")
	}
	spec := []string{}
	if in.MinDiskGB > 0 {
		spec = append(spec, fmtNum(in.MinDiskGB)+" GB or more")
	}
	if in.MinSites > 1 {
		spec = append(spec, fmt.Sprintf("%d sites", in.MinSites))
	}
	if len(spec) > 0 {
		parts[0] += " (" + strings.Join(spec, ", ") + ")"
	}
	if in.IncludeSSL {
		parts = append(parts, "SSL")
	}
	if in.Mailboxes > 0 {
		parts = append(parts, fmt.Sprintf("%d mailbox%s", in.Mailboxes, pluralEs(in.Mailboxes)))
	}
	return strings.Join(parts, " + ")
}

// Compare prices the combo at every supplier, finds the cheapest mix and
// the competitors' retail prices. in must be normalised.
func Compare(in CompareInput, d Data) *CompareResult {
	res := &CompareResult{Home: d.Home, Input: in, Tiers: d.Tiers, Need: NeedText(in, d.Tiers), Combos: []ComboResult{}, Warnings: []string{}, Today: d.Today.Format("2006-01-02")}
	bySupplier := map[string][]Item{}
	for _, it := range d.Items {
		if it.Active {
			bySupplier[it.SupplierID] = append(bySupplier[it.SupplierID], it)
		}
	}
	tlds := in.TLDs
	if len(tlds) == 0 {
		tlds = []string{""}
	}
	wantHosting := !in.NoHosting
	used := map[string]bool{}
	for _, tld := range tlds {
		wantDomain := tld != ""
		cr := ComboResult{TLD: tld, Options: []Option{}, Benchmarks: []Option{}}
		switch {
		case wantDomain && wantHosting:
			cr.Label = tld + " domain + hosting"
		case wantDomain:
			cr.Label = tld + " domain"
		default:
			cr.Label = "Hosting only"
		}
		var bestDomain, bestHostPart *Part
		var bestBundle *bundle
		domainBy, bundleBy := map[string]*Part{}, map[string]*bundle{}
		nameBy := map[string]string{}
		// The halves of the renewal-optimised mix.
		var renewDomain *Part
		var renewBundle *bundle
		for _, s := range d.Suppliers {
			nameBy[s.ID] = s.Name
			items := bySupplier[s.ID]
			// A competitor's retail price is what ONE customer pays: its plans
			// are never spread over several of your sites.
			in := in
			if s.Type == TypeCompetitor {
				in.ShareSites = 1
			}
			o := Option{Key: "s:" + s.ID, SupplierID: s.ID, SupplierName: s.Name, SupplierType: s.Type, Label: s.Name}
			if wantDomain {
				p, why := d.domainPart(s, items, tld, in)
				o.Domain = p
				if p == nil && s.Type != TypeSelfHosted {
					o.Reasons = append(o.Reasons, why)
				} else if p == nil {
					o.Reasons = append(o.Reasons, "your own server does not sell domains — see the cheapest mix")
				}
			}
			if wantHosting {
				b, why := d.hostingBundle(s, items, in, false)
				if b != nil {
					o.Hosting, o.SSL, o.Email = b.hosting, b.ssl, b.email
					o.Flags = append(o.Flags, b.flags...)
				} else {
					o.Reasons = append(o.Reasons, why)
				}
				if b != nil && s.Type != TypeCompetitor && b.hosting.Rate > 0 {
					bundleBy[s.ID] = b
					if b.betterThan(bestBundle) {
						bestBundle, bestHostPart = b, b.hosting
					}
					// The plan that is cheapest to renew may not be the one that is cheapest over the term.
					if rb, _ := d.hostingBundle(s, items, in, true); rb != nil && rb.hosting.Rate > 0 && rb.cheaperToRenew(renewBundle) {
						renewBundle = rb
					}
				}
			}
			if o.Domain != nil && s.Type != TypeCompetitor && o.Domain.Rate > 0 {
				domainBy[s.ID] = o.Domain
				if bestDomain == nil || cheaper(o.Domain, bestDomain) {
					bestDomain = o.Domain
				}
				if renewDomain == nil || o.Domain.OngoingH < renewDomain.OngoingH-0.005 || (math.Abs(o.Domain.OngoingH-renewDomain.OngoingH) <= 0.005 && cheaper(o.Domain, renewDomain)) {
					renewDomain = o.Domain
				}
			}
			d.assemble(&o, in, wantDomain, wantHosting)
			for _, p := range o.Parts() {
				used[p.Currency] = true
			}
			if s.Type == TypeCompetitor {
				// A competitor with nothing for this combo says nothing about the market.
				if len(o.Parts()) > 0 {
					cr.Benchmarks = append(cr.Benchmarks, o)
				}
				continue
			}
			cr.Options = append(cr.Options, o)
		}
		sort.SliceStable(cr.Options, func(i, j int) bool { return optionLess(cr.Options[i], cr.Options[j]) })
		sort.SliceStable(cr.Benchmarks, func(i, j int) bool {
			a, b := cr.Benchmarks[i], cr.Benchmarks[j]
			if a.Complete != b.Complete {
				return a.Complete
			}
			return a.FirstYearH < b.FirstYearH
		})
		// The mix: the cheapest domain and the cheapest hosting, wherever each
		// comes from — unless a half is pinned to a supplier.
		pinned := false
		if wantDomain && wantHosting {
			if id := in.DomainFrom; id != "" && nameBy[id] != "" {
				pinned = true
				if bestDomain = domainBy[id]; bestDomain == nil {
					cr.MixNote = nameBy[id] + " has no " + tld + " price to mix in."
				}
			}
			if id := in.HostingFrom; id != "" && nameBy[id] != "" {
				pinned = true
				if b := bundleBy[id]; b != nil {
					bestBundle, bestHostPart = b, b.hosting
				} else {
					bestBundle, bestHostPart = nil, nil
					cr.MixNote = strings.TrimSpace(cr.MixNote + " " + nameBy[id] + " has no hosting for this need to mix in.")
				}
			}
		}
		if bestDomain != nil && bestBundle != nil && wantDomain && wantHosting {
			m := Option{Key: "mix", SupplierType: "mix", Domain: bestDomain, Hosting: bestHostPart, SSL: bestBundle.ssl, Email: bestBundle.email, Pinned: pinned}
			m.Flags = append(m.Flags, bestBundle.flags...)
			m.SameSupplier = bestDomain.SupplierID == bestHostPart.SupplierID
			if m.SameSupplier {
				m.SupplierID, m.SupplierName = bestDomain.SupplierID, bestDomain.SupplierName
				m.Label = "Everything from " + bestDomain.SupplierName
			} else {
				m.SupplierName = bestDomain.SupplierName + " + " + bestHostPart.SupplierName
				m.Label = "Domain from " + bestDomain.SupplierName + " + hosting from " + bestHostPart.SupplierName
			}
			d.assemble(&m, in, true, true)
			cr.Mix = &m
		}
		// The renewal-optimised mix: the domain that is cheapest to renew +
		// the hosting that costs least per year once introductory prices end.
		if wantDomain && wantHosting && !pinned && renewDomain != nil && renewBundle != nil && renewDomain.SupplierID != renewBundle.hosting.SupplierID {
			same := cr.Mix != nil && samePart(cr.Mix.Domain, renewDomain) && samePart(cr.Mix.Hosting, renewBundle.hosting)
			if !same {
				m := Option{Key: "mix_ongoing", SupplierType: "mix", Domain: renewDomain, Hosting: renewBundle.hosting, SSL: renewBundle.ssl, Email: renewBundle.email,
					SupplierName: renewDomain.SupplierName + " + " + renewBundle.hosting.SupplierName,
					Label:        "Domain from " + renewDomain.SupplierName + " + hosting from " + renewBundle.hosting.SupplierName}
				m.Flags = append(m.Flags, renewBundle.flags...)
				d.assemble(&m, in, true, true)
				if m.Complete {
					cr.MixOngoing = &m
				}
			}
		}
		// Cheapest complete option.
		best := -1
		for i := range cr.Options {
			if cr.Options[i].Complete {
				best = i
				break
			}
		}
		switch {
		case cr.Mix != nil && cr.Mix.Complete && (best < 0 || cr.Mix.TermH < cr.Options[best].TermH-0.005):
			cr.Mix.Cheapest = true
			cr.Best = "mix"
			if best >= 0 {
				cr.Mix.Saving = RoundCur(cr.Options[best].TermH-cr.Mix.TermH, d.Home)
			}
		case best >= 0:
			cr.Options[best].Cheapest = true
			cr.Best = cr.Options[best].Key
		}
		// Cheapest per year at the regular prices (what a renewal year costs).
		cands := []*Option{}
		if cr.Mix != nil && cr.Mix.Complete && !cr.Mix.SameSupplier {
			cands = append(cands, cr.Mix)
		}
		singleLow := -1.0
		for i := range cr.Options {
			if cr.Options[i].Complete {
				cands = append(cands, &cr.Options[i])
				if singleLow < 0 || cr.Options[i].OngoingH < singleLow {
					singleLow = cr.Options[i].OngoingH
				}
			}
		}
		if cr.MixOngoing != nil {
			cands = append(cands, cr.MixOngoing)
			if singleLow > cr.MixOngoing.OngoingH {
				cr.MixOngoing.SavingOngoing = RoundCur(singleLow-cr.MixOngoing.OngoingH, d.Home)
			}
		}
		if len(cands) > 1 {
			low := cands[0]
			for _, c := range cands[1:] {
				if c.OngoingH < low.OngoingH-0.005 || (math.Abs(c.OngoingH-low.OngoingH) <= 0.005 && c.Cheapest) {
					low = c
				}
			}
			low.LowestOngoing = true
			cr.BestOngoing = low.Key
		}
		// Competitors: the market's retail prices.
		fy, og := []float64{}, []float64{}
		for _, b := range cr.Benchmarks {
			if b.Complete {
				fy = append(fy, b.FirstYearH)
				og = append(og, b.OngoingH)
			}
		}
		if len(fy) > 0 {
			cr.Market = &Market{FirstYear: stats(fy, d.Home), Ongoing: stats(og, d.Home)}
		}
		res.Combos = append(res.Combos, cr)
	}
	// Rates used, home first.
	curs := []string{}
	for c := range used {
		if c != d.Home {
			curs = append(curs, c)
		}
	}
	sort.Strings(curs)
	res.Rates = []RateInfo{}
	for _, c := range curs {
		if ri, ok := d.Rates.Info[c]; ok {
			res.Rates = append(res.Rates, ri)
		} else {
			res.Rates = append(res.Rates, RateInfo{Currency: c, Rate: d.Rates.Rate(c), Source: "missing"})
		}
	}
	buyable := 0
	for _, s := range d.Suppliers {
		if s.Type != TypeCompetitor && len(bySupplier[s.ID]) > 0 {
			buyable++
		}
		if s.Type != TypeWholesale || !wantHosting {
			continue
		}
		for _, it := range bySupplier[s.ID] {
			if it.Kind == KindHosting && it.Sites != nil && (*it.Sites == Unlimited || *it.Sites > 1) {
				res.MultiSitePlans++
			}
		}
	}
	if buyable == 0 {
		res.Warnings = append(res.Warnings, "No supplier has any prices yet — import a price list or add prices to compare.")
	}
	return res
}

// samePart reports whether two parts are the same price from the same supplier.
func samePart(a, b *Part) bool {
	return a != nil && b != nil && a.SupplierID == b.SupplierID && a.ItemID == b.ItemID
}

// FindOption returns the option with key in the TLD's result.
func (r *CompareResult) FindOption(tld, key string) *Option {
	for i := range r.Combos {
		c := &r.Combos[i]
		if c.TLD != tld {
			continue
		}
		if key == "mix" || key == "" && c.Best == "mix" {
			return c.Mix
		}
		if key == "mix_ongoing" {
			return c.MixOngoing
		}
		if key == "" {
			key = c.Best
		}
		for j := range c.Options {
			if c.Options[j].Key == key {
				return &c.Options[j]
			}
		}
	}
	return nil
}

// Combo returns the result for a TLD.
func (r *CompareResult) Combo(tld string) *ComboResult {
	for i := range r.Combos {
		if r.Combos[i].TLD == tld {
			return &r.Combos[i]
		}
	}
	return nil
}
