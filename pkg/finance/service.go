package finance

import (
	"fmt"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"time"
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// InitializeDefaultAccounts creates standard chart of accounts
func (s *Service) InitializeDefaultAccounts(orgID string) error {
	accounts := []ChartOfAccount{
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "1000", Name: "Cash", AccountType: "Asset", SubType: "Current Asset"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "1200", Name: "Accounts Receivable", AccountType: "Asset", SubType: "Current Asset"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "1500", Name: "Equipment", AccountType: "Asset", SubType: "Fixed Asset"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "2000", Name: "Accounts Payable", AccountType: "Liability", SubType: "Current Liability"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "3000", Name: "Owner's Equity", AccountType: "Equity", SubType: "Capital"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "4000", Name: "Service Revenue", AccountType: "Revenue", SubType: "Operating Revenue"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "5000", Name: "Operating Expenses", AccountType: "Expense", SubType: "Operating Expense"},
		{ID: uuid.New().String(), OrganizationID: orgID, Code: "5100", Name: "Rent Expense", AccountType: "Expense", SubType: "Operating Expense"},
	}

	return s.db.Create(&accounts).Error
}

func (s *Service) GetChartOfAccounts(orgID string) ([]ChartOfAccount, error) {
	var accounts []ChartOfAccount
	err := s.db.Where("organization_id = ?", orgID).Order("code").Find(&accounts).Error
	return accounts, err
}

func (s *Service) CreateAccount(account *ChartOfAccount) error {
	account.ID = uuid.New().String()
	account.CreatedAt = time.Now()
	account.UpdatedAt = time.Now()
	return s.db.Create(account).Error
}

