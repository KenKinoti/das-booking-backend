package seccall

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Handler serves the HTTP API.
//
//	Host (signed in, manager level or above; rooms are the creator's own):
//	  POST   /secure-calls                        create (body: title, expires 1h|24h|7d, lobby, link_check)
//	  GET    /secure-calls?status=active|ended    my calls
//	  GET    /secure-calls/:id                    one call with everyone who joined
//	  PATCH  /secure-calls/:id                    {lobby?, locked?, title?}
//	  POST   /secure-calls/:id/join               join as host (bypasses lobby and lock)
//	  POST   /secure-calls/:id/end                end for all
//	  POST   /secure-calls/:id/lock | /unlock
//	  POST   /secure-calls/:id/participants/:pid/admit | /deny | /remove
//	Super admin:
//	  GET|PUT /secure-call-settings               STUN / TURN (static or coturn shared secret)
//	  GET     /secure-call-settings/preview       ICE servers as a participant gets them
//	Public (no account; Authorization: Bearer <participant token> where noted):
//	  GET    /public/calls/:id                    {status} only — no secret needed
//	  POST   /public/calls/:id/preview            {link_check} → title, host, lobby … (pre-join)
//	  POST   /public/calls/:id/join               {name, link_check} → participant token + peers
//	  POST   /public/calls/:id/signal   (token)   {to, ciphertext} or {messages:[…]}
//	  GET    /public/calls/:id/poll?since= (token) long poll ≤ 25 s; also the heartbeat
//	  POST   /public/calls/:id/leave    (token)   (also accepts {"token"} as text/plain for sendBeacon)
//	  GET    /public/calls/:id/ice      (token)   ICE servers (joined participants only)
type Handler struct {
	S *Service
}

// RegisterAuth mounts the host and super admin routes (JWT group).
func (h *Handler) RegisterAuth(g *gin.RouterGroup) {
	g.POST("/secure-calls", h.create)
	g.GET("/secure-calls", h.list)
	g.GET("/secure-calls/:id", h.get)
	g.PATCH("/secure-calls/:id", h.update)
	g.POST("/secure-calls/:id/join", h.hostJoin)
	g.POST("/secure-calls/:id/end", h.end)
	g.POST("/secure-calls/:id/lock", h.lock(true))
	g.POST("/secure-calls/:id/unlock", h.lock(false))
	g.POST("/secure-calls/:id/participants/:pid/admit", h.admit)
	g.POST("/secure-calls/:id/participants/:pid/deny", h.deny)
	g.POST("/secure-calls/:id/participants/:pid/remove", h.remove)

	g.GET("/secure-call-settings", h.getSettings)
	g.PUT("/secure-call-settings", h.putSettings)
	g.GET("/secure-call-settings/preview", h.previewSettings)
}

// RegisterPublic mounts the participant routes (/api/v1/public).
func (h *Handler) RegisterPublic(g *gin.RouterGroup) {
	g.GET("/calls/:id", h.pubStatus)
	g.POST("/calls/:id/preview", h.pubPreview)
	g.POST("/calls/:id/join", h.pubJoin)
	g.POST("/calls/:id/signal", h.pubSignal)
	g.GET("/calls/:id/poll", h.pubPoll)
	g.POST("/calls/:id/leave", h.pubLeave)
	g.GET("/calls/:id/ice", h.pubICE)
}

func ok(c *gin.Context, data any) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func fail(c *gin.Context, err error) {
	c.Header("Cache-Control", "no-store")
	var e *Error
	if errors.As(err, &e) {
		if e.Status == http.StatusTooManyRequests {
			c.Header("Retry-After", "30")
		}
		c.JSON(e.Status, gin.H{"success": false, "error": gin.H{"code": e.Code, "message": e.Message}})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": gin.H{"code": "NOT_FOUND", "message": "Not found"}})
		return
	}
	log.Printf("seccall: %s %s: %v", c.Request.Method, c.FullPath(), err)
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"code": "SERVER_ERROR", "message": "Something went wrong on our side. Try again."}})
}

func bind(c *gin.Context, v any) bool {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 2<<20))
	if err != nil {
		fail(c, invalid("Could not read the request."))
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		fail(c, invalid("The request is not valid JSON."))
		return false
	}
	return true
}

func who(c *gin.Context) (org, user, role string) {
	for _, k := range []string{"org_id", "organization_id"} {
		if v := strings.TrimSpace(c.GetString(k)); v != "" {
			org = v
			break
		}
	}
	return org, strings.TrimSpace(c.GetString("user_id")), strings.TrimSpace(c.GetString("user_role"))
}

// hostName is the signed-in user's display name.
func (h *Handler) hostName(user string) string {
	var u struct {
		FirstName string
		LastName  string
		Email     string
	}
	h.S.DB.Table("users").Select("first_name, last_name, email").Where("id = ?", user).Limit(1).Scan(&u)
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if n == "" {
		n = strings.Split(u.Email, "@")[0]
	}
	return n
}

// ---------------------------------------------------------------- host

func (h *Handler) create(c *gin.Context) {
	org, user, role := who(c)
	var in CreateInput
	if !bind(c, &in) {
		return
	}
	v, err := h.S.Create(org, user, role, h.hostName(user), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"call": v, "app_url": h.S.AppURL})
}

func (h *Handler) list(c *gin.Context) {
	org, user, _ := who(c)
	st := c.Query("status")
	if st != "" && st != "active" && st != "ended" {
		fail(c, invalid("status must be active or ended"))
		return
	}
	out, err := h.S.List(org, user, st)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"calls": out, "app_url": h.S.AppURL, "can_create": CanCreate(c.GetString("user_role"))})
}

