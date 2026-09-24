package smallbiz

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// MaxAttachmentBytes caps an uploaded receipt image (decoded size).
const MaxAttachmentBytes = 600 * 1024

// Handler serves /api/v1/smallbiz.
type Handler struct {
	DB  *gorm.DB
	Now func() time.Time
}

func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db} }

// RegisterRoutes expects org_id / user_id in the gin context (JWT middleware).
func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/settings", h.GetSettings)
	r.PUT("/settings", h.UpdateSettings)

	r.GET("/categories", h.ListCategories)
	r.POST("/categories", h.CreateCategory)
	r.PUT("/categories/:id", h.UpdateCategory)
	r.DELETE("/categories/:id", h.DeleteCategory)

	r.GET("/entries", h.ListEntries)
	r.POST("/entries", h.CreateEntry)
	r.GET("/entries/:id", h.GetEntry)
	r.PUT("/entries/:id", h.UpdateEntry)
	r.DELETE("/entries/:id", h.DeleteEntry)
	r.GET("/export", h.ExportEntries)

	r.GET("/overview", h.Overview)
	r.GET("/reports/pnl", h.ProfitAndLoss)
	r.GET("/reports/tax", h.TaxReport)

	r.GET("/reminders", h.ListReminders)
	r.POST("/reminders", h.CreateReminder)
	r.PUT("/reminders/:id", h.UpdateReminder)
	r.DELETE("/reminders/:id", h.DeleteReminder)
	r.POST("/reminders/:id/toggle", h.ToggleReminder)
}

// ---------- plumbing ----------

type apiError struct {
	status int
	code   string
	msg    string
	fields map[string]string
}

func (e *apiError) Error() string { return e.msg }

func invalid(fields map[string]string) *apiError {
	msg := "Please check the highlighted fields"
	if len(fields) == 1 {
		for _, v := range fields {
			msg = v
		}
	}
	return &apiError{http.StatusUnprocessableEntity, "VALIDATION_ERROR", msg, fields}
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func failErr(c *gin.Context, err error, what string) {
	var ae *apiError
	if errors.As(err, &ae) {
		body := gin.H{"code": ae.code, "message": ae.msg}
		if len(ae.fields) > 0 {
			body["fields"] = ae.fields
		}
		c.JSON(ae.status, gin.H{"success": false, "error": body})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", what+" not found")
		return
	}
	fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func ctxString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	if s, isStr := v.(string); isStr {
		return s
	}
	return fmt.Sprint(v)
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id")
	if org == "" {
		org = ctxString(c, "organization_id")
	}
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return org, true
}

func (h *Handler) today() time.Time {
	if h.Now != nil {
		return dateOnly(h.Now())
	}
	return dateOnly(time.Now())
}

// flexAmount accepts a JSON number or string.
type flexAmount struct {
	Set   bool
	Value string
}

func (f *flexAmount) UnmarshalJSON(b []byte) error {
	f.Set = true
	s := strings.TrimSpace(string(b))
	if s == "null" {
		f.Value = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		return json.Unmarshal(b, &f.Value)
	}
	f.Value = s
	return nil
}

// ---------- settings ----------

var taxDefaults = map[string]struct {
	Name string
	Rate float64
}{
	"AUD": {"GST", 10}, "NZD": {"GST", 15}, "SGD": {"GST", 9}, "INR": {"GST", 18}, "CAD": {"GST", 5},
	"GBP": {"VAT", 20}, "EUR": {"VAT", 20}, "CHF": {"VAT", 8.1}, "ZAR": {"VAT", 15}, "KES": {"VAT", 16},
	"NGN": {"VAT", 7.5}, "GHS": {"VAT", 15}, "EGP": {"VAT", 14}, "MAD": {"VAT", 20}, "TZS": {"VAT", 18},
	"UGX": {"VAT", 18}, "AED": {"VAT", 5}, "SAR": {"VAT", 15}, "CNY": {"VAT", 13}, "JPY": {"Consumption tax", 10},
	"HKD": {"Tax", 0}, "KWD": {"Tax", 0}, "USD": {"Sales tax", 0},
}

// fyDefaults is the usual first month of the financial year by currency.
var fyDefaults = map[string]int{"AUD": 7, "EGP": 7, "KES": 7, "UGX": 7, "TZS": 7, "NZD": 4, "GBP": 4, "INR": 4, "JPY": 4, "HKD": 4, "ZAR": 3}

func (h *Handler) orgCurrency(org string) string {
	cur := ""
	if h.DB.Migrator().HasTable("organization_settings") {
		h.DB.Raw("SELECT currency FROM organization_settings WHERE organization_id = ? LIMIT 1", org).Scan(&cur)
	}
	cur = strings.ToUpper(strings.TrimSpace(cur))
	if !currency.IsSupported(cur) {
		cur = "AUD"
	}
	return cur
}

func (h *Handler) defaults(org string) Settings {
	cur := h.orgCurrency(org)
	td, found := taxDefaults[cur]
	if !found {
		td.Name = "Tax"
	}
	fy, found := fyDefaults[cur]
	if !found {
		fy = 1
	}
	return Settings{OrganizationID: org, Currency: cur, TaxName: td.Name, TaxRate: td.Rate, FinancialYearStart: fy}
}

// ensure loads (creating if needed) the org's settings and seeds categories once.
func (h *Handler) ensure(org string) (*Settings, error) {
	var s Settings
	r := h.DB.Where("organization_id = ?", org).Limit(1).Find(&s)
	if r.Error != nil {
		return nil, r.Error
	}
	if r.RowsAffected == 1 && s.CategoriesSeeded {
		return &s, nil
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		def := h.defaults(org)
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&def).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ?", org).First(&s).Error; err != nil {
			return err
		}
		if s.CategoriesSeeded {
			return nil
		}
		var n int64
		tx.Model(&Category{}).Where("organization_id = ?", org).Count(&n)
		if n == 0 {
			cats := make([]Category, 0, len(DefaultCategories))
			for i, d := range DefaultCategories {
				cats = append(cats, Category{ID: uuid.NewString(), OrganizationID: org, Name: d.Name, Type: d.Type, TaxFree: d.TaxFree, SortOrder: i})
			}
			if err := tx.Create(&cats).Error; err != nil {
				return err
			}
		}
		s.CategoriesSeeded = true
		return tx.Model(&Settings{}).Where("organization_id = ?", org).Update("categories_seeded", true).Error
	})
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (h *Handler) GetSettings(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	ok(c, http.StatusOK, settingsView(s))
}