func (s *Service) CreateJournalEntry(entry *JournalEntry) error {
	// Start transaction for journal entry posting
	tx := s.db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	entry.ID = uuid.New().String()
	entry.EntryNumber = fmt.Sprintf("JE-%d", time.Now().Unix())
	entry.CreatedAt = time.Now()
	entry.UpdatedAt = time.Now()

	// Prepare line items
	for i := range entry.LineItems {
		entry.LineItems[i].ID = uuid.New().String()
		entry.LineItems[i].JournalEntryID = entry.ID
		entry.LineItems[i].CreatedAt = time.Now()
		entry.LineItems[i].UpdatedAt = time.Now()
	}

	// Create journal entry
	if err := tx.Create(entry).Error; err != nil {
		tx.Rollback()
		return err
	}

	// Post to General Ledger and update account balances
	if entry.Status == "posted" {
		if err := s.postToGeneralLedger(tx, entry, "system"); err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit().Error
}

// PostJournalEntry posts a draft journal entry to the general ledger
func (s *Service) PostJournalEntry(entryID, userID string) error {
	tx := s.db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var entry JournalEntry
	if err := tx.Preload("LineItems").First(&entry, "id = ?", entryID).Error; err != nil {
		tx.Rollback()
		return err
	}

	if entry.Status != "draft" {
		tx.Rollback()
		return fmt.Errorf("journal entry is already %s", entry.Status)
	}

	// Update entry status
	entry.Status = "posted"
	entry.UpdatedAt = time.Now()
	if err := tx.Save(&entry).Error; err != nil {
		tx.Rollback()
		return err
	}

	// Post to General Ledger
	if err := s.postToGeneralLedger(tx, &entry, userID); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

// postToGeneralLedger posts journal entry to general ledger with running balances
func (s *Service) postToGeneralLedger(tx *gorm.DB, entry *JournalEntry, userID string) error {
	for _, lineItem := range entry.LineItems {
		// Get current account balance
		var currentBalance AccountBalance
		if err := tx.Where("organization_id = ? AND account_id = ?", entry.OrganizationID, lineItem.ChartOfAccountID).First(&currentBalance).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				// Create initial balance record
				currentBalance = AccountBalance{
					ID:             uuid.New().String(),
					OrganizationID: entry.OrganizationID,
					AccountID:      lineItem.ChartOfAccountID,
					Balance:        0,
					DebitBalance:   0,
					CreditBalance:  0,
					LastUpdated:    time.Now(),
					UpdatedBy:      userID,
				}
			} else {
				return err
			}
		}

		// Calculate new running balance
		var newBalance float64
		if lineItem.DebitAmount > 0 {
			newBalance = currentBalance.Balance + lineItem.DebitAmount
			currentBalance.DebitBalance += lineItem.DebitAmount
		} else {
			newBalance = currentBalance.Balance - lineItem.CreditAmount
			currentBalance.CreditBalance += lineItem.CreditAmount
		}

		// Create general ledger entry
		ledgerEntry := GeneralLedger{
			ID:              uuid.New().String(),
			OrganizationID:  entry.OrganizationID,
			AccountID:       lineItem.ChartOfAccountID,
			JournalEntryID:  entry.ID,
			TransactionDate: entry.Date,
			Description:     lineItem.Description,
			Reference:       entry.Reference,
			DebitAmount:     lineItem.DebitAmount,
			CreditAmount:    lineItem.CreditAmount,
			RunningBalance:  newBalance,
			PostedBy:        userID,
			PostedAt:        time.Now(),
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		if err := tx.Create(&ledgerEntry).Error; err != nil {
			return err
		}

		// Update account balance
		currentBalance.Balance = newBalance
		currentBalance.LastUpdated = time.Now()
		currentBalance.UpdatedBy = userID

		if err := tx.Save(&currentBalance).Error; err != nil {
			return err
		}

		// Update chart of account balances
		var account ChartOfAccount
		if err := tx.First(&account, "id = ?", lineItem.ChartOfAccountID).Error; err == nil {
			account.DebitBalance = currentBalance.DebitBalance
			account.CreditBalance = currentBalance.CreditBalance
			account.UpdatedAt = time.Now()
			tx.Save(&account)
		}

		// Create audit trail
		s.createAuditTrail(tx, entry.OrganizationID, "general_ledger", ledgerEntry.ID, "CREATE", "", "", userID)
	}

	return nil
}

// GetTrialBalance generates trial balance report
func (s *Service) GetTrialBalance(orgID string, asOfDate time.Time) ([]TrialBalance, error) {
	var results []TrialBalance

	query := `
		SELECT
			coa.code as account_code,
			coa.name as account_name,
			coa.account_type,
			COALESCE(ab.debit_balance, 0) as debit_balance,
			COALESCE(ab.credit_balance, 0) as credit_balance
		FROM chart_of_accounts coa
		LEFT JOIN account_balances ab ON coa.id = ab.account_id
		WHERE coa.organization_id = ? AND coa.is_active = ?
		ORDER BY coa.code
	`

	err := s.db.Raw(query, orgID, true).Scan(&results).Error
	return results, err
}

// GetProfitLoss generates P&L statement
func (s *Service) GetProfitLoss(orgID string, startDate, endDate time.Time) (*ProfitLoss, error) {
	pl := &ProfitLoss{
		Period: fmt.Sprintf("%s to %s", startDate.Format("2006-01-02"), endDate.Format("2006-01-02")),
	}

	// Get Revenue accounts
	revenueQuery := `
		SELECT coa.code as account_code, coa.name as account_name,
			   COALESCE(SUM(gl.credit_amount - gl.debit_amount), 0) as amount
		FROM chart_of_accounts coa
		LEFT JOIN general_ledgers gl ON coa.id = gl.account_id
			AND gl.organization_id = ? AND gl.transaction_date BETWEEN ? AND ?
		WHERE coa.organization_id = ? AND coa.account_type = 'Revenue'
		GROUP BY coa.id, coa.code, coa.name
		ORDER BY coa.code
	`
	s.db.Raw(revenueQuery, orgID, startDate, endDate, orgID).Scan(&pl.Revenue)

	// Get Expense accounts
	expenseQuery := `
		SELECT coa.code as account_code, coa.name as account_name,
			   COALESCE(SUM(gl.debit_amount - gl.credit_amount), 0) as amount
		FROM chart_of_accounts coa
		LEFT JOIN general_ledgers gl ON coa.id = gl.account_id
			AND gl.organization_id = ? AND gl.transaction_date BETWEEN ? AND ?
		WHERE coa.organization_id = ? AND coa.account_type = 'Expense'
		GROUP BY coa.id, coa.code, coa.name
		ORDER BY coa.code
	`
	s.db.Raw(expenseQuery, orgID, startDate, endDate, orgID).Scan(&pl.Expenses)

	// Calculate totals
	for _, rev := range pl.Revenue {
		pl.TotalRevenue += rev.Amount
	}
	for _, exp := range pl.Expenses {
		pl.TotalExpenses += exp.Amount
	}
	pl.NetIncome = pl.TotalRevenue - pl.TotalExpenses

	return pl, nil
}

// GetBalanceSheet generates balance sheet
func (s *Service) GetBalanceSheet(orgID string, asOfDate time.Time) (*BalanceSheet, error) {
	bs := &BalanceSheet{
		AsOfDate: asOfDate,
	}

	// Get Asset accounts
	assetQuery := `
		SELECT coa.code as account_code, coa.name as account_name,
			   COALESCE(ab.debit_balance - ab.credit_balance, 0) as amount
		FROM chart_of_accounts coa
		LEFT JOIN account_balances ab ON coa.id = ab.account_id
		WHERE coa.organization_id = ? AND coa.account_type = 'Asset' AND coa.is_active = ?
		ORDER BY coa.code
	`
	var assetLines []FinancialReportLine
	s.db.Raw(assetQuery, orgID, true).Scan(&assetLines)

	// Categorize assets (simplified - all as current for now)
	bs.Assets.Current = assetLines
	for _, asset := range assetLines {
		bs.Assets.Total += asset.Amount
	}
	bs.TotalAssets = bs.Assets.Total

	// Get Liability accounts
	liabilityQuery := `
		SELECT coa.code as account_code, coa.name as account_name,
			   COALESCE(ab.credit_balance - ab.debit_balance, 0) as amount
		FROM chart_of_accounts coa
		LEFT JOIN account_balances ab ON coa.id = ab.account_id
		WHERE coa.organization_id = ? AND coa.account_type = 'Liability' AND coa.is_active = ?
		ORDER BY coa.code
	`
	var liabilityLines []FinancialReportLine
	s.db.Raw(liabilityQuery, orgID, true).Scan(&liabilityLines)
	bs.Liabilities.Current = liabilityLines
	for _, liability := range liabilityLines {
		bs.Liabilities.Total += liability.Amount
	}

	// Get Equity accounts
	equityQuery := `
		SELECT coa.code as account_code, coa.name as account_name,
			   COALESCE(ab.credit_balance - ab.debit_balance, 0) as amount
		FROM chart_of_accounts coa
		LEFT JOIN account_balances ab ON coa.id = ab.account_id
		WHERE coa.organization_id = ? AND coa.account_type = 'Equity' AND coa.is_active = ?
		ORDER BY coa.code
	`
	var equityLines []FinancialReportLine
	s.db.Raw(equityQuery, orgID, true).Scan(&equityLines)
	bs.Equity.Current = equityLines
	for _, equity := range equityLines {
		bs.Equity.Total += equity.Amount
	}

	bs.TotalLiabEq = bs.Liabilities.Total + bs.Equity.Total

	return bs, nil
}

// GetGeneralLedger gets ledger entries for an account
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
		query = query.Where("transaction_date <= ?", endDate)
	}

	err := query.Preload("ChartOfAccount").Preload("JournalEntry").
		Order("transaction_date DESC, created_at DESC").Find(&entries).Error

	return entries, err
}

