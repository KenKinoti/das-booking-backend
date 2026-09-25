package finance

import (
	"errors"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var bankTypes = map[string]string{"checking": "Checking", "savings": "Savings", "credit_card": "Credit card", "cash": "Cash"}

func (s *Service) bankInOrg(tx *gorm.DB, orgID, id string) (*BankAccount, error) {
	var b BankAccount
	if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&b).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &b, nil
}

// ensureBankLedgerAccount links a bank account to its own chart account
// (Asset 1010-1099, or Liability 2210-2299 for credit cards).
func (s *Service) ensureBankLedgerAccount(tx *gorm.DB, b *BankAccount) (string, error) {
	if b.ChartAccountID != "" {
		if _, err := s.accountInOrg(tx, b.OrganizationID, b.ChartAccountID); err == nil {
			return b.ChartAccountID, nil
		}
	}
	start, end, typ, sub := bankCodeStart, bankCodeEnd, TypeAsset, subBank
	if b.AccountType == "credit_card" {
		start, end, typ, sub = cardCodeStart, cardCodeEnd, TypeLiability, subCreditCard
	}
	var codes []string
	tx.Model(&ChartOfAccount{}).Where("organization_id = ?", b.OrganizationID).Pluck("code", &codes)
	used := map[string]bool{}
	for _, c := range codes {
		used[c] = true
	}
	code := ""
	for i := start; i <= end; i++ {
		if c := strconv.Itoa(i); !used[c] {
			code = c
			break
		}
	}
	if code == "" {
		code = fmt.Sprintf("%d-%s", start, b.ID[:6])
	}
	name := b.AccountName
	if b.BankName != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(b.BankName)) {
		name = b.BankName + " — " + b.AccountName
	}
	acc := ChartOfAccount{ID: uuid.NewString(), OrganizationID: b.OrganizationID, Code: code, Name: name, AccountType: typ, SubType: sub,
		IsActive: true, IsSystem: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := tx.Create(&acc).Error; err != nil {
		return "", err
	}
	b.ChartAccountID = acc.ID
	return acc.ID, tx.Model(&BankAccount{}).Where("id = ?", b.ID).Update("chart_account_id", acc.ID).Error
}

