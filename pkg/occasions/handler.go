package occasions

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Handler serves the seasonal themes API:
//
//	GET  /occasions/theme?country=      today's theme for the signed-in person
//	GET  /occasions/settings            the organisation's and the person's switches
//	PUT  /occasions/settings/org        {enabled}       organisation owners / admins
//	PUT  /occasions/settings/me         {show: bool|null}
//	/super-admin/occasions/**           calendar, switches, custom occasions, preview
type Handler struct {
	DB  *gorm.DB
	Svc *Service
}

// RegisterRoutes mounts the routes on the authenticated group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	auth.GET("/occasions/theme", h.theme)
	auth.GET("/occasions/settings", h.settings)
	auth.PUT("/occasions/settings/org", h.setOrg)
	auth.PUT("/occasions/settings/me", h.setMe)

	sa := auth.Group("/super-admin/occasions", requireSuperAdmin)
	sa.GET("", h.list)
	sa.GET("/themes", h.themes)
	sa.GET("/resolve", h.resolve)
	sa.PUT("/settings", h.setGlobal)
	sa.PUT("/toggles/:key", h.setToggle)
	sa.GET("/custom", h.listCustom)
	sa.POST("/custom", h.createCustom)
	sa.PUT("/custom/:id", h.updateCustom)
	sa.DELETE("/custom/:id", h.deleteCustom)
}

func requireSuperAdmin(c *gin.Context) {
	if c.GetString("user_role") != "super_admin" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "error": gin.H{"code": "SUPER_ADMIN_REQUIRED", "message": "Super administrator access required"}})
		return
	}
	c.Next()
}

func fail(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"success": false, "error": msg, "message": msg})
}

// CanEditOrg: who may switch the organisation's seasonal themes.
func CanEditOrg(role string) bool {
	switch role {
	case "owner", "admin", "org_admin", "super_admin":
		return true
	}
	return false
}

var iso2 = regexp.MustCompile(`^[A-Z]{2}$`)

// orgContext returns the organisation's country (may be "") and today in
// its time zone.
func (h *Handler) orgContext(org string) (string, time.Time) {
	if org == "" {
		return "", Today(time.UTC)
	}
	p := orgprefs.Get(h.DB, org)
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return p.CountryCode, Today(loc)
}

// ThemeResponse is GET /occasions/theme.
type ThemeResponse struct {
	Active   bool      `json:"active"`
	Reason   string    `json:"reason"`
	Theme    string    `json:"theme"`
	Occasion *Occasion `json:"occasion"`
	Today    string    `json:"today"`
	Country  string    `json:"country"`
	Settings Switches  `json:"settings"`
}

// Switches are the three levels of the seasonal themes setting.
type Switches struct {
	Global    bool  `json:"global"`
	Org       bool  `json:"org"`
	User      *bool `json:"user"`
	Effective bool  `json:"effective"`
	CanEdit   bool  `json:"can_edit_org"`
}

func (h *Handler) switches(c *gin.Context) Switches {
	org, user := c.GetString("org_id"), c.GetString("user_id")
	s := Switches{Global: h.Svc.GlobalEnabled(), Org: OrgEnabled(h.DB, org), User: UserChoice(h.DB, user), CanEdit: CanEditOrg(c.GetString("user_role")) && org != ""}
	s.Effective, _ = Resolve(s.Global, s.Org, s.User)
	return s
}

