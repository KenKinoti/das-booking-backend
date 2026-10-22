package subscriptions

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Bulk import of existing clients' subscriptions from CSV. One row is one
// subscription line; rows with the same customer and "group" become one
// subscription with several lines (e.g. hosting + a .co.ke domain).
// next_renewal (the client's next renewal date) starts billing there, so
// clients who already paid up to then are not invoiced again.

// CSVTemplate is the documented template.
const CSVTemplate = "customer_email,customer_name,service,description,price,quantity,period,next_renewal,start_date,billing_day,prorate,payment_terms,grace_days,auto_send,auto_suspend,currency,site,domain,domain_expiry,supplier,supplier_plan,group,notes\n" +
	"jane@wanjiku.co.ke,Wanjiku Traders,Starter hosting — monthly,,,1,monthly,2026-11-01,,,,,,yes,yes,KES,wanjiku.co.ke,,,synergy,Micro Env,A,\n" +
	"jane@wanjiku.co.ke,Wanjiku Traders,,.co.ke domain renewal,1200,1,monthly,2026-11-01,,,,,,yes,yes,KES,,wanjiku.co.ke,2027-03-14,synergy,,A,Domain billed monthly with the hosting\n"

// ImportRow is the outcome of one CSV row.
type ImportRow struct {
	Line        int    `json:"line"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Customer    string `json:"customer"`
	CustomerID  string `json:"customer_id,omitempty"`
	NewCustomer bool   `json:"new_customer,omitempty"`
	Service     string `json:"service"`
	Period      string `json:"period"`
	Start       string `json:"start"`
	Group       string `json:"group"`
	Number      string `json:"number,omitempty"`
}

// ImportResult is a dry run or an import.
type ImportResult struct {
	DryRun    bool        `json:"dry_run"`
	Rows      []ImportRow `json:"rows"`
	Valid     int         `json:"valid"`
	Invalid   int         `json:"invalid"`
	Created   int         `json:"created"` // subscriptions
	Customers int         `json:"customers_created"`
	Numbers   []string    `json:"numbers"`
}

func normHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
	h = strings.NewReplacer(" ", "_", "-", "_").Replace(h)
	switch h {
	case "email", "client_email", "customer_e_mail":
		return "customer_email"
	case "client", "client_name", "name", "customer":
		return "customer_name"
	case "plan", "plan_name", "package", "service_name", "product":
		return "service"
	case "item", "line":
		return "description"
	case "amount", "unit_price":
		return "price"
	case "qty":
		return "quantity"
	case "billing_period", "frequency", "cycle":
		return "period"
	case "next_due", "next_due_date", "renewal", "renewal_date", "next_renewal_date", "paid_until_next":
		return "next_renewal"
	case "start":
		return "start_date"
	case "terms", "payment_terms_days":
		return "payment_terms"
	case "hosted_site", "site_domain", "website":
		return "site"
	}
	return h
}

func parsePeriod(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "monthly", "month", "1m", "per month":
		return PeriodMonthly
	case "quarterly", "quarter", "3m", "3 months":
		return PeriodQuarterly
	case "yearly", "year", "annual", "annually", "12m", "1y":
		return PeriodYearly
	}
	return ""
}

func parseDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if d, ok := ParseDay(v); ok {
		return d, true
	}
	for _, f := range []string{"2/1/2006", "02/01/2006", "2 Jan 2006", "2 January 2006", "Jan 2 2006"} {
		if t, err := time.Parse(f, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func yes(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "y", "yes", "true", "1", "on":
		return true, true
	case "n", "no", "false", "0", "off":
		return false, true
	}
	return false, false
}

type importGroup struct {
	key   string
	rows  []int
	input SubInput
	name  string
}

// Import reads the CSV; dryRun only checks it. createCustomers adds
// customers that don't exist yet (by email, with customer_name).
func (s *Service) Import(org, user string, r io.Reader, dryRun, createCustomers bool) (*ImportResult, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	all, err := cr.ReadAll()
	if err != nil {
		return nil, invalid("the file is not valid CSV: %v", err)
	}
	if len(all) < 2 {
		return nil, invalid("the file has no rows — use the template")
	}
	if len(all) > 2001 {
		return nil, invalid("import at most 2000 rows at a time")
	}
	cols := map[string]int{}
	for i, h := range all[0] {
		cols[normHeader(h)] = i
	}
	if _, ok := cols["customer_email"]; !ok {
		if _, ok2 := cols["customer_name"]; !ok2 {
			return nil, invalid("the file needs a customer_email (or customer_name) column")
		}
	}
	get := func(rec []string, k string) string {
		if i, ok := cols[k]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	res := &ImportResult{DryRun: dryRun, Rows: []ImportRow{}, Numbers: []string{}}
	cat := s.Catalog(org)
	svcByName := map[string]CatalogService{}
	for _, c := range cat {
		svcByName[strings.ToLower(c.Name)] = c
		if _, dup := svcByName[strings.ToLower(c.Label)]; !dup {
			svcByName[strings.ToLower(c.Label)] = c
		}
	}
	// Customers by email and by name.
	type cust struct{ ID, Email, Name string }
	var cl []cust
	if hasTable(s.DB, "customers") {
		s.DB.Raw(`SELECT id, LOWER(COALESCE(email,'')) AS email, LOWER(TRIM(COALESCE(NULLIF(company_name,''), first_name || ' ' || last_name))) AS name FROM customers
			WHERE organization_id = ? AND deleted_at IS NULL`, org).Scan(&cl)
	}
	byEmail, byName := map[string]string{}, map[string]string{}
	for _, c := range cl {
		if c.Email != "" {
			byEmail[c.Email] = c.ID
		}
		if c.Name != "" {
			byName[c.Name] = c.ID
		}
	}
	newCust := map[string]string{} // email → pending id
	groups := []*importGroup{}
	byKey := map[string]*importGroup{}
	for n, rec := range all[1:] {
		line := n + 2
		row := ImportRow{Line: line}
		empty := true
		for _, v := range rec {
			if strings.TrimSpace(v) != "" {
				empty = false
			}
		}
		if empty {
			continue
		}
		fail := func(format string, a ...any) {
			row.Error = fmt.Sprintf(format, a...)
			res.Rows = append(res.Rows, row)
			res.Invalid++
		}
		email := strings.ToLower(get(rec, "customer_email"))
		name := get(rec, "customer_name")
		row.Customer = firstNonEmpty(name, email)
		cid := get(rec, "customer_id")
		if cid == "" && email != "" {
			cid = byEmail[email]
		}
		if cid == "" && email == "" && name != "" {
			cid = byName[strings.ToLower(name)]
		}
		if cid == "" {
			if !createCustomers {
				fail("customer %s not found — add them first or tick “Create missing customers”", firstNonEmpty(email, name))
				continue
			}
			if !mail.ValidAddress(email) || name == "" {
				fail("a new customer needs customer_email and customer_name")
				continue
			}
			if id, ok := newCust[email]; ok {
				cid = id
			} else {
				cid = "new:" + email
				newCust[email] = cid
			}
			row.NewCustomer = true
		}
		row.CustomerID = cid
		period := parsePeriod(get(rec, "period"))
		if period == "" {
			fail("unknown period %q (monthly, quarterly or yearly)", get(rec, "period"))
			continue
		}
		row.Period = period
		li := LineInput{Description: get(rec, "description")}
		if pn := get(rec, "service"); pn != "" {
			c, ok := svcByName[strings.ToLower(strings.TrimSpace(pn))]
			if !ok {
				fail("service %q is not in the catalogue (Settings → Services)", pn)
				continue
			}
			li.ServiceID = c.ID
			row.Service = c.Name
		} else {
			row.Service = li.Description
		}
		if v := get(rec, "price"); v != "" {
			f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
			if err != nil || f < 0 {
				fail("price %q is not a number", v)
				continue
			}
			li.UnitPrice = &f
		}
		if li.ServiceID == "" && (li.Description == "" || li.UnitPrice == nil) {
			fail("give a catalogue service, or a description and a price")
			continue
		}
		if v := get(rec, "quantity"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				fail("quantity %q is not a positive number", v)
				continue
			}
			li.Quantity = f
		}
		start := ""
		if v := firstNonEmpty(get(rec, "next_renewal"), get(rec, "start_date")); v != "" {
			d, ok := parseDate(v)
			if !ok {
				fail("date %q is not a date (YYYY-MM-DD)", v)
				continue
			}
			start = DayStr(d)
		}
		row.Start = start
		in := SubInput{CustomerID: cid, Period: period, StartDate: start, Currency: strings.ToUpper(get(rec, "currency")), SiteDomain: get(rec, "site"),
			Domain: get(rec, "domain"), DomainExpiry: get(rec, "domain_expiry"), SupplierPlan: get(rec, "supplier_plan"), Notes: get(rec, "notes"), Source: "csv"}
		if v := in.DomainExpiry; v != "" {
			d, ok := parseDate(v)
			if !ok {
				fail("domain_expiry %q is not a date (YYYY-MM-DD)", v)
				continue
			}
			in.DomainExpiry = DayStr(d)
		}
		switch strings.ToLower(get(rec, "supplier")) {
		case "":
		case "synergy", "synergy wholesale", "synergy_wholesale":
			in.Supplier = SupplierSynergy
		case "hosting.com", "hosting_com", "hostingcom", "a2", "a2 hosting":
			in.Supplier = SupplierHostingCom
		default:
			in.Supplier = SupplierOther
		}
		if v := get(rec, "billing_day"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 31 {
				fail("billing_day %q must be 1 to 31", v)
				continue
			}
			in.BillingDay = n
		}
		if v := get(rec, "prorate"); v != "" {
			b, ok := yes(v)
			if !ok {
				fail("prorate must be yes or no")
				continue
			}
			in.Prorate = b
		}
		if v := get(rec, "auto_send"); v != "" {
			b, ok := yes(v)
			if !ok {
				fail("auto_send must be yes or no")
				continue
			}
			in.AutoSend = &b
		}
		if v := get(rec, "auto_suspend"); v != "" {
			b, ok := yes(v)
			if !ok {
				fail("auto_suspend must be yes or no")
				continue
			}
			in.AutoSuspend = &b
		}
		if v := get(rec, "grace_days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				fail("grace_days %q is not a number of days", v)
				continue
			}
			in.GraceDays = &n
		}
		if v := get(rec, "payment_terms"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				fail("payment_terms %q is not a number of days", v)
				continue
			}
			in.PaymentTermsDays = &n
		}
		row.Group = get(rec, "group")
		key := cid + "|" + row.Group
		if row.Group == "" {
			key = cid + "|line" + strconv.Itoa(line)
		}
		g := byKey[key]
		if g == nil {
			g = &importGroup{key: key, input: in, name: name}
			byKey[key] = g
			groups = append(groups, g)
		} else if g.input.Period != period {
			fail("rows of group %s must share one billing period", row.Group)
			continue
		} else {
			// Later rows of a group fill in what the first left empty (e.g. the domain).
			for _, x := range [][2]*string{{&g.input.SiteDomain, &in.SiteDomain}, {&g.input.Domain, &in.Domain}, {&g.input.DomainExpiry, &in.DomainExpiry},
				{&g.input.SupplierPlan, &in.SupplierPlan}, {&g.input.Supplier, &in.Supplier}, {&g.input.Currency, &in.Currency}} {
				if *x[0] == "" {
					*x[0] = *x[1]
				}
			}
			if in.Notes != "" {
				g.input.Notes = strings.TrimSpace(g.input.Notes + "\n" + in.Notes)
			}
		}
		g.input.Lines = append(g.input.Lines, li)
		g.rows = append(g.rows, len(res.Rows))
		row.OK = true
		res.Rows = append(res.Rows, row)
		res.Valid++
	}
	// Check every group as the API would (customers that will be created
	// are checked against a stand-in).
	for _, g := range groups {
		if strings.HasPrefix(g.input.CustomerID, "new:") {
			continue
		}
		probe := &Subscription{}
		if _, err := s.apply(org, probe, g.input, false); err != nil {
			for _, i := range g.rows {
				if res.Rows[i].OK {
					res.Rows[i].OK, res.Rows[i].Error = false, err.Error()
					res.Valid--
					res.Invalid++
				}
			}
		}
	}
	if dryRun || res.Invalid > 0 {
		return res, nil
	}
	created := map[string]string{}
	// New customers are in the organisation's country (the customers table
	// would otherwise default the address country), in the row's currency
	// when it gives one.
	cc := strings.ToUpper(orgprefs.Country(s.DB, org))
	for email, pending := range newCust {
		name, cur := "", ""
		for _, g := range groups {
			if g.input.CustomerID == pending {
				name, cur = g.name, g.input.Currency
				break
			}
		}
		id := uuid.NewString()
		now := s.now()
		if err := s.DB.Exec(`INSERT INTO customers (id, first_name, last_name, email, phone, company_name, organization_id, is_active, created_at, updated_at, source,
			country_code, address_country, currency)
			VALUES (?, ?, '', ?, '', ?, ?, true, ?, ?, 'csv', ?, ?, ?)`, id, name, email, name, org, now, now, cc, currency.CountryName(cc), cur).Error; err != nil {
			return nil, err
		}
		created[pending] = id
		res.Customers++
	}
	for _, g := range groups {
		if id, ok := created[g.input.CustomerID]; ok {
			g.input.CustomerID = id
		}
		sub, err := s.Create(org, user, g.input)
		if err != nil {
			for _, i := range g.rows {
				res.Rows[i].OK, res.Rows[i].Error = false, err.Error()
			}
			continue
		}
		res.Created++
		res.Numbers = append(res.Numbers, sub.Number)
		for _, i := range g.rows {
			res.Rows[i].Number = sub.Number
			res.Rows[i].CustomerID = sub.CustomerID
		}
	}
	return res, nil
}
