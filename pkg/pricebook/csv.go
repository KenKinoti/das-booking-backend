package pricebook

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// CSVHeader is the exact header of the price list template (import and
// export use the same columns in the same order).
const CSVHeader = "supplier,supplier_type,supplier_currency,kind,name,tld,period,price,regular_price,setup_fee,disk_gb,sites,email_accounts,as_of,source_url,notes"

// CSVColumns is CSVHeader split.
var CSVColumns = strings.Split(CSVHeader, ",")

// CSVColumnDoc documents one column of the template.
type CSVColumnDoc struct {
	Name     string `json:"name"`
	Required string `json:"required"` // yes | domains | no
	Help     string `json:"help"`
	Example  string `json:"example"`
}

// CSVDocs explains every column (shown in the import dialog and written as
// comment lines into the template).
func CSVDocs() []CSVColumnDoc {
	return []CSVColumnDoc{
		{"supplier", "yes", "Supplier name. A new name creates the supplier.", "Synergy Wholesale"},
		{"supplier_type", "no", "wholesale (you buy from them), self_hosted (your own server) or competitor (market retail price, a benchmark only). Blank = wholesale, or the existing supplier's type.", "wholesale"},
		{"supplier_currency", "no", "Currency of the prices in this row (ISO code). Blank = the supplier's currency, or your home currency for a new supplier.", "AUD"},
		{"kind", "yes", "hosting_plan, domain_register, domain_renew, domain_transfer, ssl, email_mailbox or other.", "hosting_plan"},
		{"name", "yes", "Plan or product name. May be blank for domains (the TLD is used).", "Starter"},
		{"tld", "domains", "Domain ending for the three domain kinds, e.g. .co.ke (case and the leading dot do not matter).", ".co.ke"},
		{"period", "yes", "What the price is charged per: month, year, 2y, 3y or one_off. Blank = year for domains and SSL, month for hosting and mailboxes.", "month"},
		{"price", "yes", "Price per period, in the row's currency. For an introductory offer this is the first-period price. Currency symbols and thousand separators are ignored.", "6.50"},
		{"regular_price", "no", "What it renews at per period when price is an introductory offer. Blank = no offer.", "9.00"},
		{"setup_fee", "no", "One-off fee charged at the start.", "0"},
		{"disk_gb", "no", "Disk space in GB (hosting plans). \"unlimited\" is accepted.", "2"},
		{"sites", "no", "Websites the plan allows. \"unlimited\" is accepted. Blank = 1.", "1"},
		{"email_accounts", "no", "Mailboxes included in the plan. \"unlimited\" is accepted.", "5"},
		{"as_of", "no", "Date the price was checked (YYYY-MM-DD). Blank = today. Prices older than 90 days are flagged \"check price\".", "2026-10-02"},
		{"source_url", "no", "Where the price was found.", "https://example.com/pricing"},
		{"notes", "no", "Anything worth remembering (GST included, minimum term …).", "incl. GST"},
	}
}

// TemplateCSV is the documented template: the header, then comment lines
// (starting with #, ignored on import) that explain the columns and show
// example rows.
func TemplateCSV() []byte {
	var b bytes.Buffer
	b.WriteString(CSVHeader + "\n")
	b.WriteString("# Lines that start with # are ignored. One row per price. Rows are matched on supplier + kind + name + tld + period:\n")
	b.WriteString("# importing the same row again updates its price (the old price is kept in the history).\n")
	for _, d := range CSVDocs() {
		req := map[string]string{"yes": "required", "domains": "required for domains", "no": "optional"}[d.Required]
		b.WriteString("# " + d.Name + " (" + req + "): " + strings.ReplaceAll(d.Help, ",", ";") + "\n")
	}
	b.WriteString("# Examples (remove the # to use them):\n")
	b.WriteString("# Example Wholesale,wholesale,AUD,hosting_plan,Starter,,month,6.50,,0,2,1,5,2026-10-02,https://example.com/hosting,\n")
	b.WriteString("# Example Wholesale,wholesale,AUD,domain_register,,.co.ke,year,22.00,,,,,,2026-10-02,https://example.com/domains,\n")
	b.WriteString("# Example Wholesale,wholesale,AUD,domain_renew,,.co.ke,year,24.00,,,,,,2026-10-02,https://example.com/domains,\n")
	b.WriteString("# Example Retailer,competitor,KES,hosting_plan,Basic,,year,2999,4500,0,10,1,10,2026-10-02,https://example.co.ke,intro price then renews at 4500\n")
	return b.Bytes()
}

