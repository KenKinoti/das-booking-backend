package main

import (
	"log"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Undo send and recall for invoice, quote, receipt and reminder emails
// (pkg/invoicing hold.go, recall.go, emailopts.go):
//
//   - Undo send: an email sent from the send dialog, the payment dialog's
//     receipt or the AI tools ("hold": true) is checked, then held for the
//     organisation's undo window (Invoice settings → Email: off / 10 / 30 /
//     60 / 120 s, default 30) and sent exactly once by a timer here — the
//     hold is stored, so a restart neither loses nor repeats it;
//
//   - Recall: switch off a sent email's links (per person, or every link to
//     the document), send a "please disregard" notice, then correct it.
//
//     GET|PUT /api/v1/invoicing/email-options                       {undo_seconds, attach_pdf}
//     POST    /api/v1/invoicing/invoices/:id/emails/:logId/undo      cancel a held email
//     POST    /api/v1/invoicing/invoices/:id/emails/:logId/send-now  send a held email at once
//     GET|POST /api/v1/invoicing/invoices/:id/emails/:logId/recall   preview / recall
//
// The routes are mounted by the invoicing handler (main.go); this file runs
// the hold engine. Env: INVOICE_HOLD_SWEEP (sweep interval, default 15s);
// INVOICE_HOLDS=off stops this server from sending held emails (another
// server must run it).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		if os.Getenv("INVOICE_HOLDS") == "off" {
			log.Printf("invoicing: held emails are not sent by this server (INVOICE_HOLDS=off)")
			return
		}
		h := invoicing.NewHandler(db)
		h.Mail = mail.Shared(db)
		h.PublicURL = os.Getenv("PUBLIC_APP_URL")
		sweep := 15 * time.Second
		if d, err := time.ParseDuration(os.Getenv("INVOICE_HOLD_SWEEP")); err == nil && d >= time.Second {
			sweep = d
		}
		// Start once the invoicing tables exist (migrations run in the background).
		go func() {
			for i := 0; ; i++ {
				if db.Migrator().HasTable("invoice_email_holds") {
					invoicing.StartHolds(h, sweep)
					return
				}
				time.Sleep(2 * time.Second)
				if i == 300 {
					log.Printf("invoicing: invoice_email_holds is still missing — held emails wait until the next start")
					return
				}
			}
		}()
	})
}
