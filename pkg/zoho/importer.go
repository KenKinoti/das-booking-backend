package zoho

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Outcome of writing one record.
const (
	outCreated   = "created"
	outUpdated   = "updated"
	outUnchanged = "unchanged"
	outSkipped   = "skipped"
)

// Importer writes normalised records into the organisation's data. One
// importer is used per job run; it is not safe for concurrent use.
type Importer struct {
	DB        *gorm.DB
	Org       string
	User      string
	JobID     string
	RefSource string // "zoho" (API or Zoho export files) | "csv" (other tools)
	Opts      Options
	Home      string // organisation home currency
	SrcBase   string // base currency in the source system
	DayFirst  bool   // ambiguous dates in files are dd/mm
	Now       func() time.Time

	Counts  map[string]*Count
	OnError func(JobError)

	contactLocal map[string]string   // source contact id → customer id
	contacts     map[string]*Contact // source contact id → contact
	quoteLocal   map[string]string   // source estimate id → local quote id
	numbers      map[string][]string // doc type → numbers seen
	recon        map[string]*Reconciliation
	reconIDs     map[string]bool // local invoice ids in the reconciliation
	flags        int
	loaded       bool
	byEmail      map[string]string // existing customers by email
	byName       map[string]string // existing customers by lower name
	custLabel    map[string]string
	docByNumber  map[string]string // type|number → id
	linked       map[string]bool   // local ids already linked by a ref
	emailsTaken  map[string]bool
}

func (im *Importer) now() time.Time {
	if im.Now != nil {
		return im.Now()
	}
	return time.Now()
}

func (im *Importer) count(ent string) *Count {
	if im.Counts == nil {
		im.Counts = map[string]*Count{}
	}
	c := im.Counts[ent]
	if c == nil {
		c = &Count{}
		im.Counts[ent] = c
	}
	return c
}

func (im *Importer) tally(ent, outcome string) {
	c := im.count(ent)
	c.Done++
	switch outcome {
	case outCreated:
		c.Created++
	case outUpdated:
		c.Updated++
	case outUnchanged:
		c.Unchanged++
	case outSkipped:
		c.Skipped++
	default:
		c.Failed++
	}
}

func (im *Importer) fail(ent, ext, label string, row int, err error) {
	im.tally(ent, "failed")
	im.report(JobError{Entity: ent, ExternalID: ext, Label: label, Severity: "error", Message: err.Error(), Row: row})
}

func (im *Importer) warn(ent, ext, label, msg string) {
	im.report(JobError{Entity: ent, ExternalID: ext, Label: label, Severity: "warning", Message: msg})
}

func (im *Importer) report(e JobError) {
	if im.OnError != nil {
		e.JobID, e.OrganizationID, e.CreatedAt = im.JobID, im.Org, im.now()
		if len(e.Label) > 250 {
			e.Label = e.Label[:250]
		}
		im.OnError(e)
	}
}

// cut truncates to n runes (column sizes).
func cut(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ---------- refs ----------

func (im *Importer) ref(entity, ext string) *ExternalRef {
	var r ExternalRef
	if err := im.DB.Where("organization_id = ? AND source = ? AND entity = ? AND external_id = ?", im.Org, im.RefSource, entity, ext).Take(&r).Error; err != nil {
		return nil
	}
	return &r
}

func (im *Importer) saveRef(tx *gorm.DB, old *ExternalRef, entity, ext, local, label, hash, modified string, created bool) error {
	now := im.now()
	if old != nil {
		return tx.Model(&ExternalRef{}).Where("id = ?", old.ID).Updates(map[string]interface{}{
			"local_id": local, "label": cut(label, 250), "hash": hash, "modified": cut(modified, 60), "last_job_id": im.JobID, "last_synced": now.Add(time.Second),
		}).Error
	}
	r := ExternalRef{OrganizationID: im.Org, Source: im.RefSource, Entity: entity, ExternalID: cut(ext, 150), LocalID: local, Label: cut(label, 250),
		Hash: hash, Modified: cut(modified, 60), JobID: im.JobID, LastJobID: im.JobID, Created: created, LastSynced: now.Add(time.Second), CreatedAt: now}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}, {Name: "source"}, {Name: "entity"}, {Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"local_id", "label", "hash", "modified", "last_job_id", "last_synced"}),
	}).Create(&r).Error
}

// locallyEdited: the local record changed after the last sync.
func (im *Importer) locallyEdited(table, id string, r *ExternalRef) bool {
	var row struct{ UpdatedAt time.Time }
	if err := im.DB.Table(table).Select("updated_at").Where("id = ?", id).Take(&row).Error; err != nil {
		return false
	}
	return row.UpdatedAt.After(r.LastSynced.Add(2 * time.Second))
}

func (im *Importer) exists(table, id string) bool {
	var n int64
	im.DB.Table(table).Where("id = ? AND organization_id = ? AND deleted_at IS NULL", id, im.Org).Count(&n)
	return n > 0
}

// ---------- matching existing data ----------

func (im *Importer) load() {
	if im.loaded {
		return
	}
	im.loaded = true
	im.contactLocal, im.contacts, im.quoteLocal = map[string]string{}, map[string]*Contact{}, map[string]string{}
	im.numbers, im.recon, im.reconIDs = map[string][]string{}, map[string]*Reconciliation{}, map[string]bool{}
	im.byEmail, im.byName, im.custLabel, im.docByNumber = map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	im.linked, im.emailsTaken = map[string]bool{}, map[string]bool{}
	if im.Home == "" {
		im.Home = strings.ToUpper(orgprefs.Currency(im.DB, im.Org))
	}
	var refs []ExternalRef
	im.DB.Select("local_id").Where("organization_id = ?", im.Org).Find(&refs)
	for _, r := range refs {
		im.linked[r.LocalID] = true
	}
	var custs []struct {
		ID, FirstName, LastName, Email, CompanyName string
	}
	im.DB.Table("customers").Select("id, first_name, last_name, email, company_name").Where("organization_id = ? AND deleted_at IS NULL", im.Org).Order("created_at ASC").Scan(&custs)
	for _, c := range custs {
		name := strings.TrimSpace(c.FirstName + " " + c.LastName)
		im.custLabel[c.ID] = name
		if e := lowerTrim(c.Email); e != "" {
			im.emailsTaken[e] = true
			if _, ok := im.byEmail[e]; !ok && !im.linked[c.ID] {
				im.byEmail[e] = c.ID
			}
		}
		if im.linked[c.ID] {
			continue
		}
		for _, n := range []string{name, c.CompanyName} {
			if k := lowerTrim(n); k != "" {
				if _, ok := im.byName[k]; !ok {
					im.byName[k] = c.ID
				}
			}
		}
	}
	var cts []struct{ CustomerID, Email string }
	im.DB.Table("customer_contacts").Select("customer_id, email").Where("organization_id = ? AND deleted_at IS NULL", im.Org).Scan(&cts)
	for _, c := range cts {
		if e := lowerTrim(c.Email); e != "" && !im.linked[c.CustomerID] {
			if _, ok := im.byEmail[e]; !ok {
				im.byEmail[e] = c.CustomerID
			}
		}
	}
	var docs []struct{ ID, DocType, Number string }
	im.DB.Table("billing_invoices").Select("id, doc_type, number").Where("organization_id = ? AND deleted_at IS NULL", im.Org).Scan(&docs)
	for _, d := range docs {
		if !im.linked[d.ID] {
			im.docByNumber[d.DocType+"|"+lowerTrim(d.Number)] = d.ID
		}
	}
}

