package occasions

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OrgSetting: does the organisation show seasonal themes (occasion_org_settings)?
// No row = on.
type OrgSetting struct {
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	SeasonalThemes bool      `json:"seasonal_themes" gorm:"not null"`
	UpdatedBy      string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (OrgSetting) TableName() string { return "occasion_org_settings" }

// UserPref: a person's own choice (occasion_user_prefs). ShowThemes nil =
// follow the organisation.
type UserPref struct {
	UserID     string    `json:"user_id" gorm:"type:varchar(255);primaryKey"`
	ShowThemes *bool     `json:"show_themes"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TableName is fixed.
func (UserPref) TableName() string { return "occasion_user_prefs" }

// Models are the package's tables.
func Models() []interface{} {
	return []interface{}{&CustomOccasion{}, &Toggle{}, &OrgSetting{}, &UserPref{}}
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

// Why a theme is (not) showing.
const (
	ReasonOn        = "on"         // a theme is showing
	ReasonNone      = "none"       // themes are on but no occasion today
	ReasonPreview   = "preview"    // a super admin's preview
	ReasonOffGlobal = "off_global" // switched off platform-wide
	ReasonOffOrg    = "off_org"    // the organisation switched them off
	ReasonOffUser   = "off_user"   // the person switched them off
)

// Resolve decides whether seasonal themes show for a person: the platform
// switch first, then the person's own choice, then the organisation's
// setting (default on).
func Resolve(global, org bool, user *bool) (bool, string) {
	if !global {
		return false, ReasonOffGlobal
	}
	if user != nil {
		if *user {
			return true, ReasonOn
		}
		return false, ReasonOffUser
	}
	if !org {
		return false, ReasonOffOrg
	}
	return true, ReasonOn
}

// OrgEnabled: the organisation's setting (on when never set).
func OrgEnabled(db *gorm.DB, org string) bool {
	if db == nil || org == "" {
		return true
	}
	var row OrgSetting
	if err := db.Where("organization_id = ?", org).Limit(1).Find(&row).Error; err != nil || row.OrganizationID == "" {
		return true
	}
	return row.SeasonalThemes
}

// SetOrgEnabled saves the organisation's setting.
func SetOrgEnabled(db *gorm.DB, org, by string, on bool) error {
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}}, DoUpdates: clause.AssignmentColumns([]string{"seasonal_themes", "updated_by", "updated_at"})}).
		Create(&OrgSetting{OrganizationID: org, SeasonalThemes: on, UpdatedBy: by, UpdatedAt: time.Now()}).Error
}

// UserChoice: the person's own choice (nil = follow the organisation).
func UserChoice(db *gorm.DB, user string) *bool {
	if db == nil || user == "" {
		return nil
	}
	var row UserPref
	if err := db.Where("user_id = ?", user).Limit(1).Find(&row).Error; err != nil || row.UserID == "" {
		return nil
	}
	return row.ShowThemes
}

// SetUserChoice saves the person's choice (nil = follow the organisation).
func SetUserChoice(db *gorm.DB, user string, show *bool) error {
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"show_themes", "updated_at"})}).
		Create(&UserPref{UserID: user, ShowThemes: show, UpdatedAt: time.Now()}).Error
}

// SetToggle switches a built-in occasion's theme (key) or all themes
// (GlobalKey) platform-wide.
func (s *Service) SetToggle(key, by string, on bool) error {
	err := s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"enabled", "updated_by", "updated_at"})}).
		Create(&Toggle{Key: key, Enabled: on, UpdatedBy: by, UpdatedAt: time.Now()}).Error
	s.Invalidate()
	return err
}
