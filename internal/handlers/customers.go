package handlers

import (
	"math"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
	"gorm.io/gorm"
)

// customerInput is the writable part of a customer. Pointers let PUT
// distinguish "not sent" from "cleared" so fields can be emptied.
type customerInput struct {
	FirstName   *string         `json:"first_name"`
	LastName    *string         `json:"last_name"`
	Email       *string         `json:"email"`
	Phone       *string         `json:"phone"`
	DateOfBirth *string         `json:"date_of_birth"`
	Address     *models.Address `json:"address"`
	CountryCode *string         `json:"country_code"`
	Currency    *string         `json:"currency"`
	Notes       *string         `json:"notes"`
	IsActive    *bool           `json:"is_active"`
}

// customerSummary adds activity figures to a customer row for list views.
type customerSummary struct {
	models.Customer
	Organization  *struct{}  `json:"organization,omitempty"`
	VehiclesCount int64      `json:"vehicles_count"`
	BookingsCount int64      `json:"bookings_count"`
	LastBookingAt *time.Time `json:"last_booking_at,omitempty"`
	NextBookingAt *time.Time `json:"next_booking_at,omitempty"`
	TotalSpent    float64    `json:"total_spent"`
	ContactsCount int64      `json:"contacts_count"`
	// BillingCurrency is the currency new invoices for this customer use:
	// the customer's own choice, else their country's, else the org default.
	BillingCurrency string `json:"billing_currency"`
	CurrencySource  string `json:"currency_source"` // customer | country | default
	CountryName     string `json:"country_name"`
}

func customerNotFound(c *gin.Context, err error) {
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customer"})
}

func trimPtr(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

func parseDOB(s string) (*time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return &t, true
		}
	}
	return nil, false
}

// orgDefaultCurrency is the organisation's invoicing currency (invoicing
// settings, else the plan's default currency, else AUD).
func (h *Handler) orgDefaultCurrency(orgID string) string {
	var cur string
	h.DB.Raw("SELECT UPPER(currency) FROM billing_settings WHERE organization_id = ? AND currency <> '' LIMIT 1", orgID).Row().Scan(&cur)
	if currency.IsSupported(cur) {
		return cur
	}
	cur = ""
	h.DB.Raw("SELECT UPPER(COALESCE(NULLIF(default_currency,''), currency)) FROM plan_subscriptions WHERE organization_id = ? LIMIT 1", orgID).Row().Scan(&cur)
	if currency.IsSupported(cur) {
		return cur
	}
	return "AUD"
}

// billingCurrency resolves the currency to invoice a customer in.
func billingCurrency(cu models.Customer, orgDefault string) (string, string) {
	return currency.ForCustomer(cu.Currency, cu.CountryCode, cu.Address.Country, orgDefault)
}

// applyCountryCurrency validates and normalises country_code / currency on a
// customer being created or updated. Returns a user-facing error message.
func applyCountryCurrency(cu *models.Customer, in customerInput) string {
	if in.CountryCode != nil {
		code := strings.ToUpper(strings.TrimSpace(*in.CountryCode))
		if code != "" && !currency.IsCountry(code) {
			return "Choose a country from the list"
		}
		cu.CountryCode = code
		if code != "" {
			cu.Address.Country = currency.CountryName(code)
		} else if in.Address == nil {
			cu.Address.Country = ""
		}
	} else if in.Address != nil {
		// Older clients send only the free-text country name.
		cu.CountryCode = currency.CountryCode(cu.Address.Country)
	}
	if in.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Currency))
		if c != "" && !currency.IsSupported(c) {
			return "Currency " + c + " is not supported"
		}
		cu.Currency = c
	}
	return ""
}

