package invoicing

import (
	"strings"
	"testing"
)

func TestEmailHTMLLogo(t *testing.T) {
	logo := "https://api.example.com/api/v1/public/org-logo/org-1?v=abc123"
	h := EmailHTML("Acme & Co", logo, "#2563eb", "Hi <Sam>,\n\nView it online: https://app.example.com/i/tok123.\n\nThanks")
	img := strings.Index(h, `<img src="`+logo+`"`)
	if img < 0 {
		t.Fatalf("logo image missing: %s", h)
	}
	if strings.Index(h, "Hi &lt;Sam&gt;") < img {
		t.Fatal("logo must come before the message, and the message must be escaped")
	}
	if !strings.Contains(h, `<a href="https://app.example.com/i/tok123"`) || !strings.Contains(h, `alt="Acme &amp; Co"`) {
		t.Fatalf("links/alt: %s", h)
	}
	// no logo (or a relative URL mail clients can't load): business name in bold
	for _, u := range []string{"", "/api/v1/public/org-logo/x?v=1"} {
		h = EmailHTML("Acme", u, "red;}</style>", "Hello")
		if strings.Contains(h, "<img") || !strings.Contains(h, "color:#1a1d2e\">Acme</div>") || strings.Contains(h, "red;}") || strings.Count(h, "</style>") != 1 {
			t.Fatalf("fallback header wrong for %q: %s", u, h)
		}
	}
}

func TestBuildHTMLMessage(t *testing.T) {
	long := strings.Repeat("x", 3000)
	msg := string(BuildHTMLMessage("Shop <shop@example.com>", "c@example.com", []string{"cc@example.com"}, "Invoice INV-1", "text "+long, "<p>"+long+"</p>"))
	for _, want := range []string{"Cc: cc@example.com", "Subject: Invoice INV-1", "multipart/alternative; boundary=", "Content-Type: text/plain", "Content-Type: text/html", "quoted-printable"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in\n%s", want, msg[:400])
		}
	}
	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line longer than 998 chars (%d)", len(line))
		}
	}
}

// Secondary button groups ("Add to calendar"): each button is its own
// left-aligned table so a row wraps on a phone, Outlook gets ghost cells, the
// plain-text part lists the links, and only http(s) links are rendered.
func TestEmailActionGroups(t *testing.T) {
	v := EmailView{Business: EmailBusiness{Name: "Acme"}, Message: "Hi", Actions: []EmailAction{{Label: "Open booking", URL: "https://app.example/b/1", Primary: true}},
		Groups: []EmailActionGroup{{Label: "Add to your calendar", Actions: []EmailAction{
			{Label: "Google Calendar", URL: "https://calendar.google.com/calendar/render?action=TEMPLATE&text=A%20%26%20B"},
			{Label: "Outlook", URL: "https://outlook.office.com/calendar/0/deeplink/compose?subject=A"},
			{Label: "Bad", URL: "javascript:alert(1)"}, {Label: "Empty", URL: ""}},
			Links: []EmailAction{{Label: "Outlook.com <personal>", URL: "https://outlook.live.com/x"}}}}}
	out, text := RenderEmail(v)
	for _, w := range []string{"Add to your calendar</div>", `<table role="presentation" align="left"`,
		`href="https://calendar.google.com/calendar/render?action=TEMPLATE&amp;text=A%20%26%20B"`, ">Google Calendar</a>", ">Outlook</a>",
		`<!--[if mso]><td valign="top"><![endif]-->`, "<v:roundrect", `>Outlook.com &lt;personal&gt;</a>`} {
		if !strings.Contains(out, w) {
			t.Fatalf("html lacks %q", w)
		}
	}
	if strings.Contains(out, "javascript:") || strings.Contains(out, ">Bad<") || strings.Contains(out, ">Empty<") {
		t.Fatal("invalid links were rendered")
	}
	if strings.Count(out, `align="left"`) != 2 {
		t.Fatalf("want two wrapping buttons, got %d", strings.Count(out, `align="left"`))
	}
	// The group comes after the main buttons.
	if strings.Index(out, ">Open booking</a>") > strings.Index(out, "Add to your calendar</div>") {
		t.Fatal("group is not below the main actions")
	}
	want := "Open booking: https://app.example/b/1\n\nAdd to your calendar\nGoogle Calendar: https://calendar.google.com/calendar/render?action=TEMPLATE&text=A%20%26%20B\n" +
		"Outlook: https://outlook.office.com/calendar/0/deeplink/compose?subject=A\nOutlook.com <personal>: https://outlook.live.com/x\n"
	if !strings.Contains(text, want) {
		t.Fatalf("text part:\n%s", text)
	}
	// A group without any valid link renders nothing.
	out2, text2 := RenderEmail(EmailView{Business: EmailBusiness{Name: "Acme"}, Message: "Hi", Groups: []EmailActionGroup{{Label: "Nothing here", Actions: []EmailAction{{Label: "x", URL: "ftp://x"}}}}})
	if strings.Contains(out2, "Nothing here") || strings.Contains(text2, "Nothing here") {
		t.Fatal("empty group rendered")
	}
}
