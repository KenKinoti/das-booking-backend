package zoho

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// Undo deletes what job created, as long as it has not been changed here
// since: customers (with their contact people), documents (with lines,
// payments, history and attachments), payments added to documents the job
// did not create, and catalog items. Records that existed before (merged)
// are only unlinked. The next invoice/quote numbers go back to what they
// were if nobody changed them since.
func Undo(db *gorm.DB, j *Job, now time.Time) (*UndoResult, error) {
	res := &UndoResult{At: now.UTC().Format(time.RFC3339), Deleted: map[string]int{}, Kept: map[string]int{}, KeptWhy: []string{}}
	var refs []ExternalRef
	if err := db.Where("organization_id = ? AND job_id = ?", j.OrganizationID, j.ID).Find(&refs).Error; err != nil {
		return nil, err
	}
	org := j.OrganizationID
	edited := func(table, id string, r ExternalRef) bool {
		var row struct{ UpdatedAt time.Time }
		if db.Table(table).Select("updated_at").Where("id = ?", id).Take(&row).Error != nil {
			return false
		}
		return row.UpdatedAt.After(r.LastSynced.Add(2 * time.Second))
	}
	keep := func(ent, why string) {
		res.Kept[ent]++
		if len(res.KeptWhy) < 50 {
			res.KeptWhy = append(res.KeptWhy, why)
		}
	}
	deletedDocs := map[string]bool{}
	hasBookings := db.Migrator().HasTable("bookings")
	hasBookingServices := db.Migrator().HasTable("booking_services")
	err := db.Transaction(func(tx *gorm.DB) error {
		// Documents first.
		for _, r := range refs {
			if !strings.HasPrefix(r.Entity, entDocRef+":") {
				continue
			}
			if !r.Created {
				tx.Delete(&ExternalRef{}, r.ID)
				continue
			}
			var inv invoicing.Invoice
			if tx.Where("id = ? AND organization_id = ?", r.LocalID, org).Take(&inv).Error != nil {
				tx.Delete(&ExternalRef{}, r.ID)
				continue
			}
			var localPays int64
			tx.Model(&invoicing.Payment{}).Where("invoice_id = ? AND COALESCE(source,'') = ''", inv.ID).Count(&localPays)
			if edited("billing_invoices", inv.ID, r) || localPays > 0 || inv.ConvertedToID != nil && !isJobDoc(refs, *inv.ConvertedToID) {
				keep("documents", fmt.Sprintf("%s %s was changed here after the import", strings.ReplaceAll(inv.DocType, "_", " "), inv.Number))
				continue
			}
			for _, m := range []interface{}{&invoicing.InvoiceItem{}, &invoicing.Payment{}, &invoicing.Activity{}, &invoicing.Attachment{}} {
				if err := tx.Where("invoice_id = ?", inv.ID).Delete(m).Error; err != nil {
					return err
				}
			}
			if err := tx.Unscoped().Delete(&invoicing.Invoice{}, "id = ?", inv.ID).Error; err != nil {
				return err
			}
			deletedDocs[inv.ID] = true
			res.Deleted["documents"]++
			tx.Delete(&ExternalRef{}, r.ID)
		}
		// Payments added by this job to documents that stay.
		touched := map[string]bool{}
		for _, r := range refs {
			if r.Entity != entPaymentRef {
				continue
			}
			var p invoicing.Payment
			if tx.Where("id = ?", r.LocalID).Take(&p).Error == nil && !deletedDocs[p.InvoiceID] {
				var docRef ExternalRef
				ownDoc := tx.Where("organization_id = ? AND local_id = ? AND job_id = ? AND created = ?", org, p.InvoiceID, j.ID, true).Take(&docRef).Error == nil
				if ownDoc { // the document was kept (edited): keep its payments too
					continue
				}
				tx.Delete(&invoicing.Payment{}, "id = ?", p.ID)
				touched[p.InvoiceID] = true
				res.Deleted["payments"]++
			} else if deletedDocs[p.InvoiceID] {
				res.Deleted["payments"]++
			}
			tx.Delete(&ExternalRef{}, r.ID)
		}
		for id := range touched {
			var inv invoicing.Invoice
			if tx.Preload("Payments").Where("id = ?", id).Take(&inv).Error == nil {
				invoicing.ApplyPayments(&inv, now)
				tx.Model(&invoicing.Invoice{}).Where("id = ?", id).UpdateColumns(map[string]interface{}{"amount_paid": inv.AmountPaid, "balance_due": inv.BalanceDue, "status": inv.Status, "paid_at": inv.PaidAt})
			}
		}
		// History and PDFs of documents that stay.
		for _, r := range refs {
			if r.Entity == "history" || r.Entity == entPDFRef {
				tx.Delete(&ExternalRef{}, r.ID)
			}
		}
		// Contact people, then customers.
		for _, r := range refs {
			if r.Entity != entPersonRef {
				continue
			}
			if r.Created && !edited("customer_contacts", r.LocalID, r) {
				if tx.Where("id = ?", r.LocalID).Delete(&models.CustomerContact{}).RowsAffected > 0 {
					res.Deleted["contact_people"]++
				}
			}
			tx.Delete(&ExternalRef{}, r.ID)
		}
		for _, r := range refs {
			if r.Entity != entContactRef {
				continue
			}
			if !r.Created {
				tx.Delete(&ExternalRef{}, r.ID)
				continue
			}
			var cu models.Customer
			if tx.Omit("Organization").Where("id = ? AND organization_id = ?", r.LocalID, org).Take(&cu).Error != nil {
				tx.Delete(&ExternalRef{}, r.ID)
				continue
			}
			var docs, bookings int64
			tx.Model(&invoicing.Invoice{}).Where("customer_id = ?", cu.ID).Count(&docs)
			if hasBookings {
				tx.Table("bookings").Where("customer_id = ? AND deleted_at IS NULL", cu.ID).Count(&bookings)
			}
			name := strings.TrimSpace(cu.FirstName + " " + cu.LastName)
			switch {
			case edited("customers", cu.ID, r):
				keep("customers", name+" was changed here after the import")
				continue
			case docs > 0 || bookings > 0:
				keep("customers", name+" has invoices, quotes or bookings that were not created by this import")
				continue
			}
			tx.Where("customer_id = ?", cu.ID).Delete(&models.CustomerContact{})
			if err := tx.Unscoped().Delete(&models.Customer{}, "id = ?", cu.ID).Error; err != nil {
				return err
			}
			res.Deleted["customers"]++
			tx.Delete(&ExternalRef{}, r.ID)
		}
		for _, r := range refs {
			if r.Entity != entItemRef {
				continue
			}
			if r.Created && !edited("services", r.LocalID, r) {
				var used int64
				if hasBookingServices {
					tx.Table("booking_services").Where("service_id = ?", r.LocalID).Count(&used)
				}
				if used == 0 {
					tx.Unscoped().Delete(&models.Service{}, "id = ?", r.LocalID)
					res.Deleted["items"]++
				} else {
					keep("items", "an item is used by bookings")
					continue
				}
			}
			tx.Delete(&ExternalRef{}, r.ID)
		}
		if j.Summary != nil {
			for _, n := range j.Summary.Numbering {
				if invoicing.RestoreNumbering(tx, org, n) {
					res.Deleted["numbering_restored"]++
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func isJobDoc(refs []ExternalRef, localID string) bool {
	for _, r := range refs {
		if r.LocalID == localID && r.Created {
			return true
		}
	}
	return false
}
