package insights

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Service lines of a web / IT services business. Invoice lines are classified
// by keywords (first matching line in priority order wins); the mapping is
// editable per organisation (insights_service_rules).
const (
	LineWorkspace   = "workspace"
	LineMaintenance = "maintenance"
	LineHosting     = "hosting"
	LineDomains     = "domains"
	LineWebDev      = "web_dev"
	LineSoftware    = "software"
	LineConsulting  = "consulting"
	LineOther       = "other"
)

// LineLabels are display names.
var LineLabels = map[string]string{
	LineWorkspace:   "Google Workspace & email",
	LineMaintenance: "Website maintenance & support",
	LineHosting:     "Hosting",
	LineDomains:     "Domains",
	LineWebDev:      "Web development",
	LineSoftware:    "Software & ERP",
	LineConsulting:  "Consulting & training",
	LineOther:       "Other",
}

// LineOrder is the display order.
var LineOrder = []string{LineWebDev, LineHosting, LineDomains, LineWorkspace, LineMaintenance, LineSoftware, LineConsulting, LineOther}

// ServiceRule maps keywords to a service line for one organisation.
type ServiceRule struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_ins_rule_line,priority:1"`
	Line           string    `json:"line" gorm:"type:varchar(32);not null;uniqueIndex:idx_ins_rule_line,priority:2"`
	Keywords       []string  `json:"keywords" gorm:"type:text;serializer:json"`
	Priority       int       `json:"priority"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (ServiceRule) TableName() string { return "insights_service_rules" }

// DefaultRules is the built-in mapping, in priority order.
func DefaultRules() []ServiceRule {
	mk := func(line string, p int, kw ...string) ServiceRule {
		return ServiceRule{Line: line, Priority: p, Keywords: kw}
	}
	return []ServiceRule{
		mk(LineWorkspace, 10, "google workspace", "g suite", "gsuite", "workspace", "gmail", "email hosting", "microsoft 365", "office 365", "m365", "mailbox", "email account"),
		mk(LineMaintenance, 20, "maintenance", "support plan", "retainer", "care plan", "website support", "monthly support", "backup", "updates"),
		mk(LineHosting, 30, "hosting", "cpanel", "vps", "server", "ssl", "bandwidth", "disk space", "synergy", "cloud"),
		mk(LineDomains, 40, "domain", "registration", "dns", "whois"),
		mk(LineWebDev, 50, "website", "web design", "web development", "wordpress", "landing page", "redesign", "e-commerce", "ecommerce", "theme", "plugin", "web app"),
		mk(LineSoftware, 60, "erp", "software", "licence", "license", "saas", "system", "crm", "integration", "app development", "mobile app"),
		mk(LineConsulting, 70, "consult", "advisory", "training", "strategy", "audit", "data analysis", "analytics", "workshop", "project management", "hours"),
	}
}

// Rules returns the organisation's mapping (defaults until edited).
func (s *Service) Rules(org string) []ServiceRule {
	var rows []ServiceRule
	s.DB.Where("organization_id = ?", org).Order("priority, line").Find(&rows)
	if len(rows) == 0 {
		return DefaultRules()
	}
	return rows
}

// SaveRules replaces the mapping. Lines not listed are dropped (so "other"
// catches them); keywords are trimmed, lower-cased and de-duplicated.
func (s *Service) SaveRules(org string, in []ServiceRule) ([]ServiceRule, error) {
	seen := map[string]bool{}
	out := []ServiceRule{}
	for i, r := range in {
		r.Line = strings.TrimSpace(r.Line)
		if _, ok := LineLabels[r.Line]; !ok || r.Line == LineOther {
			return nil, fmt.Errorf("unknown service line %q", r.Line)
		}
		if seen[r.Line] {
			return nil, fmt.Errorf("service line %q is listed twice", r.Line)
		}
		seen[r.Line] = true
		kw := []string{}
		dup := map[string]bool{}
		for _, k := range r.Keywords {
			k = strings.ToLower(strings.TrimSpace(k))
			if k == "" || dup[k] {
				continue
			}
			if len(k) > 60 {
				return nil, fmt.Errorf("keyword %q is too long", k)
			}
			dup[k] = true
			kw = append(kw, k)
		}
		if len(kw) > 60 {
			return nil, fmt.Errorf("too many keywords for %s", LineLabels[r.Line])
		}
		out = append(out, ServiceRule{ID: uuid.NewString(), OrganizationID: org, Line: r.Line, Keywords: kw, Priority: (i + 1) * 10, UpdatedAt: s.now()})
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("organization_id = ?", org).Delete(&ServiceRule{}).Error; err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		return tx.Create(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResetRules restores the defaults.
func (s *Service) ResetRules(org string) []ServiceRule {
	s.DB.Where("organization_id = ?", org).Delete(&ServiceRule{})
	return DefaultRules()
}

// Classify returns the service line of a description (Go version of LineSQL).
func Classify(rules []ServiceRule, desc string) string {
	d := strings.ToLower(desc)
	rs := append([]ServiceRule(nil), rules...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
	for _, r := range rs {
		for _, k := range r.Keywords {
			if k != "" && strings.Contains(d, k) {
				return r.Line
			}
		}
	}
	return LineOther
}

func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	return strings.ReplaceAll(s, "_", `\_`)
}

// LineSQL is a CASE expression classifying the SQL text expression expr.
func LineSQL(rules []ServiceRule, expr string) string {
	rs := append([]ServiceRule(nil), rules...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
	b := &strings.Builder{}
	b.WriteString("(CASE")
	n := 0
	for _, r := range rs {
		if _, ok := LineLabels[r.Line]; !ok || len(r.Keywords) == 0 {
			continue
		}
		pats := []string{}
		for _, k := range r.Keywords {
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
				pats = append(pats, sqlQuote("%"+likeEscape(k)+"%"))
			}
		}
		if len(pats) == 0 {
			continue
		}
		fmt.Fprintf(b, " WHEN LOWER(%s) LIKE ANY (ARRAY[%s]) THEN '%s'", expr, strings.Join(pats, ","), r.Line)
		n++
	}
	if n == 0 {
		return "'" + LineOther + "'"
	}
	b.WriteString(" ELSE 'other' END)")
	return b.String()
}

// costLine maps an expense category (pkg/expenses) to a service line.
func costLine(cat string) string {
	switch cat {
	case "hosting", "support", "prepaid":
		return LineHosting
	case "domains":
		return LineDomains
	case "email":
		return LineWorkspace
	case "software":
		return LineSoftware
	}
	return ""
}