// summarise attaches vehicle/booking counts and booking dates to customers.
func (h *Handler) summariseCustomers(orgID string, customers []models.Customer) []customerSummary {
	out := make([]customerSummary, len(customers))
	if len(customers) == 0 {
		return out
	}
	ids := make([]string, len(customers))
	def := h.orgDefaultCurrency(orgID)
	for i, cu := range customers {
		cu.Vehicles = nil
		out[i] = customerSummary{Customer: cu}
		out[i].BillingCurrency, out[i].CurrencySource = billingCurrency(cu, def)
		out[i].CountryName = currency.CountryName(cu.CountryCode)
		if out[i].CountryName == "" {
			out[i].CountryName = cu.Address.Country
		}
		ids[i] = cu.ID
	}

	var crows []struct {
		CustomerID string
		N          int64
	}
	h.DB.Model(&models.CustomerContact{}).Select("customer_id, COUNT(*) AS n").
		Where("organization_id = ? AND customer_id IN ?", orgID, ids).Group("customer_id").Scan(&crows)

	type vrow struct {
		CustomerID string
		N          int64
	}
	var vrows []vrow
	h.DB.Model(&models.Vehicle{}).Select("customer_id, COUNT(*) AS n").
		Where("organization_id = ? AND customer_id IN ?", orgID, ids).Group("customer_id").Scan(&vrows)

	type brow struct {
		CustomerID string
		N          int64
		Last       *time.Time
		Next       *time.Time
		Spent      float64
	}
	var brows []brow
	now := time.Now()
	h.DB.Model(&models.Booking{}).
		Select(`customer_id, COUNT(*) AS n,
			MAX(CASE WHEN start_time <= ? AND status NOT IN ('cancelled','no_show') THEN start_time END) AS last,
			MIN(CASE WHEN start_time > ? AND status NOT IN ('cancelled','no_show') THEN start_time END) AS next,
			0 AS spent`, now, now).
		Where("organization_id = ? AND customer_id IN ?", orgID, ids).Group("customer_id").Scan(&brows)

	// Spent = money actually received from the customer: payments on their
	// invoices (in the organisation's invoicing currency) plus completed POS
	// sales. Bookings are not payments (a completed booking is usually invoiced).
	type srow struct {
		CustomerID string
		V          float64
	}
	var srows []srow
	h.DB.Raw(`SELECT customer_id, SUM(v) AS v FROM (
			SELECT i.customer_id, COALESCE(SUM(i.amount_paid),0) AS v FROM billing_invoices i
			WHERE i.organization_id = ? AND i.customer_id IN ? AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status <> 'void'
			  AND UPPER(COALESCE(NULLIF(i.currency,''),'AUD')) = COALESCE((SELECT UPPER(currency) FROM billing_settings WHERE organization_id = ? AND currency <> '' LIMIT 1),'AUD')
			GROUP BY 1
			UNION ALL
			SELECT t.customer_id, COALESCE(SUM(t.total_amount),0) FROM pos_transactions t
			WHERE t.organization_id = ? AND t.customer_id IN ? AND t.deleted_at IS NULL AND t.status = 'completed' AND t.type = 'sale'
			GROUP BY 1
		) x GROUP BY customer_id`, orgID, ids, orgID, orgID, ids).Scan(&srows)

	idx := map[string]int{}
	for i, id := range ids {
		idx[id] = i
	}
	for _, v := range vrows {
		if i, ok := idx[v.CustomerID]; ok {
			out[i].VehiclesCount = v.N
		}
	}
	for _, b := range brows {
		if i, ok := idx[b.CustomerID]; ok {
			out[i].BookingsCount = b.N
			out[i].LastBookingAt = b.Last
			out[i].NextBookingAt = b.Next
		}
	}
	for _, cr := range crows {
		if i, ok := idx[cr.CustomerID]; ok {
			out[i].ContactsCount = cr.N
		}
	}
	for _, sp := range srows {
		if i, ok := idx[sp.CustomerID]; ok {
			out[i].TotalSpent = math.Round(sp.V*100) / 100
		}
	}
	return out
}

// GetCustomers lists the organisation's customers with activity summary.
// Query: search, status (active|inactive), sort (name|recent|oldest).
func (h *Handler) GetCustomers(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	query := h.DB.Where("organization_id = ?", orgID)
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		like := "%" + search + "%"
		query = query.Where("(first_name ILIKE ? OR last_name ILIKE ? OR (first_name || ' ' || last_name) ILIKE ? OR email ILIKE ? OR phone ILIKE ? OR address_suburb ILIKE ? OR address_country ILIKE ?)",
			like, like, like, like, like, like, like)
	}
	switch c.Query("status") {
	case "active":
		query = query.Where("is_active = ?", true)
	case "inactive":
		query = query.Where("is_active = ?", false)
	}
	switch c.Query("sort") {
	case "name":
		query = query.Order("first_name ASC, last_name ASC")
	case "oldest":
		query = query.Order("created_at ASC")
	default:
		query = query.Order("created_at DESC")
	}

	var customers []models.Customer
	if err := query.Find(&customers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customers"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"customers": h.summariseCustomers(orgID, customers), "total": len(customers)})
}

