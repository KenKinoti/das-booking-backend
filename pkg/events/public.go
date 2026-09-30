package events

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata" // event timezones on the landing page

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

type publicTicket struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Available   int     `json:"available"` // -1 = unlimited
	MaxPerOrder int     `json:"max_per_order"`
	SoldOut     bool    `json:"sold_out"`
}

type publicEvent struct {
	ID               string         `json:"id"`
	Title            string         `json:"title"`
	Description      string         `json:"description"`
	ShortDescription string         `json:"short_description"`
	Category         string         `json:"category"`
	Type             string         `json:"type"`
	Status           string         `json:"status"`
	DisplayStatus    string         `json:"display_status"`
	StartDate        time.Time      `json:"start_date"`
	EndDate          time.Time      `json:"end_date"`
	Timezone         string         `json:"timezone"`
	VenueName        string         `json:"venue_name"`
	Address          models.Address `json:"address"`
	OnlinePlatform   string         `json:"online_platform"`
	Currency         string         `json:"currency"`
	CoverImageURL    string         `json:"cover_image_url"`
	ContactName      string         `json:"contact_name"`
	ContactEmail     string         `json:"contact_email"`
	IsFree           bool           `json:"is_free"`
	RegistrationOpen bool           `json:"registration_open"`
	Remaining        int            `json:"remaining"` // -1 = unlimited
	Tickets          []publicTicket `json:"tickets"`
}

