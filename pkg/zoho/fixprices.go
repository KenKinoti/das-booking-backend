package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// "Fix item prices from Zoho": re-read the items (Zoho API, or the last
// uploaded Item.csv) and correct only price, currency and tax-inclusive on
// the services imported from them (found through the external refs).
// Services edited here since the last sync are left alone unless asked.
//
//	POST /api/v1/imports/zoho/fix-prices
//	  {"dry_run": true, "source": "auto|api|file", "item_prices": {...}, "include_edited": false}

// FixPricesRequest is the repair request.
type FixPricesRequest struct {
	DryRun        bool             `json:"dry_run"`
	Source        string           `json:"source"` // auto (API when connected, else file) | api | file
	ItemPrices    ItemPriceOptions `json:"item_prices"`
	IncludeEdited bool             `json:"include_edited"`
}

// FixPriceRow is one service's old → new price.
type FixPriceRow struct {
	ServiceID   string  `json:"service_id"`
	Name        string  `json:"name"`
	ZohoPrice   float64 `json:"zoho_price"`
	ZohoCur     string  `json:"zoho_currency"`
	OldPrice    float64 `json:"old_price"`
	OldCurrency string  `json:"old_currency"`
	OldInclTax  bool    `json:"old_includes_tax"`
	NewPrice    float64 `json:"new_price"`
	NewCurrency string  `json:"new_currency"`
	NewInclTax  bool    `json:"new_includes_tax"`
	// Status: change | same | edited (changed here since the import, or a
	// service that existed before the import; skipped unless include_edited) | applied
	Status string `json:"status"`
}

// FixPricesResult is the dry run / apply outcome.
type FixPricesResult struct {
	Source         string        `json:"source"` // api | file
	SourceLabel    string        `json:"source_label"`
	SourceCurrency string        `json:"source_currency"`
	HomeCurrency   string        `json:"home_currency"`
	Pricing        *ItemPricing  `json:"pricing"`
	Rows           []FixPriceRow `json:"rows"`
	Changes        int           `json:"changes"`
	Applied        int           `json:"applied"`
	Edited         int           `json:"edited"`
	Unchanged      int           `json:"unchanged"`
	NotImported    int           `json:"not_imported"`
	DryRun         bool          `json:"dry_run"`
}

var errNoItemSource = errors.New("no items to read: connect Zoho Books or upload your Zoho Item.csv in a file import first")

// itemsForRepair reads the items and their source currency.
func (e *Engine) itemsForRepair(ctx context.Context, org, source string) (items []Item, srcBase, kind, label string, err error) {
	var conn Connection
	connected := e.DB.Where("organization_id = ?", org).Take(&conn).Error == nil && conn.RefreshToken != "" && conn.ZohoOrgID != ""
	if source == "api" || (source != "file" && connected) {
		if !connected {
			return nil, "", "", "", errors.New("Zoho Books is not connected")
		}
		c, err := e.ClientFor(&conn)
		if err != nil {
			return nil, "", "", "", err
		}
		srcBase = strings.ToUpper(conn.ZohoCurrency)
		if orgs, err := c.Organizations(ctx); err == nil {
			for _, o := range orgs {
				if o.OrganizationID == conn.ZohoOrgID && o.CurrencyCode != "" {
					srcBase = strings.ToUpper(o.CurrencyCode)
				}
			}
		}
		l := apiLists[EntItems]
		for page := 1; page < 200; page++ {
			rows, more, err := c.Page(ctx, l.path, l.key, page, l.extra)
			if err != nil {
				return nil, "", "", "", err
			}
			for _, raw := range rows {
				var z zItem
				if json.Unmarshal(raw, &z) == nil {
					items = append(items, z.norm())
				}
			}
			if !more {
				break
			}
		}
		return items, srcBase, "api", "Zoho Books (" + firstNonEmpty(conn.ZohoOrgName, "connected organisation") + ")", nil
	}
	var u Upload
	if err := e.DB.Where("org_id = ? AND kind = ?", org, FileItems).Order("created_at DESC").Take(&u).Error; err != nil {
		return nil, "", "", "", errNoItemSource
	}
	ds, err := BuildDataset([]Upload{u}, true)
	if err != nil {
		return nil, "", "", "", err
	}
	return ds.Items, "", "file", u.Name + " (uploaded " + u.CreatedAt.Format("2 Jan 2006") + ")", nil
}

