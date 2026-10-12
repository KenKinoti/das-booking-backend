package barstock

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ---------------------------------------------------------------- settings

func (h *Handler) settingsView(db *gorm.DB, org string) gin.H {
	st := h.S.Settings(db, org)
	targets := map[string]*float64{}
	for _, c := range Categories {
		targets[c] = nil
		if v, ok := st.PourCostTargets[c]; ok && v != nil {
			targets[c] = v
		}
	}
	return gin.H{
		"tot_ml": st.TotML, "day_cutoff": st.DayCutoff, "block_when_out": st.BlockWhenOut,
		"variance_pct_threshold": st.VariancePctThreshold, "variance_amount_threshold": st.VarianceAmountThreshold,
		"comp_allowance_per_shift": st.CompAllowancePerShift, "pour_cost_targets": targets,
		"night_report":   gin.H{"enabled": st.NightReportEnabled, "send_at": st.NightReportSendAt},
		"default_bar_id": st.DefaultBarID, "main_store_id": st.MainStoreID,
		"currency": h.S.Currency(db, org), "timezone": h.S.Location(db, org).String(),
		"business_date": h.S.Today(db, org), "categories": Categories, "category_labels": CategoryLabels,
		"location_types": LocationTypes, "wastage_reasons": WastageReasons,
	}
}

func (h *Handler) getSettings(c *gin.Context) {
	a := h.access(c)
	h.ok(c, http.StatusOK, a, h.settingsView(h.S.DB, a.Org))
}

type settingsInput struct {
	TotML                   *float64            `json:"tot_ml"`
	DayCutoff               *string             `json:"day_cutoff"`
	BlockWhenOut            *bool               `json:"block_when_out"`
	VariancePctThreshold    *float64            `json:"variance_pct_threshold"`
	VarianceAmountThreshold *float64            `json:"variance_amount_threshold"`
	CompAllowancePerShift   *float64            `json:"comp_allowance_per_shift"`
	PourCostTargets         map[string]*float64 `json:"pour_cost_targets"`
	NightReport             *struct {
		Enabled *bool   `json:"enabled"`
		SendAt  *string `json:"send_at"`
	} `json:"night_report"`
	DefaultBarID *string `json:"default_bar_id"`
	MainStoreID  *string `json:"main_store_id"`
}

func cutoffOK(s string) bool {
	h, m, ok := ParseHM(s)
	return ok && h*60+m <= 12*60
}