var kindAliases = map[string]string{
	"hosting_plan": KindHosting, "hosting": KindHosting, "plan": KindHosting, "host": KindHosting, "web_hosting": KindHosting, "shared_hosting": KindHosting,
	"domain_register": KindRegister, "domain_registration": KindRegister, "register": KindRegister, "registration": KindRegister, "domain": KindRegister, "domain_new": KindRegister,
	"domain_renew": KindRenew, "domain_renewal": KindRenew, "renew": KindRenew, "renewal": KindRenew,
	"domain_transfer": KindTransfer, "transfer": KindTransfer,
	"ssl": KindSSL, "ssl_certificate": KindSSL, "certificate": KindSSL,
	"email_mailbox": KindEmail, "email": KindEmail, "mailbox": KindEmail, "mail": KindEmail, "email_hosting": KindEmail,
	"other": KindOther, "addon": KindOther, "add_on": KindOther,
}

var periodAliases = map[string]string{
	"month": PeriodMonth, "monthly": PeriodMonth, "mo": PeriodMonth, "m": PeriodMonth, "1m": PeriodMonth, "1_month": PeriodMonth, "per_month": PeriodMonth,
	"year": PeriodYear, "yearly": PeriodYear, "annual": PeriodYear, "annually": PeriodYear, "yr": PeriodYear, "y": PeriodYear, "1y": PeriodYear, "1_year": PeriodYear, "12m": PeriodYear, "12_months": PeriodYear, "per_year": PeriodYear,
	"2y": Period2Y, "2_years": Period2Y, "2_year": Period2Y, "24m": Period2Y, "24_months": Period2Y, "biennial": Period2Y, "2yr": Period2Y,
	"3y": Period3Y, "3_years": Period3Y, "3_year": Period3Y, "36m": Period3Y, "36_months": Period3Y, "triennial": Period3Y, "3yr": Period3Y,
	"one_off": PeriodOneOff, "oneoff": PeriodOneOff, "once": PeriodOneOff, "one_time": PeriodOneOff, "onetime": PeriodOneOff, "lifetime": PeriodOneOff, "setup": PeriodOneOff,
}

var typeAliases = map[string]string{
	"wholesale": TypeWholesale, "reseller": TypeWholesale, "supplier": TypeWholesale,
	"self_hosted": TypeSelfHosted, "selfhosted": TypeSelfHosted, "self": TypeSelfHosted, "own": TypeSelfHosted, "own_server": TypeSelfHosted,
	"competitor": TypeCompetitor, "retail": TypeCompetitor, "benchmark": TypeCompetitor, "market": TypeCompetitor,
}

var reToken = regexp.MustCompile(`[\s\-]+`)

func token(s string) string {
	return strings.Trim(reToken.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_"), "_")
}

// ParseKind reads a kind, tolerant of case, spaces and common names.
func ParseKind(s string) (string, bool) {
	k, ok := kindAliases[token(s)]
	return k, ok
}

// ParsePeriod reads a billing period.
func ParsePeriod(s string) (string, bool) {
	p, ok := periodAliases[token(s)]
	return p, ok
}

// ParseType reads a supplier type.
func ParseType(s string) (string, bool) {
	t, ok := typeAliases[token(s)]
	return t, ok
}

var currencySymbols = []struct{ sym, code string }{
	{"KSH", "KES"}, {"KES", "KES"}, {"A$", "AUD"}, {"AU$", "AUD"}, {"AUD", "AUD"}, {"US$", "USD"}, {"USD", "USD"}, {"£", "GBP"}, {"€", "EUR"},
}

// ParseCurrency reads a currency code ("aud", "A$", "KSh").
func ParseCurrency(s string) (string, bool) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if currency.IsSupported(t) {
		return t, true
	}
	for _, x := range currencySymbols {
		if t == x.sym && currency.IsSupported(x.code) {
			return x.code, true
		}
	}
	if t == "$" {
		return "USD", true
	}
	return "", false
}

