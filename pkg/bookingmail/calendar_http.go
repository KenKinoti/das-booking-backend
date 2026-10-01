package bookingmail

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
)

// ---------------------------------------------------------------------------
// Calendar endpoints.
//
//	GET /bookings/:id/calendar.ics          signed-in download (role / own-scope rules apply to /bookings/…)
//	GET /bookings/:id/calendar-links        Google, Outlook and public .ics links for one booking
//	GET /booking-emails/feed                my feed (+ the organisation feed for admins)
//	POST /booking-emails/feed/regenerate    new secret link, the old one stops working
//	DELETE /booking-emails/feed             turn a feed off
//	GET /public/bookings/:id/calendar.ics?t=…   signed, login-free add-to-calendar file
//	GET /public/calendar/<token>.ics            subscription feed (also reachable as webcal://)
// ---------------------------------------------------------------------------

// RegisterCalendarRoutes mounts the calendar routes.
func (h *Handler) RegisterCalendarRoutes(auth, public *gin.RouterGroup) {
	if h.limit == nil {
		h.limit = &rateLimiter{buckets: map[string]*rateBucket{}}
	}
	auth.GET("/bookings/:id/calendar.ics", h.bookingICS)
	auth.GET("/bookings/:id/calendar-links", h.bookingLinks)
	g := auth.Group("/booking-emails")
	g.GET("/feed", h.getFeeds)
	g.POST("/feed/regenerate", h.regenerateFeed)
	g.DELETE("/feed", h.revokeFeed)
	public.GET("/bookings/:id/calendar.ics", h.publicBookingICS)
	public.HEAD("/bookings/:id/calendar.ics", h.publicBookingICS)
	public.GET("/calendar/:file", h.publicFeed)
	public.HEAD("/calendar/:file", h.publicFeed)
}

// ---------------------------------------------------------------- rate limit

// Public calendar endpoints: requests per minute per address / per feed. A
// request that fails (unknown link, bad signature) costs failCost, which
// slows guessing down to a crawl.
const (
	ratePerIP    = 120
	ratePerFeed  = 30
	rateFailCost = 10
)

type rateBucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