func settingsView(s *Settings) gin.H {
	d := decimalsFor(s.Currency)
	return gin.H{
		"configured": s.Configured, "trading_name": s.TradingName, "owner_name": s.OwnerName,
		"registration_number": s.RegistrationNumber, "tax_number": s.TaxNumber, "industry": s.Industry,
		"email": s.Email, "phone": s.Phone, "address": s.Address, "financial_year_start": s.FinancialYearStart,
		"currency": s.Currency, "decimals": d, "tax_name": s.TaxName, "tax_rate": s.TaxRate,
		"tax_registered": s.TaxRegistered, "opening_balance_minor": s.OpeningBalance,
		"opening_balance": FormatMinor(s.OpeningBalance, d), "updated_at": s.UpdatedAt,
	}
}

type settingsInput struct {
	TradingName        string     `json:"trading_name"`
	OwnerName          string     `json:"owner_name"`
	RegistrationNumber string     `json:"registration_number"`
	TaxNumber          string     `json:"tax_number"`
	Industry           string     `json:"industry"`
	Email              string     `json:"email"`
	Phone              string     `json:"phone"`
	Address            string     `json:"address"`
	FinancialYearStart int        `json:"financial_year_start"`
	Currency           string     `json:"currency"`
	TaxName            string     `json:"tax_name"`
	TaxRate            float64    `json:"tax_rate"`
	TaxRegistered      bool       `json:"tax_registered"`
	OpeningBalance     flexAmount `json:"opening_balance"`
}

func (h *Handler) UpdateSettings(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in settingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	fields := map[string]string{}
	in.TradingName = strings.TrimSpace(in.TradingName)
	if in.TradingName == "" {
		fields["trading_name"] = "Add your trading name"
	} else if len(in.TradingName) > 200 {
		fields["trading_name"] = "Trading name is too long"
	}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = s.Currency
	}
	if !currency.IsSupported(cur) {
		fields["currency"] = "Choose a supported currency"
	}
	if in.FinancialYearStart < 1 || in.FinancialYearStart > 12 {
		fields["financial_year_start"] = "Choose the month your financial year starts"
	}
	if !ValidRate(in.TaxRate) {
		fields["tax_rate"] = "Tax rate must be between 0 and 100"
	}
	if in.Email != "" && !strings.Contains(in.Email, "@") {
		fields["email"] = "Enter a valid email"
	}
	var opening int64
	if in.OpeningBalance.Set && strings.TrimSpace(in.OpeningBalance.Value) != "" && currency.IsSupported(cur) {
		if opening, err = ParseMinor(in.OpeningBalance.Value, decimalsFor(cur)); err != nil {
			fields["opening_balance"] = "Opening balance: " + err.Error()
		}
	} else if !in.OpeningBalance.Set {
		opening = s.OpeningBalance
	}
	for k, v := range map[string]string{"owner_name": in.OwnerName, "registration_number": in.RegistrationNumber, "tax_number": in.TaxNumber, "industry": in.Industry, "phone": in.Phone, "email": in.Email} {
		if len(v) > 100 && k != "owner_name" && k != "email" {
			fields[k] = "Too long"
		}
	}
	if len(fields) > 0 {
		failErr(c, invalid(fields), "")
		return
	}
	taxName := strings.TrimSpace(in.TaxName)
	if taxName == "" {
		taxName = "Tax"
	}
	upd := map[string]interface{}{
		"configured": true, "trading_name": in.TradingName, "owner_name": strings.TrimSpace(in.OwnerName),
		"registration_number": strings.TrimSpace(in.RegistrationNumber), "tax_number": strings.TrimSpace(in.TaxNumber),
		"industry": strings.TrimSpace(in.Industry), "email": strings.TrimSpace(in.Email), "phone": strings.TrimSpace(in.Phone),
		"address": strings.TrimSpace(in.Address), "financial_year_start": in.FinancialYearStart, "currency": cur,
		"tax_name": taxName, "tax_rate": in.TaxRate, "tax_registered": in.TaxRegistered, "opening_balance": opening,
		"updated_at": time.Now(),
	}
	if err := h.DB.Model(&Settings{}).Where("organization_id = ?", org).Updates(upd).Error; err != nil {
		failErr(c, err, "Settings")
		return
	}
	s, err = h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	ok(c, http.StatusOK, settingsView(s))
}

// ---------- categories ----------

func (h *Handler) categories(org string) ([]Category, map[string]string, error) {
	var cats []Category
	if err := h.DB.Where("organization_id = ?", org).Order("type DESC, sort_order, name").Find(&cats).Error; err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(cats))
	for _, c := range cats {
		names[c.ID] = c.Name
	}
	return cats, names, nil
}

func (h *Handler) ListCategories(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	if _, err := h.ensure(org); err != nil {
		failErr(c, err, "Settings")
		return
	}
	cats, _, err := h.categories(org)
	if err != nil {
		failErr(c, err, "Category")
		return
	}
	type cnt struct {
		CategoryID string
		N          int64
	}
	var counts []cnt
	h.DB.Model(&Entry{}).Select("category_id, COUNT(*) AS n").Where("organization_id = ?", org).Group("category_id").Scan(&counts)
	m := map[string]int64{}
	for _, x := range counts {
		m[x.CategoryID] = x.N
	}
	for i := range cats {
		cats[i].EntryCount = m[cats[i].ID]
	}
	ok(c, http.StatusOK, gin.H{"categories": cats})
}

type categoryInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	TaxFree bool   `json:"tax_free"`
}

func (h *Handler) saveCategory(c *gin.Context, id string) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in categoryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	if _, err := h.ensure(org); err != nil {
		failErr(c, err, "Settings")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	fields := map[string]string{}
	if in.Name == "" || len(in.Name) > 80 {
		fields["name"] = "Give the category a name (up to 80 characters)"
	}
	if in.Type != TypeIncome && in.Type != TypeExpense {
		fields["type"] = "Choose money in or money out"
	}
	var dup int64
	q := h.DB.Model(&Category{}).Where("organization_id = ? AND type = ? AND LOWER(name) = LOWER(?)", org, in.Type, in.Name)
	if id != "" {
		q = q.Where("id <> ?", id)
	}
	q.Count(&dup)
	if dup > 0 {
		fields["name"] = "A category with this name already exists"
	}
	if len(fields) > 0 {
		failErr(c, invalid(fields), "")
		return
	}
	var cat Category
	if id == "" {
		var maxOrder int
		h.DB.Model(&Category{}).Select("COALESCE(MAX(sort_order), 0)").Where("organization_id = ?", org).Scan(&maxOrder)
		cat = Category{ID: uuid.NewString(), OrganizationID: org, Name: in.Name, Type: in.Type, TaxFree: in.TaxFree, SortOrder: maxOrder + 1}
		if err := h.DB.Create(&cat).Error; err != nil {
			failErr(c, err, "Category")
			return
		}
		ok(c, http.StatusCreated, cat)
		return
	}
	if err := h.DB.Where("organization_id = ? AND id = ?", org, id).First(&cat).Error; err != nil {
		failErr(c, err, "Category")
		return
	}
	if cat.Type != in.Type {
		var used int64
		h.DB.Model(&Entry{}).Where("organization_id = ? AND category_id = ?", org, id).Count(&used)
		if used > 0 {
			failErr(c, invalid(map[string]string{"type": "This category is used by entries, so its type can't change"}), "")
			return
		}
	}
	cat.Name, cat.Type, cat.TaxFree = in.Name, in.Type, in.TaxFree
	if err := h.DB.Save(&cat).Error; err != nil {
		failErr(c, err, "Category")
		return
	}
	ok(c, http.StatusOK, cat)
}

func (h *Handler) CreateCategory(c *gin.Context) { h.saveCategory(c, "") }
func (h *Handler) UpdateCategory(c *gin.Context) { h.saveCategory(c, c.Param("id")) }

// DeleteCategory removes a category; its entries become "Uncategorised".
func (h *Handler) DeleteCategory(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var moved int64
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var cat Category
		if err := tx.Where("organization_id = ? AND id = ?", org, c.Param("id")).First(&cat).Error; err != nil {
			return err
		}
		r := tx.Model(&Entry{}).Where("organization_id = ? AND category_id = ?", org, cat.ID).Update("category_id", "")
		if r.Error != nil {
			return r.Error
		}
		moved = r.RowsAffected
		return tx.Delete(&cat).Error
	})
	if err != nil {
		failErr(c, err, "Category")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true, "entries_uncategorised": moved})
}

// ---------- entries ----------

type entryInput struct {
	Type           string     `json:"type"`
	Date           string     `json:"date"`
	Amount         flexAmount `json:"amount"`
	Currency       string     `json:"currency"`
	TaxRate        *float64   `json:"tax_rate"`
	Tax            flexAmount `json:"tax"`
	CategoryID     string     `json:"category_id"`
	PaymentMethod  string     `json:"payment_method"`
	Description    string     `json:"description"`
	Reference      string     `json:"reference"`
	AttachmentURL  string     `json:"attachment_url"`
	AttachmentName string     `json:"attachment_name"`
	AttachmentData *string    `json:"attachment_data"`
}

var allowedAttachmentTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif", "application/pdf"}