var reNumberJunk = regexp.MustCompile(`(?i)[a-z$£€¥₹\s'’/]+`)

// ParseAmount reads a number, ignoring currency symbols and codes, spaces
// and thousand separators: "A$1,234.50", "KES 4 990", "1.234,50", "12,5".
// blank reports an empty cell (v 0, ok true).
func ParseAmount(s string) (v float64, blank, ok bool) {
	t := strings.TrimSpace(s)
	if t == "" || t == "-" || t == "—" || strings.EqualFold(t, "n/a") || strings.EqualFold(t, "na") {
		return 0, true, true
	}
	if strings.EqualFold(t, "free") || strings.EqualFold(t, "included") {
		return 0, false, true
	}
	neg := strings.HasPrefix(t, "-") || (strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")"))
	t = strings.Trim(t, "-()")
	t = reNumberJunk.ReplaceAllString(t, "")
	if t == "" {
		return 0, false, false
	}
	lastDot, lastComma := strings.LastIndex(t, "."), strings.LastIndex(t, ",")
	switch {
	case lastDot >= 0 && lastComma >= 0:
		if lastComma > lastDot { // 1.234,50
			t = strings.ReplaceAll(t, ".", "")
			t = strings.Replace(t, ",", ".", 1)
		} else { // 1,234.50
			t = strings.ReplaceAll(t, ",", "")
		}
	case lastComma >= 0:
		// "1,234" and "12,345,678" are thousands; "12,5" and "0,99" are decimals.
		if strings.Count(t, ",") > 1 || len(t)-lastComma-1 == 3 {
			t = strings.ReplaceAll(t, ",", "")
		} else {
			t = strings.Replace(t, ",", ".", 1)
		}
	case strings.Count(t, ".") > 1: // 1.234.567
		t = strings.ReplaceAll(t, ".", "")
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, false
	}
	if neg {
		f = -f
	}
	return f, false, true
}

func isUnlimited(s string) bool {
	switch token(s) {
	case "unlimited", "unmetered", "infinite", "∞", "no_limit", "nolimit":
		return true
	}
	return false
}

// ParseDiskGB reads disk space in GB: "10", "10 GB", "500MB", "1 TB",
// "unlimited" (-1).
func ParseDiskGB(s string) (*float64, bool) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return nil, true
	}
	if isUnlimited(t) {
		v := float64(Unlimited)
		return &v, true
	}
	mult := 1.0
	for _, u := range []struct {
		suffix string
		m      float64
	}{{"GB", 1}, {"G", 1}, {"MB", 1.0 / 1024}, {"M", 1.0 / 1024}, {"TB", 1024}, {"T", 1024}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.m
			break
		}
	}
	f, blank, ok := ParseAmount(t)
	if !ok || blank || f < 0 {
		return nil, false
	}
	v := math.Round(f*mult*1000) / 1000
	return &v, true
}

// ParseCount reads a whole number of sites or mailboxes ("unlimited" = -1).
func ParseCount(s string) (*int, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, true
	}
	if isUnlimited(t) {
		v := Unlimited
		return &v, true
	}
	f, blank, ok := ParseAmount(t)
	if !ok || blank || f < 0 || f != math.Trunc(f) || f > 1e6 {
		return nil, false
	}
	v := int(f)
	return &v, true
}

var dateLayouts = []string{"2006-01-02", "2006/01/02", "02/01/2006", "2/1/2006", "02-01-2006", "2 Jan 2006", "02 Jan 2006", "2 January 2006", "Jan 2, 2006", "January 2, 2006", "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05"}

// ParseDay reads a date (day first when written with slashes).
func ParseDay(s string) (*time.Time, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, true
	}
	for _, l := range dateLayouts {
		if d, err := time.Parse(l, t); err == nil {
			if d.Year() < 2000 || d.Year() > 2100 {
				return nil, false
			}
			x := Day(d)
			return &x, true
		}
	}
	return nil, false
}

