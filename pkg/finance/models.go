package finance

import (
	"time"
)

// Account types used by the chart of accounts.
const (
	TypeAsset     = "Asset"
	TypeLiability = "Liability"
	TypeEquity    = "Equity"
	TypeRevenue   = "Revenue"
	TypeExpense   = "Expense"
)

// ChartOfAccount represents the chart of accounts for double-entry bookkeeping.
// Codes are unique per organisation.
type ChartOfAccount struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null;uniqueIndex:idx_coa_org_code,priority:1"`
	Code           string    `json:"code" gorm:"not null;uniqueIndex:idx_coa_org_code,priority:2"`
	Name           string    `json:"name" gorm:"not null"`
	AccountType    string    `json:"account_type" gorm:"not null"` // Asset, Liability, Equity, Revenue, Expense
	SubType        string    `json:"sub_type"`                     // Current Asset, Fixed Asset, Cost of Sales …
	Description    string    `json:"description"`
	DebitBalance   float64   `json:"debit_balance" gorm:"default:0"`
	CreditBalance  float64   `json:"credit_balance" gorm:"default:0"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	IsSystem       bool      `json:"is_system" gorm:"default:false"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Computed (not stored)
	Balance     float64 `json:"balance" gorm:"-"`
	HasActivity bool    `json:"has_activity" gorm:"-"`
}

// JournalEntry represents a journal entry in double-entry bookkeeping.
type JournalEntry struct {
	ID             string             `json:"id" gorm:"primaryKey"`
	OrganizationID string             `json:"organization_id" gorm:"not null;index:idx_je_org_number,priority:1"`
	EntryNumber    string             `json:"entry_number" gorm:"not null;index:idx_je_org_number,priority:2"`
	Date           time.Time          `json:"date" gorm:"not null"`
	Description    string             `json:"description"`
	Reference      string             `json:"reference"`
	TotalDebit     float64            `json:"total_debit" gorm:"not null"`
	TotalCredit    float64            `json:"total_credit" gorm:"not null"`
	Status         string             `json:"status" gorm:"default:'draft'"`  // draft, posted, reversed
	Source         string             `json:"source" gorm:"default:'manual'"` // manual, bill, bill_payment, bank, opening_balance, reversal
	SourceID       string             `json:"source_id"`
	ReversalOf     string             `json:"reversal_of"`
	ReversedByID   string             `json:"reversed_by_id"`
	CreatedBy      string             `json:"created_by"`
	PostedAt       *time.Time         `json:"posted_at"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	LineItems      []JournalEntryLine `json:"line_items" gorm:"foreignKey:JournalEntryID"`
}

// JournalEntryLine represents individual line items in a journal entry.
type JournalEntryLine struct {
	ID               string         `json:"id" gorm:"primaryKey"`
	JournalEntryID   string         `json:"journal_entry_id" gorm:"not null;index"`
	ChartOfAccountID string         `json:"chart_of_account_id" gorm:"not null"`
	Description      string         `json:"description"`
	DebitAmount      float64        `json:"debit_amount" gorm:"default:0"`
	CreditAmount     float64        `json:"credit_amount" gorm:"default:0"`
	SortOrder        int            `json:"sort_order" gorm:"default:0"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	ChartOfAccount   ChartOfAccount `json:"chart_of_account" gorm:"foreignKey:ChartOfAccountID"`
}