func validateAttachmentData(s string) error {
	head, data, found := strings.Cut(s, ",")
	if !found || !strings.HasPrefix(head, "data:") || !strings.HasSuffix(head, ";base64") {
		return errors.New("Attachment must be an image or PDF file")
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")
	allowed := false
	for _, t := range allowedAttachmentTypes {
		if mime == t {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("Attachment must be a PNG, JPEG, WebP, GIF or PDF")
	}
	if base64.StdEncoding.DecodedLen(len(data)) > MaxAttachmentBytes+3 {
		return fmt.Errorf("Attachment is too large (max %d KB)", MaxAttachmentBytes/1024)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return errors.New("Attachment could not be read")
	}
	if len(raw) > MaxAttachmentBytes {
		return fmt.Errorf("Attachment is too large (max %d KB)", MaxAttachmentBytes/1024)
	}
	return nil
}

func (h *Handler) applyEntry(org string, s *Settings, in *entryInput, e *Entry) error {
	fields := map[string]string{}
	if in.Type != TypeIncome && in.Type != TypeExpense {
		fields["type"] = "Choose money in or money out"
	}
	d, err := ParseDate(in.Date)
	if err != nil {
		fields["date"] = "Enter a valid date"
	} else if d.Year() < 1970 || d.Year() > 2200 {
		fields["date"] = "Enter a realistic date"
	}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = s.Currency
	}
	if !currency.IsSupported(cur) {
		fields["currency"] = "Choose a supported currency"
	}
	dec := decimalsFor(cur)
	amount, err := ParseMinor(in.Amount.Value, dec)
	if err != nil {
		fields["amount"] = strings.ToUpper(err.Error()[:1]) + err.Error()[1:]
	} else if amount <= 0 {
		fields["amount"] = "Amount must be more than zero"
	}
	rate := 0.0
	if in.TaxRate != nil {
		rate = *in.TaxRate
	}
	if !ValidRate(rate) {
		fields["tax_rate"] = "Tax rate must be between 0 and 100"
	}
	tax := TaxFromInclusive(amount, rate)
	if in.Tax.Set && strings.TrimSpace(in.Tax.Value) != "" {
		t, err := ParseMinor(in.Tax.Value, dec)
		switch {
		case err != nil:
			fields["tax"] = "Tax: " + err.Error()
		case t < 0 || (amount > 0 && t > amount):
			fields["tax"] = "Tax can't be more than the amount"
		default:
			tax = t
		}
	}
	catID := strings.TrimSpace(in.CategoryID)
	if catID != "" {
		var cat Category
		if err := h.DB.Where("organization_id = ? AND id = ?", org, catID).First(&cat).Error; err != nil {
			fields["category_id"] = "Choose a category"
		} else if cat.Type != in.Type && (in.Type == TypeIncome || in.Type == TypeExpense) {
			fields["category_id"] = "That category is for money " + map[string]string{TypeIncome: "in", TypeExpense: "out"}[cat.Type]
		}
	}
	desc := strings.TrimSpace(in.Description)
	if len(desc) > 500 {
		fields["description"] = "Description is too long (500 characters max)"
	}
	if len(in.Reference) > 120 {
		fields["reference"] = "Reference is too long"
	}
	if len(in.PaymentMethod) > 40 {
		fields["payment_method"] = "Payment method is too long"
	}
	url := strings.TrimSpace(in.AttachmentURL)
	if url != "" && !(strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")) {
		fields["attachment_url"] = "Link must start with https://"
	} else if len(url) > 1000 {
		fields["attachment_url"] = "Link is too long"
	}
	if in.AttachmentData != nil && *in.AttachmentData != "" {
		if err := validateAttachmentData(*in.AttachmentData); err != nil {
			fields["attachment_data"] = err.Error()
		}
	}
	if len(fields) > 0 {
		return invalid(fields)
	}
	e.Type, e.Date, e.Amount, e.Currency, e.TaxRate, e.Tax = in.Type, d, amount, cur, float64(rateBP(rate))/100, tax
	e.CategoryID, e.PaymentMethod, e.Description = catID, strings.TrimSpace(in.PaymentMethod), desc
	e.Reference, e.AttachmentURL = strings.TrimSpace(in.Reference), url
	if in.AttachmentData != nil {
		e.AttachmentData = *in.AttachmentData
		name := strings.TrimSpace(in.AttachmentName)
		if len(name) > 200 {
			name = name[:200]
		}
		if e.AttachmentData == "" {
			name = ""
		}
		e.AttachmentName = name
	}
	e.HasAttachment = e.AttachmentData != "" || e.AttachmentURL != ""
	return nil
}

func (h *Handler) decorate(org string, entries []Entry, names map[string]string) {
	for i := range entries {
		entries[i].CategoryName = names[entries[i].CategoryID]
	}
}

func (h *Handler) CreateEntry(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in entryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	e := Entry{ID: uuid.NewString(), OrganizationID: org, CreatedBy: ctxString(c, "user_id")}
	if err := h.applyEntry(org, s, &in, &e); err != nil {
		failErr(c, err, "Entry")
		return
	}
	if err := h.DB.Create(&e).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	_, names, _ := h.categories(org)
	e.CategoryName = names[e.CategoryID]
	ok(c, http.StatusCreated, e)
}

func (h *Handler) GetEntry(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var e Entry
	if err := h.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).First(&e).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	_, names, _ := h.categories(org)
	e.CategoryName = names[e.CategoryID]
	ok(c, http.StatusOK, e)
}

func (h *Handler) UpdateEntry(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in entryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	var e Entry
	if err := h.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).First(&e).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	if err := h.applyEntry(org, s, &in, &e); err != nil {
		failErr(c, err, "Entry")
		return
	}
	if err := h.DB.Save(&e).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	_, names, _ := h.categories(org)
	e.CategoryName = names[e.CategoryID]
	ok(c, http.StatusOK, e)
}

func (h *Handler) DeleteEntry(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	r := h.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).Delete(&Entry{})
	if r.Error != nil {
		failErr(c, r.Error, "Entry")
		return
	}
	if r.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Entry not found")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// light columns: everything but the attachment payload.
const entryCols = "id, type, date, amount, currency, tax_rate, tax, category_id, payment_method, description, reference, attachment_url, attachment_name, has_attachment, created_by, created_at, updated_at"

type entryFilter struct {
	period   Period
	typ      string
	category string
	currency string
	q        string
}

func (h *Handler) parseFilter(c *gin.Context, s *Settings) (entryFilter, error) {
	f := entryFilter{typ: c.Query("type"), category: c.Query("category_id"), q: strings.TrimSpace(c.Query("q"))}
	f.currency = strings.ToUpper(c.Query("currency"))
	if f.currency != "" && !currency.IsSupported(f.currency) {
		return f, invalid(map[string]string{"currency": "Unsupported currency"})
	}
	if f.typ != "" && f.typ != TypeIncome && f.typ != TypeExpense {
		return f, invalid(map[string]string{"type": "type must be income or expense"})
	}
	key := c.DefaultQuery("period", "all")
	p, err := ResolvePeriod(key, c.Query("from"), c.Query("to"), h.today(), s.FinancialYearStart)
	if err != nil {
		return f, invalid(map[string]string{"period": err.Error()})
	}
	f.period = p
	return f, nil
}