// take consumes n units of key's per-minute budget; false + wait when empty.
func (l *rateLimiter) take(key string, n, perMin int, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &rateBucket{tokens: float64(perMin), last: now}
		l.buckets[key] = b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens += d.Minutes() * float64(perMin)
	}
	if b.tokens > float64(perMin) {
		b.tokens = float64(perMin)
	}
	b.last = now
	if len(l.buckets) > 20000 {
		for k, v := range l.buckets {
			if now.Sub(v.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	if b.tokens < 1 {
		return false, time.Duration((1-b.tokens)/float64(perMin)*float64(time.Minute)) + time.Second
	}
	b.tokens -= float64(n) // may go negative: a failed lookup has to be paid off
	return true, 0
}

// allow applies the per-address limit (429 + Retry-After when exceeded).
func (h *Handler) allow(c *gin.Context, key string, cost, perMin int) bool {
	if h.limit == nil {
		return true
	}
	ok, wait := h.limit.take(key, cost, perMin, h.N.now())
	if !ok {
		c.Header("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusTooManyRequests, "Too many requests — try again in a moment.\n")
		c.Abort()
	}
	return ok
}

// penalise charges a failed public lookup to the caller's address.
func (h *Handler) penalise(c *gin.Context) {
	if h.limit != nil {
		h.limit.take("ip:"+c.ClientIP(), rateFailCost, ratePerIP, h.N.now())
	}
}

// ---------------------------------------------------------------- one booking

// apiBase is the public origin of this API: PUBLIC_API_URL, else taken from
// the request.
func (h *Handler) apiBase(c *gin.Context) string {
	if b := strings.TrimRight(strings.TrimSpace(h.N.APIURL), "/"); b != "" {
		return b
	}
	return branding.BaseURL(c.Request)
}

// loadBooking loads a booking with what its calendar entry shows — also a
// deleted one, which yields the cancellation file.
func (h *Handler) loadBooking(id, org string) *models.Booking {
	var b models.Booking
	q := h.N.DB.Unscoped().Preload("Customer").Preload("Vehicle").Preload("Services").Where("id = ?", id)
	if org != "" {
		q = q.Where("organization_id = ?", org)
	}
	if strings.TrimSpace(id) == "" || q.First(&b).Error != nil {
		return nil
	}
	return &b
}

func quoteFilename(name string) string {
	return `"` + strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "").Replace(name) + `"`
}

// writeICS sends an add-to-calendar file. inline hands it straight to the
// calendar app in mobile browsers; attachment saves it.
func writeICS(c *gin.Context, ics, filename, method, disposition string) {
	c.Header("Content-Disposition", disposition+"; filename="+quoteFilename(filename))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	c.Data(http.StatusOK, "text/calendar; charset=utf-8; method="+method, []byte(ics))
}

// bookingICS is the signed-in download. The business-type guard has already
// applied the role and own-scope rules to /bookings/:id/….
func (h *Handler) bookingICS(c *gin.Context) {
	org := orgOf(c)
	b := h.loadBooking(c.Param("id"), org)
	if org == "" || b == nil {
		bad(c, http.StatusNotFound, "Booking not found")
		return
	}
	ics, name, cancelled := h.N.BookingICS(b)
	disp := "attachment"
	if c.Query("inline") == "1" {
		disp = "inline"
	}
	writeICS(c, ics, name, AddMethod(cancelled), disp)
}

// bookingLinks returns the add-to-calendar links of a booking.
func (h *Handler) bookingLinks(c *gin.Context) {
	org := orgOf(c)
	b := h.loadBooking(c.Param("id"), org)
	if org == "" || b == nil {
		bad(c, http.StatusNotFound, "Booking not found")
		return
	}
	oc := h.N.orgCtx(org)
	okJSON(c, h.N.LinksFor(h.apiBase(c), b, h.N.Event(b, h.N.Sequence(b.ID), oc), oc.Loc))
}

// publicBookingICS serves the add-to-calendar file to whoever holds the
// signed link (no login): the PUBLISH file, or the CANCEL file once the
// booking is cancelled or deleted. An unknown booking and a wrong signature
// are the same 404.
func (h *Handler) publicBookingICS(c *gin.Context) {
	if !h.allow(c, "ip:"+c.ClientIP(), 1, ratePerIP) {
		return
	}
	id := c.Param("id")
	b := h.loadBooking(id, "")
	org := ""
	if b != nil {
		org = b.OrganizationID
	}
	// Verify even when the booking is unknown so both cases take the same path.
	if !VerifyLinkToken(h.N.Secret, org, id, c.Query("t")) || b == nil {
		h.penalise(c)
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusNotFound, "This calendar link is not valid.\n")
		return
	}
	ics, name, cancelled := h.N.BookingICS(b)
	writeICS(c, ics, name, AddMethod(cancelled), "inline")
}

// ---------------------------------------------------------------- feeds

// etagMatch implements If-None-Match (weak comparison, list or "*").
func etagMatch(header, etag string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimPrefix(strings.TrimSpace(p), "W/")
		if p == "*" || (p != "" && p == etag) {
			return true
		}
	}
	return false
}

// publicFeed serves a subscription feed with ETag / Last-Modified so calendar
// apps that poll every few minutes mostly get a 304.
func (h *Handler) publicFeed(c *gin.Context) {
	if !h.allow(c, "ip:"+c.ClientIP(), 1, ratePerIP) {
		return
	}
	token := strings.TrimSuffix(c.Param("file"), ".ics")
	ft, err := h.N.FeedByToken(token)
	if err != nil {
		c.Header("Cache-Control", "no-store")
		if errors.Is(err, ErrFeedRevoked) {
			// A calendar app still polling a replaced link is not guessing.
			c.String(http.StatusGone, "This calendar link was replaced or turned off. Get the new link in the app: My profile → My bookings calendar.\n")
			return
		}
		h.penalise(c)
		c.String(http.StatusNotFound, "This calendar link is not valid.\n")
		return
	}
	if !h.allow(c, "feed:"+ft.ID, 1, ratePerFeed) {
		return
	}
	body := h.N.FeedICS(ft)
	etag := FeedETag(body)
	changed := h.N.MarkServed(ft, etag)
	c.Header("ETag", etag)
	c.Header("Last-Modified", changed.UTC().Format(http.TimeFormat))
	// private: the address is a secret; no-cache: keep a copy but revalidate.
	c.Header("Cache-Control", "private, no-cache")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	if inm := c.GetHeader("If-None-Match"); inm != "" {
		if etagMatch(inm, etag) {
			c.Status(http.StatusNotModified)
			return
		}
	} else if ims := c.GetHeader("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !changed.After(t) {
			c.Status(http.StatusNotModified)
			return
		}
	}
	c.Header("Content-Disposition", "inline; filename="+quoteFilename("dasyin-bookings.ics"))
	c.Data(http.StatusOK, "text/calendar; charset=utf-8", []byte(body))
}

