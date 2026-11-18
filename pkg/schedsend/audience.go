package schedsend

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// MaxRecipients bounds one occurrence.
const MaxRecipients = 5000

func fresh(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{NewDB: true}) }

func hasTable(db *gorm.DB, name string) bool {
	var ok bool
	if err := fresh(db).Raw("SELECT to_regclass(?) IS NOT NULL", name).Row().Scan(&ok); err != nil {
		return false
	}
	return ok
}

func hasColumn(db *gorm.DB, table, col string) bool {
	var n int64
	fresh(db).Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`, table, col).Scan(&n)
	return n > 0
}

// Recipient is one customer or staff member.
type Recipient struct {
	Type      string `json:"type"` // customer | user
	ID        string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Company   string `json:"company"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Country   string `json:"country"`
	Deleted   bool   `json:"deleted,omitempty"`
	OptedOut  bool   `json:"opted_out,omitempty"`
	Level     string `json:"level,omitempty"`
}

// Name is the full name (or the company, or the email).
func (r Recipient) Name() string {
	n := strings.TrimSpace(r.FirstName + " " + r.LastName)
	if n == "" {
		n = r.Company
	}
	if n == "" {
		n = r.Email
	}
	return n
}

// Key is the delivery recipient key.
func (r Recipient) Key() string { return r.Type + ":" + r.ID }

type customerRow struct {
	ID, FirstName, LastName, Email, Phone, CompanyName, CountryCode string
	Deleted                                                         bool
}

const customerCols = `c.id, COALESCE(c.first_name,'') AS first_name, COALESCE(c.last_name,'') AS last_name, COALESCE(c.email,'') AS email,
	COALESCE(c.phone,'') AS phone, COALESCE(c.company_name,'') AS company_name, COALESCE(c.country_code,'') AS country_code,
	(c.deleted_at IS NOT NULL) AS deleted`

func (s *Service) optedOut(org string, ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	var got []string
	fresh(s.DB).Model(&OptOut{}).Where("organization_id = ? AND customer_id IN ?", org, ids).Pluck("customer_id", &got)
	for _, id := range got {
		out[id] = true
	}
	return out
}

func toRecipients(rows []customerRow, opted map[string]bool) []Recipient {
	out := make([]Recipient, 0, len(rows))
	for _, c := range rows {
		out = append(out, Recipient{Type: "customer", ID: c.ID, FirstName: strings.TrimSpace(c.FirstName), LastName: strings.TrimSpace(c.LastName),
			Company: strings.TrimSpace(c.CompanyName), Email: strings.ToLower(strings.TrimSpace(c.Email)), Phone: strings.TrimSpace(c.Phone),
			Country: strings.ToUpper(strings.TrimSpace(c.CountryCode)), Deleted: c.Deleted, OptedOut: opted[c.ID]})
	}
	return out
}

// today is the organisation's calendar date (for "overdue").
func (s *Service) today(org string) string {
	loc, err := LoadZone(s.orgTZ(org))
	if err != nil {
		loc = time.UTC
	}
	return Now().In(loc).Format(DateLayout)
}

