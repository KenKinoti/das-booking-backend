package expenses

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Synergy Wholesale exports two CSV files the business uploads every month:
//
//   - the account statement ("statement-September-2026.csv"):
//     date,"product type",description,credit,debit,balance — newest first,
//     balances like "$1.54" / "-$107.71" (a prepaid account: negative = owing);
//   - the hosting account usage report ("hosting-account-usage-2026-10-01.csv"):
//     Identifier,Status,Plan,Usage,Limit,"Is at 75%","% Used","Is at 100MB",Remaining
//     — one row per hosting service, disk in MB; the snapshot date is only in
//     the file name.
//
// This file reads and classifies them (pure functions, no database).

// Synergy transaction kinds.
const (
	SynHostingMonthly  = "hosting_monthly"       // "x.com (from 1 Sep 2026 to 1 Oct 2026)"
	SynHostingProrata  = "hosting_prorata_new"   // "x.com - (Pro rata for 23 Sep 26 to 1 Nov 26)"
	SynPlanUpgrade     = "plan_upgrade"          // "x.com (Plan changed from A to B)" with a debit
	SynPlanDowngrade   = "plan_downgrade_credit" // the same with a credit
	SynCancelCredit    = "cancellation_credit"   // "Prorata for cancelled hosting service (x.com)"
	SynDomainRenewal   = "domain_renewal"        // "Auto renewal of x.com.au for 1 year", registrations, transfers
	SynDomainDelete    = "domain_delete"         // "Domain deletion for x.com" (0.00)
	SynSupportFee      = "support_fee"           // Priority Ticket "Submission - Outside 9-5"
	SynTopup           = "topup"                 // Refill "Payment via Credit Card" — money into the account
	SynOther           = "other"
	SynFileStatement   = "statement"
	SynFileUsage       = "usage"
	CatSupport         = "support"
	synStatementSource = "synergy_statement"
)

// SynergyKinds lists the kinds in display order with their labels.
var SynergyKinds = []struct{ Key, Label string }{
	{SynHostingMonthly, "Monthly hosting"}, {SynHostingProrata, "Pro-rata (new service)"}, {SynPlanUpgrade, "Plan upgrade"},
	{SynPlanDowngrade, "Plan downgrade credit"}, {SynCancelCredit, "Cancellation credit"}, {SynDomainRenewal, "Domain renewal"},
	{SynDomainDelete, "Domain deletion"}, {SynSupportFee, "Support fee"}, {SynTopup, "Top-up (transfer)"}, {SynOther, "Other"},
}

// SynRow is one parsed statement line.
type SynRow struct {
	Line        int        `json:"line"`
	Seq         int        `json:"seq"` // position from the bottom of the file (oldest = 0): orders lines in the same second
	Date        time.Time  `json:"date"`
	ProductType string     `json:"product_type"`
	Description string     `json:"description"`
	Credit      float64    `json:"credit"`
	Debit       float64    `json:"debit"`
	Balance     float64    `json:"balance"`
	HasBalance  bool       `json:"has_balance"`
	Kind        string     `json:"kind"`
	Category    string     `json:"category"`
	Domain      string     `json:"domain"`
	PeriodFrom  *time.Time `json:"period_from"`
	PeriodTo    *time.Time `json:"period_to"`
	PlanFrom    string     `json:"plan_from"`
	PlanTo      string     `json:"plan_to"`
}

// Amount is the cost of the line: debit positive, credit negative.
func (r SynRow) Amount() float64 { return r2(r.Debit - r.Credit) }

// SynUsageRow is one hosting service in a usage report.
type SynUsageRow struct {
	Line        int     `json:"line"`
	Domain      string  `json:"domain"`
	Status      string  `json:"status"`
	Plan        string  `json:"plan"`
	UsageMB     float64 `json:"usage_mb"`
	LimitMB     float64 `json:"limit_mb"`
	PctUsed     float64 `json:"pct_used"`
	At75        bool    `json:"at_75"`
	At100MB     bool    `json:"at_100mb"`
	RemainingMB float64 `json:"remaining_mb"`
}

func readCSV(data []byte) ([][]string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	return r.ReadAll()
}

func headerIndex(rec []string) map[string]int {
	m := map[string]int{}
	for i, c := range rec {
		m[normHeader(c)] = i
	}
	return m
}

