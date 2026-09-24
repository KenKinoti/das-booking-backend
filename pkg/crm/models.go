package crm

import (
	"time"
)

// Lead statuses and opportunity stages.
var (
	LeadStatuses = []string{"new", "contacted", "qualified", "unqualified", "converted"}
	Stages       = []string{"prospecting", "qualification", "proposal", "negotiation", "closed_won", "closed_lost"}
	// DefaultProbability is used when an opportunity moves to a stage and no
	// explicit probability is given.
	DefaultProbability = map[string]int{
		"prospecting": 10, "qualification": 25, "proposal": 50, "negotiation": 75, "closed_won": 100, "closed_lost": 0,
	}
)

// Lead represents a potential customer
type Lead struct {
	ID             string     `json:"id" gorm:"primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"not null;index"`
	FirstName      string     `json:"first_name" gorm:"not null"`
	LastName       string     `json:"last_name" gorm:"not null"`
	Email          string     `json:"email"`
	Phone          string     `json:"phone"`
	Company        string     `json:"company"`
	JobTitle       string     `json:"job_title"`
	Source         string     `json:"source"`              // website, referral, social_media, ...
	Status         string     `json:"status" gorm:"index"` // new, contacted, qualified, unqualified, converted
	Rating         string     `json:"rating"`              // hot, warm, cold
	EstimatedValue float64    `json:"estimated_value" gorm:"default:0"`
	Notes          string     `json:"notes"`
	ConvertedDate  *time.Time `json:"converted_date"`
	CustomerID     *string    `json:"customer_id"` // core customers table, set on conversion
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// CRMCustomer represents a customer in the CRM context (legacy; the pipeline
// links to the core customers table instead).
type CRMCustomer struct {
	ID               string     `json:"id" gorm:"primaryKey"`
	OrganizationID   string     `json:"organization_id" gorm:"not null"`
	FirstName        string     `json:"first_name" gorm:"not null"`
	LastName         string     `json:"last_name" gorm:"not null"`
	Email            string     `json:"email"`
	Phone            string     `json:"phone"`
	Company          string     `json:"company"`
	CustomerType     string     `json:"customer_type"` // individual, business
	LifetimeValue    float64    `json:"lifetime_value" gorm:"default:0"`
	LastPurchaseDate *time.Time `json:"last_purchase_date"`
	Notes            string     `json:"notes"`
	IsActive         bool       `json:"is_active" gorm:"default:true"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// Opportunity represents a sales opportunity (a deal in the pipeline).
type Opportunity struct {
	ID                string     `json:"id" gorm:"primaryKey"`
	OrganizationID    string     `json:"organization_id" gorm:"not null;index"`
	Name              string     `json:"name" gorm:"not null"`
	Description       string     `json:"description"`
	Value             float64    `json:"value" gorm:"not null"`
	Stage             string     `json:"stage" gorm:"not null;index"`  // prospecting, qualification, proposal, negotiation, closed_won, closed_lost
	Probability       int        `json:"probability" gorm:"default:0"` // 0-100
	ExpectedCloseDate *time.Time `json:"expected_close_date"`
	ActualCloseDate   *time.Time `json:"actual_close_date"`
	LeadID            *string    `json:"lead_id"`
	CustomerID        *string    `json:"customer_id"`
	ContactName       string     `json:"contact_name"`
	Company           string     `json:"company"`
	LostReason        string     `json:"lost_reason"`
	Position          float64    `json:"position" gorm:"default:0"`
	Notes             string     `json:"notes"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Lead              *Lead      `json:"lead,omitempty" gorm:"foreignKey:LeadID"`
}

// StageSummary is the count and value of opportunities in one stage.
type StageSummary struct {
	Stage    string  `json:"stage"`
	Count    int64   `json:"count"`
	Value    float64 `json:"value"`
	Weighted float64 `json:"weighted"`
}

// Dashboard represents CRM dashboard metrics
type Dashboard struct {
	LeadsCount         int64            `json:"leads_count"`
	OpenLeads          int64            `json:"open_leads"`
	NewLeadsThisMonth  int64            `json:"new_leads_this_month"`
	CustomersCount     int64            `json:"customers_count"`
	OpportunitiesCount int64            `json:"opportunities_count"`
	OpenOpportunities  int64            `json:"open_opportunities"`
	TotalPipelineValue float64          `json:"total_pipeline_value"`
	WeightedPipeline   float64          `json:"weighted_pipeline_value"`
	WonValue           float64          `json:"won_value"`
	WonThisMonth       float64          `json:"won_this_month"`
	WonCount           int64            `json:"won_count"`
	LostCount          int64            `json:"lost_count"`
	WinRate            float64          `json:"win_rate"`
	ConversionRate     float64          `json:"conversion_rate"`
	AverageDealSize    float64          `json:"average_deal_size"`
	Stages             []StageSummary   `json:"stages"`
	LeadsByStatus      map[string]int64 `json:"leads_by_status"`
	HotLeads           []Lead           `json:"hot_leads"`
}