// CSVRow is one parsed line of a price list file.
type CSVRow struct {
	Line             int      `json:"line"`
	Supplier         string   `json:"supplier"`
	SupplierType     string   `json:"supplier_type"`     // "" = not given
	SupplierCurrency string   `json:"supplier_currency"` // "" = not given
	Kind             string   `json:"kind"`
	Name             string   `json:"name"`
	TLD              string   `json:"tld"`
	Period           string   `json:"period"`
	Price            float64  `json:"price"`
	RegularPrice     *float64 `json:"regular_price"`
	SetupFee         float64  `json:"setup_fee"`
	DiskGB           *float64 `json:"disk_gb"`
	Sites            *int     `json:"sites"`
	EmailAccounts    *int     `json:"email_accounts"`
	AsOf             string   `json:"as_of"` // YYYY-MM-DD, "" = today
	SourceURL        string   `json:"source_url"`
	Notes            string   `json:"notes"`
	Errors           []string `json:"errors"`
	Warnings         []string `json:"warnings"`
	// Filled in by the import.
	Action   string   `json:"action"` // create | update | unchanged | error | duplicate
	Currency string   `json:"currency"`
	OldPrice *float64 `json:"old_price,omitempty"`
	asOf     *time.Time
}

// Key is the row's identity within its supplier.
func (r CSVRow) Key() string {
	return NameKey(r.Supplier) + "|" + r.Kind + "|" + NameKey(r.Name) + "|" + r.TLD + "|" + r.Period
}

var headerAliases = map[string]string{
	"supplier": "supplier", "supplier_name": "supplier", "provider": "supplier", "vendor": "supplier",
	"supplier_type": "supplier_type", "type": "supplier_type",
	"supplier_currency": "supplier_currency", "currency": "supplier_currency",
	"kind": "kind", "item_kind": "kind", "category": "kind",
	"name": "name", "plan": "name", "product": "name", "item": "name",
	"tld": "tld", "extension": "tld",
	"period": "period", "billing_period": "period", "term": "period", "billing": "period",
	"price": "price", "amount": "price", "cost": "price",
	"regular_price": "regular_price", "renewal_price": "regular_price", "regular": "regular_price", "renews_at": "regular_price",
	"setup_fee": "setup_fee", "setup": "setup_fee",
	"disk_gb": "disk_gb", "disk": "disk_gb", "storage_gb": "disk_gb", "storage": "disk_gb",
	"sites": "sites", "websites": "sites",
	"email_accounts": "email_accounts", "mailboxes": "email_accounts", "emails": "email_accounts",
	"as_of": "as_of", "date": "as_of", "checked": "as_of", "asof": "as_of",
	"source_url": "source_url", "source": "source_url", "url": "source_url",
	"notes": "notes", "note": "notes", "comment": "notes", "comments": "notes",
}

// ParseError is a file that cannot be read at all.
type ParseError struct{ Msg string }

func (e *ParseError) Error() string { return e.Msg }

// ParseCSV reads a price list. It never stops at a bad row: each row carries
// its own errors and warnings. Lines starting with # and blank lines are
// skipped. Columns are found by header name (any order, any case).
func ParseCSV(data []byte) ([]CSVRow, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &ParseError{"The file is empty."}
	}
	// A ; or tab separated export (Excel in some locales).
	first := data
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		first = data[:i]
	}
	sep := ','
	if n := bytes.Count(first, []byte(",")); n == 0 {
		switch {
		case bytes.Count(first, []byte(";")) > 2:
			sep = ';'
		case bytes.Count(first, []byte("\t")) > 2:
			sep = '\t'
		}
	}
	rd := csv.NewReader(bytes.NewReader(data))
	rd.Comma = sep
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	rd.TrimLeadingSpace = true
	var cols map[string]int
	rows := []CSVRow{}
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			if pe, ok := err.(*csv.ParseError); ok {
				rows = append(rows, CSVRow{Line: pe.Line, Action: "error", Errors: []string{"could not read this line: " + pe.Err.Error()}, Warnings: []string{}})
				continue
			}
			return nil, &ParseError{"Could not read the file: " + err.Error()}
		}
		line := 0
		if len(rec) > 0 {
			line, _ = rd.FieldPos(0)
		}
		empty := true
		for _, c := range rec {
			if strings.TrimSpace(c) != "" {
				empty = false
			}
		}
		if empty || strings.HasPrefix(strings.TrimSpace(rec[0]), "#") {
			continue
		}
		if cols == nil {
			cols = map[string]int{}
			for i, h := range rec {
				if name, ok := headerAliases[token(strings.TrimPrefix(h, "\xef\xbb\xbf"))]; ok {
					if _, dup := cols[name]; !dup {
						cols[name] = i
					}
				}
			}
			missing := []string{}
			for _, need := range []string{"supplier", "kind", "price"} {
				if _, ok := cols[need]; !ok {
					missing = append(missing, need)
				}
			}
			if len(missing) > 0 {
				return nil, &ParseError{"The first line must be the header. Missing column" + plural(len(missing)) + ": " + strings.Join(missing, ", ") + ". Expected: " + CSVHeader}
			}
			continue
		}
		get := func(name string) string {
			i, ok := cols[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		rows = append(rows, parseRow(line, get))
		if len(rows) > 5000 {
			return nil, &ParseError{"The file has more than 5,000 rows — split it."}
		}
	}
	if cols == nil {
		return nil, &ParseError{"The file has no header line. Expected: " + CSVHeader}
	}
	// The same row twice: the last one wins.
	seen := map[string]int{}
	for i := range rows {
		r := &rows[i]
		if len(r.Errors) > 0 {
			continue
		}
		if j, dup := seen[r.Key()]; dup {
			rows[j].Action = "duplicate"
			rows[j].Warnings = append(rows[j].Warnings, fmt.Sprintf("replaced by line %d (same supplier, kind, name, TLD and period)", r.Line))
		}
		seen[r.Key()] = i
	}
	return rows, nil
}

