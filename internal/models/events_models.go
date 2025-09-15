package models

import (
	"time"
	"gorm.io/gorm"
)

// Event represents an event that can be created by organizers
type Event struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID  string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Title           string         `json:"title" gorm:"type:varchar(255);not null"`
	Description     string         `json:"description" gorm:"type:text"`
	ShortDescription string        `json:"short_description" gorm:"type:varchar(500)"`
	Category        string         `json:"category" gorm:"type:varchar(100);not null;index"`
	Type            string         `json:"type" gorm:"type:varchar(50);not null;default:'in_person'"` // in_person, online, hybrid
	Status          string         `json:"status" gorm:"type:varchar(50);not null;default:'draft'"` // draft, published, cancelled, postponed, completed

	// Event timing
	StartDate       time.Time      `json:"start_date" gorm:"not null"`
	EndDate         time.Time      `json:"end_date" gorm:"not null"`
	Timezone        string         `json:"timezone" gorm:"type:varchar(100);default:'UTC'"`
	Duration        int            `json:"duration" gorm:"not null"` // in minutes

	// Location details
	VenueName       string         `json:"venue_name" gorm:"type:varchar(255)"`
	Address         Address        `json:"address" gorm:"embedded;embeddedPrefix:venue_"`
	OnlineURL       string         `json:"online_url" gorm:"type:varchar(500)"`
	OnlinePlatform  string         `json:"online_platform" gorm:"type:varchar(100)"` // zoom, teams, etc.

	// Pricing and capacity
	IsFree          bool           `json:"is_free" gorm:"default:false"`
	BasePrice       float64        `json:"base_price" gorm:"type:decimal(10,2);default:0"`
	Currency        string         `json:"currency" gorm:"type:varchar(3);default:'AUD'"`
	MaxCapacity     int            `json:"max_capacity" gorm:"default:0"` // 0 means unlimited
	AvailableTickets int           `json:"available_tickets" gorm:"default:0"`

	// Media and branding
	CoverImageURL   string         `json:"cover_image_url" gorm:"type:varchar(500)"`
	GalleryImages   []EventImage   `json:"gallery_images,omitempty" gorm:"foreignKey:EventID"`
	VideoURL        string         `json:"video_url" gorm:"type:varchar(500)"`

	// Additional settings
	IsPublic        bool           `json:"is_public" gorm:"default:true"`
	RequiresApproval bool          `json:"requires_approval" gorm:"default:false"`
	AllowWaitlist   bool           `json:"allow_waitlist" gorm:"default:false"`
	Tags            string         `json:"tags" gorm:"type:text"` // comma-separated tags

	// Metadata
	CreatedBy       string         `json:"created_by" gorm:"type:varchar(255);not null"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization    Organization   `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Creator         User           `json:"creator,omitempty" gorm:"foreignKey:CreatedBy"`
	TicketTypes     []TicketType   `json:"ticket_types,omitempty" gorm:"foreignKey:EventID"`
	Registrations   []EventRegistration `json:"registrations,omitempty" gorm:"foreignKey:EventID"`
	Sessions        []EventSession `json:"sessions,omitempty" gorm:"foreignKey:EventID"`
	Reviews         []EventReview  `json:"reviews,omitempty" gorm:"foreignKey:EventID"`
}

