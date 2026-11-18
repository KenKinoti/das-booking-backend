package brief

import (
	"fmt"
	"html"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

const (
	emailFont   = `-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif`
	emailText   = "#1a1d2e"
	emailMuted  = "#5b6275"
	emailBorder = "#e4e7ee"
	emailAccent = "#4f46e5"
	bodyMarker  = "@@DASYIN_MORNING_BRIEF_BODY@@"
)

// link makes an app path absolute.
func (s *Service) link(path string) string {
	if path == "" || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	base := strings.TrimRight(strings.TrimSpace(s.AppURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

// BriefURL is the link to a brief in the app.
func (s *Service) BriefURL(date string) string { return s.link("/brief/" + date) }

// Subject is the email subject of a brief.
func Subject(b *Brief, test bool) string {
	s := "Morning brief · " + b.DateLabel + " — " + b.Headline
	if r := []rune(s); len(r) > 220 {
		s = strings.TrimSpace(string(r[:219])) + "…"
	}
	if test {
		s = "[Test] " + s
	}
	return s
}

// orgBlock is the sender block of the email.
func (s *Service) orgBlock(org string) (invoicing.EmailBusiness, string, string) {
	var o struct{ Name, Email, Phone string }
	s.DB.Raw(`SELECT COALESCE(name,'') AS name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone FROM organizations WHERE id = ?`, org).Scan(&o)
	accent := ""
	if hasTable(s.DB, "billing_settings") {
		var a []string
		s.DB.Raw(`SELECT COALESCE(accent_color,'') FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&a)
		if len(a) > 0 {
			accent = a[0]
		}
	}
	logo := ""
	if s.LogoURL != nil {
		logo = s.LogoURL(org)
	}
	return invoicing.EmailBusiness{Name: nz(o.Name, "DASYIN ERP"), Email: o.Email, Phone: o.Phone}, accent, logo
}

// Email renders a brief as the shared branded email (pkg/invoicing's
// template) with the sections in place of the message.
func (s *Service) Email(v *Viewer, b *Brief, test bool) mail.Message {
	biz, accent, logo := s.orgBlock(v.OrgID)
	if accent == "" {
		accent = emailAccent
	}
	subject := Subject(b, test)
	actions := []invoicing.EmailAction{}
	if u := s.BriefURL(b.Date); u != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open today's brief", URL: u, Primary: true})
	}
	note := "You get this because you switched on the morning brief in My profile → Morning brief — change the time, the channels or the sections there."
	if test {
		note = "Test send — only you received this. " + note
	}
	view := invoicing.EmailView{Kind: "reminder", Business: biz, LogoURL: logo, Accent: accent, Preheader: b.Headline,
		DocLabel: "Morning brief", Number: b.DateLabel, Actions: actions, Note: note}
	tv := view
	tv.Message = s.Text(b, false)
	_, text := invoicing.RenderEmail(tv)
	view.Message = bodyMarker
	htmlOut, _ := invoicing.RenderEmail(view)
	htmlOut = strings.Replace(htmlOut, bodyMarker, s.sectionsHTML(b, accent), 1)
	return mail.Message{To: []string{v.Email}, Subject: subject, HTML: htmlOut, Text: text}
}

func esc(s string) string { return html.EscapeString(s) }

func toneColor(t string) (string, string) {
	switch t {
	case "danger":
		return "#b42318", "#fdecec"
	case "warn":
		return "#a15c07", "#fdf3e2"
	case "ok":
		return "#0f7a45", "#e6f6ee"
	}
	return "#3f4a63", "#eef0f5"
}

// sectionsHTML is the body of the email: greeting, headline and sections.
func (s *Service) sectionsHTML(b *Brief, accent string) string {
	var w strings.Builder
	w.WriteString(`<div class="tx" style="font:600 16px/1.5 ` + emailFont + `;color:` + emailText + `;margin:0 0 6px">` + esc(b.Greeting) + `,</div>`)
	w.WriteString(`<div class="tx" style="font:15px/1.55 ` + emailFont + `;color:` + emailText + `;margin:0 0 14px">Here is what needs your attention today.</div>`)
	w.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="summary" style="background:#f8f9fc;border:1px solid ` + emailBorder + `;border-left:4px solid ` + accent + `;border-radius:10px"><tr><td style="padding:14px 16px;font:700 16px/1.45 ` + emailFont + `;color:` + emailText + `" class="tx">` + esc(b.Headline) + `</td></tr></table>`)
	for _, sec := range b.Sections {
		w.WriteString(`<div class="tx" style="font:700 13px/1.4 ` + emailFont + `;letter-spacing:.06em;text-transform:uppercase;color:` + emailText + `;margin:24px 0 2px">`)
		if u := s.link(sec.Link); u != "" {
			w.WriteString(`<a href="` + esc(u) + `" style="color:` + emailText + `;text-decoration:none">` + esc(sec.Title) + `</a>`)
		} else {
			w.WriteString(esc(sec.Title))
		}
		w.WriteString(`</div>`)
		if sec.Summary != "" {
			w.WriteString(`<div class="mt" style="font:13px/1.5 ` + emailFont + `;color:` + emailMuted + `;margin:0 0 6px">` + esc(sec.Summary) + `</div>`)
		}
		for _, n := range sec.Notes {
			w.WriteString(`<div style="font:13px/1.5 ` + emailFont + `;color:#a15c07;background:#fdf3e2;border-radius:8px;padding:8px 10px;margin:6px 0">` + esc(n) + `</div>`)
		}
		for _, g := range sec.Groups {
			w.WriteString(`<div class="mt" style="font:600 12px/1.4 ` + emailFont + `;color:` + emailMuted + `;margin:12px 0 4px">` + esc(g.Title) + `</div>`)
			w.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">`)
			for _, it := range g.Items {
				w.WriteString(`<tr><td class="rule" style="padding:8px 10px 8px 0;border-bottom:1px solid ` + emailBorder + `;font:14px/1.45 ` + emailFont + `;vertical-align:top">`)
				title := esc(it.Title)
				if u := s.link(it.Link); u != "" {
					title = `<a href="` + esc(u) + `" style="color:` + emailText + `;text-decoration:none;font-weight:600" class="tx">` + title + `</a>`
				} else {
					title = `<span class="tx" style="color:` + emailText + `;font-weight:600">` + title + `</span>`
				}
				w.WriteString(title)
				if it.Tag != "" {
					fg, bg := toneColor(it.Tone)
					w.WriteString(` <span style="display:inline-block;font:600 11px/1.6 ` + emailFont + `;color:` + fg + `;background:` + bg + `;border-radius:999px;padding:0 8px;white-space:nowrap">` + esc(it.Tag) + `</span>`)
				}
				if it.Detail != "" || it.External != "" {
					w.WriteString(`<br><span class="mt" style="font-size:13px;color:` + emailMuted + `">` + esc(it.Detail))
					if it.External != "" {
						if it.Detail != "" {
							w.WriteString(` · `)
						}
						w.WriteString(`<a href="` + esc(it.External) + `" style="color:` + accent + `">` + esc(externalLabel(it.External)) + `</a>`)
					}
					w.WriteString(`</span>`)
				}
				w.WriteString(`</td><td class="tx rule" align="right" style="padding:8px 0;border-bottom:1px solid ` + emailBorder + `;font:600 14px/1.45 ` + emailFont + `;color:` + emailText + `;text-align:right;white-space:nowrap;vertical-align:top;font-variant-numeric:tabular-nums">` + esc(it.Value) + `</td></tr>`)
			}
			if g.More > 0 {
				more := fmt.Sprintf("and %d more", g.More)
				if u := s.link(g.Link); u != "" {
					more = `<a href="` + esc(u) + `" style="color:` + accent + `">` + more + `</a>`
				}
				w.WriteString(`<tr><td colspan="2" class="mt" style="padding:6px 0 0;font:13px/1.4 ` + emailFont + `;color:` + emailMuted + `">` + more + `</td></tr>`)
			}
			w.WriteString(`</table>`)
		}
	}
	return w.String()
}

func externalLabel(u string) string {
	if strings.Contains(u, "atlassian") || strings.Contains(u, "/browse/") {
		return "Open in Jira"
	}
	return "Open"
}

// Text is the plain-text brief (the email's text part; the WhatsApp text
// message when whatsapp is true, with *bold* titles and fewer lines).
func (s *Service) Text(b *Brief, whatsapp bool) string {
	var w strings.Builder
	bold := func(t string) string {
		if whatsapp {
			return "*" + t + "*"
		}
		return strings.ToUpper(t)
	}
	if whatsapp {
		w.WriteString(bold(b.Greeting) + " — your brief for " + b.DateLabel + "\n\n")
	} else {
		w.WriteString(b.Greeting + ",\n\nHere is what needs your attention today.\n\n")
	}
	w.WriteString(b.Headline + "\n")
	perGroup := 8
	if whatsapp {
		perGroup = 3
	}
	for _, sec := range b.Sections {
		w.WriteString("\n" + bold(sec.Title) + "\n")
		if sec.Summary != "" && !whatsapp {
			w.WriteString(sec.Summary + "\n")
		}
		for _, n := range sec.Notes {
			w.WriteString("⚠ " + n + "\n")
		}
		for _, g := range sec.Groups {
			if !whatsapp {
				w.WriteString(g.Title + ":\n")
			} else {
				w.WriteString("_" + g.Title + "_\n")
			}
			for i, it := range g.Items {
				if i >= perGroup {
					break
				}
				line := "• " + it.Title
				if it.Value != "" {
					line += " — " + it.Value
				}
				if it.Detail != "" {
					line += " (" + it.Detail + ")"
				}
				w.WriteString(line + "\n")
				if !whatsapp {
					if u := s.link(it.Link); u != "" {
						w.WriteString("  " + u + "\n")
					}
				}
			}
			if more := g.More + max(0, len(g.Items)-perGroup); more > 0 {
				w.WriteString(fmt.Sprintf("  …and %d more\n", more))
			}
		}
	}
	if u := s.BriefURL(b.Date); u != "" {
		w.WriteString("\nOpen the full brief: " + u + "\n")
	}
	out := w.String()
	if whatsapp {
		if r := []rune(out); len(r) > 4000 {
			out = string(r[:3900]) + "…\n\nOpen the full brief: " + s.BriefURL(b.Date)
		}
	}
	return out
}
