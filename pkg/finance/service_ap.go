package finance

import (
	"errors"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Vendors

// VendorInput is the payload for creating/updating a vendor.
type VendorInput struct {
	Name             string `json:"name"`
	ContactName      string `json:"contact_name"`
	Email            string `json:"email"`
	Phone            string `json:"phone"`
	Address          string `json:"address"`
	TaxID            string `json:"tax_id"`
	PaymentTerms     string `json:"payment_terms"`
	DefaultAccountID string `json:"default_account_id"`
	Notes            string `json:"notes"`
	IsActive         *bool  `json:"is_active"`
}

func (s *Service) vendorInOrg(tx *gorm.DB, orgID, id string) (*Vendor, error) {
	var v Vendor
	if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&v).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

func (s *Service) GetVendors(orgID string, includeInactive bool, q string) ([]Vendor, error) {
	s.EnsureSetup(orgID)
	query := s.db.Where("organization_id = ?", orgID)
	if !includeInactive {
		query = query.Where("is_active = ?", true)
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		query = query.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(contact_name) LIKE ?", like, like, like)
	}
	var vendors []Vendor
	if err := query.Order("LOWER(name)").Find(&vendors).Error; err != nil {
		return nil, err
	}
	type agg struct {
		VendorID string
		Cnt      int
		Due      float64
	}
	var rows []agg
	s.db.Model(&Bill{}).Select("vendor_id, COUNT(*) AS cnt, COALESCE(SUM(CASE WHEN status <> 'draft' THEN total - paid_amount ELSE 0 END),0) AS due").
		Where("organization_id = ?", orgID).Group("vendor_id").Scan(&rows)
	m := map[string]agg{}
	for _, r := range rows {
		m[r.VendorID] = r
	}
	for i := range vendors {
		if a, ok := m[vendors[i].ID]; ok {
			vendors[i].BillCount, vendors[i].Outstanding = a.Cnt, r2(a.Due)
		}
	}
	return vendors, nil
}

func (s *Service) applyVendor(orgID string, v *Vendor, in VendorInput) error {
	v.Name = strings.TrimSpace(in.Name)
	if v.Name == "" {
		return invalid("vendor name is required")
	}
	v.ContactName, v.Email, v.Phone = strings.TrimSpace(in.ContactName), strings.TrimSpace(in.Email), strings.TrimSpace(in.Phone)
	v.Address, v.TaxID, v.Notes = strings.TrimSpace(in.Address), strings.TrimSpace(in.TaxID), strings.TrimSpace(in.Notes)
	if v.Email != "" && !strings.Contains(v.Email, "@") {
		return invalid("email address looks invalid")
	}
	v.PaymentTerms = strings.TrimSpace(in.PaymentTerms)
	if v.PaymentTerms == "" {
		v.PaymentTerms = "NET30"
	}
	v.DefaultAccountID = ""
	if in.DefaultAccountID != "" {
		if _, err := s.accountInOrg(s.db, orgID, in.DefaultAccountID); err != nil {
			return err
		}
		v.DefaultAccountID = in.DefaultAccountID
	}
	if in.IsActive != nil {
		v.IsActive = *in.IsActive
	}
	return nil
}

func (s *Service) CreateVendor(orgID string, in VendorInput) (*Vendor, error) {
	v := Vendor{ID: uuid.NewString(), OrganizationID: orgID, IsActive: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.applyVendor(orgID, &v, in); err != nil {
		return nil, err
	}
	var n int64
	s.db.Model(&Vendor{}).Where("organization_id = ? AND LOWER(name) = ? AND is_active = ?", orgID, strings.ToLower(v.Name), true).Count(&n)
	if n > 0 {
		return nil, invalid("a vendor called %s already exists", v.Name)
	}
	return &v, s.db.Create(&v).Error
}

func (s *Service) UpdateVendor(orgID, id string, in VendorInput) (*Vendor, error) {
	v, err := s.vendorInOrg(s.db, orgID, id)
	if err != nil {
		return nil, err
	}
	if err := s.applyVendor(orgID, v, in); err != nil {
		return nil, err
	}
	v.UpdatedAt = time.Now()
	return v, s.db.Save(v).Error
}

// DeleteVendor deletes a vendor with no bills, otherwise archives it.
func (s *Service) DeleteVendor(orgID, id string) (archived bool, err error) {
	v, err := s.vendorInOrg(s.db, orgID, id)
	if err != nil {
		return false, err
	}
	var n int64
	s.db.Model(&Bill{}).Where("organization_id = ? AND vendor_id = ?", orgID, id).Count(&n)
	if n > 0 {
		return true, s.db.Model(v).Updates(map[string]interface{}{"is_active": false, "updated_at": time.Now()}).Error
	}
	return false, s.db.Delete(v).Error
}

// ---------------------------------------------------------------------------
// Bills

// BillLineInput is a bill line.
type BillLineInput struct {
	Description string  `json:"description"`
	AccountID   string  `json:"account_id"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	TaxRate     float64 `json:"tax_rate"`
}

// BillInput is the payload for creating/updating a bill.
type BillInput struct {
	VendorID  string          `json:"vendor_id"`
	Reference string          `json:"reference"`
	BillDate  string          `json:"bill_date"`
	DueDate   string          `json:"due_date"`
	Notes     string          `json:"notes"`
	Status    string          `json:"status"` // "draft" keeps it unapproved; anything else approves
	LineItems []BillLineInput `json:"line_items"`
	// PurchaseOrderID (optional) links the bill to a purchase order; nil keeps
	// the current link, "" removes it.
	PurchaseOrderID *string `json:"purchase_order_id"`
}

// linkPurchaseOrder validates and applies the optional purchase-order link.
// A purchase order can be linked to one bill only (so it is counted once).
func (s *Service) linkPurchaseOrder(tx *gorm.DB, orgID string, b *Bill, in BillInput) error {
	if in.PurchaseOrderID == nil {
		return nil
	}
	id := strings.TrimSpace(*in.PurchaseOrderID)
	if id == "" {
		b.PurchaseOrderID = ""
		return nil
	}
	var po struct {
		OrderNumber string
		Status      string
	}
	if err := tx.Table("purchase_orders").Select("order_number, status").
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", id, orgID).Take(&po).Error; err != nil {
		return invalid("choose a purchase order from your suppliers")
	}
	if po.Status == "cancelled" {
		return invalid("purchase order %s is cancelled", po.OrderNumber)
	}
	var other string
	tx.Model(&Bill{}).Select("bill_number").Where("organization_id = ? AND purchase_order_id = ? AND id <> ?", orgID, id, b.ID).Limit(1).Scan(&other)
	if other != "" {
		return invalid("purchase order %s is already linked to bill %s", po.OrderNumber, other)
	}
	b.PurchaseOrderID = id
	return nil
}

// FilterBillsByDate keeps bills dated in [from, to] and/or with a payment
// dated in [paidFrom, paidTo]. Empty bounds are open.
func (s *Service) FilterBillsByDate(orgID string, bills []Bill, from, to, paidFrom, paidTo string) []Bill {
	f, okF := ParseDate(from)
	t, okT := ParseDate(to)
	pf, okPF := ParseDate(paidFrom)
	pt, okPT := ParseDate(paidTo)
	if !okF && !okT && !okPF && !okPT {
		return bills
	}
	paid := map[string]bool{}
	if okPF || okPT {
		q := s.db.Model(&Payment{}).Where("organization_id = ? AND type = ? AND bill_id IS NOT NULL", orgID, "vendor_payment")
		if okPF {
			q = q.Where("payment_date >= ?", pf)
		}
		if okPT {
			q = q.Where("payment_date < ?", pt.AddDate(0, 0, 1))
		}
		var ids []string
		q.Pluck("bill_id", &ids)
		for _, id := range ids {
			paid[id] = true
		}
	}
	out := make([]Bill, 0, len(bills))
	for _, b := range bills {
		d := dateOnly(b.BillDate)
		if (okF && d.Before(f)) || (okT && d.After(t)) {
			continue
		}
		if (okPF || okPT) && !paid[b.ID] {
			continue
		}
		out = append(out, b)
	}
	return out
}

// poNumbers maps purchase-order ids to their numbers for display.
func (s *Service) poNumbers(orgID string, bills []Bill) map[string]string {
	ids := []string{}
	for _, b := range bills {
		if b.PurchaseOrderID != "" {
			ids = append(ids, b.PurchaseOrderID)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	var rows []struct{ ID, OrderNumber string }
	s.db.Table("purchase_orders").Select("id, order_number").Where("organization_id = ? AND id IN ?", orgID, ids).Scan(&rows)
	for _, r := range rows {
		out[r.ID] = r.OrderNumber
	}
	return out
}

// decorateBill fills computed fields.
func decorateBill(b *Bill, vendorNames map[string]string) { decorateBillOn(b, vendorNames, today()) }

// decorateBillOn derives display fields with t as "today" (the user's calendar date).
func decorateBillOn(b *Bill, vendorNames map[string]string, t time.Time) {
	b.VendorName = vendorNames[b.VendorID]
	b.AmountDue = r2(b.Total - b.PaidAmount)
	if b.AmountDue < 0 {
		b.AmountDue = 0
	}
	b.DisplayStatus = b.Status
	if b.Status == "unpaid" || b.Status == "partial" {
		d := dateOnly(b.DueDate)
		if d.Before(t) {
			b.DisplayStatus = "overdue"
			b.DaysOverdue = int(t.Sub(d).Hours() / 24)
		}
	}
	sort.Slice(b.LineItems, func(i, j int) bool { return b.LineItems[i].SortOrder < b.LineItems[j].SortOrder })
}

func (s *Service) vendorNames(orgID string) map[string]string {
	var vs []Vendor
	s.db.Select("id, name").Where("organization_id = ?", orgID).Find(&vs)
	m := make(map[string]string, len(vs))
	for _, v := range vs {
		m[v.ID] = v.Name
	}
	return m
}

// GetBills lists bills. status: all, draft, unpaid (unpaid+partial), overdue, paid.
func (s *Service) GetBills(orgID, status, q, vendorID string) ([]Bill, error) {
	return s.GetBillsOn(orgID, status, q, vendorID, today())
}

// GetBillsOn lists bills with t as "today" for the overdue status.
func (s *Service) GetBillsOn(orgID, status, q, vendorID string, t time.Time) ([]Bill, error) {
	s.EnsureSetup(orgID)
	query := s.db.Where("organization_id = ?", orgID)
	if vendorID != "" {
		query = query.Where("vendor_id = ?", vendorID)
	}
	switch status {
	case "draft", "paid":
		query = query.Where("status = ?", status)
	case "unpaid", "overdue":
		query = query.Where("status IN ?", []string{"unpaid", "partial"})
	}
	var bills []Bill
	if err := query.Preload("LineItems").Order("due_date ASC, created_at DESC").Find(&bills).Error; err != nil {
		return nil, err
	}
	names := s.vendorNames(orgID)
	pos := s.poNumbers(orgID, bills)
	q = strings.ToLower(strings.TrimSpace(q))
	out := make([]Bill, 0, len(bills))
	for i := range bills {
		b := &bills[i]
		decorateBillOn(b, names, t)
		b.PurchaseOrderNumber = pos[b.PurchaseOrderID]
		if status == "overdue" && b.DisplayStatus != "overdue" {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(b.BillNumber+" "+b.VendorName+" "+b.Reference+" "+b.Notes), q) {
			continue
		}
		out = append(out, *b)
	}
	if status == "paid" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].BillDate.After(out[j].BillDate) })
	}
	return out, nil
}

func (s *Service) GetBill(orgID, id string) (*Bill, error) {
	var b Bill
	err := s.db.Where("organization_id = ? AND id = ?", orgID, id).Preload("LineItems").
		Preload("Payments", func(db *gorm.DB) *gorm.DB { return db.Order("payment_date DESC, created_at DESC") }).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	decorateBill(&b, s.vendorNames(orgID))
	b.PurchaseOrderNumber = s.poNumbers(orgID, []Bill{b})[b.PurchaseOrderID]
	if len(b.Payments) > 0 {
		var banks []BankAccount
		s.db.Select("id, account_name").Where("organization_id = ?", orgID).Find(&banks)
		bn := map[string]string{}
		for _, bk := range banks {
			bn[bk.ID] = bk.AccountName
		}
		for i := range b.Payments {
			if b.Payments[i].BankAccountID != nil {
				b.Payments[i].BankAccountName = bn[*b.Payments[i].BankAccountID]
			}
		}
	}
	return &b, nil
}

// buildBill validates input and computes line and bill totals in cents.
func (s *Service) buildBill(tx *gorm.DB, orgID string, b *Bill, in BillInput) error {
	v, err := s.vendorInOrg(tx, orgID, in.VendorID)
	if err != nil {
		return invalid("choose a vendor")
	}
	bd, ok := ParseDate(in.BillDate)
	if !ok {
		bd = today()
	}
	dd, ok := ParseDate(in.DueDate)
	if !ok {
		dd = bd.AddDate(0, 0, 30)
	}
	if dd.Before(bd) {
		return invalid("due date cannot be before the bill date")
	}
	fallback := v.DefaultAccountID
	if fallback == "" {
		acc, err := s.systemAccount(tx, orgID, codeOtherExpense)
		if err != nil {
			return err
		}
		fallback = acc.ID
	}
	var lines []BillLineItem
	var sub, tax int64
	now := time.Now()
	for _, l := range in.LineItems {
		l.Description = strings.TrimSpace(l.Description)
		if l.Description == "" && l.UnitPrice == 0 {
			continue
		}
		if l.Description == "" {
			return invalid("every line needs a description")
		}
		if l.Quantity == 0 {
			l.Quantity = 1
		}
		if l.Quantity < 0 || l.UnitPrice < 0 {
			return invalid("quantities and prices cannot be negative")
		}
		if l.TaxRate < 0 || l.TaxRate > 100 {
			return invalid("tax rate must be between 0 and 100")
		}
		acct := l.AccountID
		if acct == "" {
			acct = fallback
		}
		acc, err := s.accountInOrg(tx, orgID, acct)
		if err != nil {
			return err
		}
		if acc.AccountType == TypeRevenue {
			return invalid("bill lines should be coded to expense or asset accounts (not %s)", acc.Name)
		}
		lineTotal := r2(l.Quantity * r2(l.UnitPrice))
		lineTax := r2(lineTotal * l.TaxRate / 100)
		sub += cents(lineTotal)
		tax += cents(lineTax)
		lines = append(lines, BillLineItem{ID: uuid.NewString(), BillID: b.ID, AccountID: acct, Description: l.Description,
			Quantity: l.Quantity, UnitPrice: r2(l.UnitPrice), TaxRate: l.TaxRate, TaxAmount: lineTax, LineTotal: lineTotal,
			SortOrder: len(lines), CreatedAt: now, UpdatedAt: now})
	}
	if len(lines) == 0 {
		return invalid("add at least one line item")
	}
	if sub+tax <= 0 {
		return invalid("bill total must be greater than zero")
	}
	if err := s.linkPurchaseOrder(tx, orgID, b, in); err != nil {
		return err
	}
	b.VendorID, b.Reference, b.Notes = v.ID, strings.TrimSpace(in.Reference), strings.TrimSpace(in.Notes)
	b.BillDate, b.DueDate = bd, dd
	b.SubTotal, b.TaxAmount, b.Total = float64(sub)/100, float64(tax)/100, float64(sub+tax)/100
	b.LineItems = lines
	return nil
}

// postBill writes the AP journal: Dr expense lines + tax, Cr accounts payable.
func (s *Service) postBill(tx *gorm.DB, b *Bill, userID, vendorName string) error {
	ap, err := s.systemAccount(tx, b.OrganizationID, codePayable)
	if err != nil {
		return err
	}
	byAcct := map[string]int64{}
	var order []string
	for _, l := range b.LineItems {
		if _, ok := byAcct[l.AccountID]; !ok {
			order = append(order, l.AccountID)
		}
		byAcct[l.AccountID] += cents(l.LineTotal)
	}
	var lines []JournalLineInput
	for _, a := range order {
		lines = append(lines, JournalLineInput{AccountID: a, Description: vendorName + " — " + b.BillNumber, DebitAmount: float64(byAcct[a]) / 100})
	}
	if cents(b.TaxAmount) > 0 {
		tr, err := s.systemAccount(tx, b.OrganizationID, codeTaxReceivable)
		if err != nil {
			return err
		}
		lines = append(lines, JournalLineInput{AccountID: tr.ID, Description: "Tax on " + b.BillNumber, DebitAmount: b.TaxAmount})
	}
	lines = append(lines, JournalLineInput{AccountID: ap.ID, Description: vendorName + " — " + b.BillNumber, CreditAmount: b.Total})
	ref := b.BillNumber
	if b.Reference != "" {
		ref = b.Reference
	}
	e, err := s.createPosted(tx, b.OrganizationID, userID, b.BillDate, "Bill "+b.BillNumber+" from "+vendorName, ref, "bill", b.ID, lines)
	if err != nil {
		return err
	}
	b.JournalEntryID = e.ID
	return tx.Model(&Bill{}).Where("id = ?", b.ID).Update("journal_entry_id", e.ID).Error
}

// unpostBill reverses a bill's AP journal, if any.
func (s *Service) unpostBill(tx *gorm.DB, b *Bill, userID string) error {
	if b.JournalEntryID == "" {
		return nil
	}
	var e JournalEntry
	if err := tx.Preload("LineItems").Where("id = ?", b.JournalEntryID).First(&e).Error; err == nil && e.Status == "posted" {
		if _, err := s.reverseEntry(tx, &e, userID, today()); err != nil {
			return err
		}
	}
	b.JournalEntryID = ""
	return tx.Model(&Bill{}).Where("id = ?", b.ID).Update("journal_entry_id", "").Error
}

func (s *Service) CreateBill(orgID, userID string, in BillInput) (*Bill, error) {
	s.EnsureSetup(orgID)
	var id string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		b := Bill{ID: uuid.NewString(), OrganizationID: orgID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := s.buildBill(tx, orgID, &b, in); err != nil {
			return err
		}
		b.BillNumber = nextNumber(tx, &Bill{}, "bill_number", orgID, "BILL")
		b.Currency = orgprefs.Currency(tx, orgID)
		b.Status = "unpaid"
		if in.Status == "draft" {
			b.Status = "draft"
		}
		lines := b.LineItems
		b.LineItems = nil
		if err := tx.Omit("LineItems", "Payments").Create(&b).Error; err != nil {
			return err
		}
		if err := tx.Create(&lines).Error; err != nil {
			return err
		}
		b.LineItems = lines
		id = b.ID
		if b.Status != "draft" {
			v, _ := s.vendorInOrg(tx, orgID, b.VendorID)
			return s.postBill(tx, &b, userID, v.Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetBill(orgID, id)
}

// UpdateBill edits a bill. Bills with payments can only change reference,
// notes and due date.
func (s *Service) UpdateBill(orgID, userID, id string, in BillInput) (*Bill, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var b Bill
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).Preload("LineItems").First(&b).Error; err != nil {
			return ErrNotFound
		}
		if cents(b.PaidAmount) > 0 {
			if dd, ok := ParseDate(in.DueDate); ok {
				if dd.Before(dateOnly(b.BillDate)) {
					return invalid("due date cannot be before the bill date")
				}
				b.DueDate = dd
			}
			if err := s.linkPurchaseOrder(tx, orgID, &b, in); err != nil {
				return err
			}
			return tx.Model(&Bill{}).Where("id = ?", b.ID).Updates(map[string]interface{}{
				"reference": strings.TrimSpace(in.Reference), "notes": strings.TrimSpace(in.Notes), "due_date": b.DueDate,
				"purchase_order_id": b.PurchaseOrderID, "updated_at": time.Now()}).Error
		}
		if err := s.unpostBill(tx, &b, userID); err != nil {
			return err
		}
		if err := s.buildBill(tx, orgID, &b, in); err != nil {
			return err
		}
		if in.Status == "draft" {
			b.Status = "draft"
		} else {
			b.Status = "unpaid"
		}
		b.UpdatedAt = time.Now()
		lines := b.LineItems
		b.LineItems = nil
		if err := tx.Where("bill_id = ?", b.ID).Delete(&BillLineItem{}).Error; err != nil {
			return err
		}
		if err := tx.Create(&lines).Error; err != nil {
			return err
		}
		if err := tx.Omit("LineItems", "Payments").Save(&b).Error; err != nil {
			return err
		}
		b.LineItems = lines
		if b.Status != "draft" {
			v, _ := s.vendorInOrg(tx, orgID, b.VendorID)
			return s.postBill(tx, &b, userID, v.Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetBill(orgID, id)
}

// ApproveBill moves a draft bill to unpaid and posts it.
func (s *Service) ApproveBill(orgID, userID, id string) (*Bill, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var b Bill
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).Preload("LineItems").First(&b).Error; err != nil {
			return ErrNotFound
		}
		if b.Status != "draft" {
			return invalid("bill is already approved")
		}
		if err := tx.Model(&Bill{}).Where("id = ?", b.ID).Updates(map[string]interface{}{"status": "unpaid", "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		v, err := s.vendorInOrg(tx, orgID, b.VendorID)
		if err != nil {
			return err
		}
		return s.postBill(tx, &b, userID, v.Name)
	})
	if err != nil {
		return nil, err
	}
	return s.GetBill(orgID, id)
}

// DeleteBill deletes a bill without payments and reverses its journal.
func (s *Service) DeleteBill(orgID, userID, id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var b Bill
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&b).Error; err != nil {
			return ErrNotFound
		}
		var n int64
		tx.Model(&Payment{}).Where("bill_id = ?", b.ID).Count(&n)
		if n > 0 {
			return invalid("this bill has payments — remove them before deleting the bill")
		}
		if err := s.unpostBill(tx, &b, userID); err != nil {
			return err
		}
		if err := tx.Where("bill_id = ?", b.ID).Delete(&BillLineItem{}).Error; err != nil {
			return err
		}
		return tx.Delete(&b).Error
	})
}

// PaymentInput is a bill payment.
type PaymentInput struct {
	Amount        float64 `json:"amount"`
	PaymentDate   string  `json:"payment_date"`
	PaymentMethod string  `json:"payment_method"`
	BankAccountID string  `json:"bank_account_id"`
	Reference     string  `json:"reference"`
	Notes         string  `json:"notes"`
}

func billStatus(total, paid float64) string {
	switch {
	case cents(paid) >= cents(total):
		return "paid"
	case cents(paid) > 0:
		return "partial"
	default:
		return "unpaid"
	}
}

// RecordBillPayment records a (partial) payment: Dr AP, Cr bank/cash, and a
// withdrawal on the bank account.
func (s *Service) RecordBillPayment(orgID, userID, billID string, in PaymentInput) (*Bill, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var b Bill
		if err := tx.Where("organization_id = ? AND id = ?", orgID, billID).First(&b).Error; err != nil {
			return ErrNotFound
		}
		if b.Status == "draft" {
			return invalid("approve the bill before recording a payment")
		}
		amt := r2(in.Amount)
		due := cents(b.Total) - cents(b.PaidAmount)
		if cents(amt) <= 0 {
			return invalid("payment amount must be greater than zero")
		}
		if cents(amt) > due {
			return invalid("payment is more than the amount due (%.2f)", float64(due)/100)
		}
		date, ok := ParseDate(in.PaymentDate)
		if !ok {
			date = today()
		}
		method := strings.TrimSpace(in.PaymentMethod)
		if method == "" {
			method = "bank_transfer"
		}
		v, _ := s.vendorInOrg(tx, orgID, b.VendorID)
		vendorName := "Vendor"
		if v != nil {
			vendorName = v.Name
		}
		ap, err := s.systemAccount(tx, orgID, codePayable)
		if err != nil {
			return err
		}
		var bank *BankAccount
		var creditAcct string
		if in.BankAccountID != "" {
			bank, err = s.bankInOrg(tx, orgID, in.BankAccountID)
			if err != nil {
				return invalid("bank account not found")
			}
			if creditAcct, err = s.ensureBankLedgerAccount(tx, bank); err != nil {
				return err
			}
		} else {
			cash, err := s.systemAccount(tx, orgID, codeCash)
			if err != nil {
				return err
			}
			creditAcct = cash.ID
		}
		pay := Payment{ID: uuid.NewString(), OrganizationID: orgID, Type: "vendor_payment", BillID: &b.ID, Amount: amt, PaymentDate: date,
			PaymentMethod: method, Reference: strings.TrimSpace(in.Reference), Notes: strings.TrimSpace(in.Notes), CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if bank != nil {
			pay.BankAccountID = &bank.ID
		}
		desc := "Payment to " + vendorName + " for " + b.BillNumber
		e, err := s.createPosted(tx, orgID, userID, date, desc, pay.Reference, "bill_payment", pay.ID, []JournalLineInput{
			{AccountID: ap.ID, Description: desc, DebitAmount: amt},
			{AccountID: creditAcct, Description: desc, CreditAmount: amt},
		})
		if err != nil {
			return err
		}
		pay.JournalEntryID = e.ID
		if err := tx.Create(&pay).Error; err != nil {
			return err
		}
		if bank != nil {
			if _, err := s.addBankLine(tx, bank, date, desc, pay.Reference, -amt, ap.ID, ap.Name, e.ID, "bill_payment", pay.ID); err != nil {
				return err
			}
		}
		paid := float64(cents(b.PaidAmount)+cents(amt)) / 100
		status := billStatus(b.Total, paid)
		upd := map[string]interface{}{"paid_amount": paid, "status": status, "updated_at": time.Now()}
		if status == "paid" {
			upd["paid_at"] = date
		}
		return tx.Model(&Bill{}).Where("id = ?", b.ID).Updates(upd).Error
	})
	if err != nil {
		return nil, err
	}
	return s.GetBill(orgID, billID)
}

// DeleteBillPayment removes a payment, reversing its journal and bank line.
func (s *Service) DeleteBillPayment(orgID, userID, billID, paymentID string) (*Bill, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var b Bill
		if err := tx.Where("organization_id = ? AND id = ?", orgID, billID).First(&b).Error; err != nil {
			return ErrNotFound
		}
		var p Payment
		if err := tx.Where("organization_id = ? AND id = ? AND bill_id = ?", orgID, paymentID, billID).First(&p).Error; err != nil {
			return ErrNotFound
		}
		if p.JournalEntryID != "" {
			var e JournalEntry
			if err := tx.Preload("LineItems").Where("id = ?", p.JournalEntryID).First(&e).Error; err == nil && e.Status == "posted" {
				if _, err := s.reverseEntry(tx, &e, userID, today()); err != nil {
					return err
				}
			}
		}
		var bt BankTransaction
		if err := tx.Where("organization_id = ? AND source = ? AND source_id = ?", orgID, "bill_payment", p.ID).First(&bt).Error; err == nil {
			if err := s.removeBankLine(tx, &bt); err != nil {
				return err
			}
		}
		if err := tx.Delete(&p).Error; err != nil {
			return err
		}
		paid := float64(cents(b.PaidAmount)-cents(p.Amount)) / 100
		if paid < 0 {
			paid = 0
		}
		return tx.Model(&Bill{}).Where("id = ?", b.ID).Updates(map[string]interface{}{
			"paid_amount": paid, "status": billStatus(b.Total, paid), "paid_at": nil, "updated_at": time.Now()}).Error
	})
	if err != nil {
		return nil, err
	}
	return s.GetBill(orgID, billID)
}

// Legacy helper kept for compatibility.
func (s *Service) CreatePayment(payment *Payment) error {
	payment.ID = uuid.NewString()
	payment.Amount = r2(payment.Amount)
	payment.CreatedAt, payment.UpdatedAt = time.Now(), time.Now()
	return s.db.Create(payment).Error
}
