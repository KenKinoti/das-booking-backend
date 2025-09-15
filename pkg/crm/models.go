package crm

import (
	"time"
)

// Lead represents a potential customer
type Lead struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	FirstName      string    `json:"first_name" gorm:"not null"`
	LastName       string    `json:"last_name" gorm:"not null"`
	Email          string    `json:"email"`
	Phone          string    `json:"phone"`
	Company        string    `json:"company"`
	JobTitle       string    `json:"job_title"`
	Source         string    `json:"source"`         // website, referral, social_media
	Status         string    `json:"status"`         // new, contacted, qualified, converted
	Rating         string    `json:"rating"`         // hot, warm, cold
	Notes          string    `json:"notes"`
	ConvertedDate  *time.Time `json:"converted_date"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CRMCustomer represents a customer in the CRM context
type CRMCustomer struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	OrganizationID   string    `json:"organization_id" gorm:"not null"`
	FirstName        string    `json:"first_name" gorm:"not null"`
	LastName         string    `json:"last_name" gorm:"not null"`
	Email            string    `json:"email"`
	Phone            string    `json:"phone"`
	Company          string    `json:"company"`
	CustomerType     string    `json:"customer_type"` // individual, business
	LifetimeValue    float64   `json:"lifetime_value" gorm:"default:0"`
	LastPurchaseDate *time.Time `json:"last_purchase_date"`
	Notes            string    `json:"notes"`
	IsActive         bool      `json:"is_active" gorm:"default:true"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Opportunity represents a sales opportunity
type Opportunity struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Description    string    `json:"description"`
	Value          float64   `json:"value" gorm:"not null"`
	Stage          string    `json:"stage" gorm:"not null"` // prospecting, qualification, proposal, negotiation, closed_won, closed_lost
	Probability    int       `json:"probability" gorm:"default:0"` // 0-100
	ExpectedCloseDate *time.Time `json:"expected_close_date"`
	ActualCloseDate   *time.Time `json:"actual_close_date"`
	LeadID         *string   `json:"lead_id"`
	CustomerID     *string   `json:"customer_id"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Lead           *Lead     `json:"lead,omitempty" gorm:"foreignKey:LeadID"`
	Customer       *CRMCustomer `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
}

// Dashboard represents CRM dashboard metrics
type Dashboard struct {
	LeadsCount           int64   `json:"leads_count"`
	CustomersCount       int64   `json:"customers_count"`
	OpportunitiesCount   int64   `json:"opportunities_count"`
	TotalPipelineValue   float64 `json:"total_pipeline_value"`
	ConversionRate       float64 `json:"conversion_rate,omitempty"`
	HotLeads             []Lead  `json:"hot_leads,omitempty"`
}