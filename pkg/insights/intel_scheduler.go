package insights

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Background work for the industry watch and the strategy brief:
//
//   - once a day per organisation: fetch its enabled sources;
//   - once a month per organisation: generate the rule-based brief of the
//     month that just ended (and email it when that is switched on).
//
// Only organisations that have opened these views (a row in insights_settings)
// take part. Each job runs under a Postgres advisory lock, so with several
// instances (e.g. Cloud Run containers) exactly one does it.
// INSIGHTS_SCHEDULER=off disables the scheduler.

// watchEvery is the minimum time between two scheduled refreshes of an organisation.
const watchEvery = 20 * time.Hour

func (i *Intel) orgLoc(org string) *time.Location {
	if tz := orgprefs.Get(i.DB, org).Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.UTC
}

// RunScheduled does the due work once; it returns how many organisations were
// refreshed and how many briefs were generated.
func (i *Intel) RunScheduled(ctx context.Context) (refreshed, briefs int) {
	var rows []IntelSettings
	i.DB.Find(&rows)
	for _, st := range rows {
		if ctx.Err() != nil {
			return
		}
		org := st.OrganizationID
		now := i.now()
		// --- daily watch refresh ---
		if st.WatchLastRun == nil || now.Sub(*st.WatchLastRun) >= watchEvery {
			var n int64
			i.DB.Model(&WatchFeed{}).Where("organization_id = ? AND enabled", org).Count(&n)
			if n > 0 {
				ok := i.withLock(ctx, "insights-watch:"+org, func() {
					// Re-check under the lock: another instance may have just run it.
					var fresh IntelSettings
					if i.DB.Where("organization_id = ?", org).First(&fresh).Error != nil {
						return
					}
					if fresh.WatchLastRun != nil && i.now().Sub(*fresh.WatchLastRun) < watchEvery {
						return
					}
					sum := &RefreshSummary{Feeds: []FeedRun{}}
					i.refresh(ctx, org, "", sum)
					refreshed++
					if sum.New > 0 || sum.Errors > 0 {
						log.Printf("insights watch: %s — %d sources, %d new items, %d errors", org, sum.Checked, sum.New, sum.Errors)
					}
				})
				if !ok {
					log.Printf("insights watch: %s is being refreshed on another instance", org)
				}
			}
		}
		// --- monthly brief: the month that ended ---
		if st.BriefAuto {
			loc := i.orgLoc(org)
			today := day(now.In(loc))
			period := monthStart(today).AddDate(0, -1, 0).Format("2006-01")
			if i.ruleBrief(org, period) != nil {
				continue
			}
			var docs int64
			i.DB.Raw(`SELECT COUNT(*) FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND doc_type = 'invoice' AND status NOT IN ('draft','void')`, org).Scan(&docs)
			if docs == 0 {
				continue // nothing to brief on yet
			}
			i.withLock(ctx, "insights-brief:"+org, func() {
				if i.ruleBrief(org, period) != nil {
					return
				}
				b, err := i.GenerateBrief(ctx, org, "system", period, "scheduled", today, loc)
				if err != nil {
					log.Printf("insights brief: %s %s: %v", org, period, err)
					return
				}
				briefs++
				var fresh IntelSettings
				if i.DB.Where("organization_id = ?", org).First(&fresh).Error == nil && fresh.BriefEmail {
					if _, err := i.EmailBrief(ctx, org, b.ID); err != nil {
						log.Printf("insights brief: %s %s: email: %v", org, period, err)
					}
				}
			})
		}
	}
	return refreshed, briefs
}

// StartScheduler checks every 30 minutes for due work.
func (i *Intel) StartScheduler(ctx context.Context) {
	if strings.EqualFold(os.Getenv("INSIGHTS_SCHEDULER"), "off") {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("insights: scheduled run failed: %v", r)
					}
				}()
				i.RunScheduled(ctx)
			}()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