// GetCustomer returns one customer with vehicles, bookings and summary.
func (h *Handler) GetCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).
		Preload("Vehicles", func(db *gorm.DB) *gorm.DB { return db.Order("created_at DESC") }).
		First(&customer).Error; err != nil {
		customerNotFound(c, err)
		return
	}

	var bookings []models.Booking
	h.DB.Where("organization_id = ? AND customer_id = ?", orgID, customer.ID).
		Preload("Services").Preload("Vehicle").
		Order("start_time DESC").Limit(100).Find(&bookings)
	type bookingRow struct {
		ID         string     `json:"id"`
		StartTime  time.Time  `json:"start_time"`
		EndTime    time.Time  `json:"end_time"`
		Status     string     `json:"status"`
		TotalPrice float64    `json:"total_price"`
		Notes      string     `json:"notes"`
		Services   []string   `json:"services"`
		Vehicle    string     `json:"vehicle,omitempty"`
		StaffID    *string    `json:"staff_id,omitempty"`
		CreatedAt  *time.Time `json:"created_at,omitempty"`
	}
	rows := make([]bookingRow, 0, len(bookings))
	for _, b := range bookings {
		r := bookingRow{ID: b.ID, StartTime: b.StartTime, EndTime: b.EndTime, Status: b.Status, TotalPrice: b.TotalPrice, Notes: b.Notes, StaffID: b.StaffID, Services: []string{}}
		for _, s := range b.Services {
			r.Services = append(r.Services, s.Name)
		}
		if b.Vehicle != nil {
			r.Vehicle = strings.TrimSpace(b.Vehicle.Make + " " + b.Vehicle.Model + " " + b.Vehicle.LicensePlate)
		}
		created := b.CreatedAt
		r.CreatedAt = &created
		rows = append(rows, r)
	}

	summary := h.summariseCustomers(orgID, []models.Customer{customer})[0]
	vehicles := customer.Vehicles
	if vehicles == nil {
		vehicles = []models.Vehicle{}
	}
	contacts := []models.CustomerContact{}
	h.DB.Where("organization_id = ? AND customer_id = ?", orgID, customer.ID).Order("is_primary_billing DESC, created_at ASC").Find(&contacts)
	c.JSON(http.StatusOK, gin.H{"customer": summary, "vehicles": vehicles, "bookings": rows, "contacts": contacts})
}

// CreateCustomer creates a new customer.
func (h *Handler) CreateCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var in customerInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid customer data: " + err.Error()})
		return
	}
	customer := models.Customer{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		FirstName:      trimPtr(in.FirstName),
		LastName:       trimPtr(in.LastName),
		Email:          strings.ToLower(trimPtr(in.Email)),
		Phone:          trimPtr(in.Phone),
		Notes:          trimPtr(in.Notes),
		IsActive:       true,
	}
	if in.Address != nil {
		customer.Address = *in.Address
	}
	if msg := applyCountryCurrency(&customer, in); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if customer.Address.Country == "" && customer.CountryCode == "" {
		customer.Address.Country = "Australia"
		customer.CountryCode = "AU"
	}
	if in.DateOfBirth != nil {
		dob, ok := parseDOB(*in.DateOfBirth)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Date of birth must be YYYY-MM-DD"})
			return
		}
		customer.DateOfBirth = dob
	}
	if customer.FirstName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "First name is required"})
		return
	}
	if customer.Email == "" && customer.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Add an email address or a phone number"})
		return
	}

	if customer.Email != "" {
		var n int64
		h.DB.Model(&models.Customer{}).Where("LOWER(email) = ? AND organization_id = ?", customer.Email, orgID).Count(&n)
		if n > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "A customer with this email already exists"})
			return
		}
	}

	if err := h.DB.Omit("Organization", "Vehicles", "Bookings").Create(&customer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create customer"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"customer": h.summariseCustomers(orgID, []models.Customer{customer})[0]})
}