func (h *Handler) filtered(org string, f entryFilter) *gorm.DB {
	q := h.DB.Model(&Entry{}).Where("organization_id = ? AND date >= ? AND date < ?", org, f.period.From, f.period.Until())
	if f.typ != "" {
		q = q.Where("type = ?", f.typ)
	}
	switch f.category {
	case "":
	case "none":
		q = q.Where("category_id = '' OR category_id IS NULL")
	default:
		q = q.Where("category_id = ?", f.category)
	}
	if f.currency != "" {
		q = q.Where("currency = ?", f.currency)
	}
	if f.q != "" {
		like := "%" + strings.ToLower(f.q) + "%"
		cond := h.DB.Where("LOWER(description) LIKE ? OR LOWER(reference) LIKE ? OR LOWER(payment_method) LIKE ?", like, like, like).
			Or("category_id IN (?)", h.DB.Model(&Category{}).Select("id").Where("organization_id = ? AND LOWER(name) LIKE ?", org, like))
		if v, err := ParseMinor(f.q, 2); err == nil && v > 0 {
			// amount search in any currency: match both 2-dp and whole-number interpretations
			alts := []int64{v, v * 10}
			if v%100 == 0 {
				alts = append(alts, v/100)
			}
			cond = cond.Or("amount IN ?", alts)
		}
		q = q.Where(cond)
	}
	return q
}

type currencyTotals struct {
	Currency string `json:"currency"`
	Decimals int    `json:"decimals"`
	Totals
}

func totalsByCurrency(entries []Entry, base string) []currencyTotals {
	m := map[string]*currencyTotals{}
	order := []string{}
	for _, e := range entries {
		t, found := m[e.Currency]
		if !found {
			t = &currencyTotals{Currency: e.Currency, Decimals: decimalsFor(e.Currency)}
			m[e.Currency] = t
			order = append(order, e.Currency)
		}
		t.Add(e)
	}
	out := []currencyTotals{}
	if t, found := m[base]; found {
		out = append(out, *t)
	}
	for _, k := range order {
		if k != base {
			out = append(out, *m[k])
		}
	}
	return out
}

func (h *Handler) ListEntries(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	f, err := h.parseFilter(c, s)
	if err != nil {
		failErr(c, err, "")
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "50"))
	if page < 1 {
		page = 1
	}
	if per < 1 || per > 500 {
		per = 50
	}
	var all []Entry
	if err := h.filtered(org, f).Select("id, type, amount, tax, currency").Find(&all).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	var rows []Entry
	if err := h.filtered(org, f).Select(entryCols).Order("date DESC, created_at DESC, id DESC").
		Offset((page - 1) * per).Limit(per).Find(&rows).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	// running balance over the full (unfiltered) cash book
	var every []Entry
	h.DB.Model(&Entry{}).Select("id, type, amount, currency, date, created_at").Where("organization_id = ?", org).Find(&every)
	bal := RunningBalances(every, s.Currency, s.OpeningBalance)
	_, names, _ := h.categories(org)
	h.decorate(org, rows, names)
	for i := range rows {
		if b, found := bal[rows[i].ID]; found {
			v := b
			rows[i].Balance = &v
		}
	}
	ok(c, http.StatusOK, gin.H{
		"entries": rows, "total": len(all), "page": page, "per_page": per,
		"totals": totalsByCurrency(all, s.Currency), "currency": s.Currency,
		"period": gin.H{"key": f.period.Key, "label": f.period.Label, "from": f.period.From.Format("2006-01-02"), "to": f.period.To.Format("2006-01-02")},
	})
}

func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}

func writeCSV(c *gin.Context, filename string, rows [][]string) {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	_ = w.WriteAll(rows)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}

func (h *Handler) ExportEntries(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	f, err := h.parseFilter(c, s)
	if err != nil {
		failErr(c, err, "")
		return
	}
	var rows []Entry
	if err := h.filtered(org, f).Select(entryCols).Order("date ASC, created_at ASC, id ASC").Find(&rows).Error; err != nil {
		failErr(c, err, "Entry")
		return
	}
	var every []Entry
	h.DB.Model(&Entry{}).Select("id, type, amount, currency, date, created_at").Where("organization_id = ?", org).Find(&every)
	bal := RunningBalances(every, s.Currency, s.OpeningBalance)
	_, names, _ := h.categories(org)
	out := [][]string{{"Date", "Type", "Category", "Description", "Reference", "Payment method", "Currency", "Amount", "Tax rate %", "Tax", "Amount excl. tax", "Balance", "Receipt"}}
	for _, e := range rows {
		d := decimalsFor(e.Currency)
		typ := "Money in"
		if e.Type == TypeExpense {
			typ = "Money out"
		}
		cat := names[e.CategoryID]
		if cat == "" {
			cat = "Uncategorised"
		}
		receipt := e.AttachmentURL
		if receipt == "" && e.HasAttachment {
			receipt = "uploaded: " + e.AttachmentName
		}
		out = append(out, []string{
			e.Date.Format("2006-01-02"), typ, csvSafe(cat), csvSafe(e.Description), csvSafe(e.Reference), csvSafe(e.PaymentMethod), e.Currency,
			FormatMinor(e.Amount, d), strconv.FormatFloat(e.TaxRate, 'f', -1, 64), FormatMinor(e.Tax, d), FormatMinor(e.Amount-e.Tax, d),
			FormatMinor(bal[e.ID], d), csvSafe(receipt),
		})
	}
	writeCSV(c, fmt.Sprintf("cash-book-%s.csv", h.today().Format("2006-01-02")), out)
}

// ---------- overview & reports ----------

func (h *Handler) loadEntries(org string, from, until time.Time) ([]Entry, error) {
	var rows []Entry
	err := h.DB.Model(&Entry{}).Select("id, type, date, amount, currency, tax_rate, tax, category_id, created_at").
		Where("organization_id = ? AND date >= ? AND date < ?", org, from, until).Find(&rows).Error
	return rows, err
}

func periodView(p Period) gin.H {
	return gin.H{"key": p.Key, "label": p.Label, "from": p.From.Format("2006-01-02"), "to": p.To.Format("2006-01-02")}
}