// DetectSynergyFile tells a Synergy statement from a hosting usage report by
// the header row ("" when the CSV is neither).
func DetectSynergyFile(data []byte) string {
	first, _, _ := bytes.Cut(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), []byte("\n"))
	recs, err := readCSV(first)
	if err != nil || len(recs) == 0 {
		return ""
	}
	h := headerIndex(recs[0])
	has := func(k ...string) bool {
		for _, x := range k {
			if _, ok := h[x]; !ok {
				return false
			}
		}
		return true
	}
	switch {
	case has("date", "product type", "description", "credit", "debit", "balance"):
		return SynFileStatement
	case has("identifier", "plan", "usage", "limit") && (has("remaining") || has("% used")):
		return SynFileUsage
	}
	return ""
}

// parseAmount reads "1.54", "$1.54", "-$107.71", "$-1.00", "(1.00)", "1,234.50".
func parseAmount(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, strings.Trim(s, "()")
	}
	s = strings.NewReplacer("A$", "", "AU$", "", "$", "", ",", "", " ", "", "AUD", "").Replace(s)
	if strings.HasPrefix(s, "-") {
		neg, s = !neg, s[1:]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return math.Round(v*100) / 100, true
}

var synDateLayouts = []string{"02 Jan 2006 15:04:05", "2 Jan 2006 15:04:05", "02 Jan 2006 15:04", "2 Jan 2006", "02 Jan 2006", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "02/01/2006 15:04:05", "02/01/2006", "2006-01-02"}

// parseSynDate reads the statement date (wall-clock time, kept as UTC).
func parseSynDate(s string) (time.Time, bool) {
	s = strings.Join(strings.Fields(s), " ")
	for _, l := range synDateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	if t := ParseDate(s); t != nil {
		return *t, true
	}
	return time.Time{}, false
}

// parseShortDate reads "1 Sep 2026" or "23 Sep 26".
func parseShortDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	for _, l := range []string{"2 Jan 2006", "2 Jan 06", "2 January 2006", "2 January 06"} {
		if t, err := time.Parse(l, s); err == nil {
			return &t
		}
	}
	return ParseDate(s)
}

var (
	reSynMonthly = regexp.MustCompile(`(?i)^\s*(.+?)\s*\(\s*from\s+(\d{1,2}\s+[a-z]+\s+\d{2,4})\s+to\s+(\d{1,2}\s+[a-z]+\s+\d{2,4})\s*\)\s*$`)
	reSynPlan    = regexp.MustCompile(`(?i)^\s*(.+?)\s*\(\s*plan\s+changed\s+from\s+(.+?)\s+to\s+(.+?)\s*\)\s*$`)
	reSynCancel  = regexp.MustCompile(`(?i)pro\s*-?\s*rata\s+for\s+cancel+ed\s+(?:hosting\s+)?service\s*\(\s*(.+?)\s*\)`)
	reSynProrata = regexp.MustCompile(`(?i)^\s*(.+?)\s*-?\s*\(\s*pro\s*-?\s*rata\s+for\s+(\d{1,2}\s+[a-z]+\s+\d{2,4})\s+to\s+(\d{1,2}\s+[a-z]+\s+\d{2,4})\s*\)`)
	reSynRenew   = regexp.MustCompile(`(?i)(?:auto\s+)?(?:renewal|registration|register|transfer)\s+(?:of|for)?\s*([a-z0-9][a-z0-9.-]*\.[a-z]{2,})`)
	reSynDelete  = regexp.MustCompile(`(?i)domain\s+deletion\s+(?:for|of)\s+([a-z0-9][a-z0-9.-]*\.[a-z]{2,})`)
	reSynDomain  = regexp.MustCompile(`(?i)\b([a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,})\b`)
	reSynTopup   = regexp.MustCompile(`(?i)payment\s+via|top\s*-?\s*up|add(?:ed)?\s+funds|account\s+funds|refill|deposit`)
)

func cleanDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "www."), "/")
	if i := strings.IndexAny(s, "/ "); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, ".-")
}

