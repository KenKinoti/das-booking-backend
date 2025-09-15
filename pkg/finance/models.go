package finance

import (
	"time"
)

// ChartOfAccount represents the chart of accounts for double-entry bookkeeping
type ChartOfAccount struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Code           string    `json:"code" gorm:"unique;not null"`
	Name           string    `json:"name" gorm:"not null"`
	AccountType    string    `json:"account_type" gorm:"not null"` // Asset, Liability, Equity, Revenue, Expense
	SubType        string    `json:"sub_type"`                     // Current Asset, Fixed Asset, etc.
	DebitBalance   float64   `json:"debit_balance" gorm:"default:0"`
	CreditBalance  float64   `json:"credit_balance" gorm:"default:0"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// JournalEntry represents a journal entry in double-entry bookkeeping
type JournalEntry struct {
	ID             string               `json:"id" gorm:"primaryKey"`
	OrganizationID string               `json:"organization_id" gorm:"not null"`
	EntryNumber    string               `json:"entry_number" gorm:"unique;not null"`
	Date           time.Time            `json:"date" gorm:"not null"`
	Description    string               `json:"description"`
	Reference      string               `json:"reference"`
	TotalDebit     float64              `json:"total_debit" gorm:"not null"`
	TotalCredit    float64              `json:"total_credit" gorm:"not null"`
	Status         string               `json:"status" gorm:"default:'draft'"` // draft, posted
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	LineItems      []JournalEntryLine   `json:"line_items" gorm:"foreignKey:JournalEntryID"`
}

// JournalEntryLine represents individual line items in a journal entry
type JournalEntryLine struct {
	ID              string    `json:"id" gorm:"primaryKey"`
	JournalEntryID  string    `json:"journal_entry_id" gorm:"not null"`
	ChartOfAccountID string   `json:"chart_of_account_id" gorm:"not null"`
	Description     string    `json:"description"`
	DebitAmount     float64   `json:"debit_amount" gorm:"default:0"`
	CreditAmount    float64   `json:"credit_amount" gorm:"default:0"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ChartOfAccount  ChartOfAccount `json:"chart_of_account" gorm:"foreignKey:ChartOfAccountID"`
}