// matchCustomer finds an existing (not yet linked) customer for c.
func (im *Importer) matchCustomer(c *Contact) (id, by string) {
	im.load()
	emails := []string{c.Email}
	for _, p := range c.Persons {
		emails = append(emails, p.Email)
	}
	for _, e := range emails {
		if e = lowerTrim(e); e != "" {
			if id := im.byEmail[e]; id != "" {
				return id, "email " + e
			}
		}
	}
	for _, n := range []string{c.Name, c.Company, strings.TrimSpace(c.FirstName + " " + c.LastName)} {
		if k := lowerTrim(n); k != "" {
			if id := im.byName[k]; id != "" {
				return id, "name"
			}
		}
	}
	return "", ""
}

func docType(kind string) string {
	switch kind {
	case KindQuote:
		return invoicing.TypeQuote
	case KindCredit:
		return invoicing.TypeCreditNote
	}
	return invoicing.TypeInvoice
}

func (im *Importer) matchDoc(d *Doc) string {
	im.load()
	if d.Number == "" {
		return ""
	}
	return im.docByNumber[docType(d.Kind)+"|"+lowerTrim(d.Number)]
}

func (im *Importer) decision(key, def string) string {
	if v := im.Opts.Decisions[key]; v != "" {
		return v
	}
	return def
}

// ---------- organisation settings ----------

// TaxRate is a named tax from the source.
type TaxRate struct {
	Name    string
	Percent float64
}

// ImportTaxes adds source tax rates missing here (by name).
func (im *Importer) ImportTaxes(taxes []TaxRate) {
	var have []invoicing.TaxRate
	im.DB.Where("organization_id = ?", im.Org).Find(&have)
	seen := map[string]bool{}
	for _, t := range have {
		seen[lowerTrim(t.Name)] = true
	}
	for _, t := range taxes {
		k := lowerTrim(t.Name)
		if k == "" || seen[k] {
			im.tally(EntSettings, outUnchanged)
			continue
		}
		seen[k] = true
		r := invoicing.TaxRate{ID: uuid.NewString(), OrganizationID: im.Org, Name: cut(t.Name, 100), Rate: t.Percent, CreatedAt: im.now()}
		if err := im.DB.Create(&r).Error; err != nil {
			im.fail(EntSettings, t.Name, t.Name, 0, err)
			continue
		}
		im.tally(EntSettings, outCreated)
	}
}

// ---------- items ----------

// UpsertItem adds an item to the services catalog (when chosen).
func (im *Importer) UpsertItem(it Item) {
	im.load()
	if !im.Opts.ItemsToCatalog || strings.TrimSpace(it.Name) == "" {
		im.tally(EntItems, outSkipped)
		return
	}
	ext := it.ExtID
	if ext == "" {
		ext = "name:" + lowerTrim(it.Name)
	}
	hash := hashOf(it)
	ref := im.ref(entItemRef, ext)
	svc := models.Service{Name: cut(it.Name, 255), Description: it.Description, Category: "Zoho Books items", Duration: 0, Price: math.Round(it.Rate*100) / 100, IsActive: it.Active}
	if im.RefSource != "zoho" {
		svc.Category = "Imported items"
	}
	if ref != nil && im.exists("services", ref.LocalID) {
		if ref.Hash == hash {
			im.tally(EntItems, outUnchanged)
			return
		}
		if im.locallyEdited("services", ref.LocalID, ref) && !im.Opts.Overwrite {
			im.tally(EntItems, outSkipped)
			return
		}
		err := im.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.Service{}).Where("id = ?", ref.LocalID).Updates(map[string]interface{}{
				"name": svc.Name, "description": svc.Description, "price": svc.Price, "is_active": svc.IsActive}).Error; err != nil {
				return err
			}
			return im.saveRef(tx, ref, entItemRef, ext, ref.LocalID, it.Name, hash, "", false)
		})
		if err != nil {
			im.fail(EntItems, ext, it.Name, it.Row, err)
			return
		}
		im.tally(EntItems, outUpdated)
		return
	}
	// Same name already in the catalog: link it.
	var existing struct{ ID string }
	im.DB.Table("services").Select("id").Where("organization_id = ? AND deleted_at IS NULL AND LOWER(name) = ?", im.Org, lowerTrim(it.Name)).Limit(1).Scan(&existing)
	err := im.DB.Transaction(func(tx *gorm.DB) error {
		if existing.ID != "" {
			return im.saveRef(tx, ref, entItemRef, ext, existing.ID, it.Name, hash, "", false)
		}
		svc.ID, svc.OrganizationID = uuid.NewString(), im.Org
		if err := tx.Create(&svc).Error; err != nil {
			return err
		}
		if !it.Active {
			tx.Model(&models.Service{}).Where("id = ?", svc.ID).UpdateColumn("is_active", false)
		}
		return im.saveRef(tx, ref, entItemRef, ext, svc.ID, it.Name, hash, "", true)
	})
	if err != nil {
		im.fail(EntItems, ext, it.Name, it.Row, err)
		return
	}
	if existing.ID != "" {
		im.tally(EntItems, outUnchanged)
	} else {
		im.tally(EntItems, outCreated)
	}
}

// ---------- contacts ----------

func (c *Contact) primaryEmail() string {
	if c.Email != "" {
		return c.Email
	}
	for _, p := range c.Persons {
		if p.Primary && p.Email != "" {
			return p.Email
		}
	}
	for _, p := range c.Persons {
		if p.Email != "" {
			return p.Email
		}
	}
	return ""
}

func (c *Contact) displayName() string {
	for _, s := range []string{c.Name, c.Company, strings.TrimSpace(c.FirstName + " " + c.LastName), c.primaryEmail()} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return "Unnamed customer"
}

func (c *Contact) ext() string {
	if c.ExtID != "" {
		return c.ExtID
	}
	if e := c.primaryEmail(); e != "" {
		return "email:" + lowerTrim(e)
	}
	return "name:" + lowerTrim(c.displayName())
}

// customerFields maps a contact to customer columns.
func (im *Importer) customerFields(c *Contact) map[string]interface{} {
	first, last := c.displayName(), ""
	if c.Individual && strings.TrimSpace(c.FirstName+c.LastName) != "" {
		first, last = c.FirstName, c.LastName
	}
	phone := c.Phone
	if phone == "" {
		phone = c.Mobile
	}
	if phone == "" {
		phone = c.Billing.Phone
	}
	street := strings.Join(nonEmpty(c.Billing.Street, c.Billing.Street2), ", ")
	country := strings.TrimSpace(c.Billing.Country)
	cc := ""
	if country != "" {
		cc = currency.CountryCode(country)
	}
	cur := strings.ToUpper(strings.TrimSpace(c.Currency))
	if cur != "" && !currency.IsSupported(cur) {
		cur = ""
	}
	notes := c.Notes
	if c.Billing.Attention != "" && !c.Individual {
		notes = strings.TrimSpace(notes + "\nAttention: " + c.Billing.Attention)
	}
	f := map[string]interface{}{
		"first_name": cut(first, 100), "last_name": cut(last, 100), "phone": cut(phone, 20),
		"address_street": cut(street, 255), "address_suburb": cut(c.Billing.City, 100), "address_state": cut(c.Billing.State, 50),
		"address_postcode": cut(c.Billing.Zip, 10), "country_code": cut(cc, 2), "currency": cur, "notes": notes,
		"company_name": cut(c.Company, 255), "tax_number": cut(c.TaxNumber, 100), "website": cut(c.Website, 255),
		"payment_terms_days": c.TermsDays, "is_active": c.Active,
	}
	if country != "" {
		f["address_country"] = cut(country, 100)
	}
	return f
}