// Invoice represents customer invoices for Accounts Receivable (legacy; the
// invoicing module is the primary AR system).
type Invoice struct {
	ID             string            `json:"id" gorm:"primaryKey"`
	OrganizationID string            `json:"organization_id" gorm:"not null"`
	InvoiceNumber  string            `json:"invoice_number" gorm:"not null;index"`
	CustomerID     string            `json:"customer_id" gorm:"not null"`
	IssueDate      time.Time         `json:"issue_date" gorm:"not null"`
	DueDate        time.Time         `json:"due_date" gorm:"not null"`
	Status         string            `json:"status" gorm:"default:'draft'"` // draft, sent, paid, overdue, cancelled
	SubTotal       float64           `json:"subtotal" gorm:"not null"`
	TaxAmount      float64           `json:"tax_amount" gorm:"default:0"`
	Total          float64           `json:"total" gorm:"not null"`
	PaidAmount     float64           `json:"paid_amount" gorm:"default:0"`
	Notes          string            `json:"notes"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	LineItems      []InvoiceLineItem `json:"line_items" gorm:"foreignKey:InvoiceID"`
	Payments       []Payment         `json:"payments" gorm:"foreignKey:InvoiceID"`
}

// InvoiceLineItem represents individual line items in an invoice.
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

// Bill represents vendor bills for Accounts Payable.
type Bill struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null;index:idx_bill_org_number,priority:1"`
	BillNumber     string    `json:"bill_number" gorm:"not null;index:idx_bill_org_number,priority:2"`
	VendorID       string    `json:"vendor_id" gorm:"not null;index"`
	Reference      string    `json:"reference"` // supplier's invoice number
	BillDate       time.Time `json:"bill_date" gorm:"not null"`
	DueDate        time.Time `json:"due_date" gorm:"not null"`
	Status         string    `json:"status" gorm:"default:'unpaid'"` // draft, unpaid, partial, paid
	SubTotal       float64   `json:"subtotal" gorm:"not null"`
	TaxAmount      float64   `json:"tax_amount" gorm:"default:0"`
	Total          float64   `json:"total" gorm:"not null"`
	PaidAmount     float64   `json:"paid_amount" gorm:"default:0"`
	// Currency of the bill (defaults to the organisation's home currency).
	Currency       string `json:"currency" gorm:"type:varchar(3)"`
	Notes          string `json:"notes"`
	JournalEntryID string `json:"journal_entry_id"`
	// PurchaseOrderID links a supplier bill to the purchase order it charges
	// for, so the Profit & loss report counts that stock purchase once (the
	// bill) instead of twice (the received order and the bill).
	PurchaseOrderID string         `json:"purchase_order_id" gorm:"type:varchar(255);index"`
	PaidAt          *time.Time     `json:"paid_at"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	LineItems       []BillLineItem `json:"line_items" gorm:"foreignKey:BillID"`
	Payments        []Payment      `json:"payments,omitempty" gorm:"foreignKey:BillID"`

	// Computed
	VendorName          string  `json:"vendor_name" gorm:"-"`
	PurchaseOrderNumber string  `json:"purchase_order_number,omitempty" gorm:"-"`
	AmountDue           float64 `json:"amount_due" gorm:"-"`
	DisplayStatus       string  `json:"display_status" gorm:"-"`
	DaysOverdue         int     `json:"days_overdue" gorm:"-"`
}

// BillLineItem represents individual line items in a bill.
type BillLineItem struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	BillID      string    `json:"bill_id" gorm:"not null;index"`
	ProductID   *string   `json:"product_id"`
	AccountID   string    `json:"account_id"`
	Description string    `json:"description" gorm:"not null"`
	Quantity    float64   `json:"quantity" gorm:"not null"`
	UnitPrice   float64   `json:"unit_price" gorm:"not null"`
	TaxRate     float64   `json:"tax_rate" gorm:"default:0"`
	TaxAmount   float64   `json:"tax_amount" gorm:"default:0"`
	LineTotal   float64   `json:"line_total" gorm:"not null"`
	SortOrder   int       `json:"sort_order" gorm:"default:0"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Vendor represents suppliers/vendors.
type Vendor struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	OrganizationID   string    `json:"organization_id" gorm:"not null;index"`
	Name             string    `json:"name" gorm:"not null"`
	ContactName      string    `json:"contact_name"`
	Email            string    `json:"email"`
	Phone            string    `json:"phone"`
	Address          string    `json:"address"`
	TaxID            string    `json:"tax_id"`
	PaymentTerms     string    `json:"payment_terms" gorm:"default:'NET30'"`
	DefaultAccountID string    `json:"default_account_id"`
	Notes            string    `json:"notes"`
	IsActive         bool      `json:"is_active" gorm:"default:true"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// Computed
	Outstanding float64 `json:"outstanding" gorm:"-"`
	BillCount   int     `json:"bill_count" gorm:"-"`
}

// Payment represents both customer payments (AR) and vendor payments (AP).
type Payment struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Type           string    `json:"type" gorm:"not null"` // customer_payment, vendor_payment
	InvoiceID      *string   `json:"invoice_id"`
	BillID         *string   `json:"bill_id"`
	BankAccountID  *string   `json:"bank_account_id"`
	JournalEntryID string    `json:"journal_entry_id"`
	Amount         float64   `json:"amount" gorm:"not null"`
	PaymentDate    time.Time `json:"payment_date" gorm:"not null"`
	PaymentMethod  string    `json:"payment_method" gorm:"not null"` // cash, cheque, card, bank_transfer
	Reference      string    `json:"reference"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	BankAccountName string `json:"bank_account_name" gorm:"-"`
}

