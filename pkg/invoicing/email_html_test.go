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
		if strings.Contains(h, "<img") || !strings.Contains(h, "font-weight:700;color:#111322\">Acme</div>") || strings.Contains(h, "</style>") {
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
