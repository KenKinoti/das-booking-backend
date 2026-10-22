package subscriptions

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// Domains never expire silently: the administrators are reminded 30, 14 and
// 7 days (Settings → domain reminders) before a domain linked to a live
// subscription expires, and once more if it does. The expiry comes from
// Synergy Wholesale's domain list (listDomains → domain_expiry, stored by
// Expenses → Sources → Synergy sync in expense_renewals) when available,
// else from the date typed on the subscription.

// DomainInfo is one linked domain's expiry.
type DomainInfo struct {
	Domain    string `json:"domain"`
	Expires   string `json:"expires,omitempty"`
	DaysLeft  *int   `json:"days_left,omitempty"`
	AutoRenew *bool  `json:"auto_renew,omitempty"`
	Status    string `json:"status,omitempty"`
	Source    string `json:"source"` // synergy | typed | ""
}

// DomainDue is a domain expiring soon, with its subscription.
type DomainDue struct {
	Domain         string `json:"domain"`
	Expires        string `json:"expires"`
	DaysLeft       int    `json:"days_left"`
	AutoRenew      bool   `json:"auto_renew"`
	Source         string `json:"source"`
	SubscriptionID string `json:"subscription_id"`
	Number         string `json:"number"`
	Customer       string `json:"customer"`
	PaidThrough    string `json:"paid_through,omitempty"`
}

type synDomain struct {
	Expires   time.Time
	AutoRenew bool
	Status    string
}

// synergyDomains are the domains Synergy Wholesale lists for the account.
func (s *Service) synergyDomains(org string) map[string]synDomain {
	out := map[string]synDomain{}
	if !hasTable(s.DB, "expense_renewals") {
		return out
	}
	var rows []struct {
		Name      string
		ExpiresOn *time.Time
		AutoRenew bool
		Status    string
	}
	s.DB.Raw(`SELECT name, expires_on, COALESCE(auto_renew, false) AS auto_renew, COALESCE(status, '') AS status FROM expense_renewals
		WHERE organization_id = ? AND source = ? AND kind = 'domain' AND COALESCE(active, true)`, org, expenses.SourceSynergy).Scan(&rows)
	for _, r := range rows {
		if r.ExpiresOn != nil {
			out[strings.ToLower(strings.TrimSpace(r.Name))] = synDomain{Expires: Day(*r.ExpiresOn), AutoRenew: r.AutoRenew, Status: r.Status}
		}
	}
	return out
}

// watched are the subscription's domains whose expiry is known.
func (s *Service) watched(sub *Subscription, syn map[string]synDomain, today time.Time) []DomainInfo {
	out := []DomainInfo{}
	typedFor := firstNonEmpty(sub.Domain, sub.SiteDomain)
	for _, d := range sub.AllDomains() {
		di := DomainInfo{Domain: d}
		if x, ok := syn[strings.ToLower(d)]; ok {
			n := DaysBetween(today, x.Expires)
			ar := x.AutoRenew
			di.Expires, di.DaysLeft, di.AutoRenew, di.Status, di.Source = DayStr(x.Expires), &n, &ar, x.Status, "synergy"
		} else if d == typedFor && sub.DomainExpiry != nil {
			n := DaysBetween(today, Day(*sub.DomainExpiry))
			di.Expires, di.DaysLeft, di.Source = DayStr(Day(*sub.DomainExpiry)), &n, "typed"
		}
		out = append(out, di)
	}
	return out
}

// domainInfo is the drawer's list of linked domains with their expiry.
func (s *Service) domainInfo(org string, sub *Subscription, today time.Time) []DomainInfo {
	return s.watched(sub, s.synergyDomains(org), today)
}

// DomainsDue are the linked domains of live subscriptions expiring within
// `within` days (or expired in the last 30).
func (s *Service) DomainsDue(org string, subs []Subscription, today time.Time, within int) []DomainDue {
	out := []DomainDue{}
	syn := s.synergyDomains(org)
	seen := map[string]bool{}
	for i := range subs {
		sub := &subs[i]
		if sub.Status == StatusCancelled {
			continue
		}
		for _, di := range s.watched(sub, syn, today) {
			if di.DaysLeft == nil || *di.DaysLeft > within || *di.DaysLeft < -30 || seen[di.Domain] {
				continue
			}
			seen[di.Domain] = true
			dd := DomainDue{Domain: di.Domain, Expires: di.Expires, DaysLeft: *di.DaysLeft, Source: di.Source, SubscriptionID: sub.ID, Number: sub.Number, Customer: sub.CustomerName}
			if di.AutoRenew != nil {
				dd.AutoRenew = *di.AutoRenew
			}
			if sub.PaidThrough != nil {
				dd.PaidThrough = DayStr(*sub.PaidThrough)
			}
			out = append(out, dd)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Expires < out[j].Expires })
	return out
}

