package expenses

import (
	"strings"
	"time"
)

// Helpers used by pkg/recurbills (recurring supplier bills) so a detected
// subscription can become a recurring bill without being counted twice, and
// so the weekly bill digest can warn about the Synergy balance.

// SubscriptionsFor returns the subscriptions detected from approved
// expenses as of today.
func (s *Service) SubscriptionsFor(org string, today time.Time) []Subscription {
	charges, _ := s.approvedCharges(org, "")
	return DetectSubscriptions(charges, Day(today))
}

// SubscriptionTarget resolves (creating when missing) the finance vendor and
// expense account bills for a subscription are coded to — the same ones
// Approve uses, so a converted subscription and its imported bills line up.
func (s *Service) SubscriptionTarget(org string, sub Subscription) (financeVendorID, accountID string, err error) {
	cat := nonEmpty(sub.Category, CatOther)
	accountID, err = s.Fin.EnsureExpenseAccount(org, categoryCodes[cat], CategoryLabels[cat])
	if err != nil {
		return "", "", err
	}
	var v *Vendor
	if sub.VendorID != "" {
		v = s.vendorByID(org, sub.VendorID)
	}
	if v != nil && s.Fin.VendorExists(org, v.FinanceVendor) {
		return v.FinanceVendor, accountID, nil
	}
	email := ""
	if v != nil && len(v.Senders) > 0 && strings.Contains(v.Senders[0], "@") {
		email = v.Senders[0]
	}
	name := sub.Vendor
	if v != nil && name == "" {
		name = v.Name
	}
	financeVendorID, err = s.Fin.EnsureVendorNamed(org, name, email, accountID)
	if err != nil {
		return "", "", err
	}
	if v != nil {
		s.DB.Model(&Vendor{}).Where("id = ?", v.ID).Update("finance_vendor_id", financeVendorID)
	}
	return financeVendorID, accountID, nil
}

// FinanceVendorOf returns the finance vendor id an Expenses vendor's bills
// use ("" when it has none yet).
func (s *Service) FinanceVendorOf(org, expenseVendorID string) string {
	if v := s.vendorByID(org, expenseVendorID); v != nil && s.Fin.VendorExists(org, v.FinanceVendor) {
		return v.FinanceVendor
	}
	return ""
}

// BalanceAlerts returns the current (not dismissed) prepaid-balance alerts:
// low Synergy balance and balance runway.
func (s *Service) BalanceAlerts(org string, today time.Time) []Alert {
	d := s.DashboardFor(org, today, "", "")
	out := []Alert{}
	if d == nil {
		return out
	}
	for _, a := range d.Alerts {
		if a.Type == "low_balance" || a.Type == "synergy_runway" {
			out = append(out, a)
		}
	}
	return out
}

// RecurringBillLinks maps subscription keys to the recurring bills linked to
// them (finance_recurring_bills.subscription_key), for the Expenses page.
func (s *Service) RecurringBillLinks(org string) map[string]string {
	out := map[string]string{}
	var rows []struct{ ID, SubscriptionKey string }
	if err := s.DB.Raw(`SELECT id, subscription_key FROM finance_recurring_bills WHERE organization_id = ? AND subscription_key <> '' AND status <> 'ended'`, org).Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.SubscriptionKey] = r.ID
	}
	return out
}