// FixPrices runs the repair (dry run or apply).
func (e *Engine) FixPrices(ctx context.Context, org string, req FixPricesRequest) (*FixPricesResult, error) {
	items, srcBase, kind, label, err := e.itemsForRepair(ctx, org, req.Source)
	if err != nil {
		return nil, err
	}
	home := strings.ToUpper(orgprefs.Currency(e.DB, org))
	im := &Importer{DB: e.DB, Org: org, RefSource: "zoho", Home: home, SrcBase: srcBase, Opts: Options{ItemPrices: req.ItemPrices}, Now: e.Now}
	if kind == "file" && req.ItemPrices.Currency == "" {
		im.SrcBase = home
	}
	res := &FixPricesResult{Source: kind, SourceLabel: label, HomeCurrency: home, Rows: []FixPriceRow{}, DryRun: req.DryRun}
	res.Pricing = e.itemPricing(im, items)
	res.SourceCurrency = res.Pricing.SourceCurrency
	if im.Opts.ItemPrices.Currency == "" {
		// Price items in the currency the data shows (e.g. "AUD 250.00").
		im.Opts.ItemPrices.Currency = res.SourceCurrency
	}
	var refs []ExternalRef
	e.DB.Where("organization_id = ? AND entity = ? AND source IN ?", org, entItemRef, []string{"zoho", "csv"}).Find(&refs)
	byExt := map[string]*ExternalRef{}
	for i := range refs {
		byExt[refs[i].ExternalID] = &refs[i]
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.005 }
	for _, it := range items {
		ext := it.ExtID
		if ext == "" {
			ext = "name:" + lowerTrim(it.Name)
		}
		ref := byExt[ext]
		if ref == nil {
			res.NotImported++
			continue
		}
		var svc models.Service
		if e.DB.Where("id = ? AND organization_id = ?", ref.LocalID, org).Take(&svc).Error != nil {
			res.NotImported++
			continue
		}
		price, cur, incl := im.ItemPrice(it)
		oldCur := strings.ToUpper(firstNonEmpty(svc.Currency, home))
		row := FixPriceRow{ServiceID: svc.ID, Name: svc.Name, ZohoPrice: it.Rate, ZohoCur: strings.ToUpper(firstNonEmpty(it.Currency, im.itemSource())),
			OldPrice: svc.Price, OldCurrency: oldCur, OldInclTax: svc.PriceIncludesTax, NewPrice: price, NewCurrency: cur, NewInclTax: incl}
		switch {
		case near(svc.Price, price) && oldCur == cur && svc.PriceIncludesTax == incl:
			row.Status = "same"
			res.Unchanged++
		// Edited here since the import, or a service that existed before
		// and was only linked to the Zoho item: left alone unless chosen.
		case (svc.UpdatedAt.After(ref.LastSynced.Add(2*time.Second)) || !ref.Created) && !req.IncludeEdited:
			row.Status = "edited"
			res.Edited++
		default:
			row.Status = "change"
			res.Changes++
			if !req.DryRun {
				err := e.DB.Transaction(func(tx *gorm.DB) error {
					if err := tx.Model(&models.Service{}).Where("id = ? AND organization_id = ?", svc.ID, org).Updates(map[string]interface{}{
						"price": price, "currency": cur, "price_includes_tax": incl, "updated_at": now}).Error; err != nil {
						return err
					}
					// Our own write isn't a local edit; the next re-sync sees the new hash.
					return tx.Model(&ExternalRef{}).Where("id = ?", ref.ID).Updates(map[string]interface{}{"last_synced": now.Add(time.Second), "hash": im.itemHash(it)}).Error
				})
				if err != nil {
					return nil, err
				}
				row.Status = "applied"
				res.Applied++
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// FixItemPrices serves POST /imports/zoho/fix-prices.
func (h *Handler) FixItemPrices(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	var req FixPricesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	req.ItemPrices.Currency = strings.ToUpper(strings.TrimSpace(req.ItemPrices.Currency))
	if req.ItemPrices.Mode == "convert" && !(req.ItemPrices.Rate > 0) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter the exchange rate to convert with")
		return
	}
	if j := h.busy(org); j != nil && !req.DryRun {
		fail(c, http.StatusConflict, "IMPORT_RUNNING", "An import is running — wait for it to finish")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	res, err := h.Engine.FixPrices(ctx, org, req)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNoItemSource) {
			status = http.StatusNotFound
		}
		fail(c, status, "FIX_PRICES_FAILED", err.Error())
		return
	}
	ok(c, res)
}
