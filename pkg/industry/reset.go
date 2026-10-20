package industry

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SetupResetBy is recorded as updated_by when ResetSetup reopens the wizard.
const SetupResetBy = "platform.reset"

// ResetSetup puts an organisation's business-type setup back to "pending"
// with no step inside tx, so its admin is taken through the setup wizard
// again on the next sign-in (/industry/me → needs_setup). It is used by the
// platform's "reset organisation for handover".
//
// The business type itself, whether it was confirmed, the extra modules and
// the organisation's roles are kept. An organisation that has no settings
// row yet gets one for its business_type (unconfirmed, like a legacy
// organisation, but with the wizard pending).
//
// Services cache organisation state (Service.Invalidate): callers should
// invalidate the organisation once tx has committed.
func ResetSetup(tx *gorm.DB, org string) error {
	org = strings.TrimSpace(org)
	if org == "" {
		return errors.New("industry: organisation required")
	}
	now := time.Now()
	res := tx.Model(&OrgSettings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{
		"setup_status": SetupPending, "setup_step": "", "updated_by": SetupResetBy, "updated_at": now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	var bt string
	if err := tx.Table("organizations").Select("COALESCE(business_type, '')").Where("id = ?", org).Scan(&bt).Error; err != nil {
		return err
	}
	key := TypeKeyFor(bt)
	var n int64
	tx.Model(&BusinessType{}).Where("key = ?", key).Count(&n)
	if n == 0 {
		key = "general"
	}
	st := OrgSettings{OrganizationID: org, TypeKey: key, SetupStatus: SetupPending, ExtraModules: []string{},
		UpdatedBy: SetupResetBy, CreatedAt: now, UpdatedAt: now}
	return tx.Create(&st).Error
}
