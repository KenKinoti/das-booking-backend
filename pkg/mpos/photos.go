package mpos

import (
	"bytes"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	// MaxPhotoBytes: the till sends ~100–300 KB JPEGs (1280 px, compressed on the device).
	MaxPhotoBytes = 600 * 1024
	// MaxPhotosPerSale keeps a sale's attachments to what a person needs.
	MaxPhotosPerSale = 5
	// MaxPhotoSide rejects absurd images (decompression bombs).
	MaxPhotoSide = 4096
	// staffAttachWindow: staff may add photos to their own sale for this long.
	staffAttachWindow = 24 * time.Hour
)

var photoKinds = map[string]bool{"payment": true, "damage": true, "other": true}

type saleRow struct {
	ID         string
	CashierID  string
	CreatedAt  time.Time
	PhotoCount int
}

func (h *Handler) sale(org, id string) (*saleRow, error) {
	var s saleRow
	err := h.DB.Table("pos_transactions").Select("id, cashier_id, created_at, photo_count").
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", id, org).Limit(1).Scan(&s).Error
	if err != nil || s.ID == "" {
		return nil, err
	}
	return &s, nil
}

// addPhoto: POST /pos/transactions/:id/photos (multipart: photo=<jpeg>, kind, note).
func (h *Handler) addPhoto(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	s, err := h.sale(w.Org, c.Param("id"))
	if err != nil || s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sale not found"})
		return
	}
	mgr := h.manager(w)
	if !mgr && (s.CashierID != w.User || time.Since(s.CreatedAt) > staffAttachWindow) {
		forbidden(c, "You can add photos to your own sales on the day. Ask your manager for older sales.")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxPhotoBytes+64*1024)
	fh, err := c.FormFile("photo")
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "That photo is too large (600 KB at most)"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Attach a photo"})
		return
	}
	if fh.Size > MaxPhotoBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "That photo is too large (600 KB at most)"})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Could not read the photo"})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxPhotoBytes+1))
	if err != nil || len(data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Could not read the photo"})
		return
	}
	if len(data) > MaxPhotoBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "That photo is too large (600 KB at most)"})
		return
	}
	if len(data) < 3 || data[0] != 0xFF || data[1] != 0xD8 || data[2] != 0xFF {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Photos must be JPEG images"})
		return
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxPhotoSide || cfg.Height > MaxPhotoSide {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "That isn't a JPEG photo we can store"})
		return
	}
	kind := strings.ToLower(strings.TrimSpace(c.PostForm("kind")))
	if !photoKinds[kind] {
		kind = "other"
	}
	note := cleanLine(c.PostForm("note"), 200)
	p := SalePhoto{OrganizationID: w.Org, TransactionID: s.ID, UploadedBy: w.User, Kind: kind, Note: note,
		ContentType: "image/jpeg", Size: len(data), Width: cfg.Width, Height: cfg.Height, Data: data}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		tx.Model(&SalePhoto{}).Where("organization_id = ? AND transaction_id = ?", w.Org, s.ID).Count(&n)
		if n >= MaxPhotosPerSale {
			return errTooMany
		}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		return tx.Table("pos_transactions").Where("id = ? AND organization_id = ?", s.ID, w.Org).
			UpdateColumn("photo_count", n+1).Error
	})
	if err == errTooMany {
		c.JSON(http.StatusConflict, gin.H{"error": "A sale can have up to 5 photos"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save the photo"})
		return
	}
	p.UploaderName = h.userName(p.UploadedBy)
	c.JSON(http.StatusCreated, gin.H{"photo": p})
}

type sentinel string

func (e sentinel) Error() string { return string(e) }

const errTooMany = sentinel("too many photos")

func (h *Handler) userName(id string) string {
	var u struct{ FirstName, LastName, Email string }
	h.DB.Table("users").Select("first_name, last_name, email").Where("id = ?", id).Limit(1).Scan(&u)
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return u.Email
}

// listPhotos: managers see every photo of the sale; staff the ones they added.
func (h *Handler) listPhotos(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	s, err := h.sale(w.Org, c.Param("id"))
	if err != nil || s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sale not found"})
		return
	}
	q := h.DB.Select("id, transaction_id, uploaded_by, kind, note, content_type, size, width, height, created_at").
		Where("organization_id = ? AND transaction_id = ?", w.Org, s.ID)
	mgr := h.manager(w)
	if !mgr {
		q = q.Where("uploaded_by = ?", w.User)
	}
	photos := []SalePhoto{}
	if err := q.Order("created_at ASC").Find(&photos).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not load the photos"})
		return
	}
	names := map[string]string{}
	for i := range photos {
		id := photos[i].UploadedBy
		if _, ok := names[id]; !ok {
			names[id] = h.userName(id)
		}
		photos[i].UploaderName = names[id]
	}
	c.JSON(http.StatusOK, gin.H{"photos": photos, "total": s.PhotoCount, "can_view_all": mgr})
}

func (h *Handler) photo(c *gin.Context, w caller) (*SalePhoto, bool) {
	var p SalePhoto
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("photo_id"), w.Org).First(&p).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Photo not found"})
		return nil, false
	}
	if !h.manager(w) && p.UploadedBy != w.User {
		forbidden(c, "Only managers can open the photos of a sale.")
		return nil, false
	}
	return &p, true
}

func (h *Handler) getPhoto(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	p, ok := h.photo(c, w)
	if !ok {
		return
	}
	c.Header("Cache-Control", "private, max-age=3600")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", `inline; filename="sale-photo-`+p.ID[:8]+`.jpg"`)
	c.Data(http.StatusOK, "image/jpeg", p.Data)
}

func (h *Handler) deletePhoto(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	if !h.manager(w) {
		forbidden(c, "Only managers can remove the photos of a sale.")
		return
	}
	p, ok := h.photo(c, w)
	if !ok {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND organization_id = ?", p.ID, w.Org).Delete(&SalePhoto{}).Error; err != nil {
			return err
		}
		var n int64
		tx.Model(&SalePhoto{}).Where("organization_id = ? AND transaction_id = ?", w.Org, p.TransactionID).Count(&n)
		return tx.Table("pos_transactions").Where("id = ? AND organization_id = ?", p.TransactionID, w.Org).UpdateColumn("photo_count", n).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not remove the photo"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