// Customers resolves a customer audience at now. Chosen customers are
// returned even when deleted (so the skip is reported); groups only include
// active ones.
func (s *Service) Customers(org string, a Audience) ([]Recipient, error) {
	if !hasTable(s.DB, "customers") {
		return []Recipient{}, nil
	}
	var rows []customerRow
	switch a.Mode {
	case AudOne, AudChosen:
		ids := uniq(a.CustomerIDs)
		if len(ids) == 0 {
			return nil, fmt.Errorf("choose at least one customer")
		}
		if err := fresh(s.DB).Raw(`SELECT `+customerCols+` FROM customers c WHERE c.organization_id = ? AND c.id IN ?`, org, ids).Scan(&rows).Error; err != nil {
			return nil, err
		}
		// Keep the order they were chosen in; unknown ids count as deleted.
		byID := map[string]customerRow{}
		for _, r := range rows {
			byID[r.ID] = r
		}
		rows = rows[:0]
		for _, id := range ids {
			if r, ok := byID[id]; ok {
				rows = append(rows, r)
			} else {
				rows = append(rows, customerRow{ID: id, Deleted: true})
			}
		}
	case AudGroup:
		q := `SELECT ` + customerCols + ` FROM customers c WHERE c.organization_id = @org AND c.deleted_at IS NULL AND COALESCE(c.is_active, true)`
		args := map[string]any{"org": org, "today": s.today(org), "val": strings.TrimSpace(a.GroupValue)}
		openInv := `SELECT i.customer_id FROM billing_invoices i WHERE i.organization_id = @org AND i.doc_type = 'invoice' AND i.status IN ('sent','partial')
			AND i.balance_due > 0 AND i.deleted_at IS NULL AND i.customer_id IS NOT NULL`
		switch a.Group {
		case GroupAllEmail:
			q += ` AND COALESCE(c.email,'') <> ''`
		case GroupAllPhone:
			q += ` AND COALESCE(c.phone,'') <> ''`
		case GroupType:
			switch strings.ToLower(args["val"].(string)) {
			case "business":
				q += ` AND COALESCE(c.company_name,'') <> ''`
			case "individual":
				q += ` AND COALESCE(c.company_name,'') = ''`
			default:
				return nil, fmt.Errorf("customer type must be business or individual")
			}
		case GroupCountry:
			if len(args["val"].(string)) != 2 {
				return nil, fmt.Errorf("choose a country")
			}
			args["val"] = strings.ToUpper(args["val"].(string))
			q += ` AND UPPER(COALESCE(c.country_code,'')) = @val`
		case GroupOverdue:
			if !hasTable(s.DB, "billing_invoices") {
				return []Recipient{}, nil
			}
			q += ` AND c.id IN (` + openInv + ` AND i.due_date < CAST(@today AS date))`
		case GroupOpenBalance:
			if !hasTable(s.DB, "billing_invoices") {
				return []Recipient{}, nil
			}
			q += ` AND c.id IN (` + openInv + `)`
		case GroupSubscriptions:
			if !hasTable(s.DB, "client_subscriptions") {
				return []Recipient{}, nil
			}
			q += ` AND c.id IN (SELECT cs.customer_id FROM client_subscriptions cs WHERE cs.organization_id = @org AND cs.status IN ('active','past_due'))`
		default:
			return nil, fmt.Errorf("unknown customer group %q", a.Group)
		}
		q += ` ORDER BY c.first_name, c.last_name, c.id LIMIT ` + fmt.Sprint(MaxRecipients+1)
		if err := fresh(s.DB).Raw(q, args).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) > MaxRecipients {
			return nil, fmt.Errorf("this group has more than %d customers — choose a smaller group", MaxRecipients)
		}
	default:
		return nil, fmt.Errorf("choose who to send to")
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return toRecipients(rows, s.optedOut(org, ids)), nil
}

// Customer is one customer as a recipient (nil when unknown).
func (s *Service) Customer(org, id string) *Recipient {
	if id == "" || !hasTable(s.DB, "customers") {
		return nil
	}
	var rows []customerRow
	fresh(s.DB).Raw(`SELECT `+customerCols+` FROM customers c WHERE c.organization_id = ? AND c.id = ?`, org, id).Scan(&rows)
	if len(rows) == 0 {
		return nil
	}
	r := toRecipients(rows, s.optedOut(org, []string{id}))[0]
	return &r
}

// Staff resolves a team audience: chosen users, users at the chosen levels,
// or every active user.
func (s *Service) Staff(org string, a Audience) ([]Recipient, error) {
	var rows []struct {
		ID, Email, FirstName, LastName, Phone, Role string
	}
	phone := "''"
	if hasColumn(s.DB, "users", "phone") {
		phone = "COALESCE(phone,'')"
	}
	q := fresh(s.DB).Raw(`SELECT id, COALESCE(email,'') AS email, COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name,
		`+phone+` AS phone, COALESCE(role,'') AS role FROM users WHERE organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active)
		ORDER BY first_name, last_name, id`, org)
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range a.UserIDs {
		want[id] = true
	}
	lv := map[string]bool{}
	for _, l := range a.Levels {
		lv[strings.ToLower(strings.TrimSpace(l))] = true
	}
	out := []Recipient{}
	for _, u := range rows {
		level, _ := emailprefs.LevelOf(org, u.ID, u.Role)
		if len(want) > 0 || len(lv) > 0 {
			if !want[u.ID] && !lv[level] {
				continue
			}
		}
		out = append(out, Recipient{Type: "user", ID: u.ID, FirstName: strings.TrimSpace(u.FirstName), LastName: strings.TrimSpace(u.LastName),
			Email: strings.ToLower(strings.TrimSpace(u.Email)), Phone: strings.TrimSpace(u.Phone), Level: level})
	}
	if len(want) > 0 && len(out) == 0 && len(lv) == 0 {
		return nil, fmt.Errorf("none of the chosen people has an active account")
	}
	return out, nil
}

