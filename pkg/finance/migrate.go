package finance

import (
	"log"

	"gorm.io/gorm"
)

// MigrateLegacy removes global unique constraints from older schema versions
// (account codes and document numbers are now unique per organisation) and
// back-fills organisation ids on bank transactions.
func MigrateLegacy(db *gorm.DB) {
	if db.Dialector.Name() != "postgres" {
		return
	}
	stmts := []string{
		`ALTER TABLE chart_of_accounts DROP CONSTRAINT IF EXISTS uni_chart_of_accounts_code`,
		`ALTER TABLE chart_of_accounts DROP CONSTRAINT IF EXISTS chart_of_accounts_code_key`,
		`DROP INDEX IF EXISTS idx_chart_of_accounts_code`,
		`ALTER TABLE journal_entries DROP CONSTRAINT IF EXISTS uni_journal_entries_entry_number`,
		`ALTER TABLE journal_entries DROP CONSTRAINT IF EXISTS journal_entries_entry_number_key`,
		`DROP INDEX IF EXISTS idx_journal_entries_entry_number`,
		`ALTER TABLE bills DROP CONSTRAINT IF EXISTS uni_bills_bill_number`,
		`ALTER TABLE bills DROP CONSTRAINT IF EXISTS bills_bill_number_key`,
		`DROP INDEX IF EXISTS idx_bills_bill_number`,
		`ALTER TABLE invoices DROP CONSTRAINT IF EXISTS uni_invoices_invoice_number`,
		`ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_invoice_number_key`,
		`UPDATE bank_transactions t SET organization_id = b.organization_id FROM bank_accounts b WHERE t.bank_account_id = b.id AND (t.organization_id IS NULL OR t.organization_id = '')`,
		`UPDATE journal_entries SET source = 'manual' WHERE source IS NULL OR source = ''`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			log.Printf("finance migrate: %v", err)
		}
	}
	// Recreate the non-unique lookup indexes.
	for _, m := range []interface{}{&ChartOfAccount{}, &JournalEntry{}, &Bill{}, &Invoice{}} {
		if err := db.AutoMigrate(m); err != nil {
			log.Printf("finance migrate: %v", err)
		}
	}
}
