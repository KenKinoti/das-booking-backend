package mpos

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var digitsRe = regexp.MustCompile(`^\d+$`)

// gtinCheck is the GS1 mod-10 check digit for the digits before it.
func gtinCheck(body string) byte {
	sum := 0
	w := 3
	for i := len(body) - 1; i >= 0; i-- {
		sum += int(body[i]-'0') * w
		if w == 3 {
			w = 1
		} else {
			w = 3
		}
	}
	return byte('0' + (10-sum%10)%10)
}

// ValidGTIN: EAN-8, UPC-A, EAN-13 or GTIN-14 with a correct check digit.
func ValidGTIN(s string) bool {
	if !digitsRe.MatchString(s) {
		return false
	}
	switch len(s) {
	case 8, 12, 13, 14:
		return gtinCheck(s[:len(s)-1]) == s[len(s)-1]
	}
	return false
}

// BarcodeKeys lists the spellings one barcode can have when it is stored on a
// product: the code itself (lower case) and, for GTINs, the same number with or
// without leading zeros (a UPC-A 036000291452 is the EAN-13 0036000291452).
func BarcodeKeys(code string) []string {
	raw := strings.ToLower(strings.TrimSpace(code))
	if raw == "" {
		return nil
	}
	out := []string{raw}
	seen := map[string]bool{raw: true}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	d := strings.NewReplacer(" ", "", "-", "").Replace(raw)
	if !digitsRe.MatchString(d) || !ValidGTIN(d) {
		return out
	}
	core := strings.TrimLeft(d, "0")
	for _, n := range []int{8, 12, 13, 14} {
		if len(core) <= n {
			add(strings.Repeat("0", n-len(core)) + core)
		}
	}
	return out
}

var barcodeOK = regexp.MustCompile(`^[\x21-\x7E](?:[\x20-\x7E]{0,98}[\x21-\x7E])?$`)

// CleanBarcode checks a scanned / typed barcode: 1–100 printable characters.
func CleanBarcode(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("Scan or type a barcode")
	}
	if len(s) > 100 {
		return "", errors.New("That barcode is too long")
	}
	if !barcodeOK.MatchString(s) {
		return "", errors.New("A barcode can only contain letters, digits and symbols")
	}
	return s, nil
}

type productRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	SKU     string `json:"sku"`
	Barcode string `json:"barcode"`
}

// ProductWithBarcode finds another product of the organisation that already
// has this barcode (in any of its spellings).
func ProductWithBarcode(db *gorm.DB, org, code, except string) (*productRef, error) {
	keys := BarcodeKeys(code)
	if len(keys) == 0 {
		return nil, nil
	}
	var p productRef
	q := db.Table("products").Select("id, name, sku, barcode").
		Where("organization_id = ? AND deleted_at IS NULL AND LOWER(TRIM(barcode)) IN ?", org, keys)
	if except != "" {
		q = q.Where("id <> ?", except)
	}
	res := q.Order("is_active DESC, name").Limit(1).Scan(&p)
	if res.Error != nil {
		return nil, res.Error
	}
	if p.ID == "" {
		return nil, nil
	}
	return &p, nil
}

// linkBarcode: POST /pos/barcode-links {product_id, barcode, replace, move}
//
//	409 code=BARCODE_TAKEN     another product has it (move: true takes it over)
//	409 code=PRODUCT_HAS_CODE  the product has a different barcode (replace: true overwrites)
func (h *Handler) linkBarcode(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	if !h.manager(w) {
		forbidden(c, "Ask your manager to link this barcode to a product.")
		return
	}
	var in struct {
		ProductID string `json:"product_id"`
		Barcode   string `json:"barcode"`
		Replace   bool   `json:"replace"`
		Move      bool   `json:"move"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	code, err := CleanBarcode(in.Barcode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var p productRef
	if err := h.DB.Table("products").Select("id, name, sku, barcode").
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", strings.TrimSpace(in.ProductID), w.Org).Limit(1).Scan(&p).Error; err != nil || p.ID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	for _, k := range BarcodeKeys(p.Barcode) {
		if k == strings.ToLower(code) {
			c.JSON(http.StatusOK, gin.H{"product": p, "unchanged": true})
			return
		}
	}
	other, err := ProductWithBarcode(h.DB, w.Org, code, p.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not check the barcode"})
		return
	}
	if other != nil && !in.Move {
		c.JSON(http.StatusConflict, gin.H{"error": "Barcode " + code + " is already on " + other.Name, "code": "BARCODE_TAKEN", "product": other})
		return
	}
	if strings.TrimSpace(p.Barcode) != "" && !in.Replace {
		c.JSON(http.StatusConflict, gin.H{"error": p.Name + " already has barcode " + p.Barcode, "code": "PRODUCT_HAS_CODE", "current_barcode": p.Barcode})
		return
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if other != nil {
			if err := tx.Table("products").Where("id = ? AND organization_id = ?", other.ID, w.Org).
				Updates(map[string]interface{}{"barcode": "", "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return tx.Table("products").Where("id = ? AND organization_id = ?", p.ID, w.Org).
			Updates(map[string]interface{}{"barcode": code, "updated_at": now}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save the barcode"})
		return
	}
	p.Barcode = code
	resp := gin.H{"product": p}
	if other != nil {
		resp["moved_from"] = other
	}
	c.JSON(http.StatusOK, resp)
}