// BankAccount represents company bank accounts. Balance is the running
// statement balance (deposits positive, withdrawals negative).
type BankAccount struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	AccountName    string    `json:"account_name" gorm:"not null"`
	AccountNumber  string    `json:"account_number" gorm:"not null"`
	BSB            string    `json:"bsb"`
	BankName       string    `json:"bank_name" gorm:"not null"`
	AccountType    string    `json:"account_type" gorm:"not null"` // checking, savings, credit_card, cash
	Currency       string    `json:"currency" gorm:"default:'AUD'"`
	Balance        float64   `json:"balance" gorm:"default:0"`
	OpeningBalance float64   `json:"opening_balance" gorm:"default:0"`
	ChartAccountID string    `json:"chart_account_id"`
	Notes          string    `json:"notes"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Computed
	UnreconciledCount   int64      `json:"unreconciled_count" gorm:"-"`
	UncategorisedCount  int64      `json:"uncategorised_count" gorm:"-"`
	LastTransactionDate *time.Time `json:"last_transaction_date" gorm:"-"`
	ChartAccountCode    string     `json:"chart_account_code" gorm:"-"`
}

// BankTransaction represents bank account transactions.
type BankTransaction struct {
	ID             string     `json:"id" gorm:"primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"index"`
	BankAccountID  string     `json:"bank_account_id" gorm:"not null;index"`
	Date           time.Time  `json:"date" gorm:"not null"`
	Description    string     `json:"description" gorm:"not null"`
	Reference      string     `json:"reference"`
	Amount         float64    `json:"amount" gorm:"not null"` // positive for deposits, negative for withdrawals
	Balance        float64    `json:"balance" gorm:"not null"`
	Category       string     `json:"category"`
	AccountID      string     `json:"account_id"`
	JournalEntryID string     `json:"journal_entry_id"`
	Source         string     `json:"source" gorm:"default:'manual'"` // manual, bill_payment, opening_balance
	SourceID       string     `json:"source_id"`
	IsReconciled   bool       `json:"is_reconciled" gorm:"default:false"`
	ReconciledAt   *time.Time `json:"reconciled_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// Computed
	BankAccountName string `json:"bank_account_name" gorm:"-"`
	AccountCode     string `json:"account_code" gorm:"-"`
}

// GeneralLedger represents the complete ledger with all transactions.
type GeneralLedger struct {
	ID              string         `json:"id" gorm:"primaryKey"`
	OrganizationID  string         `json:"organization_id" gorm:"not null;index"`
	AccountID       string         `json:"account_id" gorm:"not null;index"`
	JournalEntryID  string         `json:"journal_entry_id" gorm:"not null;index"`
	TransactionDate time.Time      `json:"transaction_date" gorm:"not null;index"`
	Description     string         `json:"description" gorm:"not null"`
	Reference       string         `json:"reference"`
	DebitAmount     float64        `json:"debit_amount" gorm:"default:0"`
	CreditAmount    float64        `json:"credit_amount" gorm:"default:0"`
	RunningBalance  float64        `json:"running_balance" gorm:"not null"`
	PostedBy        string         `json:"posted_by" gorm:"not null"` // User ID
	PostedAt        time.Time      `json:"posted_at" gorm:"not null"`
	Reversed        bool           `json:"reversed" gorm:"default:false"`
	ReversedBy      *string        `json:"reversed_by"`
	ReversedAt      *time.Time     `json:"reversed_at"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	ChartOfAccount  ChartOfAccount `json:"chart_of_account" gorm:"foreignKey:AccountID"`
	JournalEntry    JournalEntry   `json:"journal_entry" gorm:"foreignKey:JournalEntryID"`
}