// UpsertContact imports a customer and its contact people.
func (im *Importer) UpsertContact(c Contact) (string, string) {
	im.load()
	ext := c.ext()
	label := c.displayName()
	c.Email = lowerTrim(c.Email)
	hash := hashOf(c)
	ref := im.ref(entContactRef, ext)
	cp := c
	im.contacts[ext] = &cp
	if c.ExtID != "" && c.ExtID != ext {
		im.contacts[c.ExtID] = &cp
	}

	if ref != nil && im.exists("customers", ref.LocalID) {
		im.contactLocal[ext] = ref.LocalID
		if ref.Hash == hash {
			im.tally(EntContacts, outUnchanged)
			im.countPersons(c, outUnchanged)
			return ref.LocalID, outUnchanged
		}
		if !ref.Created || (im.locallyEdited("customers", ref.LocalID, ref) && !im.Opts.Overwrite) {
			// Merged into an existing customer, or edited here since: only add new people.
			if err := im.DB.Transaction(func(tx *gorm.DB) error {
				if err := im.upsertPersons(tx, ref.LocalID, c, false); err != nil {
					return err
				}
				return im.saveRef(tx, ref, entContactRef, ext, ref.LocalID, label, hash, c.Modified, ref.Created)
			}); err != nil {
				im.fail(EntContacts, ext, label, c.Row, err)
				return ref.LocalID, "failed"
			}
			im.tally(EntContacts, outSkipped)
			return ref.LocalID, outSkipped
		}
		err := im.DB.Transaction(func(tx *gorm.DB) error {
			f := im.customerFields(&c)
			if e := c.primaryEmail(); e != "" {
				f["email"] = cut(lowerTrim(e), 255)
			}
			f["updated_at"] = im.now()
			if err := tx.Model(&models.Customer{}).Where("id = ?", ref.LocalID).Updates(f).Error; err != nil {
				return err
			}
			if err := im.upsertPersons(tx, ref.LocalID, c, true); err != nil {
				return err
			}
			return im.saveRef(tx, ref, entContactRef, ext, ref.LocalID, label, hash, c.Modified, ref.Created)
		})
		if err != nil {
			im.fail(EntContacts, ext, label, c.Row, err)
			return ref.LocalID, "failed"
		}
		im.tally(EntContacts, outUpdated)
		return ref.LocalID, outUpdated
	}

	// Not imported before.
	matchID, _ := im.matchCustomer(&c)
	action := im.decision("contact:"+ext, im.Opts.OnCustomerMatch)
	if action == "" {
		action = "merge"
	}
	if matchID != "" && action == "skip" {
		im.contactLocal[ext] = matchID // documents still go to the existing customer
		im.tally(EntContacts, outSkipped)
		im.countPersons(c, outSkipped)
		return matchID, outSkipped
	}
	if matchID != "" && action == "merge" {
		err := im.DB.Transaction(func(tx *gorm.DB) error {
			if err := im.fillBlanks(tx, matchID, &c); err != nil {
				return err
			}
			if err := im.upsertPersons(tx, matchID, c, false); err != nil {
				return err
			}
			return im.saveRef(tx, nil, entContactRef, ext, matchID, label, hash, c.Modified, false)
		})
		if err != nil {
			im.fail(EntContacts, ext, label, c.Row, err)
			return "", "failed"
		}
		im.linked[matchID] = true
		im.contactLocal[ext] = matchID
		im.tally(EntContacts, outUpdated)
		return matchID, outUpdated
	}
	id := uuid.NewString()
	err := im.DB.Transaction(func(tx *gorm.DB) error {
		cu := models.Customer{ID: id, OrganizationID: im.Org, IsActive: true, Source: im.recordSource(), ExternalID: cut(c.ExtID, 100)}
		if err := tx.Omit(clause.Associations).Create(&cu).Error; err != nil {
			return err
		}
		f := im.customerFields(&c)
		if e := lowerTrim(c.primaryEmail()); e != "" {
			if im.emailsTaken[e] {
				im.warn(EntContacts, ext, label, "Another customer already uses "+e+"; it was added as a contact person only.")
			} else {
				f["email"] = cut(e, 255)
				im.emailsTaken[e] = true
			}
		}
		if _, ok := f["address_country"]; !ok {
			f["address_country"] = ""
		}
		if err := tx.Model(&models.Customer{}).Where("id = ?", id).Updates(f).Error; err != nil {
			return err
		}
		if !c.Active {
			tx.Model(&models.Customer{}).Where("id = ?", id).UpdateColumn("is_active", false)
		}
		if err := im.upsertPersons(tx, id, c, true); err != nil {
			return err
		}
		return im.saveRef(tx, nil, entContactRef, ext, id, label, hash, c.Modified, true)
	})
	if err != nil {
		im.fail(EntContacts, ext, label, c.Row, err)
		return "", "failed"
	}
	im.linked[id] = true
	im.contactLocal[ext] = id
	if c.primaryEmail() == "" {
		im.warn(EntContacts, ext, label, "No email address — invoices can't be emailed to this customer until one is added.")
	}
	im.tally(EntContacts, outCreated)
	return id, outCreated
}

func (im *Importer) recordSource() string {
	if im.RefSource == "zoho" {
		return "zoho"
	}
	return "csv"
}

func (im *Importer) countPersons(c Contact, outcome string) {
	for range c.Persons {
		im.tally(EntPersons, outcome)
	}
}

// fillBlanks copies imported values into empty fields of an existing customer.
func (im *Importer) fillBlanks(tx *gorm.DB, id string, c *Contact) error {
	var cu models.Customer
	if err := tx.Omit(clause.Associations).Where("id = ?", id).Take(&cu).Error; err != nil {
		return err
	}
	f := im.customerFields(c)
	upd := map[string]interface{}{}
	set := func(col string, cur string) {
		if strings.TrimSpace(cur) == "" {
			if v, ok := f[col].(string); ok && v != "" {
				upd[col] = v
			}
		}
	}
	set("phone", cu.Phone)
	set("address_street", cu.Address.Street)
	set("address_suburb", cu.Address.Suburb)
	set("address_state", cu.Address.State)
	set("address_postcode", cu.Address.Postcode)
	set("country_code", cu.CountryCode)
	set("currency", cu.Currency)
	set("company_name", cu.CompanyName)
	set("tax_number", cu.TaxNumber)
	set("website", cu.Website)
	if cu.Email == "" {
		if e := lowerTrim(c.primaryEmail()); e != "" && !im.emailsTaken[e] {
			upd["email"] = e
			im.emailsTaken[e] = true
		}
	}
	if cu.PaymentTermsDays == nil && c.TermsDays != nil {
		upd["payment_terms_days"] = *c.TermsDays
	}
	if len(upd) == 0 {
		return nil
	}
	return tx.Model(&models.Customer{}).Where("id = ?", id).UpdateColumns(upd).Error
}