func (s *Service) applySettings(db *gorm.DB, org, user string, in settingsInput) (Settings, error) {
	st := s.Settings(db, org)
	if in.TotML != nil {
		if *in.TotML <= 0 || *in.TotML > 500 {
			return st, badReq("Tot size must be between 1 and 500 ml")
		}
		st.TotML = round3(*in.TotML)
	}
	if in.DayCutoff != nil {
		v := strings.TrimSpace(*in.DayCutoff)
		if !cutoffOK(v) {
			return st, badReq("Day cutoff must be a time between 00:00 and 12:00 (HH:MM)")
		}
		h, m, _ := ParseHM(v)
		st.DayCutoff = time.Date(2000, 1, 1, h, m, 0, 0, time.UTC).Format("15:04")
	}
	if in.BlockWhenOut != nil {
		st.BlockWhenOut = *in.BlockWhenOut
	}
	if in.VariancePctThreshold != nil {
		if *in.VariancePctThreshold < 0 || *in.VariancePctThreshold > 1000 {
			return st, badReq("Variance %% threshold must be 0 (off) or more")
		}
		st.VariancePctThreshold = *in.VariancePctThreshold
	}
	if in.VarianceAmountThreshold != nil {
		if *in.VarianceAmountThreshold < 0 {
			return st, badReq("Variance amount threshold must be 0 (off) or more")
		}
		st.VarianceAmountThreshold = *in.VarianceAmountThreshold
	}
	if in.CompAllowancePerShift != nil {
		if *in.CompAllowancePerShift < 0 || *in.CompAllowancePerShift > 10000 {
			return st, badReq("Comp allowance must be 0 or more drinks")
		}
		st.CompAllowancePerShift = *in.CompAllowancePerShift
	}
	if in.PourCostTargets != nil {
		if st.PourCostTargets == nil {
			st.PourCostTargets = map[string]*float64{}
		}
		for k, v := range in.PourCostTargets {
			if !validCategory(k) {
				return st, badReq("Unknown category %q", k)
			}
			if v == nil {
				delete(st.PourCostTargets, k)
				continue
			}
			if *v <= 0 || *v >= 100 {
				return st, badReq("Pour cost targets are percentages between 0 and 100 (leave empty for no target)")
			}
			st.PourCostTargets[k] = ptr(*v)
		}
	}
	if in.NightReport != nil {
		if in.NightReport.Enabled != nil {
			st.NightReportEnabled = *in.NightReport.Enabled
		}
		if in.NightReport.SendAt != nil {
			v := strings.TrimSpace(*in.NightReport.SendAt)
			h, m, ok := ParseHM(v)
			if !ok {
				return st, badReq("Night report time must be HH:MM")
			}
			st.NightReportSendAt = time.Date(2000, 1, 1, h, m, 0, 0, time.UTC).Format("15:04")
		}
	}
	ch, cm, _ := ParseHM(st.DayCutoff)
	sh, sm, _ := ParseHM(st.NightReportSendAt)
	if sh*60+sm < ch*60+cm {
		if in.NightReport != nil && in.NightReport.SendAt != nil {
			return st, badReq("Send the night report at or after the day cutoff (%s)", st.DayCutoff)
		}
		st.NightReportSendAt = st.DayCutoff
	}
	for _, f := range []struct {
		in  *string
		out **string
	}{{in.DefaultBarID, &st.DefaultBarID}, {in.MainStoreID, &st.MainStoreID}} {
		if f.in == nil {
			continue
		}
		v := strings.TrimSpace(*f.in)
		if v == "" {
			*f.out = nil
			continue
		}
		if _, err := s.location(db, org, v); err != nil {
			return st, err
		}
		*f.out = ptr(v)
	}
	st.OrganizationID, st.UpdatedBy = org, user
	if st.CreatedAt.IsZero() {
		st.CreatedAt = s.now()
	}
	st.UpdatedAt = s.now()
	return st, s.saveSettings(db, st)
}

func (h *Handler) putSettings(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Approve, "change bar stock settings"); err != nil {
		h.fail(c, err)
		return
	}
	var in settingsInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	if _, err := h.S.applySettings(h.S.DB, a.Org, a.User, in); err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, h.settingsView(h.S.DB, a.Org))
}

// ---------------------------------------------------------------- setup

func (h *Handler) catalog(c *gin.Context) {
	a := h.access(c)
	h.ok(c, http.StatusOK, a, Catalog)
}

// SetupInput is POST /bar/setup.
type SetupInput struct {
	Locations []struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		IsMainStore  bool   `json:"is_main_store"`
		IsDefaultBar bool   `json:"is_default_bar"`
	} `json:"locations"`
	TotML         *float64 `json:"tot_ml"`
	DayCutoff     *string  `json:"day_cutoff"`
	CatalogKeys   []string `json:"catalog_keys"`
	MakeMenuItems bool     `json:"make_menu_items"`
}

// SetupResult summarises what POST /bar/setup did.
type SetupResult struct {
	LocationsCreated []string `json:"locations_created"`
	LocationsExisted []string `json:"locations_existing"`
	ItemsCreated     []string `json:"items_created"`
	ItemsExisted     []string `json:"items_existing"`
	MenuItemsCreated []string `json:"menu_items_created"`
	UnknownKeys      []string `json:"unknown_keys"`
	MainStoreID      *string  `json:"main_store_id"`
	DefaultBarID     *string  `json:"default_bar_id"`
}