// TicketType represents different ticket options for an event
type TicketType struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	EventID         string         `json:"event_id" gorm:"type:varchar(255);not null;index"`
	Name            string         `json:"name" gorm:"type:varchar(255);not null"`
	Description     string         `json:"description" gorm:"type:text"`
	Price           float64        `json:"price" gorm:"type:decimal(10,2);not null"`
	Quantity        int            `json:"quantity" gorm:"not null"` // total available
	SoldQuantity    int            `json:"sold_quantity" gorm:"default:0"`
	MinPerOrder     int            `json:"min_per_order" gorm:"default:1"`
	MaxPerOrder     int            `json:"max_per_order" gorm:"default:10"`
	SaleStartDate   time.Time      `json:"sale_start_date"`
	SaleEndDate     time.Time      `json:"sale_end_date"`
	IsActive        bool           `json:"is_active" gorm:"default:true"`
	SortOrder       int            `json:"sort_order" gorm:"default:0"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Event           Event          `json:"event,omitempty" gorm:"foreignKey:EventID"`
	Registrations   []EventRegistration `json:"registrations,omitempty" gorm:"foreignKey:TicketTypeID"`
}

// EventRegistration represents a user's registration for an event
type EventRegistration struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	EventID         string         `json:"event_id" gorm:"type:varchar(255);not null;index"`
	TicketTypeID    string         `json:"ticket_type_id" gorm:"type:varchar(255);not null;index"`
	UserID          string         `json:"user_id" gorm:"type:varchar(255);index"`
	CustomerID      string         `json:"customer_id" gorm:"type:varchar(255);index"`

	// Registration details
	RegistrantName  string         `json:"registrant_name" gorm:"type:varchar(255);not null"`
	RegistrantEmail string         `json:"registrant_email" gorm:"type:varchar(255);not null"`
	RegistrantPhone string         `json:"registrant_phone" gorm:"type:varchar(20)"`
	Quantity        int            `json:"quantity" gorm:"not null;default:1"`
	TotalAmount     float64        `json:"total_amount" gorm:"type:decimal(10,2);not null"`

	// Status and payment
	Status          string         `json:"status" gorm:"type:varchar(50);not null;default:'pending'"` // pending, confirmed, cancelled, attended, no_show
	PaymentStatus   string         `json:"payment_status" gorm:"type:varchar(50);not null;default:'pending'"` // pending, paid, failed, refunded
	PaymentMethod   string         `json:"payment_method" gorm:"type:varchar(50)"`
	TransactionID   string         `json:"transaction_id" gorm:"type:varchar(255)"`

	// Check-in details
	CheckedInAt     *time.Time     `json:"checked_in_at"`
	CheckedInBy     string         `json:"checked_in_by" gorm:"type:varchar(255)"`
	QRCode          string         `json:"qr_code" gorm:"type:varchar(255);unique"`

	// Special requirements
	SpecialRequests string         `json:"special_requests" gorm:"type:text"`
	DietaryRequirements string     `json:"dietary_requirements" gorm:"type:text"`

	// Metadata
	RegistrationDate time.Time     `json:"registration_date"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Event           Event          `json:"event,omitempty" gorm:"foreignKey:EventID"`
	TicketType      TicketType     `json:"ticket_type,omitempty" gorm:"foreignKey:TicketTypeID"`
	User            User           `json:"user,omitempty" gorm:"foreignKey:UserID"`
	Customer        Customer       `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
}

// EventSession represents sessions within a multi-session event
type EventSession struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	EventID         string         `json:"event_id" gorm:"type:varchar(255);not null;index"`
	Title           string         `json:"title" gorm:"type:varchar(255);not null"`
	Description     string         `json:"description" gorm:"type:text"`
	SpeakerName     string         `json:"speaker_name" gorm:"type:varchar(255)"`
	SpeakerBio      string         `json:"speaker_bio" gorm:"type:text"`
	SpeakerImage    string         `json:"speaker_image" gorm:"type:varchar(500)"`
	StartTime       time.Time      `json:"start_time" gorm:"not null"`
	EndTime         time.Time      `json:"end_time" gorm:"not null"`
	RoomName        string         `json:"room_name" gorm:"type:varchar(255)"`
	MaxCapacity     int            `json:"max_capacity" gorm:"default:0"`
	SortOrder       int            `json:"sort_order" gorm:"default:0"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Event           Event          `json:"event,omitempty" gorm:"foreignKey:EventID"`
}