// upsertPersons writes contact people: the primary one is the billing (To)
// contact, the others are copied on invoices.
func (im *Importer) upsertPersons(tx *gorm.DB, customerID string, c Contact, update bool) error {
	var existing []models.CustomerContact
	tx.Where("customer_id = ? AND organization_id = ?", customerID, im.Org).Find(&existing)
	byEmail := map[string]*models.CustomerContact{}
	hasPrimary := false
	for i := range existing {
		byEmail[lowerTrim(existing[i].Email)] = &existing[i]
		if existing[i].IsPrimaryBilling {
			hasPrimary = true
		}
	}
	for _, p := range c.Persons {
		e := lowerTrim(p.Email)
		label := strings.TrimSpace(p.FirstName + " " + p.LastName)
		if e == "" {
			im.tally(EntPersons, outSkipped)
			if label != "" {
				im.warn(EntPersons, p.ExtID, label+" ("+c.displayName()+")", "Contact person has no email address and was not added.")
			}
			continue
		}
		phone := p.Phone
		if phone == "" {
			phone = p.Mobile
		}
		ext := p.ExtID
		if ext == "" {
			ext = c.ext() + ":" + e
		}
		if ct := byEmail[e]; ct != nil {
			if update {
				upd := map[string]interface{}{"name": cut(label, 200), "phone": cut(phone, 50), "role": cut(p.Role, 120)}
				if err := tx.Model(&models.CustomerContact{}).Where("id = ?", ct.ID).Updates(upd).Error; err != nil {
					return err
				}
				im.tally(EntPersons, outUpdated)
			} else {
				im.tally(EntPersons, outUnchanged)
			}
			if err := im.saveRef(tx, im.ref(entPersonRef, ext), entPersonRef, ext, ct.ID, label, "", "", false); err != nil {
				return err
			}
			continue
		}
		primary := p.Primary && !hasPrimary
		if primary {
			hasPrimary = true
		}
		ct := models.CustomerContact{ID: uuid.NewString(), CustomerID: customerID, OrganizationID: im.Org, Name: cut(label, 200), Email: cut(e, 255),
			Phone: cut(phone, 50), Role: cut(p.Role, 120), IsPrimaryBilling: primary, CCInvoices: !primary, Notes: ""}
		if err := tx.Create(&ct).Error; err != nil {
			return err
		}
		byEmail[e] = &ct
		if err := im.saveRef(tx, im.ref(entPersonRef, ext), entPersonRef, ext, ct.ID, label, "", "", true); err != nil {
			return err
		}
		im.tally(EntPersons, outCreated)
	}
	return nil
}

// customerFor returns the local customer of a document, creating a simple
// one from the document's name/email when the source had no contact list.
func (im *Importer) customerFor(d *Doc) string {
	im.load()
	if d.ContactID != "" {
		if id := im.contactLocal[d.ContactID]; id != "" {
			return id
		}
		if r := im.ref(entContactRef, d.ContactID); r != nil && im.exists("customers", r.LocalID) {
			im.contactLocal[d.ContactID] = r.LocalID
			return r.LocalID
		}
	}
	name := strings.TrimSpace(d.CustomerName)
	if name == "" && d.Email == "" {
		return ""
	}
	c := Contact{ExtID: d.ContactID, Name: name, Email: d.Email, Active: true, Currency: d.Currency}
	ext := c.ext()
	if id := im.contactLocal[ext]; id != "" {
		return id
	}
	if r := im.ref(entContactRef, ext); r != nil && im.exists("customers", r.LocalID) {
		im.contactLocal[ext] = r.LocalID
		return r.LocalID
	}
	if !im.Opts.want(EntContacts) {
		if id, _ := im.matchCustomer(&c); id != "" {
			return id
		}
		return ""
	}
	id, _ := im.UpsertContact(c)
	return id
}

// ---------- documents ----------

