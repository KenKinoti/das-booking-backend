package expenses

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Service is the expenses module.
type Service struct {
	DB  *gorm.DB
	Fin *finance.Service
	FX  *fxrates.Service
	Now func() time.Time

	ipMu   sync.Mutex
	ipVal  string
	ipWhen time.Time
}

// NewService builds the service.
func NewService(db *gorm.DB, fx *fxrates.Service) *Service {
	if fx == nil {
		fx = &fxrates.Service{DB: db}
	}
	s := &Service{DB: db, Fin: finance.NewService(db), FX: fx, Now: time.Now}
	EgressIP = func() string { return s.CachedEgressIP() }
	return s
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

// ValidationError is a user-facing input problem (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// ErrNotFound is returned for unknown ids.
var ErrNotFound = errors.New("not found")

// ---------- vendors ----------

var builtinVendors = []Vendor{
	{Key: "synergy", Name: "Synergy Wholesale", Category: CatDomains, Senders: []string{"synergywholesale.com"}, Builtin: true, CountFrom: "invoices"},
	{Key: "google_workspace", Name: "Google Workspace", Category: CatEmail, Senders: []string{"payments-noreply@google.com"}, Builtin: true},
}

// EnsureVendors creates the built-in vendors for an organisation.
func (s *Service) EnsureVendors(org string) {
	for _, b := range builtinVendors {
		var n int64
		s.DB.Model(&Vendor{}).Where("organization_id = ? AND key = ?", org, b.Key).Count(&n)
		if n == 0 {
			v := b
			v.ID, v.OrganizationID, v.CreatedAt, v.UpdatedAt = uuid.NewString(), org, s.now(), s.now()
			if err := s.DB.Create(&v).Error; err != nil {
				log.Printf("expenses: create vendor %s: %v", b.Key, err)
			}
		}
	}
}

// Vendors lists the organisation's vendors.
func (s *Service) Vendors(org string) []Vendor {
	s.EnsureVendors(org)
	out := []Vendor{}
	s.DB.Where("organization_id = ?", org).Order("builtin DESC, name").Find(&out)
	return out
}

func (s *Service) vendorByKey(org, key string) *Vendor {
	if key == "" {
		return nil
	}
	s.EnsureVendors(org)
	var v Vendor
	if s.DB.Where("organization_id = ? AND key = ?", org, key).First(&v).Error != nil {
		return nil
	}
	return &v
}

func (s *Service) vendorByID(org, id string) *Vendor {
	if id == "" {
		return nil
	}
	var v Vendor
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&v).Error != nil {
		return nil
	}
	return &v
}

// vendorForSender matches a custom vendor by its sender list.
func (s *Service) vendorForSender(org, sender string) *Vendor {
	sender = strings.ToLower(sender)
	if sender == "" {
		return nil
	}
	for _, v := range s.Vendors(org) {
		for _, snd := range v.Senders {
			snd = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(snd, "@")))
			if snd != "" && strings.Contains(sender, snd) {
				vv := v
				return &vv
			}
		}
	}
	return nil
}

// VendorInput creates / edits a custom vendor.
type VendorInput struct {
	Name      string   `json:"name"`
	Category  string   `json:"category"`
	Senders   []string `json:"senders"`
	CountFrom string   `json:"count_from"`
}

// SaveVendor creates (id "") or updates a vendor.
func (s *Service) SaveVendor(org, id string, in VendorInput) (*Vendor, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Category == "" {
		in.Category = CatOther
	}
	if _, ok := CategoryLabels[in.Category]; !ok {
		return nil, invalid("unknown category")
	}
	senders := []string{}
	for _, x := range in.Senders {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			senders = append(senders, x)
		}
	}
	if id == "" {
		if in.Name == "" {
			return nil, invalid("enter the vendor name")
		}
		v := Vendor{ID: uuid.NewString(), OrganizationID: org, Key: "custom-" + uuid.NewString()[:8], Name: in.Name, Category: in.Category, Senders: senders,
			CreatedAt: s.now(), UpdatedAt: s.now()}
		return &v, s.DB.Create(&v).Error
	}
	v := s.vendorByID(org, id)
	if v == nil {
		return nil, ErrNotFound
	}
	if in.Name != "" && !v.Builtin {
		v.Name = in.Name
	}
	v.Category, v.Senders, v.UpdatedAt = in.Category, senders, s.now()
	if in.CountFrom == "invoices" || in.CountFrom == "statement" {
		v.CountFrom = in.CountFrom
	}
	return v, s.DB.Save(v).Error
}

// ---------- currency ----------

// Home is the organisation's home currency.
func (s *Service) Home(org string) string { return orgprefs.Currency(s.DB, org) }

// convert fills the home-currency amounts and locks the rate.
func (s *Service) convert(org string, it *Item, force bool) {
	home := s.Home(org)
	it.HomeCurrency = home
	if it.Currency == "" {
		it.Currency = home
	}
	it.Net = r2(it.Total - it.Tax)
	if force || it.FxRate <= 0 || it.FxSource == "" {
		q := s.FX.Quote(org, it.Currency, home)
		if q.OK() {
			it.FxRate, it.FxSource = q.Rate, q.Source
			t := s.now()
			if q.AsOf != nil {
				t = *q.AsOf
			}
			it.FxDate = &t
		} else {
			it.FxRate, it.FxSource = 0, fxrates.SourceMissing
		}
	}
	if it.Currency == home {
		it.FxRate, it.FxSource = 1, fxrates.SourceSame
	}
	it.HomeTotal = fxrates.Convert(it.Total, it.FxRate, home)
	it.HomeTax = fxrates.Convert(it.Tax, it.FxRate, home)
	it.HomeNet = fxrates.Round(it.HomeTotal-it.HomeTax, home)
}

func (s *Service) toHome(org string) func(v float64, cur string) float64 {
	home := s.Home(org)
	cache := map[string]float64{}
	return func(v float64, cur string) float64 {
		cur = strings.ToUpper(cur)
		if cur == "" || cur == home {
			return v
		}
		r, ok := cache[cur]
		if !ok {
			q := s.FX.Quote(org, cur, home)
			r = q.Rate
			cache[cur] = r
		}
		return fxrates.Convert(v, r, home)
	}
}

// ---------- ingest ----------

// Incoming is a document found by a source.
type Incoming struct {
	Source, ExternalID string
	X                  Extracted
	VendorID           string // explicit vendor (upload form / CSV import)
	VendorName         string // free-text vendor (AI / CSV)
	Category           string
	Subject, Sender    string
	Text               string
	DocumentID         string
	Description        string
}

var reInvNorm = regexp.MustCompile(`[^a-z0-9]`)

func normInv(s string) string { return reInvNorm.ReplaceAllString(strings.ToLower(s), "") }

func dedupeKey(vendor string, it *Item) string {
	if n := normInv(it.InvoiceNumber); n != "" {
		return vendor + "|inv|" + n
	}
	d := ""
	if it.InvoiceDate != nil {
		d = it.InvoiceDate.Format(dayFmt)
	}
	return fmt.Sprintf("%s|amt|%s|%.2f|%s", vendor, it.Currency, it.Total, d)
}

// exists returns the item already stored for (source, external id).
func (s *Service) exists(org, source, ext string) *Item {
	var it Item
	if s.DB.Where("organization_id = ? AND source = ? AND external_id = ?", org, source, ext).First(&it).Error == nil {
		return &it
	}
	return nil
}

// findDuplicate finds the pending/approved item that is the same invoice.
func (s *Service) findDuplicate(org string, it *Item) *Item {
	vendorQ := s.DB.Where("organization_id = ? AND id <> ? AND status IN ?", org, it.ID, []string{StatusPending, StatusApproved})
	if it.VendorID != "" {
		vendorQ = vendorQ.Where("vendor_id = ?", it.VendorID)
	} else if it.VendorName != "" {
		vendorQ = vendorQ.Where("LOWER(vendor_name) = LOWER(?)", it.VendorName)
	} else {
		// Unknown vendor (e.g. an uploaded PDF that does not name it): only a
		// strong match — same invoice number and same amount — is the same invoice.
		n := normInv(it.InvoiceNumber)
		if len(n) < 4 || it.Total <= 0 {
			return nil
		}
		var cands []Item
		s.DB.Where("organization_id = ? AND id <> ? AND status IN ? AND invoice_number <> ''", org, it.ID, []string{StatusPending, StatusApproved}).Find(&cands)
		for i := range cands {
			c := &cands[i]
			if normInv(c.InvoiceNumber) == n && c.Currency == it.Currency && math.Abs(c.Total-it.Total) < 0.005 {
				return c
			}
		}
		return nil
	}
	var cands []Item
	vendorQ.Order("created_at").Find(&cands)
	n := normInv(it.InvoiceNumber)
	for i := range cands {
		c := &cands[i]
		cn := normInv(c.InvoiceNumber)
		if n != "" && cn != "" {
			if n == cn {
				return c
			}
			continue
		}
		if it.Total <= 0 || c.Total <= 0 || c.Currency != it.Currency || math.Abs(c.Total-it.Total) >= 0.005 {
			continue
		}
		if it.InvoiceDate == nil || c.InvoiceDate == nil {
			continue
		}
		if math.Abs(it.InvoiceDate.Sub(*c.InvoiceDate).Hours()) <= 72 {
			return c
		}
	}
	return nil
}

// enrich copies fields the target lacks from a duplicate.
func (s *Service) enrich(org string, target, from *Item) {
	upd := map[string]any{}
	if target.Status == StatusPending {
		if target.Total <= 0 && from.Total > 0 {
			target.Total, target.Tax, target.Currency, target.TaxNote = from.Total, from.Tax, from.Currency, from.TaxNote
			if from.TaxNote == "" && target.TaxNote != "" {
				target.TaxNote = ""
			}
			s.convert(org, target, true)
			upd["total"], upd["tax"], upd["net"], upd["currency"], upd["tax_note"] = target.Total, target.Tax, target.Net, target.Currency, target.TaxNote
			upd["home_currency"], upd["fx_rate"], upd["fx_source"], upd["fx_date"] = target.HomeCurrency, target.FxRate, target.FxSource, target.FxDate
			upd["home_total"], upd["home_tax"], upd["home_net"] = target.HomeTotal, target.HomeTax, target.HomeNet
			c := math.Max(target.Confidence, from.Confidence)
			upd["confidence"] = c
			w := []string{}
			for _, x := range target.Warnings {
				if !strings.HasPrefix(x, "No amount found") {
					w = append(w, x)
				}
			}
			wj, _ := json.Marshal(w)
			upd["warnings"] = string(wj)
		} else if strings.Contains(target.TaxNote, "estimated") && from.TaxNote != "" && !strings.Contains(from.TaxNote, "estimated") && math.Abs(from.Total-target.Total) < 0.005 {
			// A real tax figure (from the invoice PDF) replaces an estimate.
			target.Tax, target.TaxNote = from.Tax, from.TaxNote
			s.convert(org, target, false)
			upd["tax"], upd["net"], upd["tax_note"], upd["home_tax"], upd["home_net"] = target.Tax, target.Net, target.TaxNote, target.HomeTax, target.HomeNet
		}
		if target.InvoiceNumber == "" && from.InvoiceNumber != "" {
			upd["invoice_number"] = from.InvoiceNumber
		}
		if target.PeriodStart == nil && from.PeriodStart != nil {
			upd["period_start"], upd["period_end"] = from.PeriodStart, from.PeriodEnd
		}
		if target.PaidDate == nil && from.PaidDate != nil {
			upd["paid_date"] = from.PaidDate
		}
	}
	if target.DocumentID == "" && from.DocumentID != "" {
		upd["document_id"] = from.DocumentID
	}
	if len(upd) > 0 {
		upd["updated_at"] = s.now()
		s.DB.Model(&Item{}).Where("id = ?", target.ID).Updates(upd)
	}
}

