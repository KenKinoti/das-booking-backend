package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
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

// summarise attaches vehicle/booking counts and booking dates to customers.
func (h *Handler) summariseCustomers(orgID string, customers []models.Customer) []customerSummary {
	out := make([]customerSummary, len(customers))
	if len(customers) == 0 {
		return out
	}
	ids := make([]string, len(customers))
	for i, cu := range customers {
		cu.Vehicles = nil
		out[i] = customerSummary{Customer: cu}
		ids[i] = cu.ID
	}

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
			MAX(CASE WHEN start_time <= ? THEN start_time END) AS last,
			MIN(CASE WHEN start_time > ? AND status NOT IN ('cancelled','no_show') THEN start_time END) AS next,
			COALESCE(SUM(CASE WHEN status = 'completed' THEN total_price ELSE 0 END), 0) AS spent`, now, now).
		Where("organization_id = ? AND customer_id IN ?", orgID, ids).Group("customer_id").Scan(&brows)

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
			out[i].TotalSpent = b.Spent
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
		query = query.Where("(first_name ILIKE ? OR last_name ILIKE ? OR (first_name || ' ' || last_name) ILIKE ? OR email ILIKE ? OR phone ILIKE ? OR address_suburb ILIKE ?)",
			like, like, like, like, like, like)
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
	c.JSON(http.StatusOK, gin.H{"customer": summary, "vehicles": vehicles, "bookings": rows})
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
	if customer.Address.Country == "" {
		customer.Address.Country = "Australia"
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
	monthStart := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Now().Location())
	base().Where("created_at >= ?", monthStart).Count(&stats.NewThisMonth)
	h.DB.Model(&models.Booking{}).Where("organization_id = ? AND start_time > ? AND status NOT IN ?", orgID, time.Now(), []string{"cancelled", "no_show"}).
		Distinct("customer_id").Count(&stats.WithUpcoming)
	h.DB.Model(&models.Vehicle{}).Where("organization_id = ?", orgID).Count(&stats.TotalVehicles)
	h.DB.Model(&models.Booking{}).Where("organization_id = ? AND status = ?", orgID, "completed").
		Select("COALESCE(SUM(total_price), 0)").Row().Scan(&stats.Revenue)

	c.JSON(http.StatusOK, gin.H{"stats": stats})
}
