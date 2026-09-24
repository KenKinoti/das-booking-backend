package crm

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// ErrNotFound is returned when a record does not exist in the organisation.
var ErrNotFound = errors.New("not found")

func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// InitializeSampleData creates sample CRM data (used by the demo monolith).
func (s *Service) InitializeSampleData(orgID string) error {
	leads := []Lead{
		{ID: uuid.New().String(), OrganizationID: orgID, FirstName: "John", LastName: "Smith", Email: "john@example.com",
			Phone: "555-0101", Company: "ABC Corp", Status: "qualified", Rating: "hot"},
		{ID: uuid.New().String(), OrganizationID: orgID, FirstName: "Sarah", LastName: "Johnson", Email: "sarah@example.com",
			Phone: "555-0102", Company: "XYZ Inc", Status: "new", Rating: "warm"},
	}
	return s.db.Create(&leads).Error
}

// LeadFilter narrows GetLeads.
type LeadFilter struct {
	Status string
	Rating string
	Source string
	Search string
}

// ---- Leads ----

func (s *Service) GetLeads(orgID string, f ...LeadFilter) ([]Lead, error) {
	q := s.db.Where("organization_id = ?", orgID)
	if len(f) > 0 {
		lf := f[0]
		switch lf.Status {
		case "", "all":
		case "open":
			q = q.Where("status NOT IN ?", []string{"converted", "unqualified"})
		default:
			q = q.Where("status = ?", lf.Status)
		}
		if lf.Rating != "" {
			q = q.Where("rating = ?", lf.Rating)
		}
		if lf.Source != "" {
			q = q.Where("source = ?", lf.Source)
		}
		if t := strings.TrimSpace(lf.Search); t != "" {
			like := "%" + t + "%"
			q = q.Where("(first_name ILIKE ? OR last_name ILIKE ? OR (first_name || ' ' || last_name) ILIKE ? OR email ILIKE ? OR company ILIKE ? OR phone ILIKE ?)",
				like, like, like, like, like, like)
		}
	}
	leads := []Lead{}
	err := q.Order("created_at desc").Find(&leads).Error
	return leads, err
}

func (s *Service) CreateLead(lead *Lead) error {
	lead.ID = uuid.New().String()
	if lead.Status == "" {
		lead.Status = "new"
	}
	lead.CreatedAt = time.Now()
	lead.UpdatedAt = time.Now()
	return s.db.Create(lead).Error
}

func (s *Service) GetLead(orgID, id string) (*Lead, error) {
	var lead Lead
	err := s.db.First(&lead, "id = ? AND organization_id = ?", id, orgID).Error
	return &lead, notFound(err)
}