func (h *Handler) get(c *gin.Context) {
	org, user, _ := who(c)
	v, err := h.S.Get(org, user, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"call": v, "app_url": h.S.AppURL})
}

func (h *Handler) update(c *gin.Context) {
	org, user, _ := who(c)
	var in UpdateInput
	if !bind(c, &in) {
		return
	}
	if err := h.S.Update(org, user, c.Param("id"), in); err != nil {
		fail(c, err)
		return
	}
	h.get(c)
}

func (h *Handler) hostJoin(c *gin.Context) {
	org, user, _ := who(c)
	var in JoinInput
	if !bind(c, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = h.hostName(user)
	}
	res, err := h.S.JoinHost(org, user, c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}

func (h *Handler) end(c *gin.Context) {
	org, user, _ := who(c)
	if err := h.S.End(org, user, c.Param("id")); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"ended": true})
}

func (h *Handler) lock(on bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		org, user, _ := who(c)
		if err := h.S.SetLocked(org, user, c.Param("id"), on); err != nil {
			fail(c, err)
			return
		}
		ok(c, gin.H{"locked": on})
	}
}

func (h *Handler) admit(c *gin.Context) {
	org, user, _ := who(c)
	if err := h.S.Admit(org, user, c.Param("id"), c.Param("pid")); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"admitted": true})
}

func (h *Handler) deny(c *gin.Context) {
	org, user, _ := who(c)
	if err := h.S.Deny(org, user, c.Param("id"), c.Param("pid")); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"denied": true})
}

func (h *Handler) remove(c *gin.Context) {
	org, user, _ := who(c)
	if err := h.S.Remove(org, user, c.Param("id"), c.Param("pid")); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"removed": true})
}

// ---------------------------------------------------------------- super admin

func superAdmin(c *gin.Context) bool {
	if c.GetString("user_role") == "super_admin" {
		return true
	}
	fail(c, refuse(http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a platform administrator can change call settings."))
	return false
}

func (h *Handler) getSettings(c *gin.Context) {
	if !superAdmin(c) {
		return
	}
	ok(c, h.S.SettingsFor())
}

func (h *Handler) putSettings(c *gin.Context) {
	if !superAdmin(c) {
		return
	}
	var in SettingsInput
	if !bind(c, &in) {
		return
	}
	by := c.GetString("user_email")
	if by == "" {
		by = c.GetString("user_id")
	}
	v, err := h.S.SaveSettings(in, by)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) previewSettings(c *gin.Context) {
	if !superAdmin(c) {
		return
	}
	ok(c, h.S.PreviewICE())
}

// ---------------------------------------------------------------- public

func bearer(c *gin.Context) string {
	a := c.GetHeader("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func (h *Handler) pubStatus(c *gin.Context) {
	if !h.S.lim.allow("join:"+ClientIP(c), h.S.Limits.JoinPerIPPerMin*3, time.Minute, h.S.now()) {
		fail(c, errLimited)
		return
	}
	st, err := h.S.Status(c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"status": st})
}

func (h *Handler) pubPreview(c *gin.Context) {
	var in JoinInput
	if !bind(c, &in) {
		return
	}
	r, err := h.S.Preview(c.Param("id"), in.LinkCheck, ClientIP(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, r)
}

func (h *Handler) pubJoin(c *gin.Context) {
	var in JoinInput
	if !bind(c, &in) {
		return
	}
	res, err := h.S.JoinGuest(c.Param("id"), in, ClientIP(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}

type signalBody struct {
	SignalIn
	Messages []SignalIn `json:"messages"`
	// Fields a plain-text client might send; refused when present.
	SDP       json.RawMessage `json:"sdp"`
	Candidate json.RawMessage `json:"candidate"`
	Data      json.RawMessage `json:"data"`
}

func (h *Handler) pubSignal(c *gin.Context) {
	var in signalBody
	if !bind(c, &in) {
		return
	}
	if len(in.SDP) > 0 || len(in.Candidate) > 0 || len(in.Data) > 0 {
		fail(c, refuse(http.StatusBadRequest, "CIPHERTEXT_REQUIRED", "Messages must be end-to-end encrypted: send {to, ciphertext}, never sdp, candidate or data."))
		return
	}
	msgs := in.Messages
	if len(msgs) == 0 && (in.To != "" || in.Ciphertext != "") {
		msgs = []SignalIn{in.SignalIn}
	}
	res, err := h.S.Signal(c.Param("id"), bearer(c), msgs)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}

func (h *Handler) pubPoll(c *gin.Context) {
	since, _ := strconv.ParseInt(c.Query("since"), 10, 64)
	wait := h.S.Limits.PollWait
	if w, err := strconv.Atoi(c.Query("wait")); err == nil && w >= 0 {
		wait = time.Duration(w) * time.Second
		if w == 0 {
			wait = time.Millisecond
		}
	}
	res, err := h.S.Poll(c.Request.Context(), c.Param("id"), bearer(c), since, wait, ClientIP(c))
	if err != nil {
		if c.Request.Context().Err() != nil {
			return // the browser went away
		}
		fail(c, err)
		return
	}
	ok(c, res)
}

func (h *Handler) pubLeave(c *gin.Context) {
	tok := bearer(c)
	if tok == "" {
		// navigator.sendBeacon cannot set headers: {"token": "..."} as text/plain
		var b struct {
			Token string `json:"token"`
		}
		body, _ := io.ReadAll(io.LimitReader(c.Request.Body, 4096))
		_ = json.Unmarshal(body, &b)
		tok = b.Token
	}
	if err := h.S.Leave(c.Param("id"), tok); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"left": true})
}

func (h *Handler) pubICE(c *gin.Context) {
	res, err := h.S.ICE(c.Param("id"), bearer(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}
