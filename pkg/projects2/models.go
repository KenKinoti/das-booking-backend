// Package projects2 is the Projects module: organisation-scoped projects and
// tasks, plus a read-only sync from Jira Cloud (Atlassian).
package projects2

import (
	"time"

	"gorm.io/gorm"
)

// Project statuses.
const (
	StatusPlanned   = "planned"
	StatusActive    = "active"
	StatusOnHold    = "on_hold"
	StatusCompleted = "completed"
)

// Task status categories (mirrors Jira's new / indeterminate / done).
const (
	CatTodo       = "todo"
	CatInProgress = "in_progress"
	CatDone       = "done"
)

// Record sources.
const (
	SourceLocal = "local"
	SourceJira  = "jira"
)

var validProjectStatus = map[string]bool{StatusPlanned: true, StatusActive: true, StatusOnHold: true, StatusCompleted: true}
var validCategory = map[string]bool{CatTodo: true, CatInProgress: true, CatDone: true}

// Project is a local or Jira-mirrored project.
type Project struct {
	ID             string     `gorm:"primaryKey;type:varchar(36)" json:"id"`
	OrgID          string     `gorm:"type:varchar(64);index;not null" json:"org_id"`
	Key            string     `gorm:"type:varchar(32)" json:"key"`
	Name           string     `gorm:"type:varchar(255);not null" json:"name"`
	Description    string     `gorm:"type:text" json:"description"`
	Status         string     `gorm:"type:varchar(20);default:active" json:"status"`
	StartDate      *time.Time `gorm:"type:date" json:"start_date"`
	DueDate        *time.Time `gorm:"type:date" json:"due_date"`
	Budget         float64    `gorm:"type:numeric(14,2);default:0" json:"budget"`
	OwnerName      string     `gorm:"type:varchar(255)" json:"owner_name"`
	Source         string     `gorm:"type:varchar(10);default:local;index" json:"source"`
	ExternalID     string     `gorm:"type:varchar(64);index" json:"external_id"`
	ExternalURL    string     `gorm:"type:varchar(512)" json:"external_url"`
	LastSyncedAt   *time.Time `json:"last_synced_at"`
	NextTaskNumber int        `gorm:"default:1" json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	Stats *ProjectStats `gorm:"-" json:"stats,omitempty"`
}

func (Project) TableName() string { return "pm_projects" }

// ProjectStats is the computed progress of a project.
type ProjectStats struct {
	Total      int `json:"total"`
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
	Overdue    int `json:"overdue"`
	Progress   int `json:"progress"` // % done, 0-100
}

// Task is a local task or a mirrored Jira issue.
type Task struct {
	ID              string     `gorm:"primaryKey;type:varchar(36)" json:"id"`
	OrgID           string     `gorm:"type:varchar(64);index;not null" json:"org_id"`
	ProjectID       string     `gorm:"type:varchar(36);index;not null" json:"project_id"`
	Key             string     `gorm:"type:varchar(64)" json:"key"`
	Title           string     `gorm:"type:varchar(512);not null" json:"title"`
	Description     string     `gorm:"type:text" json:"description"`
	Status          string     `gorm:"type:varchar(64)" json:"status"`
	StatusCategory  string     `gorm:"type:varchar(20);default:todo;index" json:"status_category"`
	Type            string     `gorm:"type:varchar(64)" json:"type"`
	Priority        string     `gorm:"type:varchar(32)" json:"priority"`
	AssigneeName    string     `gorm:"type:varchar(255)" json:"assignee_name"`
	AssigneeEmail   string     `gorm:"type:varchar(255)" json:"assignee_email"`
	DueDate         *time.Time `gorm:"type:date" json:"due_date"`
	StoryPoints     *float64   `json:"story_points"`
	Source          string     `gorm:"type:varchar(10);default:local" json:"source"`
	ExternalID      string     `gorm:"type:varchar(64);index" json:"external_id"`
	URL             string     `gorm:"type:varchar(512)" json:"url"`
	CompletedAt     *time.Time `json:"completed_at"`
	RemoteUpdatedAt *time.Time `json:"remote_updated_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (Task) TableName() string { return "pm_tasks" }

// JiraConnection holds an organisation's Atlassian credentials. The API token
// is stored encrypted and never returned by the API.
type JiraConnection struct {
	OrgID           string     `gorm:"primaryKey;type:varchar(64)"`
	SiteURL         string     `gorm:"type:varchar(255)"`
	Email           string     `gorm:"type:varchar(255)"`
	TokenEnc        string     `gorm:"type:text"`
	TokenLast4      string     `gorm:"type:varchar(8)"`
	DefaultJQL      string     `gorm:"type:text"`
	ProjectKeys     string     `gorm:"type:text"` // comma separated
	AccountName     string     `gorm:"type:varchar(255)"`
	LastSyncAt      *time.Time //
	LastSyncStatus  string     `gorm:"type:varchar(20)"` // success | partial | error
	LastSyncMessage string     `gorm:"type:text"`
	LastSyncStats   string     `gorm:"type:text"` // JSON SyncResult
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (JiraConnection) TableName() string { return "pm_jira_connections" }

// Models lists the tables for AutoMigrate.
func Models() []interface{} { return []interface{}{&Project{}, &Task{}, &JiraConnection{}} }

// Migrate creates / updates the module's tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }
