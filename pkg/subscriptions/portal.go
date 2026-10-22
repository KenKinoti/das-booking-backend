package subscriptions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// The client-facing "Your services" page: one signed link per customer
// (no sign-in) listing their subscriptions, next due dates and a pay button
// for every open invoice (the invoice's own /pay page: Flutterwave, M-Pesa
// and card where enabled). The link is an HMAC of the organisation and
// customer, so it can't be guessed or edited to show someone else's.

// Tokens are "<customer id>.<signature>" in lower case (hex), so they never
// look like an amount to the invoice email's checks.

func (s *Service) portalSig(org, customer string) string {
	m := hmac.New(sha256.New, append([]byte("subscriptions-portal:"), s.Secret...))
	m.Write([]byte("v1|" + org + "|" + customer))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// PortalToken is the signed token of a customer's page ("" without a secret).
func (s *Service) PortalToken(org, customer string) string {
	if len(s.Secret) == 0 || org == "" || customer == "" {
		return ""
	}
	return customer + "." + s.portalSig(org, customer)
}

// splitPortalToken returns the customer id and signature of a token.
func splitPortalToken(tok string) (string, string, bool) {
	i := strings.LastIndex(tok, ".")
	if i <= 0 || i == len(tok)-1 || len(tok) > 200 {
		return "", "", false
	}
	return tok[:i], tok[i+1:], true
}

// ParsePortalToken checks a token and returns the organisation and customer.
func (s *Service) ParsePortalToken(tok string) (org, customer string, ok bool) {
	cid, sig, ok := splitPortalToken(tok)
	if !ok || len(s.Secret) == 0 {
		return "", "", false
	}
	var orgs []string
	if s.DB != nil && hasTable(s.DB, "customers") {
		s.DB.Raw(`SELECT organization_id FROM customers WHERE id = ? AND deleted_at IS NULL`, cid).Scan(&orgs)
	}
	for _, o := range orgs {
		if hmac.Equal([]byte(s.portalSig(o, cid)), []byte(sig)) {
			return o, cid, true
		}
	}
	return "", "", false
}

// PortalURL is the customer's "Your services" link ("" when the app address is unknown).
func (s *Service) PortalURL(org, customer string) string {
	tok := s.PortalToken(org, customer)
	if tok == "" {
		return ""
	}
	return s.appLink("/your-services/" + tok)
}

// PortalSub is one subscription on the page.
type PortalSub struct {
	Number   string   `json:"number"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Period   string   `json:"period"`
	Amount   float64  `json:"amount"` // per period incl. tax
	Currency string   `json:"currency"`
	NextDue  string   `json:"next_due,omitempty"`
	PaidTo   string   `json:"paid_through,omitempty"`
	Domain   string   `json:"domain,omitempty"`
	Items    []string `json:"items"`
	Notice   string   `json:"notice,omitempty"`
}

// PortalInvoice is one invoice on the page.
type PortalInvoice struct {
	Number   string  `json:"number"`
	Service  string  `json:"service"`
	Issued   string  `json:"issued"`
	Due      string  `json:"due"`
	Total    float64 `json:"total"`
	Balance  float64 `json:"balance"`
	Currency string  `json:"currency"`
	Status   string  `json:"status"` // due | overdue | paid
	Token    string  `json:"token"`  // the invoice's public token: /i/<token>, /pay/<token>
	Payable  bool    `json:"payable"`
	PaidOn   string  `json:"paid_on,omitempty"`
}

// PortalView is the whole page.
type PortalView struct {
	Business      map[string]string  `json:"business"`
	Customer      string             `json:"customer"`
	Today         string             `json:"today"`
	Subscriptions []PortalSub        `json:"subscriptions"`
	Open          []PortalInvoice    `json:"open"`
	Paid          []PortalInvoice    `json:"paid"`
	TotalDue      map[string]float64 `json:"total_due"`
	OnlinePay     bool               `json:"online_pay"`
}

// Portal builds the page for a token.
func (s *Service) Portal(tok string) (*PortalView, error) {
	org, cid, ok := s.ParsePortalToken(tok)
	if !ok {
		return nil, ErrNotFound
	}
	cu, ok := s.customer(org, cid)
	if !ok {
		return nil, ErrNotFound
	}
	info := s.orgInfo(org)
	today := s.Today(org)
	v := &PortalView{Business: map[string]string{"name": info.Name, "email": info.Email, "phone": info.Phone, "logo": info.Logo, "accent": info.Accent},
		Customer: cu.Name, Today: DayStr(today), Subscriptions: []PortalSub{}, Open: []PortalInvoice{}, Paid: []PortalInvoice{}, TotalDue: map[string]float64{}}
	v.OnlinePay = invoicing.OnlinePay != nil && invoicing.OnlinePay(s.DB, org)
	subs := s.List(org, Filter{CustomerID: cid})
	names := map[string]string{}
	for _, x := range subs {
		names[x.ID] = x.Name
		if x.Status == StatusCancelled && (x.CancelledAt == nil || DaysBetween(Day(*x.CancelledAt), today) > 90) {
			continue
		}
		ps := PortalSub{Number: x.Number, Name: x.Name, Status: x.Status, Period: PeriodLabel(x.Period), Amount: x.AmountTax, Currency: x.Currency,
			NextDue: x.NextDueDate, Domain: strings.Join(x.AllDomains(), ", "), Items: []string{}}
		if x.PaidThrough != nil {
			ps.PaidTo = DayStr(*x.PaidThrough)
		}
		if x.Open != nil {
			ps.NextDue = x.Open.DueDate
		}
		for _, l := range x.Lines {
			it := l.Description
			if l.Quantity != 1 {
				it = trimQty(l.Quantity) + " × " + it
			}
			ps.Items = append(ps.Items, it)
		}
		switch x.Status {
		case StatusSuspended:
			ps.Notice = "Suspended — pay the overdue invoice to restore it."
		case StatusPastDue:
			ps.Notice = "Overdue — please pay the open invoice."
			if x.Open != nil && x.Open.SuspendOn != "" {
				if d, ok := ParseDay(x.Open.SuspendOn); ok {
					ps.Notice = "Overdue — pay before " + longDate(d) + " to avoid suspension."
				}
			}
		case StatusCancelled:
			ps.Notice = "Cancelled."
		}
		if x.EndDate != nil && x.Status != StatusCancelled {
			ps.Notice = strings.TrimSpace(ps.Notice + " Ends on " + DayStr(*x.EndDate) + ".")
		}
		v.Subscriptions = append(v.Subscriptions, ps)
	}
	var rows []struct {
		SubscriptionID, Number, Currency, PublicToken, Status string
		IssueDate, DueDate                                    time.Time
		Total, BalanceDue                                     float64
		PaidAt                                                *time.Time
	}
	s.DB.Raw(`SELECT p.subscription_id, i.number, i.currency, i.public_token, i.status, i.issue_date, i.due_date, i.total, i.balance_due, i.paid_at
		FROM client_subscription_periods p JOIN billing_invoices i ON i.id = p.invoice_id
		WHERE p.organization_id = ? AND p.subscription_id IN (SELECT id FROM client_subscriptions WHERE organization_id = ? AND customer_id = ?)
		AND i.deleted_at IS NULL AND i.status <> 'void' AND (p.sent OR i.status <> 'draft') ORDER BY i.due_date DESC LIMIT 60`, org, org, cid).Scan(&rows)
	for _, r := range rows {
		pi := PortalInvoice{Number: r.Number, Service: names[r.SubscriptionID], Issued: DayStr(r.IssueDate), Due: DayStr(r.DueDate), Total: r.Total, Balance: r.BalanceDue,
			Currency: r.Currency, Token: r.PublicToken}
		if r.BalanceDue > 0.004 {
			pi.Status = "due"
			if Day(r.DueDate).Before(today) {
				pi.Status = "overdue"
			}
			pi.Payable = v.OnlinePay
			v.Open = append(v.Open, pi)
			v.TotalDue[r.Currency] = RoundCur(v.TotalDue[r.Currency]+r.BalanceDue, r.Currency)
		} else if len(v.Paid) < 12 {
			pi.Status = "paid"
			if r.PaidAt != nil {
				pi.PaidOn = DayStr(*r.PaidAt)
			}
			v.Paid = append(v.Paid, pi)
		}
	}
	// Oldest unpaid first.
	for i, j := 0, len(v.Open)-1; i < j; i, j = i+1, j-1 {
		v.Open[i], v.Open[j] = v.Open[j], v.Open[i]
	}
	return v, nil
}

func trimQty(q float64) string { return strconv.FormatFloat(q, 'f', -1, 64) }