// createAuditTrail creates audit trail entries
func (s *Service) createAuditTrail(tx *gorm.DB, orgID, tableName, recordID, action, oldValue, newValue, userID string) {
	audit := AuditTrail{
		ID:             uuid.New().String(),
		OrganizationID: orgID,
		TableName:      tableName,
		RecordID:       recordID,
		Action:         action,
		OldValue:       oldValue,
		NewValue:       newValue,
		UserID:         userID,
		Timestamp:      time.Now(),
	}
	tx.Create(&audit)
}

// Invoice methods
func (s *Service) CreateInvoice(invoice *Invoice) error {
	invoice.ID = uuid.New().String()
	invoice.InvoiceNumber = fmt.Sprintf("INV-%d", time.Now().Unix())
	invoice.CreatedAt = time.Now()
	invoice.UpdatedAt = time.Now()

	for i := range invoice.LineItems {
		invoice.LineItems[i].ID = uuid.New().String()
		invoice.LineItems[i].InvoiceID = invoice.ID
		invoice.LineItems[i].CreatedAt = time.Now()
		invoice.LineItems[i].UpdatedAt = time.Now()
	}

	return s.db.Create(invoice).Error
}

func (s *Service) GetInvoices(orgID string) ([]Invoice, error) {
	var invoices []Invoice
	err := s.db.Where("organization_id = ?", orgID).Preload("LineItems").Find(&invoices).Error
	return invoices, err
}

// Bill methods
func (s *Service) CreateBill(bill *Bill) error {
	bill.ID = uuid.New().String()
	bill.BillNumber = fmt.Sprintf("BILL-%d", time.Now().Unix())
	bill.CreatedAt = time.Now()
	bill.UpdatedAt = time.Now()

	for i := range bill.LineItems {
		bill.LineItems[i].ID = uuid.New().String()
		bill.LineItems[i].BillID = bill.ID
		bill.LineItems[i].CreatedAt = time.Now()
		bill.LineItems[i].UpdatedAt = time.Now()
	}

	return s.db.Create(bill).Error
}

func (s *Service) GetBills(orgID string) ([]Bill, error) {
	var bills []Bill
	err := s.db.Where("organization_id = ?", orgID).Preload("LineItems").Find(&bills).Error
	return bills, err
}

// Vendor methods
func (s *Service) CreateVendor(vendor *Vendor) error {
	vendor.ID = uuid.New().String()
	vendor.CreatedAt = time.Now()
	vendor.UpdatedAt = time.Now()
	return s.db.Create(vendor).Error
}