// UpdateCustomer updates the fields that were sent.
func (h *Handler) UpdateCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&customer).Error; err != nil {
		customerNotFound(c, err)
		return
	}

	var in customerInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid customer data: " + err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if in.FirstName != nil {
		if trimPtr(in.FirstName) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "First name is required"})
			return
		}
		updates["first_name"] = trimPtr(in.FirstName)
	}
	if in.LastName != nil {
		updates["last_name"] = trimPtr(in.LastName)
	}
	if in.Email != nil {
		email := strings.ToLower(trimPtr(in.Email))
		if email != "" && !strings.EqualFold(email, customer.Email) {
			var n int64
			h.DB.Model(&models.Customer{}).Where("LOWER(email) = ? AND organization_id = ? AND id <> ?", email, orgID, id).Count(&n)
			if n > 0 {
				c.JSON(http.StatusConflict, gin.H{"error": "A customer with this email already exists"})
				return
			}
		}
		updates["email"] = email
	}
	if in.Phone != nil {
		updates["phone"] = trimPtr(in.Phone)
	}
	if in.Notes != nil {
		updates["notes"] = *in.Notes
	}
	if in.IsActive != nil {
		updates["is_active"] = *in.IsActive
	}
	if in.DateOfBirth != nil {
		dob, ok := parseDOB(*in.DateOfBirth)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Date of birth must be YYYY-MM-DD"})
			return
		}
		updates["date_of_birth"] = dob
	}
	if in.Address != nil {
		updates["address_street"] = in.Address.Street
		updates["address_suburb"] = in.Address.Suburb
		updates["address_state"] = in.Address.State
		updates["address_postcode"] = in.Address.Postcode
		updates["address_country"] = in.Address.Country
	}
	if in.Address != nil || in.CountryCode != nil || in.Currency != nil {
		next := customer
		if in.Address != nil {
			next.Address = *in.Address
		}
		if msg := applyCountryCurrency(&next, in); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		if in.Address != nil || in.CountryCode != nil {
			updates["address_country"] = next.Address.Country
			updates["country_code"] = next.CountryCode
		}
		updates["currency"] = next.Currency
	}

	if len(updates) > 0 {
		if err := h.DB.Model(&models.Customer{}).Where("id = ? AND organization_id = ?", id, orgID).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update customer"})
			return
		}
	}
	h.DB.Where("id = ?", id).First(&customer)
	c.JSON(http.StatusOK, gin.H{"customer": h.summariseCustomers(orgID, []models.Customer{customer})[0]})
}

// DeleteCustomer soft deletes a customer that has no upcoming bookings.
func (h *Handler) DeleteCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&customer).Error; err != nil {
		customerNotFound(c, err)
		return
	}

	var active int64
	h.DB.Model(&models.Booking{}).Where("organization_id = ? AND customer_id = ? AND status NOT IN ?",
		orgID, id, []string{"completed", "cancelled", "no_show"}).Count(&active)
	if active > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "This customer has open bookings. Complete or cancel them first, or deactivate the customer instead."})
		return
	}

	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("organization_id = ? AND customer_id = ?", orgID, id).Delete(&models.Vehicle{}).Error; err != nil {
			return err
		}
		if err := tx.Where("organization_id = ? AND customer_id = ?", orgID, id).Delete(&models.CustomerContact{}).Error; err != nil {
			return err
		}
		return tx.Delete(&customer).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete customer"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Customer deleted"})
}

// ToggleCustomerStatus flips a customer between active and inactive.
func (h *Handler) ToggleCustomerStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).First(&customer).Error; err != nil {
		customerNotFound(c, err)
		return
	}
	customer.IsActive = !customer.IsActive
	if err := h.DB.Model(&models.Customer{}).Where("id = ?", customer.ID).Update("is_active", customer.IsActive).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update customer status"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"customer": h.summariseCustomers(orgID, []models.Customer{customer})[0]})
}

