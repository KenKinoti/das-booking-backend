package zoho

import (
	"time"

	"gorm.io/gorm"
)

// Connection is an organisation's link to Zoho Books (one per organisation).
// The client secret and refresh token are stored encrypted (pkg/mail
// EncryptSecret: AES-GCM keyed by ENCRYPTION_KEY, else JWT_SECRET).
type Connection struct {
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	DC             string     `json:"dc" gorm:"type:varchar(10)"`
	ClientID       string     `json:"client_id" gorm:"type:varchar(255)"`
	ClientSecret   string     `json:"-" gorm:"type:text"`
	RefreshToken   string     `json:"-" gorm:"type:text"`
	AccountsServer string     `json:"accounts_server" gorm:"type:varchar(255)"`
	APIDomain      string     `json:"api_domain" gorm:"type:varchar(255)"`
	ZohoOrgID      string     `json:"zoho_org_id" gorm:"type:varchar(64)"`
	ZohoOrgName    string     `json:"zoho_org_name" gorm:"type:varchar(255)"`
	ZohoCurrency   string     `json:"zoho_currency" gorm:"type:varchar(3)"`
	Scopes         string     `json:"scopes" gorm:"type:text"`
	ConnectedAt    *time.Time `json:"connected_at"`
	ConnectedBy    string     `json:"connected_by" gorm:"type:varchar(255)"`
	LastError      string     `json:"last_error" gorm:"type:text"`
	LastSyncedAt   *time.Time `json:"last_synced_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (Connection) TableName() string { return "zoho_connections" }

// OAuthState binds an authorisation request to the organisation and user
// that started it (CSRF protection); single use, expires after 15 minutes.
type OAuthState struct {
	State          string    `gorm:"type:varchar(80);primaryKey"`
	OrganizationID string    `gorm:"type:varchar(255);index"`
	UserID         string    `gorm:"type:varchar(255)"`
	ReturnTo       string    `gorm:"type:text"`
	DC             string    `gorm:"type:varchar(10)"`
	RedirectURI    string    `gorm:"type:text"`
	ExpiresAt      time.Time `gorm:"index"`
	CreatedAt      time.Time
}

func (OAuthState) TableName() string { return "zoho_oauth_states" }

// Job statuses.
const (
	JobDraft      = "draft"      // CSV files uploaded, mapping not confirmed
	JobPreviewing = "previewing" // building the preview
	JobPreview    = "preview"    // preview ready, waiting for "Import"
	JobQueued     = "queued"
	JobRunning    = "running"
	JobPaused     = "paused" // rate limit / restart; resumes by itself or on request
	JobCompleted  = "completed"
	JobFailed     = "failed"
	JobCancelled  = "cancelled"
	JobUndone     = "undone"
)

// Sources.
const (
	SourceAPI = "zoho_api"
	SourceCSV = "csv" // Zoho export files or generic CSV/XLSX
)

// Job is one import (or re-sync) run with its preview, progress and report.
type Job struct {
	ID             string            `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string            `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	Source         string            `json:"source" gorm:"type:varchar(20)"`
	Status         string            `json:"status" gorm:"type:varchar(20);index"`
	Options        Options           `json:"options" gorm:"type:text;serializer:json"`
	Preview        *Preview          `json:"preview,omitempty" gorm:"type:text;serializer:json"`
	Step           string            `json:"step" gorm:"type:varchar(40)"`
	StepIndex      int               `json:"step_index"`
	Cursor         int               `json:"cursor"` // page within the current step (API)
	Message        string            `json:"message" gorm:"type:text"`
	Counts         map[string]*Count `json:"counts" gorm:"type:text;serializer:json"`
	Summary        *Summary          `json:"summary,omitempty" gorm:"type:text;serializer:json"`
	ErrorCount     int               `json:"error_count"`
	Error          string            `json:"error" gorm:"type:text"`
	APICalls       int               `json:"api_calls"`
	ResumeAt       *time.Time        `json:"resume_at"`
	Heartbeat      *time.Time        `json:"heartbeat"`
	StartedAt      *time.Time        `json:"started_at"`
	FinishedAt     *time.Time        `json:"finished_at"`
	CreatedBy      string            `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	DeletedAt      gorm.DeletedAt    `json:"-" gorm:"index"`
}

func (Job) TableName() string { return "import_jobs" }

// Count is the progress of one entity.
type Count struct {
	Total     int `json:"total"`
	Done      int `json:"done"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
}

// JobError is one record that could not be imported (downloadable as CSV).
type JobError struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	JobID          string    `json:"job_id" gorm:"type:varchar(64);index"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);index"`
	Entity         string    `json:"entity" gorm:"type:varchar(30)"`
	ExternalID     string    `json:"external_id" gorm:"type:varchar(100)"`
	Label          string    `json:"label" gorm:"type:varchar(255)"`
	Severity       string    `json:"severity" gorm:"type:varchar(10)"` // error | warning
	Message        string    `json:"message" gorm:"type:text"`
	Row            int       `json:"row"`
	CreatedAt      time.Time `json:"created_at"`
}

func (JobError) TableName() string { return "import_errors" }

// ExternalRef links a record in the source system to the local record it
// became, so re-syncs update instead of duplicating. Hash is a fingerprint of
// the source data last written; SyncedAt lets local edits made afterwards win.
type ExternalRef struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);uniqueIndex:ux_import_ref,priority:1;not null"`
	Source         string    `json:"source" gorm:"type:varchar(20);uniqueIndex:ux_import_ref,priority:2;not null"`
	Entity         string    `json:"entity" gorm:"type:varchar(30);uniqueIndex:ux_import_ref,priority:3;not null"`
	ExternalID     string    `json:"external_id" gorm:"type:varchar(150);uniqueIndex:ux_import_ref,priority:4;not null"`
	LocalID        string    `json:"local_id" gorm:"type:varchar(255);index"`
	Label          string    `json:"label" gorm:"type:varchar(255)"`
	Hash           string    `json:"hash" gorm:"type:varchar(64)"`
	Modified       string    `json:"modified" gorm:"type:varchar(64)"`     // source last_modified_time
	JobID          string    `json:"job_id" gorm:"type:varchar(64);index"` // job that created the local record
	LastJobID      string    `json:"last_job_id" gorm:"type:varchar(64)"`
	Created        bool      `json:"created"` // false when linked to a record that already existed (merge)
	LastSynced     time.Time `json:"last_synced"`
	CreatedAt      time.Time `json:"created_at"`
}

func (ExternalRef) TableName() string { return "import_external_refs" }

// Upload is one file of a CSV/XLSX import job.
type Upload struct {
	ID        string            `json:"id" gorm:"type:varchar(64);primaryKey"`
	JobID     string            `json:"job_id" gorm:"type:varchar(64);index"`
	OrgID     string            `json:"-" gorm:"type:varchar(255);index"`
	Name      string            `json:"name" gorm:"type:varchar(255)"`
	Kind      string            `json:"kind" gorm:"type:varchar(30)"` // detected or chosen file type
	Detected  string            `json:"detected" gorm:"type:varchar(30)"`
	Headers   []string          `json:"headers" gorm:"type:text;serializer:json"`
	Mapping   map[string]string `json:"mapping" gorm:"type:text;serializer:json"` // field → header
	Rows      int               `json:"rows"`
	Data      []byte            `json:"-" gorm:"type:bytea"`
	CreatedAt time.Time         `json:"created_at"`
}

func (Upload) TableName() string { return "import_uploads" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Connection{}, &OAuthState{}, &Job{}, &JobError{}, &ExternalRef{}, &Upload{})
}