// loadPublic returns a published, public event (any organisation).
func (h *Handler) loadPublic(db *gorm.DB, id string) (*models.Event, error) {
	var e models.Event
	err := db.Preload("TicketTypes", ticketOrder).
		Where("id = ? AND is_public = ? AND status IN ?", id, true, []string{"published", "completed", "cancelled"}).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, newErr(http.StatusNotFound, "NOT_FOUND", "Event not found")
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (h *Handler) publicView(e *models.Event) (*publicEvent, error) {
	v, err := h.view(e)
	if err != nil {
		return nil, err
	}
	open := v.DisplayStatus == "upcoming" || v.DisplayStatus == "live"
	p := &publicEvent{
		ID: e.ID, Title: e.Title, Description: e.Description, ShortDescription: e.ShortDescription,
		Category: e.Category, Type: e.Type, Status: e.Status, DisplayStatus: v.DisplayStatus,
		StartDate: e.StartDate, EndDate: e.EndDate, Timezone: e.Timezone,
		VenueName: e.VenueName, Address: e.Address, OnlinePlatform: e.OnlinePlatform,
		Currency: e.Currency, CoverImageURL: e.CoverImageURL, ContactName: e.ContactName, ContactEmail: e.ContactEmail,
		IsFree: e.IsFree, Remaining: -1,
	}
	if v.Stats.Capacity > 0 {
		p.Remaining = v.Stats.Remaining
	}
	anyAvail := false
	for _, t := range v.TicketTypes {
		if !t.IsActive {
			continue
		}
		avail := -1
		if t.Quantity > 0 {
			avail = t.Quantity - t.SoldQuantity
			if avail < 0 {
				avail = 0
			}
		}
		if p.Remaining >= 0 && (avail < 0 || avail > p.Remaining) {
			avail = p.Remaining
		}
		pt := publicTicket{ID: t.ID, Name: t.Name, Description: t.Description, Price: t.Price, Available: avail, MaxPerOrder: t.MaxPerOrder, SoldOut: avail == 0}
		if !pt.SoldOut {
			anyAvail = true
		}
		p.Tickets = append(p.Tickets, pt)
	}
	p.RegistrationOpen = open && (anyAvail || e.AllowWaitlist)
	return p, nil
}

// PublicGetEvent GET /public/events/:id
func (h *Handler) PublicGetEvent(c *gin.Context) {
	e, err := h.loadPublic(h.db, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	p, err := h.publicView(e)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, p)
}

// PublicRegister POST /public/events/:id/register
func (h *Handler) PublicRegister(c *gin.Context) {
	var body struct {
		Name                string `json:"name"`
		Email               string `json:"email"`
		Phone               string `json:"phone"`
		Company             string `json:"company"`
		TicketTypeID        string `json:"ticket_type_id"`
		Quantity            int    `json:"quantity"`
		DietaryRequirements string `json:"dietary_requirements"`
		SpecialRequests     string `json:"special_requests"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	if body.Quantity == 0 {
		body.Quantity = 1
	}
	var reg models.EventRegistration
	var ev *models.Event
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := lockEvent(tx, "", c.Param("id")); err != nil {
			return err
		}
		e, err := h.loadPublic(tx, c.Param("id"))
		if err != nil {
			return err
		}
		ev = e
		now := time.Now()
		if e.Status != "published" || e.EndDate.Before(now) {
			return newErr(http.StatusConflict, "CLOSED", "Registration for this event is closed")
		}
		tt := findTicket(e, body.TicketTypeID)
		if tt == nil || !tt.IsActive {
			return newErr(http.StatusBadRequest, "VALIDATION", "Please choose a ticket")
		}
		if body.Quantity < 1 || (tt.MaxPerOrder > 0 && body.Quantity > tt.MaxPerOrder) {
			return newErr(http.StatusBadRequest, "VALIDATION", fmt.Sprintf("You can book between 1 and %d tickets", tt.MaxPerOrder))
		}
		status := "confirmed"
		if e.RequiresApproval {
			status = "pending"
		}
		in := regInput{
			RegistrantName: &body.Name, RegistrantEmail: &body.Email, RegistrantPhone: &body.Phone, Company: &body.Company,
			TicketTypeID: &body.TicketTypeID, Quantity: &body.Quantity, Status: &status,
			DietaryRequirements: &body.DietaryRequirements, SpecialRequests: &body.SpecialRequests,
		}
		reg = models.EventRegistration{OrganizationID: e.OrganizationID, EventID: e.ID, Source: "public", Quantity: 1, Status: status}
		err = h.applyReg(tx, e, &reg, &in, true)
		var ae *apiError
		if errors.As(err, &ae) && ae.code == "SOLD_OUT" && e.AllowWaitlist {
			w := "waitlisted"
			in.Status = &w
			reg.Status = w
			err = h.applyReg(tx, e, &reg, &in, true)
		}
		if err != nil {
			return err
		}
		return tx.Create(&reg).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	notifyRegistration(h.db, ev, &reg)
	ok(c, http.StatusCreated, gin.H{
		"reference": reg.QRCode, "status": reg.Status, "payment_status": reg.PaymentStatus,
		"total_amount": reg.TotalAmount, "currency": ev.Currency, "quantity": reg.Quantity,
		"ticket": reg.TicketTypeName, "event_title": ev.Title,
	})
}

// PublicPage GET /public/events/:id/page — a shareable landing page with a registration form.
func (h *Handler) PublicPage(c *gin.Context) {
	e, err := h.loadPublic(h.db, c.Param("id"))
	if err != nil {
		c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Event not found</title><body style="font-family:system-ui;padding:48px;text-align:center"><h1>Event not found</h1><p>This event is not available or is no longer public.</p></body>`))
		return
	}
	p, err := h.publicView(e)
	if err != nil {
		failErr(c, err)
		return
	}
	loc, lerr := time.LoadLocation(e.Timezone)
	if lerr != nil {
		loc = time.UTC
	}
	start, end := e.StartDate.In(loc), e.EndDate.In(loc)
	when := start.Format("Monday 2 January 2006, 3:04 PM")
	if start.Format("2006-01-02") == end.Format("2006-01-02") {
		when += " – " + end.Format("3:04 PM")
	} else {
		when += " – " + end.Format("Mon 2 Jan 2006, 3:04 PM")
	}
	when += " (" + loc.String() + ")"
	var where []string
	if e.Type != "online" {
		addr := strings.Join(nonEmpty(e.Address.Street, e.Address.Suburb, e.Address.State, e.Address.Postcode), ", ")
		where = append(where, strings.Join(nonEmpty(e.VenueName, addr), " · "))
	}
	if e.Type != "in_person" {
		plat := e.OnlinePlatform
		if plat == "" {
			plat = "Online"
		}
		where = append(where, plat+" (link sent after registering)")
	}
	data := gin.H{
		"E": p, "When": when, "Month": start.Format("Jan"), "Day": start.Format("2"), "Where": strings.Join(nonEmpty(where...), " + "),
		"Money": func(v float64) string {
			if v == 0 {
				return "Free"
			}
			return fmt.Sprintf("%s %.2f", e.Currency, v)
		},
	}
	if OrgLogoURL != nil {
		if u := OrgLogoURL(c.Request, e.OrganizationID); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			data["Logo"] = u
		}
	}
	nonce := newNonce()
	data["Nonce"] = nonce
	// The global security middleware sends "default-src 'self'", which blocks
	// this page's inline styles and script; replace it with a tight policy.
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; img-src https: http: data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	if err := landingTpl.Execute(c.Writer, data); err != nil {
		_, _ = c.Writer.WriteString("template error")
	}
}