func (h *Handler) Overview(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	cur := strings.ToUpper(c.DefaultQuery("currency", s.Currency))
	if !currency.IsSupported(cur) {
		cur = s.Currency
	}
	today := h.today()
	thisM, _ := ResolvePeriod("this_month", "", "", today, s.FinancialYearStart)
	lastM, _ := ResolvePeriod("last_month", "", "", today, s.FinancialYearStart)
	taxP, err := ResolvePeriod(c.DefaultQuery("tax_period", "this_quarter"), c.Query("tax_from"), c.Query("tax_to"), today, s.FinancialYearStart)
	if err != nil {
		failErr(c, invalid(map[string]string{"tax_period": err.Error()}), "")
		return
	}
	topP, err := ResolvePeriod(c.DefaultQuery("top_period", "this_month"), "", "", today, s.FinancialYearStart)
	if err != nil {
		failErr(c, invalid(map[string]string{"top_period": err.Error()}), "")
		return
	}
	trendFrom := monthStart(today).AddDate(0, -5, 0)
	from := trendFrom
	for _, p := range []Period{lastM, taxP, topP} {
		if p.From.Before(from) {
			from = p.From
		}
	}
	entries, err := h.loadEntries(org, from, thisM.Until().AddDate(100, 0, 0))
	if err != nil {
		failErr(c, err, "Entry")
		return
	}
	pick := func(p Period) []Entry {
		out := []Entry{}
		for _, e := range entries {
			if p.Contains(e.Date) {
				out = append(out, e)
			}
		}
		return out
	}
	_, names, _ := h.categories(org)

	// cash position per currency, all time
	type agg struct {
		Currency string
		Type     string
		Total    int64
		N        int64
	}
	var sums []agg
	h.DB.Model(&Entry{}).Select("currency, type, SUM(amount) AS total, COUNT(*) AS n").Where("organization_id = ?", org).Group("currency, type").Scan(&sums)
	balances := map[string]int64{s.Currency: s.OpeningBalance}
	var incomeCount, expenseCount int64
	for _, a := range sums {
		if a.Type == TypeIncome {
			balances[a.Currency] += a.Total
			incomeCount += a.N
		} else {
			balances[a.Currency] -= a.Total
			expenseCount += a.N
		}
	}
	balanceList := []gin.H{}
	for k, v := range balances {
		if k != cur && v == 0 {
			continue
		}
		balanceList = append(balanceList, gin.H{"currency": k, "decimals": decimalsFor(k), "balance": v})
	}

	var recent []Entry
	h.DB.Model(&Entry{}).Select(entryCols).Where("organization_id = ?", org).Order("date DESC, created_at DESC").Limit(8).Find(&recent)
	h.decorate(org, recent, names)

	var reminders []Reminder
	h.DB.Where("organization_id = ? AND done = ? AND due_date < ?", org, false, today.AddDate(0, 0, 31)).Order("due_date ASC").Limit(8).Find(&reminders)
	var overdue, reminderCount int64
	h.DB.Model(&Reminder{}).Where("organization_id = ? AND done = ? AND due_date < ?", org, false, today).Count(&overdue)
	h.DB.Model(&Reminder{}).Where("organization_id = ?", org).Count(&reminderCount)

	others := []string{}
	seen := map[string]bool{cur: true}
	for _, e := range entries {
		if !seen[e.Currency] {
			seen[e.Currency] = true
			others = append(others, e.Currency)
		}
	}

	ok(c, http.StatusOK, gin.H{
		"currency": cur, "decimals": decimalsFor(cur), "base_currency": s.Currency, "other_currencies": others,
		"tax_name": s.TaxName, "tax_registered": s.TaxRegistered,
		"this_month":   gin.H{"period": periodView(thisM), "totals": Summarize(pick(thisM), cur)},
		"last_month":   gin.H{"period": periodView(lastM), "totals": Summarize(pick(lastM), cur)},
		"tax":          gin.H{"period": periodView(taxP), "totals": Summarize(pick(taxP), cur)},
		"top_expenses": gin.H{"period": periodView(topP), "categories": ByCategory(pick(topP), cur, TypeExpense, names)},
		"trend":        MonthlyTotals(entries, cur, trendFrom, thisM.To),
		"balances":     balanceList, "cash_position": balances[cur],
		"recent": recent, "reminders": reminderViews(reminders, today), "overdue_reminders": overdue,
		"checklist": gin.H{"profile": s.Configured, "income": incomeCount > 0, "expense": expenseCount > 0, "reminder": reminderCount > 0},
	})
}

type catRow struct {
	CategoryID string  `json:"category_id"`
	Name       string  `json:"name"`
	Months     []int64 `json:"months"`
	Total      int64   `json:"total"`
}

func categoryMatrix(entries []Entry, cur, typ string, months []MonthRow, names map[string]string) []catRow {
	idx := map[string]int{}
	for i, m := range months {
		idx[m.Month] = i
	}
	rows := map[string]*catRow{}
	for _, ct := range ByCategory(entries, cur, typ, names) {
		rows[ct.CategoryID] = &catRow{CategoryID: ct.CategoryID, Name: ct.Name, Months: make([]int64, len(months)), Total: ct.Amount}
	}
	for _, e := range entries {
		if e.Type != typ || e.Currency != cur {
			continue
		}
		if i, found := idx[e.Date.Format("2006-01")]; found {
			rows[e.CategoryID].Months[i] += e.Amount - e.Tax
		}
	}
	out := []catRow{}
	for _, ct := range ByCategory(entries, cur, typ, names) {
		out = append(out, *rows[ct.CategoryID])
	}
	return out
}

