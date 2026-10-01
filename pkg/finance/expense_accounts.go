package finance

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EnsureExpenseAccount returns the id of the organisation's operating-expense
// account named name, creating it (with code, or the next free 64xx code)
// when missing. Used by pkg/expenses to code approved vendor bills (Web
// Hosting, Domain Names, Email & Workspace …) so each category is its own P&L
// line.
func (s *Service) EnsureExpenseAccount(orgID, code, name string) (string, error) {
	s.EnsureSetup(orgID)
	name = strings.TrimSpace(name)
	var acc ChartOfAccount
	err := s.db.Where("organization_id = ? AND LOWER(name) = LOWER(?) AND account_type = ?", orgID, name, TypeExpense).Order("is_active DESC, code").First(&acc).Error
	if err == nil {
		return acc.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	used := func(c string) bool {
		var n int64
		s.db.Model(&ChartOfAccount{}).Where("organization_id = ? AND code = ?", orgID, c).Count(&n)
		return n > 0
	}
	if code == "" || used(code) {
		code = ""
		for c := 6410; c < 6990; c += 10 {
			if !used(strconv.Itoa(c)) {
				code = strconv.Itoa(c)
				break
			}
		}
		if code == "" {
			return "", invalid("no free account code for %s", name)
		}
	}
	acc = ChartOfAccount{ID: uuid.NewString(), OrganizationID: orgID, Code: code, Name: name, AccountType: TypeExpense, SubType: "Operating Expense",
		Description: "Created by Expenses", IsActive: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.db.Create(&acc).Error; err != nil {
		return "", err
	}
	return acc.ID, nil
}

// EnsureVendorNamed returns the id of the finance vendor called name,
// creating it when missing.
func (s *Service) EnsureVendorNamed(orgID, name, email, defaultAccountID string) (string, error) {
	name = strings.TrimSpace(name)
	var v Vendor
	err := s.db.Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, name).Order("is_active DESC, created_at").First(&v).Error
	if err == nil {
		return v.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	nv, err := s.CreateVendor(orgID, VendorInput{Name: name, Email: email, DefaultAccountID: defaultAccountID, Notes: "Created by Expenses"})
	if err != nil {
		return "", err
	}
	return nv.ID, nil
}

// VendorExists reports whether a finance vendor id belongs to the organisation.
func (s *Service) VendorExists(orgID, id string) bool {
	if id == "" {
		return false
	}
	var n int64
	s.db.Model(&Vendor{}).Where("organization_id = ? AND id = ?", orgID, id).Count(&n)
	return n > 0
}
