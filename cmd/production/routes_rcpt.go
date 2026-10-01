package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/payments"
)

// Payment receipts (v2.6.0):
//
//   - every invoice payment has a receipt number (Invoice settings → Receipt
//     prefix); existing payments are numbered once, in date order;
//   - after an online (Flutterwave) payment is verified, the receipt is
//     emailed to the client when Settings → Online payments → "Automatically
//     email a receipt" is on (default).
//
// Routes live in pkg/invoicing (PUT /invoicing/invoices/:id/payments/:paymentId …).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := invoicing.NewHandler(db)
		h.Mail = mail.Shared(db)
		h.PublicURL = os.Getenv("PUBLIC_APP_URL")
		payments.AfterRecorded = func(db *gorm.DB, t *payments.Transaction) {
			if !payments.AutoReceipt(db, t.OrganizationID) {
				return
			}
			org, inv, pay := t.OrganizationID, t.InvoiceID, t.InvoicePaymentID
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("receipt: auto email panic: %v", r)
					}
				}()
				if r := h.AutoReceipt(org, inv, pay); !r.Emailed {
					log.Printf("receipt: online payment %s not emailed: %s", pay, r.Reason)
				}
			}()
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if n := invoicing.BackfillReceiptNumbers(db); n > 0 {
			log.Printf("✅ receipt numbers given to %d existing payments", n)
		}
	})
}