// domainThreshold is the latest reminder reached for daysLeft (0 = none;
// −1 = expired).
func domainThreshold(days []int, daysLeft int) int {
	if daysLeft < 0 {
		return -1
	}
	best := 0
	for _, t := range days {
		if t > 0 && daysLeft <= t && (best == 0 || t < best) {
			best = t
		}
	}
	return best
}

// domainReminders sends the admins the domain renewal reminders that are due.
func (s *Service) domainReminders(ctx context.Context, org string, subs []Subscription, st Settings, today time.Time, res *RunResult) {
	if len(st.DomainDays) == 0 {
		return
	}
	syn := s.synergyDomains(org)
	seen := map[string]bool{}
	for i := range subs {
		sub := &subs[i]
		if sub.Status == StatusCancelled {
			continue
		}
		for _, di := range s.watched(sub, syn, today) {
			if di.DaysLeft == nil || seen[di.Domain] {
				continue
			}
			seen[di.Domain] = true
			left := *di.DaysLeft
			th := domainThreshold(st.DomainDays, left)
			if th == 0 || left < -30 {
				continue
			}
			stage := fmt.Sprintf("domain_%d", th)
			if th < 0 {
				stage = "domain_expired"
			}
			row, ok := s.claim(org, Event{Key: "domain|" + di.Domain + "|" + di.Expires + "|" + stage, SubscriptionID: sub.ID, Stage: stage, Audience: ToAdmin, Day: today})
			if !ok {
				continue
			}
			a := s.domainAlert(org, sub, di, left)
			to, inApp, err := s.alertAdmins(ctx, org, st, a)
			row.Subject, row.Recipients, row.InApp = a.Subject, strings.Join(to, ", "), inApp
			s.finish(row, "sent", err)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: domain reminder for %s: %v", sub.Number, di.Domain, err))
			} else {
				res.Domains++
				res.logf("%s: domain reminder for %s (%d days left)", sub.Number, di.Domain, left)
			}
		}
	}
}

func (s *Service) domainAlert(org string, sub *Subscription, di DomainInfo, left int) Alert {
	cu, _ := s.customer(org, sub.CustomerID)
	exp, _ := ParseDay(di.Expires)
	a := Alert{Type: "domain", Severity: notify.Warning, Link: subLink(sub.ID, ""), LinkText: "Open the subscription", Entity: sub.ID, Tone: "warn", Label: "Domain renewal"}
	client := firstNonEmpty(cu.Name, "the client")
	switch {
	case left < 0:
		a.Severity, a.Tone = notify.Error, "danger"
		a.Subject = fmt.Sprintf("%s expired on %s — renew it now (%s)", di.Domain, shortDate(exp), client)
		a.Status = "Expired"
	case left == 0:
		a.Severity, a.Tone = notify.Error, "danger"
		a.Subject = fmt.Sprintf("%s expires today — renew it (%s)", di.Domain, client)
		a.Status = "Expires today"
	default:
		a.Subject = fmt.Sprintf("%s expires in %d day%s (%s) — renew it for %s", di.Domain, left, plural(left), shortDate(exp), client)
		a.Status = fmt.Sprintf("Expires in %d day%s", left, plural(left))
		if left <= 7 {
			a.Tone = "danger"
		}
	}
	a.Lead = fmt.Sprintf("The domain %s (%s, %s) expires on %s.", di.Domain, client, sub.Number, longDate(exp))
	if di.AutoRenew != nil {
		if *di.AutoRenew {
			a.Lead += " Auto-renew is on at Synergy Wholesale, so it renews from your account balance — check there's enough credit."
		} else {
			a.Lead += " Auto-renew is OFF at Synergy Wholesale: it lapses unless you renew it."
		}
	}
	switch {
	case sub.Status == StatusSuspended:
		a.Body = "The client's subscription is suspended for an unpaid invoice — decide whether to renew before it lapses, and tell the client."
	case sub.PaidThrough != nil && !Day(*sub.PaidThrough).Before(exp):
		a.Body = fmt.Sprintf("The client has paid through %s, so renew it.", longDate(Day(*sub.PaidThrough)))
	default:
		a.Body = "Renew it at the registrar (Synergy Wholesale: Domains → " + di.Domain + " → Renew), or tell the client before it lapses if they are not renewing."
	}
	a.Facts = []invoicing.EmailRow{{Label: "Client", Value: client, Strong: true}, {Label: "Domain", Value: di.Domain}, {Label: "Expires", Value: longDate(exp), Strong: true},
		{Label: "Subscription", Value: sub.Number + " · " + sub.Name}}
	return a
}
