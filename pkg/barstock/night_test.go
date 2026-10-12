package barstock

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (f *fakeMailer) SendAs(_ context.Context, _ string, m mail.Message) (mail.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return mail.Config{}, nil
}

func TestDBNightReportScheduler(t *testing.T) {
	e := newTestEnv(t)
	_, bar, _, items := e.setup()
	fm := &fakeMailer{}
	e.s.Mail = fm
	e.s.AppURL = "https://app.test"
	jam := items["spirit_jameson_750"]
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{LocationID: bar, Lines: []dl{{jam, 1, "unit", ptr(2400.0), 0}}})
	e.must(err)
	e.at("2026-10-09 23:00")
	_, _, err = e.sale(bar, map[string]int{e.recipeOf("tot", jam): 4}, 300)
	e.must(err)
	// Off by default: nothing is sent.
	e.at("2026-10-10 07:30")
	if n := e.s.RunNightReports(context.Background()); n != 0 || len(fm.sent) != 0 {
		t.Fatalf("sent while off: %d", n)
	}
	_, err = e.s.applySettings(e.db, torg, "u-mgr", settingsInput{NightReport: &struct {
		Enabled *bool   `json:"enabled"`
		SendAt  *string `json:"send_at"`
	}{Enabled: ptr(true), SendAt: ptr("07:00")}})
	e.must(err)
	// Before the send time: not due.
	e.at("2026-10-10 06:30")
	if n := e.s.RunNightReports(context.Background()); n != 0 {
		t.Fatalf("sent before send_at: %d", n)
	}
	// After it: Friday's report, once, to the managers only.
	e.at("2026-10-10 07:30")
	if n := e.s.RunNightReports(context.Background()); n != 1 || len(fm.sent) != 1 {
		t.Fatalf("first run sent %d (%d mails)", n, len(fm.sent))
	}
	m := fm.sent[0]
	if !strings.Contains(m.Subject, "Fri 9 Oct 2026") || len(m.To) != 1 || m.To[0] != "mgr@club.test" {
		t.Errorf("mail = %q to %v", m.Subject, m.To)
	}
	if !strings.Contains(m.Text, "Spirits") || !strings.Contains(m.HTML, "https://app.test/bar-stock?tab=overview") {
		t.Errorf("mail body lacks sales by category / link: %s", m.Text)
	}
	if n := e.s.RunNightReports(context.Background()); n != 0 || len(fm.sent) != 1 {
		t.Errorf("second run sent again: %d", n)
	}
	var rows []NightReport
	e.db.Find(&rows)
	if len(rows) != 1 || rows[0].BusinessDate != "2026-10-09" {
		t.Errorf("night report log = %+v", rows)
	}
	// Another instance holding the lock: this one skips.
	got := e.s.withLock(context.Background(), "barstock:night:"+torg, func() {
		if e.s.withLock(context.Background(), "barstock:night:"+torg, func() { t.Error("lock taken twice") }) {
			t.Error("second holder got the lock")
		}
	})
	if !got {
		t.Error("lock not taken")
	}
	// Send time before the cutoff is refused.
	if _, err := e.s.applySettings(e.db, torg, "u-mgr", settingsInput{NightReport: &struct {
		Enabled *bool   `json:"enabled"`
		SendAt  *string `json:"send_at"`
	}{SendAt: ptr("05:00")}}); err == nil {
		t.Error("send_at before the cutoff accepted")
	}
}