// Ingest stores one incoming document. It never creates the same (source,
// external id) twice and links duplicates of an existing invoice.
func (s *Service) Ingest(org string, in Incoming) (*Item, bool, error) {
	if in.ExternalID == "" {
		return nil, false, errors.New("missing external id")
	}
	if ex := s.exists(org, in.Source, in.ExternalID); ex != nil {
		return ex, false, nil
	}
	x := in.X
	it := &Item{ID: uuid.NewString(), OrganizationID: org, Source: in.Source, ExternalID: in.ExternalID, Status: StatusPending, Kind: x.Kind,
		InvoiceNumber: x.InvoiceNumber, InvoiceDate: x.Date, PeriodStart: x.PeriodStart, PeriodEnd: x.PeriodEnd, Service: x.Service,
		Currency: strings.ToUpper(x.Currency), Total: r2(x.Total), Tax: r2(x.Tax), TaxNote: x.TaxNote, Confidence: x.Confidence, Method: x.Method,
		AIExtract: x.Method == "ai", Warnings: x.Warnings, Subject: clip(in.Subject, 490), Sender: clip(in.Sender, 250), Snippet: clip(in.Text, 1500),
		DocumentID: in.DocumentID, Description: in.Description, Paid: true, PaidDate: x.PaidDate, CreatedAt: s.now(), UpdatedAt: s.now()}
	if it.Kind == "" {
		it.Kind = KindInvoice
	}
	if x.Paid != nil {
		it.Paid = *x.Paid
	}
	if it.Warnings == nil {
		it.Warnings = []string{}
	}
	// Vendor
	var v *Vendor
	if in.VendorID != "" {
		v = s.vendorByID(org, in.VendorID)
	}
	if v == nil && x.VendorKey != "" {
		v = s.vendorByKey(org, x.VendorKey)
	}
	if v == nil && in.Sender != "" {
		v = s.vendorForSender(org, in.Sender)
	}
	if v != nil {
		it.VendorID, it.VendorName = v.ID, v.Name
		it.Category = v.Category
	} else {
		it.VendorName = strings.TrimSpace(in.VendorName)
		if it.VendorName == "" {
			it.VendorName = senderName(in.Sender)
		}
	}
	if in.Category != "" {
		if _, ok := CategoryLabels[in.Category]; ok {
			it.Category = in.Category
		}
	}
	if it.Category == "" {
		it.Category = guessCategory(it.VendorName + " " + it.Service + " " + in.Subject)
	}
	if v != nil && v.Key == "synergy" {
		it.Category = synergyCategory(in.Text+" "+it.Service, v.Category)
	}
	if it.Service == "" {
		it.Service = it.VendorName
	}
	if v != nil && v.Key == "synergy" && regexp.MustCompile(`(?i)account funds|add funds|top[ -]?up|prepay`).MatchString(in.Text) {
		it.Warnings = append(it.Warnings, "Looks like an account top-up (prepaid funds) — approve it if you count Synergy costs from invoices, or ignore it if you count statement lines")
	}
	if it.Paid && it.PaidDate == nil && it.Kind != KindInvoice {
		it.PaidDate = it.InvoiceDate
	}
	if it.Total > 0 || it.Currency != "" {
		s.convert(org, it, true)
	}
	if x.Ignore != "" {
		it.Status = StatusIgnored
		it.Notes = x.Ignore
	}
	if it.Kind == KindDeclined {
		it.Status = StatusIgnored
		it.Notes = "Payment declined notice — shown as an alert, not an expense"
	}
	it.DedupeKey = dedupeKey(it.VendorID+it.VendorName, it)

	if it.Status == StatusPending {
		// A "payment received" without an invoice number completes the
		// invoice email it pays (Google sends the amount only in the PDF and
		// in the payment confirmation).
		if it.Kind == KindPayment && it.InvoiceNumber == "" && it.Total > 0 && it.VendorID != "" && it.InvoiceDate != nil {
			var cands []Item
			s.DB.Where("organization_id = ? AND vendor_id = ? AND kind = ? AND status IN ? AND invoice_date BETWEEN ? AND ?", org, it.VendorID, KindInvoice,
				[]string{StatusPending, StatusApproved}, it.InvoiceDate.AddDate(0, 0, -40), it.InvoiceDate.AddDate(0, 0, 1)).Order("invoice_date DESC").Find(&cands)
			for i := range cands {
				c := &cands[i]
				if c.Total <= 0 && c.Status == StatusPending {
					s.enrich(org, c, it)
					it.Status, it.DuplicateOf = StatusDuplicate, c.ID
					it.Notes = "Payment confirmation for invoice " + nonEmpty(c.InvoiceNumber, c.ID) + " (its amount was taken from this email)"
					break
				}
				if c.Currency == it.Currency && math.Abs(c.Total-it.Total) < 0.005 {
					it.Status, it.DuplicateOf = StatusDuplicate, c.ID
					it.Notes = "Payment confirmation for invoice " + nonEmpty(c.InvoiceNumber, c.ID)
					break
				}
			}
		}
		// The other order (Gmail lists newest first): an invoice without an
		// amount takes it from a payment confirmation already stored.
		if it.Kind == KindInvoice && it.Total <= 0 && it.VendorID != "" && it.InvoiceDate != nil {
			var pays []Item
			s.DB.Where("organization_id = ? AND vendor_id = ? AND kind = ? AND status = ? AND invoice_number = '' AND total > 0 AND invoice_date BETWEEN ? AND ?",
				org, it.VendorID, KindPayment, StatusPending, it.InvoiceDate.AddDate(0, 0, -1), it.InvoiceDate.AddDate(0, 0, 40)).Order("invoice_date").Limit(1).Find(&pays)
			if len(pays) == 1 {
				p := pays[0]
				it.Total, it.Tax, it.Currency, it.TaxNote = p.Total, p.Tax, p.Currency, p.TaxNote
				it.Paid, it.PaidDate = true, p.InvoiceDate
				it.Confidence = math.Max(it.Confidence, p.Confidence)
				it.Warnings = dropWarn(it.Warnings, "No amount found")
				s.convert(org, it, true)
				s.DB.Model(&Item{}).Where("id = ?", p.ID).Updates(map[string]any{"status": StatusDuplicate, "duplicate_of": it.ID,
					"notes": "Payment confirmation for invoice " + nonEmpty(it.InvoiceNumber, "") + " (its amount was used for the invoice)", "updated_at": s.now()})
			}
		}
		if it.Status == StatusPending {
			if d := s.findDuplicate(org, it); d != nil && d.Status == StatusPending && d.Kind == KindPayment && d.InvoiceNumber == "" && it.Kind != KindPayment {
				// The invoice is the better record: the earlier payment confirmation becomes its duplicate.
				if it.Total <= 0 {
					it.Total, it.Tax, it.Currency = d.Total, d.Tax, d.Currency
					s.convert(org, it, true)
				}
				if it.PaidDate == nil {
					it.Paid, it.PaidDate = true, d.InvoiceDate
				}
				s.DB.Model(&Item{}).Where("id = ?", d.ID).Updates(map[string]any{"status": StatusDuplicate, "duplicate_of": it.ID,
					"notes": "Payment confirmation for invoice " + nonEmpty(it.InvoiceNumber, ""), "updated_at": s.now()})
			} else if d != nil {
				if it.VendorID == "" && it.VendorName == "" {
					it.VendorID, it.VendorName, it.Category = d.VendorID, d.VendorName, d.Category
				}
				s.enrich(org, d, it)
				it.Status, it.DuplicateOf = StatusDuplicate, d.ID
				it.Notes = "Same invoice as one already in Expenses (" + nonEmpty(d.InvoiceNumber, d.Source) + ")"
			}
		}
	}
	if err := s.DB.Create(it).Error; err != nil {
		if ex := s.exists(org, in.Source, in.ExternalID); ex != nil {
			return ex, false, nil // concurrent sync stored it first
		}
		return nil, false, err
	}
	return it, true, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func senderName(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.Index(from, "<"); i > 0 {
		return strings.Trim(strings.TrimSpace(from[:i]), `"`)
	}
	if i := strings.Index(from, "@"); i >= 0 {
		d := from[i+1:]
		d = strings.TrimSuffix(strings.Split(d, ">")[0], ".com")
		return strings.Title(strings.Split(d, ".")[0])
	}
	return from
}

func guessCategory(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "workspace") || strings.Contains(l, "microsoft 365") || strings.Contains(l, "office 365") || strings.Contains(l, "email") || strings.Contains(l, "mailbox") || strings.Contains(l, "zoho mail"):
		return CatEmail
	case strings.Contains(l, "domain") || strings.Contains(l, ".com") || strings.Contains(l, "registration") || strings.Contains(l, "renewal"):
		return CatDomains
	case strings.Contains(l, "hosting") || strings.Contains(l, "server") || strings.Contains(l, "vps") || strings.Contains(l, "cpanel") || strings.Contains(l, "cloud"):
		return CatHosting
	case strings.Contains(l, "subscription") || strings.Contains(l, "software") || strings.Contains(l, "licen") || strings.Contains(l, "saas") || strings.Contains(l, "plan"):
		return CatSoftware
	}
	return CatOther
}

// synergyCategory: Synergy sells domains and hosting; hosting when the text says so.
func synergyCategory(text, def string) string {
	l := strings.ToLower(text)
	if strings.Contains(l, "hosting") || strings.Contains(l, "cpanel") || strings.Contains(l, "wordpress") {
		return CatHosting
	}
	if strings.Contains(l, "ssl") || strings.Contains(l, "certificate") {
		return CatSoftware
	}
	if def == "" {
		return CatDomains
	}
	return def
}

// ---------- documents ----------

// StoreDocument saves a file once per organisation (by SHA-256).
func (s *Service) StoreDocument(org, name, mime, source string, data []byte, text string) (*Document, bool, error) {
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	var d Document
	if s.DB.Where("organization_id = ? AND sha256 = ?", org, h).First(&d).Error == nil {
		return &d, false, nil
	}
	d = Document{ID: uuid.NewString(), OrganizationID: org, SHA256: h, Filename: clip(name, 250), MimeType: mime, Size: len(data), Data: data, Text: text, Source: source, CreatedAt: s.now()}
	if err := s.DB.Create(&d).Error; err != nil {
		if s.DB.Where("organization_id = ? AND sha256 = ?", org, h).First(&d).Error == nil {
			return &d, false, nil
		}
		return nil, false, err
	}
	return &d, true, nil
}

// ---------- extraction pipeline ----------