func parseRow(line int, get func(string) string) CSVRow {
	r := CSVRow{Line: line, Errors: []string{}, Warnings: []string{}}
	bad := func(f string, a ...any) { r.Errors = append(r.Errors, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(f, a...)) }

	r.Supplier = reSpaces.ReplaceAllString(get("supplier"), " ")
	if r.Supplier == "" {
		bad("supplier is required")
	} else if len(r.Supplier) > 160 {
		bad("supplier name is too long")
	}
	if v := get("supplier_type"); v != "" {
		if t, ok := ParseType(v); ok {
			r.SupplierType = t
		} else {
			bad("supplier_type %q is not wholesale, self_hosted or competitor", v)
		}
	}
	if v := get("supplier_currency"); v != "" {
		if c, ok := ParseCurrency(v); ok {
			r.SupplierCurrency = c
		} else {
			bad("supplier_currency %q is not a supported currency code", v)
		}
	}
	if v := get("kind"); v == "" {
		bad("kind is required")
	} else if k, ok := ParseKind(v); ok {
		r.Kind = k
	} else {
		bad("kind %q is not one of hosting_plan, domain_register, domain_renew, domain_transfer, ssl, email_mailbox, other", v)
	}
	r.Name = reSpaces.ReplaceAllString(get("name"), " ")
	if len(r.Name) > 200 {
		bad("name is too long")
	}
	tld := get("tld")
	if IsDomainKind(r.Kind) {
		if tld == "" && strings.HasPrefix(r.Name, ".") {
			tld = r.Name // ".co.ke" typed in the name column
		}
		switch r.TLD = NormTLD(tld); {
		case tld == "":
			bad("tld is required for %s (e.g. .co.ke)", r.Kind)
		case r.TLD == "":
			bad("tld %q is not a domain ending like .co.ke", tld)
		}
		if r.Name == "" || NormTLD(r.Name) == r.TLD {
			r.Name = DomainItemName(r.Kind, r.TLD)
		}
	} else {
		if tld != "" && r.Kind != "" {
			warn("tld ignored: only domain prices have one")
		}
		if r.Name == "" && r.Kind != "" {
			bad("name is required")
		}
	}
	if v := get("period"); v == "" {
		if r.Kind != "" {
			r.Period = DefaultPeriod(r.Kind)
			warn("period blank — assumed %s", r.Period)
		}
	} else if p, ok := ParsePeriod(v); ok {
		r.Period = p
	} else {
		bad("period %q is not month, year, 2y, 3y or one_off", v)
	}
	if IsDomainKind(r.Kind) && (r.Period == PeriodMonth || r.Period == PeriodOneOff) {
		bad("a domain price is per year (year, 2y or 3y), not %s", r.Period)
	}
	switch v, blank, ok := ParseAmount(get("price")); {
	case !ok:
		bad("price %q is not a number", get("price"))
	case blank:
		bad("price is required")
	case v < 0:
		bad("price cannot be negative")
	default:
		r.Price = v
	}
	switch v, blank, ok := ParseAmount(get("regular_price")); {
	case !ok:
		bad("regular_price %q is not a number", get("regular_price"))
	case blank:
	case v < 0:
		bad("regular_price cannot be negative")
	case v > 0:
		x := v
		r.RegularPrice = &x
		if v < r.Price-0.0001 {
			warn("regular_price is lower than price — it renews cheaper than the first period")
		}
	}
	switch v, blank, ok := ParseAmount(get("setup_fee")); {
	case !ok:
		bad("setup_fee %q is not a number", get("setup_fee"))
	case blank:
	case v < 0:
		bad("setup_fee cannot be negative")
	default:
		r.SetupFee = v
	}
	if v, ok := ParseDiskGB(get("disk_gb")); ok {
		r.DiskGB = v
	} else {
		bad("disk_gb %q is not a number of GB", get("disk_gb"))
	}
	if v, ok := ParseCount(get("sites")); ok {
		r.Sites = v
	} else {
		bad("sites %q is not a whole number", get("sites"))
	}
	if v, ok := ParseCount(get("email_accounts")); ok {
		r.EmailAccounts = v
	} else {
		bad("email_accounts %q is not a whole number", get("email_accounts"))
	}
	if v, ok := ParseDay(get("as_of")); ok {
		if v != nil {
			r.asOf = v
			r.AsOf = v.Format("2006-01-02")
		}
	} else {
		bad("as_of %q is not a date (use YYYY-MM-DD)", get("as_of"))
	}
	if u, ok := CleanURL(get("source_url")); ok {
		r.SourceURL = u
	} else {
		// A note typed in the link column is not worth losing the price over.
		warn("source_url %q is not a web address — left out", get("source_url"))
	}
	r.Notes = get("notes")
	if len(r.Errors) > 0 {
		r.Action = "error"
	}
	return r
}

