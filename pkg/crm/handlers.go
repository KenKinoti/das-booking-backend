package crm

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func orgOf(c *gin.Context) (string, bool) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = c.GetString("org_id")
	}
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "Organization not found"})
		return "", false
	}
	return orgID, true
}

func fail(c *gin.Context, err error, what string) {
	if errors.Is(err, ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": what + " not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Could not save " + strings.ToLower(what)})
}

func bad(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": msg})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// parseDate accepts YYYY-MM-DD or RFC3339; "" means nil.
func parseDate(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if len(v) >= 10 {
		if t, err := time.Parse("2006-01-02", v[:10]); err == nil {
			return &t, nil
		}
	}
	return nil, errors.New("invalid date")
}

// ---- Leads ----

type leadInput struct {
	FirstName      *string  `json:"first_name"`
	LastName       *string  `json:"last_name"`
	Email          *string  `json:"email"`
	Phone          *string  `json:"phone"`
	Company        *string  `json:"company"`
	JobTitle       *string  `json:"job_title"`
	Source         *string  `json:"source"`
	Status         *string  `json:"status"`
	Rating         *string  `json:"rating"`
	EstimatedValue *float64 `json:"estimated_value"`
	Notes          *string  `json:"notes"`
}

func (in leadInput) updates(create bool) (map[string]interface{}, string) {
	u := map[string]interface{}{}
	str := func(key string, p *string) {
		if p != nil {
			u[key] = strings.TrimSpace(*p)
		}
	}
	str("first_name", in.FirstName)
	str("last_name", in.LastName)
	str("email", in.Email)
	str("phone", in.Phone)
	str("company", in.Company)
	str("job_title", in.JobTitle)
	str("source", in.Source)
	str("rating", in.Rating)
	if in.Notes != nil {
		u["notes"] = *in.Notes
	}
	if (create || in.FirstName != nil) && strings.TrimSpace(deref(in.FirstName)) == "" {
		return nil, "First name is required"
	}
	if e := strings.TrimSpace(deref(in.Email)); e != "" && !strings.Contains(e, "@") {
		return nil, "Enter a valid email address"
	}
	if in.Status != nil {
		if !contains(LeadStatuses, *in.Status) {
			return nil, "Invalid lead status"
		}
		u["status"] = *in.Status
	}
	if in.Rating != nil && *in.Rating != "" && !contains([]string{"hot", "warm", "cold"}, *in.Rating) {
		return nil, "Rating must be hot, warm or cold"
	}
	if in.EstimatedValue != nil {
		if *in.EstimatedValue < 0 {
			return nil, "Estimated value cannot be negative"
		}
		u["estimated_value"] = *in.EstimatedValue
	}
	return u, ""
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (h *Handler) GetLeads(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	leads, err := h.service.GetLeads(orgID, LeadFilter{Status: c.Query("status"), Rating: c.Query("rating"), Source: c.Query("source"), Search: c.Query("search")})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Could not load leads"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": leads})
}

func (h *Handler) CreateLead(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	var in leadInput
	if err := c.ShouldBindJSON(&in); err != nil {
		bad(c, "Invalid lead data")
		return
	}
	if _, msg := in.updates(true); msg != "" {
		bad(c, msg)
		return
	}
	lead := Lead{
		OrganizationID: orgID, FirstName: strings.TrimSpace(deref(in.FirstName)), LastName: strings.TrimSpace(deref(in.LastName)),
		Email: strings.TrimSpace(deref(in.Email)), Phone: strings.TrimSpace(deref(in.Phone)), Company: strings.TrimSpace(deref(in.Company)),
		JobTitle: strings.TrimSpace(deref(in.JobTitle)), Source: deref(in.Source), Status: deref(in.Status), Rating: deref(in.Rating),
		Notes: deref(in.Notes),
	}
	if in.EstimatedValue != nil {
		lead.EstimatedValue = *in.EstimatedValue
	}
	if err := h.service.CreateLead(&lead); err != nil {
		fail(c, err, "Lead")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": lead})
}

func (h *Handler) GetLead(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	lead, err := h.service.GetLead(orgID, c.Param("id"))
	if err != nil {
		fail(c, err, "Lead")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": lead})
}

func (h *Handler) UpdateLead(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	var in leadInput
	if err := c.ShouldBindJSON(&in); err != nil {
		bad(c, "Invalid lead data")
		return
	}
	u, msg := in.updates(false)
	if msg != "" {
		bad(c, msg)
		return
	}
	lead, err := h.service.UpdateLead(orgID, c.Param("id"), u)
	if err != nil {
		fail(c, err, "Lead")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": lead})
}

func (h *Handler) DeleteLead(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	if err := h.service.DeleteLead(orgID, c.Param("id")); err != nil {
		fail(c, err, "Lead")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Lead deleted"})
}

// ConvertLead: POST /leads/:id/convert
// {create_customer: bool, create_opportunity: bool, opportunity_name, value, expected_close_date}
func (h *Handler) ConvertLead(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	var in struct {
		CreateCustomer    *bool   `json:"create_customer"`
		CreateOpportunity bool    `json:"create_opportunity"`
		OpportunityName   string  `json:"opportunity_name"`
		Value             float64 `json:"value"`
		ExpectedCloseDate string  `json:"expected_close_date"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Value < 0 {
		bad(c, "Value cannot be negative")
		return
	}
	closeDate, err := parseDate(in.ExpectedCloseDate)
	if err != nil {
		bad(c, "Expected close date must be YYYY-MM-DD")
		return
	}
	opt := ConvertOptions{CreateCustomer: in.CreateCustomer == nil || *in.CreateCustomer, CreateOpportunity: in.CreateOpportunity,
		OpportunityName: in.OpportunityName, Value: in.Value, ExpectedCloseDate: closeDate}
	lead, err := h.service.GetLead(orgID, c.Param("id"))
	if err != nil {
		fail(c, err, "Lead")
		return
	}
	if lead.Status == "converted" {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "This lead has already been converted"})
		return
	}
	res, err := h.service.ConvertLead(orgID, lead.ID, opt)
	if err != nil {
		fail(c, err, "Lead")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": res})
}

// ---- Legacy CRM customers ----

func (h *Handler) GetCustomers(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	customers, err := h.service.GetCustomers(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Could not load customers"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customers})
}

func (h *Handler) CreateCustomer(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	var customer CRMCustomer
	if err := c.ShouldBindJSON(&customer); err != nil {
		bad(c, "Invalid customer data")
		return
	}
	customer.OrganizationID = orgID
	if err := h.service.CreateCustomer(&customer); err != nil {
		fail(c, err, "Customer")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": customer})
}

// ---- Opportunities ----

type oppInput struct {
	Name              *string  `json:"name"`
	Description       *string  `json:"description"`
	Value             *float64 `json:"value"`
	Stage             *string  `json:"stage"`
	Probability       *int     `json:"probability"`
	ExpectedCloseDate *string  `json:"expected_close_date"`
	LeadID            *string  `json:"lead_id"`
	CustomerID        *string  `json:"customer_id"`
	ContactName       *string  `json:"contact_name"`
	Company           *string  `json:"company"`
	LostReason        *string  `json:"lost_reason"`
	Position          *float64 `json:"position"`
	Notes             *string  `json:"notes"`
}

func nullable(p *string) interface{} {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return strings.TrimSpace(*p)
}

func (in oppInput) updates(create bool) (map[string]interface{}, string) {
	u := map[string]interface{}{}
	if create || in.Name != nil {
		if strings.TrimSpace(deref(in.Name)) == "" {
			return nil, "Deal name is required"
		}
		u["name"] = strings.TrimSpace(deref(in.Name))
	}
	if in.Description != nil {
		u["description"] = *in.Description
	}
	if in.Value != nil {
		if *in.Value < 0 {
			return nil, "Value cannot be negative"
		}
		u["value"] = *in.Value
	}
	if in.Stage != nil {
		if !contains(Stages, *in.Stage) {
			return nil, "Invalid stage"
		}
		u["stage"] = *in.Stage
	}
	if in.Probability != nil {
		if *in.Probability < 0 || *in.Probability > 100 {
			return nil, "Probability must be between 0 and 100"
		}
		u["probability"] = *in.Probability
	}
	if in.ExpectedCloseDate != nil {
		d, err := parseDate(*in.ExpectedCloseDate)
		if err != nil {
			return nil, "Expected close date must be YYYY-MM-DD"
		}
		if d == nil {
			u["expected_close_date"] = nil
		} else {
			u["expected_close_date"] = *d
		}
	}
	if in.LeadID != nil {
		u["lead_id"] = nullable(in.LeadID)
	}
	if in.CustomerID != nil {
		u["customer_id"] = nullable(in.CustomerID)
	}
	if in.ContactName != nil {
		u["contact_name"] = strings.TrimSpace(*in.ContactName)
	}
	if in.Company != nil {
		u["company"] = strings.TrimSpace(*in.Company)
	}
	if in.LostReason != nil {
		u["lost_reason"] = *in.LostReason
	}
	if in.Position != nil {
		u["position"] = *in.Position
	}
	if in.Notes != nil {
		u["notes"] = *in.Notes
	}
	return u, ""
}

func (h *Handler) GetOpportunities(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	opportunities, err := h.service.GetOpportunities(orgID, c.Query("stage"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Could not load opportunities"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": opportunities})
}

func (h *Handler) GetOpportunity(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	o, err := h.service.GetOpportunity(orgID, c.Param("id"))
	if err != nil {
		fail(c, err, "Opportunity")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": o})
}

func (h *Handler) CreateOpportunity(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	var in oppInput
	if err := c.ShouldBindJSON(&in); err != nil {
		bad(c, "Invalid opportunity data")
		return
	}
	if _, msg := in.updates(true); msg != "" {
		bad(c, msg)
		return
	}
	o := Opportunity{OrganizationID: orgID, Name: strings.TrimSpace(deref(in.Name)), Description: deref(in.Description),
		Stage: deref(in.Stage), ContactName: strings.TrimSpace(deref(in.ContactName)), Company: strings.TrimSpace(deref(in.Company)),
		Notes: deref(in.Notes), LostReason: deref(in.LostReason)}
	if in.Value != nil {
		o.Value = *in.Value
	}
	if o.Stage == "" {
		o.Stage = "prospecting"
	}
	if in.Probability != nil {
		o.Probability = *in.Probability
	} else {
		o.Probability = DefaultProbability[o.Stage]
	}
	if in.ExpectedCloseDate != nil {
		o.ExpectedCloseDate, _ = parseDate(*in.ExpectedCloseDate)
	}
	if v, _ := nullable(in.LeadID).(string); v != "" {
		o.LeadID = &v
	}
	if v, _ := nullable(in.CustomerID).(string); v != "" {
		o.CustomerID = &v
	}
	if in.Position != nil {
		o.Position = *in.Position
	}
	if err := h.service.CreateOpportunity(&o); err != nil {
		fail(c, err, "Opportunity")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": o})
}

func (h *Handler) UpdateOpportunity(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	var in oppInput
	if err := c.ShouldBindJSON(&in); err != nil {
		bad(c, "Invalid opportunity data")
		return
	}
	u, msg := in.updates(false)
	if msg != "" {
		bad(c, msg)
		return
	}
	o, err := h.service.UpdateOpportunity(orgID, c.Param("id"), u)
	if err != nil {
		fail(c, err, "Opportunity")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": o})
}

func (h *Handler) DeleteOpportunity(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	if err := h.service.DeleteOpportunity(orgID, c.Param("id")); err != nil {
		fail(c, err, "Opportunity")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Opportunity deleted"})
}

func (h *Handler) GetDashboard(c *gin.Context) {
	orgID, ok := orgOf(c)
	if !ok {
		return
	}
	dashboard, err := h.service.GetDashboard(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Could not load dashboard"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": dashboard})
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	// Leads
	r.GET("/leads", h.GetLeads)
	r.POST("/leads", h.CreateLead)
	r.GET("/leads/:id", h.GetLead)
	r.PUT("/leads/:id", h.UpdateLead)
	r.PATCH("/leads/:id", h.UpdateLead)
	r.DELETE("/leads/:id", h.DeleteLead)
	r.POST("/leads/:id/convert", h.ConvertLead)

	// Legacy CRM customers
	r.GET("/customers", h.GetCustomers)
	r.POST("/customers", h.CreateCustomer)

	// Opportunities (deals)
	r.GET("/opportunities", h.GetOpportunities)
	r.POST("/opportunities", h.CreateOpportunity)
	r.GET("/opportunities/:id", h.GetOpportunity)
	r.PUT("/opportunities/:id", h.UpdateOpportunity)
	r.PATCH("/opportunities/:id", h.UpdateOpportunity)
	r.DELETE("/opportunities/:id", h.DeleteOpportunity)

	// Dashboard
	r.GET("/dashboard", h.GetDashboard)
}