// Setup creates locations, settings, catalogue items and menu items. Running
// it again never duplicates: locations match by name (case-insensitive),
// items by catalogue key, menu items by (kind, stock item).
func (s *Service) Setup(org, user string, in SetupInput) (SetupResult, error) {
	res := SetupResult{LocationsCreated: []string{}, LocationsExisted: []string{}, ItemsCreated: []string{}, ItemsExisted: []string{},
		MenuItemsCreated: []string{}, UnknownKeys: []string{}}
	for _, l := range in.Locations {
		t := strings.ToLower(strings.TrimSpace(l.Type))
		if strings.TrimSpace(l.Name) == "" {
			return res, badReq("Each location needs a name")
		}
		if t == "" {
			t = "bar"
		}
		if !validLocationType(t) {
			return res, badReq("Location type must be one of %s", strings.Join(LocationTypes, ", "))
		}
	}
	for _, k := range in.CatalogKeys {
		if _, ok := CatalogByKey(k); !ok {
			res.UnknownKeys = append(res.UnknownKeys, k)
		}
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "barstock:setup:"+org).Error; err != nil {
			return err
		}
		if in.TotML != nil || in.DayCutoff != nil {
			if _, err := s.applySettings(tx, org, user, settingsInput{TotML: in.TotML, DayCutoff: in.DayCutoff}); err != nil {
				return err
			}
		}
		st := s.Settings(tx, org)
		existing, err := s.locations(tx, org)
		if err != nil {
			return err
		}
		byName := map[string]LocationRow{}
		for _, l := range existing {
			byName[strings.ToLower(strings.TrimSpace(l.Name))] = l
		}
		var mainStore, defaultBar, firstStore, firstBar string
		for _, l := range in.Locations {
			name := strings.TrimSpace(l.Name)
			t := strings.ToLower(strings.TrimSpace(l.Type))
			if t == "" {
				t = "bar"
			}
			id := ""
			if ex, ok := byName[strings.ToLower(name)]; ok {
				id = ex.ID
				res.LocationsExisted = append(res.LocationsExisted, name)
			} else {
				loc := models.InventoryLocation{ID: uuid.NewString(), OrganizationID: org, Name: name, Type: t, IsActive: true}
				if err := tx.Omit(clause.Associations).Create(&loc).Error; err != nil {
					return err
				}
				id = loc.ID
				byName[strings.ToLower(name)] = LocationRow{ID: id, Name: name, Type: t, IsActive: true}
				res.LocationsCreated = append(res.LocationsCreated, name)
			}
			if l.IsMainStore {
				mainStore = id
			}
			if l.IsDefaultBar {
				defaultBar = id
			}
			if t == "store" && firstStore == "" {
				firstStore = id
			}
			if t == "bar" && firstBar == "" {
				firstBar = id
			}
		}
		changed := false
		if mainStore != "" {
			st.MainStoreID, changed = ptr(mainStore), true
		} else if (st.MainStoreID == nil || *st.MainStoreID == "") && firstStore != "" {
			st.MainStoreID, changed = ptr(firstStore), true
		}
		if defaultBar != "" {
			st.DefaultBarID, changed = ptr(defaultBar), true
		} else if (st.DefaultBarID == nil || *st.DefaultBarID == "") && firstBar != "" {
			st.DefaultBarID, changed = ptr(firstBar), true
		}
		if changed {
			if st.CreatedAt.IsZero() {
				st.CreatedAt = s.now()
			}
			st.OrganizationID, st.UpdatedBy, st.UpdatedAt = org, user, s.now()
			if err := s.saveSettings(tx, st); err != nil {
				return err
			}
		}
		res.MainStoreID, res.DefaultBarID = st.MainStoreID, st.DefaultBarID

		for _, key := range uniq(in.CatalogKeys) {
			ci, ok := CatalogByKey(key)
			if !ok {
				continue
			}
			var ex Item
			found := tx.Where("organization_id = ? AND catalog_key = ?", org, key).Limit(1).Find(&ex).Error == nil && ex.ProductID != ""
			pid := ex.ProductID
			if found {
				res.ItemsExisted = append(res.ItemsExisted, ci.Name)
			} else {
				it, err := s.createItem(tx, org, user, ItemInput{Name: ci.Name, Category: ci.Category, BaseUnit: ci.BaseUnit, UnitSize: ptr(ci.UnitSize),
					UnitLabel: ci.UnitLabel, PackSize: ptr(ci.PackSize), PackLabel: ci.PackLabel, CatalogKey: key, SelfRecipe: contains(ci.MenuItems, "self")})
				if err != nil {
					return err
				}
				pid = it.ProductID
				res.ItemsCreated = append(res.ItemsCreated, ci.Name)
			}
			if in.MakeMenuItems {
				wantTot, wantBottle := contains(ci.MenuItems, "tot"), contains(ci.MenuItems, "bottle")
				if wantTot || wantBottle {
					made, err := s.makeMenuItems(tx, org, pid, MenuInput{Tot: wantTot, Bottle: wantBottle})
					if err != nil {
						return err
					}
					for _, m := range made {
						if m.Created {
							res.MenuItemsCreated = append(res.MenuItemsCreated, m.Name)
						}
					}
				}
				if contains(ci.MenuItems, "self") {
					if _, err := s.ensureSelfRecipe(tx, org, pid); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	return res, err
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (h *Handler) setup(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Edit, "set up bar stock"); err != nil {
		h.fail(c, err)
		return
	}
	var in SetupInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	res, err := h.S.Setup(a.Org, a.User, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"summary": res, "settings": h.settingsView(h.S.DB, a.Org)})
}

// ---------------------------------------------------------------- locations

func (h *Handler) locationViews(db *gorm.DB, a access) ([]gin.H, error) {
	locs, err := h.S.locations(db, a.Org)
	if err != nil {
		return nil, err
	}
	st := h.S.Settings(db, a.Org)
	var staff []LocationStaff
	db.Where("organization_id = ?", a.Org).Find(&staff)
	byLoc := map[string][]string{}
	for _, s := range staff {
		byLoc[s.LocationID] = append(byLoc[s.LocationID], s.UserID)
	}
	type agg struct {
		LocationID string
		Items      int
		Value      float64
	}
	var aggs []agg
	db.Raw(`SELECT s.location_id, COUNT(*) FILTER (WHERE s.qty <> 0) AS items, COALESCE(SUM(GREATEST(s.qty,0) * i.avg_cost), 0) AS value
		FROM bar_stock s JOIN bar_items i ON i.product_id = s.product_id AND i.organization_id = s.organization_id
		WHERE s.organization_id = ? GROUP BY s.location_id`, a.Org).Scan(&aggs)
	am := map[string]agg{}
	for _, x := range aggs {
		am[x.LocationID] = x
	}
	cur := h.S.Currency(db, a.Org)
	out := []gin.H{}
	for _, l := range locs {
		if !a.canLoc(l.ID) {
			continue
		}
		ids := byLoc[l.ID]
		if ids == nil {
			ids = []string{}
		}
		out = append(out, gin.H{"id": l.ID, "name": l.Name, "type": l.Type, "description": l.Description, "is_active": l.IsActive,
			"is_main_store": st.MainStoreID != nil && *st.MainStoreID == l.ID, "is_default_bar": st.DefaultBarID != nil && *st.DefaultBarID == l.ID,
			"staff_ids": ids, "item_count": am[l.ID].Items, "stock_value": RoundMoney(am[l.ID].Value, cur)})
	}
	return out, nil
}

func (h *Handler) listLocations(c *gin.Context) {
	a := h.access(c)
	out, err := h.locationViews(h.S.DB, a)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}

type locationInput struct {
	Name         *string   `json:"name"`
	Type         *string   `json:"type"`
	Description  *string   `json:"description"`
	IsActive     *bool     `json:"is_active"`
	IsMainStore  *bool     `json:"is_main_store"`
	IsDefaultBar *bool     `json:"is_default_bar"`
	StaffIDs     *[]string `json:"staff_ids"`
}

func (s *Service) saveLocation(org, user, id string, in locationInput) (string, error) {
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var loc models.InventoryLocation
		if id != "" {
			if err := tx.Where("id = ? AND organization_id = ?", id, org).First(&loc).Error; err != nil {
				return notFound("Location not found")
			}
		} else {
			loc = models.InventoryLocation{ID: uuid.NewString(), OrganizationID: org, Type: "bar", IsActive: true}
		}
		if in.Name != nil {
			loc.Name = strings.TrimSpace(*in.Name)
		}
		if loc.Name == "" {
			return badReq("Enter a name")
		}
		var dup int64
		tx.Model(&models.InventoryLocation{}).Where("organization_id = ? AND LOWER(name) = LOWER(?) AND id <> ?", org, loc.Name, loc.ID).Count(&dup)
		if dup > 0 {
			return conflict("A location called %s already exists", loc.Name)
		}
		if in.Type != nil {
			t := strings.ToLower(strings.TrimSpace(*in.Type))
			if !validLocationType(t) {
				return badReq("Location type must be one of %s", strings.Join(LocationTypes, ", "))
			}
			loc.Type = t
		}
		if in.Description != nil {
			loc.Description = strings.TrimSpace(*in.Description)
		}
		if in.IsActive != nil {
			loc.IsActive = *in.IsActive
		}
		if id == "" {
			if err := tx.Omit(clause.Associations).Create(&loc).Error; err != nil {
				return err
			}
		} else if err := tx.Model(&models.InventoryLocation{}).Where("id = ?", loc.ID).Updates(map[string]any{
			"name": loc.Name, "type": loc.Type, "description": loc.Description, "is_active": loc.IsActive, "updated_at": s.now()}).Error; err != nil {
			return err
		}
		id = loc.ID
		st := s.Settings(tx, org)
		changed := false
		if in.IsMainStore != nil {
			if *in.IsMainStore {
				st.MainStoreID = ptr(id)
			} else if st.MainStoreID != nil && *st.MainStoreID == id {
				st.MainStoreID = nil
			}
			changed = true
		}
		if in.IsDefaultBar != nil {
			if *in.IsDefaultBar {
				st.DefaultBarID = ptr(id)
			} else if st.DefaultBarID != nil && *st.DefaultBarID == id {
				st.DefaultBarID = nil
			}
			changed = true
		}
		if changed {
			if st.CreatedAt.IsZero() {
				st.CreatedAt = s.now()
			}
			st.OrganizationID, st.UpdatedBy, st.UpdatedAt = org, user, s.now()
			if err := s.saveSettings(tx, st); err != nil {
				return err
			}
		}
		if in.StaffIDs != nil {
			ids := uniq(*in.StaffIDs)
			if len(ids) > 0 {
				var n int64
				tx.Table("users").Where("organization_id = ? AND id IN ? AND deleted_at IS NULL", org, ids).Count(&n)
				if int(n) != len(ids) {
					return badReq("One or more staff were not found")
				}
			}
			if err := tx.Where("organization_id = ? AND location_id = ?", org, id).Delete(&LocationStaff{}).Error; err != nil {
				return err
			}
			for _, u := range ids {
				if err := tx.Create(&LocationStaff{OrganizationID: org, LocationID: id, UserID: u}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	return id, err
}

func (h *Handler) createLocation(c *gin.Context) { h.writeLocation(c, "") }
func (h *Handler) updateLocation(c *gin.Context) { h.writeLocation(c, c.Param("id")) }

func (h *Handler) writeLocation(c *gin.Context, id string) {
	a := h.access(c)
	if err := a.need(a.Edit, "manage locations"); err != nil {
		h.fail(c, err)
		return
	}
	var in locationInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	if id == "" && in.Name == nil {
		h.fail(c, badReq("Enter a name"))
		return
	}
	newID, err := h.S.saveLocation(a.Org, a.User, id, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	views, _ := h.locationViews(h.S.DB, a)
	for _, v := range views {
		if v["id"] == newID {
			status := http.StatusOK
			if id == "" {
				status = http.StatusCreated
			}
			h.ok(c, status, a, v)
			return
		}
	}
	h.ok(c, http.StatusOK, a, gin.H{"id": newID})
}

func (h *Handler) deleteLocation(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Delete, "delete locations"); err != nil {
		h.fail(c, err)
		return
	}
	id := c.Param("id")
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		if _, err := h.S.location(tx, a.Org, id); err != nil {
			return err
		}
		var n int64
		tx.Model(&Stock{}).Where("organization_id = ? AND location_id = ? AND qty <> 0", a.Org, id).Count(&n)
		if n > 0 {
			return conflict("This location still holds stock — transfer or count it to zero first")
		}
		var open int64
		tx.Model(&Transfer{}).Where("organization_id = ? AND status IN ? AND (from_location_id = ? OR to_location_id = ?)", a.Org, []string{TrRequested, TrIssued}, id, id).Count(&open)
		if open > 0 {
			return conflict("Finish or cancel the open transfers of this location first")
		}
		if err := tx.Where("organization_id = ? AND id = ?", a.Org, id).Delete(&models.InventoryLocation{}).Error; err != nil {
			return err
		}
		tx.Where("organization_id = ? AND location_id = ?", a.Org, id).Delete(&LocationStaff{})
		tx.Where("organization_id = ? AND location_id = ?", a.Org, id).Delete(&Terminal{})
		tx.Where("organization_id = ? AND location_id = ?", a.Org, id).Delete(&Par{})
		st := h.S.Settings(tx, a.Org)
		if (st.MainStoreID != nil && *st.MainStoreID == id) || (st.DefaultBarID != nil && *st.DefaultBarID == id) {
			if st.MainStoreID != nil && *st.MainStoreID == id {
				st.MainStoreID = nil
			}
			if st.DefaultBarID != nil && *st.DefaultBarID == id {
				st.DefaultBarID = nil
			}
			st.UpdatedAt = h.S.now()
			return h.S.saveSettings(tx, st)
		}
		return nil
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"deleted": true, "id": id})
}

// ---------------------------------------------------------------- terminals

func (h *Handler) terminalViews(a access) []gin.H {
	var rows []Terminal
	h.S.DB.Where("organization_id = ?", a.Org).Order("terminal_id").Find(&rows)
	var known []string
	h.S.DB.Table("cash_drawers").Where("organization_id = ? AND deleted_at IS NULL AND terminal_id <> ''", a.Org).Distinct("terminal_id").Pluck("terminal_id", &known)
	out := []gin.H{}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.TerminalID] = true
		out = append(out, gin.H{"terminal_id": r.TerminalID, "location_id": r.LocationID})
	}
	for _, t := range known {
		if !seen[t] {
			seen[t] = true
			out = append(out, gin.H{"terminal_id": t, "location_id": nil})
		}
	}
	return out
}

func (h *Handler) getTerminals(c *gin.Context) {
	a := h.access(c)
	h.ok(c, http.StatusOK, a, h.terminalViews(a))
}

func (h *Handler) putTerminals(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Edit, "map POS terminals"); err != nil {
		h.fail(c, err)
		return
	}
	type row struct {
		TerminalID string  `json:"terminal_id"`
		LocationID *string `json:"location_id"`
	}
	var rows []row
	raw, _ := c.GetRawData()
	var wrapped struct {
		Terminals []row `json:"terminals"`
	}
	if err := jsonUnmarshal(raw, &rows); err != nil {
		if err2 := jsonUnmarshal(raw, &wrapped); err2 != nil {
			h.fail(c, badReq("Send [{terminal_id, location_id}]"))
			return
		}
		rows = wrapped.Terminals
	}
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("organization_id = ?", a.Org).Delete(&Terminal{}).Error; err != nil {
			return err
		}
		for _, r := range rows {
			t := strings.TrimSpace(r.TerminalID)
			if t == "" || r.LocationID == nil || strings.TrimSpace(*r.LocationID) == "" {
				continue
			}
			if len(t) > 50 {
				return badReq("Terminal ids are at most 50 characters")
			}
			if _, err := h.S.location(tx, a.Org, *r.LocationID); err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&Terminal{OrganizationID: a.Org, TerminalID: t, LocationID: *r.LocationID, UpdatedAt: h.S.now()}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, h.terminalViews(a))
}