// DefaultPeriod is the billing period assumed when a row has none.
func DefaultPeriod(kind string) string {
	switch kind {
	case KindHosting, KindEmail:
		return PeriodMonth
	case KindOther:
		return PeriodOneOff
	}
	return PeriodYear
}

// DomainItemName names a domain price that was given without a name.
func DomainItemName(kind, tld string) string {
	return tld + " " + map[string]string{KindRegister: "registration", KindRenew: "renewal", KindTransfer: "transfer"}[kind]
}

func csvNum(v float64) string {
	return strconv.FormatFloat(math.Round(v*10000)/10000, 'f', -1, 64)
}

func csvCountF(v *float64) string {
	switch {
	case v == nil:
		return ""
	case *v == Unlimited:
		return "unlimited"
	}
	return csvNum(*v)
}

func csvCount(v *int) string {
	switch {
	case v == nil:
		return ""
	case *v == Unlimited:
		return "unlimited"
	}
	return strconv.Itoa(*v)
}

// ExportCSV writes items in the template's format. suppliers maps id →
// supplier.
func ExportCSV(items []Item, suppliers map[string]Supplier) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(CSVColumns)
	for _, it := range items {
		s := suppliers[it.SupplierID]
		name := it.Name
		if IsDomainKind(it.Kind) && name == DomainItemName(it.Kind, it.TLD) {
			name = ""
		}
		reg, setup, asOf := "", "", ""
		if it.RegularPrice != nil && *it.RegularPrice > 0 {
			reg = csvNum(*it.RegularPrice)
		}
		if it.SetupFee > 0 {
			setup = csvNum(it.SetupFee)
		}
		if it.AsOf != nil {
			asOf = it.AsOf.Format("2006-01-02")
		}
		_ = w.Write([]string{s.Name, s.Type, it.Currency, it.Kind, name, it.TLD, it.Period, csvNum(it.Price), reg, setup,
			csvCountF(it.DiskGB), csvCount(it.Sites), csvCount(it.EmailAccounts), asOf, it.SourceURL, it.Notes})
	}
	w.Flush()
	return b.Bytes()
}