// ClassifySynergy works out what a statement line is.
func ClassifySynergy(productType, desc string, credit, debit float64) SynRow {
	r := SynRow{ProductType: strings.TrimSpace(productType), Description: strings.TrimSpace(desc), Credit: credit, Debit: debit, Kind: SynOther, Category: CatOther}
	pt := strings.ToLower(r.ProductType)
	d := r.Description
	switch {
	case pt == "refill" || (credit > 0 && debit == 0 && reSynTopup.MatchString(d) && !strings.Contains(strings.ToLower(d), "cancel")):
		r.Kind, r.Category = SynTopup, ""
	case strings.Contains(pt, "domain delete") || reSynDelete.MatchString(d):
		r.Kind, r.Category = SynDomainDelete, CatDomains
		if m := reSynDelete.FindStringSubmatch(d); m != nil {
			r.Domain = cleanDomain(m[1])
		}
	case strings.Contains(pt, "ticket") || strings.Contains(pt, "support"):
		r.Kind, r.Category = SynSupportFee, CatSupport
	case reSynCancel.MatchString(d):
		r.Kind, r.Category = SynCancelCredit, CatHosting
		r.Domain = cleanDomain(reSynCancel.FindStringSubmatch(d)[1])
	case reSynPlan.MatchString(d):
		m := reSynPlan.FindStringSubmatch(d)
		r.Domain, r.PlanFrom, r.PlanTo, r.Category = cleanDomain(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3]), CatHosting
		r.Kind = SynPlanUpgrade
		if credit > debit {
			r.Kind = SynPlanDowngrade
		}
	case reSynProrata.MatchString(d):
		m := reSynProrata.FindStringSubmatch(d)
		r.Kind, r.Category, r.Domain = SynHostingProrata, CatHosting, cleanDomain(m[1])
		r.PeriodFrom, r.PeriodTo = parseShortDate(m[2]), parseShortDate(m[3])
	case reSynMonthly.MatchString(d):
		m := reSynMonthly.FindStringSubmatch(d)
		r.Kind, r.Category, r.Domain = SynHostingMonthly, CatHosting, cleanDomain(m[1])
		r.PeriodFrom, r.PeriodTo = parseShortDate(m[2]), parseShortDate(m[3])
	case strings.Contains(pt, "renewal") || strings.Contains(pt, "registration") || strings.Contains(pt, "transfer") || reSynRenew.MatchString(d):
		r.Kind, r.Category = SynDomainRenewal, CatDomains
		if m := reSynRenew.FindStringSubmatch(d); m != nil {
			r.Domain = cleanDomain(m[1])
		}
	case strings.Contains(pt, "hosting"):
		r.Kind, r.Category = SynOther, CatHosting
	}
	if r.Domain == "" && r.Kind != SynTopup && r.Kind != SynSupportFee {
		if m := reSynDomain.FindStringSubmatch(d); m != nil {
			r.Domain = cleanDomain(m[1])
		}
	}
	return r
}

