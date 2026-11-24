package outbox

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

func gormExpr(expr string) any { return gorm.Expr(expr) }

// Person is a user reference.
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func person(id, name string) *Person {
	if id == "" && name == "" {
		return nil
	}
	return &Person{ID: id, Name: name}
}

// Record is the linked record.
type Record struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Number string `json:"number"`
	Label  string `json:"label"`
	Link   string `json:"link"`
}

// Customer is the linked customer.
type Customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Row is one item in a list.
type Row struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	KindLabel        string     `json:"kind_label"`
	Module           string     `json:"module"`
	Channel          string     `json:"channel"`
	Status           string     `json:"status"`
	StatusLabel      string     `json:"status_label"`
	To               string     `json:"to"`
	ToName           string     `json:"to_name"`
	CC               []string   `json:"cc"`
	BCC              []string   `json:"bcc"`
	Subject          string     `json:"subject"`
	Snippet          string     `json:"snippet"`
	Record           *Record    `json:"record"`
	Customer         *Customer  `json:"customer"`
	Amount           *float64   `json:"amount"`
	Currency         string     `json:"currency"`
	Trigger          string     `json:"trigger"`
	TriggerKind      string     `json:"trigger_kind"`
	CreatedBy        *Person    `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	SendAt           *time.Time `json:"send_at"`
	Stale            bool       `json:"stale"`
	StaleSince       *time.Time `json:"stale_since"`
	TimeSensitive    bool       `json:"time_sensitive"`
	Deadline         *time.Time `json:"deadline"`
	Flag             string     `json:"flag"`
	Blocks           string     `json:"blocks"`
	Edited           bool       `json:"edited"`
	ApprovedBy       *Person    `json:"approved_by"`
	ApprovedAt       *time.Time `json:"approved_at"`
	ApproveMode      string     `json:"approve_mode"`
	DecidedBy        *Person    `json:"decided_by"`
	DecidedAt        *time.Time `json:"decided_at"`
	DecisionNote     string     `json:"decision_note"`
	SentAt           *time.Time `json:"sent_at"`
	Result           string     `json:"result"`
	Error            string     `json:"error"`
	SupersededReason string     `json:"superseded_reason"`
	Attempts         int        `json:"attempts"`
	Link             string     `json:"link"`
}

// staleSince is when a dated pending item became stale (nil = not stale).
func (s *Service) staleSince(it *Item, st Settings) *time.Time {
	if !it.Dated || (it.Status != StatusPending && it.Status != StatusFailed) {
		return nil
	}
	at := it.CreatedAt.Add(time.Duration(st.StaleDays) * 24 * time.Hour)
	if s.now().Before(at) {
		return nil
	}
	return &at
}

func (s *Service) statusLabel(it *Item) string {
	switch it.Status {
	case StatusPending:
		return "Waiting for approval"
	case StatusApproved:
		if it.SendAt != nil {
			return "Approved — goes " + s.localTime(it.OrganizationID, *it.SendAt)
		}
		return "Approved"
	case StatusSending:
		return "Sending"
	case StatusSent:
		return "Sent"
	case StatusRejected:
		return "Rejected"
	case StatusSuperseded:
		return "Not needed any more"
	case StatusFailed:
		return "Failed"
	}
	return it.Status
}

func snippet(it *Item) string {
	t := it.Message
	if it.Channel == ChanWhatsApp {
		t = it.WAText
		if t == "" || it.WAMode == "template" {
			t = "Template " + it.WATemplate + ": " + strings.Join(it.WAParams, " · ")
		}
	}
	t = strings.Join(strings.Fields(t), " ")
	if utf8.RuneCountInString(t) > 140 {
		r := []rune(t)
		t = strings.TrimSpace(string(r[:139])) + "…"
	}
	return t
}

// RowOf builds the list row.
func (s *Service) RowOf(it *Item, st Settings) Row {
	r := Row{ID: it.ID, Kind: it.Kind, KindLabel: KindLabel(it.Kind), Module: it.Module, Channel: it.Channel, Status: it.Status, StatusLabel: s.statusLabel(it),
		To: it.To, ToName: it.ToName, CC: nn(it.CC), BCC: nn(it.BCC), Subject: it.Subject, Snippet: snippet(it), Amount: it.Amount, Currency: it.Currency,
		Trigger: it.Trigger, TriggerKind: it.TriggerKind, CreatedBy: person(it.CreatedBy, it.CreatedByName), CreatedAt: it.CreatedAt, SendAt: it.SendAt,
		Deadline: it.Deadline, Flag: it.Flag, Blocks: it.Blocks, Edited: it.Edited, ApprovedBy: person(it.ApprovedBy, it.ApprovedByName), ApprovedAt: it.ApprovedAt,
		ApproveMode: it.ApproveMode, DecidedBy: person(it.DecidedBy, it.DecidedByName), DecidedAt: it.DecidedAt, DecisionNote: it.DecisionNote,
		SentAt: it.SentAt, Result: it.Result, Error: it.Error, SupersededReason: it.SupersededReason, Attempts: it.Attempts, Link: "/outbox?open=" + it.ID}
	if it.RecordType != "" || it.RecordID != "" {
		r.Record = &Record{Type: it.RecordType, ID: it.RecordID, Number: it.RecordNumber, Label: it.RecordLabel, Link: it.RecordLink}
	}
	if it.CustomerID != "" || it.CustomerName != "" {
		r.Customer = &Customer{ID: it.CustomerID, Name: it.CustomerName}
	}
	if ss := s.staleSince(it, st); ss != nil {
		r.Stale, r.StaleSince = true, ss
	}
	open := it.Status == StatusPending || it.Status == StatusFailed || it.Status == StatusApproved
	r.TimeSensitive = open && (it.Blocks != "" || (it.Deadline != nil && it.Deadline.Sub(s.now()) <= DeadlineSoonWindow))
	return r
}

func nn(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// WhatsAppView is the WhatsApp part of the detail.
type WhatsAppView struct {
	Mode       string   `json:"mode"`
	Text       string   `json:"text"`
	Template   string   `json:"template"`
	Language   string   `json:"language"`
	Params     []string `json:"params"`
	ParamCount int      `json:"param_count"`
	WindowOpen bool     `json:"window_open"`
}

// Detail is one item with its preview and history.
type Detail struct {
	Row
	Message    string          `json:"message"`
	SMSText    string          `json:"sms_text"`
	WhatsApp   *WhatsAppView   `json:"whatsapp"`
	Editable   map[string]bool `json:"editable"`
	Preview    Preview         `json:"preview"`
	Context    []ContextLine   `json:"context"`
	History    []EventView     `json:"history"`
	Actions    map[string]bool `json:"actions"`
	CanApprove bool            `json:"can_approve"`
}

// EventView is one history line.
type EventView struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	By      *Person   `json:"by"`
	Text    string    `json:"text"`
	Changes Changes   `json:"changes"`
	Note    string    `json:"note"`
}

// DetailOf builds the detail (with the module's preview).
func (s *Service) DetailOf(ctx context.Context, it *Item, canApprove bool) Detail {
	st := s.SettingsFor(it.OrganizationID)
	d := Detail{Row: s.RowOf(it, st), Message: it.Message, Context: it.Context, CanApprove: canApprove}
	if d.Context == nil {
		d.Context = []ContextLine{}
	}
	d.Context = append(d.Context, ContextLine{Label: "Why", Value: it.Trigger}, ContextLine{Label: "Waiting since", Value: s.localTime(it.OrganizationID, it.CreatedAt)})
	if it.SendAt != nil {
		d.Context = append(d.Context, ContextLine{Label: "Meant to go", Value: s.localTime(it.OrganizationID, *it.SendAt)})
	}
	if it.Channel == ChanSMS {
		d.SMSText = it.Message
	}
	d.Editable = map[string]bool{}
	for _, f := range []string{FieldTo, FieldCC, FieldBCC, FieldSubject, FieldMessage, FieldWAText, FieldWAParams} {
		d.Editable[f] = can(it, f)
	}
	if r := releaserOf(it.Module); r != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					d.Preview = Preview{Error: fmt.Sprintf("The preview couldn't be made (%v)", rec)}
				}
			}()
			d.Preview = r.Preview(ctx, it)
		}()
	} else {
		d.Preview = Preview{Subject: it.Subject, To: it.To, Text: it.Message, Error: "The preview isn't available on this server."}
	}
	if d.Preview.CC == nil {
		d.Preview.CC = []string{}
	}
	if d.Preview.BCC == nil {
		d.Preview.BCC = []string{}
	}
	if d.Preview.Attachments == nil {
		d.Preview.Attachments = []Attachment{}
	}
	if it.Channel == ChanSMS && d.Preview.SMS == nil {
		d.Preview.SMS = smsInfo(it.Message)
	}
	if it.Channel == ChanWhatsApp {
		w := &WhatsAppView{Mode: it.WAMode, Text: it.WAText, Template: it.WATemplate, Language: it.WALanguage, Params: nn(it.WAParams), ParamCount: it.WAParamCount}
		if d.Preview.WindowOpen != nil {
			w.WindowOpen = *d.Preview.WindowOpen
		}
		d.WhatsApp = w
	}
	var evs []Event
	s.db().Where("item_id = ?", it.ID).Order("at, id").Find(&evs)
	d.History = []EventView{}
	for _, e := range evs {
		d.History = append(d.History, EventView{ID: e.ID, At: e.At, Action: e.Action, By: person(e.UserID, e.UserName), Text: e.Text, Changes: e.Changes, Note: e.Note})
	}
	open := it.Status == StatusPending || it.Status == StatusFailed || it.Status == StatusApproved
	d.Actions = map[string]bool{
		"edit":              canApprove && open && len(it.Editable) > 0,
		"approve_now":       canApprove && (it.Status == StatusPending || it.Status == StatusApproved),
		"approve_scheduled": canApprove && (it.Status == StatusPending || it.Status == StatusFailed) && it.SendAt != nil && it.SendAt.After(s.now()),
		"reject":            canApprove && open,
		"retry":             canApprove && it.Status == StatusFailed,
	}
	return d
}

// smsInfo counts characters and segments (GSM-7: 160 / 153, otherwise 70 / 67).
func smsInfo(t string) *SMSInfo {
	n := utf8.RuneCountInString(t)
	gsm := true
	for _, r := range t {
		if r > 127 && !strings.ContainsRune("£¥èéùìòÇØøÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ¤¡ÄÖÑÜ§¿äöñüà€", r) {
			gsm = false
			break
		}
	}
	one, many := 160, 153
	if !gsm {
		one, many = 70, 67
	}
	seg := 1
	if n > one {
		seg = (n + many - 1) / many
	}
	return &SMSInfo{Chars: n, Segments: seg}
}

// ---------------------------------------------------------------- lists

// Query filters a list.
type Query struct {
	Tab, Status, Kind, Channel, CustomerID, CreatedBy, TriggerKind, Flag, Q, Sort string
	Page, PerPage                                                                 int
}

// Counts are the badge numbers.
type Counts struct {
	Enabled       bool       `json:"enabled"`
	CanApprove    bool       `json:"can_approve"`
	CanSendNow    bool       `json:"can_send_now"` // can_approve and "admins can send at once"
	Waiting       int64      `json:"waiting"`
	Pending       int64      `json:"pending"`
	Failed        int64      `json:"failed"`
	Stale         int64      `json:"stale"`
	TimeSensitive int64      `json:"time_sensitive"`
	Approved      int64      `json:"approved"`
	Sent7d        int64      `json:"sent_7d"`
	Rejected30d   int64      `json:"rejected_30d"`
	Superseded30d int64      `json:"superseded_30d"`
	SentNow7d     int64      `json:"sent_now_7d"`
	OldestAt      *time.Time `json:"oldest_at"`
	NextSendAt    *time.Time `json:"next_send_at"`
}

// scope limits non-approvers to their own items.
func (s *Service) scope(a Actor, canApprove bool) *gorm.DB {
	q := s.db().Model(&Item{}).Where("organization_id = ?", a.Org)
	if !canApprove {
		q = q.Where("created_by = ?", a.User)
	}
	return q
}

// CountsFor are the badge numbers for a person.
func (s *Service) CountsFor(a Actor) Counts {
	canA := s.CanApprove(a.Org, a.User, a.Role)
	st := s.SettingsFor(a.Org)
	c := Counts{Enabled: st.Enabled, CanApprove: canA, CanSendNow: canA && st.AdminsCanSendNow}
	now := s.now()
	type row struct {
		Status string
		N      int64
	}
	var rows []row
	s.scope(a, canA).Select("status, COUNT(*) AS n").Where("status IN ?", []string{StatusPending, StatusFailed, StatusApproved, StatusSending}).Group("status").Scan(&rows)
	for _, r := range rows {
		switch r.Status {
		case StatusPending:
			c.Pending = r.N
		case StatusFailed:
			c.Failed = r.N
		case StatusApproved, StatusSending:
			c.Approved += r.N
		}
	}
	c.Waiting = c.Pending + c.Failed
	cut := now.Add(-time.Duration(st.StaleDays) * 24 * time.Hour)
	s.scope(a, canA).Where("status IN ? AND dated AND created_at <= ?", []string{StatusPending, StatusFailed}, cut).Count(&c.Stale)
	s.scope(a, canA).Where("status IN ? AND (blocks <> '' OR (deadline IS NOT NULL AND deadline <= ?))", []string{StatusPending, StatusFailed, StatusApproved}, now.Add(DeadlineSoonWindow)).Count(&c.TimeSensitive)
	s.scope(a, canA).Where("status = ? AND sent_at >= ?", StatusSent, now.AddDate(0, 0, -7)).Count(&c.Sent7d)
	s.scope(a, canA).Where("status = ? AND decided_at >= ?", StatusRejected, now.AddDate(0, 0, -ClosedListDays)).Count(&c.Rejected30d)
	s.scope(a, canA).Where("status = ? AND decided_at >= ?", StatusSuperseded, now.AddDate(0, 0, -ClosedListDays)).Count(&c.Superseded30d)
	if s.db().Migrator().HasTable("audit_logs") {
		s.db().Table("audit_logs").Where("organization_id = ? AND action = ? AND created_at >= ?", a.Org, "outbox.send_now", now.AddDate(0, 0, -7)).Count(&c.SentNow7d)
	}
	var oldest []time.Time
	s.scope(a, canA).Where("status IN ?", []string{StatusPending, StatusFailed}).Order("created_at").Limit(1).Pluck("created_at", &oldest)
	if len(oldest) > 0 {
		c.OldestAt = &oldest[0]
	}
	var next []time.Time
	s.scope(a, canA).Where("status = ? AND send_at IS NOT NULL", StatusApproved).Order("send_at").Limit(1).Pluck("send_at", &next)
	if len(next) > 0 {
		c.NextSendAt = &next[0]
	}
	return c
}

// Filter is one filter choice with its count.
type Filter struct {
	Key   string `json:"key,omitempty"`
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	Name  string `json:"name,omitempty"`
	N     int64  `json:"n"`
}

var tabStatuses = map[string][]string{
	"waiting":  {StatusPending, StatusFailed},
	"approved": {StatusApproved, StatusSending},
	"sent":     {StatusSent},
	"closed":   {StatusRejected, StatusSuperseded},
}

// List lists items for a person.
func (s *Service) List(a Actor, q Query) ([]Row, int64, map[string][]Filter) {
	canA := s.CanApprove(a.Org, a.User, a.Role)
	st := s.SettingsFor(a.Org)
	base := func() *gorm.DB {
		db := s.scope(a, canA)
		if q.Status != "" {
			db = db.Where("status = ?", q.Status)
		} else {
			tab := q.Tab
			if _, ok := tabStatuses[tab]; !ok {
				tab = "waiting"
			}
			db = db.Where("status IN ?", tabStatuses[tab])
			if tab == "closed" {
				db = db.Where("COALESCE(decided_at, updated_at) >= ?", s.now().AddDate(0, 0, -ClosedListDays))
			}
		}
		return db
	}
	db := base()
	if q.Kind != "" {
		db = db.Where("kind = ?", q.Kind)
	}
	if q.Channel != "" {
		db = db.Where("channel = ?", q.Channel)
	}
	if q.CustomerID != "" {
		db = db.Where("customer_id = ?", q.CustomerID)
	}
	if q.CreatedBy != "" {
		who := q.CreatedBy
		if who == "me" {
			who = a.User
		}
		if who == "automatic" {
			who = ""
		}
		db = db.Where("created_by = ?", who)
	}
	if q.TriggerKind != "" {
		db = db.Where("trigger_kind = ?", q.TriggerKind)
	}
	now := s.now()
	switch q.Flag {
	case "stale":
		db = db.Where("dated AND status IN ? AND created_at <= ?", []string{StatusPending, StatusFailed}, now.Add(-time.Duration(st.StaleDays)*24*time.Hour))
	case "time_sensitive":
		db = db.Where("blocks <> '' OR (deadline IS NOT NULL AND deadline <= ?)", now.Add(DeadlineSoonWindow))
	}
	if t := strings.TrimSpace(q.Q); t != "" {
		like := "%" + strings.ToLower(t) + "%"
		db = db.Where(`LOWER("to") LIKE ? OR LOWER(to_name) LIKE ? OR LOWER(subject) LIKE ? OR LOWER(record_number) LIKE ? OR LOWER(customer_name) LIKE ? OR LOWER(message) LIKE ?`,
			like, like, like, like, like, like)
	}
	var total int64
	db.Count(&total)
	order := "created_at ASC, id"
	switch {
	case q.Sort == "newest":
		order = "created_at DESC, id"
	case q.Sort == "oldest":
	case q.Tab == "sent" || q.Tab == "closed" || q.Status == StatusSent || q.Status == StatusRejected || q.Status == StatusSuperseded:
		order = "updated_at DESC, id"
	case q.Tab == "approved" || q.Status == StatusApproved:
		order = "send_at ASC NULLS FIRST, created_at, id"
	}
	per := q.PerPage
	if per <= 0 {
		per = 50
	}
	if per > 200 {
		per = 200
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	var items []Item
	db.Order(order).Offset((page - 1) * per).Limit(per).Find(&items)
	rows := make([]Row, 0, len(items))
	for i := range items {
		rows = append(rows, s.RowOf(&items[i], st))
	}
	// Filter choices (within the tab).
	filters := map[string][]Filter{"kinds": {}, "channels": {}, "people": {}}
	type kv struct {
		K    string
		Name string
		N    int64
	}
	var ks []kv
	base().Select("kind AS k, COUNT(*) AS n").Group("kind").Order("n DESC").Scan(&ks)
	for _, k := range ks {
		filters["kinds"] = append(filters["kinds"], Filter{Key: k.K, Label: KindLabel(k.K), N: k.N})
	}
	var cs []kv
	base().Select("channel AS k, COUNT(*) AS n").Group("channel").Order("n DESC").Scan(&cs)
	for _, c := range cs {
		filters["channels"] = append(filters["channels"], Filter{Key: c.K, Label: map[string]string{ChanEmail: "Email", ChanSMS: "SMS", ChanWhatsApp: "WhatsApp"}[c.K], N: c.N})
	}
	var ps []kv
	base().Select("created_by AS k, MAX(created_by_name) AS name, COUNT(*) AS n").Group("created_by").Order("n DESC").Scan(&ps)
	for _, p := range ps {
		name := p.Name
		if p.K == "" {
			name = "Automatic"
		}
		filters["people"] = append(filters["people"], Filter{ID: p.K, Name: name, N: p.N})
	}
	return rows, total, filters
}
