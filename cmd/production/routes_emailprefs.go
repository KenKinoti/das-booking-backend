package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/barstock"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/bookingmail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

// Who gets emails (pkg/emailprefs): one resolver for every internal email —
// booking emails and the daily agenda (pkg/bookingmail), bill reminders
// (pkg/recurbills), the bar night report (pkg/barstock) and the strategy
// brief (pkg/insights). Admins choose a mode (everyone involved / owners and
// managers / only owners; nightclubs start with only owners) and switch
// people off or on; everyone can turn their own emails off in My profile.
//
//	GET  /api/v1/email-recipients                 mode, categories, people
//	PUT  /api/v1/email-recipients/policy          {mode}
//	PUT  /api/v1/email-recipients/users/:id       {all_off?, overrides?}
//	GET  /api/v1/email-recipients/preview?category=
//	GET  /api/v1/me/email-preferences             the signed-in user's switches
//	PUT  /api/v1/me/email-preferences             {all_off?, categories?}
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		ind := industryService(db)
		// Levels from the business-type role ("Bar manager" → manager).
		emailprefs.RoleOf = func(org, user, legacy string) (string, string, bool) {
			r, err := ind.RoleFor(org, user, legacy)
			if err != nil || r == nil {
				return "", "", false
			}
			return r.Base, r.Name, true
		}
		// Night report audience: may approve on inventory (as pkg/barstock).
		can := func(org, user, role, module, act string) bool {
			a, err := ind.AccessFor(org, user, role)
			if err != nil {
				return barLegacyCan(role, act)
			}
			return a.Can(module, act)
		}
		emailprefs.Can = can

		// Previews use each sender's own recipient code.
		bills := &recurbills.Service{DB: db, Mail: mail.Shared(db)}
		bar := barstock.New(db)
		bar.Can = can
		intel := insights.NewIntel(&insights.Service{DB: db}, nil)
		bookingSettings := func(org string) bookingmail.Settings {
			if bookingmail.Default != nil {
				return bookingmail.Default.SettingsFor(org)
			}
			return bookingmail.DefaultSettings(org)
		}
		h := &emailprefs.Handler{DB: db, Previews: map[string]emailprefs.PreviewFunc{
			emailprefs.Bookings: func(org string) emailprefs.Preview {
				p := emailprefs.Preview{Recipients: emailprefs.MembersFor(db, org, emailprefs.Bookings), Enabled: bookingSettings(org).StaffEmails,
					Note: "Each booking email goes only to the person the booking is assigned to; these are the people who would get one."}
				if !p.Enabled {
					p.Note = "Staff booking emails are switched off for the whole organisation (Settings → Booking emails)."
				}
				return p
			},
			emailprefs.Agenda: func(org string) emailprefs.Preview {
				st := bookingSettings(org)
				p := emailprefs.Preview{Recipients: emailprefs.MembersFor(db, org, emailprefs.Agenda), Enabled: st.StaffEmails && st.DailyAgenda,
					Note: "Each person gets their own day's bookings, on days they have some."}
				if !p.Enabled {
					p.Note = "The daily agenda is switched off (Settings → Booking emails)."
				}
				return p
			},
			emailprefs.BillReminders: func(org string) emailprefs.Preview {
				st := bills.SettingsFor(org)
				p := emailprefs.Preview{Recipients: bills.Recipients(org, st, nil), Enabled: st.Enabled,
					Note: "A recurring bill's own extra recipients are added to its reminders."}
				if !st.Enabled {
					p.Note = "Bill reminders are switched off (Settings → Bill reminders)."
				}
				return p
			},
			emailprefs.BarNightReport: func(org string) emailprefs.Preview {
				p := emailprefs.Preview{Recipients: bar.Recipients(db, org), Enabled: bar.Settings(db, org).NightReportEnabled}
				if !p.Enabled {
					p.Note = "The night report is switched off (Bar stock → Setup). These people would get it, or a manual “Send now”."
				}
				return p
			},
			emailprefs.StrategyBrief: func(org string) emailprefs.Preview {
				p := emailprefs.Preview{Recipients: intel.BriefRecipients(org), Enabled: intel.Settings(org).BriefEmail}
				if !p.Enabled {
					p.Note = "Emailing the monthly brief is switched off (Insights → Strategy brief). These people get it when it is sent."
				}
				return p
			},
		}}
		h.RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := emailprefs.Migrate(db); err != nil {
			log.Printf("emailprefs migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "emailprefs", err))
		}
	})
}