func (s *Service) UpdateLead(orgID, id string, updates map[string]interface{}) (*Lead, error) {
	if _, err := s.GetLead(orgID, id); err != nil {
		return nil, err
	}
	updates["updated_at"] = time.Now()
	if err := s.db.Model(&Lead{}).Where("id = ? AND organization_id = ?", id, orgID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetLead(orgID, id)
}

func (s *Service) DeleteLead(orgID, id string) error {
	res := s.db.Where("id = ? AND organization_id = ?", id, orgID).Delete(&Lead{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	// Keep deals but drop the dangling link.
	s.db.Model(&Opportunity{}).Where("organization_id = ? AND lead_id = ?", orgID, id).Update("lead_id", nil)
	return nil
}

// ConvertOptions controls lead conversion.
type ConvertOptions struct {
	CreateCustomer    bool
	CreateOpportunity bool
	OpportunityName   string
	Value             float64
	ExpectedCloseDate *time.Time
}

// ConvertResult is what a conversion created.
type ConvertResult struct {
	Lead        *Lead            `json:"lead"`
	Customer    *models.Customer `json:"customer,omitempty"`
	Opportunity *Opportunity     `json:"opportunity,omitempty"`
}

// ConvertLead marks a lead converted, optionally creating (or matching by
// email) a customer in the core customers table and an opportunity.
func (s *Service) ConvertLead(orgID, id string, opt ConvertOptions) (*ConvertResult, error) {
	lead, err := s.GetLead(orgID, id)
	if err != nil {
		return nil, err
	}
	res := &ConvertResult{}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		updates := map[string]interface{}{"status": "converted", "converted_date": now, "updated_at": now}
		if opt.CreateCustomer {
			var cust models.Customer
			found := false
			if lead.Email != "" {
				found = tx.Where("organization_id = ? AND LOWER(email) = LOWER(?)", orgID, lead.Email).First(&cust).Error == nil
			}
			if !found {
				notes := strings.TrimSpace(strings.Join([]string{lead.Company, lead.JobTitle}, " · "))
				notes = strings.Trim(notes, " ·")
				if lead.Notes != "" {
					if notes != "" {
						notes += "\n"
					}
					notes += lead.Notes
				}
				last := lead.LastName
				cust = models.Customer{
					ID: uuid.NewString(), OrganizationID: orgID, FirstName: lead.FirstName, LastName: last,
					Email: strings.ToLower(lead.Email), Phone: lead.Phone, Notes: notes, IsActive: true,
					Address: models.Address{Country: "Australia"},
				}
				if err := tx.Omit("Organization", "Vehicles", "Bookings").Create(&cust).Error; err != nil {
					return err
				}
			}
			updates["customer_id"] = cust.ID
			res.Customer = &cust
		}
		if err := tx.Model(&Lead{}).Where("id = ?", lead.ID).Updates(updates).Error; err != nil {
			return err
		}
		if opt.CreateOpportunity {
			name := strings.TrimSpace(opt.OpportunityName)
			if name == "" {
				name = strings.TrimSpace(lead.Company)
				if name == "" {
					name = strings.TrimSpace(lead.FirstName + " " + lead.LastName)
				}
			}
			value := opt.Value
			if value == 0 {
				value = lead.EstimatedValue
			}
			opp := Opportunity{
				ID: uuid.NewString(), OrganizationID: orgID, Name: name, Value: value, Stage: "qualification",
				Probability: DefaultProbability["qualification"], ExpectedCloseDate: opt.ExpectedCloseDate,
				LeadID: &lead.ID, ContactName: strings.TrimSpace(lead.FirstName + " " + lead.LastName),
				Company: lead.Company, CreatedAt: now, UpdatedAt: now,
			}
			if res.Customer != nil {
				opp.CustomerID = &res.Customer.ID
			}
			if err := tx.Omit("Lead").Create(&opp).Error; err != nil {
				return err
			}
			res.Opportunity = &opp
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Lead, _ = s.GetLead(orgID, id)
	return res, nil
}

// ---- Legacy CRM customers ----

func (s *Service) GetCustomers(orgID string) ([]CRMCustomer, error) {
	customers := []CRMCustomer{}
	err := s.db.Where("organization_id = ? AND is_active = ?", orgID, true).Order("first_name, last_name").Find(&customers).Error
	return customers, err
}

func (s *Service) CreateCustomer(customer *CRMCustomer) error {
	customer.ID = uuid.New().String()
	customer.CreatedAt = time.Now()
	customer.UpdatedAt = time.Now()
	return s.db.Create(customer).Error
}

// ---- Opportunities ----

func (s *Service) GetOpportunities(orgID string, stage ...string) ([]Opportunity, error) {
	q := s.db.Where("organization_id = ?", orgID)
	if len(stage) > 0 && stage[0] != "" {
		switch stage[0] {
		case "open":
			q = q.Where("stage NOT IN ?", []string{"closed_won", "closed_lost"})
		default:
			q = q.Where("stage = ?", stage[0])
		}
	}
	opportunities := []Opportunity{}
	err := q.Preload("Lead").Order("position asc, created_at desc").Find(&opportunities).Error
	return opportunities, err
}

func (s *Service) GetOpportunity(orgID, id string) (*Opportunity, error) {
	var o Opportunity
	err := s.db.Preload("Lead").First(&o, "id = ? AND organization_id = ?", id, orgID).Error
	return &o, notFound(err)
}

func (s *Service) CreateOpportunity(opportunity *Opportunity) error {
	opportunity.ID = uuid.New().String()
	if opportunity.Stage == "" {
		opportunity.Stage = "prospecting"
	}
	opportunity.CreatedAt = time.Now()
	opportunity.UpdatedAt = time.Now()
	if opportunity.Stage == "closed_won" || opportunity.Stage == "closed_lost" {
		now := time.Now()
		opportunity.ActualCloseDate = &now
	}
	return s.db.Omit("Lead").Create(opportunity).Error
}

func (s *Service) UpdateOpportunity(orgID, id string, updates map[string]interface{}) (*Opportunity, error) {
	cur, err := s.GetOpportunity(orgID, id)
	if err != nil {
		return nil, err
	}
	if st, ok := updates["stage"].(string); ok && st != cur.Stage {
		if st == "closed_won" || st == "closed_lost" {
			updates["actual_close_date"] = time.Now()
		} else {
			updates["actual_close_date"] = nil
		}
		if _, has := updates["probability"]; !has {
			updates["probability"] = DefaultProbability[st]
		}
	}
	updates["updated_at"] = time.Now()
	if err := s.db.Model(&Opportunity{}).Where("id = ? AND organization_id = ?", id, orgID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetOpportunity(orgID, id)
}

func (s *Service) DeleteOpportunity(orgID, id string) error {
	res := s.db.Where("id = ? AND organization_id = ?", id, orgID).Delete(&Opportunity{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Dashboard ----

func (s *Service) GetDashboard(orgID string) (*Dashboard, error) {
	return s.GetDashboardIn(orgID, time.Local)
}

// GetDashboardIn computes the dashboard with "this month" in the given zone
// (the user's calendar month).
func (s *Service) GetDashboardIn(orgID string, loc *time.Location) (*Dashboard, error) {
	d := Dashboard{LeadsByStatus: map[string]int64{}, Stages: []StageSummary{}, HotLeads: []Lead{}}
	now := time.Now().In(loc)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	s.db.Model(&Lead{}).Where("organization_id = ?", orgID).Count(&d.LeadsCount)
	s.db.Model(&Lead{}).Where("organization_id = ? AND created_at >= ?", orgID, monthStart).Count(&d.NewLeadsThisMonth)
	type sc struct {
		Status string
		N      int64
	}
	var byStatus []sc
	s.db.Model(&Lead{}).Select("COALESCE(NULLIF(status, ''), 'new') AS status, COUNT(*) AS n").Where("organization_id = ?", orgID).Group("COALESCE(NULLIF(status, ''), 'new')").Scan(&byStatus)
	var converted int64
	for _, r := range byStatus {
		d.LeadsByStatus[r.Status] = r.N
		if r.Status == "converted" {
			converted = r.N
		}
		if r.Status != "converted" && r.Status != "unqualified" {
			d.OpenLeads += r.N
		}
	}
	if d.LeadsCount > 0 {
		d.ConversionRate = float64(converted) / float64(d.LeadsCount) * 100
	}
	s.db.Model(&models.Customer{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&d.CustomersCount)

	var stages []StageSummary
	s.db.Model(&Opportunity{}).
		Select("stage, COUNT(*) AS count, COALESCE(SUM(value), 0) AS value, COALESCE(SUM(value * probability / 100.0), 0) AS weighted").
		Where("organization_id = ?", orgID).Group("stage").Scan(&stages)
	byStage := map[string]StageSummary{}
	for _, st := range stages {
		byStage[st.Stage] = st
	}
	for _, name := range Stages {
		st := byStage[name]
		st.Stage = name
		d.Stages = append(d.Stages, st)
		d.OpportunitiesCount += st.Count
		switch name {
		case "closed_won":
			d.WonCount, d.WonValue = st.Count, st.Value
		case "closed_lost":
			d.LostCount = st.Count
		default:
			d.OpenOpportunities += st.Count
			d.TotalPipelineValue += st.Value
			d.WeightedPipeline += st.Weighted
		}
	}
	if d.WonCount+d.LostCount > 0 {
		d.WinRate = float64(d.WonCount) / float64(d.WonCount+d.LostCount) * 100
	}
	if d.WonCount > 0 {
		d.AverageDealSize = d.WonValue / float64(d.WonCount)
	}
	s.db.Model(&Opportunity{}).Where("organization_id = ? AND stage = ? AND actual_close_date >= ?", orgID, "closed_won", monthStart).
		Select("COALESCE(SUM(value), 0)").Row().Scan(&d.WonThisMonth)

	s.db.Where("organization_id = ? AND rating = ? AND status NOT IN ?", orgID, "hot", []string{"converted", "unqualified"}).
		Order("created_at desc").Limit(5).Find(&d.HotLeads)
	return &d, nil
}
