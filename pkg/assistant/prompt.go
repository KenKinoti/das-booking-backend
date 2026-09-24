package assistant

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

const rules = `You are DASYIN Assistant ("Ask DASYIN"), built into the DASYIN ERP web app. You help the signed-in user look up and update their business records using the tools provided.

Rules:
- Use tools to answer questions about the user's data. Never guess or invent records, numbers, totals, dates or IDs; if a tool does not give you the answer, say so.
- Before changing anything, make sure you have the right record. If a name matches more than one record (e.g. two customers called Sam) or a request is ambiguous, ask a short clarifying question listing the options instead of picking one.
- Tools that change data are shown to the user for confirmation before they run; just call the tool — do not ask "shall I proceed?" in text first. If the user declines, accept it and ask what they would like instead.
- Keep answers short and scannable: a sentence or two, or a compact list/table. No filler, no repeating the question.
- Format money with its currency (e.g. AUD 1,250.00 or $1,250.00 AUD). Use the organisation's default currency when a record has none.
- Dates: interpret "today", "this week", "overdue" etc. in the user's time zone below.
- When a tool result includes a link/url to a record, link it in Markdown, e.g. [INV-0042](/invoicing/invoices/123). Only use links that came from tool results or are plain app paths you are sure of.
- Use Markdown sparingly (bold, lists, small tables). Never output raw HTML.
- You can only act within this organisation and with this user's permissions. If a tool returns a permission or plan error, explain it plainly.`

// systemPrompt builds the system prompt blocks for a request.
func (s *Service) systemPrompt(who aitools.Identity, page Page) []any {
	loc := time.UTC
	if who.TZ != "" {
		if l, err := time.LoadLocation(who.TZ); err == nil {
			loc = l
		}
	}
	now := s.now().In(loc)
	name, orgName, cur := s.profile(who)
	var b strings.Builder
	b.WriteString("Context:\n")
	fmt.Fprintf(&b, "- User: %s", strings.TrimSpace(name))
	if who.Email != "" {
		fmt.Fprintf(&b, " <%s>", who.Email)
	}
	if who.Role != "" {
		fmt.Fprintf(&b, ", role %s", who.Role)
	}
	b.WriteString("\n")
	if orgName != "" {
		fmt.Fprintf(&b, "- Organisation: %s\n", orgName)
	}
	fmt.Fprintf(&b, "- Now: %s (%s), time zone %s\n", now.Format("Monday 2 January 2006, 15:04"), now.Format("2006-01-02"), loc.String())
	fmt.Fprintf(&b, "- Default currency: %s\n", cur)
	if pc := pageContext(page); pc != "" {
		fmt.Fprintf(&b, "- The user is currently viewing: %s. \"This\", \"it\" or \"here\" probably refer to that page/record.\n", pc)
	}
	return []any{
		map[string]any{"type": "text", "text": rules},
		map[string]any{"type": "text", "text": b.String()},
	}
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), n)
}

func pageContext(p Page) string {
	if strings.TrimSpace(p.Path) == "" {
		return ""
	}
	parts := []string{"route " + clip(p.Path, 200)}
	if p.Name != "" {
		parts = append(parts, "page \""+clip(p.Name, 60)+"\"")
	}
	if p.Title != "" {
		parts = append(parts, "title \""+clip(p.Title, 100)+"\"")
	}
	var kv []string
	for k, v := range p.Params {
		if v != "" {
			kv = append(kv, clip(k, 40)+"="+clip(v, 80))
		}
	}
	sort.Strings(kv)
	if len(kv) > 0 {
		parts = append(parts, "record "+strings.Join(kv, ", "))
	}
	return strings.Join(parts, ", ")
}

// profile looks up the user's name, organisation name and default currency.
func (s *Service) profile(who aitools.Identity) (name, org, cur string) {
	name = who.Email
	if s.DB == nil {
		return name, "", "AUD"
	}
	var u struct{ FirstName, LastName string }
	if s.DB.Raw("SELECT first_name, last_name FROM users WHERE id = ? LIMIT 1", who.UserID).Scan(&u).Error == nil {
		if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
			name = n
		}
	}
	s.DB.Raw("SELECT name FROM organizations WHERE id = ? LIMIT 1", who.OrgID).Row().Scan(&org)
	s.DB.Raw("SELECT UPPER(currency) FROM billing_settings WHERE organization_id = ? AND currency <> '' LIMIT 1", who.OrgID).Row().Scan(&cur)
	if !currency.IsSupported(cur) {
		cur = ""
		s.DB.Raw("SELECT UPPER(COALESCE(NULLIF(default_currency,''), currency)) FROM plan_subscriptions WHERE organization_id = ? LIMIT 1", who.OrgID).Row().Scan(&cur)
	}
	if !currency.IsSupported(cur) {
		cur = "AUD"
	}
	return name, org, cur
}
