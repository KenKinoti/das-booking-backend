package crm

import (
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

// InitializeSampleData creates sample CRM data
func (s *Service) InitializeSampleData(orgID string) error {
	leads := []Lead{
		{
			ID: uuid.New().String(), OrganizationID: orgID,
			FirstName: "John", LastName: "Smith", Email: "john@example.com",
			Phone: "555-0101", Company: "ABC Corp", Status: "qualified", Rating: "hot",
		},
		{
			ID: uuid.New().String(), OrganizationID: orgID,
			FirstName: "Sarah", LastName: "Johnson", Email: "sarah@example.com",
			Phone: "555-0102", Company: "XYZ Inc", Status: "new", Rating: "warm",
		},
	}
	return s.db.Create(&leads).Error
}

// Lead methods
func (s *Service) GetLeads(orgID string) ([]Lead, error) {
	var leads []Lead
	err := s.db.Where("organization_id = ?", orgID).Order("created_at desc").Find(&leads).Error
	return leads, err
}

func (s *Service) CreateLead(lead *Lead) error {
	lead.ID = uuid.New().String()
	lead.CreatedAt = time.Now()
	lead.UpdatedAt = time.Now()
	return s.db.Create(lead).Error
}

func (s *Service) GetLead(id string) (*Lead, error) {
	var lead Lead
	err := s.db.First(&lead, "id = ?", id).Error
	return &lead, err
}

func (s *Service) UpdateLead(lead *Lead) error {
	lead.UpdatedAt = time.Now()
	return s.db.Save(lead).Error
}

func (s *Service) DeleteLead(id string) error {
	return s.db.Delete(&Lead{}, "id = ?", id).Error
}

// Customer methods
func (s *Service) GetCustomers(orgID string) ([]CRMCustomer, error) {
	var customers []CRMCustomer
	err := s.db.Where("organization_id = ? AND is_active = ?", orgID, true).Order("first_name, last_name").Find(&customers).Error
	return customers, err
}

func (s *Service) CreateCustomer(customer *CRMCustomer) error {
	customer.ID = uuid.New().String()
	customer.CreatedAt = time.Now()
	customer.UpdatedAt = time.Now()
	return s.db.Create(customer).Error
}

// Opportunity methods
func (s *Service) GetOpportunities(orgID string) ([]Opportunity, error) {
	var opportunities []Opportunity
	err := s.db.Where("organization_id = ?", orgID).Preload("Lead").Preload("Customer").Order("created_at desc").Find(&opportunities).Error
	return opportunities, err
}

func (s *Service) CreateOpportunity(opportunity *Opportunity) error {
	opportunity.ID = uuid.New().String()
	opportunity.CreatedAt = time.Now()
	opportunity.UpdatedAt = time.Now()
	return s.db.Create(opportunity).Error
}

func (s *Service) GetDashboard(orgID string) (*Dashboard, error) {
	var dashboard Dashboard

	// Count leads, customers, opportunities
	s.db.Model(&Lead{}).Where("organization_id = ?", orgID).Count(&dashboard.LeadsCount)
	s.db.Model(&CRMCustomer{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&dashboard.CustomersCount)
	s.db.Model(&Opportunity{}).Where("organization_id = ?", orgID).Count(&dashboard.OpportunitiesCount)

	// Calculate total pipeline value
	var pipelineValue float64
	s.db.Model(&Opportunity{}).Where("organization_id = ? AND stage NOT IN (?)", orgID, []string{"closed_won", "closed_lost"}).
		Select("COALESCE(SUM(value), 0)").Row().Scan(&pipelineValue)
	dashboard.TotalPipelineValue = pipelineValue

	return &dashboard, nil
}