// ParseSynergyStatement reads a statement CSV. Rows come newest first; Seq
// numbers them from the bottom so lines in the same second keep their order.
func ParseSynergyStatement(data []byte) ([]SynRow, []string, error) {
	recs, err := readCSV(data)
	if err != nil {
		return nil, nil, err
	}
	if len(recs) == 0 {
		return nil, nil, parseError("the file is empty")
	}
	h := headerIndex(recs[0])
	col := func(k string) int {
		if i, ok := h[k]; ok {
			return i
		}
		return -1
	}
	cDate, cType, cDesc, cCred, cDeb, cBal := col("date"), col("product type"), col("description"), col("credit"), col("debit"), col("balance")
	if cDate < 0 || cDesc < 0 || cCred < 0 || cDeb < 0 {
		return nil, nil, parseError("not a Synergy Wholesale statement (expected date, product type, description, credit, debit, balance)")
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	rows := []SynRow{}
	warns := []string{}
	for i := 1; i < len(recs); i++ {
		rec := recs[i]
		if strings.TrimSpace(strings.Join(rec, "")) == "" {
			continue
		}
		dt, ok := parseSynDate(get(rec, cDate))
		if !ok {
			warns = append(warns, fmt.Sprintf("Line %d: unreadable date %q — skipped", i+1, get(rec, cDate)))
			continue
		}
		cr, okc := parseAmount(get(rec, cCred))
		db, okd := parseAmount(get(rec, cDeb))
		if !okc && !okd {
			warns = append(warns, fmt.Sprintf("Line %d: no credit or debit amount — skipped", i+1))
			continue
		}
		r := ClassifySynergy(get(rec, cType), get(rec, cDesc), math.Abs(cr), math.Abs(db))
		r.Line, r.Date = i+1, dt
		if b, ok := parseAmount(get(rec, cBal)); ok {
			r.Balance, r.HasBalance = b, true
		}
		if r.Kind == SynOther {
			warns = append(warns, fmt.Sprintf("Line %d: unrecognised line %q (%s) — counted as an other expense", i+1, r.Description, r.ProductType))
		}
		if (r.Kind == SynHostingMonthly || r.Kind == SynHostingProrata) && r.Credit > 0 {
			warns = append(warns, fmt.Sprintf("Line %d: a credit on a hosting charge (%s)", i+1, r.Description))
		}
		rows = append(rows, r)
	}
	n := len(rows)
	// Newest first in the file → oldest gets Seq 0. Detect oldest-first files by dates.
	oldestFirst := n > 1 && rows[0].Date.Before(rows[n-1].Date)
	for i := range rows {
		if oldestFirst {
			rows[i].Seq = i
		} else {
			rows[i].Seq = n - 1 - i
		}
	}
	return rows, warns, nil
}

// ParseSynergyUsage reads a hosting account usage report.
func ParseSynergyUsage(data []byte) ([]SynUsageRow, []string, error) {
	recs, err := readCSV(data)
	if err != nil {
		return nil, nil, err
	}
	if len(recs) == 0 {
		return nil, nil, parseError("the file is empty")
	}
	h := headerIndex(recs[0])
	col := func(ks ...string) int {
		for _, k := range ks {
			if i, ok := h[k]; ok {
				return i
			}
		}
		return -1
	}
	cID, cSt, cPlan, cUse, cLim := col("identifier", "domain"), col("status"), col("plan"), col("usage"), col("limit")
	c75, cPct, c100, cRem := col("is at 75%"), col("% used", "used %", "percent used"), col("is at 100mb"), col("remaining")
	if cID < 0 || cUse < 0 || cLim < 0 {
		return nil, nil, parseError("not a Synergy hosting usage report (expected Identifier, Plan, Usage, Limit)")
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	num := func(s string) float64 { v, _ := parseAmount(strings.TrimSuffix(strings.TrimSpace(s), "%")); return v }
	yes := func(s string) bool { s = strings.ToLower(s); return s == "true" || s == "yes" || s == "1" }
	out := []SynUsageRow{}
	warns := []string{}
	seen := map[string]bool{}
	for i := 1; i < len(recs); i++ {
		rec := recs[i]
		d := cleanDomain(get(rec, cID))
		if d == "" {
			continue
		}
		if seen[d] {
			warns = append(warns, fmt.Sprintf("Line %d: %s listed twice — the first row is kept", i+1, d))
			continue
		}
		seen[d] = true
		u := SynUsageRow{Line: i + 1, Domain: d, Status: get(rec, cSt), Plan: strings.TrimSpace(get(rec, cPlan)), UsageMB: num(get(rec, cUse)), LimitMB: num(get(rec, cLim)),
			At75: yes(get(rec, c75)), At100MB: yes(get(rec, c100))}
		if cPct >= 0 && get(rec, cPct) != "" {
			u.PctUsed = num(get(rec, cPct))
		} else if u.LimitMB > 0 {
			u.PctUsed = math.Round(u.UsageMB/u.LimitMB*100*100) / 100
		}
		if cRem >= 0 && get(rec, cRem) != "" {
			u.RemainingMB = num(get(rec, cRem))
		} else {
			u.RemainingMB = u.LimitMB - u.UsageMB
		}
		if u.LimitMB <= 0 {
			warns = append(warns, fmt.Sprintf("Line %d: %s has no disk limit", i+1, d))
		}
		out = append(out, u)
	}
	return out, warns, nil
}

var (
	reNameISO  = regexp.MustCompile(`(20\d{2})[-_.](\d{1,2})[-_.](\d{1,2})`)
	reNameDMY  = regexp.MustCompile(`(\d{1,2})[-_.](\d{1,2})[-_.](20\d{2})`)
	reNameComp = regexp.MustCompile(`(20\d{2})(\d{2})(\d{2})`)
)

// SnapshotDateFromName reads the snapshot date from a usage report's file
// name ("hosting-account-usage-2026-10-01.csv").
func SnapshotDateFromName(name string) *time.Time {
	at := func(y, m, d string) *time.Time {
		yi, _ := strconv.Atoi(y)
		mi, _ := strconv.Atoi(m)
		di, _ := strconv.Atoi(d)
		return mkDate(yi, time.Month(mi), di)
	}
	if m := reNameISO.FindStringSubmatch(name); m != nil {
		if t := at(m[1], m[2], m[3]); t != nil {
			return t
		}
	}
	if m := reNameDMY.FindStringSubmatch(name); m != nil {
		if t := at(m[3], m[2], m[1]); t != nil {
			return t
		}
	}
	if m := reNameComp.FindStringSubmatch(name); m != nil {
		if t := at(m[1], m[2], m[3]); t != nil {
			return t
		}
	}
	if t := ParseDate(strings.NewReplacer("-", " ", "_", " ").Replace(name)); t != nil {
		return t
	}
	return nil
}

// synTxnKey identifies a statement line across uploads (re-uploads and
// overlapping months never duplicate it).
func synTxnKey(r SynRow) string {
	k := fmt.Sprintf("%s|%s|%s|%.2f|%.2f|%.2f|%t", r.Date.Format("2006-01-02T15:04:05"), strings.ToLower(r.ProductType), strings.ToLower(strings.Join(strings.Fields(r.Description), " ")),
		r.Credit, r.Debit, r.Balance, r.HasBalance)
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:20])
}