// Invoice represents customer invoices for Accounts Receivable
type Invoice struct {
	ID             string          `json:"id" gorm:"primaryKey"`
	OrganizationID string          `json:"organization_id" gorm:"not null"`
	InvoiceNumber  string          `json:"invoice_number" gorm:"unique;not null"`
	CustomerID     string          `json:"customer_id" gorm:"not null"`
	IssueDate      time.Time       `json:"issue_date" gorm:"not null"`
	DueDate        time.Time       `json:"due_date" gorm:"not null"`
	Status         string          `json:"status" gorm:"default:'draft'"` // draft, sent, paid, overdue, cancelled
	SubTotal       float64         `json:"subtotal" gorm:"not null"`
	TaxAmount      float64         `json:"tax_amount" gorm:"default:0"`
	Total          float64         `json:"total" gorm:"not null"`
	PaidAmount     float64         `json:"paid_amount" gorm:"default:0"`
	Notes          string          `json:"notes"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	LineItems      []InvoiceLineItem `json:"line_items" gorm:"foreignKey:InvoiceID"`
	Payments       []Payment       `json:"payments" gorm:"foreignKey:InvoiceID"`
}

// InvoiceLineItem represents individual line items in an invoice
type InvoiceLineItem struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	InvoiceID   string    `json:"invoice_id" gorm:"not null"`
	ProductID   *string   `json:"product_id"`
	ServiceID   *string   `json:"service_id"`
	Description string    `json:"description" gorm:"not null"`
	Quantity    int       `json:"quantity" gorm:"not null"`
	UnitPrice   float64   `json:"unit_price" gorm:"not null"`
	LineTotal   float64   `json:"line_total" gorm:"not null"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Bill represents vendor bills for Accounts Payable
type Bill struct {
	ID             string          `json:"id" gorm:"primaryKey"`
	OrganizationID string          `json:"organization_id" gorm:"not null"`
	BillNumber     string          `json:"bill_number" gorm:"unique;not null"`
	VendorID       string          `json:"vendor_id" gorm:"not null"`
	BillDate       time.Time       `json:"bill_date" gorm:"not null"`
	DueDate        time.Time       `json:"due_date" gorm:"not null"`
	Status         string          `json:"status" gorm:"default:'unpaid'"` // unpaid, paid, overdue
	SubTotal       float64         `json:"subtotal" gorm:"not null"`
	TaxAmount      float64         `json:"tax_amount" gorm:"default:0"`
	Total          float64         `json:"total" gorm:"not null"`
	PaidAmount     float64         `json:"paid_amount" gorm:"default:0"`
	Notes          string          `json:"notes"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	LineItems      []BillLineItem  `json:"line_items" gorm:"foreignKey:BillID"`
}

// BillLineItem represents individual line items in a bill
type BillLineItem struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	BillID      string    `json:"bill_id" gorm:"not null"`
	ProductID   *string   `json:"product_id"`
	Description string    `json:"description" gorm:"not null"`
	Quantity    int       `json:"quantity" gorm:"not null"`
	UnitPrice   float64   `json:"unit_price" gorm:"not null"`
	LineTotal   float64   `json:"line_total" gorm:"not null"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Vendor represents suppliers/vendors
type Vendor struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Email          string    `json:"email"`
	Phone          string    `json:"phone"`
	Address        string    `json:"address"`
	TaxID          string    `json:"tax_id"`
	PaymentTerms   string    `json:"payment_terms" gorm:"default:'NET30'"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Payment represents both customer payments (AR) and vendor payments (AP)
type Payment struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Type           string    `json:"type" gorm:"not null"` // customer_payment, vendor_payment
	InvoiceID      *string   `json:"invoice_id"`
	BillID         *string   `json:"bill_id"`
	Amount         float64   `json:"amount" gorm:"not null"`
	PaymentDate    time.Time `json:"payment_date" gorm:"not null"`
	PaymentMethod  string    `json:"payment_method" gorm:"not null"` // cash, check, credit_card, bank_transfer
	Reference      string    `json:"reference"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// BankAccount represents company bank accounts
type BankAccount struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	AccountName    string    `json:"account_name" gorm:"not null"`
	AccountNumber  string    `json:"account_number" gorm:"not null"`
	BankName       string    `json:"bank_name" gorm:"not null"`
	AccountType    string    `json:"account_type" gorm:"not null"` // checking, savings, credit
	Balance        float64   `json:"balance" gorm:"default:0"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// BankTransaction represents bank account transactions
type BankTransaction struct {
	ID            string    `json:"id" gorm:"primaryKey"`
	BankAccountID string    `json:"bank_account_id" gorm:"not null"`
	Date          time.Time `json:"date" gorm:"not null"`
	Description   string    `json:"description" gorm:"not null"`
	Amount        float64   `json:"amount" gorm:"not null"` // positive for deposits, negative for withdrawals
	Balance       float64   `json:"balance" gorm:"not null"`
	Category      string    `json:"category"`
	IsReconciled  bool      `json:"is_reconciled" gorm:"default:false"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// GeneralLedger represents the complete ledger with all transactions
type GeneralLedger struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null;index"`
	AccountID      string    `json:"account_id" gorm:"not null;index"`
	JournalEntryID string    `json:"journal_entry_id" gorm:"not null;index"`
	TransactionDate time.Time `json:"transaction_date" gorm:"not null;index"`
	Description    string    `json:"description" gorm:"not null"`
	Reference      string    `json:"reference"`
	DebitAmount    float64   `json:"debit_amount" gorm:"default:0"`
	CreditAmount   float64   `json:"credit_amount" gorm:"default:0"`
	RunningBalance float64   `json:"running_balance" gorm:"not null"`
	PostedBy       string    `json:"posted_by" gorm:"not null"` // User ID
	PostedAt       time.Time `json:"posted_at" gorm:"not null"`
	Reversed       bool      `json:"reversed" gorm:"default:false"`
	ReversedBy     *string   `json:"reversed_by"`
	ReversedAt     *time.Time `json:"reversed_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ChartOfAccount ChartOfAccount `json:"chart_of_account" gorm:"foreignKey:AccountID"`
	JournalEntry   JournalEntry `json:"journal_entry" gorm:"foreignKey:JournalEntryID"`
}

// AccountBalance represents real-time account balances
type AccountBalance struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null;index"`
	AccountID      string    `json:"account_id" gorm:"uniqueIndex:idx_org_account;not null"`
	Balance        float64   `json:"balance" gorm:"not null"`
	DebitBalance   float64   `json:"debit_balance" gorm:"default:0"`
	CreditBalance  float64   `json:"credit_balance" gorm:"default:0"`
	LastUpdated    time.Time `json:"last_updated" gorm:"not null"`
	UpdatedBy      string    `json:"updated_by" gorm:"not null"`
	ChartOfAccount ChartOfAccount `json:"chart_of_account" gorm:"foreignKey:AccountID"`
}

// AuditTrail represents all system changes for compliance
type AuditTrail struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null;index"`
	TableName      string    `json:"table_name" gorm:"not null;index"`
	RecordID       string    `json:"record_id" gorm:"not null;index"`
	Action         string    `json:"action" gorm:"not null"` // CREATE, UPDATE, DELETE
	FieldName      string    `json:"field_name"`
	OldValue       string    `json:"old_value"`
	NewValue       string    `json:"new_value"`
	UserID         string    `json:"user_id" gorm:"not null"`
	IPAddress      string    `json:"ip_address"`
	UserAgent      string    `json:"user_agent"`
	Timestamp      time.Time `json:"timestamp" gorm:"not null;index"`
}