// GetCustomerStats returns headline customer figures.
func (h *Handler) GetCustomerStats(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var stats struct {
		TotalCustomers    int64   `json:"total_customers"`
		ActiveCustomers   int64   `json:"active_customers"`
		InactiveCustomers int64   `json:"inactive_customers"`
		NewThisMonth      int64   `json:"new_this_month"`
		WithUpcoming      int64   `json:"with_upcoming_bookings"`
		TotalVehicles     int64   `json:"total_vehicles"`
		Revenue           float64 `json:"completed_booking_revenue"`
	}
	base := func() *gorm.DB { return h.DB.Model(&models.Customer{}).Where("organization_id = ?", orgID) }
	base().Count(&stats.TotalCustomers)
	base().Where("is_active = ?", true).Count(&stats.ActiveCustomers)
	stats.InactiveCustomers = stats.TotalCustomers - stats.ActiveCustomers
	monthStart := reqtz.MonthStart(c) // the user's calendar month (?tz=)
	base().Where("created_at >= ?", monthStart).Count(&stats.NewThisMonth)
	h.DB.Model(&models.Booking{}).Where("organization_id = ? AND start_time > ? AND status NOT IN ?", orgID, time.Now(), []string{"cancelled", "no_show"}).
		Distinct("customer_id").Count(&stats.WithUpcoming)
	h.DB.Model(&models.Vehicle{}).Where("organization_id = ?", orgID).Count(&stats.TotalVehicles)
	h.DB.Model(&models.Booking{}).Where("organization_id = ? AND status = ?", orgID, "completed").
		Select("COALESCE(SUM(total_price), 0)").Row().Scan(&stats.Revenue)

	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

// ---------- customer contacts (CC recipients) ----------

type contactInput struct {
	Name             *string `json:"name"`
	Email            *string `json:"email"`
	Phone            *string `json:"phone"`
	Role             *string `json:"role"`
	CCInvoices       *bool   `json:"cc_invoices"`
	CCQuotes         *bool   `json:"cc_quotes"`
	CCReminders      *bool   `json:"cc_reminders"`
	CCBookings       *bool   `json:"cc_bookings"`
	IsPrimaryBilling *bool   `json:"is_primary_billing"`
	Notes            *string `json:"notes"`
}

func (in contactInput) apply(ct *models.CustomerContact) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&ct.Name, in.Name)
	set(&ct.Phone, in.Phone)
	set(&ct.Role, in.Role)
	if in.Notes != nil {
		ct.Notes = *in.Notes
	}
	if in.Email != nil {
		ct.Email = strings.ToLower(strings.TrimSpace(*in.Email))
	}
	for _, f := range []struct {
		dst *bool
		src *bool
	}{{&ct.CCInvoices, in.CCInvoices}, {&ct.CCQuotes, in.CCQuotes}, {&ct.CCReminders, in.CCReminders}, {&ct.CCBookings, in.CCBookings}, {&ct.IsPrimaryBilling, in.IsPrimaryBilling}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
}

// contactCustomer loads the customer in the caller's organisation.
func (h *Handler) contactCustomer(c *gin.Context) (string, *models.Customer, bool) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return "", nil, false
	}
	var cu models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).First(&cu).Error; err != nil {
		customerNotFound(c, err)
		return "", nil, false
	}
	return orgID, &cu, true
}

// validateContact checks the email and that it isn't already a contact of the
// same customer (case-insensitive). Returns status and message on failure.
func (h *Handler) validateContact(ct *models.CustomerContact) (int, string) {
	if ct.Email == "" {
		return http.StatusBadRequest, "Email is required"
	}
	if addr, err := mail.ParseAddress(ct.Email); err != nil || addr.Address != ct.Email || !strings.Contains(ct.Email[strings.LastIndex(ct.Email, "@"):], ".") {
		return http.StatusBadRequest, "Enter a valid email address"
	}
	if len(ct.Name) > 200 || len(ct.Role) > 120 || len(ct.Phone) > 50 {
		return http.StatusBadRequest, "Name, role or phone is too long"
	}
	var n int64
	q := h.DB.Model(&models.CustomerContact{}).Where("organization_id = ? AND customer_id = ? AND LOWER(email) = ?", ct.OrganizationID, ct.CustomerID, ct.Email)
	if ct.ID != "" {
		q = q.Where("id <> ?", ct.ID)
	}
	q.Count(&n)
	if n > 0 {
		return http.StatusConflict, "This email is already a contact for this customer"
	}
	return 0, ""
}

