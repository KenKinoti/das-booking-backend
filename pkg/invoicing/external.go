package invoicing

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Helpers for documents brought in from other systems (pkg/zoho): original
// files kept with a document, totals that must stay exactly as they were,
// and numbering that continues after the imported numbers.

// Attachment is a file kept with a document, e.g. the original PDF of an
// invoice imported from Zoho Books ("Original from Zoho").
type Attachment struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string    `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	Name           string    `json:"name" gorm:"type:varchar(255)"`
	ContentType    string    `json:"content_type" gorm:"type:varchar(100)"`
	Size           int       `json:"size"`
	Source         string    `json:"source" gorm:"type:varchar(20)"` // zoho | csv | ""
	Kind           string    `json:"kind" gorm:"type:varchar(30)"`   // original
	Data           []byte    `json:"-" gorm:"type:bytea"`
	CreatedAt      time.Time `json:"created_at"`
}

func (Attachment) TableName() string { return "billing_attachments" }

// SetBaseTotal fills TotalBase from Total and the locked exchange rate.
func SetBaseTotal(inv *Invoice) { setBaseTotal(inv) }

// RoundCur rounds v to the minor units of currency cur.
func RoundCur(v float64, cur string) float64 { return roundCur(v, cur) }

// NewPublicToken returns a fresh token for a document's share link.
func NewPublicToken() string { return newToken() }

var numberRe = regexp.MustCompile(`^(.*?)(\d+)$`)

// SplitNumber splits "INV-000123" into ("INV-", 123, 6). ok is false when
// the number does not end in digits.
func SplitNumber(n string) (prefix string, num int, width int, ok bool) {
	m := numberRe.FindStringSubmatch(strings.TrimSpace(n))
	if m == nil || len(m[2]) > 9 {
		return "", 0, 0, false
	}
	v, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, 0, false
	}
	return m[1], v, len(m[2]), true
}

// Numbering is the result of continuing a number series.
type Numbering struct {
	DocType        string `json:"doc_type"`
	Prefix         string `json:"prefix"`
	Padding        int    `json:"padding"`
	Next           int    `json:"next"`
	NextNumber     string `json:"next_number"`
	Highest        string `json:"highest"`
	Changed        bool   `json:"changed"`
	PreviousPrefix string `json:"previous_prefix"`
	PreviousNext   int    `json:"previous_next"`
	PreviousPad    int    `json:"previous_padding"`
	Note           string `json:"note,omitempty"`
}

// ContinueNumbering sets the organisation's next invoice (or quote) number to
// follow the highest of numbers, using the prefix most of them share
// (INV-000123 → prefix "INV-", padding 6, next 124). Documents already
// numbered higher with that prefix push it further. Padding is shared by invoices and
// quotes; it is only changed for invoices.
func ContinueNumbering(tx *gorm.DB, org, docType string, numbers []string) (*Numbering, error) {
	h := &Handler{DB: tx}
	s, err := h.settingsFor(tx, org, true)
	if err != nil {
		return nil, err
	}
	byPrefix := map[string]int{}
	best := map[string]int{}
	width := map[string]int{}
	high := map[string]string{}
	for _, n := range numbers {
		p, v, w, ok := SplitNumber(n)
		if !ok {
			continue
		}
		byPrefix[p]++
		if v >= best[p] {
			best[p], high[p] = v, n
		}
		if w > width[p] {
			width[p] = w
		}
	}
	if len(byPrefix) == 0 {
		return nil, nil
	}
	prefix, cnt := "", -1
	for p, c := range byPrefix {
		if c > cnt || (c == cnt && p < prefix) {
			prefix, cnt = p, c
		}
	}
	out := &Numbering{DocType: docType, Prefix: prefix, Padding: width[prefix], Highest: high[prefix], PreviousPad: s.NumberPadding}
	cur := s.InvoicePrefix
	next := s.NextInvoiceNumber
	if docType == TypeQuote {
		cur, next = s.QuotePrefix, s.NextQuoteNumber
	}
	out.PreviousPrefix, out.PreviousNext = cur, next
	// Continue after the highest number in use with that prefix (imported
	// or created here), so the series never goes backwards or collides.
	want := best[prefix] + 1
	var used []string
	tx.Model(&Invoice{}).Where("organization_id = ? AND doc_type = ? AND number LIKE ?", org, docType, likePrefix(prefix)+"%").Pluck("number", &used)
	for _, n := range used {
		if p, v, w, ok := SplitNumber(n); ok && p == prefix && v+1 > want {
			want = v + 1
			width[prefix] = w // keep the width of the series in use
		}
	}
	out.Next = want
	out.Padding = width[prefix]
	pad := s.NumberPadding
	upd := map[string]interface{}{}
	if docType == TypeQuote {
		upd["quote_prefix"], upd["next_quote_number"] = prefix, want
		if out.Padding != pad && out.Padding > 0 {
			out.Note = fmt.Sprintf("Quote numbers use %d digits but invoices use %d; new quotes will use %d.", out.Padding, pad, pad)
		}
	} else {
		upd["invoice_prefix"], upd["next_invoice_number"] = prefix, want
		if out.Padding > 0 && out.Padding <= 10 {
			upd["number_padding"] = out.Padding
			pad = out.Padding
		}
	}
	out.Padding = pad
	out.Changed = cur != prefix || next != want || pad != s.NumberPadding
	out.NextNumber = fmt.Sprintf("%s%0*d", prefix, pad, want)
	if err := tx.Model(&Settings{}).Where("organization_id = ?", org).Updates(upd).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func likePrefix(p string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(p)
}

// RestoreNumbering puts back the settings recorded in n (undo of an import),
// but only while they still hold the values the import set.
func RestoreNumbering(tx *gorm.DB, org string, n *Numbering) bool {
	if n == nil || !n.Changed {
		return false
	}
	var s Settings
	if tx.Where("organization_id = ?", org).First(&s).Error != nil {
		return false
	}
	if n.DocType == TypeQuote {
		if s.QuotePrefix != n.Prefix || s.NextQuoteNumber != n.Next {
			return false
		}
		return tx.Model(&Settings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{"quote_prefix": n.PreviousPrefix, "next_quote_number": n.PreviousNext}).Error == nil
	}
	if s.InvoicePrefix != n.Prefix || s.NextInvoiceNumber != n.Next {
		return false
	}
	upd := map[string]interface{}{"invoice_prefix": n.PreviousPrefix, "next_invoice_number": n.PreviousNext}
	if n.PreviousPad > 0 {
		upd["number_padding"] = n.PreviousPad
	}
	return tx.Model(&Settings{}).Where("organization_id = ?", org).Updates(upd).Error == nil
}

// ---------- attachment routes ----------

// RegisterAttachmentRoutes mounts the attachment API under the invoicing
// group: GET /invoices/:id/attachments and /invoices/:id/attachments/:attachmentId.
func RegisterAttachmentRoutes(r *gin.RouterGroup, db *gorm.DB) {
	h := &Handler{DB: db}
	r.GET("/invoices/:id/attachments", h.ListAttachments)
	r.GET("/invoices/:id/attachments/:attachmentId", h.GetAttachment)
}

func (h *Handler) ListAttachments(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var list []Attachment
	if err := h.DB.Select("id, organization_id, invoice_id, name, content_type, size, source, kind, created_at").
		Where("organization_id = ? AND invoice_id = ?", org, c.Param("id")).Order("created_at ASC").Find(&list).Error; err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, list)
}

func (h *Handler) GetAttachment(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var a Attachment
	if err := h.DB.Where("organization_id = ? AND invoice_id = ? AND id = ?", org, c.Param("id"), c.Param("attachmentId")).First(&a).Error; err != nil {
		failErr(c, err)
		return
	}
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	disp := "inline"
	if c.Query("download") == "1" {
		disp = "attachment"
	}
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disp, strings.ReplaceAll(a.Name, `"`, "")))
	c.Header("Cache-Control", "private, max-age=300")
	c.Data(http.StatusOK, ct, a.Data)
}
