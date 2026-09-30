// Package hooks wires notifications into modules through their extension
// points (gorm callbacks, exported hook variables) so those modules don't
// have to know about notifications.
package hooks

import (
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/payments"
)

type docInfo struct {
	ID, DocType, Number, ClientName, Currency string
	Total, BalanceDue                         float64
}

func loadDoc(db *gorm.DB, id string) (docInfo, bool) {
	var d docInfo
	err := db.Session(&gorm.Session{NewDB: true}).Table("billing_invoices").
		Select("id, doc_type, number, client_name, currency, total, balance_due").Where("id = ?", id).Take(&d).Error
	return d, err == nil
}

func (d docInfo) label() string {
	kind := "Invoice"
	if d.DocType == "quote" {
		kind = "Quote"
	}
	if d.Number != "" {
		return kind + " " + d.Number
	}
	return kind
}

func (d docInfo) link() string {
	if d.DocType == "quote" {
		return "/quotes/" + d.ID
	}
	return "/invoices/" + d.ID
}

func (d docInfo) entity() string {
	if d.DocType == "quote" {
		return "quote"
	}
	return "invoice"
}

// billingEvent maps an invoice activity type to a notification.
func billingEvent(a *invoicing.Activity, d docInfo) (notify.Message, bool) {
	who := d.ClientName
	if who == "" {
		who = "The client"
	}
	m := notify.Message{Link: d.link(), EntityType: d.entity(), EntityID: d.ID, ActorID: a.UserID}
	switch a.Type {
	case "viewed":
		m.Type, m.Severity = d.entity()+".viewed", notify.Info
		m.Title = fmt.Sprintf("%s viewed %s", who, d.label())
		m.Body = "Opened from the link you sent."
	case "accepted":
		m.Type, m.Severity = "quote.accepted", notify.Success
		m.Title = fmt.Sprintf("%s accepted %s", who, d.label())
		m.Body = a.Message
	case "declined":
		m.Type, m.Severity = "quote.declined", notify.Warning
		m.Title = fmt.Sprintf("%s declined %s", who, d.label())
		m.Body = a.Message
	case "payment":
		m.Type, m.Severity = "invoice.payment", notify.Success
		m.Title = "Payment recorded on " + d.label()
		m.Body = a.Message
		if d.BalanceDue <= 0.004 {
			m.Title = d.label() + " is paid in full"
		}
	case "payment_removed":
		m.Type, m.Severity = "invoice.payment_removed", notify.Warning
		m.Title = "Payment removed from " + d.label()
		m.Body = a.Message
	case "voided":
		m.Type, m.Severity = d.entity()+".voided", notify.Warning
		m.Title = d.label() + " was voided"
		m.Body = a.Message
	case "converted":
		m.Type, m.Severity = "quote.converted", notify.Success
		m.Title = d.label() + " converted to an invoice"
		m.Body = a.Message
	default:
		return m, false
	}
	return m, true
}

// RegisterBilling notifies admins about invoice/quote activity (client
// views, quote decisions, payments, voids) and online payment outcomes.
// It observes rows written to billing_activities with a gorm callback so the
// invoicing package stays untouched.
func RegisterBilling(db *gorm.DB) error {
	payments.OnTransaction = func(db *gorm.DB, t *payments.Transaction, recorded bool) {
		d, ok := loadDoc(db, t.InvoiceID)
		if !ok {
			return
		}
		m := notify.Message{Link: d.link(), EntityType: "invoice", EntityID: d.ID}
		amt := invoicing.FormatMoney(t.Amount, t.Currency)
		method := t.MethodLabel
		if method == "" {
			method = "online"
		}
		if recorded {
			m.Type, m.Severity = "payment.received", notify.Success
			m.Title = fmt.Sprintf("Payment received · %s", amt)
			m.Body = fmt.Sprintf("%s paid %s online (%s).", firstNonEmpty(d.ClientName, "A client"), d.label(), method)
			if d.BalanceDue <= 0.004 {
				m.Body += " The invoice is now paid in full."
			}
		} else {
			m.Type, m.Severity = "payment.failed", notify.Error
			m.Title = fmt.Sprintf("Online payment failed · %s", amt)
			m.Body = fmt.Sprintf("%s tried to pay %s (%s): %s", firstNonEmpty(d.ClientName, "A client"), d.label(), method, firstNonEmpty(t.Error, "declined"))
		}
		notify.Go(db, t.OrganizationID, notify.Admins(), m)
	}

	return db.Callback().Create().After("gorm:create").Register("notify:billing_activity", func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement == nil || tx.Statement.Table != "billing_activities" {
			return
		}
		a, ok := tx.Statement.Dest.(*invoicing.Activity)
		if !ok || a == nil || a.OrganizationID == "" {
			return
		}
		if strings.HasPrefix(a.Type, "online_payment") {
			return // payments.OnTransaction covers these
		}
		d, found := loadDoc(tx, a.InvoiceID)
		if !found {
			return
		}
		m, send := billingEvent(a, d)
		if !send {
			return
		}
		SendInTx(tx, a.OrganizationID, notify.Admins().Except(a.UserID), m)
	})
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// SendInTx sends inside the caller's transaction when there is one (so the
// notification is rolled back with it) behind a savepoint, so a failure here
// can never abort the caller's transaction.
func SendInTx(tx *gorm.DB, org string, to notify.Target, m notify.Message) {
	db := tx.Session(&gorm.Session{NewDB: true})
	if _, inTx := tx.Statement.ConnPool.(gorm.TxCommitter); !inTx {
		notify.Go(db, org, to, m)
		return
	}
	const sp = "notify_sp"
	if err := db.SavePoint(sp).Error; err != nil {
		return
	}
	if _, err := notify.Send(db, org, to, m); err != nil {
		db.RollbackTo(sp)
	}
}
