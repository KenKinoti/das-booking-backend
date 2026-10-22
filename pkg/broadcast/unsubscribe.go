package broadcast

import (
	"strings"

	"gorm.io/gorm/clause"
)

// Unsubscribe stops offers for a person: no more offer emails, no in-app
// offers (unread ones already in the bell are taken away). deliveryID is the
// offer they unsubscribed from ("" from the app).
func (s *Service) Unsubscribe(userID, deliveryID, source string) error {
	var u struct{ Email, OrganizationID string }
	fresh(s.DB).Raw(`SELECT COALESCE(email,'') AS email, COALESCE(organization_id,'') AS organization_id FROM users WHERE id = ?`, userID).Scan(&u)
	ann := ""
	if deliveryID != "" {
		var d Delivery
		if fresh(s.DB).First(&d, "id = ? AND user_id = ?", deliveryID, userID).Error == nil {
			ann = d.AnnouncementID
			fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Update("unsubscribed", true)
		}
	}
	row := Unsubscribe{UserID: userID, Email: strings.ToLower(strings.TrimSpace(u.Email)), OrganizationID: u.OrganizationID, AnnouncementID: ann, Source: source, CreatedAt: Now()}
	if err := fresh(s.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	fresh(s.DB).Model(&Delivery{}).Where("user_id = ? AND kind = ? AND email_status IN ?", userID, KindPromotion, []string{EmailPending}).
		Updates(map[string]any{"email_status": EmailSkipped, "email_note": "Unsubscribed from DASYIN offers"})
	if hasTable(s.DB, "notifications") {
		fresh(s.DB).Exec(`DELETE FROM notifications WHERE user_id = ? AND type = ? AND read_at IS NULL`, userID, NotificationType(KindPromotion))
	}
	return nil
}

// Resubscribe turns offers back on for a person.
func (s *Service) Resubscribe(userID string) error {
	return fresh(s.DB).Where("user_id = ?", userID).Delete(&Unsubscribe{}).Error
}

// MaskEmail shows enough of an address to recognise it (ke****@club.test).
func MaskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at <= 0 {
		return ""
	}
	name := e[:at]
	keep := 2
	if len(name) <= 2 {
		keep = 1
	}
	return name[:keep] + strings.Repeat("•", 4) + e[at:]
}