// extractDoc runs rules on text, then AI when rules are unsure.
func extractDoc(ctx context.Context, text string, file []byte, mime string, h Hints) (Extracted, string, string) {
	x := Extract(text, h)
	aiCat, aiVendor := "", ""
	needAI := (x.Confidence < 0.6 || !x.HasTotal) && x.Ignore == "" && x.Kind != KindDeclined && x.Balance == nil
	if needAI && AIEnabled() && (strings.TrimSpace(text) != "" || len(file) > 0) {
		if a, meta, err := AIExtract(ctx, text, file, mime); err == nil && a != nil {
			parts := strings.SplitN(meta, "|", 2)
			aiCat = parts[0]
			if len(parts) > 1 {
				aiVendor = parts[1]
			}
			// Keep what the rules were sure about; fill the rest from AI.
			if x.InvoiceNumber == "" {
				x.InvoiceNumber = a.InvoiceNumber
			}
			if x.Date == nil || containsStr(x.Warnings, "Date taken from the email") {
				if a.Date != nil {
					x.Date = a.Date
				}
			}
			if !x.HasTotal && a.HasTotal {
				x.Total, x.HasTotal = a.Total, true
				if a.Currency != "" {
					x.Currency = a.Currency
				}
				if a.HasTax {
					x.Tax, x.HasTax, x.TaxNote = a.Tax, true, a.TaxNote
				}
			}
			if x.Service == "" {
				x.Service = a.Service
			}
			if x.VendorKey == "" {
				x.VendorKey = a.VendorKey
			}
			if x.PeriodStart == nil {
				x.PeriodStart, x.PeriodEnd = a.PeriodStart, a.PeriodEnd
			}
			if a.Ignore != "" && !x.HasTotal {
				x.Ignore = a.Ignore
			}
			x.Method, x.Confidence = "ai", math.Max(x.Confidence, a.Confidence)
			w := []string{"AI-extracted, please confirm"}
			for _, s := range x.Warnings {
				if !strings.HasPrefix(s, "No amount found") || !x.HasTotal {
					w = append(w, s)
				}
			}
			x.Warnings = w
		} else if err != nil {
			x.Warnings = append(x.Warnings, "AI extraction failed: "+err.Error())
		}
	}
	return x, aiCat, aiVendor
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// UploadResult reports one uploaded file.
type UploadResult struct {
	File    string  `json:"file"`
	Items   []*Item `json:"items"`
	Created int     `json:"created"`
	Skipped int     `json:"skipped"`
	Message string  `json:"message"`
}

// Upload processes an uploaded invoice PDF, CSV or image.
func (s *Service) Upload(ctx context.Context, org, name, mime string, data []byte, vendorID, category string) (*UploadResult, error) {
	res := &UploadResult{File: name, Items: []*Item{}}
	if len(data) == 0 {
		return nil, invalid("%s is empty", name)
	}
	lname := strings.ToLower(name)
	mime = sniffMime(lname, mime, data)
	var defCur string
	if v := s.vendorByID(org, vendorID); v != nil && v.Key == "synergy" {
		defCur = "AUD"
	}
	if defCur == "" {
		defCur = s.Home(org)
	}
	switch {
	case mime == "text/csv":
		doc, _, err := s.StoreDocument(org, name, mime, SourceUpload, data, "")
		if err != nil {
			return nil, err
		}
		vk := VendorFor("", name, string(data[:min(len(data), 4000)]))
		if vk == "synergy" && defCur == s.Home(org) {
			defCur = "AUD"
		}
		rows, err := ParseCSV(data, defCur)
		if err != nil {
			return nil, invalid("%s: %v", name, err)
		}
		for _, r := range rows {
			x := Extracted{Kind: KindLine, VendorKey: vk, InvoiceNumber: r.InvoiceNumber, Date: r.Date, Currency: r.Currency, Total: r.Total, Tax: r.Tax,
				HasTotal: true, HasTax: r.HasTax, Service: r.Description, Method: "csv", Confidence: 0.75, Paid: boolp(true)}
			if r.HasTax {
				x.TaxNote = "Tax (from CSV)"
				x.Confidence = 0.85
			}
			if vk == "synergy" && !r.HasTax && r.Currency == "AUD" {
				x.Tax, x.HasTax, x.TaxNote = math.Round(r.Total/11*100)/100, true, "GST 1/11 of total (estimated — Synergy prices include GST)"
			}
			if r.Skip != "" {
				x.Ignore = r.Skip
			}
			it, created, err := s.Ingest(org, Incoming{Source: SourceUpload, ExternalID: fmt.Sprintf("%s#%d", doc.SHA256, r.Line), X: x, VendorID: vendorID,
				Category: category, Subject: name, Text: r.Description, DocumentID: doc.ID, Description: r.Description})
			if err != nil {
				return nil, err
			}
			if created {
				res.Created++
			} else {
				res.Skipped++
			}
			res.Items = append(res.Items, it)
		}
		res.Message = fmt.Sprintf("%d line(s) read", len(rows))
		return res, nil
	case mime == "application/pdf" || strings.HasPrefix(mime, "image/"):
		text := ""
		if mime == "application/pdf" {
			text = PDFText(data)
		}
		doc, _, err := s.StoreDocument(org, name, mime, SourceUpload, data, text)
		if err != nil {
			return nil, err
		}
		x, aiCat, aiVendor := extractDoc(ctx, text, data, mime, Hints{Filename: name, DefaultCur: defCur})
		if strings.TrimSpace(text) == "" && x.Method != "ai" {
			x.Warnings = append(x.Warnings, map[bool]string{true: "No text could be read from this image — enter the details (AI extraction is not configured)",
				false: "No text could be read from this PDF (scanned?) — enter the details"}[strings.HasPrefix(mime, "image/")])
			x.Confidence = 0
		}
		if category == "" {
			category = aiCat
		}
		it, created, err := s.Ingest(org, Incoming{Source: SourceUpload, ExternalID: doc.SHA256, X: x, VendorID: vendorID, VendorName: aiVendor, Category: category,
			Subject: name, Text: text, DocumentID: doc.ID})
		if err != nil {
			return nil, err
		}
		if created {
			res.Created++
		} else {
			res.Skipped++
			res.Message = "Already uploaded"
		}
		res.Items = append(res.Items, it)
		return res, nil
	}
	return nil, invalid("%s: upload a PDF, CSV or image (PNG/JPG)", name)
}

func sniffMime(name, mime string, data []byte) string {
	switch {
	case strings.HasSuffix(name, ".pdf") || strings.HasPrefix(string(data[:min(len(data), 5)]), "%PDF"):
		return "application/pdf"
	case strings.HasSuffix(name, ".csv") || strings.Contains(mime, "csv"):
		return "text/csv"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	case strings.HasSuffix(name, ".gif"):
		return "image/gif"
	}
	m := http.DetectContentType(data)
	if strings.HasPrefix(m, "image/") {
		return m
	}
	if strings.HasPrefix(m, "text/plain") && strings.Contains(string(data[:min(len(data), 300)]), ",") {
		return "text/csv"
	}
	return m
}

func boolp(b bool) *bool { return &b }

// ---------- egress IP ----------

// CachedEgressIP returns this server's public IP (cached 10 minutes) via an
// echo service (EXPENSES_IP_ECHO_URL, default api.ipify.org). "" when unreachable.
func (s *Service) CachedEgressIP() string {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	if s.ipVal != "" && time.Since(s.ipWhen) < 10*time.Minute {
		return s.ipVal
	}
	if time.Since(s.ipWhen) < time.Minute && s.ipVal == "" && !s.ipWhen.IsZero() {
		return ""
	}
	s.ipWhen = time.Now()
	u := envOr("EXPENSES_IP_ECHO_URL", "https://api.ipify.org?format=json")
	if u == "off" {
		return ""
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var j struct {
		IP string `json:"ip"`
	}
	ip := ""
	if json.Unmarshal(raw, &j) == nil && j.IP != "" {
		ip = j.IP
	} else if m := regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b|[0-9a-fA-F:]{6,}`).FindString(string(raw)); m != "" {
		ip = m
	}
	s.ipVal = ip
	return ip
}

// ---------- connections ----------

func (s *Service) connection(org, kind string) *Connection {
	var c Connection
	if s.DB.Where("organization_id = ? AND kind = ?", org, kind).First(&c).Error != nil {
		return nil
	}
	return &c
}

func (s *Service) connectionOrNew(org, kind string) *Connection {
	if c := s.connection(org, kind); c != nil {
		return c
	}
	return &Connection{ID: uuid.NewString(), OrganizationID: org, Kind: kind, CreatedAt: s.now()}
}

func enc(v string) string {
	if v == "" {
		return ""
	}
	out, err := encryption.GlobalEncryption.Encrypt(v)
	if err != nil {
		return ""
	}
	return out
}

func dec(v string) string {
	if v == "" {
		return ""
	}
	out, err := encryption.GlobalEncryption.Decrypt(v)
	if err != nil {
		return ""
	}
	return out
}

// SynergyInput configures the Synergy Wholesale connection.
type SynergyInput struct {
	ResellerID string   `json:"reseller_id"`
	APIKey     string   `json:"api_key"` // blank keeps the stored key
	LowBalance *float64 `json:"low_balance"`
	Enabled    *bool    `json:"enabled"`
	CountFrom  string   `json:"count_from"`
}

// SaveSynergy stores the reseller ID and (encrypted) API key.
func (s *Service) SaveSynergy(org string, in SynergyInput) (*Connection, error) {
	c := s.connectionOrNew(org, "synergy")
	in.ResellerID = strings.TrimSpace(in.ResellerID)
	if in.ResellerID == "" {
		return nil, invalid("enter your Synergy Wholesale reseller ID")
	}
	c.Account = in.ResellerID
	if k := strings.TrimSpace(in.APIKey); k != "" {
		c.SecretEnc = enc(k)
	}
	if c.SecretEnc == "" {
		return nil, invalid("enter your API key")
	}
	if in.LowBalance != nil {
		if *in.LowBalance < 0 {
			return nil, invalid("the low-balance alert must be zero or more")
		}
		c.Settings.LowBalance = *in.LowBalance
	}
	c.Enabled = true
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	if in.CountFrom == "invoices" || in.CountFrom == "statement" {
		if v := s.vendorByKey(org, "synergy"); v != nil {
			s.DB.Model(&Vendor{}).Where("id = ?", v.ID).Update("count_from", in.CountFrom)
		}
	}
	c.UpdatedAt = s.now()
	return c, s.DB.Save(c).Error
}

func (s *Service) synergyClient(org string) (*SynergyClient, *Connection, error) {
	c := s.connection(org, "synergy")
	if c == nil || c.Account == "" || c.SecretEnc == "" {
		return nil, nil, invalid("connect Synergy Wholesale first (reseller ID and API key)")
	}
	key := dec(c.SecretEnc)
	if key == "" {
		return nil, c, invalid("the stored API key could not be read — enter it again")
	}
	return NewSynergyClient(c.Account, key), c, nil
}

// TestSynergy checks the credentials with balanceQuery.
func (s *Service) TestSynergy(ctx context.Context, org string) (float64, error) {
	cl, c, err := s.synergyClient(org)
	if err != nil {
		return 0, err
	}
	bal, err := cl.Balance(ctx)
	now := s.now()
	if err != nil {
		s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"last_status": "error", "last_error": err.Error(), "updated_at": now})
		return 0, err
	}
	s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"balance": bal, "balance_at": now, "last_status": "ok", "last_error": "", "updated_at": now})
	return bal, nil
}

// SyncResult summarises one sync.
type SyncResult struct {
	Source     string   `json:"source"`
	OK         bool     `json:"ok"`
	Error      string   `json:"error,omitempty"`
	Hint       string   `json:"hint,omitempty"`
	Found      int      `json:"found"`
	Created    int      `json:"created"`
	Skipped    int      `json:"skipped"`
	Duplicates int      `json:"duplicates"`
	Renewals   int      `json:"renewals,omitempty"`
	Balance    *float64 `json:"balance,omitempty"`
	Notes      []string `json:"notes,omitempty"`
	At         string   `json:"at"`

	createdIDs []string
}

func (s *Service) finishSync(c *Connection, r *SyncResult) {
	now := s.now()
	r.At = now.UTC().Format(time.RFC3339)
	b, _ := json.Marshal(r)
	upd := map[string]any{"last_sync_at": now, "last_result": string(b), "updated_at": now}
	if r.OK {
		upd["last_status"], upd["last_error"] = "ok", ""
	} else {
		upd["last_status"], upd["last_error"] = "error", r.Error
	}
	s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(upd)
}

// SyncSynergy refreshes the balance, domains, hosting and renewal prices.
func (s *Service) SyncSynergy(ctx context.Context, org string) *SyncResult {
	r := &SyncResult{Source: "synergy"}
	cl, c, err := s.synergyClient(org)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	fail := func(err error) *SyncResult {
		r.Error = err.Error()
		var se *SynergyError
		if errors.As(err, &se) {
			r.Error, r.Hint = se.Message, se.Hint
		}
		s.finishSync(c, r)
		return r
	}
	bal, err := cl.Balance(ctx)
	if err != nil {
		return fail(err)
	}
	r.Balance = &bal
	now := s.now()
	s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"balance": bal, "balance_at": now})
	v := s.vendorByKey(org, "synergy")
	vid := ""
	if v != nil {
		vid = v.ID
	}
	prices, perr := cl.RenewPrices(ctx)
	if perr != nil {
		r.Notes = append(r.Notes, "Renewal prices unavailable: "+perr.Error())
	}
	seen := map[string]bool{}
	domains, err := cl.Domains(ctx)
	if err != nil {
		return fail(err)
	}
	for _, d := range domains {
		var cost *float64
		src := ""
		if t := TLDOf(d.Name, prices); t != "" {
			p := prices[t]
			cost, src = &p, "synergy_pricing"
		}
		s.upsertRenewal(org, Renewal{Source: SourceSynergy, ExternalID: "domain:" + d.Name, Kind: "domain", Name: d.Name, VendorID: vid, Status: d.Status,
			ExpiresOn: d.Expiry, AutoRenew: d.AutoRenew, BillingPeriod: "yearly", Cost: cost, Currency: "AUD", CostSource: src})
		seen["domain:"+d.Name] = true
	}
	hosting, herr := cl.Hosting(ctx)
	if herr != nil {
		r.Notes = append(r.Notes, "Hosting services unavailable: "+herr.Error())
	}
	for _, h := range hosting {
		name := h.Domain
		if name == "" {
			name = h.Plan
		}
		s.upsertRenewal(org, Renewal{Source: SourceSynergy, ExternalID: "hosting:" + h.ID, Kind: "hosting", Name: name, Plan: h.Plan, VendorID: vid, Status: h.Status,
			ExpiresOn: h.NextRenewal, AutoRenew: true, BillingPeriod: h.BillingPeriod, Currency: "AUD"})
		seen["hosting:"+h.ID] = true
	}
	// Services no longer listed are inactive.
	var rows []Renewal
	s.DB.Where("organization_id = ? AND source = ? AND active", org, SourceSynergy).Find(&rows)
	for _, row := range rows {
		if !seen[row.ExternalID] && (herr == nil || strings.HasPrefix(row.ExternalID, "domain:")) {
			s.DB.Model(&Renewal{}).Where("id = ?", row.ID).Updates(map[string]any{"active": false, "updated_at": now})
		}
	}
	r.Renewals = len(domains) + len(hosting)
	r.Found = r.Renewals
	r.OK = true
	r.Notes = append(r.Notes, "The Synergy Wholesale API has no invoice list — invoices come from Gmail (receipts and statements) or uploads.")
	s.finishSync(c, r)
	return r
}

func (s *Service) upsertRenewal(org string, in Renewal) {
	var ex Renewal
	now := s.now()
	if s.DB.Where("organization_id = ? AND source = ? AND external_id = ?", org, in.Source, in.ExternalID).First(&ex).Error == nil {
		upd := map[string]any{"name": in.Name, "plan": in.Plan, "status": in.Status, "expires_on": in.ExpiresOn, "auto_renew": in.AutoRenew,
			"billing_period": in.BillingPeriod, "vendor_id": in.VendorID, "active": true, "seen_at": now, "updated_at": now}
		if ex.CostSource != "manual" && in.Cost != nil {
			upd["cost"], upd["currency"], upd["cost_source"] = in.Cost, in.Currency, in.CostSource
		}
		s.DB.Model(&Renewal{}).Where("id = ?", ex.ID).Updates(upd)
		return
	}
	in.ID, in.OrganizationID, in.Active, in.SeenAt, in.CreatedAt, in.UpdatedAt = uuid.NewString(), org, true, &now, now, now
	if err := s.DB.Create(&in).Error; err != nil {
		log.Printf("expenses: renewal %s: %v", in.ExternalID, err)
	}
}

// ---------- Gmail ----------

// GmailInput configures the Gmail source.
type GmailInput struct {
	ClientID      string   `json:"client_id"`
	ClientSecret  string   `json:"client_secret"` // blank keeps the stored one
	Presets       []string `json:"presets"`
	CustomSenders []string `json:"custom_senders"`
	LookbackDays  int      `json:"lookback_days"`
}

// SaveGmail stores the Gmail options (and optionally the org's own OAuth client).
func (s *Service) SaveGmail(org string, in GmailInput) (*Connection, error) {
	c := s.connectionOrNew(org, "gmail")
	c.ClientID = strings.TrimSpace(in.ClientID)
	if v := strings.TrimSpace(in.ClientSecret); v != "" {
		c.ClientSecret = enc(v)
	}
	if c.ClientID == "" {
		c.ClientSecret = ""
	}
	ps := []string{}
	for _, p := range in.Presets {
		for _, k := range Presets {
			if k.Key == p {
				ps = append(ps, p)
			}
		}
	}
	c.Settings.Presets = ps
	snd := []string{}
	for _, x := range in.CustomSenders {
		x = strings.ToLower(strings.TrimSpace(x))
		if x == "" {
			continue
		}
		if !strings.Contains(x, ".") || strings.ContainsAny(x, " ()\"") {
			return nil, invalid("%q is not an email address or @domain", x)
		}
		snd = append(snd, x)
	}
	c.Settings.CustomSenders = snd
	if in.LookbackDays < 0 || in.LookbackDays > 1100 {
		return nil, invalid("look back between 1 and 1100 days")
	}
	c.Settings.LookbackDays = in.LookbackDays
	c.UpdatedAt = s.now()
	return c, s.DB.Save(c).Error
}

// OAuthClient returns the client id/secret to use (org's own, else env).
func (s *Service) OAuthClient(c *Connection) (string, string) {
	if c != nil && c.ClientID != "" {
		return c.ClientID, dec(c.ClientSecret)
	}
	return GoogleClientFromEnv()
}

// StartGmail creates the consent URL.
func (s *Service) StartGmail(org, user, redirect, returnURL string) (string, error) {
	c := s.connectionOrNew(org, "gmail")
	id, secret := s.OAuthClient(c)
	if id == "" || secret == "" {
		return "", invalid("add a Google OAuth client (client ID and secret) first — see the setup steps")
	}
	if len(c.Settings.Presets) == 0 && c.ID != "" && c.LastSyncAt == nil && c.Account == "" {
		c.Settings.Presets = []string{"google_workspace", "synergy"}
	}
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	c.State = hex.EncodeToString(b)
	exp := s.now().Add(15 * time.Minute)
	c.StateExpires, c.StateUser, c.ReturnURL, c.UpdatedAt = &exp, user, returnURL, s.now()
	if err := s.DB.Save(c).Error; err != nil {
		return "", err
	}
	return AuthCodeURL(id, redirect, c.State, c.Account), nil
}

// FinishGmail handles the OAuth callback; returns the organisation id.
func (s *Service) FinishGmail(ctx context.Context, state, code, redirect string) (string, error) {
	if state == "" || code == "" {
		return "", errors.New("Google did not return an authorisation code")
	}
	var c Connection
	if s.DB.Where("kind = ? AND state = ?", "gmail", state).First(&c).Error != nil {
		return "", errors.New("this sign-in link has expired — start again from Expenses → Sources")
	}
	if c.StateExpires == nil || s.now().After(*c.StateExpires) {
		return c.OrganizationID, errors.New("the sign-in took too long — start again")
	}
	id, secret := s.OAuthClient(&c)
	tok, err := ExchangeCode(ctx, id, secret, redirect, code)
	if err != nil {
		s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"state": "", "last_status": "error", "last_error": err.Error()})
		return c.OrganizationID, err
	}
	if tok.RefreshToken == "" && c.SecretEnc == "" {
		return c.OrganizationID, errors.New("Google did not return offline access — remove DASYIN from your Google account's third-party access and connect again")
	}
	email, err := (&Gmail{Token: tok.AccessToken}).Profile(ctx)
	if err != nil {
		return c.OrganizationID, err
	}
	upd := map[string]any{"state": "", "state_expires": nil, "account": email, "enabled": true, "last_status": "ok", "last_error": "", "updated_at": s.now()}
	if tok.RefreshToken != "" {
		upd["secret_enc"] = enc(tok.RefreshToken)
	}
	if len(c.Settings.Presets) == 0 {
		c.Settings.Presets = []string{"google_workspace", "synergy"}
		sj, _ := json.Marshal(c.Settings)
		upd["settings"] = string(sj)
	}
	return c.OrganizationID, s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(upd).Error
}

// DisconnectGmail forgets the token.
func (s *Service) DisconnectGmail(org string) error {
	return s.DB.Model(&Connection{}).Where("organization_id = ? AND kind = ?", org, "gmail").
		Updates(map[string]any{"secret_enc": "", "account": "", "enabled": false, "updated_at": s.now()}).Error
}

// DisconnectSynergy removes the credentials (renewals stay).
func (s *Service) DisconnectSynergy(org string) error {
	return s.DB.Where("organization_id = ? AND kind = ?", org, "synergy").Delete(&Connection{}).Error
}

// GmailQueries returns the searches to run.
func GmailQueries(c *Connection) []string {
	qs := []string{}
	for _, p := range Presets {
		for _, k := range c.Settings.Presets {
			if k == p.Key {
				qs = append(qs, p.Query)
			}
		}
	}
	if q := SenderQuery(c.Settings.CustomSenders); q != "" {
		qs = append(qs, q)
	}
	return qs
}

// SyncGmail reads new billing emails.
func (s *Service) SyncGmail(ctx context.Context, org string) *SyncResult {
	r := &SyncResult{Source: "gmail"}
	c := s.connection(org, "gmail")
	if c == nil || c.SecretEnc == "" {
		r.Error = "connect Gmail first"
		return r
	}
	id, secret := s.OAuthClient(c)
	tok, err := Refresh(ctx, id, secret, dec(c.SecretEnc))
	if err != nil {
		r.Error = err.Error()
		s.finishSync(c, r)
		return r
	}
	g := &Gmail{Token: tok.AccessToken}
	qs := GmailQueries(c)
	if len(qs) == 0 {
		r.Error = "choose at least one sender preset or add a sender"
		s.finishSync(c, r)
		return r
	}
	since := s.now().AddDate(0, 0, -400)
	if c.Settings.LookbackDays > 0 {
		since = s.now().AddDate(0, 0, -c.Settings.LookbackDays)
	}
	if c.LastSyncAt != nil && c.LastStatus == "ok" {
		since = c.LastSyncAt.AddDate(0, 0, -3)
	}
	seen := map[string]bool{}
	defer func() {
		// Final state of what this sync stored (a payment stored first can
		// become a duplicate of the invoice that arrives after it).
		r.Created, r.Duplicates = 0, 0
		if len(r.createdIDs) > 0 {
			var rows []struct {
				Status string
				N      int
			}
			s.DB.Raw("SELECT status, COUNT(*) AS n FROM expense_items WHERE id IN ? GROUP BY status", r.createdIDs).Scan(&rows)
			for _, x := range rows {
				switch x.Status {
				case StatusPending:
					r.Created = x.N
				case StatusDuplicate:
					r.Duplicates = x.N
				}
			}
		}
		s.finishSync(c, r)
	}()
	for _, q := range qs {
		ids, err := g.List(ctx, q+" after:"+since.Format("2006/01/02"), 500)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		for _, mid := range ids {
			if seen[mid] {
				continue
			}
			seen[mid] = true
			r.Found++
			if err := s.ingestEmail(ctx, org, g, mid, r); err != nil {
				r.Notes = append(r.Notes, err.Error())
			}
		}
	}
	r.OK = true
	return r
}

func (s *Service) ingestEmail(ctx context.Context, org string, g *Gmail, mid string, r *SyncResult) error {
	if s.exists(org, SourceGmail, mid) != nil || s.exists(org, SourceGmail, mid+"#ignored") != nil {
		r.Skipped++
		return nil
	}
	// CSV statement lines are stored as mid#att#line.
	var n int64
	s.DB.Model(&Item{}).Where("organization_id = ? AND source = ? AND external_id LIKE ?", org, SourceGmail, mid+"#%").Count(&n)
	if n > 0 {
		r.Skipped++
		return nil
	}
	m, err := g.Get(ctx, mid)
	if err != nil {
		return err
	}
	plain, htmlBody, atts := m.Bodies()
	text := plain
	if strings.TrimSpace(text) == "" && htmlBody != "" {
		text = HTMLToText(htmlBody)
	}
	subj, from := m.Header("Subject"), m.Header("From")
	when := m.Time()
	vk := VendorFor(from, subj, text)
	defCur := ""
	if vk == "synergy" {
		defCur = "AUD"
	}
	// Attachments: invoice PDFs (text joins the email's), statement CSVs.
	docID, pdfText := "", ""
	var csvAtt *MessagePart
	for i := range atts {
		a := atts[i]
		ln := strings.ToLower(a.Filename)
		isPDF := strings.HasSuffix(ln, ".pdf") || a.MimeType == "application/pdf"
		isCSV := strings.HasSuffix(ln, ".csv") || strings.Contains(a.MimeType, "csv")
		if !isPDF && !isCSV {
			continue
		}
		if isCSV {
			csvAtt = &atts[i]
			continue
		}
		var data []byte
		if a.Body.AttachmentID != "" {
			data, err = g.Attachment(ctx, mid, a.Body.AttachmentID)
			if err != nil {
				return err
			}
		} else {
			data, _ = decodeB64URL(a.Body.Data)
		}
		t := PDFText(data)
		doc, _, err := s.StoreDocument(org, a.Filename, "application/pdf", SourceGmail, data, t)
		if err != nil {
			return err
		}
		if docID == "" {
			docID, pdfText = doc.ID, t
		}
	}
	h := Hints{Subject: subj, Sender: from, When: when, DefaultCur: defCur}
	full := text
	if pdfText != "" {
		full = text + "\n" + pdfText
	}
	x, aiCat, aiVendor := extractDoc(ctx, full, nil, "", h)
	if x.Balance != nil {
		// Synergy low-balance notice: keep the balance when the API is not connected / older.
		if c := s.connection(org, "synergy"); c != nil && when != nil && (c.BalanceAt == nil || c.BalanceAt.Before(*when)) {
			s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"balance": *x.Balance, "balance_at": *when})
		} else if c == nil && when != nil {
			nc := &Connection{ID: uuid.NewString(), OrganizationID: org, Kind: "synergy", Balance: x.Balance, BalanceAt: when, CreatedAt: s.now(), UpdatedAt: s.now()}
			s.DB.Create(nc)
		}
	}
	// Statement CSV (Synergy monthly transaction statement).
	if csvAtt != nil {
		v := s.vendorByKey(org, vk)
		var data []byte
		if csvAtt.Body.AttachmentID != "" {
			data, err = g.Attachment(ctx, mid, csvAtt.Body.AttachmentID)
			if err != nil {
				return err
			}
		} else {
			data, _ = decodeB64URL(csvAtt.Body.Data)
		}
		doc, _, err := s.StoreDocument(org, csvAtt.Filename, "text/csv", SourceGmail, data, "")
		if err != nil {
			return err
		}
		countLines := v != nil && v.CountFrom == "statement"
		rows, perr := ParseCSV(data, nonEmpty(defCur, s.Home(org)))
		if perr == nil && countLines {
			for _, row := range rows {
				lx := Extracted{Kind: KindLine, VendorKey: vk, InvoiceNumber: row.InvoiceNumber, Date: row.Date, Currency: row.Currency, Total: row.Total, Tax: row.Tax,
					HasTotal: true, HasTax: row.HasTax, Service: row.Description, Method: "csv", Confidence: 0.75, Paid: boolp(true)}
				if vk == "synergy" && !row.HasTax && row.Currency == "AUD" {
					lx.Tax, lx.HasTax, lx.TaxNote = math.Round(row.Total/11*100)/100, true, "GST 1/11 of total (estimated — Synergy prices include GST)"
				}
				if row.Skip != "" {
					lx.Ignore = row.Skip
				}
				it, created, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: fmt.Sprintf("%s#csv#%d", mid, row.Line), X: lx, Subject: subj, Sender: from,
					Text: row.Description, DocumentID: doc.ID, Description: row.Description})
				if err != nil {
					return err
				}
				countResult(r, it, created)
			}
			return nil
		}
		// Not counted line by line: keep the statement as an ignored document.
		note := "Monthly statement stored; Synergy expenses are counted from invoices/receipts (change in Sources to count statement lines instead)"
		if perr != nil {
			note = "Statement CSV could not be read: " + perr.Error()
		}
		x = Extracted{Kind: KindInvoice, VendorKey: vk, Ignore: note, Method: "csv", Date: when}
		it, created, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: mid, X: x, Subject: subj, Sender: from, Text: text, DocumentID: doc.ID})
		if err != nil {
			return err
		}
		countResult(r, it, created)
		return nil
	}
	it, created, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: mid, X: x, VendorName: aiVendor, Category: aiCat, Subject: subj, Sender: from, Text: full, DocumentID: docID})
	if err != nil {
		return err
	}
	countResult(r, it, created)
	return nil
}

func countResult(r *SyncResult, it *Item, created bool) {
	if created {
		r.createdIDs = append(r.createdIDs, it.ID)
	}
	switch {
	case !created:
		r.Skipped++
	case it.Status == StatusDuplicate:
		r.Duplicates++
	case it.Status == StatusPending:
		r.Created++
	default:
		r.Skipped++
	}
}

// SyncAll syncs every connected source, then sends notifications for new alerts.
func (s *Service) SyncAll(ctx context.Context, org string) []*SyncResult {
	out := []*SyncResult{}
	if c := s.connection(org, "synergy"); c != nil && c.Enabled && c.SecretEnc != "" {
		out = append(out, s.SyncSynergy(ctx, org))
	}
	if c := s.connection(org, "gmail"); c != nil && c.Enabled && c.SecretEnc != "" {
		out = append(out, s.SyncGmail(ctx, org))
	}
	s.NotifyAlerts(org)
	return out
}

// ---------- approve ----------

// ApproveInput are optional choices at approval.
type ApproveInput struct {
	BankAccountID string `json:"bank_account_id"`
	PaymentMethod string `json:"payment_method"`
}

var categoryCodes = map[string]string{CatHosting: "6410", CatDomains: "6420", CatEmail: "6430", CatSoftware: "6400", CatOther: "6990"}

// Approve turns an inbox item into a paid (or unpaid) supplier bill.
func (s *Service) Approve(org, user, id string, in ApproveInput) (*Item, error) {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, ErrNotFound
	}
	if it.Status == StatusApproved {
		return &it, nil
	}
	if it.Status == StatusDuplicate {
		return nil, invalid("this is a duplicate of another expense — approve that one")
	}
	if it.Kind == KindDeclined {
		return nil, invalid("a declined-payment notice is not an expense")
	}
	if strings.TrimSpace(it.VendorName) == "" {
		return nil, invalid("choose the vendor")
	}
	if it.Total <= 0 {
		return nil, invalid("enter the amount")
	}
	if it.InvoiceDate == nil {
		return nil, invalid("enter the invoice date")
	}
	if !currency.IsSupported(it.Currency) {
		return nil, invalid("choose a supported currency")
	}
	if it.Tax < 0 || it.Tax >= it.Total {
		return nil, invalid("tax must be less than the total")
	}
	if d := s.findDuplicate(org, &it); d != nil && d.Status == StatusApproved {
		return nil, invalid("the same invoice was already approved (%s) — mark this one as a duplicate", nonEmpty(d.InvoiceNumber, d.VendorName))
	}
	s.convert(org, &it, false)
	if it.FxRate <= 0 {
		return nil, invalid("no exchange rate for %s → %s — set a manual rate in Invoice settings → Exchange rates", it.Currency, it.HomeCurrency)
	}
	if s.Home(org) != it.HomeCurrency {
		s.convert(org, &it, true)
	}
	// Finance vendor + account.
	accID, err := s.Fin.EnsureExpenseAccount(org, categoryCodes[it.Category], CategoryLabels[nonEmpty(it.Category, CatOther)])
	if err != nil {
		return nil, err
	}
	vendorEmail := ""
	var v *Vendor
	if it.VendorID != "" {
		v = s.vendorByID(org, it.VendorID)
	}
	finVendor := ""
	if v != nil && s.Fin.VendorExists(org, v.FinanceVendor) {
		finVendor = v.FinanceVendor
	} else {
		if v != nil && len(v.Senders) > 0 && strings.Contains(v.Senders[0], "@") {
			vendorEmail = v.Senders[0]
		}
		finVendor, err = s.Fin.EnsureVendorNamed(org, it.VendorName, vendorEmail, accID)
		if err != nil {
			return nil, err
		}
		if v != nil {
			s.DB.Model(&Vendor{}).Where("id = ?", v.ID).Update("finance_vendor_id", finVendor)
		}
	}
	now := s.now()
	upd := map[string]any{"status": StatusApproved, "approved_at": now, "approved_by": user, "updated_at": now,
		"home_currency": it.HomeCurrency, "fx_rate": it.FxRate, "fx_source": it.FxSource, "fx_date": it.FxDate,
		"home_total": it.HomeTotal, "home_tax": it.HomeTax, "home_net": it.HomeNet, "net": it.Net}

	// An existing bill for the same supplier invoice (entered by hand in Bills) is linked, not duplicated.
	if ref := strings.TrimSpace(it.InvoiceNumber); ref != "" {
		var bill finance.Bill
		if s.DB.Where("organization_id = ? AND vendor_id = ? AND LOWER(reference) = LOWER(?) AND status NOT IN ?", org, finVendor, ref, []string{"void", "cancelled"}).First(&bill).Error == nil {
			upd["bill_id"], upd["bill_linked"] = bill.ID, true
			if err := s.DB.Model(&Item{}).Where("id = ?", it.ID).Updates(upd).Error; err != nil {
				return nil, err
			}
			return s.Get(org, it.ID)
		}
	}

	desc := nonEmpty(it.Service, it.VendorName)
	if it.PeriodStart != nil && it.PeriodEnd != nil {
		desc += fmt.Sprintf(" (%s – %s)", it.PeriodStart.Format("2 Jan 2006"), it.PeriodEnd.Format("2 Jan 2006"))
	}
	rate := 0.0
	if it.HomeNet > 0 && it.HomeTax > 0 {
		rate = math.Round(it.HomeTax/it.HomeNet*100*10000) / 10000
	}
	notes := "Imported by Expenses (" + it.Source + ")."
	if it.Currency != it.HomeCurrency {
		notes += fmt.Sprintf(" Original: %s %.2f at %s %s per %s (%s rate).", it.Currency, it.Total, trimFloat(it.FxRate), it.HomeCurrency, it.Currency, it.FxSource)
	}
	if it.TaxNote != "" {
		notes += " " + it.TaxNote + "."
	}
	billDate := it.InvoiceDate.Format(dayFmt)
	due := billDate
	if it.PaidDate != nil && it.PaidDate.After(*it.InvoiceDate) {
		due = it.PaidDate.Format(dayFmt)
	}
	bill, err := s.Fin.CreateBill(org, user, finance.BillInput{VendorID: finVendor, Reference: clip(it.InvoiceNumber, 120), BillDate: billDate, DueDate: due, Notes: notes,
		LineItems: []finance.BillLineInput{{Description: clip(desc, 250), AccountID: accID, Quantity: 1, UnitPrice: it.HomeNet, TaxRate: rate}}})
	if err != nil {
		return nil, err
	}
	if it.Paid {
		pd := billDate
		if it.PaidDate != nil {
			pd = it.PaidDate.Format(dayFmt)
		}
		method := nonEmpty(in.PaymentMethod, "card")
		if _, err := s.Fin.RecordBillPayment(org, user, bill.ID, finance.PaymentInput{Amount: bill.Total, PaymentDate: pd, PaymentMethod: method,
			BankAccountID: in.BankAccountID, Reference: it.InvoiceNumber, Notes: "Paid to " + it.VendorName}); err != nil {
			_ = s.Fin.DeleteBill(org, user, bill.ID)
			return nil, err
		}
	}
	upd["bill_id"], upd["bill_linked"] = bill.ID, false
	if err := s.DB.Model(&Item{}).Where("id = ?", it.ID).Updates(upd).Error; err != nil {
		return nil, err
	}
	return s.Get(org, it.ID)
}

func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

// Unapprove removes the bill (and its payments) and returns the item to the inbox.
func (s *Service) Unapprove(org, user, id string) (*Item, error) {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, ErrNotFound
	}
	if it.Status != StatusApproved {
		return nil, invalid("this expense is not approved")
	}
	if it.BillID != "" && !it.BillLinked {
		b, err := s.Fin.GetBill(org, it.BillID)
		if err == nil && b != nil {
			for _, p := range b.Payments {
				if _, err := s.Fin.DeleteBillPayment(org, user, b.ID, p.ID); err != nil {
					return nil, err
				}
			}
			if err := s.Fin.DeleteBill(org, user, b.ID); err != nil {
				return nil, err
			}
		}
	}
	s.DB.Model(&Item{}).Where("id = ?", it.ID).Updates(map[string]any{"status": StatusPending, "bill_id": "", "bill_linked": false, "approved_at": nil, "approved_by": "", "updated_at": s.now()})
	return s.Get(org, id)
}

// SetStatus ignores / restores an inbox item.
func (s *Service) SetStatus(org, id, status, note string) (*Item, error) {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, ErrNotFound
	}
	if it.Status == StatusApproved {
		return nil, invalid("undo the approval first")
	}
	upd := map[string]any{"status": status, "updated_at": s.now()}
	if status == StatusPending {
		upd["duplicate_of"] = ""
	}
	if note != "" {
		upd["notes"] = note
	}
	s.DB.Model(&Item{}).Where("id = ?", it.ID).Updates(upd)
	return s.Get(org, id)
}

// Delete removes an item that is not approved.
func (s *Service) Delete(org, id string) error {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return ErrNotFound
	}
	if it.Status == StatusApproved {
		return invalid("undo the approval first")
	}
	s.DB.Where("item_id = ?", it.ID).Delete(&Allocation{})
	return s.DB.Delete(&it).Error
}

// ItemInput edits an item (pointers: only the fields sent).
type ItemInput struct {
	VendorID      *string  `json:"vendor_id"`
	VendorName    *string  `json:"vendor_name"`
	InvoiceNumber *string  `json:"invoice_number"`
	InvoiceDate   *string  `json:"invoice_date"`
	PeriodStart   *string  `json:"period_start"`
	PeriodEnd     *string  `json:"period_end"`
	Service       *string  `json:"service"`
	Description   *string  `json:"description"`
	Category      *string  `json:"category"`
	Currency      *string  `json:"currency"`
	Total         *float64 `json:"total"`
	Tax           *float64 `json:"tax"`
	FxRate        *float64 `json:"fx_rate"`
	Paid          *bool    `json:"paid"`
	PaidDate      *string  `json:"paid_date"`
	CustomerID    *string  `json:"customer_id"`
	Notes         *string  `json:"notes"`
}

func parseDayPtr(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(dayFmt, s)
	if err != nil {
		return nil, invalid("dates must be YYYY-MM-DD")
	}
	return &t, nil
}

// Update edits an inbox item. Approved items only accept client and notes.
func (s *Service) Update(org, id string, in ItemInput) (*Item, error) {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, ErrNotFound
	}
	if in.CustomerID != nil {
		cid := strings.TrimSpace(*in.CustomerID)
		if cid != "" && !s.customerExists(org, cid) {
			return nil, invalid("customer not found")
		}
		it.CustomerID = cid
	}
	if in.Notes != nil {
		it.Notes = strings.TrimSpace(*in.Notes)
	}
	if it.Status == StatusApproved {
		changing := in.Total != nil || in.Tax != nil || in.Currency != nil || in.InvoiceDate != nil || in.VendorID != nil || in.Category != nil || in.Paid != nil || in.FxRate != nil
		if changing {
			return nil, invalid("undo the approval to change amounts, dates, vendor or category")
		}
		it.UpdatedAt = s.now()
		if err := s.DB.Model(&Item{}).Where("id = ?", it.ID).Updates(map[string]any{"customer_id": it.CustomerID, "notes": it.Notes, "updated_at": it.UpdatedAt}).Error; err != nil {
			return nil, err
		}
		return s.Get(org, id)
	}
	if in.VendorID != nil {
		if *in.VendorID == "" {
			it.VendorID = ""
		} else if v := s.vendorByID(org, *in.VendorID); v != nil {
			it.VendorID, it.VendorName = v.ID, v.Name
			if in.Category == nil && it.Category == CatOther {
				it.Category = v.Category
			}
		} else {
			return nil, invalid("vendor not found")
		}
	}
	if in.VendorName != nil && it.VendorID == "" {
		it.VendorName = clip(strings.TrimSpace(*in.VendorName), 250)
	}
	if in.InvoiceNumber != nil {
		it.InvoiceNumber = clip(strings.TrimSpace(*in.InvoiceNumber), 120)
	}
	var err error
	if in.InvoiceDate != nil {
		if it.InvoiceDate, err = parseDayPtr(*in.InvoiceDate); err != nil {
			return nil, err
		}
	}
	if in.PeriodStart != nil {
		if it.PeriodStart, err = parseDayPtr(*in.PeriodStart); err != nil {
			return nil, err
		}
	}
	if in.PeriodEnd != nil {
		if it.PeriodEnd, err = parseDayPtr(*in.PeriodEnd); err != nil {
			return nil, err
		}
	}
	if in.PaidDate != nil {
		if it.PaidDate, err = parseDayPtr(*in.PaidDate); err != nil {
			return nil, err
		}
	}
	if in.Service != nil {
		it.Service = clip(strings.TrimSpace(*in.Service), 250)
	}
	if in.Description != nil {
		it.Description = strings.TrimSpace(*in.Description)
	}
	if in.Category != nil {
		if _, ok := CategoryLabels[*in.Category]; !ok {
			return nil, invalid("unknown category")
		}
		it.Category = *in.Category
	}
	amountsChanged := false
	if in.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Currency))
		if !currency.IsSupported(c) {
			return nil, invalid("choose a supported currency")
		}
		if c != it.Currency {
			it.Currency, it.FxRate, it.FxSource = c, 0, ""
		}
		amountsChanged = true
	}
	if in.Total != nil {
		if *in.Total < 0 || *in.Total > 1e9 {
			return nil, invalid("enter a valid amount")
		}
		it.Total, amountsChanged = r2(*in.Total), true
		it.Warnings = dropWarn(it.Warnings, "No amount found")
	}
	if in.Tax != nil {
		if *in.Tax < 0 {
			return nil, invalid("tax cannot be negative")
		}
		it.Tax, amountsChanged = r2(*in.Tax), true
		if strings.Contains(it.TaxNote, "estimated") {
			it.TaxNote = "Tax (entered)"
		}
	}
	if it.Total > 0 && it.Tax >= it.Total {
		return nil, invalid("tax must be less than the total")
	}
	if in.FxRate != nil {
		if *in.FxRate <= 0 {
			return nil, invalid("the exchange rate must be positive")
		}
		it.FxRate, it.FxSource = *in.FxRate, fxrates.SourceManual
		t := s.now()
		it.FxDate = &t
		amountsChanged = true
	}
	if in.Paid != nil {
		it.Paid = *in.Paid
	}
	if amountsChanged || it.HomeCurrency == "" {
		s.convert(org, &it, false)
	}
	it.DedupeKey = dedupeKey(it.VendorID+it.VendorName, &it)
	if it.Method != "manual" && (in.Total != nil || in.InvoiceNumber != nil || in.InvoiceDate != nil) {
		it.Confidence = 1 // a person checked it
	}
	it.UpdatedAt = s.now()
	if err := s.DB.Save(&it).Error; err != nil {
		return nil, err
	}
	return s.Get(org, id)
}

func dropWarn(w []string, prefix string) []string {
	out := []string{}
	for _, x := range w {
		if !strings.HasPrefix(x, prefix) {
			out = append(out, x)
		}
	}
	return out
}

// CreateManual adds an expense typed in by hand (lands in the inbox).
func (s *Service) CreateManual(org string, in ItemInput) (*Item, error) {
	it, _, err := s.Ingest(org, Incoming{Source: SourceManual, ExternalID: uuid.NewString(), X: Extracted{Kind: KindInvoice, Method: "manual", Confidence: 1, Currency: s.Home(org)}})
	if err != nil {
		return nil, err
	}
	out, err := s.Update(org, it.ID, in)
	if err != nil {
		s.DB.Delete(it)
		return nil, err
	}
	if out.VendorName == "" || out.Total <= 0 {
		s.DB.Delete(it)
		return nil, invalid("enter the vendor and amount")
	}
	if d := s.findDuplicate(org, out); d != nil {
		s.DB.Model(&Item{}).Where("id = ?", out.ID).Updates(map[string]any{"status": StatusDuplicate, "duplicate_of": d.ID, "notes": "Same invoice as one already in Expenses"})
		return s.Get(org, out.ID)
	}
	return out, nil
}

// SetAllocations replaces the client split of an item.
func (s *Service) SetAllocations(org, id string, rows []Allocation) (*Item, error) {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, ErrNotFound
	}
	total := 0.0
	seen := map[string]bool{}
	clean := []Allocation{}
	for _, r := range rows {
		r.CustomerID = strings.TrimSpace(r.CustomerID)
		if r.CustomerID == "" {
			continue
		}
		if seen[r.CustomerID] {
			return nil, invalid("each client once")
		}
		seen[r.CustomerID] = true
		if r.Percent <= 0 || r.Percent > 100 {
			return nil, invalid("percentages must be between 0 and 100")
		}
		if !s.customerExists(org, r.CustomerID) {
			return nil, invalid("customer not found")
		}
		total += r.Percent
		clean = append(clean, Allocation{ID: uuid.NewString(), OrganizationID: org, ItemID: id, CustomerID: r.CustomerID, Percent: r.Percent, Note: clip(r.Note, 250), CreatedAt: s.now()})
	}
	if total > 100.0001 {
		return nil, invalid("the split adds up to more than 100%%")
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("organization_id = ? AND item_id = ?", org, id).Delete(&Allocation{}).Error; err != nil {
			return err
		}
		if len(clean) > 0 {
			if err := tx.Create(&clean).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Item{}).Where("id = ?", id).Updates(map[string]any{"customer_id": "", "updated_at": s.now()}).Error
	})
	if err != nil {
		return nil, err
	}
	return s.Get(org, id)
}

func (s *Service) customerExists(org, id string) bool {
	var n int64
	s.DB.Table("customers").Where("organization_id = ? AND id = ? AND deleted_at IS NULL", org, id).Count(&n)
	return n > 0
}

func (s *Service) customerNames(org string, ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	var rows []struct{ ID, Name string }
	s.DB.Raw(`SELECT id, TRIM(COALESCE(first_name,'') || ' ' || COALESCE(last_name,'')) AS name FROM customers WHERE organization_id = ? AND id IN ?`, org, ids).Scan(&rows)
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// Get returns an item with derived fields.
func (s *Service) Get(org, id string) (*Item, error) {
	var it Item
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, ErrNotFound
	}
	items := []Item{it}
	s.decorate(org, items)
	return &items[0], nil
}

func (s *Service) decorate(org string, items []Item) {
	ids, cids, bills, docs := []string{}, []string{}, []string{}, []string{}
	for _, it := range items {
		ids = append(ids, it.ID)
		if it.CustomerID != "" {
			cids = append(cids, it.CustomerID)
		}
		if it.BillID != "" {
			bills = append(bills, it.BillID)
		}
		if it.DocumentID != "" {
			docs = append(docs, it.DocumentID)
		}
	}
	allocs := map[string][]Allocation{}
	if len(ids) > 0 {
		var al []Allocation
		s.DB.Where("organization_id = ? AND item_id IN ?", org, ids).Order("percent DESC").Find(&al)
		for _, a := range al {
			cids = append(cids, a.CustomerID)
			allocs[a.ItemID] = append(allocs[a.ItemID], a)
		}
	}
	names := s.customerNames(org, cids)
	billNo := map[string]string{}
	if len(bills) > 0 {
		var rows []struct{ ID, BillNumber string }
		s.DB.Raw("SELECT id, bill_number FROM bills WHERE organization_id = ? AND id IN ?", org, bills).Scan(&rows)
		for _, r := range rows {
			billNo[r.ID] = r.BillNumber
		}
	}
	docName := map[string]string{}
	if len(docs) > 0 {
		var rows []struct{ ID, Filename string }
		s.DB.Raw("SELECT id, filename FROM expense_documents WHERE organization_id = ? AND id IN ?", org, docs).Scan(&rows)
		for _, r := range rows {
			docName[r.ID] = r.Filename
		}
	}
	for i := range items {
		it := &items[i]
		it.CustomerName = names[it.CustomerID]
		it.BillNumber = billNo[it.BillID]
		it.DocumentName = docName[it.DocumentID]
		for _, a := range allocs[it.ID] {
			a.CustomerName = names[a.CustomerID]
			it.Allocations = append(it.Allocations, a)
		}
		if it.Warnings == nil {
			it.Warnings = []string{}
		}
	}
}

// ListFilter filters items.
type ListFilter struct {
	Status, VendorID, Category, Q, From, To, Source, CustomerID string
	Limit                                                       int
}

// List returns items (newest invoice first).
func (s *Service) List(org string, f ListFilter) ([]Item, int64) {
	q := s.DB.Model(&Item{}).Where("organization_id = ?", org)
	switch f.Status {
	case "", "all":
	case "inbox":
		q = q.Where("status = ?", StatusPending)
	default:
		q = q.Where("status IN ?", strings.Split(f.Status, ","))
	}
	if f.VendorID != "" {
		q = q.Where("vendor_id = ?", f.VendorID)
	}
	if f.Category != "" {
		q = q.Where("category = ?", f.Category)
	}
	if f.Source != "" {
		q = q.Where("source = ?", f.Source)
	}
	if f.CustomerID != "" {
		q = q.Where("customer_id = ? OR id IN (SELECT item_id FROM expense_allocations WHERE organization_id = ? AND customer_id = ?)", f.CustomerID, org, f.CustomerID)
	}
	if f.From != "" {
		q = q.Where("invoice_date >= ?::date", f.From)
	}
	if f.To != "" {
		q = q.Where("invoice_date <= ?::date", f.To)
	}
	if t := strings.TrimSpace(f.Q); t != "" {
		like := "%" + strings.ToLower(t) + "%"
		q = q.Where("LOWER(vendor_name) LIKE ? OR LOWER(invoice_number) LIKE ? OR LOWER(service) LIKE ? OR LOWER(subject) LIKE ?", like, like, like, like)
	}
	var total int64
	q.Count(&total)
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	items := []Item{}
	q.Order("invoice_date DESC NULLS LAST, created_at DESC").Limit(f.Limit).Find(&items)
	s.decorate(org, items)
	return items, total
}

// approvedCharges loads approved items as charges (optionally one vendor).
func (s *Service) approvedCharges(org, vendorID string) ([]Charge, []Item) {
	q := s.DB.Where("organization_id = ? AND status = ?", org, StatusApproved)
	if vendorID != "" {
		q = q.Where("vendor_id = ?", vendorID)
	}
	var items []Item
	q.Order("invoice_date").Find(&items)
	cs := make([]Charge, 0, len(items))
	for _, it := range items {
		cs = append(cs, ChargeOf(it))
	}
	return cs, items
}

// Renewals lists active renewals with days left.
func (s *Service) Renewals(org string, within int) []Renewal {
	out := []Renewal{}
	q := s.DB.Where("organization_id = ? AND active", org)
	today := Day(s.now())
	if within > 0 {
		q = q.Where("expires_on IS NOT NULL AND expires_on <= ?", today.AddDate(0, 0, within))
	}
	q.Order("expires_on NULLS LAST, name").Find(&out)
	cids := []string{}
	for _, r := range out {
		if r.CustomerID != "" {
			cids = append(cids, r.CustomerID)
		}
	}
	names := s.customerNames(org, cids)
	for i := range out {
		out[i].CustomerName = names[out[i].CustomerID]
		if out[i].ExpiresOn != nil {
			d := int(math.Round(Day(*out[i].ExpiresOn).Sub(today).Hours() / 24))
			out[i].DaysLeft = &d
		}
	}
	return out
}

// RenewalInput edits / creates a renewal.
type RenewalInput struct {
	Name       *string  `json:"name"`
	Kind       *string  `json:"kind"`
	ExpiresOn  *string  `json:"expires_on"`
	AutoRenew  *bool    `json:"auto_renew"`
	Cost       *float64 `json:"cost"`
	Currency   *string  `json:"currency"`
	Price      *float64 `json:"price"`
	CustomerID *string  `json:"customer_id"`
	Active     *bool    `json:"active"`
}

// SaveRenewal creates (id "") or updates a renewal.
func (s *Service) SaveRenewal(org, id string, in RenewalInput) (*Renewal, error) {
	var r Renewal
	if id == "" {
		r = Renewal{ID: uuid.NewString(), OrganizationID: org, Source: SourceManual, ExternalID: uuid.NewString(), Kind: "other", Active: true, Currency: s.Home(org), CreatedAt: s.now()}
	} else if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&r).Error != nil {
		return nil, ErrNotFound
	}
	if in.Name != nil {
		r.Name = clip(strings.TrimSpace(*in.Name), 250)
	}
	if r.Name == "" {
		return nil, invalid("enter a name")
	}
	if in.Kind != nil {
		switch *in.Kind {
		case "domain", "hosting", "ssl", "other":
			r.Kind = *in.Kind
		default:
			return nil, invalid("unknown kind")
		}
	}
	if in.ExpiresOn != nil {
		d, err := parseDayPtr(*in.ExpiresOn)
		if err != nil {
			return nil, err
		}
		r.ExpiresOn = d
	}
	if in.AutoRenew != nil {
		r.AutoRenew = *in.AutoRenew
	}
	if in.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Currency))
		if !currency.IsSupported(c) {
			return nil, invalid("choose a supported currency")
		}
		r.Currency = c
	}
	if in.Cost != nil {
		if *in.Cost < 0 {
			return nil, invalid("cost cannot be negative")
		}
		v := r2(*in.Cost)
		r.Cost, r.CostSource = &v, "manual"
		if *in.Cost == 0 {
			r.Cost = nil
		}
	}
	if in.Price != nil {
		v := r2(*in.Price)
		r.Price = &v
		if *in.Price <= 0 {
			r.Price = nil
		}
	}
	if in.CustomerID != nil {
		c := strings.TrimSpace(*in.CustomerID)
		if c != "" && !s.customerExists(org, c) {
			return nil, invalid("customer not found")
		}
		r.CustomerID = c
	}
	if in.Active != nil {
		r.Active = *in.Active
	}
	r.UpdatedAt = s.now()
	if err := s.DB.Save(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

// ---------- dashboard ----------

// Dashboard is everything the Expenses page shows.
type Dashboard struct {
	Currency      string            `json:"currency"`
	Decimals      int               `json:"decimals"`
	Today         string            `json:"today"`
	KPIs          map[string]any    `json:"kpis"`
	Months        []map[string]any  `json:"months"`
	ByVendor      []map[string]any  `json:"by_vendor"`
	ByCategory    []map[string]any  `json:"by_category"`
	Subscriptions []Subscription    `json:"subscriptions"`
	Renewals      []Renewal         `json:"renewals"`
	Alerts        []Alert           `json:"alerts"`
	Forecast      []ForecastMonth   `json:"forecast"`
	Inbox         map[string]int64  `json:"inbox"`
	Balance       map[string]any    `json:"balance"`
	Sources       map[string]any    `json:"sources"`
	Range         map[string]string `json:"range"`
}

func fmtMoney(v float64, cur string) string {
	d := currency.Decimals(cur)
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.*f", d, v)
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := cur + " " + b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

func sumNet(cs []Charge, from, to time.Time) (float64, int) {
	var v float64
	n := 0
	for _, c := range cs {
		if !c.Date.Before(from) && !c.Date.After(to) {
			v += c.Net
			n++
		}
	}
	return r2(v), n
}

func pctChange(cur, prev float64) *float64 {
	if prev == 0 {
		if cur == 0 {
			z := 0.0
			return &z
		}
		return nil
	}
	v := math.Round((cur-prev)/math.Abs(prev)*1000) / 10
	return &v
}

// DashboardFor builds the dashboard; from/to bound the vendor/category breakdowns
// (default: the last 12 months).
func (s *Service) DashboardFor(org string, today time.Time, from, to string) *Dashboard {
	s.EnsureVendors(org)
	today = Day(today)
	home := s.Home(org)
	charges, _ := s.approvedCharges(org, "")
	ms := monthStart(today)
	thisM, nThis := sumNet(charges, ms, today)
	lastStart := ms.AddDate(0, -1, 0)
	lastM, nLast := sumNet(charges, lastStart, ms.AddDate(0, 0, -1))
	ldom := today.Day()
	lastEnd := lastStart.AddDate(0, 0, ldom-1)
	if lastEnd.Month() != lastStart.Month() {
		lastEnd = ms.AddDate(0, 0, -1)
	}
	lastMTD, _ := sumNet(charges, lastStart, lastEnd)
	ys := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	ytd, nYtd := sumNet(charges, ys, today)
	pys := ys.AddDate(-1, 0, 0)
	pytd, _ := sumNet(charges, pys, today.AddDate(-1, 0, 0))

	subs := DetectSubscriptions(charges, today)
	var annual, monthly float64
	activeSubs := 0
	for _, sb := range subs {
		if sb.Status != "lapsed" {
			annual += sb.Annualised
			activeSubs++
		}
	}
	monthly = r2(annual / 12)
	renewals := s.Renewals(org, 0)
	toHome := s.toHome(org)
	fc := Forecast(charges, subs, renewals, today, 3, toHome)
	var fcTotal float64
	for _, f := range fc {
		fcTotal += f.Total
	}

	syn := s.connection(org, "synergy")
	var bal *float64
	low := 20.0
	balInfo := map[string]any{"available": false}
	if syn != nil && syn.Balance != nil {
		bal = syn.Balance
		if syn.Settings.LowBalance > 0 {
			low = syn.Settings.LowBalance
		}
		balInfo = map[string]any{"available": true, "amount": *syn.Balance, "currency": "AUD", "as_of": syn.BalanceAt, "low_balance": low,
			"source": map[bool]string{true: "api", false: "email"}[syn.SecretEnc != ""]}
	}
	var declined []Item
	s.DB.Where("organization_id = ? AND kind = ? AND invoice_date >= ?", org, KindDeclined, today.AddDate(0, 0, -30)).Find(&declined)
	alerts := Alerts(AlertInput{Today: today, Home: home, Charges: charges, Subs: subs, Declined: declined, Renewals: renewals, Balance: bal, LowBalance: low, Fmt: fmtMoney})
	alerts = s.withoutDismissed(org, alerts)

	// Range for breakdowns.
	rFrom := ms.AddDate(0, -11, 0)
	rTo := today
	if t, err := time.Parse(dayFmt, from); err == nil {
		rFrom = t
	}
	if t, err := time.Parse(dayFmt, to); err == nil {
		rTo = t
	}
	byV := map[string]*struct {
		id, name string
		net      float64
		n        int
		last     time.Time
	}{}
	byC := map[string]float64{}
	var rangeTotal float64
	for _, c := range charges {
		if c.Date.Before(rFrom) || c.Date.After(rTo) {
			continue
		}
		k := c.VendorID + "|" + c.Vendor
		v := byV[k]
		if v == nil {
			v = &struct {
				id, name string
				net      float64
				n        int
				last     time.Time
			}{id: c.VendorID, name: c.Vendor}
			byV[k] = v
		}
		v.net += c.Net
		v.n++
		if c.Date.After(v.last) {
			v.last = c.Date
		}
		byC[nonEmpty(c.Category, CatOther)] += c.Net
		rangeTotal += c.Net
	}
	vend := []map[string]any{}
	for _, v := range byV {
		vend = append(vend, map[string]any{"vendor_id": v.id, "vendor": v.name, "amount": r2(v.net), "count": v.n, "last_date": v.last.Format(dayFmt),
			"share_pct": shareOf(v.net, rangeTotal)})
	}
	sort.Slice(vend, func(i, j int) bool { return vend[i]["amount"].(float64) > vend[j]["amount"].(float64) })
	cats := []map[string]any{}
	for _, k := range Categories {
		if v, ok := byC[k]; ok && math.Abs(v) > 0.0001 {
			cats = append(cats, map[string]any{"key": k, "label": CategoryLabels[k], "amount": r2(v), "share_pct": shareOf(v, rangeTotal)})
		}
	}
	months := []map[string]any{}
	for m := ms.AddDate(0, -11, 0); !m.After(ms); m = m.AddDate(0, 1, 0) {
		end := m.AddDate(0, 1, -1)
		v, n := sumNet(charges, m, end)
		row := map[string]any{"month": m.Format("2006-01"), "amount": v, "count": n, "partial": end.After(today)}
		for _, k := range Categories {
			var cv float64
			for _, c := range charges {
				if c.Category == k && !c.Date.Before(m) && !c.Date.After(end) {
					cv += c.Net
				}
			}
			row[k] = r2(cv)
		}
		months = append(months, row)
	}
	inbox := map[string]int64{}
	var rows []struct {
		Status string
		N      int64
	}
	s.DB.Raw("SELECT status, COUNT(*) AS n FROM expense_items WHERE organization_id = ? GROUP BY status", org).Scan(&rows)
	for _, r := range rows {
		inbox[r.Status] = r.N
	}
	var renew30 int
	var renew30Cost float64
	upcoming := []Renewal{}
	for _, r := range renewals {
		if r.DaysLeft != nil && *r.DaysLeft >= -45 && *r.DaysLeft <= 90 {
			upcoming = append(upcoming, r)
			if *r.DaysLeft >= 0 && *r.DaysLeft <= 30 {
				renew30++
				if r.Cost != nil {
					renew30Cost += toHome(*r.Cost, r.Currency)
				}
			}
		}
	}
	sources := map[string]any{}
	for _, k := range []string{"synergy", "gmail"} {
		c := s.connection(org, k)
		sources[k] = map[string]any{"connected": c != nil && c.SecretEnc != "", "last_sync_at": nil, "last_status": ""}
		if c != nil {
			sources[k] = map[string]any{"connected": c.SecretEnc != "", "last_sync_at": c.LastSyncAt, "last_status": c.LastStatus, "account": c.Account}
		}
	}
	return &Dashboard{
		Currency: home, Decimals: currency.Decimals(home), Today: today.Format(dayFmt),
		KPIs: map[string]any{
			"this_month": thisM, "this_month_count": nThis, "last_month": lastM, "last_month_count": nLast, "last_month_to_date": lastMTD,
			"this_month_change_pct": pctChange(thisM, lastMTD), "ytd": ytd, "ytd_count": nYtd, "prev_ytd": pytd, "ytd_change_pct": pctChange(ytd, pytd),
			"subscriptions": activeSubs, "subscriptions_annual": r2(annual), "subscriptions_monthly": monthly,
			"forecast_3m": r2(fcTotal), "renewals_30d": renew30, "renewals_30d_cost": r2(renew30Cost),
			"alerts": len(alerts), "inbox": inbox[StatusPending],
		},
		Months: months, ByVendor: vend, ByCategory: cats, Subscriptions: subs, Renewals: upcoming, Alerts: alerts, Forecast: fc, Inbox: inbox,
		Balance: balInfo, Sources: sources, Range: map[string]string{"from": rFrom.Format(dayFmt), "to": rTo.Format(dayFmt)},
	}
}

func shareOf(v, total float64) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(v/total*1000) / 10
}

func (s *Service) withoutDismissed(org string, alerts []Alert) []Alert {
	var keys []string
	s.DB.Model(&AlertLog{}).Where("organization_id = ? AND dismissed_at IS NOT NULL", org).Pluck("key", &keys)
	if len(keys) == 0 {
		return alerts
	}
	d := map[string]bool{}
	for _, k := range keys {
		d[k] = true
	}
	out := []Alert{}
	for _, a := range alerts {
		if !d[a.Key] {
			out = append(out, a)
		}
	}
	return out
}

// Dismiss hides an alert.
func (s *Service) Dismiss(org, key string) error {
	now := s.now()
	var l AlertLog
	if s.DB.Where("organization_id = ? AND key = ?", org, key).First(&l).Error == nil {
		return s.DB.Model(&AlertLog{}).Where("id = ?", l.ID).Update("dismissed_at", now).Error
	}
	return s.DB.Create(&AlertLog{ID: uuid.NewString(), OrganizationID: org, Key: clip(key, 250), Type: strings.Split(key, ":")[0], DismissedAt: &now, CreatedAt: now}).Error
}

// NotifyAlerts sends a notification for each alert not notified before.
func (s *Service) NotifyAlerts(org string) int {
	d := s.DashboardFor(org, s.now().In(locOf(s.DB, org)), "", "")
	sent := 0
	for _, a := range d.Alerts {
		var n int64
		s.DB.Model(&AlertLog{}).Where("organization_id = ? AND key = ?", org, a.Key).Count(&n)
		if n > 0 {
			continue
		}
		if err := s.DB.Create(&AlertLog{ID: uuid.NewString(), OrganizationID: org, Key: clip(a.Key, 250), Type: a.Type, CreatedAt: s.now()}).Error; err != nil {
			continue
		}
		sev := map[string]string{"danger": notify.Error, "warning": notify.Warning, "info": notify.Info}[a.Severity]
		if _, err := notify.Send(s.DB, org, notify.Admins(), notify.Message{Type: "expenses." + a.Type, Severity: sev, Title: a.Title, Body: a.Body,
			Link: a.Link, EntityType: "expense_alert", EntityID: clip(a.Key, 250), Dedupe: true}); err == nil {
			sent++
		}
	}
	return sent
}

func locOf(db *gorm.DB, org string) *time.Location {
	if tz := orgprefs.Get(db, org).Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.UTC
}

// ---------- margin per client ----------

// ClientMargin is revenue vs attributed costs for one client.
type ClientMargin struct {
	CustomerID    string   `json:"customer_id"`
	Customer      string   `json:"customer"`
	Revenue       float64  `json:"revenue"` // invoices issued, ex tax, home currency
	Invoices      int      `json:"invoices"`
	Costs         float64  `json:"costs"` // approved expenses attributed, ex tax, home
	CostItems     int      `json:"cost_items"`
	Margin        float64  `json:"margin"`
	MarginPct     *float64 `json:"margin_pct"`
	RenewalCosts  float64  `json:"renewal_costs_12m"` // expected renewals in the next 12 months (home)
	RenewalPrices float64  `json:"renewal_prices_12m"`
	Unconverted   int      `json:"unconverted_invoices"` // invoices in another currency without a locked rate
}

// ClientMargins compares what each client was billed with what their
// attributed expenses cost, for invoices / expenses dated in [from, to].
func (s *Service) ClientMargins(org string, from, to time.Time) []ClientMargin {
	home := s.Home(org)
	by := map[string]*ClientMargin{}
	get := func(id string) *ClientMargin {
		m := by[id]
		if m == nil {
			m = &ClientMargin{CustomerID: id}
			by[id] = m
		}
		return m
	}
	// Costs first: only clients with attributed costs are listed.
	var items []Item
	s.DB.Where("organization_id = ? AND status = ? AND invoice_date BETWEEN ? AND ?", org, StatusApproved, from, to).Find(&items)
	ids := []string{}
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	allocs := map[string][]Allocation{}
	if len(ids) > 0 {
		var al []Allocation
		s.DB.Where("organization_id = ? AND item_id IN ?", org, ids).Find(&al)
		for _, a := range al {
			allocs[a.ItemID] = append(allocs[a.ItemID], a)
		}
	}
	for _, it := range items {
		if al := allocs[it.ID]; len(al) > 0 {
			for _, a := range al {
				m := get(a.CustomerID)
				m.Costs += it.HomeNet * a.Percent / 100
				m.CostItems++
			}
		} else if it.CustomerID != "" {
			m := get(it.CustomerID)
			m.Costs += it.HomeNet
			m.CostItems++
		}
	}
	toHome := s.toHome(org)
	today := Day(s.now())
	var rens []Renewal
	s.DB.Where("organization_id = ? AND active AND customer_id <> '' AND expires_on BETWEEN ? AND ?", org, today, today.AddDate(1, 0, 0)).Find(&rens)
	for _, r := range rens {
		m := get(r.CustomerID)
		if r.Cost != nil {
			m.RenewalCosts += toHome(*r.Cost, r.Currency)
		}
		if r.Price != nil {
			m.RenewalPrices += toHome(*r.Price, r.Currency)
		}
	}
	if len(by) == 0 {
		return []ClientMargin{}
	}
	cids := []string{}
	for id := range by {
		cids = append(cids, id)
	}
	var invs []struct {
		CustomerID, Currency       string
		Total, TaxTotal, TotalBase float64
	}
	s.DB.Raw(`SELECT customer_id, UPPER(COALESCE(NULLIF(currency,''), ?)) AS currency, total, tax_total, COALESCE(total_base,0) AS total_base
		FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND doc_type = 'invoice' AND status NOT IN ('draft','void')
		  AND customer_id IN ? AND issue_date BETWEEN ? AND ?`, home, org, cids, from, to).Scan(&invs)
	for _, iv := range invs {
		m := get(iv.CustomerID)
		net := iv.Total - iv.TaxTotal
		switch {
		case iv.Currency == home:
		case iv.TotalBase > 0 && iv.Total > 0:
			net = net * iv.TotalBase / iv.Total
		default:
			m.Unconverted++
			continue
		}
		m.Revenue += net
		m.Invoices++
	}
	names := s.customerNames(org, cids)
	out := []ClientMargin{}
	for _, m := range by {
		m.Customer = nonEmpty(names[m.CustomerID], "(deleted customer)")
		m.Revenue, m.Costs = fxrates.Round(m.Revenue, home), fxrates.Round(m.Costs, home)
		m.RenewalCosts, m.RenewalPrices = r2(m.RenewalCosts), r2(m.RenewalPrices)
		m.Margin = fxrates.Round(m.Revenue-m.Costs, home)
		if m.Revenue != 0 {
			p := math.Round(m.Margin/m.Revenue*1000) / 10
			m.MarginPct = &p
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Margin != out[j].Margin {
			return out[i].Margin < out[j].Margin
		}
		return out[i].Customer < out[j].Customer
	})
	return out
}

// ---------- scheduler ----------

// ScheduleInput sets the daily auto-sync.
type ScheduleInput struct {
	AutoSync bool `json:"auto_sync"`
	Hour     int  `json:"hour"`
}

// SaveSchedule stores the auto-sync choice.
func (s *Service) SaveSchedule(org string, in ScheduleInput) (*Connection, error) {
	if in.Hour < 0 || in.Hour > 23 {
		return nil, invalid("choose an hour between 0 and 23")
	}
	c := s.connectionOrNew(org, "schedule")
	c.Enabled, c.Settings.AutoSync, c.Settings.Hour, c.UpdatedAt = in.AutoSync, in.AutoSync, in.Hour, s.now()
	return c, s.DB.Save(c).Error
}

// due reports whether an organisation's daily sync should run now.
func due(c Connection, now time.Time, loc *time.Location) bool {
	if !c.Settings.AutoSync {
		return false
	}
	local := now.In(loc)
	if local.Hour() < c.Settings.Hour {
		return false
	}
	if c.LastSyncAt == nil {
		return true
	}
	last := c.LastSyncAt.In(loc)
	return last.Format(dayFmt) != local.Format(dayFmt)
}

// RunScheduled runs due daily syncs once. A Postgres advisory lock per
// organisation makes sure only one instance (e.g. of several Cloud Run
// containers) syncs it.
func (s *Service) RunScheduled(ctx context.Context) int {
	var cons []Connection
	s.DB.Where("kind = ? AND enabled", "schedule").Find(&cons)
	ran := 0
	for _, c := range cons {
		loc := locOf(s.DB, c.OrganizationID)
		if !due(c, s.now(), loc) {
			continue
		}
		ok := s.withLock(ctx, "expenses-sync:"+c.OrganizationID, func(db *gorm.DB) {
			// Re-check after taking the lock: another instance may have just run it.
			var fresh Connection
			if db.Where("id = ?", c.ID).First(&fresh).Error != nil || !due(fresh, s.now(), loc) {
				return
			}
			s.SyncAll(ctx, c.OrganizationID)
			now := s.now()
			db.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"last_sync_at": now, "last_status": "ok", "updated_at": now})
			ran++
		})
		if !ok {
			log.Printf("expenses: sync for %s is running on another instance", c.OrganizationID)
		}
	}
	return ran
}

// withLock runs fn while holding a session-level advisory lock on one
// pooled connection; returns false when another session holds it.
func (s *Service) withLock(ctx context.Context, key string, fn func(db *gorm.DB)) bool {
	got := false
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", key)
		fn(s.DB)
		return nil
	})
	if err != nil {
		log.Printf("expenses: lock %s: %v", key, err)
		return false
	}
	return got
}

// StartScheduler checks every 10 minutes for due syncs (EXPENSES_SCHEDULER=off disables).
func (s *Service) StartScheduler(ctx context.Context) {
	if strings.EqualFold(os.Getenv("EXPENSES_SCHEDULER"), "off") {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("expenses: scheduler stopped: %v", r)
			}
		}()
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		time.Sleep(30 * time.Second)
		for {
			s.RunScheduled(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