// parseDay reads a date in the forms Zoho uses (ISO, dd/mm/yyyy, 14 Mar 2025).
func parseDay(s string, dayFirst bool) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if len(s) >= 10 && s[4] == '-' {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, true
		}
	}
	if strings.ContainsAny(s, "/.-") && len(s) <= 10 {
		sep := "/"
		if strings.Contains(s, ".") {
			sep = "."
		} else if strings.Contains(s, "-") {
			sep = "-"
		}
		p := strings.Split(s, sep)
		if len(p) == 3 {
			a, _ := parseNum(p[0])
			b, _ := parseNum(p[1])
			y, _ := parseNum(p[2])
			if y < 100 {
				y += 2000
			}
			d, m := int(a), int(b)
			if !dayFirst || (b > 12 && a <= 12) {
				d, m = int(b), int(a)
			}
			if a > 12 && b <= 12 {
				d, m = int(a), int(b)
			}
			if m >= 1 && m <= 12 && d >= 1 && d <= 31 {
				return time.Date(int(y), time.Month(m), d, 0, 0, 0, 0, time.UTC), true
			}
		}
	}
	for _, f := range []string{"2 Jan 2006", "02 Jan 2006", "Jan 2, 2006", "2 January 2006", "January 2, 2006", "02-Jan-2006", "2-Jan-2006", time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(f, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

// mapStatus turns a source status into ours.
func mapStatus(kind, s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", "_"))
	switch kind {
	case KindQuote:
		switch s {
		case "draft":
			return invoicing.StatusDraft
		case "accepted":
			return invoicing.StatusAccepted
		case "declined", "rejected":
			return invoicing.StatusDeclined
		case "invoiced", "converted":
			return invoicing.StatusConverted
		case "void":
			return invoicing.StatusVoid
		}
		return invoicing.StatusSent // sent, viewed, expired (derived again from the expiry date)
	case KindCredit:
		switch s {
		case "draft":
			return invoicing.StatusDraft
		case "void":
			return invoicing.StatusVoid
		case "closed":
			return invoicing.StatusPaid
		}
		return invoicing.StatusSent // open
	case KindRecurring:
		return invoicing.StatusDraft
	}
	switch s {
	case "draft", "pending_approval", "approved":
		return invoicing.StatusDraft
	case "paid", "closed":
		return invoicing.StatusPaid
	case "partially_paid", "partial", "partiallypaid":
		return invoicing.StatusPartial
	case "void", "voided":
		return invoicing.StatusVoid
	}
	return invoicing.StatusSent // sent, viewed, overdue, unpaid
}

// mapMethod turns a Zoho payment mode into ours.
func mapMethod(m string) string {
	s := strings.ToLower(strings.TrimSpace(m))
	switch {
	case s == "":
		return "other"
	case strings.Contains(s, "cash"):
		return "cash"
	case strings.Contains(s, "cheque") || strings.Contains(s, "check"):
		return "cheque"
	case strings.Contains(s, "card"):
		return "card"
	case strings.Contains(s, "bank") || strings.Contains(s, "transfer") || strings.Contains(s, "eft") || strings.Contains(s, "remittance") || strings.Contains(s, "direct"):
		return "bank_transfer"
	case strings.Contains(s, "paypal"):
		return "paypal"
	case strings.Contains(s, "stripe"):
		return "stripe"
	case strings.Contains(s, "mpesa") || strings.Contains(s, "m-pesa") || strings.Contains(s, "mobile"):
		return "mobile_money"
	}
	return "other"
}

// DocResult is what UpsertDoc did.
type DocResult struct {
	LocalID string
	Outcome string
	Changed bool // the document was (re)written
}

func entForKind(kind string) string {
	switch kind {
	case KindQuote:
		return EntQuotes
	case KindCredit:
		return EntCredits
	case KindRecurring:
		return EntRecurring
	}
	return EntInvoices
}

// UpsertDoc imports an invoice, quote, credit note or recurring profile with
// its lines and payments, keeping the source's totals and numbers exactly.
func (im *Importer) UpsertDoc(d Doc) DocResult {
	im.load()
	ent := entForKind(d.Kind)
	ext := d.ExtID
	if ext == "" {
		ext = "number:" + lowerTrim(d.Number)
	}
	refEnt := entDocRef + ":" + d.Kind
	label := strings.TrimSpace(strings.TrimSpace(d.Number+" "+d.RecurName) + " · " + d.CustomerName)
	hash := hashOf(d)
	if d.Kind == KindInvoice || d.Kind == KindQuote {
		im.numbers[docType(d.Kind)] = append(im.numbers[docType(d.Kind)], d.Number)
	}
	ref := im.ref(refEnt, ext)
	target := ""
	created := true
	if ref != nil && im.exists("billing_invoices", ref.LocalID) {
		if d.Kind == KindQuote {
			im.quoteLocal[d.ExtID] = ref.LocalID
		}
		if ref.Hash == hash {
			im.tally(ent, outUnchanged)
			im.tallyPayments(d, outUnchanged)
			im.addRecon(d, ref.LocalID)
			return DocResult{LocalID: ref.LocalID, Outcome: outUnchanged}
		}
		if im.locallyEdited("billing_invoices", ref.LocalID, ref) && !im.Opts.Overwrite {
			im.tally(ent, outSkipped)
			im.tallyPayments(d, outSkipped)
			im.warn(ent, ext, label, "Changed in Zoho but also edited here since the last import — kept your version (choose \"Overwrite local changes\" to replace it).")
			return DocResult{LocalID: ref.LocalID, Outcome: outSkipped}
		}
		target, created = ref.LocalID, ref.Created
	} else {
		if m := im.matchDoc(&d); m != "" {
			def := im.Opts.OnDocMatch
			if def == "" {
				def = "skip"
			}
			switch im.decision("doc:"+d.Kind+":"+ext, def) {
			case "overwrite":
				target, created = m, false
			default:
				im.tally(ent, outSkipped)
				im.tallyPayments(d, outSkipped)
				im.warn(ent, ext, label, "A "+strings.ReplaceAll(docType(d.Kind), "_", " ")+" numbered "+d.Number+" already exists here — skipped.")
				return DocResult{Outcome: outSkipped}
			}
		}
	}
	custID := im.customerFor(&d)
	inv, flag, err := im.buildInvoice(&d, custID)
	if err != nil {
		im.fail(ent, ext, label, d.Row, err)
		return DocResult{Outcome: "failed"}
	}
	isNew := target == ""
	if isNew {
		inv.ID = uuid.NewString()
		inv.PublicToken = invoicing.NewPublicToken()
	} else {
		inv.ID = target
		var old struct {
			PublicToken string
			CreatedBy   string
		}
		im.DB.Table("billing_invoices").Select("public_token, created_by").Where("id = ?", target).Take(&old)
		inv.PublicToken = old.PublicToken
		if old.CreatedBy != "" {
			inv.CreatedBy = old.CreatedBy
		}
		if inv.PublicToken == "" {
			inv.PublicToken = invoicing.NewPublicToken()
		}
	}
	payOutcomes := map[string]string{}
	err = im.DB.Transaction(func(tx *gorm.DB) error {
		items := inv.Items
		pays := inv.Payments
		inv.Items, inv.Payments, inv.Activities = nil, nil, nil
		if isNew {
			if err := tx.Omit(clause.Associations).Create(inv).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Where("invoice_id = ?", inv.ID).Delete(&invoicing.InvoiceItem{}).Error; err != nil {
				return err
			}
			// Imported payments are replaced; payments recorded here stay.
			if err := tx.Where("invoice_id = ? AND source = ?", inv.ID, im.recordSource()).Delete(&invoicing.Payment{}).Error; err != nil {
				return err
			}
			if !created { // took over a document that existed here: its own payments are replaced too
				tx.Where("invoice_id = ? AND COALESCE(source,'') = ''", inv.ID).Delete(&invoicing.Payment{})
			}
			tx.Where("invoice_id = ? AND type IN ?", inv.ID, []string{"imported", "imported_payment"}).Delete(&invoicing.Activity{})
			var local []invoicing.Payment
			tx.Where("invoice_id = ?", inv.ID).Find(&local)
			for _, p := range local {
				inv.AmountPaid += p.Amount
			}
			if len(local) > 0 {
				inv.AmountPaid = invoicing.RoundCur(inv.AmountPaid, inv.Currency)
				inv.BalanceDue = invoicing.RoundCur(inv.Total-inv.AmountPaid-inv.CreditsApplied-inv.WriteOffAmount, inv.Currency)
			}
			if err := tx.Model(&invoicing.Invoice{}).Where("id = ?", inv.ID).Select("*").Omit("id", "organization_id", "created_at", "deleted_at", "Items", "Payments", "Activities").Updates(inv).Error; err != nil {
				return err
			}
		}
		for i := range items {
			items[i].InvoiceID = inv.ID
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return err
			}
		}
		for i := range pays {
			pays[i].InvoiceID = inv.ID
			pr := im.ref(entPaymentRef, pays[i].ExternalID)
			if pr != nil {
				payOutcomes[pays[i].ExternalID] = outUpdated
			} else {
				payOutcomes[pays[i].ExternalID] = outCreated
			}
			if err := tx.Create(&pays[i]).Error; err != nil {
				return err
			}
			if err := im.saveRef(tx, pr, entPaymentRef, pays[i].ExternalID, pays[i].ID, d.Number+" "+pays[i].Reference, "", "", true); err != nil {
				return err
			}
			msg := fmt.Sprintf("Imported from Zoho: payment of %s %s received on %s (%s%s)", inv.Currency, money(pays[i].Amount, inv.Currency),
				pays[i].Date.Format("2 Jan 2006"), strings.ReplaceAll(pays[i].Method, "_", " "), refSuffix(pays[i].Reference))
			if err := im.activity(tx, inv, "imported_payment", msg, pays[i].Date); err != nil {
				return err
			}
		}
		what := "Imported from Zoho Books"
		if im.RefSource != "zoho" {
			what = "Imported from a CSV file"
		}
		msg := fmt.Sprintf("%s (status there: %s)", what, strings.ReplaceAll(firstNonEmpty(d.Status, "unknown"), "_", " "))
		if flag != "" {
			msg += ". " + flag
		}
		at := inv.IssueDate
		if t, ok := parseDay(d.Created, im.DayFirst); ok && !t.After(inv.IssueDate.AddDate(0, 0, 1)) {
			at = t
		}
		if err := im.activity(tx, inv, "imported", msg, at); err != nil {
			return err
		}
		if d.Kind == KindInvoice && d.EstimateID != "" {
			if qid := im.quoteLocal[d.EstimateID]; qid != "" {
				tx.Model(&invoicing.Invoice{}).Where("id = ?", inv.ID).UpdateColumn("converted_from_id", qid)
				tx.Model(&invoicing.Invoice{}).Where("id = ?", qid).UpdateColumns(map[string]interface{}{"converted_to_id": inv.ID, "status": invoicing.StatusConverted})
			}
		}
		return im.saveRef(tx, ref, refEnt, ext, inv.ID, label, hash, d.Modified, created)
	})
	if err != nil {
		im.fail(ent, ext, label, d.Row, err)
		return DocResult{Outcome: "failed"}
	}
	if flag != "" {
		im.flags++
		im.warn(ent, ext, label, flag)
	}
	if d.Kind == KindQuote {
		im.quoteLocal[d.ExtID] = inv.ID
	}
	im.linked[inv.ID] = true
	for _, p := range d.Payments {
		if o := payOutcomes[payExt(d, p)]; o != "" {
			if ref != nil && ref.Hash != "" && o == outUpdated {
				im.tally(EntPayments, outUnchanged)
			} else {
				im.tally(EntPayments, o)
			}
		}
	}
	im.addRecon(d, inv.ID)
	out := outCreated
	if !isNew {
		out = outUpdated
	}
	im.tally(ent, out)
	return DocResult{LocalID: inv.ID, Outcome: out, Changed: true}
}

func payExt(d Doc, p Pay) string {
	if p.ExtID != "" {
		return p.ExtID
	}
	return d.Kind + ":" + d.ExtID + ":" + p.Date + ":" + money(p.Amount, d.Currency)
}

func (im *Importer) tallyPayments(d Doc, outcome string) {
	for range d.Payments {
		im.tally(EntPayments, outcome)
	}
}

func refSuffix(r string) string {
	if strings.TrimSpace(r) == "" {
		return ""
	}
	return ", ref " + r
}

func money(v float64, cur string) string {
	return fmt.Sprintf("%.*f", currency.Decimals(cur), invoicing.RoundCur(v, cur))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// activity writes a history entry. Types imported* are not mapped to
// notifications (pkg/notify/hooks), so imports never alert anyone.
func (im *Importer) activity(tx *gorm.DB, inv *invoicing.Invoice, typ, msg string, at time.Time) error {
	if at.IsZero() {
		at = im.now()
	}
	return tx.Create(&invoicing.Activity{ID: uuid.NewString(), InvoiceID: inv.ID, OrganizationID: im.Org, Type: typ, Message: msg, UserID: im.User, CreatedAt: at}).Error
}

// buildInvoice maps a document. flag describes a difference kept from the source.
func (im *Importer) buildInvoice(d *Doc, custID string) (*invoicing.Invoice, string, error) {
	issue, ok := parseDay(d.Date, im.DayFirst)
	if !ok {
		return nil, "", fmt.Errorf("missing or unreadable date %q", d.Date)
	}
	due, ok := parseDay(d.DueDate, im.DayFirst)
	if !ok {
		due = issue
	}
	cur := strings.ToUpper(strings.TrimSpace(d.Currency))
	if cur == "" {
		cur = firstNonEmpty(im.SrcBase, im.Home)
	}
	if !currency.IsSupported(cur) {
		return nil, "", fmt.Errorf("currency %s is not supported", cur)
	}
	inv := &invoicing.Invoice{OrganizationID: im.Org, DocType: docType(d.Kind), Number: cut(d.Number, 50), Status: mapStatus(d.Kind, d.Status),
		ClientName: cut(d.CustomerName, 255), ClientEmail: cut(d.Email, 255), ClientAddress: d.BillingAddress, Reference: cut(d.Reference, 255),
		IssueDate: issue, DueDate: due, Currency: cur, BaseCurrency: im.Home, Notes: d.Notes, Terms: d.Terms, PricesIncludeTax: d.InclusiveTax,
		ShippingAmount: d.Shipping, Source: im.recordSource(), ExternalID: cut(d.ExtID, 100), ExternalURL: d.URL, CreatedBy: im.User,
		CreatedAt: issue, UpdatedAt: im.now(), CCEmails: []string{}}
	if custID != "" {
		inv.CustomerID = &custID
	}
	if c := im.contacts[d.ContactID]; c != nil {
		if inv.ClientName == "" {
			inv.ClientName = cut(c.displayName(), 255)
		}
		if inv.ClientEmail == "" {
			inv.ClientEmail = cut(c.primaryEmail(), 255)
		}
		if inv.ClientAddress == "" {
			inv.ClientAddress = formatAddress(c.Billing)
		}
		inv.ClientTaxNumber = cut(c.TaxNumber, 100)
		inv.ClientPhone = cut(firstNonEmpty(c.Phone, c.Mobile), 50)
	} else if custID != "" && (inv.ClientEmail == "" || inv.ClientName == "") {
		var cu struct{ FirstName, LastName, Email, Phone string }
		im.DB.Table("customers").Select("first_name, last_name, email, phone").Where("id = ?", custID).Take(&cu)
		if inv.ClientName == "" {
			inv.ClientName = strings.TrimSpace(cu.FirstName + " " + cu.LastName)
		}
		if inv.ClientEmail == "" {
			inv.ClientEmail = cu.Email
		}
	}
	if d.Kind == KindRecurring {
		inv.Title = cut("Recurring profile: "+firstNonEmpty(d.RecurName, d.CustomerName), 255)
		inv.RecurrenceInterval = d.RecurInterval
		if t, ok := parseDay(d.RecurNext, im.DayFirst); ok {
			inv.NextRecurrenceDate = &t
		}
		if t, ok := parseDay(d.RecurEnd, im.DayFirst); ok {
			inv.RecurrenceEndDate = &t
		}
		note := fmt.Sprintf("Recurring profile imported from Zoho (%s, every %s, next invoice %s, status there: %s). It is NOT active here — turn on \"Recurring\" when you stop it in Zoho, so customers are not invoiced twice.",
			firstNonEmpty(d.RecurName, "unnamed"), d.RecurInterval, firstNonEmpty(d.RecurNext, "—"), firstNonEmpty(d.Status, "unknown"))
		inv.Notes = strings.TrimSpace(note + "\n\n" + inv.Notes)
	}

	// Exchange rate: Zoho stores base units per 1 document unit; we store
	// document units per 1 home unit.
	now := im.now().UTC()
	rd := issue
	inv.RateDate = &rd
	switch {
	case cur == im.Home:
		inv.ExchangeRate, inv.RateSource, inv.RateProvider = 1, fxrates.SourceSame, "Same currency"
		inv.RateDate = &now
	case d.ExchangeRate > 0 && (im.SrcBase == "" || strings.EqualFold(im.SrcBase, im.Home)) && d.ExchangeRate != 1:
		inv.ExchangeRate = fxrates.SigRound(1 / d.ExchangeRate)
		inv.RateSource, inv.RateProvider = "zoho", "Zoho Books (rate on the document)"
	default:
		inv.ExchangeRate, inv.RateSource, inv.RateProvider = 0, fxrates.SourceMissing, ""
		inv.RateDate = nil
	}

	// Lines.
	for i, l := range d.Lines {
		desc := strings.TrimSpace(l.Name)
		if dd := strings.TrimSpace(l.Description); dd != "" && dd != desc {
			if desc != "" {
				desc += "\n" + dd
			} else {
				desc = dd
			}
		}
		sub := invoicing.RoundCur(l.Quantity*l.Rate, cur)
		pct := l.DiscountPct
		if pct == 0 && l.DiscountAmt != 0 && sub != 0 {
			pct = math.Round(l.DiscountAmt/sub*100*10000) / 10000
		}
		typ := "service"
		inv.Items = append(inv.Items, invoicing.InvoiceItem{ID: uuid.NewString(), Position: i + 1, ItemType: typ, ReferenceID: cut(l.ItemID, 255),
			Description: desc, Quantity: l.Quantity, Unit: cut(l.Unit, 30), UnitPrice: l.Rate, DiscountPercent: pct, TaxRate: l.TaxPercent, TaxName: cut(l.TaxName, 50)})
	}
	if d.Adjustment != 0 {
		inv.Items = append(inv.Items, invoicing.InvoiceItem{ID: uuid.NewString(), Position: len(inv.Items) + 1, ItemType: "custom",
			Description: firstNonEmpty(d.AdjustmentDesc, "Adjustment"), Quantity: 1, UnitPrice: d.Adjustment})
	}
	if d.DiscountPct > 0 {
		inv.DiscountType, inv.DiscountValue = "percent", d.DiscountPct
	} else if d.DiscountAmt > 0 {
		inv.DiscountType, inv.DiscountValue = "amount", d.DiscountAmt
	} else {
		inv.DiscountType = "percent"
	}
	invoicing.Recalculate(inv)
	calcTotal, calcTax := inv.Total, inv.TaxTotal
	var flags []string
	if len(d.Lines) == 0 && d.Total != 0 {
		// A summary row without lines (e.g. a generic CSV): one line for the amount.
		net := d.Total - d.TaxTotal - d.Shipping
		inv.Items = []invoicing.InvoiceItem{{ID: uuid.NewString(), Position: 1, ItemType: "custom", Description: firstNonEmpty(d.Reference, "Imported "+strings.ReplaceAll(docType(d.Kind), "_", " ")+" "+d.Number),
			Quantity: 1, UnitPrice: net}}
		if net != 0 && d.TaxTotal != 0 {
			inv.Items[0].TaxRate = math.Round(d.TaxTotal/net*100*100) / 100
		}
		invoicing.Recalculate(inv)
		calcTotal, calcTax = inv.Total, inv.TaxTotal
	}
	srcTotal := d.Total
	if srcTotal == 0 && len(d.Lines) > 0 && d.SubTotal == 0 {
		srcTotal = calcTotal // the source gave no totals
	}
	srcTax := d.TaxTotal
	if d.TaxUnknown {
		srcTax = calcTax
	}
	if math.Abs(calcTotal-srcTotal) > 0.01 || math.Abs(calcTax-srcTax) > 0.01 {
		flags = append(flags, fmt.Sprintf("Totals kept exactly as in the source (%s %s, tax %s); recalculating the lines here gives %s, tax %s",
			cur, money(srcTotal, cur), money(srcTax, cur), money(calcTotal, cur), money(calcTax, cur)))
		inv.TaxTotal = invoicing.RoundCur(srcTax, cur)
		// Keep the printed breakdown adding up: the difference goes into the discount.
		excl := inv.Subtotal - inv.DiscountTotal + inv.ShippingAmount
		if !inv.PricesIncludeTax {
			excl += inv.TaxTotal
		}
		inv.DiscountTotal = invoicing.RoundCur(inv.DiscountTotal+(excl-srcTotal), cur)
	} else {
		inv.TaxTotal = invoicing.RoundCur(srcTax, cur)
	}
	inv.Total = invoicing.RoundCur(srcTotal, cur)
	if d.DiscountAfterTax {
		flags = append(flags, "Zoho applied the discount after tax; the totals are Zoho's")
	}

	// Payments.
	paid := 0.0
	if im.Opts.want(EntPayments) && d.Kind == KindInvoice {
		for _, p := range d.Payments {
			pd, ok := parseDay(p.Date, im.DayFirst)
			if !ok {
				pd = issue
			}
			amt := invoicing.RoundCur(p.Amount, cur)
			if amt == 0 {
				continue
			}
			inv.Payments = append(inv.Payments, invoicing.Payment{ID: uuid.NewString(), OrganizationID: im.Org, Amount: amt, Date: pd, Method: mapMethod(p.Mode),
				Reference: cut(p.Reference, 255), Notes: p.Notes, Source: im.recordSource(), ExternalID: cut(payExt(*d, p), 100), CreatedBy: im.User, CreatedAt: im.now()})
			paid += amt
		}
	}
	inv.AmountPaid = invoicing.RoundCur(paid, cur)
	switch d.Kind {
	case KindInvoice:
		inv.WriteOffAmount = invoicing.RoundCur(d.WriteOff, cur)
		bal := d.Balance
		if d.BalanceUnknown {
			bal = inv.Total - inv.AmountPaid - inv.WriteOffAmount
			if d.Credits != nil {
				bal -= *d.Credits
			}
		}
		if d.Credits != nil {
			inv.CreditsApplied = invoicing.RoundCur(*d.Credits, cur)
		}
		if inv.Status == invoicing.StatusVoid {
			inv.BalanceDue = 0
			break
		}
		residual := invoicing.RoundCur(inv.Total-inv.AmountPaid-inv.CreditsApplied-inv.WriteOffAmount-bal, cur)
		if residual > 0.005 && d.Credits == nil && inv.Status != invoicing.StatusDraft {
			inv.CreditsApplied = invoicing.RoundCur(inv.CreditsApplied+residual, cur)
			flags = append(flags, fmt.Sprintf("%s %s was settled in the source without a payment record in this import (credit notes, write-offs or payments not included); recorded as credits applied", cur, money(residual, cur)))
		}
		inv.BalanceDue = invoicing.RoundCur(inv.Total-inv.AmountPaid-inv.CreditsApplied-inv.WriteOffAmount, cur)
		if inv.Status != invoicing.StatusDraft && math.Abs(inv.BalanceDue-bal) > 0.01 {
			flags = append(flags, fmt.Sprintf("Balance kept as in the source (%s %s); payments and credits here give %s", cur, money(bal, cur), money(inv.BalanceDue, cur)))
			inv.BalanceDue = invoicing.RoundCur(bal, cur)
		}
	case KindCredit:
		inv.BalanceDue = invoicing.RoundCur(d.Balance, cur)
		inv.CreditsApplied = invoicing.RoundCur(inv.Total-d.Balance, cur)
	default:
		inv.BalanceDue = inv.Total
	}
	invoicing.SetBaseTotal(inv)

	// Dates of events.
	if inv.Status != invoicing.StatusDraft {
		t := issue
		inv.SentAt = &t
	}
	if strings.EqualFold(d.Status, "viewed") {
		t := issue
		inv.ViewedAt = &t
	}
	if inv.Status == invoicing.StatusPaid && d.Kind == KindInvoice {
		t := issue
		if lp, ok := parseDay(d.LastPaymentDate, im.DayFirst); ok {
			t = lp
		} else if n := len(inv.Payments); n > 0 {
			sort.Slice(inv.Payments, func(i, j int) bool { return inv.Payments[i].Date.Before(inv.Payments[j].Date) })
			t = inv.Payments[n-1].Date
		}
		inv.PaidAt = &t
	}
	if inv.Status == invoicing.StatusVoid {
		t := issue
		inv.VoidedAt = &t
	}
	if inv.ExchangeRate == 0 && cur != im.Home {
		flags = append(flags, fmt.Sprintf("No exchange rate to %s — open the document and set one so it counts in reports", im.Home))
	}
	flag := strings.Join(flags, ". ")
	inv.ImportFlag = flag
	return inv, flag, nil
}

func (im *Importer) addRecon(d Doc, localID string) {
	if d.Kind != KindInvoice {
		return
	}
	st := mapStatus(d.Kind, d.Status)
	if st == invoicing.StatusDraft || st == invoicing.StatusVoid || im.reconIDs[localID] {
		return
	}
	cur := strings.ToUpper(firstNonEmpty(d.Currency, im.SrcBase, im.Home))
	total, bal := d.Total, d.Balance
	if (total == 0 && len(d.Lines) > 0) || d.BalanceUnknown {
		// The file gave no total/balance: they were worked out on import.
		var row struct{ Total, BalanceDue float64 }
		im.DB.Table("billing_invoices").Select("total, balance_due").Where("id = ?", localID).Take(&row)
		if total == 0 && len(d.Lines) > 0 {
			total = row.Total
		}
		if d.BalanceUnknown {
			bal = row.BalanceDue
		}
	}
	r := im.recon[cur]
	if r == nil {
		r = &Reconciliation{Currency: cur}
		im.recon[cur] = r
	}
	r.Documents++
	r.SourceTotal += total
	r.SourceOutstanding += bal
	im.reconIDs[localID] = true
}

// Reconcile compares the source totals with what is stored here now.
func (im *Importer) Reconcile() []Reconciliation {
	var out []Reconciliation
	ids := make([]string, 0, len(im.reconIDs))
	for id := range im.reconIDs {
		ids = append(ids, id)
	}
	local := map[string][2]float64{}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		var rows []struct {
			Currency   string
			Total, Bal float64
		}
		im.DB.Table("billing_invoices").Select("UPPER(currency) AS currency, SUM(total) AS total, SUM(balance_due) AS bal").
			Where("id IN ? AND deleted_at IS NULL", ids[start:end]).Group("UPPER(currency)").Scan(&rows)
		for _, r := range rows {
			v := local[r.Currency]
			v[0] += r.Total
			v[1] += r.Bal
			local[r.Currency] = v
		}
	}
	for cur, r := range im.recon {
		r.SourceTotal = invoicing.RoundCur(r.SourceTotal, cur)
		r.SourceOutstanding = invoicing.RoundCur(r.SourceOutstanding, cur)
		r.LocalTotal = invoicing.RoundCur(local[cur][0], cur)
		r.LocalOutstanding = invoicing.RoundCur(local[cur][1], cur)
		r.OK = math.Abs(r.SourceTotal-r.LocalTotal) < 0.01 && math.Abs(r.SourceOutstanding-r.LocalOutstanding) < 0.01
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// ContinueNumbering moves the next invoice/quote numbers past the imported ones.
func (im *Importer) ContinueNumbering() []*invoicing.Numbering {
	var out []*invoicing.Numbering
	if !im.Opts.continueNumbering() {
		return out
	}
	for _, t := range []string{invoicing.TypeInvoice, invoicing.TypeQuote} {
		if len(im.numbers[t]) == 0 {
			continue
		}
		var n *invoicing.Numbering
		err := im.DB.Transaction(func(tx *gorm.DB) error {
			var err error
			n, err = invoicing.ContinueNumbering(tx, im.Org, t, im.numbers[t])
			return err
		})
		if err != nil {
			im.warn(EntSettings, "", "Numbering", "Could not update the next "+t+" number: "+err.Error())
			continue
		}
		if n != nil {
			out = append(out, n)
		}
	}
	return out
}

// Flags is the number of documents whose totals or balance were kept from the source.
func (im *Importer) Flags() int { return im.flags }

// WriteHistory replaces the imported history (comments, emails) of a document.
func (im *Importer) WriteHistory(d Doc, localID string, comments []Comment) string {
	ext := d.Kind + ":" + d.ExtID
	hash := hashOf(comments)
	ref := im.ref("history", ext)
	if ref != nil && ref.Hash == hash && ref.LocalID == localID {
		im.tally(EntHistory, outUnchanged)
		return outUnchanged
	}
	inv := &invoicing.Invoice{ID: localID, OrganizationID: im.Org}
	err := im.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("invoice_id = ? AND type IN ?", localID, []string{"imported_email", "imported_history"}).Delete(&invoicing.Activity{}).Error; err != nil {
			return err
		}
		for _, c := range comments {
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			at, ok := parseDay(c.Date, im.DayFirst)
			if !ok {
				at = im.now()
			}
			typ := "imported_history"
			msg := "Imported from Zoho: " + c.Text
			if c.Kind == "email" {
				typ = "imported_email"
				msg = fmt.Sprintf("Imported from Zoho: %s (on %s)", strings.TrimSuffix(c.Text, "."), at.Format("2 Jan 2006"))
			}
			if c.By != "" {
				msg += " — by " + c.By
			}
			if err := im.activity(tx, inv, typ, msg, at); err != nil {
				return err
			}
		}
		return im.saveRef(tx, ref, "history", ext, localID, d.Number, hash, "", true)
	})
	if err != nil {
		im.fail(EntHistory, ext, d.Number, 0, err)
		return "failed"
	}
	if ref == nil {
		im.tally(EntHistory, outCreated)
		return outCreated
	}
	im.tally(EntHistory, outUpdated)
	return outUpdated
}

// HasRef reports whether something was imported for entity/ext before.
func (im *Importer) HasRef(entity, ext string) bool { return im.ref(entity, ext) != nil }

// SavePDF stores the source PDF as the document's "Original from Zoho".
func (im *Importer) SavePDF(d Doc, localID string, pdf []byte) {
	ext := d.Kind + ":" + d.ExtID
	name := firstNonEmpty(d.Number, "document") + ".pdf"
	ref := im.ref(entPDFRef, ext)
	err := im.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("invoice_id = ? AND source = ? AND kind = ?", localID, "zoho", "original").Delete(&invoicing.Attachment{}).Error; err != nil {
			return err
		}
		a := invoicing.Attachment{ID: uuid.NewString(), OrganizationID: im.Org, InvoiceID: localID, Name: cut(strings.ReplaceAll(name, "/", "-"), 250),
			ContentType: "application/pdf", Size: len(pdf), Source: "zoho", Kind: "original", Data: pdf, CreatedAt: im.now()}
		if err := tx.Create(&a).Error; err != nil {
			return err
		}
		return im.saveRef(tx, ref, entPDFRef, ext, a.ID, name, hashOf(len(pdf)), "", true)
	})
	if err != nil {
		im.fail(EntPDFs, ext, name, 0, err)
		return
	}
	if ref != nil {
		im.tally(EntPDFs, outUpdated)
	} else {
		im.tally(EntPDFs, outCreated)
	}
}

var errStop = errors.New("stopped")

// Unchanged records a document listed by the source that has not changed
// since it was last imported (its details are not fetched again).
func (im *Importer) Unchanged(d Doc, localID string) {
	im.load()
	if d.Kind == KindInvoice || d.Kind == KindQuote {
		im.numbers[docType(d.Kind)] = append(im.numbers[docType(d.Kind)], d.Number)
	}
	if d.Kind == KindQuote {
		im.quoteLocal[d.ExtID] = localID
	}
	im.tally(entForKind(d.Kind), outUnchanged)
	im.addRecon(d, localID)
}

func splitNum(n string) (string, int, int, bool) { return invoicing.SplitNumber(n) }