// AccountBalance represents real-time account balances.
type AccountBalance struct {
	ID             string         `json:"id" gorm:"primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"not null;index"`
	AccountID      string         `json:"account_id" gorm:"uniqueIndex:idx_org_account;not null"`
	Balance        float64        `json:"balance" gorm:"not null"`
	DebitBalance   float64        `json:"debit_balance" gorm:"default:0"`
	CreditBalance  float64        `json:"credit_balance" gorm:"default:0"`
	LastUpdated    time.Time      `json:"last_updated" gorm:"not null"`
	UpdatedBy      string         `json:"updated_by" gorm:"not null"`
	ChartOfAccount ChartOfAccount `json:"chart_of_account" gorm:"foreignKey:AccountID"`
}

// AuditTrail represents all system changes for compliance.
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

// TrialBalance represents a trial balance report row.
type TrialBalance struct {
	AccountID     string  `json:"account_id"`
	AccountCode   string  `json:"account_code"`
	AccountName   string  `json:"account_name"`
	AccountType   string  `json:"account_type"`
	DebitBalance  float64 `json:"debit_balance"`
	CreditBalance float64 `json:"credit_balance"`
}

// TrialBalanceReport wraps the trial balance rows with totals.
type TrialBalanceReport struct {
	AsOfDate    string         `json:"as_of_date"`
	Rows        []TrialBalance `json:"rows"`
	TotalDebit  float64        `json:"total_debit"`
	TotalCredit float64        `json:"total_credit"`
	Balanced    bool           `json:"balanced"`
}

// ProfitLoss represents P&L statement data.
type ProfitLoss struct {
	Period           string                `json:"period"`
	StartDate        string                `json:"start_date"`
	EndDate          string                `json:"end_date"`
	Revenue          []FinancialReportLine `json:"revenue"`
	CostOfSales      []FinancialReportLine `json:"cost_of_sales"`
	Expenses         []FinancialReportLine `json:"expenses"`
	TotalRevenue     float64               `json:"total_revenue"`
	TotalCostOfSales float64               `json:"total_cost_of_sales"`
	GrossProfit      float64               `json:"gross_profit"`
	TotalExpenses    float64               `json:"total_expenses"`
	NetIncome        float64               `json:"net_income"`
}

// BalanceSheet represents balance sheet data.
type BalanceSheet struct {
	AsOfDate    string              `json:"as_of_date"`
	Assets      BalanceSheetSection `json:"assets"`
	Liabilities BalanceSheetSection `json:"liabilities"`
	Equity      BalanceSheetSection `json:"equity"`
	TotalAssets float64             `json:"total_assets"`
	TotalLiabEq float64             `json:"total_liabilities_equity"`
	Balanced    bool                `json:"balanced"`
}

// BalanceSheetSection represents a section in balance sheet.
type BalanceSheetSection struct {
	Current    []FinancialReportLine `json:"current"`
	NonCurrent []FinancialReportLine `json:"non_current"`
	Total      float64               `json:"total"`
}