// saveContact persists ct, making it the only primary billing contact if set.
func (h *Handler) saveContact(ct *models.CustomerContact, create bool) error {
	return h.DB.Transaction(func(tx *gorm.DB) error {
		if ct.IsPrimaryBilling {
			if err := tx.Model(&models.CustomerContact{}).Where("organization_id = ? AND customer_id = ? AND id <> ?", ct.OrganizationID, ct.CustomerID, ct.ID).
				Update("is_primary_billing", false).Error; err != nil {
				return err
			}
		}
		if create {
			// Select("*") so false booleans aren't replaced by column defaults.
			return tx.Select("*").Create(ct).Error
		}
		return tx.Model(&models.CustomerContact{}).Where("id = ? AND organization_id = ?", ct.ID, ct.OrganizationID).
			Select("name", "email", "phone", "role", "cc_invoices", "cc_quotes", "cc_reminders", "cc_bookings", "is_primary_billing", "notes", "updated_at").
			Updates(ct).Error
	})
}

// ListCustomerContacts GET /customers/:id/contacts
func (h *Handler) ListCustomerContacts(c *gin.Context) {
	orgID, cu, ok := h.contactCustomer(c)
	if !ok {
		return
	}
	list := []models.CustomerContact{}
	if err := h.DB.Where("organization_id = ? AND customer_id = ?", orgID, cu.ID).Order("is_primary_billing DESC, created_at ASC").Find(&list).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load contacts"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"contacts": list, "total": len(list)})
}

// CreateCustomerContact POST /customers/:id/contacts
func (h *Handler) CreateCustomerContact(c *gin.Context) {
	orgID, cu, ok := h.contactCustomer(c)
	if !ok {
		return
	}
	var in contactInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid contact: " + err.Error()})
		return
	}
	ct := models.CustomerContact{ID: uuid.NewString(), CustomerID: cu.ID, OrganizationID: orgID, CCInvoices: true}
	in.apply(&ct)
	if status, msg := h.validateContact(&ct); status != 0 {
		ct.ID = ""
		c.JSON(status, gin.H{"error": msg})
		return
	}
	if err := h.saveContact(&ct, true); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save contact"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"contact": ct})
}

// UpdateCustomerContact PUT /customers/:id/contacts/:contactId
func (h *Handler) UpdateCustomerContact(c *gin.Context) {
	orgID, cu, ok := h.contactCustomer(c)
	if !ok {
		return
	}
	var ct models.CustomerContact
	if err := h.DB.Where("id = ? AND customer_id = ? AND organization_id = ?", c.Param("contactId"), cu.ID, orgID).First(&ct).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Contact not found"})
		return
	}
	var in contactInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid contact: " + err.Error()})
		return
	}
	in.apply(&ct)
	if status, msg := h.validateContact(&ct); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	ct.UpdatedAt = time.Now()
	if err := h.saveContact(&ct, false); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save contact"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"contact": ct})
}

// DeleteCustomerContact DELETE /customers/:id/contacts/:contactId (soft delete)
func (h *Handler) DeleteCustomerContact(c *gin.Context) {
	orgID, cu, ok := h.contactCustomer(c)
	if !ok {
		return
	}
	res := h.DB.Where("id = ? AND customer_id = ? AND organization_id = ?", c.Param("contactId"), cu.ID, orgID).Delete(&models.CustomerContact{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete contact"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Contact not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Contact removed"})
}

// RegisterCustomerContactRoutes mounts the contact CRUD under /customers/:id/contacts.
func (h *Handler) RegisterCustomerContactRoutes(r *gin.RouterGroup) {
	r.GET("/customers/:id/contacts", h.ListCustomerContacts)
	r.POST("/customers/:id/contacts", h.CreateCustomerContact)
	r.PUT("/customers/:id/contacts/:contactId", h.UpdateCustomerContact)
	r.DELETE("/customers/:id/contacts/:contactId", h.DeleteCustomerContact)
}

// BackfillCustomerCountries sets country_code from the free-text country name
// where it is recognisable (e.g. "Australia" → AU, "Kenya" → KE).
func BackfillCustomerCountries(db *gorm.DB) (int64, error) {
	var names []string
	if err := db.Model(&models.Customer{}).Where("(country_code IS NULL OR country_code = '') AND COALESCE(address_country,'') <> ''").
		Distinct("address_country").Pluck("address_country", &names).Error; err != nil {
		return 0, err
	}
	var total int64
	for _, name := range names {
		code := currency.CountryCode(name)
		if code == "" {
			continue
		}
		res := db.Model(&models.Customer{}).Where("(country_code IS NULL OR country_code = '') AND address_country = ?", name).
			Update("country_code", code)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
	return total, nil
}