// BalanceGap is a place where the running balance does not follow from the
// previous line (missing transactions, or a month not uploaded).
type BalanceGap struct {
	After      string  `json:"after"`  // date-time of the last line before the gap
	Before     string  `json:"before"` // date-time of the first line after it
	Expected   float64 `json:"expected"`
	Actual     float64 `json:"actual"`
	Difference float64 `json:"difference"`
	Message    string  `json:"message"`
}

// sortSynRows orders lines oldest first (date, then Seq).
func sortSynRows(rows []SynRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Date.Equal(rows[j].Date) {
			return rows[i].Date.Before(rows[j].Date)
		}
		return rows[i].Seq < rows[j].Seq
	})
}

// ReconcileSynergy checks the running balance line by line (rows oldest first).
func ReconcileSynergy(rows []SynRow) []BalanceGap {
	gaps := []BalanceGap{}
	var prev *SynRow
	for i := range rows {
		r := &rows[i]
		if !r.HasBalance {
			continue
		}
		if prev != nil {
			exp := r2(prev.Balance + r.Credit - r.Debit)
			if math.Abs(exp-r.Balance) > 0.005 {
				diff := r2(r.Balance - exp)
				gaps = append(gaps, BalanceGap{After: prev.Date.Format("2006-01-02 15:04:05"), Before: r.Date.Format("2006-01-02 15:04:05"), Expected: exp, Actual: r.Balance, Difference: diff,
					Message: fmt.Sprintf("Balance jumps between %s and %s: expected %s, the statement shows %s (%s unexplained) — a statement or lines may be missing",
						prev.Date.Format("2 Jan 2006"), r.Date.Format("2 Jan 2006"), audFmt(exp), audFmt(r.Balance), audSigned(diff))})
			}
		}
		prev = r
	}
	return gaps
}

// OpeningBalance is the balance before the oldest line (rows oldest first).
func OpeningBalance(rows []SynRow) *float64 {
	for _, r := range rows {
		if r.HasBalance {
			v := r2(r.Balance - r.Credit + r.Debit)
			return &v
		}
	}
	return nil
}

// ClosingBalance is the balance after the newest line (rows oldest first).
func ClosingBalance(rows []SynRow) *float64 {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].HasBalance {
			v := rows[i].Balance
			return &v
		}
	}
	return nil
}

func audFmt(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func audSigned(v float64) string {
	if v >= 0 {
		return fmt.Sprintf("+$%.2f", v)
	}
	return fmt.Sprintf("-$%.2f", -v)
}