// FinancialReportLine represents a line item in financial reports.
type FinancialReportLine struct {
	AccountID   string  `json:"account_id"`
	AccountCode string  `json:"account_code"`
	AccountName string  `json:"account_name"`
	Amount      float64 `json:"amount"`
}

// CashFlowStatement represents cash flow statement data.
type CashFlowStatement struct {
	Period              string             `json:"period"`
	OperatingActivities []CashFlowActivity `json:"operating_activities"`
	InvestingActivities []CashFlowActivity `json:"investing_activities"`
	FinancingActivities []CashFlowActivity `json:"financing_activities"`
	NetOperatingCash    float64            `json:"net_operating_cash"`
	NetInvestingCash    float64            `json:"net_investing_cash"`
	NetFinancingCash    float64            `json:"net_financing_cash"`
	NetCashFlow         float64            `json:"net_cash_flow"`
	BeginningCash       float64            `json:"beginning_cash"`
	EndingCash          float64            `json:"ending_cash"`
}

// CashFlowActivity represents a cash flow activity.
type CashFlowActivity struct {
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
}

// AccountingPeriod represents fiscal periods.
type AccountingPeriod struct {
	ID             string     `json:"id" gorm:"primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"not null"`
	Name           string     `json:"name" gorm:"not null"`
	StartDate      time.Time  `json:"start_date" gorm:"not null"`
	EndDate        time.Time  `json:"end_date" gorm:"not null"`
	Status         string     `json:"status" gorm:"default:'open'"` // open, closed, locked
	ClosedBy       *string    `json:"closed_by"`
	ClosedAt       *time.Time `json:"closed_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Dashboard represents financial dashboard metrics with enhanced ledger data.
type Dashboard struct {
	TotalAssets      float64    `json:"total_assets"`
	TotalLiabilities float64    `json:"total_liabilities"`
	TotalRevenue     float64    `json:"total_revenue"`
	TotalExpenses    float64    `json:"total_expenses"`
	NetIncome        float64    `json:"net_income"`
	AccountsCount    int64      `json:"accounts_count"`
	PendingInvoices  int64      `json:"pending_invoices"`
	OverdueInvoices  int64      `json:"overdue_invoices"`
	PendingBills     int64      `json:"pending_bills"`
	CashBalance      float64    `json:"cash_balance"`
	LedgerEntries    int64      `json:"ledger_entries"`
	LastPostedEntry  *time.Time `json:"last_posted_entry"`
	TrialBalanceSum  float64    `json:"trial_balance_sum"`
}

// Summary is the finance overview used by the Accounting, Bills and Banking pages.
type Summary struct {
	CashInBank          float64 `json:"cash_in_bank"`
	CreditCardBalance   float64 `json:"credit_card_balance"`
	BankAccounts        int64   `json:"bank_accounts"`
	PayablesOutstanding float64 `json:"payables_outstanding"`
	PayablesOverdue     float64 `json:"payables_overdue"`
	OverdueCount        int     `json:"overdue_count"`
	DueSoon             float64 `json:"due_soon"`
	DueSoonCount        int     `json:"due_soon_count"`
	PaidThisMonth       float64 `json:"paid_this_month"`
	DraftBills          int     `json:"draft_bills"`
	OpenBills           int     `json:"open_bills"`
	RevenueThisMonth    float64 `json:"revenue_this_month"`
	ExpensesThisMonth   float64 `json:"expenses_this_month"`
	NetProfitThisMonth  float64 `json:"net_profit_this_month"`
	DraftJournals       int64   `json:"draft_journals"`
	UncategorisedTxns   int64   `json:"uncategorised_transactions"`
	UnreconciledTxns    int64   `json:"unreconciled_transactions"`
	LedgerBalanced      bool    `json:"ledger_balanced"`
}
