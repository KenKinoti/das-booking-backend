package schedsend

import (
	"regexp"
	"sort"
	"strings"
)

// MergeField is a {name} that messages may use.
type MergeField struct {
	Name    string `json:"name"`
	Help    string `json:"help"`
	Example string `json:"example"`
	Team    bool   `json:"team,omitempty"` // also available in team messages
}

// MergeFields are the fields customer, statement, reminder and team messages
// may use. Values are filled per recipient at send time.
var MergeFields = []MergeField{
	{"first_name", "First name", "Wanjiru", true},
	{"last_name", "Last name", "Kamau", false},
	{"name", "Full name", "Wanjiru Kamau", true},
	{"company", "Company (or full name)", "Savanna Bistro Ltd", false},
	{"email", "Email address", "wanjiru@example.com", false},
	{"phone", "Phone number", "+254 712 345 678", false},
	{"business", "Your business name", "DASYIN", true},
	{"balance", "Total still to pay on open invoices", "KSh 12,500.00", false},
	{"open_invoices", "Number of unpaid invoices", "2", false},
	{"statement", "List of unpaid invoices (number, due date, amount)", "INV-1043 · due 5 Nov 2026 · KSh 7,500.00", false},
	{"invoice_number", "The invoice (reminders: this invoice; otherwise the oldest unpaid one)", "INV-1043", false},
	{"amount_due", "Amount due on that invoice", "KSh 7,500.00", false},
	{"due_date", "Due date of that invoice", "5 Nov 2026", false},
	{"days_overdue", "Days that invoice is overdue", "13", false},
	{"pay_link", "Link to view and pay that invoice online", "https://…/pay/…", false},
	{"today", "Today's date", "18 Nov 2026", true},
	{"month", "This month", "November 2026", true},
}

var mergeRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_]+)\s*\}\}|\{\s*([a-zA-Z_]+)\s*\}`)

// Render fills {name} (or {{name}}) merge fields. Unknown names are left as
// written and returned so the composer can warn.
func Render(text string, vals map[string]string) (string, []string) {
	unknown := map[string]bool{}
	out := mergeRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := mergeRe.FindStringSubmatch(m)
		k := sub[1]
		if k == "" {
			k = sub[2]
		}
		if v, ok := vals[strings.ToLower(k)]; ok {
			return v
		}
		unknown[k] = true
		return m
	})
	list := make([]string, 0, len(unknown))
	for k := range unknown {
		list = append(list, k)
	}
	sort.Strings(list)
	return out, list
}

// FieldsUsed lists the known merge fields text uses.
func FieldsUsed(text string) []string {
	known := map[string]bool{}
	for _, f := range MergeFields {
		known[f.Name] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, sub := range mergeRe.FindAllStringSubmatch(text, -1) {
		k := sub[1]
		if k == "" {
			k = sub[2]
		}
		k = strings.ToLower(k)
		if known[k] && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// UnknownFields lists {names} in text that are not merge fields.
func UnknownFields(text string, team bool) []string {
	vals := map[string]string{}
	for _, f := range MergeFields {
		if !team || f.Team {
			vals[f.Name] = "x"
		}
	}
	_, unk := Render(text, vals)
	return unk
}
