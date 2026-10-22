package subscriptions

import (
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orghooks"
)

// ResetTables are the organisation tables ResetOrg clears (Platform →
// Organizations → Reset for handover).
var ResetTables = []string{"client_subscription_periods", "client_subscription_events", "client_subscription_tasks", "client_subscription_lines",
	"client_subscriptions", "client_subscription_plan_map", "subscription_settings"}

// ResetOrg removes an organisation's client subscriptions (they bill
// customers and point at invoices the reset removes) and restarts their
// numbering; the billing and dunning settings stay. The "Synergy plan →
// catalogue service" choices stay with the catalogue.
func ResetOrg(tx *gorm.DB, org string, opts orghooks.ResetOptions) error {
	for _, t := range []string{"client_subscription_periods", "client_subscription_events", "client_subscription_tasks", "client_subscription_lines", "client_subscriptions"} {
		if !tx.Migrator().HasTable(t) {
			continue
		}
		if err := tx.Exec("DELETE FROM "+t+" WHERE organization_id = ?", org).Error; err != nil {
			return err
		}
	}
	if !opts.KeepCatalogue && tx.Migrator().HasTable("client_subscription_plan_map") {
		if err := tx.Exec("DELETE FROM client_subscription_plan_map WHERE organization_id = ?", org).Error; err != nil {
			return err
		}
	}
	if tx.Migrator().HasTable("subscription_settings") {
		return tx.Exec(`UPDATE subscription_settings SET next_number = 1, test_offset_days = 0, last_digest = '', last_run_at = NULL WHERE organization_id = ?`, org).Error
	}
	return nil
}