// ---------------------------------------------------------------- invoices

// OpenInvoice is an unpaid invoice of a customer.
type OpenInvoice struct {
	ID          string    `json:"id"`
	CustomerID  string    `json:"customer_id"`
	Number      string    `json:"number"`
	Currency    string    `json:"currency"`
	Total       float64   `json:"total"`
	BalanceDue  float64   `json:"balance_due"`
	DueDate     time.Time `json:"due_date"`
	PublicToken string    `json:"-"`
	Status      string    `json:"status"`
}

// openInvoices lists the unpaid invoices of customers, oldest due first.
func (s *Service) openInvoices(org string, customerIDs []string) map[string][]OpenInvoice {
	out := map[string][]OpenInvoice{}
	if len(customerIDs) == 0 || !hasTable(s.DB, "billing_invoices") {
		return out
	}
	var rows []OpenInvoice
	fresh(s.DB).Raw(`SELECT id, customer_id, COALESCE(number,'') AS number, COALESCE(currency,'') AS currency, COALESCE(total,0) AS total,
		COALESCE(balance_due,0) AS balance_due, due_date, COALESCE(public_token,'') AS public_token, status
		FROM billing_invoices WHERE organization_id = ? AND customer_id IN ? AND doc_type = 'invoice' AND status IN ('sent','partial')
		AND balance_due > 0 AND deleted_at IS NULL ORDER BY due_date, number`, org, customerIDs).Scan(&rows)
	for _, r := range rows {
		out[r.CustomerID] = append(out[r.CustomerID], r)
	}
	return out
}

// InvoiceState is what the engine checks before an invoice send.
type InvoiceState struct {
	ID          string
	Number      string
	DocType     string
	Status      string
	CustomerID  string
	ClientName  string
	ClientEmail string
	Currency    string
	Total       float64
	BalanceDue  float64
	DueDate     time.Time
	PublicToken string
	Deleted     bool
	Found       bool
}

// Invoice loads the invoice's current state.
func (s *Service) Invoice(org, id string) InvoiceState {
	st := InvoiceState{ID: id}
	if id == "" || !hasTable(s.DB, "billing_invoices") {
		return st
	}
	var rows []struct {
		ID, Number, DocType, Status, CustomerID, ClientName, ClientEmail, Currency, PublicToken string
		Total, BalanceDue                                                                       float64
		DueDate                                                                                 time.Time
		Deleted                                                                                 bool
	}
	fresh(s.DB).Raw(`SELECT id, COALESCE(number,'') AS number, COALESCE(doc_type,'invoice') AS doc_type, COALESCE(status,'') AS status,
		COALESCE(customer_id,'') AS customer_id, COALESCE(client_name,'') AS client_name, COALESCE(client_email,'') AS client_email,
		COALESCE(currency,'') AS currency, COALESCE(public_token,'') AS public_token, COALESCE(total,0) AS total, COALESCE(balance_due,0) AS balance_due,
		due_date, (deleted_at IS NOT NULL) AS deleted FROM billing_invoices WHERE organization_id = ? AND id = ?`, org, id).Scan(&rows)
	if len(rows) == 0 {
		return st
	}
	r := rows[0]
	return InvoiceState{ID: r.ID, Number: r.Number, DocType: r.DocType, Status: r.Status, CustomerID: r.CustomerID, ClientName: r.ClientName,
		ClientEmail: r.ClientEmail, Currency: r.Currency, Total: r.Total, BalanceDue: r.BalanceDue, DueDate: r.DueDate, PublicToken: r.PublicToken,
		Deleted: r.Deleted, Found: true}
}