func (h *Handler) ProfitAndLoss(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	cur := strings.ToUpper(c.DefaultQuery("currency", s.Currency))
	if !currency.IsSupported(cur) {
		failErr(c, invalid(map[string]string{"currency": "Unsupported currency"}), "")
		return
	}
	today := h.today()
	start := FYStartDate(today, s.FinancialYearStart)
	if fy := c.Query("fy"); fy != "" {
		y, err := strconv.Atoi(fy)
		if err != nil || y < 1970 || y > 2200 {
			failErr(c, invalid(map[string]string{"fy": "Invalid financial year"}), "")
			return
		}
		start = time.Date(y, start.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	end := start.AddDate(1, 0, -1)
	entries, err := h.loadEntries(org, start, end.AddDate(0, 0, 1))
	if err != nil {
		failErr(c, err, "Entry")
		return
	}
	_, names, _ := h.categories(org)
	months := MonthlyTotals(entries, cur, start, end)
	income := categoryMatrix(entries, cur, TypeIncome, months, names)
	expenses := categoryMatrix(entries, cur, TypeExpense, months, names)
	totals := Summarize(entries, cur)

	// financial years that have data, for the picker
	var first, last time.Time
	h.DB.Model(&Entry{}).Select("MIN(date)").Where("organization_id = ?", org).Scan(&first)
	h.DB.Model(&Entry{}).Select("MAX(date)").Where("organization_id = ?", org).Scan(&last)
	years := []gin.H{}
	cy := FYStartDate(today, s.FinancialYearStart)
	lo, hi := cy, cy
	if !first.IsZero() {
		if f := FYStartDate(first, s.FinancialYearStart); f.Before(lo) {
			lo = f
		}
		if l := FYStartDate(last, s.FinancialYearStart); l.After(hi) {
			hi = l
		}
	}
	for y := hi; !y.Before(lo); y = y.AddDate(-1, 0, 0) {
		years = append(years, gin.H{"year": y.Year(), "label": FYLabel(y)})
	}

	if c.Query("format") == "csv" {
		d := decimalsFor(cur)
		head := []string{"Category"}
		for _, m := range months {
			t, _ := time.Parse("2006-01", m.Month)
			head = append(head, t.Format("Jan 2006"))
		}
		head = append(head, "Total")
		out := [][]string{{"Profit & loss " + FYLabel(start) + " (" + cur + ", excl. tax)"}, head, {"Income"}}
		line := func(name string, vals []int64, total int64) []string {
			r := []string{csvSafe(name)}
			for _, v := range vals {
				r = append(r, FormatMinor(v, d))
			}
			return append(r, FormatMinor(total, d))
		}
		col := func(fn func(MonthRow) int64) []int64 {
			v := make([]int64, len(months))
			for i, m := range months {
				v[i] = fn(m)
			}
			return v
		}
		for _, r := range income {
			out = append(out, line(r.Name, r.Months, r.Total))
		}
		out = append(out, line("Total income", col(func(m MonthRow) int64 { return m.IncomeNet }), totals.IncomeNet), []string{"Expenses"})
		for _, r := range expenses {
			out = append(out, line(r.Name, r.Months, r.Total))
		}
		out = append(out, line("Total expenses", col(func(m MonthRow) int64 { return m.ExpensesNet }), totals.ExpensesNet))
		out = append(out, line("Net profit", col(func(m MonthRow) int64 { return m.Profit }), totals.Profit))
		out = append(out, []string{}, line(s.TaxName+" collected", col(func(m MonthRow) int64 { return m.TaxCollected }), totals.TaxCollected))
		out = append(out, line(s.TaxName+" paid", col(func(m MonthRow) int64 { return m.TaxPaid }), totals.TaxPaid))
		writeCSV(c, fmt.Sprintf("profit-loss-%s.csv", strings.ReplaceAll(strings.ReplaceAll(FYLabel(start), " ", "-"), "–", "-")), out)
		return
	}
	ok(c, http.StatusOK, gin.H{
		"currency": cur, "decimals": decimalsFor(cur), "tax_name": s.TaxName,
		"fy": start.Year(), "label": FYLabel(start), "from": start.Format("2006-01-02"), "to": end.Format("2006-01-02"),
		"months": months, "income": income, "expenses": expenses, "totals": totals, "years": years,
		"business": gin.H{"trading_name": s.TradingName, "tax_number": s.TaxNumber},
	})
}

func (h *Handler) TaxReport(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	cur := strings.ToUpper(c.DefaultQuery("currency", s.Currency))
	if !currency.IsSupported(cur) {
		failErr(c, invalid(map[string]string{"currency": "Unsupported currency"}), "")
		return
	}
	p, err := ResolvePeriod(c.DefaultQuery("period", "this_quarter"), c.Query("from"), c.Query("to"), h.today(), s.FinancialYearStart)
	if err != nil {
		failErr(c, invalid(map[string]string{"period": err.Error()}), "")
		return
	}
	entries, err := h.loadEntries(org, p.From, p.Until())
	if err != nil {
		failErr(c, err, "Entry")
		return
	}
	totals := Summarize(entries, cur)
	rates := TaxByRate(entries, cur)
	if c.Query("format") == "csv" {
		d := decimalsFor(cur)
		out := [][]string{
			{s.TaxName + " summary", p.Label, p.From.Format("2006-01-02") + " to " + p.To.Format("2006-01-02"), cur},
			{"Rate %", "Sales (incl. tax)", s.TaxName + " collected", "Purchases (incl. tax)", s.TaxName + " paid"},
		}
		for _, r := range rates {
			out = append(out, []string{strconv.FormatFloat(r.Rate, 'f', -1, 64), FormatMinor(r.Sales, d), FormatMinor(r.TaxCollected, d), FormatMinor(r.Purchases, d), FormatMinor(r.TaxPaid, d)})
		}
		out = append(out, []string{"Total", FormatMinor(totals.Income, d), FormatMinor(totals.TaxCollected, d), FormatMinor(totals.Expenses, d), FormatMinor(totals.TaxPaid, d)})
		out = append(out, []string{"Net " + s.TaxName + " payable (refund if negative)", FormatMinor(totals.TaxPayable, d)})
		writeCSV(c, fmt.Sprintf("tax-summary-%s-%s.csv", p.From.Format("2006-01-02"), p.To.Format("2006-01-02")), out)
		return
	}
	ok(c, http.StatusOK, gin.H{
		"currency": cur, "decimals": decimalsFor(cur), "tax_name": s.TaxName, "tax_number": s.TaxNumber,
		"period": periodView(p), "totals": totals, "rates": rates,
	})
}

// ---------- reminders ----------

type reminderView struct {
	Reminder
	Decimals int  `json:"decimals"`
	Overdue  bool `json:"overdue"`
	DaysLeft int  `json:"days_left"`
}

func reminderViews(rs []Reminder, today time.Time) []reminderView {
	out := make([]reminderView, 0, len(rs))
	for _, r := range rs {
		days := int(dateOnly(r.DueDate).Sub(today).Hours() / 24)
		out = append(out, reminderView{Reminder: r, Decimals: decimalsFor(r.Currency), Overdue: !r.Done && days < 0, DaysLeft: days})
	}
	return out
}

func (h *Handler) ListReminders(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	q := h.DB.Where("organization_id = ?", org)
	switch c.DefaultQuery("status", "all") {
	case "open":
		q = q.Where("done = ?", false)
	case "done":
		q = q.Where("done = ?", true)
	case "overdue":
		q = q.Where("done = ? AND due_date < ?", false, h.today())
	}
	var rs []Reminder
	if err := q.Order("done ASC, due_date ASC, created_at ASC").Limit(500).Find(&rs).Error; err != nil {
		failErr(c, err, "Reminder")
		return
	}
	ok(c, http.StatusOK, gin.H{"reminders": reminderViews(rs, h.today())})
}

type reminderInput struct {
	Title      string     `json:"title"`
	Notes      string     `json:"notes"`
	DueDate    string     `json:"due_date"`
	Amount     flexAmount `json:"amount"`
	Currency   string     `json:"currency"`
	Recurrence string     `json:"recurrence"`
}

func (h *Handler) applyReminder(s *Settings, in *reminderInput, r *Reminder) error {
	fields := map[string]string{}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		fields["title"] = "What do you need to remember?"
	} else if len(title) > 200 {
		fields["title"] = "Title is too long"
	}
	d, err := ParseDate(in.DueDate)
	if err != nil {
		fields["due_date"] = "Pick a due date"
	}
	rec := in.Recurrence
	if rec == "" {
		rec = "none"
	}
	if rec != "none" && RecurrenceMonths(rec) == 0 {
		fields["recurrence"] = "Choose how often it repeats"
	}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = s.Currency
	}
	if !currency.IsSupported(cur) {
		fields["currency"] = "Choose a supported currency"
	}
	var amount int64
	if strings.TrimSpace(in.Amount.Value) != "" && currency.IsSupported(cur) {
		if amount, err = ParseMinor(in.Amount.Value, decimalsFor(cur)); err != nil {
			fields["amount"] = "Amount: " + err.Error()
		} else if amount < 0 {
			fields["amount"] = "Amount can't be negative"
		}
	}
	if len(in.Notes) > 2000 {
		fields["notes"] = "Notes are too long"
	}
	if len(fields) > 0 {
		return invalid(fields)
	}
	if !r.DueDate.Equal(d) || r.AnchorDay == 0 {
		r.AnchorDay = d.Day()
	}
	r.Title, r.Notes, r.DueDate, r.Amount, r.Currency, r.Recurrence = title, strings.TrimSpace(in.Notes), d, amount, cur, rec
	return nil
}

