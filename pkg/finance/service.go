package finance

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrValidation marks errors caused by bad input (mapped to HTTP 400/409).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...interface{}) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// ErrNotFound is returned when a record does not exist in the organisation.
var ErrNotFound = errors.New("not found")

type Service struct {
	db     *gorm.DB
	seeded sync.Map
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// ---------------------------------------------------------------------------
// Money helpers — all amounts are rounded to cents.

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

func today() time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ParseDate parses YYYY-MM-DD (or RFC3339) into a UTC date.
func ParseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return dateOnly(t), true
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// Chart of accounts

type defaultAccount struct {
	Code, Name, Type, Sub string
	System                bool
}

// System account codes used by automatic postings.
const (
	codeCash          = "1000"
	codeReceivable    = "1200"
	codeTaxReceivable = "1410"
	codePayable       = "2000"
	codeTaxPayable    = "2100"
	codeOwnerEquity   = "3000"
	codeOpeningEquity = "3900"
	codeOtherIncome   = "4900"
	codeOtherExpense  = "6990"
	bankCodeStart     = 1010
	bankCodeEnd       = 1099
	cardCodeStart     = 2210
	cardCodeEnd       = 2299
	subCostOfSales    = "Cost of Sales"
	subFixedAsset     = "Fixed Asset"
	subNonCurrentLiab = "Non-current Liability"
	subBank           = "Bank"
	subCreditCard     = "Credit Card"
)

var defaultAccounts = []defaultAccount{
	{codeCash, "Cash on Hand", TypeAsset, "Current Asset", true},
	{codeReceivable, "Accounts Receivable", TypeAsset, "Current Asset", true},
	{"1300", "Inventory", TypeAsset, "Current Asset", false},
	{"1400", "Prepaid Expenses", TypeAsset, "Current Asset", false},
	{codeTaxReceivable, "GST / Tax Receivable", TypeAsset, "Current Asset", true},
	{"1500", "Office Equipment", TypeAsset, subFixedAsset, false},
	{"1510", "Motor Vehicles", TypeAsset, subFixedAsset, false},
	{"1590", "Accumulated Depreciation", TypeAsset, subFixedAsset, false},
	{codePayable, "Accounts Payable", TypeLiability, "Current Liability", true},
	{codeTaxPayable, "GST / Tax Payable", TypeLiability, "Current Liability", true},
	{"2150", "PAYG Withholding Payable", TypeLiability, "Current Liability", false},
	{"2300", "Accrued Liabilities", TypeLiability, "Current Liability", false},
	{"2500", "Business Loans", TypeLiability, subNonCurrentLiab, false},
	{codeOwnerEquity, "Owner's Capital", TypeEquity, "Capital", true},
	{"3100", "Owner's Drawings", TypeEquity, "Drawings", false},
	{"3200", "Retained Earnings", TypeEquity, "Retained Earnings", false},
	{codeOpeningEquity, "Opening Balance Equity", TypeEquity, "Capital", true},
	{"4000", "Sales Revenue", TypeRevenue, "Operating Revenue", false},
	{"4100", "Service Revenue", TypeRevenue, "Operating Revenue", false},
	{"4800", "Interest Income", TypeRevenue, "Other Income", false},
	{codeOtherIncome, "Other Income", TypeRevenue, "Other Income", true},
	{"5000", "Cost of Goods Sold", TypeExpense, subCostOfSales, false},
	{"5100", "Subcontractors", TypeExpense, subCostOfSales, false},
	{"6000", "Advertising & Marketing", TypeExpense, "Operating Expense", false},
	{"6050", "Bank Fees", TypeExpense, "Operating Expense", false},
	{"6100", "Rent", TypeExpense, "Operating Expense", false},
	{"6150", "Utilities", TypeExpense, "Operating Expense", false},
	{"6200", "Wages & Salaries", TypeExpense, "Operating Expense", false},
	{"6250", "Superannuation", TypeExpense, "Operating Expense", false},
	{"6300", "Insurance", TypeExpense, "Operating Expense", false},
	{"6350", "Office Supplies", TypeExpense, "Operating Expense", false},
	{"6400", "Software & Subscriptions", TypeExpense, "Operating Expense", false},
	{"6450", "Telephone & Internet", TypeExpense, "Operating Expense", false},
	{"6500", "Professional Fees", TypeExpense, "Operating Expense", false},
	{"6550", "Repairs & Maintenance", TypeExpense, "Operating Expense", false},
	{"6600", "Travel", TypeExpense, "Operating Expense", false},
	{"6650", "Motor Vehicle Expenses", TypeExpense, "Operating Expense", false},
	{"6700", "Depreciation", TypeExpense, "Operating Expense", false},
	{codeOtherExpense, "Other Expenses", TypeExpense, "Operating Expense", true},
}

var validTypes = map[string]bool{TypeAsset: true, TypeLiability: true, TypeEquity: true, TypeRevenue: true, TypeExpense: true}

// normalType canonicalises an account type ("asset" → "Asset").
func normalType(t string) string {
	t = strings.TrimSpace(t)
	for k := range validTypes {
		if strings.EqualFold(k, t) {
			return k
		}
	}
	return ""
}

// debitNormal reports whether the account type increases with debits.
func debitNormal(t string) bool { return t == TypeAsset || t == TypeExpense }

// InitializeDefaultAccounts creates the standard small-business chart of
// accounts for an organisation (skipping codes that already exist).
func (s *Service) InitializeDefaultAccounts(orgID string) error {
	var existing []string
	s.db.Model(&ChartOfAccount{}).Where("organization_id = ?", orgID).Pluck("code", &existing)
	have := map[string]bool{}
	for _, c := range existing {
		have[c] = true
	}
	now := time.Now()
	var accounts []ChartOfAccount
	for _, d := range defaultAccounts {
		if have[d.Code] {
			continue
		}
		accounts = append(accounts, ChartOfAccount{ID: uuid.NewString(), OrganizationID: orgID, Code: d.Code, Name: d.Name,
			AccountType: d.Type, SubType: d.Sub, IsActive: true, IsSystem: d.System, CreatedAt: now, UpdatedAt: now})
	}
	if len(accounts) == 0 {
		return nil
	}
	return s.db.Create(&accounts).Error
}

// EnsureSetup seeds the default chart of accounts the first time an
// organisation uses the finance module.
func (s *Service) EnsureSetup(orgID string) {
	if orgID == "" {
		return
	}
	if _, ok := s.seeded.Load(orgID); ok {
		return
	}
	var n int64
	if err := s.db.Model(&ChartOfAccount{}).Where("organization_id = ?", orgID).Count(&n).Error; err != nil {
		return
	}
	if n == 0 {
		if err := s.InitializeDefaultAccounts(orgID); err != nil {
			return
		}
	}
	s.seeded.Store(orgID, true)
}

// systemAccount returns the organisation's account with the given default
// code, creating it when missing.
func (s *Service) systemAccount(tx *gorm.DB, orgID, code string) (*ChartOfAccount, error) {
	var acc ChartOfAccount
	err := tx.Where("organization_id = ? AND code = ?", orgID, code).First(&acc).Error
	if err == nil {
		return &acc, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	for _, d := range defaultAccounts {
		if d.Code == code {
			acc = ChartOfAccount{ID: uuid.NewString(), OrganizationID: orgID, Code: d.Code, Name: d.Name, AccountType: d.Type,
				SubType: d.Sub, IsActive: true, IsSystem: d.System, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			return &acc, tx.Create(&acc).Error
		}
	}
	return nil, fmt.Errorf("unknown system account %s", code)
}

func (s *Service) accountInOrg(tx *gorm.DB, orgID, id string) (*ChartOfAccount, error) {
	if id == "" {
		return nil, invalid("account is required")
	}
	var acc ChartOfAccount
	if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&acc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, invalid("account not found")
		}
		return nil, err
	}
	return &acc, nil
}

type ledgerSum struct {
	AccountID string
	Debit     float64
	Credit    float64
}

// ledgerSums totals general-ledger debits and credits per account between
// from (inclusive, zero = beginning) and to (inclusive, zero = no limit).
func (s *Service) ledgerSums(orgID string, from, to time.Time) (map[string]ledgerSum, error) {
	q := s.db.Model(&GeneralLedger{}).Select("account_id, COALESCE(SUM(debit_amount),0) AS debit, COALESCE(SUM(credit_amount),0) AS credit").
		Where("organization_id = ?", orgID)
	if !from.IsZero() {
		q = q.Where("transaction_date >= ?", dateOnly(from))
	}
	if !to.IsZero() {
		q = q.Where("transaction_date < ?", dateOnly(to).AddDate(0, 0, 1))
	}
	var rows []ledgerSum
	if err := q.Group("account_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]ledgerSum, len(rows))
	for _, r := range rows {
		r.Debit, r.Credit = r2(r.Debit), r2(r.Credit)
		out[r.AccountID] = r
	}
	return out, nil
}

// signedBalance returns the balance in the account's normal direction.
func signedBalance(t string, sum ledgerSum) float64 {
	if debitNormal(t) {
		return r2(sum.Debit - sum.Credit)
	}
	return r2(sum.Credit - sum.Debit)
}

func (s *Service) GetChartOfAccounts(orgID string) ([]ChartOfAccount, error) {
	s.EnsureSetup(orgID)
	var accounts []ChartOfAccount
	if err := s.db.Where("organization_id = ?", orgID).Order("code").Find(&accounts).Error; err != nil {
		return nil, err
	}
	sums, err := s.ledgerSums(orgID, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if sum, ok := sums[accounts[i].ID]; ok {
			accounts[i].Balance = signedBalance(accounts[i].AccountType, sum)
			accounts[i].DebitBalance, accounts[i].CreditBalance = sum.Debit, sum.Credit
			accounts[i].HasActivity = sum.Debit != 0 || sum.Credit != 0
		} else {
			accounts[i].DebitBalance, accounts[i].CreditBalance = 0, 0
		}
	}
	return accounts, nil
}

// AccountInput is the payload for creating/updating an account.
type AccountInput struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	AccountType string `json:"account_type"`
	SubType     string `json:"sub_type"`
	Description string `json:"description"`
	IsActive    *bool  `json:"is_active"`
}

func (s *Service) CreateAccount(orgID string, in AccountInput) (*ChartOfAccount, error) {
	s.EnsureSetup(orgID)
	in.Code, in.Name = strings.TrimSpace(in.Code), strings.TrimSpace(in.Name)
	t := normalType(in.AccountType)
	if in.Code == "" || in.Name == "" {
		return nil, invalid("code and name are required")
	}
	if t == "" {
		return nil, invalid("account type must be Asset, Liability, Equity, Revenue or Expense")
	}
	var n int64
	s.db.Model(&ChartOfAccount{}).Where("organization_id = ? AND code = ?", orgID, in.Code).Count(&n)
	if n > 0 {
		return nil, invalid("account code %s is already in use", in.Code)
	}
	acc := ChartOfAccount{ID: uuid.NewString(), OrganizationID: orgID, Code: in.Code, Name: in.Name, AccountType: t,
		SubType: strings.TrimSpace(in.SubType), Description: in.Description, IsActive: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if in.IsActive != nil {
		acc.IsActive = *in.IsActive
	}
	return &acc, s.db.Create(&acc).Error
}

func (s *Service) UpdateAccount(orgID, id string, in AccountInput) (*ChartOfAccount, error) {
	acc, err := s.accountInOrg(s.db, orgID, id)
	if err != nil {
		return nil, ErrNotFound
	}
	in.Code, in.Name = strings.TrimSpace(in.Code), strings.TrimSpace(in.Name)
	if in.Name == "" || in.Code == "" {
		return nil, invalid("code and name are required")
	}
	t := normalType(in.AccountType)
	if t == "" {
		return nil, invalid("account type must be Asset, Liability, Equity, Revenue or Expense")
	}
	if in.Code != acc.Code {
		if acc.IsSystem {
			return nil, invalid("the code of a system account cannot be changed")
		}
		var n int64
		s.db.Model(&ChartOfAccount{}).Where("organization_id = ? AND code = ? AND id <> ?", orgID, in.Code, id).Count(&n)
		if n > 0 {
			return nil, invalid("account code %s is already in use", in.Code)
		}
	}
	if t != acc.AccountType {
		var n int64
		s.db.Model(&GeneralLedger{}).Where("organization_id = ? AND account_id = ?", orgID, id).Count(&n)
		if n > 0 || acc.IsSystem {
			return nil, invalid("the type of an account with transactions cannot be changed")
		}
	}
	updates := map[string]interface{}{"code": in.Code, "name": in.Name, "account_type": t, "sub_type": strings.TrimSpace(in.SubType),
		"description": in.Description, "updated_at": time.Now()}
	if in.IsActive != nil {
		if !*in.IsActive && acc.IsSystem {
			return nil, invalid("system accounts cannot be archived")
		}
		updates["is_active"] = *in.IsActive
	}
	if err := s.db.Model(acc).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.accountInOrg(s.db, orgID, id)
}

// DeleteAccount removes an unused account, or archives one with history.
func (s *Service) DeleteAccount(orgID, id string) (archived bool, err error) {
	acc, err := s.accountInOrg(s.db, orgID, id)
	if err != nil {
		return false, ErrNotFound
	}
	if acc.IsSystem {
		return false, invalid("system accounts are used by automatic postings and cannot be deleted")
	}
	var used int64
	s.db.Model(&JournalEntryLine{}).Where("chart_of_account_id = ?", id).Count(&used)
	var bank int64
	s.db.Model(&BankAccount{}).Where("organization_id = ? AND chart_account_id = ?", orgID, id).Count(&bank)
	if used > 0 || bank > 0 {
		return true, s.db.Model(acc).Updates(map[string]interface{}{"is_active": false, "updated_at": time.Now()}).Error
	}
	s.db.Where("account_id = ?", id).Delete(&AccountBalance{})
	return false, s.db.Delete(acc).Error
}

// ---------------------------------------------------------------------------
// Journal entries

// JournalLineInput is a debit/credit line.
type JournalLineInput struct {
	AccountID    string  `json:"chart_of_account_id"`
	Description  string  `json:"description"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// JournalInput is the payload for a manual journal entry.
type JournalInput struct {
	Date        string             `json:"date"`
	Description string             `json:"description"`
	Reference   string             `json:"reference"`
	Status      string             `json:"status"` // draft or posted
	LineItems   []JournalLineInput `json:"line_items"`
}

// validateLines rounds and checks a set of lines, returning totals.
func (s *Service) validateLines(tx *gorm.DB, orgID string, lines []JournalLineInput) ([]JournalLineInput, float64, float64, error) {
	var out []JournalLineInput
	var dr, cr int64
	for _, l := range lines {
		l.DebitAmount, l.CreditAmount = r2(l.DebitAmount), r2(l.CreditAmount)
		if l.DebitAmount == 0 && l.CreditAmount == 0 && l.AccountID == "" {
			continue // blank row
		}
		if l.DebitAmount < 0 || l.CreditAmount < 0 {
			return nil, 0, 0, invalid("amounts cannot be negative")
		}
		if l.DebitAmount > 0 && l.CreditAmount > 0 {
			return nil, 0, 0, invalid("a line can have a debit or a credit, not both")
		}
		if l.DebitAmount == 0 && l.CreditAmount == 0 {
			return nil, 0, 0, invalid("every line needs a debit or credit amount")
		}
		acc, err := s.accountInOrg(tx, orgID, l.AccountID)
		if err != nil {
			return nil, 0, 0, err
		}
		if !acc.IsActive {
			return nil, 0, 0, invalid("account %s %s is archived", acc.Code, acc.Name)
		}
		dr += cents(l.DebitAmount)
		cr += cents(l.CreditAmount)
		out = append(out, l)
	}
	if len(out) < 2 {
		return nil, 0, 0, invalid("a journal entry needs at least two lines")
	}
	if dr != cr {
		return nil, 0, 0, invalid("debits (%.2f) and credits (%.2f) must balance", float64(dr)/100, float64(cr)/100)
	}
	if dr == 0 {
		return nil, 0, 0, invalid("journal entry total cannot be zero")
	}
	return out, float64(dr) / 100, float64(cr) / 100, nil
}

// nextNumber returns the next sequential document number for an org.
func nextNumber(tx *gorm.DB, model interface{}, column, orgID, prefix string) string {
	var n int64
	tx.Model(model).Where("organization_id = ?", orgID).Count(&n)
	for i := n + 1; ; i++ {
		num := fmt.Sprintf("%s-%04d", prefix, i)
		var c int64
		tx.Model(model).Where("organization_id = ? AND "+column+" = ?", orgID, num).Count(&c)
		if c == 0 {
			return num
		}
	}
}

// insertEntry stores an entry and its lines without touching associations.
func insertEntry(tx *gorm.DB, e *JournalEntry) error {
	if err := tx.Omit("LineItems").Create(e).Error; err != nil {
		return err
	}
	if len(e.LineItems) == 0 {
		return nil
	}
	return tx.Omit("ChartOfAccount").Create(&e.LineItems).Error
}

func buildLines(entryID string, in []JournalLineInput) []JournalEntryLine {
	now := time.Now()
	out := make([]JournalEntryLine, 0, len(in))
	for i, l := range in {
		out = append(out, JournalEntryLine{ID: uuid.NewString(), JournalEntryID: entryID, ChartOfAccountID: l.AccountID,
			Description: strings.TrimSpace(l.Description), DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount, SortOrder: i,
			CreatedAt: now, UpdatedAt: now})
	}
	return out
}

func (s *Service) ListJournalEntries(orgID, status, q, source string, from, to time.Time) ([]JournalEntry, error) {
	s.EnsureSetup(orgID)
	query := s.db.Where("organization_id = ?", orgID)
	if status != "" && status != "all" {
		query = query.Where("status = ?", status)
	}
	if source == "manual" {
		query = query.Where("source = ? OR source = '' OR source IS NULL", "manual")
	} else if source == "system" {
		query = query.Where("source <> ? AND source <> ''", "manual")
	}
	if !from.IsZero() {
		query = query.Where("date >= ?", from)
	}
	if !to.IsZero() {
		query = query.Where("date < ?", to.AddDate(0, 0, 1))
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		query = query.Where("LOWER(entry_number) LIKE ? OR LOWER(description) LIKE ? OR LOWER(reference) LIKE ?", like, like, like)
	}
	var entries []JournalEntry
	err := query.Preload("LineItems", func(db *gorm.DB) *gorm.DB { return db.Order("sort_order, debit_amount DESC") }).
		Preload("LineItems.ChartOfAccount").Order("date DESC, created_at DESC").Limit(500).Find(&entries).Error
	return entries, err
}

func (s *Service) GetJournalEntry(orgID, id string) (*JournalEntry, error) {
	var e JournalEntry
	err := s.db.Where("organization_id = ? AND id = ?", orgID, id).
		Preload("LineItems", func(db *gorm.DB) *gorm.DB { return db.Order("sort_order, debit_amount DESC") }).
		Preload("LineItems.ChartOfAccount").First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &e, err
}

// CreateJournalEntry creates a manual journal entry (draft or posted).
func (s *Service) CreateJournalEntry(orgID, userID string, in JournalInput) (*JournalEntry, error) {
	s.EnsureSetup(orgID)
	date, ok := ParseDate(in.Date)
	if !ok {
		date = today()
	}
	if strings.TrimSpace(in.Description) == "" {
		return nil, invalid("description is required")
	}
	var id string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		lines, dr, cr, err := s.validateLines(tx, orgID, in.LineItems)
		if err != nil {
			return err
		}
		e := JournalEntry{ID: uuid.NewString(), OrganizationID: orgID, EntryNumber: nextNumber(tx, &JournalEntry{}, "entry_number", orgID, "JE"),
			Date: date, Description: strings.TrimSpace(in.Description), Reference: strings.TrimSpace(in.Reference), TotalDebit: dr, TotalCredit: cr,
			Status: "draft", Source: "manual", CreatedBy: userID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		e.LineItems = buildLines(e.ID, lines)
		if err := insertEntry(tx, &e); err != nil {
			return err
		}
		id = e.ID
		if in.Status == "posted" {
			return s.postEntry(tx, &e, userID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetJournalEntry(orgID, id)
}

// UpdateJournalEntry replaces a draft entry's details and lines.
func (s *Service) UpdateJournalEntry(orgID, userID, id string, in JournalInput) (*JournalEntry, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var e JournalEntry
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&e).Error; err != nil {
			return ErrNotFound
		}
		if e.Status != "draft" {
			return invalid("only draft entries can be edited — reverse a posted entry instead")
		}
		lines, dr, cr, err := s.validateLines(tx, orgID, in.LineItems)
		if err != nil {
			return err
		}
		if strings.TrimSpace(in.Description) == "" {
			return invalid("description is required")
		}
		if d, ok := ParseDate(in.Date); ok {
			e.Date = d
		}
		e.Description, e.Reference = strings.TrimSpace(in.Description), strings.TrimSpace(in.Reference)
		e.TotalDebit, e.TotalCredit, e.UpdatedAt = dr, cr, time.Now()
		if err := tx.Where("journal_entry_id = ?", e.ID).Delete(&JournalEntryLine{}).Error; err != nil {
			return err
		}
		newLines := buildLines(e.ID, lines)
		if err := tx.Omit("ChartOfAccount").Create(&newLines).Error; err != nil {
			return err
		}
		e.LineItems = nil
		if err := tx.Omit("LineItems").Save(&e).Error; err != nil {
			return err
		}
		if in.Status == "posted" {
			e.LineItems = newLines
			return s.postEntry(tx, &e, userID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetJournalEntry(orgID, id)
}

// DeleteJournalEntry deletes a draft entry.
func (s *Service) DeleteJournalEntry(orgID, id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var e JournalEntry
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&e).Error; err != nil {
			return ErrNotFound
		}
		if e.Status != "draft" {
			return invalid("posted entries cannot be deleted — reverse them instead")
		}
		if err := tx.Where("journal_entry_id = ?", e.ID).Delete(&JournalEntryLine{}).Error; err != nil {
			return err
		}
		return tx.Delete(&e).Error
	})
}

// PostJournalEntry posts a draft journal entry to the general ledger.
func (s *Service) PostJournalEntry(orgID, entryID, userID string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var entry JournalEntry
		if err := tx.Preload("LineItems").Where("organization_id = ? AND id = ?", orgID, entryID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Status != "draft" {
			return invalid("journal entry is already %s", entry.Status)
		}
		// Re-validate in case accounts changed since the draft was saved.
		in := make([]JournalLineInput, 0, len(entry.LineItems))
		for _, l := range entry.LineItems {
			in = append(in, JournalLineInput{AccountID: l.ChartOfAccountID, Description: l.Description, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount})
		}
		if _, _, _, err := s.validateLines(tx, orgID, in); err != nil {
			return err
		}
		return s.postEntry(tx, &entry, userID)
	})
}

// ReverseJournalEntry posts a mirror entry that cancels a posted manual entry.
func (s *Service) ReverseJournalEntry(orgID, entryID, userID, dateStr string) (*JournalEntry, error) {
	var newID string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var entry JournalEntry
		if err := tx.Preload("LineItems").Where("organization_id = ? AND id = ?", orgID, entryID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Status != "posted" {
			return invalid("only posted entries can be reversed")
		}
		if entry.Source != "" && entry.Source != "manual" {
			return invalid("this entry was created by a %s — change the source document instead", strings.ReplaceAll(entry.Source, "_", " "))
		}
		date := entry.Date
		if d, ok := ParseDate(dateStr); ok {
			date = d
		}
		rev, err := s.reverseEntry(tx, &entry, userID, date)
		if err != nil {
			return err
		}
		newID = rev.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetJournalEntry(orgID, newID)
}

// reverseEntry creates and posts the reversing entry for a posted entry.
func (s *Service) reverseEntry(tx *gorm.DB, entry *JournalEntry, userID string, date time.Time) (*JournalEntry, error) {
	if len(entry.LineItems) == 0 {
		tx.Where("journal_entry_id = ?", entry.ID).Find(&entry.LineItems)
	}
	lines := make([]JournalLineInput, 0, len(entry.LineItems))
	for _, l := range entry.LineItems {
		lines = append(lines, JournalLineInput{AccountID: l.ChartOfAccountID, Description: l.Description, DebitAmount: l.CreditAmount, CreditAmount: l.DebitAmount})
	}
	rev, err := s.createPosted(tx, entry.OrganizationID, userID, date, "Reversal of "+entry.EntryNumber+": "+entry.Description, entry.Reference, "reversal", entry.ID, lines)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if err := tx.Model(&JournalEntry{}).Where("id = ?", entry.ID).Updates(map[string]interface{}{"status": "reversed", "reversed_by_id": rev.ID, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	tx.Model(&JournalEntry{}).Where("id = ?", rev.ID).Update("reversal_of", entry.ID)
	tx.Model(&GeneralLedger{}).Where("journal_entry_id = ?", entry.ID).Updates(map[string]interface{}{"reversed": true, "reversed_by": userID, "reversed_at": now})
	return rev, nil
}

// createPosted creates and immediately posts a system-generated entry.
func (s *Service) createPosted(tx *gorm.DB, orgID, userID string, date time.Time, desc, ref, source, sourceID string, in []JournalLineInput) (*JournalEntry, error) {
	var dr, cr int64
	var lines []JournalLineInput
	for _, l := range in {
		l.DebitAmount, l.CreditAmount = r2(l.DebitAmount), r2(l.CreditAmount)
		if l.DebitAmount == 0 && l.CreditAmount == 0 {
			continue
		}
		dr += cents(l.DebitAmount)
		cr += cents(l.CreditAmount)
		lines = append(lines, l)
	}
	if dr != cr {
		return nil, fmt.Errorf("unbalanced system entry (%d vs %d cents)", dr, cr)
	}
	e := JournalEntry{ID: uuid.NewString(), OrganizationID: orgID, EntryNumber: nextNumber(tx, &JournalEntry{}, "entry_number", orgID, "JE"),
		Date: dateOnly(date), Description: desc, Reference: ref, TotalDebit: float64(dr) / 100, TotalCredit: float64(cr) / 100,
		Status: "draft", Source: source, SourceID: sourceID, CreatedBy: userID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	e.LineItems = buildLines(e.ID, lines)
	if err := insertEntry(tx, &e); err != nil {
		return nil, err
	}
	if err := s.postEntry(tx, &e, userID); err != nil {
		return nil, err
	}
	return &e, nil
}

// postEntry marks an entry posted and writes it to the general ledger.
func (s *Service) postEntry(tx *gorm.DB, entry *JournalEntry, userID string) error {
	now := time.Now()
	if err := tx.Model(&JournalEntry{}).Where("id = ?", entry.ID).Updates(map[string]interface{}{"status": "posted", "posted_at": now, "updated_at": now}).Error; err != nil {
		return err
	}
	entry.Status, entry.PostedAt = "posted", &now
	return s.postToGeneralLedger(tx, entry, userID)
}

// postToGeneralLedger posts journal entry lines to the general ledger and
// maintains running balances (debit-positive).
func (s *Service) postToGeneralLedger(tx *gorm.DB, entry *JournalEntry, userID string) error {
	if userID == "" {
		userID = "system"
	}
	for _, line := range entry.LineItems {
		var bal AccountBalance
		err := tx.Where("organization_id = ? AND account_id = ?", entry.OrganizationID, line.ChartOfAccountID).First(&bal).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			bal = AccountBalance{ID: uuid.NewString(), OrganizationID: entry.OrganizationID, AccountID: line.ChartOfAccountID}
		} else if err != nil {
			return err
		}
		bal.DebitBalance = r2(bal.DebitBalance + line.DebitAmount)
		bal.CreditBalance = r2(bal.CreditBalance + line.CreditAmount)
		bal.Balance = r2(bal.DebitBalance - bal.CreditBalance)
		bal.LastUpdated, bal.UpdatedBy = time.Now(), userID

		desc := line.Description
		if desc == "" {
			desc = entry.Description
		}
		if desc == "" {
			desc = entry.EntryNumber
		}
		gl := GeneralLedger{ID: uuid.NewString(), OrganizationID: entry.OrganizationID, AccountID: line.ChartOfAccountID, JournalEntryID: entry.ID,
			TransactionDate: dateOnly(entry.Date), Description: desc, Reference: entry.Reference, DebitAmount: line.DebitAmount, CreditAmount: line.CreditAmount,
			RunningBalance: bal.Balance, PostedBy: userID, PostedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := tx.Omit("ChartOfAccount", "JournalEntry").Create(&gl).Error; err != nil {
			return err
		}
		if err := tx.Omit("ChartOfAccount").Save(&bal).Error; err != nil {
			return err
		}
		if err := tx.Model(&ChartOfAccount{}).Where("id = ?", line.ChartOfAccountID).
			Updates(map[string]interface{}{"debit_balance": bal.DebitBalance, "credit_balance": bal.CreditBalance, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		s.createAuditTrail(tx, entry.OrganizationID, "general_ledger", gl.ID, "CREATE", "", "", userID)
	}
	return nil
}

// createAuditTrail creates audit trail entries.
func (s *Service) createAuditTrail(tx *gorm.DB, orgID, tableName, recordID, action, oldValue, newValue, userID string) {
	audit := AuditTrail{ID: uuid.NewString(), OrganizationID: orgID, TableName: tableName, RecordID: recordID, Action: action,
		OldValue: oldValue, NewValue: newValue, UserID: userID, Timestamp: time.Now()}
	tx.Create(&audit)
}

// ---------------------------------------------------------------------------
// Reports (all computed from the general ledger)

func (s *Service) activeAccounts(orgID string) ([]ChartOfAccount, error) {
	s.EnsureSetup(orgID)
	var accounts []ChartOfAccount
	err := s.db.Where("organization_id = ?", orgID).Order("code").Find(&accounts).Error
	return accounts, err
}

// GetTrialBalance generates the trial balance as of a date.
func (s *Service) GetTrialBalance(orgID string, asOfDate time.Time) (*TrialBalanceReport, error) {
	accounts, err := s.activeAccounts(orgID)
	if err != nil {
		return nil, err
	}
	sums, err := s.ledgerSums(orgID, time.Time{}, asOfDate)
	if err != nil {
		return nil, err
	}
	rep := &TrialBalanceReport{AsOfDate: asOfDate.Format("2006-01-02"), Rows: []TrialBalance{}}
	var dr, cr int64
	for _, a := range accounts {
		sum, ok := sums[a.ID]
		if !ok {
			continue
		}
		net := r2(sum.Debit - sum.Credit)
		if net == 0 {
			continue
		}
		row := TrialBalance{AccountID: a.ID, AccountCode: a.Code, AccountName: a.Name, AccountType: a.AccountType}
		if net > 0 {
			row.DebitBalance = net
			dr += cents(net)
		} else {
			row.CreditBalance = -net
			cr += cents(-net)
		}
		rep.Rows = append(rep.Rows, row)
	}
	rep.TotalDebit, rep.TotalCredit = float64(dr)/100, float64(cr)/100
	rep.Balanced = dr == cr
	return rep, nil
}

// GetProfitLoss generates the P&L statement for a period (inclusive dates).
func (s *Service) GetProfitLoss(orgID string, startDate, endDate time.Time) (*ProfitLoss, error) {
	accounts, err := s.activeAccounts(orgID)
	if err != nil {
		return nil, err
	}
	sums, err := s.ledgerSums(orgID, startDate, endDate)
	if err != nil {
		return nil, err
	}
	pl := &ProfitLoss{Period: fmt.Sprintf("%s to %s", startDate.Format("2006-01-02"), endDate.Format("2006-01-02")),
		StartDate: startDate.Format("2006-01-02"), EndDate: endDate.Format("2006-01-02"),
		Revenue: []FinancialReportLine{}, CostOfSales: []FinancialReportLine{}, Expenses: []FinancialReportLine{}}
	var rev, cos, exp int64
	for _, a := range accounts {
		sum, ok := sums[a.ID]
		if !ok || (a.AccountType != TypeRevenue && a.AccountType != TypeExpense) {
			continue
		}
		amt := signedBalance(a.AccountType, sum)
		if amt == 0 {
			continue
		}
		line := FinancialReportLine{AccountID: a.ID, AccountCode: a.Code, AccountName: a.Name, Amount: amt}
		switch {
		case a.AccountType == TypeRevenue:
			pl.Revenue = append(pl.Revenue, line)
			rev += cents(amt)
		case strings.EqualFold(a.SubType, subCostOfSales):
			pl.CostOfSales = append(pl.CostOfSales, line)
			cos += cents(amt)
		default:
			pl.Expenses = append(pl.Expenses, line)
			exp += cents(amt)
		}
	}
	pl.TotalRevenue = float64(rev) / 100
	pl.TotalCostOfSales = float64(cos) / 100
	pl.GrossProfit = float64(rev-cos) / 100
	pl.TotalExpenses = float64(exp) / 100
	pl.NetIncome = float64(rev-cos-exp) / 100
	return pl, nil
}

// GetBalanceSheet generates the balance sheet as of a date. Accumulated
// profit (revenue − expenses to date) is shown as "Current earnings" in
// equity so the statement balances.
func (s *Service) GetBalanceSheet(orgID string, asOfDate time.Time) (*BalanceSheet, error) {
	accounts, err := s.activeAccounts(orgID)
	if err != nil {
		return nil, err
	}
	sums, err := s.ledgerSums(orgID, time.Time{}, asOfDate)
	if err != nil {
		return nil, err
	}
	bs := &BalanceSheet{AsOfDate: asOfDate.Format("2006-01-02")}
	for _, sec := range []*BalanceSheetSection{&bs.Assets, &bs.Liabilities, &bs.Equity} {
		sec.Current, sec.NonCurrent = []FinancialReportLine{}, []FinancialReportLine{}
	}
	var assets, liab, equity, earnings int64
	for _, a := range accounts {
		sum, ok := sums[a.ID]
		if !ok {
			continue
		}
		amt := signedBalance(a.AccountType, sum)
		if amt == 0 {
			continue
		}
		line := FinancialReportLine{AccountID: a.ID, AccountCode: a.Code, AccountName: a.Name, Amount: amt}
		sub := strings.ToLower(a.SubType)
		nonCurrent := strings.Contains(sub, "fixed") || strings.Contains(sub, "non-current") || strings.Contains(sub, "long")
		switch a.AccountType {
		case TypeAsset:
			if nonCurrent {
				bs.Assets.NonCurrent = append(bs.Assets.NonCurrent, line)
			} else {
				bs.Assets.Current = append(bs.Assets.Current, line)
			}
			assets += cents(amt)
		case TypeLiability:
			if nonCurrent {
				bs.Liabilities.NonCurrent = append(bs.Liabilities.NonCurrent, line)
			} else {
				bs.Liabilities.Current = append(bs.Liabilities.Current, line)
			}
			liab += cents(amt)
		case TypeEquity:
			bs.Equity.Current = append(bs.Equity.Current, line)
			equity += cents(amt)
		case TypeRevenue:
			earnings += cents(amt)
		case TypeExpense:
			earnings -= cents(amt)
		}
	}
	if earnings != 0 {
		bs.Equity.Current = append(bs.Equity.Current, FinancialReportLine{AccountCode: "", AccountName: "Current earnings", Amount: float64(earnings) / 100})
		equity += earnings
	}
	bs.Assets.Total = float64(assets) / 100
	bs.Liabilities.Total = float64(liab) / 100
	bs.Equity.Total = float64(equity) / 100
	bs.TotalAssets = bs.Assets.Total
	bs.TotalLiabEq = float64(liab+equity) / 100
	bs.Balanced = assets == liab+equity
	return bs, nil
}

// GetGeneralLedger gets ledger entries for an account (or all accounts).
func (s *Service) GetGeneralLedger(orgID, accountID string, startDate, endDate time.Time) ([]GeneralLedger, error) {
	var entries []GeneralLedger
	query := s.db.Where("organization_id = ?", orgID)
	if accountID != "" {
		query = query.Where("account_id = ?", accountID)
	}
	if !startDate.IsZero() {
		query = query.Where("transaction_date >= ?", startDate)
	}
	if !endDate.IsZero() {
		query = query.Where("transaction_date < ?", endDate.AddDate(0, 0, 1))
	}
	err := query.Preload("ChartOfAccount").Preload("JournalEntry").
		Order("transaction_date DESC, created_at DESC").Limit(1000).Find(&entries).Error
	return entries, err
}

// ---------------------------------------------------------------------------
// Overview

// GetSummary returns the KPIs used across the finance pages.
func (s *Service) GetSummary(orgID string) (*Summary, error) {
	return s.GetSummaryOn(orgID, today())
}

// GetSummaryOn computes the summary for the calendar date t (midnight UTC of
// the user's local date), so "overdue" and "this month" follow the user's calendar.
func (s *Service) GetSummaryOn(orgID string, t time.Time) (*Summary, error) {
	s.EnsureSetup(orgID)
	sum := &Summary{}
	var banks []BankAccount
	s.db.Where("organization_id = ? AND is_active = ?", orgID, true).Find(&banks)
	var cash, card int64
	for _, b := range banks {
		if b.AccountType == "credit_card" {
			card += cents(b.Balance)
		} else {
			cash += cents(b.Balance)
		}
	}
	sum.CashInBank, sum.CreditCardBalance, sum.BankAccounts = float64(cash)/100, float64(card)/100, int64(len(banks))

	var bills []Bill
	s.db.Where("organization_id = ?", orgID).Find(&bills)
	soon := t.AddDate(0, 0, 7)
	var out, overdue, due int64
	for _, b := range bills {
		if b.Status == "draft" {
			sum.DraftBills++
			continue
		}
		dueAmt := cents(b.Total) - cents(b.PaidAmount)
		if dueAmt <= 0 {
			continue
		}
		sum.OpenBills++
		out += dueAmt
		d := dateOnly(b.DueDate)
		if d.Before(t) {
			overdue += dueAmt
			sum.OverdueCount++
		} else if !d.After(soon) {
			due += dueAmt
			sum.DueSoonCount++
		}
	}
	sum.PayablesOutstanding, sum.PayablesOverdue, sum.DueSoon = float64(out)/100, float64(overdue)/100, float64(due)/100

	monthStart := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	var paid float64
	s.db.Model(&Payment{}).Where("organization_id = ? AND type = ? AND payment_date >= ?", orgID, "vendor_payment", monthStart).
		Select("COALESCE(SUM(amount),0)").Row().Scan(&paid)
	sum.PaidThisMonth = r2(paid)

	if pl, err := s.GetProfitLoss(orgID, monthStart, t); err == nil {
		sum.RevenueThisMonth = pl.TotalRevenue
		sum.ExpensesThisMonth = r2(pl.TotalCostOfSales + pl.TotalExpenses)
		sum.NetProfitThisMonth = pl.NetIncome
	}
	s.db.Model(&JournalEntry{}).Where("organization_id = ? AND status = ?", orgID, "draft").Count(&sum.DraftJournals)
	s.db.Model(&BankTransaction{}).Where("organization_id = ? AND (account_id = '' OR account_id IS NULL)", orgID).Count(&sum.UncategorisedTxns)
	s.db.Model(&BankTransaction{}).Where("organization_id = ? AND is_reconciled = ?", orgID, false).Count(&sum.UnreconciledTxns)
	if tb, err := s.GetTrialBalance(orgID, t.AddDate(100, 0, 0)); err == nil {
		sum.LedgerBalanced = tb.Balanced
	}
	return sum, nil
}

// GetDashboard is kept for backwards compatibility.
func (s *Service) GetDashboard(orgID string) (*Dashboard, error) {
	var d Dashboard
	accounts, err := s.activeAccounts(orgID)
	if err != nil {
		return nil, err
	}
	d.AccountsCount = int64(len(accounts))
	sums, _ := s.ledgerSums(orgID, time.Time{}, time.Time{})
	var dr, cr float64
	for _, a := range accounts {
		sum := sums[a.ID]
		dr += sum.Debit
		cr += sum.Credit
		b := signedBalance(a.AccountType, sum)
		switch a.AccountType {
		case TypeAsset:
			d.TotalAssets += b
		case TypeLiability:
			d.TotalLiabilities += b
		case TypeRevenue:
			d.TotalRevenue += b
		case TypeExpense:
			d.TotalExpenses += b
		}
	}
	d.TotalAssets, d.TotalLiabilities, d.TotalRevenue, d.TotalExpenses = r2(d.TotalAssets), r2(d.TotalLiabilities), r2(d.TotalRevenue), r2(d.TotalExpenses)
	d.NetIncome = r2(d.TotalRevenue - d.TotalExpenses)
	d.TrialBalanceSum = r2(dr - cr)
	s.db.Model(&Bill{}).Where("organization_id = ? AND status IN ?", orgID, []string{"unpaid", "partial"}).Count(&d.PendingBills)
	var cash float64
	s.db.Model(&BankAccount{}).Where("organization_id = ? AND is_active = ? AND account_type <> ?", orgID, true, "credit_card").
		Select("COALESCE(SUM(balance),0)").Row().Scan(&cash)
	d.CashBalance = r2(cash)
	s.db.Model(&GeneralLedger{}).Where("organization_id = ?", orgID).Count(&d.LedgerEntries)
	var last GeneralLedger
	if err := s.db.Where("organization_id = ?", orgID).Order("posted_at DESC").First(&last).Error; err == nil {
		d.LastPostedEntry = &last.PostedAt
	}
	return &d, nil
}

// ---------------------------------------------------------------------------
// Legacy invoices (the invoicing module is the primary AR system)

func (s *Service) CreateInvoice(invoice *Invoice) error {
	invoice.ID = uuid.NewString()
	invoice.InvoiceNumber = nextNumber(s.db, &Invoice{}, "invoice_number", invoice.OrganizationID, "INV")
	invoice.CreatedAt, invoice.UpdatedAt = time.Now(), time.Now()
	var sub float64
	for i := range invoice.LineItems {
		li := &invoice.LineItems[i]
		li.ID, li.InvoiceID = uuid.NewString(), invoice.ID
		li.LineTotal = r2(float64(li.Quantity) * li.UnitPrice)
		li.CreatedAt, li.UpdatedAt = time.Now(), time.Now()
		sub += li.LineTotal
	}
	if len(invoice.LineItems) > 0 {
		invoice.SubTotal = r2(sub)
	}
	invoice.TaxAmount = r2(invoice.TaxAmount)
	invoice.Total = r2(invoice.SubTotal + invoice.TaxAmount)
	return s.db.Create(invoice).Error
}

func (s *Service) GetInvoices(orgID string) ([]Invoice, error) {
	var invoices []Invoice
	err := s.db.Where("organization_id = ?", orgID).Preload("LineItems").Order("created_at DESC").Find(&invoices).Error
	return invoices, err
}