// addBankLine records a statement line and moves the bank balance.
func (s *Service) addBankLine(tx *gorm.DB, b *BankAccount, date time.Time, desc, ref string, amount float64, accountID, category, jeID, source, sourceID string) (*BankTransaction, error) {
	amount = r2(amount)
	newBal := float64(cents(b.Balance)+cents(amount)) / 100
	t := BankTransaction{ID: uuid.NewString(), OrganizationID: b.OrganizationID, BankAccountID: b.ID, Date: dateOnly(date), Description: desc,
		Reference: ref, Amount: amount, Balance: newBal, Category: category, AccountID: accountID, JournalEntryID: jeID, Source: source,
		SourceID: sourceID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := tx.Create(&t).Error; err != nil {
		return nil, err
	}
	b.Balance = newBal
	return &t, tx.Model(&BankAccount{}).Where("id = ?", b.ID).Updates(map[string]interface{}{"balance": newBal, "updated_at": time.Now()}).Error
}

// removeBankLine deletes a statement line and reverses its balance effect.
func (s *Service) removeBankLine(tx *gorm.DB, t *BankTransaction) error {
	var b BankAccount
	if err := tx.Where("id = ?", t.BankAccountID).First(&b).Error; err == nil {
		nb := float64(cents(b.Balance)-cents(t.Amount)) / 100
		if err := tx.Model(&BankAccount{}).Where("id = ?", b.ID).Updates(map[string]interface{}{"balance": nb, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
	}
	return tx.Delete(t).Error
}

func (s *Service) GetBankAccounts(orgID string, includeInactive bool) ([]BankAccount, error) {
	s.EnsureSetup(orgID)
	q := s.db.Where("organization_id = ?", orgID)
	if !includeInactive {
		q = q.Where("is_active = ?", true)
	}
	var accounts []BankAccount
	if err := q.Order("created_at").Find(&accounts).Error; err != nil {
		return nil, err
	}
	var coa []ChartOfAccount
	s.db.Select("id, code").Where("organization_id = ?", orgID).Find(&coa)
	codes := map[string]string{}
	for _, c := range coa {
		codes[c.ID] = c.Code
	}
	for i := range accounts {
		a := &accounts[i]
		a.ChartAccountCode = codes[a.ChartAccountID]
		s.db.Model(&BankTransaction{}).Where("bank_account_id = ? AND is_reconciled = ?", a.ID, false).Count(&a.UnreconciledCount)
		s.db.Model(&BankTransaction{}).Where("bank_account_id = ? AND (account_id = '' OR account_id IS NULL)", a.ID).Count(&a.UncategorisedCount)
		var last BankTransaction
		if err := s.db.Where("bank_account_id = ?", a.ID).Order("date DESC").First(&last).Error; err == nil {
			a.LastTransactionDate = &last.Date
		}
	}
	return accounts, nil
}

// BankAccountInput is the payload for creating/updating a bank account.
type BankAccountInput struct {
	AccountName    string  `json:"account_name"`
	BankName       string  `json:"bank_name"`
	AccountNumber  string  `json:"account_number"`
	BSB            string  `json:"bsb"`
	AccountType    string  `json:"account_type"`
	Currency       string  `json:"currency"`
	OpeningBalance float64 `json:"opening_balance"`
	OpeningDate    string  `json:"opening_date"`
	Notes          string  `json:"notes"`
	IsActive       *bool   `json:"is_active"`
}

func cleanBankInput(in *BankAccountInput) error {
	in.AccountName, in.BankName = strings.TrimSpace(in.AccountName), strings.TrimSpace(in.BankName)
	in.AccountNumber, in.BSB = strings.TrimSpace(in.AccountNumber), strings.TrimSpace(in.BSB)
	in.AccountType = strings.TrimSpace(strings.ToLower(in.AccountType))
	if in.AccountName == "" {
		return invalid("account name is required")
	}
	if in.AccountType == "" {
		in.AccountType = "checking"
	}
	if _, ok := bankTypes[in.AccountType]; !ok {
		return invalid("account type must be checking, savings, credit_card or cash")
	}
	if in.BankName == "" {
		if in.AccountType != "cash" {
			return invalid("bank name is required")
		}
		in.BankName = "Cash"
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = orgprefs.DefaultCurrency
	}
	if len(in.Currency) != 3 {
		return invalid("currency must be a 3-letter code")
	}
	return nil
}

// CreateBankAccount adds a bank account, its ledger account and any opening balance.
func (s *Service) CreateBankAccount(orgID, userID string, in BankAccountInput) (*BankAccount, error) {
	s.EnsureSetup(orgID)
	if strings.TrimSpace(in.Currency) == "" {
		in.Currency = orgprefs.Currency(s.db, orgID)
	}
	if err := cleanBankInput(&in); err != nil {
		return nil, err
	}
	b := BankAccount{ID: uuid.NewString(), OrganizationID: orgID, AccountName: in.AccountName, BankName: in.BankName, AccountNumber: in.AccountNumber,
		BSB: in.BSB, AccountType: in.AccountType, Currency: in.Currency, OpeningBalance: r2(in.OpeningBalance), Notes: strings.TrimSpace(in.Notes),
		IsActive: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&b).Error; err != nil {
			return err
		}
		ledgerID, err := s.ensureBankLedgerAccount(tx, &b)
		if err != nil {
			return err
		}
		if cents(b.OpeningBalance) == 0 {
			return nil
		}
		date, ok := ParseDate(in.OpeningDate)
		if !ok {
			date = today()
		}
		obe, err := s.systemAccount(tx, orgID, codeOpeningEquity)
		if err != nil {
			return err
		}
		amt := b.OpeningBalance
		lines := []JournalLineInput{{AccountID: ledgerID, DebitAmount: amt}, {AccountID: obe.ID, CreditAmount: amt}}
		if amt < 0 {
			lines = []JournalLineInput{{AccountID: ledgerID, CreditAmount: -amt}, {AccountID: obe.ID, DebitAmount: -amt}}
		}
		e, err := s.createPosted(tx, orgID, userID, date, "Opening balance — "+b.AccountName, "", "opening_balance", b.ID, lines)
		if err != nil {
			return err
		}
		t, err := s.addBankLine(tx, &b, date, "Opening balance", "", amt, obe.ID, obe.Name, e.ID, "opening_balance", b.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(t).Updates(map[string]interface{}{"is_reconciled": true, "reconciled_at": now}).Error
	})
	if err != nil {
		return nil, err
	}
	return s.bankInOrg(s.db, orgID, b.ID)
}

// UpdateBankAccount edits details (the balance is driven by transactions).
func (s *Service) UpdateBankAccount(orgID, id string, in BankAccountInput) (*BankAccount, error) {
	b, err := s.bankInOrg(s.db, orgID, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Currency) == "" {
		in.Currency = b.Currency
	}
	if err := cleanBankInput(&in); err != nil {
		return nil, err
	}
	if in.AccountType != b.AccountType && (in.AccountType == "credit_card") != (b.AccountType == "credit_card") {
		var n int64
		s.db.Model(&BankTransaction{}).Where("bank_account_id = ?", id).Count(&n)
		if n > 0 {
			return nil, invalid("an account with transactions cannot be switched to or from a credit card")
		}
	}
	upd := map[string]interface{}{"account_name": in.AccountName, "bank_name": in.BankName, "account_number": in.AccountNumber, "bsb": in.BSB,
		"account_type": in.AccountType, "currency": in.Currency, "notes": strings.TrimSpace(in.Notes), "updated_at": time.Now()}
	if in.IsActive != nil {
		upd["is_active"] = *in.IsActive
	}
	if err := s.db.Model(b).Updates(upd).Error; err != nil {
		return nil, err
	}
	if b.ChartAccountID != "" {
		name := in.AccountName
		if in.BankName != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(in.BankName)) {
			name = in.BankName + " — " + in.AccountName
		}
		s.db.Model(&ChartOfAccount{}).Where("id = ? AND organization_id = ?", b.ChartAccountID, orgID).Updates(map[string]interface{}{"name": name, "updated_at": time.Now()})
	}
	return s.bankInOrg(s.db, orgID, id)
}

// DeleteBankAccount deletes an account with no activity, otherwise archives it.
func (s *Service) DeleteBankAccount(orgID, id string) (archived bool, err error) {
	b, err := s.bankInOrg(s.db, orgID, id)
	if err != nil {
		return false, err
	}
	var n int64
	s.db.Model(&BankTransaction{}).Where("bank_account_id = ?", id).Count(&n)
	var p int64
	s.db.Model(&Payment{}).Where("bank_account_id = ?", id).Count(&p)
	if n > 0 || p > 0 {
		return true, s.db.Model(b).Updates(map[string]interface{}{"is_active": false, "updated_at": time.Now()}).Error
	}
	return false, s.db.Transaction(func(tx *gorm.DB) error {
		if b.ChartAccountID != "" {
			var used int64
			tx.Model(&JournalEntryLine{}).Where("chart_of_account_id = ?", b.ChartAccountID).Count(&used)
			if used == 0 {
				tx.Where("account_id = ?", b.ChartAccountID).Delete(&AccountBalance{})
				tx.Where("id = ? AND organization_id = ?", b.ChartAccountID, orgID).Delete(&ChartOfAccount{})
			}
		}
		return tx.Delete(b).Error
	})
}

// ---------------------------------------------------------------------------
// Bank transactions

// TxnFilter filters the transaction list.
type TxnFilter struct {
	BankAccountID string
	Status        string // all, unreconciled, reconciled, uncategorised
	Direction     string // in, out
	Q             string
	From, To      time.Time
}

// ListBankTransactions returns transactions with running balances.
func (s *Service) ListBankTransactions(orgID string, f TxnFilter) ([]BankTransaction, error) {
	s.EnsureSetup(orgID)
	var banks []BankAccount
	s.db.Where("organization_id = ?", orgID).Find(&banks)
	bankNames := map[string]string{}
	for _, b := range banks {
		bankNames[b.ID] = b.AccountName
	}
	// Load all transactions for the selected account(s) so running balances
	// are correct, then filter in memory.
	q := s.db.Where("organization_id = ?", orgID)
	if f.BankAccountID != "" {
		q = q.Where("bank_account_id = ?", f.BankAccountID)
	}
	var all []BankTransaction
	if err := q.Order("date ASC, created_at ASC").Find(&all).Error; err != nil {
		return nil, err
	}
	running := map[string]int64{}
	for i := range all {
		running[all[i].BankAccountID] += cents(all[i].Amount)
		all[i].Balance = float64(running[all[i].BankAccountID]) / 100
	}
	var coa []ChartOfAccount
	s.db.Select("id, code, name").Where("organization_id = ?", orgID).Find(&coa)
	codes := map[string]ChartOfAccount{}
	for _, c := range coa {
		codes[c.ID] = c
	}
	needle := strings.ToLower(strings.TrimSpace(f.Q))
	out := make([]BankTransaction, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		t := all[i]
		switch f.Status {
		case "unreconciled":
			if t.IsReconciled {
				continue
			}
		case "reconciled":
			if !t.IsReconciled {
				continue
			}
		case "uncategorised":
			if t.AccountID != "" {
				continue
			}
		}
		if f.Direction == "in" && t.Amount < 0 || f.Direction == "out" && t.Amount >= 0 {
			continue
		}
		if !f.From.IsZero() && t.Date.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && t.Date.After(f.To) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(t.Description+" "+t.Reference+" "+t.Category), needle) {
			continue
		}
		t.BankAccountName = bankNames[t.BankAccountID]
		if c, ok := codes[t.AccountID]; ok {
			t.AccountCode, t.Category = c.Code, c.Name
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// BankTxnInput is a manual transaction.
type BankTxnInput struct {
	BankAccountID string  `json:"bank_account_id"`
	Date          string  `json:"date"`
	Description   string  `json:"description"`
	Reference     string  `json:"reference"`
	Type          string  `json:"type"` // deposit or withdrawal (when amount is positive)
	Amount        float64 `json:"amount"`
	AccountID     string  `json:"account_id"`
	IsReconciled  bool    `json:"is_reconciled"`
}

// postBankTxn journals a categorised transaction.
func (s *Service) postBankTxn(tx *gorm.DB, b *BankAccount, t *BankTransaction, acc *ChartOfAccount, userID string) error {
	ledger, err := s.ensureBankLedgerAccount(tx, b)
	if err != nil {
		return err
	}
	amt := t.Amount
	var lines []JournalLineInput
	if amt >= 0 {
		lines = []JournalLineInput{{AccountID: ledger, Description: t.Description, DebitAmount: amt}, {AccountID: acc.ID, Description: t.Description, CreditAmount: amt}}
	} else {
		lines = []JournalLineInput{{AccountID: acc.ID, Description: t.Description, DebitAmount: -amt}, {AccountID: ledger, Description: t.Description, CreditAmount: -amt}}
	}
	e, err := s.createPosted(tx, b.OrganizationID, userID, t.Date, t.Description, t.Reference, "bank", t.ID, lines)
	if err != nil {
		return err
	}
	t.JournalEntryID, t.AccountID, t.Category = e.ID, acc.ID, acc.Name
	return tx.Model(&BankTransaction{}).Where("id = ?", t.ID).Updates(map[string]interface{}{
		"journal_entry_id": e.ID, "account_id": acc.ID, "category": acc.Name, "updated_at": time.Now()}).Error
}

func (s *Service) unpostBankTxn(tx *gorm.DB, t *BankTransaction, userID string) error {
	if t.JournalEntryID == "" {
		return nil
	}
	var e JournalEntry
	if err := tx.Preload("LineItems").Where("id = ?", t.JournalEntryID).First(&e).Error; err == nil && e.Status == "posted" {
		if _, err := s.reverseEntry(tx, &e, userID, today()); err != nil {
			return err
		}
	}
	t.JournalEntryID = ""
	return tx.Model(&BankTransaction{}).Where("id = ?", t.ID).Updates(map[string]interface{}{"journal_entry_id": "", "account_id": "", "category": ""}).Error
}

func (s *Service) CreateBankTransaction(orgID, userID string, in BankTxnInput) (*BankTransaction, error) {
	var out BankTransaction
	err := s.db.Transaction(func(tx *gorm.DB) error {
		b, err := s.bankInOrg(tx, orgID, in.BankAccountID)
		if err != nil {
			return invalid("choose a bank account")
		}
		if !b.IsActive {
			return invalid("this bank account is archived")
		}
		desc := strings.TrimSpace(in.Description)
		if desc == "" {
			return invalid("description is required")
		}
		amt := r2(in.Amount)
		if amt == 0 {
			return invalid("amount must not be zero")
		}
		if amt > 0 && strings.EqualFold(in.Type, "withdrawal") {
			amt = -amt
		}
		date, ok := ParseDate(in.Date)
		if !ok {
			date = today()
		}
		var acc *ChartOfAccount
		if in.AccountID != "" {
			if acc, err = s.accountInOrg(tx, orgID, in.AccountID); err != nil {
				return err
			}
			if acc.ID == b.ChartAccountID {
				return invalid("choose a category other than the bank account itself")
			}
		}
		t, err := s.addBankLine(tx, b, date, desc, strings.TrimSpace(in.Reference), amt, "", "", "", "manual", "")
		if err != nil {
			return err
		}
		if in.IsReconciled {
			now := time.Now()
			t.IsReconciled, t.ReconciledAt = true, &now
			tx.Model(t).Updates(map[string]interface{}{"is_reconciled": true, "reconciled_at": now})
		}
		if acc != nil {
			if err := s.postBankTxn(tx, b, t, acc, userID); err != nil {
				return err
			}
		}
		out = *t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CategorizeBankTransaction assigns (or changes) the ledger account.
func (s *Service) CategorizeBankTransaction(orgID, userID, id, accountID string) (*BankTransaction, error) {
	var out BankTransaction
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var t BankTransaction
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&t).Error; err != nil {
			return ErrNotFound
		}
		if t.Source != "" && t.Source != "manual" {
			return invalid("this transaction was created by a %s and is already categorised", strings.ReplaceAll(t.Source, "_", " "))
		}
		b, err := s.bankInOrg(tx, orgID, t.BankAccountID)
		if err != nil {
			return err
		}
		if accountID == t.AccountID && accountID != "" {
			out = t
			return nil
		}
		if err := s.unpostBankTxn(tx, &t, userID); err != nil {
			return err
		}
		if accountID != "" {
			acc, err := s.accountInOrg(tx, orgID, accountID)
			if err != nil {
				return err
			}
			if acc.ID == b.ChartAccountID {
				return invalid("choose a category other than the bank account itself")
			}
			if err := s.postBankTxn(tx, b, &t, acc, userID); err != nil {
				return err
			}
		}
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetReconciled flags a transaction as matched to the bank statement.
func (s *Service) SetReconciled(orgID, id string, reconciled bool) (*BankTransaction, error) {
	var t BankTransaction
	if err := s.db.Where("organization_id = ? AND id = ?", orgID, id).First(&t).Error; err != nil {
		return nil, ErrNotFound
	}
	upd := map[string]interface{}{"is_reconciled": reconciled, "reconciled_at": nil, "updated_at": time.Now()}
	if reconciled {
		upd["reconciled_at"] = time.Now()
	}
	if err := s.db.Model(&t).Updates(upd).Error; err != nil {
		return nil, err
	}
	t.IsReconciled = reconciled
	return &t, nil
}

// DeleteBankTransaction removes a manual transaction and its journal.
func (s *Service) DeleteBankTransaction(orgID, userID, id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var t BankTransaction
		if err := tx.Where("organization_id = ? AND id = ?", orgID, id).First(&t).Error; err != nil {
			return ErrNotFound
		}
		switch t.Source {
		case "bill_payment":
			return invalid("this is a bill payment — remove the payment from the bill instead")
		case "opening_balance":
			return invalid("the opening balance cannot be deleted")
		}
		if err := s.unpostBankTxn(tx, &t, userID); err != nil {
			return err
		}
		return s.removeBankLine(tx, &t)
	})
}