// OrgLogoURL resolves the organiser's company logo (set by the branding
// module); "" when there is none.
var OrgLogoURL func(r *http.Request, org string) string

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

var landingTpl = template.Must(template.New("landing").Funcs(template.FuncMap{}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.E.Title}}</title>
<meta name="description" content="{{.E.ShortDescription}}">
<style>
:root{--bg:#f6f7fb;--surface:#fff;--border:#e6e8ef;--text:#111322;--text2:#475067;--text3:#7b839a;--accent:#5b4cf0;--accent-soft:#efedff;--ok:#12a150;--ok-soft:#e6f7ee;--err:#e5484d;--err-soft:#fdecec;color-scheme:light}
@media (prefers-color-scheme:dark){:root{--bg:#0d0f17;--surface:#161925;--border:#262a3a;--text:#eceef6;--text2:#b3b8cc;--text3:#8a90a6;--accent:#8b7fff;--accent-soft:#232044;--ok:#3ccf7e;--ok-soft:#12291d;--err:#ff6b6f;--err-soft:#2d1618;color-scheme:dark}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.55 Inter,ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
.wrap{max-width:1040px;margin:0 auto;padding:24px 16px 64px}
.cover{height:260px;border-radius:18px;background:var(--accent-soft) center/cover no-repeat;margin-bottom:24px;display:grid;place-items:center;color:var(--accent);font-size:56px}
.datebox{background:var(--surface);border-radius:14px;padding:10px 22px 12px;text-align:center;line-height:1.1;box-shadow:0 4px 16px rgba(0,0,0,.08)}.datebox span{display:block;font-size:13px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:var(--err)}.datebox strong{display:block;font-size:40px;color:var(--text)}
.grid{display:grid;grid-template-columns:1fr 360px;gap:24px;align-items:start}
@media (max-width:820px){.grid{grid-template-columns:1fr}.cover{height:180px}}
.card{background:var(--surface);border:1px solid var(--border);border-radius:16px;padding:22px}
.eyebrow{font-size:12px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:var(--accent)}
h1{font-size:30px;line-height:1.2;margin:6px 0 12px;letter-spacing:-.02em}
.meta{display:flex;flex-direction:column;gap:8px;color:var(--text2);margin-bottom:18px}
.desc{white-space:pre-line;color:var(--text2)}
.badge{display:inline-block;padding:3px 10px;border-radius:99px;font-size:12px;font-weight:600;background:var(--err-soft);color:var(--err)}
label{display:block;font-size:13px;font-weight:600;margin:12px 0 6px;color:var(--text2)}
input,select,textarea{width:100%;padding:10px 12px;border:1px solid var(--border);border-radius:10px;background:var(--surface);color:var(--text);font:inherit}
input:focus,select:focus{outline:2px solid var(--accent);outline-offset:0}
button{margin-top:18px;width:100%;padding:12px;border:0;border-radius:10px;background:var(--accent);color:#fff;font:inherit;font-weight:650;cursor:pointer}
button:disabled{opacity:.6;cursor:wait}
.ticket{display:flex;justify-content:space-between;gap:10px;padding:10px 0;border-bottom:1px solid var(--border)}.ticket:last-child{border-bottom:0}
.muted{color:var(--text3);font-size:13px}
.org{display:flex;align-items:center;margin:0 0 16px}.org img{display:block;max-height:48px;max-width:200px;width:auto;height:auto;object-fit:contain}
@media (prefers-color-scheme:dark){.org img{background:#f4f5f9;border-radius:8px;padding:4px 8px}}
.msg{margin-top:14px;padding:12px 14px;border-radius:10px;display:none}.msg.ok{display:block;background:var(--ok-soft);color:var(--ok)}.msg.err{display:block;background:var(--err-soft);color:var(--err)}
</style></head><body><div class="wrap">
{{if .Logo}}<div class="org"><img src="{{.Logo}}" alt="Organiser logo"></div>{{end}}
<div class="cover"{{if .E.CoverImageURL}} style="background-image:url('{{.E.CoverImageURL}}')"{{end}}>{{if not .E.CoverImageURL}}<div class="datebox"><span>{{.Month}}</span><strong>{{.Day}}</strong></div>{{end}}</div>
<div class="grid"><div class="card">
<div class="eyebrow">{{if .E.Category}}{{.E.Category}}{{else}}Event{{end}}</div>
<h1>{{.E.Title}}</h1>
{{if eq .E.DisplayStatus "cancelled"}}<p><span class="badge">This event has been cancelled</span></p>{{end}}
<div class="meta"><div>&#128339; {{.When}}</div>{{if .Where}}<div>&#128205; {{.Where}}</div>{{end}}
{{if .E.ContactEmail}}<div>&#9993; {{if .E.ContactName}}{{.E.ContactName}} · {{end}}<a href="mailto:{{.E.ContactEmail}}">{{.E.ContactEmail}}</a></div>{{end}}</div>
{{if .E.ShortDescription}}<p><strong>{{.E.ShortDescription}}</strong></p>{{end}}
<div class="desc">{{.E.Description}}</div>
</div>
<div class="card"><h2 style="margin:0 0 6px;font-size:18px">Tickets</h2>
{{range .E.Tickets}}<div class="ticket"><div><strong>{{.Name}}</strong>{{if .Description}}<div class="muted">{{.Description}}</div>{{end}}</div><div style="text-align:right;white-space:nowrap"><strong>{{call $.Money .Price}}</strong>{{if .SoldOut}}<div class="muted">Sold out</div>{{else if ge .Available 0}}<div class="muted">{{.Available}} left</div>{{end}}</div></div>{{end}}
{{if .E.RegistrationOpen}}
<form id="f" method="post" novalidate>
<label for="t">Ticket</label><select id="t" name="ticket_type_id" required>{{range .E.Tickets}}{{if not .SoldOut}}<option value="{{.ID}}">{{.Name}} — {{call $.Money .Price}}</option>{{end}}{{end}}</select>
<label for="q">Quantity</label><input id="q" name="quantity" type="number" min="1" max="20" value="1" required>
<label for="n">Full name</label><input id="n" name="name" autocomplete="name" required>
<label for="e">Email</label><input id="e" name="email" type="email" autocomplete="email" required>
<label for="p">Phone (optional)</label><input id="p" name="phone" autocomplete="tel">
<button id="b" type="submit">Register</button>
<div id="m" class="msg" role="status"></div>
</form>
<script nonce="{{.Nonce}}">
document.getElementById('f').addEventListener('submit',async function(ev){ev.preventDefault();var b=document.getElementById('b'),m=document.getElementById('m');m.className='msg';
var d={ticket_type_id:this.ticket_type_id.value,quantity:parseInt(this.quantity.value||'1',10),name:this.name.value.trim(),email:this.email.value.trim(),phone:this.phone.value.trim()};
if(!d.name||!d.email){m.className='msg err';m.textContent='Please enter your name and email.';return}
b.disabled=true;try{var r=await fetch('register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(d)});var j=await r.json();
if(!r.ok||!j.success){throw new Error((j.error&&j.error.message)||'Registration failed')}
var s=j.data.status==='waitlisted'?'You are on the waitlist.':j.data.status==='pending'?'Your registration is awaiting approval.':'You are registered!';
m.className='msg ok';m.textContent=s+' Your reference is '+j.data.reference+'.';this.reset()}catch(e){m.className='msg err';m.textContent=e.message}finally{b.disabled=false}});
</script>
{{else}}<p class="muted" style="margin-top:14px">Registration is closed for this event.</p>{{end}}
</div></div></div></body></html>`))