func (h *Handler) theme(c *gin.Context) {
	country, today := h.orgContext(c.GetString("org_id"))
	superAdmin := c.GetString("user_role") == "super_admin"
	if q := strings.ToUpper(strings.TrimSpace(c.Query("country"))); iso2.MatchString(q) {
		country = q
	}
	if superAdmin {
		if d, ok := ParseDay(c.Query("date")); ok {
			today = d
		}
	}
	sw := h.switches(c)
	out := ThemeResponse{Today: today.Format("2006-01-02"), Country: country, Settings: sw}

	// A super admin's live preview of any theme or occasion.
	if p := strings.TrimSpace(c.Query("preview")); p != "" && superAdmin {
		if o := h.Svc.Next(p, today, country); o != nil {
			out.Active, out.Reason, out.Theme, out.Occasion = true, ReasonPreview, o.Theme, o
		} else if t, ok := ThemeByKey(p); ok {
			o := &Occasion{Key: t.Key, Name: t.Label, Theme: t.Key, Date: today, ThemeFrom: today, ThemeTo: today, Greeting: t.Label, Countries: []string{}}
			out.Active, out.Reason, out.Theme, out.Occasion = true, ReasonPreview, t.Key, o
		} else {
			fail(c, http.StatusNotFound, "No theme or occasion called "+p)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
		return
	}

	on, reason := Resolve(sw.Global, sw.Org, sw.User)
	out.Reason = reason
	if on {
		var o *Occasion
		if country == "" {
			o = h.Svc.ThemeOnGlobal(today)
		} else {
			o = h.Svc.ThemeOn(today, country)
		}
		if o != nil {
			out.Active, out.Theme, out.Occasion = true, o.Theme, o
		} else {
			out.Reason = ReasonNone
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

func (h *Handler) settings(c *gin.Context) {
	country, today := h.orgContext(c.GetString("org_id"))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"settings": h.switches(c), "country": country, "today": today.Format("2006-01-02")}})
}

func (h *Handler) setOrg(c *gin.Context) {
	org := c.GetString("org_id")
	if org == "" {
		fail(c, http.StatusBadRequest, "No organisation in this session")
		return
	}
	if !CanEditOrg(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "Only the organisation's owners and admins can change seasonal themes")
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Enabled == nil {
		fail(c, http.StatusBadRequest, "Send {\"enabled\": true|false}")
		return
	}
	if err := SetOrgEnabled(h.DB, org, c.GetString("user_id"), *in.Enabled); err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the setting")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"settings": h.switches(c)}})
}

func (h *Handler) setMe(c *gin.Context) {
	user := c.GetString("user_id")
	if user == "" {
		fail(c, http.StatusUnauthorized, "Not signed in")
		return
	}
	var in struct {
		Show *bool `json:"show"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Send {\"show\": true|false|null}")
		return
	}
	if err := SetUserChoice(h.DB, user, in.Show); err != nil {
		fail(c, http.StatusInternalServerError, "Could not save your choice")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"settings": h.switches(c)}})
}

// ---- super admin

// AdminOccasion is an occasion in the admin list.
type AdminOccasion struct {
	Occasion
	ThemeEnabled bool `json:"theme_enabled"`
}

func (h *Handler) list(c *gin.Context) {
	today := Today(time.UTC)
	from, ok := ParseDay(c.Query("from"))
	if !ok {
		from = Day(today.Year(), 1, 1)
	}
	to, ok := ParseDay(c.Query("to"))
	if !ok {
		to = Day(today.Year()+1, 12, 31)
	}
	if to.Sub(from) > 4*366*24*time.Hour {
		fail(c, http.StatusBadRequest, "Choose a range of at most four years")
		return
	}
	country := strings.ToUpper(strings.TrimSpace(c.Query("country")))
	list := []AdminOccasion{}
	for _, o := range h.Svc.Between(from, to, country) {
		list = append(list, AdminOccasion{Occasion: o, ThemeEnabled: o.Theme != "" && (o.Kind == KindCustom || h.Svc.OccasionEnabled(o.Key))})
	}
	// Built-in occasions by key, for the per-occasion switches.
	type keyRow struct {
		Key       string   `json:"key"`
		Name      string   `json:"name"`
		Theme     string   `json:"theme"`
		Kind      string   `json:"kind"`
		Countries []string `json:"countries"`
		Enabled   bool     `json:"enabled"`
	}
	seen := map[string]int{}
	keys := []keyRow{}
	for y := today.Year(); y <= today.Year()+2; y++ {
		for _, o := range Builtin(y) {
			if o.Theme == "" {
				continue
			}
			if i, ok := seen[o.Key]; ok {
				for _, cc := range o.Countries {
					if !has(keys[i].Countries, cc) {
						keys[i].Countries = append(keys[i].Countries, cc)
					}
				}
				if len(o.Countries) == 0 {
					keys[i].Countries = []string{}
				}
				continue
			}
			seen[o.Key] = len(keys)
			keys = append(keys, keyRow{Key: o.Key, Name: o.Name, Theme: o.Theme, Kind: o.Kind, Countries: append([]string{}, o.Countries...), Enabled: h.Svc.OccasionEnabled(o.Key)})
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	var customs []CustomOccasion
	h.DB.Order("date").Find(&customs)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"global_enabled": h.Svc.GlobalEnabled(), "occasions": list, "keys": keys, "themes": Themes, "custom": customsOut(customs),
		"today": today.Format("2006-01-02"), "from": from.Format("2006-01-02"), "to": to.Format("2006-01-02"),
	}})
}

func (h *Handler) themes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"themes": Themes}})
}

// resolve: which theme shows on ?date= for ?country= (platform switches
// honoured, organisation / personal settings not).
func (h *Handler) resolve(c *gin.Context) {
	day, ok := ParseDay(c.Query("date"))
	if !ok {
		day = Today(time.UTC)
	}
	country := strings.ToUpper(strings.TrimSpace(c.Query("country")))
	o := h.Svc.ThemeOn(day, country)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"date": day.Format("2006-01-02"), "country": country, "occasion": o, "global_enabled": h.Svc.GlobalEnabled()}})
}

func (h *Handler) setGlobal(c *gin.Context) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Enabled == nil {
		fail(c, http.StatusBadRequest, "Send {\"enabled\": true|false}")
		return
	}
	if err := h.Svc.SetToggle(GlobalKey, c.GetString("user_id"), *in.Enabled); err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the setting")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"global_enabled": h.Svc.GlobalEnabled()}})
}

func knownKey(key string) bool {
	y := Today(time.UTC).Year()
	for yy := y - 1; yy <= y+2; yy++ {
		for _, o := range Builtin(yy) {
			if o.Key == key {
				return true
			}
		}
	}
	return false
}

func (h *Handler) setToggle(c *gin.Context) {
	key := c.Param("key")
	if key == GlobalKey || !knownKey(key) {
		fail(c, http.StatusNotFound, "Unknown occasion")
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Enabled == nil {
		fail(c, http.StatusBadRequest, "Send {\"enabled\": true|false}")
		return
	}
	if err := h.Svc.SetToggle(key, c.GetString("user_id"), *in.Enabled); err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the setting")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"key": key, "enabled": h.Svc.OccasionEnabled(key)}})
}

// CustomInput creates or updates a custom occasion.
type CustomInput struct {
	Name       string   `json:"name"`
	Date       string   `json:"date"`
	ThemeFrom  string   `json:"theme_from"`
	ThemeTo    string   `json:"theme_to"`
	Countries  []string `json:"countries"`
	Theme      string   `json:"theme"`
	Accent     string   `json:"accent"`
	AccentDark string   `json:"accent_dark"`
	Greeting   string   `json:"greeting"`
	Note       string   `json:"note"`
	Important  bool     `json:"important"`
	Enabled    *bool    `json:"enabled"`
	Yearly     bool     `json:"yearly"`
}

// FieldError is a validation problem with one field.
type FieldError struct {
	Field, Msg string
}

func (e *FieldError) Error() string { return e.Msg }

// ApplyCustom validates in onto c.
func ApplyCustom(c *CustomOccasion, in CustomInput) error {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 120 {
		return &FieldError{"name", "Give the occasion a name (up to 120 characters)"}
	}
	d, ok := ParseDay(in.Date)
	if !ok {
		return &FieldError{"date", "Pick the occasion's date"}
	}
	from, to := d, d
	if in.ThemeFrom != "" {
		if from, ok = ParseDay(in.ThemeFrom); !ok {
			return &FieldError{"theme_from", "The theme start date is not a date"}
		}
	}
	if in.ThemeTo != "" {
		if to, ok = ParseDay(in.ThemeTo); !ok {
			return &FieldError{"theme_to", "The theme end date is not a date"}
		}
	}
	if to.Before(from) {
		return &FieldError{"theme_to", "The theme must end on or after it starts"}
	}
	if days(from, to) > 62 {
		return &FieldError{"theme_to", "A theme can show for at most two months"}
	}
	if d.Before(from) || d.After(to) {
		return &FieldError{"date", "The occasion's date must be inside the theme's dates"}
	}
	if _, ok := ThemeByKey(in.Theme); !ok {
		return &FieldError{"theme", "Pick a theme"}
	}
	var cc []string
	for _, x := range in.Countries {
		x = strings.ToUpper(strings.TrimSpace(x))
		if x == "" {
			continue
		}
		if !iso2.MatchString(x) {
			return &FieldError{"countries", "Countries are two-letter codes such as KE or AU"}
		}
		if !has(cc, x) {
			cc = append(cc, x)
		}
	}
	pal, err := NormalisePalette(&Palette{Accent: in.Accent, AccentDark: in.AccentDark})
	if err != nil {
		return &FieldError{"accent", err.Error()}
	}
	if g := strings.TrimSpace(in.Greeting); len(g) > 140 {
		return &FieldError{"greeting", "Keep the greeting to one short line (140 characters)"}
	}
	c.Name, c.Date, c.ThemeFrom, c.ThemeTo, c.Countries, c.Theme = name, d, from, to, strings.Join(cc, ","), in.Theme
	c.Accent, c.AccentDark = "", ""
	if pal != nil {
		c.Accent, c.AccentDark = pal.Accent, pal.AccentDark
	}
	c.Greeting, c.Note, c.Important, c.Yearly = strings.TrimSpace(in.Greeting), strings.TrimSpace(in.Note), in.Important, in.Yearly
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	} else if c.ID == "" {
		c.Enabled = true
	}
	return nil
}

type customOut struct {
	CustomOccasion
	Key       string   `json:"key"`
	Countries []string `json:"countries"`
	Date      string   `json:"date"`
	ThemeFrom string   `json:"theme_from"`
	ThemeTo   string   `json:"theme_to"`
}

func customOut1(c CustomOccasion) customOut {
	return customOut{CustomOccasion: c, Key: c.Key(), Countries: c.CountryList(), Date: c.Date.Format("2006-01-02"), ThemeFrom: c.ThemeFrom.Format("2006-01-02"), ThemeTo: c.ThemeTo.Format("2006-01-02")}
}

func customsOut(list []CustomOccasion) []customOut {
	out := []customOut{}
	for _, c := range list {
		out = append(out, customOut1(c))
	}
	return out
}

func (h *Handler) listCustom(c *gin.Context) {
	var list []CustomOccasion
	if err := h.DB.Order("date").Find(&list).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load custom occasions")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"custom": customsOut(list)}})
}

func bindErr(c *gin.Context, err error) {
	var fe *FieldError
	if errors.As(err, &fe) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fe.Msg, "message": fe.Msg, "field": fe.Field})
		return
	}
	fail(c, http.StatusBadRequest, err.Error())
}

func (h *Handler) createCustom(c *gin.Context) {
	var in CustomInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	row := CustomOccasion{}
	if err := ApplyCustom(&row, in); err != nil {
		bindErr(c, err)
		return
	}
	row.ID, row.CreatedBy, row.UpdatedBy = uuid.NewString(), c.GetString("user_id"), c.GetString("user_id")
	if err := h.DB.Create(&row).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the occasion")
		return
	}
	h.Svc.Invalidate()
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": customOut1(row), "message": "Occasion added"})
}

func (h *Handler) findCustom(c *gin.Context) (*CustomOccasion, bool) {
	var row CustomOccasion
	if err := h.DB.Where("id = ?", c.Param("id")).Limit(1).Find(&row).Error; err != nil || row.ID == "" {
		fail(c, http.StatusNotFound, "Occasion not found")
		return nil, false
	}
	return &row, true
}

func (h *Handler) updateCustom(c *gin.Context) {
	row, ok := h.findCustom(c)
	if !ok {
		return
	}
	var in CustomInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := ApplyCustom(row, in); err != nil {
		bindErr(c, err)
		return
	}
	row.UpdatedBy = c.GetString("user_id")
	if err := h.DB.Select("*").Omit("created_at", "created_by").Save(row).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the occasion")
		return
	}
	h.Svc.Invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customOut1(*row), "message": "Occasion saved"})
}

func (h *Handler) deleteCustom(c *gin.Context) {
	row, ok := h.findCustom(c)
	if !ok {
		return
	}
	if err := h.DB.Delete(&CustomOccasion{}, "id = ?", row.ID).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not delete the occasion")
		return
	}
	h.Svc.Invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"deleted": row.ID}})
}