// EventImage represents gallery images for an event
type EventImage struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	EventID         string         `json:"event_id" gorm:"type:varchar(255);not null;index"`
	ImageURL        string         `json:"image_url" gorm:"type:varchar(500);not null"`
	Caption         string         `json:"caption" gorm:"type:varchar(255)"`
	SortOrder       int            `json:"sort_order" gorm:"default:0"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Event           Event          `json:"event,omitempty" gorm:"foreignKey:EventID"`
}

// EventReview represents reviews and ratings for events
type EventReview struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	EventID         string         `json:"event_id" gorm:"type:varchar(255);not null;index"`
	UserID          string         `json:"user_id" gorm:"type:varchar(255);index"`
	RegistrationID  string         `json:"registration_id" gorm:"type:varchar(255);index"`
	Rating          int            `json:"rating" gorm:"not null;check:rating >= 1 AND rating <= 5"`
	Review          string         `json:"review" gorm:"type:text"`
	IsPublic        bool           `json:"is_public" gorm:"default:true"`
	IsVerified      bool           `json:"is_verified" gorm:"default:false"` // verified attendee
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Event           Event          `json:"event,omitempty" gorm:"foreignKey:EventID"`
	User            User           `json:"user,omitempty" gorm:"foreignKey:UserID"`
	Registration    EventRegistration `json:"registration,omitempty" gorm:"foreignKey:RegistrationID"`
}

// EventCategory represents predefined categories for events
type EventCategory struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	Name            string         `json:"name" gorm:"type:varchar(100);not null;unique"`
	Description     string         `json:"description" gorm:"type:text"`
	IconName        string         `json:"icon_name" gorm:"type:varchar(100)"`
	Color           string         `json:"color" gorm:"type:varchar(7)"` // hex color
	IsActive        bool           `json:"is_active" gorm:"default:true"`
	SortOrder       int            `json:"sort_order" gorm:"default:0"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

// Methods for Event model
func (e *Event) BeforeCreate(tx *gorm.DB) error {
	if e.ID == "" {
		e.ID = "evt_" + generateRandomID(16)
	}
	return nil
}

func (e *Event) UpdateAvailableTickets() {
	totalSold := 0
	for _, ticketType := range e.TicketTypes {
		totalSold += ticketType.SoldQuantity
	}
	if e.MaxCapacity > 0 {
		e.AvailableTickets = e.MaxCapacity - totalSold
	}
}

func (e *Event) IsUpcoming() bool {
	return e.StartDate.After(time.Now())
}

func (e *Event) IsOngoing() bool {
	now := time.Now()
	return now.After(e.StartDate) && now.Before(e.EndDate)
}

func (e *Event) IsFinished() bool {
	return e.EndDate.Before(time.Now())
}

// Methods for TicketType model
func (tt *TicketType) BeforeCreate(tx *gorm.DB) error {
	if tt.ID == "" {
		tt.ID = "tkt_" + generateRandomID(16)
	}
	return nil
}

func (tt *TicketType) AvailableQuantity() int {
	return tt.Quantity - tt.SoldQuantity
}

func (tt *TicketType) IsSaleActive() bool {
	now := time.Now()
	return now.After(tt.SaleStartDate) && now.Before(tt.SaleEndDate) && tt.IsActive
}

// Methods for EventRegistration model
func (er *EventRegistration) BeforeCreate(tx *gorm.DB) error {
	if er.ID == "" {
		er.ID = "reg_" + generateRandomID(16)
	}
	if er.QRCode == "" {
		er.QRCode = "qr_" + generateRandomID(20)
	}
	return nil
}

func (er *EventRegistration) CanCheckIn() bool {
	return er.Status == "confirmed" && er.PaymentStatus == "paid"
}

// Methods for other models
func (es *EventSession) BeforeCreate(tx *gorm.DB) error {
	if es.ID == "" {
		es.ID = "ses_" + generateRandomID(16)
	}
	return nil
}

func (ei *EventImage) BeforeCreate(tx *gorm.DB) error {
	if ei.ID == "" {
		ei.ID = "img_" + generateRandomID(16)
	}
	return nil
}

func (er *EventReview) BeforeCreate(tx *gorm.DB) error {
	if er.ID == "" {
		er.ID = "rev_" + generateRandomID(16)
	}
	return nil
}

func (ec *EventCategory) BeforeCreate(tx *gorm.DB) error {
	if ec.ID == "" {
		ec.ID = "cat_" + generateRandomID(16)
	}
	return nil
}

// Helper function to generate random IDs
func generateRandomID(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[len(charset)/2+i%26] // Simple deterministic generation for now
	}
	return string(b)
}