// paymentExists reports whether a payment of the invoice still exists.
func (s *Service) paymentExists(org, invoiceID, paymentID string) bool {
	if !hasTable(s.DB, "billing_payments") {
		return false
	}
	var n int64
	q := `SELECT COUNT(*) FROM billing_payments WHERE organization_id = ? AND invoice_id = ? AND id = ?`
	if hasColumn(s.DB, "billing_payments", "deleted_at") {
		q += ` AND deleted_at IS NULL`
	}
	fresh(s.DB).Raw(q, org, invoiceID, paymentID).Scan(&n)
	return n > 0
}

// ---------------------------------------------------------------- merge values

func (s *Service) payLink(org, token string) string {
	base := strings.TrimRight(s.AppURL, "/")
	if base == "" || token == "" {
		return ""
	}
	if invoicing.OnlinePay != nil && invoicing.OnlinePay(s.DB, org) {
		return invoicing.PayURL(base, token)
	}
	return invoicing.ViewURL(base, token)
}

func longDate(t time.Time) string { return t.Format("2 Jan 2006") }

// balanceText adds balances per currency ("KSh 1,000.00 + US$50.00").
func balanceText(list []OpenInvoice, home string) string {
	if len(list) == 0 {
		return invoicing.FormatMoney(0, home)
	}
	sum := map[string]float64{}
	order := []string{}
	for _, i := range list {
		if _, ok := sum[i.Currency]; !ok {
			order = append(order, i.Currency)
		}
		sum[i.Currency] += i.BalanceDue
	}
	sort.SliceStable(order, func(a, b int) bool { return order[a] == home && order[b] != home })
	parts := []string{}
	for _, c := range order {
		parts = append(parts, invoicing.FormatMoney(sum[c], c))
	}
	return strings.Join(parts, " + ")
}

// Values are the merge values for one recipient. focus is the invoice
// {invoice_number} … refer to (nil = the customer's oldest unpaid invoice).
func (s *Service) Values(org string, r Recipient, open []OpenInvoice, focus *OpenInvoice, marketingLink string) map[string]string {
	loc, _ := LoadZone(s.orgTZ(org))
	if loc == nil {
		loc = time.UTC
	}
	now := Now().In(loc)
	home := s.homeCurrency(org)
	company := r.Company
	if company == "" {
		company = r.Name()
	}
	v := map[string]string{
		"first_name": firstNonEmpty(r.FirstName, r.Name(), "there"), "last_name": r.LastName, "name": firstNonEmpty(r.Name(), "there"), "company": company,
		"email": r.Email, "phone": r.Phone, "business": s.orgName(org), "today": longDate(now), "month": now.Format("January 2006"),
		"balance": balanceText(open, home), "open_invoices": fmt.Sprint(len(open)), "unsubscribe_link": marketingLink,
	}
	lines := []string{}
	for _, i := range open {
		lines = append(lines, fmt.Sprintf("%s · due %s · %s", i.Number, longDate(i.DueDate), invoicing.FormatMoney(i.BalanceDue, i.Currency)))
	}
	v["statement"] = strings.Join(lines, "\n")
	if len(lines) == 0 {
		v["statement"] = "Nothing outstanding — thank you!"
	}
	if focus == nil {
		today := now.Format(DateLayout)
		for i := range open {
			if open[i].DueDate.Format(DateLayout) < today {
				focus = &open[i]
				break
			}
		}
		if focus == nil && len(open) > 0 {
			focus = &open[0]
		}
	}
	if focus != nil {
		v["invoice_number"] = focus.Number
		v["amount_due"] = invoicing.FormatMoney(focus.BalanceDue, focus.Currency)
		v["due_date"] = longDate(focus.DueDate)
		days := int(now.Sub(time.Date(focus.DueDate.Year(), focus.DueDate.Month(), focus.DueDate.Day(), 0, 0, 0, 0, loc)).Hours() / 24)
		if days < 0 {
			days = 0
		}
		v["days_overdue"] = fmt.Sprint(days)
		v["pay_link"] = s.payLink(org, focus.PublicToken)
	} else {
		v["invoice_number"], v["amount_due"], v["due_date"], v["days_overdue"], v["pay_link"] = "", invoicing.FormatMoney(0, home), "", "0", ""
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// validEmail is mail.ValidAddress for a bare address.
func validEmail(e string) bool { return e != "" && mail.ValidAddress(e) }