func (h *Handler) CreateReminder(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in reminderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	r := Reminder{ID: uuid.NewString(), OrganizationID: org}
	if err := h.applyReminder(s, &in, &r); err != nil {
		failErr(c, err, "Reminder")
		return
	}
	if err := h.DB.Create(&r).Error; err != nil {
		failErr(c, err, "Reminder")
		return
	}
	ok(c, http.StatusCreated, reminderViews([]Reminder{r}, h.today())[0])
}

func (h *Handler) UpdateReminder(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in reminderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	s, err := h.ensure(org)
	if err != nil {
		failErr(c, err, "Settings")
		return
	}
	var r Reminder
	if err := h.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).First(&r).Error; err != nil {
		failErr(c, err, "Reminder")
		return
	}
	if err := h.applyReminder(s, &in, &r); err != nil {
		failErr(c, err, "Reminder")
		return
	}
	if err := h.DB.Save(&r).Error; err != nil {
		failErr(c, err, "Reminder")
		return
	}
	ok(c, http.StatusOK, reminderViews([]Reminder{r}, h.today())[0])
}

func (h *Handler) DeleteReminder(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	r := h.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).Delete(&Reminder{})
	if r.Error != nil {
		failErr(c, r.Error, "Reminder")
		return
	}
	if r.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Reminder not found")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// ToggleReminder flips done; completing a recurring reminder schedules the next one (once).
func (h *Handler) ToggleReminder(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var r Reminder
	var next *Reminder
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ? AND id = ?", org, c.Param("id")).First(&r).Error; err != nil {
			return err
		}
		r.Done = !r.Done
		if r.Done {
			now := time.Now()
			r.DoneAt = &now
			if due, recurring := NextDue(r.DueDate, r.Recurrence, r.AnchorDay); recurring && r.NextID == "" {
				n := Reminder{ID: uuid.NewString(), OrganizationID: org, Title: r.Title, Notes: r.Notes, DueDate: due, Amount: r.Amount,
					Currency: r.Currency, Recurrence: r.Recurrence, AnchorDay: r.AnchorDay}
				if err := tx.Create(&n).Error; err != nil {
					return err
				}
				r.NextID = n.ID
				next = &n
			}
		} else {
			r.DoneAt = nil
		}
		return tx.Save(&r).Error
	})
	if err != nil {
		failErr(c, err, "Reminder")
		return
	}
	today := h.today()
	res := gin.H{"reminder": reminderViews([]Reminder{r}, today)[0]}
	if next != nil {
		res["next"] = reminderViews([]Reminder{*next}, today)[0]
	}
	ok(c, http.StatusOK, res)
}