// feedView is a feed as the app shows it.
func (h *Handler) feedView(c *gin.Context, ft *FeedToken) gin.H {
	if ft == nil {
		return gin.H{"enabled": false}
	}
	name := h.N.FeedName(ft)
	links := SubscribeLinks(h.apiBase(c)+FeedPath(ft.Token), name)
	return gin.H{"enabled": true, "scope": ft.Scope, "name": name, "url": links.URL, "webcal_url": links.Webcal, "google_url": links.Google,
		"outlook_url": links.Outlook, "outlook_live_url": links.OutlookLive, "created_at": ft.CreatedAt, "last_used_at": ft.LastUsedAt}
}

// getFeeds returns the caller's feed (created on first use) and, for admins
// and managers, the organisation feed.
func (h *Handler) getFeeds(c *gin.Context) {
	org, uid := orgOf(c), c.GetString("user_id")
	if org == "" || uid == "" {
		bad(c, http.StatusForbidden, "No organisation")
		return
	}
	mine := h.N.ActiveFeed(org, FeedUser, uid)
	if mine == nil && !h.N.HadFeed(org, FeedUser, uid) {
		ft, err := h.N.NewFeed(org, FeedUser, uid, uid)
		if err != nil {
			bad(c, http.StatusInternalServerError, "Could not create your calendar link")
			return
		}
		mine = ft
	}
	admin := isAdmin(c.GetString("user_role"))
	out := gin.H{"user": h.feedView(c, mine), "can_manage_org": admin, "refresh_minutes": int(FeedRefresh / time.Minute),
		"past_days": FeedPastDays, "ahead_months": FeedAheadMonths}
	if admin {
		out["org"] = h.feedView(c, h.N.ActiveFeed(org, FeedOrg, ""))
	}
	okJSON(c, out)
}

func (h *Handler) feedScope(c *gin.Context, scope string) (string, bool) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = FeedUser
	}
	if scope != FeedUser && scope != FeedOrg {
		bad(c, http.StatusBadRequest, "Choose your own calendar or the organisation calendar")
		return "", false
	}
	if orgOf(c) == "" || c.GetString("user_id") == "" {
		bad(c, http.StatusForbidden, "No organisation")
		return "", false
	}
	if scope == FeedOrg && !isAdmin(c.GetString("user_role")) {
		bad(c, http.StatusForbidden, "Only admins and managers can manage the organisation calendar")
		return "", false
	}
	return scope, true
}

// regenerateFeed issues a new secret link (also turns a feed on).
func (h *Handler) regenerateFeed(c *gin.Context) {
	var in struct {
		Scope string `json:"scope"`
	}
	_ = c.ShouldBindJSON(&in)
	scope, ok := h.feedScope(c, in.Scope)
	if !ok {
		return
	}
	uid := c.GetString("user_id")
	ft, err := h.N.NewFeed(orgOf(c), scope, uid, uid)
	if err != nil {
		bad(c, http.StatusInternalServerError, "Could not create the calendar link")
		return
	}
	okJSON(c, gin.H{"feed": h.feedView(c, ft)})
}

// revokeFeed turns a feed off.
func (h *Handler) revokeFeed(c *gin.Context) {
	scope, ok := h.feedScope(c, c.Query("scope"))
	if !ok {
		return
	}
	h.N.RevokeFeed(orgOf(c), scope, c.GetString("user_id"))
	okJSON(c, gin.H{"feed": h.feedView(c, nil)})
}