// TrialBalance represents trial balance report data
type TrialBalance struct {
	AccountCode    string  `json:"account_code"`
	AccountName    string  `json:"account_name"`
	AccountType    string  `json:"account_type"`
	DebitBalance   float64 `json:"debit_balance"`
	CreditBalance  float64 `json:"credit_balance"`
}

// ProfitLoss represents P&L statement data
type ProfitLoss struct {
	Period         string                 `json:"period"`
	Revenue        []FinancialReportLine  `json:"revenue"`
	CostOfSales    []FinancialReportLine  `json:"cost_of_sales"`
	GrossProfit    float64                `json:"gross_profit"`
	Expenses       []FinancialReportLine  `json:"expenses"`
	NetIncome      float64                `json:"net_income"`
	TotalRevenue   float64                `json:"total_revenue"`
	TotalExpenses  float64                `json:"total_expenses"`
}

// BalanceSheet represents balance sheet data
type BalanceSheet struct {
	AsOfDate       time.Time              `json:"as_of_date"`
	Assets         BalanceSheetSection    `json:"assets"`
	Liabilities    BalanceSheetSection    `json:"liabilities"`
	Equity         BalanceSheetSection    `json:"equity"`
	TotalAssets    float64                `json:"total_assets"`
	TotalLiabEq    float64                `json:"total_liabilities_equity"`
}

// BalanceSheetSection represents a section in balance sheet
type BalanceSheetSection struct {
	Current    []FinancialReportLine  `json:"current"`
	NonCurrent []FinancialReportLine  `json:"non_current"`
	Total      float64                `json:"total"`
}

// FinancialReportLine represents a line item in financial reports
type FinancialReportLine struct {
	AccountCode string  `json:"account_code"`
	AccountName string  `json:"account_name"`
	Amount      float64 `json:"amount"`
}

// CashFlowStatement represents cash flow statement data
type CashFlowStatement struct {
	Period              string                `json:"period"`
	OperatingActivities []CashFlowActivity   `json:"operating_activities"`
	InvestingActivities []CashFlowActivity   `json:"investing_activities"`
	FinancingActivities []CashFlowActivity   `json:"financing_activities"`
	NetOperatingCash    float64              `json:"net_operating_cash"`
	NetInvestingCash    float64              `json:"net_investing_cash"`
	NetFinancingCash    float64              `json:"net_financing_cash"`
	NetCashFlow         float64              `json:"net_cash_flow"`
	BeginningCash       float64              `json:"beginning_cash"`
	EndingCash          float64              `json:"ending_cash"`
}

// CashFlowActivity represents a cash flow activity
type CashFlowActivity struct {
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
}

// AccountingPeriod represents fiscal periods
type AccountingPeriod struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	StartDate      time.Time `json:"start_date" gorm:"not null"`
	EndDate        time.Time `json:"end_date" gorm:"not null"`
	Status         string    `json:"status" gorm:"default:'open'"` // open, closed, locked
	ClosedBy       *string   `json:"closed_by"`
	ClosedAt       *time.Time `json:"closed_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Dashboard represents financial dashboard metrics with enhanced ledger data
type Dashboard struct {
	TotalAssets        float64 `json:"total_assets"`
	TotalLiabilities   float64 `json:"total_liabilities"`
	TotalRevenue       float64 `json:"total_revenue"`
	TotalExpenses      float64 `json:"total_expenses"`
	NetIncome          float64 `json:"net_income"`
	AccountsCount      int64   `json:"accounts_count"`
	PendingInvoices    int64   `json:"pending_invoices"`
	OverdueInvoices    int64   `json:"overdue_invoices"`
	PendingBills       int64   `json:"pending_bills"`
	CashBalance        float64 `json:"cash_balance"`
	LedgerEntries      int64   `json:"ledger_entries"`
	LastPostedEntry    *time.Time `json:"last_posted_entry"`
	TrialBalanceSum    float64 `json:"trial_balance_sum"`
}