func (s *Service) GetVendors(orgID string) ([]Vendor, error) {
	var vendors []Vendor
	err := s.db.Where("organization_id = ? AND is_active = ?", orgID, true).Find(&vendors).Error
	return vendors, err
}

// Payment methods
func (s *Service) CreatePayment(payment *Payment) error {
	payment.ID = uuid.New().String()
	payment.CreatedAt = time.Now()
	payment.UpdatedAt = time.Now()
	return s.db.Create(payment).Error
}

// Bank Account methods
func (s *Service) CreateBankAccount(account *BankAccount) error {
	account.ID = uuid.New().String()
	account.CreatedAt = time.Now()
	account.UpdatedAt = time.Now()
	return s.db.Create(account).Error
}

func (s *Service) GetBankAccounts(orgID string) ([]BankAccount, error) {
	var accounts []BankAccount
	err := s.db.Where("organization_id = ? AND is_active = ?", orgID, true).Find(&accounts).Error
	return accounts, err
}

func (s *Service) GetDashboard(orgID string) (*Dashboard, error) {
	var dashboard Dashboard

	// Get accounts count
	s.db.Model(&ChartOfAccount{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&dashboard.AccountsCount)

	// Calculate totals by account type from account balances
	var assets, liabilities, revenue, expenses float64

	s.db.Model(&AccountBalance{}).
		Joins("JOIN chart_of_accounts ON account_balances.account_id = chart_of_accounts.id").
		Where("account_balances.organization_id = ? AND chart_of_accounts.account_type = ?", orgID, "Asset").
		Select("COALESCE(SUM(debit_balance - credit_balance), 0)").Row().Scan(&assets)

	s.db.Model(&AccountBalance{}).
		Joins("JOIN chart_of_accounts ON account_balances.account_id = chart_of_accounts.id").
		Where("account_balances.organization_id = ? AND chart_of_accounts.account_type = ?", orgID, "Liability").
		Select("COALESCE(SUM(credit_balance - debit_balance), 0)").Row().Scan(&liabilities)

	s.db.Model(&AccountBalance{}).
		Joins("JOIN chart_of_accounts ON account_balances.account_id = chart_of_accounts.id").
		Where("account_balances.organization_id = ? AND chart_of_accounts.account_type = ?", orgID, "Revenue").
		Select("COALESCE(SUM(credit_balance - debit_balance), 0)").Row().Scan(&revenue)

	s.db.Model(&AccountBalance{}).
		Joins("JOIN chart_of_accounts ON account_balances.account_id = chart_of_accounts.id").
		Where("account_balances.organization_id = ? AND chart_of_accounts.account_type = ?", orgID, "Expense").
		Select("COALESCE(SUM(debit_balance - credit_balance), 0)").Row().Scan(&expenses)

	dashboard.TotalAssets = assets
	dashboard.TotalLiabilities = liabilities
	dashboard.TotalRevenue = revenue
	dashboard.TotalExpenses = expenses
	dashboard.NetIncome = revenue - expenses

	// Get invoice and bill counts
	s.db.Model(&Invoice{}).Where("organization_id = ? AND status IN (?)", orgID, []string{"draft", "sent"}).Count(&dashboard.PendingInvoices)
	s.db.Model(&Invoice{}).Where("organization_id = ? AND status = ?", orgID, "overdue").Count(&dashboard.OverdueInvoices)
	s.db.Model(&Bill{}).Where("organization_id = ? AND status = ?", orgID, "unpaid").Count(&dashboard.PendingBills)

	// Calculate cash balance
	var cashBalance float64
	s.db.Model(&BankAccount{}).Where("organization_id = ? AND is_active = ?", orgID, true).
		Select("COALESCE(SUM(balance), 0)").Row().Scan(&cashBalance)
	dashboard.CashBalance = cashBalance

	// Enhanced ledger metrics
	s.db.Model(&GeneralLedger{}).Where("organization_id = ?", orgID).Count(&dashboard.LedgerEntries)

	// Get last posted entry date
	var lastEntry GeneralLedger
	if err := s.db.Where("organization_id = ?", orgID).Order("posted_at DESC").First(&lastEntry).Error; err == nil {
		dashboard.LastPostedEntry = &lastEntry.PostedAt
	}

	// Calculate trial balance sum (should be 0 if balanced)
	var totalDebits, totalCredits float64
	s.db.Model(&AccountBalance{}).Where("organization_id = ?", orgID).
		Select("COALESCE(SUM(debit_balance), 0)").Row().Scan(&totalDebits)
	s.db.Model(&AccountBalance{}).Where("organization_id = ?", orgID).
		Select("COALESCE(SUM(credit_balance), 0)").Row().Scan(&totalCredits)
	dashboard.TrialBalanceSum = totalDebits - totalCredits

	return &dashboard, nil
}