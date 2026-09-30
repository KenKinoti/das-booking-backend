package platform

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyMerge tells the kept organisation's admins (and the super admin who
// ran it, if they belong to it) what was merged in.
func notifyMerge(db *gorm.DB, c *gin.Context, keepID string, rep *MergeReport) {
	names := make([]string, 0, len(rep.Merged))
	for _, m := range rep.Merged {
		names = append(names, m.Name)
	}
	n := len(rep.Merged)
	title := "1 organisation merged into yours"
	if n != 1 {
		title = fmt.Sprintf("%d organisations merged into yours", n)
	}
	body := fmt.Sprintf("%s — %d records and %d users moved.", strings.Join(names, ", "), rep.TotalMoved, rep.UsersMoved)
	notify.Go(db, keepID, notify.Admins(), notify.Message{
		Type: "org.merged", Severity: notify.Info, Title: title, Body: body, Link: "/settings",
		EntityType: "organization", EntityID: keepID, ActorID: c.GetString("user_id"),
	})
}
