package mpos

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

const (
	org1 = "org-mpos-1"
	org2 = "org-mpos-2"
)

type env struct {
	t  *testing.T
	db *gorm.DB
	r  *gin.Engine
}

// newEnv: a fresh schema on TEST_DATABASE_URL with the POS tables, products and users.
func newEnv(t *testing.T) *env {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := "mpos_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec("CREATE SCHEMA " + schema)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := db.DB(); err == nil {
			d.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if d, err := admin.DB(); err == nil {
			d.Close()
		}
	})
	if err := db.AutoMigrate(&models.POSUserRef{}, &models.Product{}, &models.POSTransaction{}, &models.POSItem{}, &models.POSPayment{}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	bases := map[string]string{"mgr": "manager", "own": "admin", "brian": "staff", "amina": "staff"}
	h := &Handler{DB: db, Base: func(org, user, legacy string) string { return bases[user] }}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
	})
	h.Register(g)
	for _, u := range []models.POSUserRef{{ID: "mgr", FirstName: "Wanjiru", LastName: "Kamau"}, {ID: "own", FirstName: "Kennedy", LastName: "K"},
		{ID: "brian", FirstName: "Brian", LastName: "Otieno"}, {ID: "amina", FirstName: "Amina", LastName: "Hassan"}} {
		db.Create(&u)
	}
	return &env{t: t, db: db, r: r}
}

func (e *env) product(org, name, barcode string, active bool) string {
	p := models.Product{ID: uuid.NewString(), OrganizationID: org, Name: name, SKU: strings.ToUpper(strings.ReplaceAll(name, " ", "-")) + "-" + uuid.NewString()[:4], Barcode: barcode, SellingPrice: 100, IsActive: true}
	if err := e.db.Create(&p).Error; err != nil {
		e.t.Fatal(err)
	}
	if !active {
		e.db.Model(&models.Product{}).Where("id = ?", p.ID).Update("is_active", false)
	}
	return p.ID
}

type line struct {
	pid  string
	qty  int
	disc float64
}

type saleOpt struct {
	org, user, loc, device, label, status, voidedBy string
	at                                              time.Time
	total, discount, change                         float64
	pays                                            map[string]float64
	lines                                           []line
}

func (e *env) sale(o saleOpt) string {
	if o.org == "" {
		o.org = org1
	}
	if o.status == "" {
		o.status = "completed"
	}
	if o.at.IsZero() {
		o.at = time.Now().Add(-time.Hour)
	}
	id := uuid.NewString()
	t := models.POSTransaction{ID: id, OrganizationID: o.org, CashierID: o.user, Type: "sale", Status: o.status, SubTotal: o.total, TotalAmount: o.total,
		TenderAmount: o.total + o.change, ChangeAmount: o.change, DiscountAmount: o.discount, DeviceID: o.device, DeviceLabel: o.label, LocationID: o.loc}
	if o.voidedBy != "" {
		vb := o.voidedBy
		t.VoidedBy = &vb
		at := o.at.Add(time.Minute)
		t.VoidedAt = &at
	}
	if err := e.db.Omit("Customer", "Cashier", "Items", "Payments").Create(&t).Error; err != nil {
		e.t.Fatal(err)
	}
	e.db.Model(&models.POSTransaction{}).Where("id = ?", id).Update("created_at", o.at)
	for _, l := range o.lines {
		pid := l.pid
		e.db.Create(&models.POSItem{TransactionID: id, ProductID: &pid, ItemName: "x", ItemType: "product", Quantity: l.qty, UnitPrice: 10, TotalPrice: float64(l.qty) * 10, DiscountAmount: l.disc})
	}
	for m, a := range o.pays {
		e.db.Create(&models.POSPayment{TransactionID: id, Method: m, Amount: a, Status: "completed"})
	}
	return id
}

func (e *env) do(method, path, user, role string, body interface{}, org string) (int, map[string]interface{}) {
	var rd *bytes.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case *multipartBody:
		rd = bytes.NewReader(b.buf.Bytes())
		ct = b.ct
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", ct)
	if org == "" {
		org = org1
	}
	req.Header.Set("X-Org", org)
	req.Header.Set("X-User", user)
	req.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Header().Get("Content-Type") == "image/jpeg" {
		out["_bytes"] = float64(w.Body.Len())
	}
	return w.Code, out
}

type multipartBody struct {
	buf *bytes.Buffer
	ct  string
}

func photoBody(t *testing.T, data []byte, kind, note string) *multipartBody {
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, _ := mw.CreateFormFile("photo", "sale.jpg")
	fw.Write(data)
	mw.WriteField("kind", kind)
	mw.WriteField("note", note)
	mw.Close()
	return &multipartBody{buf: buf, ct: mw.FormDataContentType()}
}

func jpegBytes(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 70}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// ------------------------------------------------------------------ pure

func TestCleaners(t *testing.T) {
	if CleanDeviceID("dev_abc123-XYZ") != "dev_abc123-XYZ" || CleanDeviceID("short") != "" || CleanDeviceID("bad id with spaces") != "" || CleanDeviceID(strings.Repeat("a", 65)) != "" {
		t.Fatal("device id")
	}
	if got := CleanDeviceLabel("  Brian's\n\tphone <b>\x00 "); got != "Brian's phone b" {
		t.Fatalf("label %q", got)
	}
	if got := CleanDeviceLabel(strings.Repeat("é", 80)); len([]rune(got)) != 60 {
		t.Fatalf("label length %d", len([]rune(got)))
	}
	for _, c := range []string{"5901234123457", "036000291452", "96385074", "10614141000415"} {
		if !ValidGTIN(c) {
			t.Fatal("gtin", c)
		}
	}
	for _, c := range []string{"5901234123458", "036000291453", "1234", "abc"} {
		if ValidGTIN(c) {
			t.Fatal("bad gtin accepted", c)
		}
	}
	keys := strings.Join(BarcodeKeys("036000291452"), ",")
	if !strings.Contains(keys, "0036000291452") || !strings.Contains(keys, "00036000291452") {
		t.Fatal("upc keys", keys)
	}
	if k := BarcodeKeys("POS-FW"); len(k) != 1 || k[0] != "pos-fw" {
		t.Fatal("non-gtin keys", k)
	}
	if _, err := CleanBarcode("  "); err == nil {
		t.Fatal("empty barcode accepted")
	}
	if _, err := CleanBarcode("abc\x01"); err == nil {
		t.Fatal("control char accepted")
	}
	if v, err := CleanBarcode(" 6161101600118 "); err != nil || v != "6161101600118" {
		t.Fatal("trim barcode", v, err)
	}
}

// ------------------------------------------------------------------ quick picks

func TestQuickPicks(t *testing.T) {
	e := newEnv(t)
	x := e.product(org1, "Tusker", "", true)
	y := e.product(org1, "Guinness", "", true)
	z := e.product(org1, "Jameson tot", "", true)
	w := e.product(org1, "White Cap", "", true)
	v := e.product(org1, "Voided only", "", true)
	u := e.product(org1, "Old sale", "", true)
	in := e.product(org1, "Inactive", "", false)
	other := e.product(org2, "Other org", "", true)
	for i := 0; i < 3; i++ {
		e.sale(saleOpt{user: "brian", loc: "bar1", lines: []line{{x, 2, 0}}})
	}
	e.sale(saleOpt{user: "brian", loc: "bar1", lines: []line{{y, 1, 0}, {in, 1, 0}}})
	for i := 0; i < 5; i++ {
		e.sale(saleOpt{user: "amina", loc: "bar1", lines: []line{{z, 1, 0}}})
	}
	for i := 0; i < 9; i++ {
		e.sale(saleOpt{user: "amina", loc: "bar2", lines: []line{{w, 1, 0}}})
	}
	e.sale(saleOpt{user: "brian", loc: "bar1", status: "voided", lines: []line{{v, 9, 0}}})
	e.sale(saleOpt{user: "brian", loc: "bar1", at: time.Now().AddDate(0, 0, -40), lines: []line{{u, 9, 0}}})
	e.sale(saleOpt{org: org2, user: "brian", loc: "bar1", lines: []line{{other, 50, 0}}})

	picks, err := QuickPicks(e.db, org1, "brian", "bar1", time.Now(), 4)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, p := range picks {
		got = append(got, p.ProductID+":"+p.Source)
	}
	want := []string{x + ":mine", y + ":mine", z + ":bar", w + ":top"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("picks\n got %v\nwant %v", got, want)
	}
	if picks[0].Qty != 6 || picks[0].Sales != 3 {
		t.Fatalf("tusker stats %+v", picks[0])
	}
	// no bar (a shop): mine, then the shop's top sellers
	picks, _ = QuickPicks(e.db, org1, "amina", "", time.Now(), 3)
	if len(picks) != 3 || picks[0].ProductID != w || picks[0].Source != "mine" || picks[1].ProductID != z || picks[2].Source != "top" {
		t.Fatalf("shop picks %+v", picks)
	}
	// HTTP: a new person gets the top sellers
	code, res := e.do("GET", "/api/v1/pos/quick-picks?location_id=bar2&limit=2", "mgr", "manager", nil, "")
	if code != 200 || res["mine"].(float64) != 0 || len(res["picks"].([]interface{})) != 2 {
		t.Fatalf("http picks %d %v", code, res)
	}
	first := res["picks"].([]interface{})[0].(map[string]interface{})
	if first["product_id"] != w || first["source"] != "bar" {
		t.Fatalf("bar2 first %v", first)
	}
}

// ------------------------------------------------------------------ people summary

func TestPeopleSummary(t *testing.T) {
	e := newEnv(t)
	p := e.product(org1, "Tusker", "", true)
	e.sale(saleOpt{user: "brian", device: "dev-brian-phone", label: "Brian's phone", total: 100, discount: 10, change: 20, pays: map[string]float64{"cash": 120}, lines: []line{{p, 4, 5}}})
	e.sale(saleOpt{user: "brian", device: "dev-brian-phone", label: "Brian's phone", total: 50, pays: map[string]float64{"mobile_money": 50}, lines: []line{{p, 2, 0}}})
	e.sale(saleOpt{user: "brian", device: "", total: 30, status: "voided", voidedBy: "mgr", pays: map[string]float64{"cash": 30}, lines: []line{{p, 1, 0}}})
	e.sale(saleOpt{user: "amina", device: "dev-brian-phone", label: "Bar phone", total: 70, pays: map[string]float64{"card": 70}, lines: []line{{p, 3, 0}}})
	e.sale(saleOpt{user: "amina", device: "dev-till-1", label: "Front till", total: 40, pays: map[string]float64{"cash": 40}, at: time.Now().AddDate(0, 0, -3)})
	e.sale(saleOpt{org: org2, user: "brian", total: 999, pays: map[string]float64{"cash": 999}})

	s, err := PeopleSummary(e.db, org1, time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.People) != 3 {
		t.Fatalf("people %+v", s.People)
	}
	var b, a, m PersonTotals
	for _, x := range s.People {
		switch x.UserID {
		case "brian":
			b = x
		case "amina":
			a = x
		case "mgr":
			m = x
		}
	}
	if b.Name != "Brian Otieno" || b.SalesCount != 2 || b.SalesTotal != 150 || b.VoidedCount != 1 || b.VoidedTotal != 30 || b.ItemsSold != 6 {
		t.Fatalf("brian %+v", b)
	}
	if b.DiscountCount != 1 || b.DiscountTotal != 15 || b.CashTotal != 100 || b.MobileTotal != 50 || b.AverageSale != 75 {
		t.Fatalf("brian money %+v", b)
	}
	if len(b.Devices) != 2 || b.Devices[0].DeviceLabel != "Brian's phone" || b.Devices[0].SalesCount != 2 || b.Devices[1].DeviceID != "" || b.Devices[1].VoidedCount != 1 {
		t.Fatalf("brian devices %+v", b.Devices)
	}
	if a.SalesCount != 1 || a.CardTotal != 70 || m.VoidsDone != 1 || m.SalesCount != 0 {
		t.Fatalf("amina %+v mgr %+v", a, m)
	}
	// one physical phone used by two people; the label of its latest sale wins
	var phone *DeviceTotals
	for i := range s.Devices {
		if s.Devices[i].DeviceID == "dev-brian-phone" {
			phone = &s.Devices[i]
		}
	}
	if phone == nil || phone.SalesCount != 3 || phone.SalesTotal != 220 || strings.Join(phone.People, ",") != "Amina Hassan,Brian Otieno" {
		t.Fatalf("phone %+v", phone)
	}
	if s.Totals.SalesCount != 3 || s.Totals.SalesTotal != 220 || s.Totals.VoidedCount != 1 {
		t.Fatalf("totals %+v", s.Totals)
	}
	// HTTP: managers only, dates in the caller's zone
	if code, _ := e.do("GET", "/api/v1/pos/people-summary", "brian", "staff", nil, ""); code != 403 {
		t.Fatal("staff saw the summary", code)
	}
	day := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	code, res := e.do("GET", "/api/v1/pos/people-summary?start_date="+day+"&end_date="+time.Now().Format("2006-01-02"), "mgr", "manager", nil, "")
	if code != 200 {
		t.Fatal("summary", code, res)
	}
	tot := res["summary"].(map[string]interface{})["totals"].(map[string]interface{})
	if tot["sales_count"].(float64) != 4 {
		t.Fatalf("4 sales in 4 days, got %v", tot)
	}
	if code, _ := e.do("GET", "/api/v1/pos/people-summary?start_date=2026-02-30", "own", "admin", nil, ""); code != 400 {
		t.Fatal("bad date accepted", code)
	}
}

// ------------------------------------------------------------------ barcode links

func TestLinkBarcode(t *testing.T) {
	e := newEnv(t)
	tusker := e.product(org1, "Tusker 500ml", "", true)
	coke := e.product(org1, "Coke 300ml", "036000291452", true)
	jam := e.product(org1, "Jameson 750ml", "5011007003005", true)
	foreign := e.product(org2, "Other org", "", true)
	post := func(user, role string, body map[string]interface{}) (int, map[string]interface{}) {
		return e.do("POST", "/api/v1/pos/barcode-links", user, role, body, "")
	}
	if code, res := post("brian", "staff", map[string]interface{}{"product_id": tusker, "barcode": "6161101600118"}); code != 403 || !strings.Contains(res["error"].(string), "manager") {
		t.Fatal("staff linked a barcode", code, res)
	}
	code, res := post("mgr", "manager", map[string]interface{}{"product_id": tusker, "barcode": " 6161101600118 "})
	if code != 200 || res["product"].(map[string]interface{})["barcode"] != "6161101600118" {
		t.Fatal("link", code, res)
	}
	var bc string
	e.db.Table("products").Select("barcode").Where("id = ?", tusker).Scan(&bc)
	if bc != "6161101600118" {
		t.Fatal("not saved", bc)
	}
	// the EAN-13 spelling of Coke's UPC-A is taken
	code, res = post("mgr", "manager", map[string]interface{}{"product_id": tusker, "barcode": "0036000291452", "replace": true})
	if code != 409 || res["code"] != "BARCODE_TAKEN" || res["product"].(map[string]interface{})["name"] != "Coke 300ml" {
		t.Fatal("taken", code, res)
	}
	// Jameson already has a code
	code, res = post("own", "admin", map[string]interface{}{"product_id": jam, "barcode": "5011007003012"})
	if code != 409 || res["code"] != "PRODUCT_HAS_CODE" || res["current_barcode"] != "5011007003005" {
		t.Fatal("has code", code, res)
	}
	if code, _ = post("own", "admin", map[string]interface{}{"product_id": jam, "barcode": "5011007003012", "replace": true}); code != 200 {
		t.Fatal("replace", code)
	}
	// move Coke's code to Tusker
	code, res = post("mgr", "manager", map[string]interface{}{"product_id": tusker, "barcode": "036000291452", "replace": true, "move": true})
	if code != 200 || res["moved_from"].(map[string]interface{})["id"] != coke {
		t.Fatal("move", code, res)
	}
	e.db.Table("products").Select("barcode").Where("id = ?", coke).Scan(&bc)
	if bc != "" {
		t.Fatal("coke kept its code", bc)
	}
	// same code again: unchanged
	if code, res = post("mgr", "manager", map[string]interface{}{"product_id": tusker, "barcode": "036000291452"}); code != 200 || res["unchanged"] != true {
		t.Fatal("unchanged", code, res)
	}
	// another organisation's product
	if code, _ = post("mgr", "manager", map[string]interface{}{"product_id": foreign, "barcode": "123456"}); code != 404 {
		t.Fatal("cross-org link", code)
	}
	if code, _ = post("mgr", "manager", map[string]interface{}{"product_id": tusker, "barcode": "bad\x01"}); code != 400 {
		t.Fatal("bad barcode", code)
	}
}

// ------------------------------------------------------------------ photos

func TestSalePhotos(t *testing.T) {
	e := newEnv(t)
	mine := e.sale(saleOpt{user: "brian", total: 100})
	hers := e.sale(saleOpt{user: "amina", total: 100})
	old := e.sale(saleOpt{user: "brian", total: 100, at: time.Now().Add(-48 * time.Hour)})
	foreign := e.sale(saleOpt{org: org2, user: "brian", total: 100})
	jp := jpegBytes(t, 320, 200)
	url := func(id string) string { return "/api/v1/pos/transactions/" + id + "/photos" }

	code, res := e.do("POST", url(mine), "brian", "staff", photoBody(t, jp, "payment", "M-Pesa QK12AB34CD\nfrom Jane"), "")
	if code != 201 {
		t.Fatal("staff photo on own sale", code, res)
	}
	ph := res["photo"].(map[string]interface{})
	if ph["kind"] != "payment" || ph["width"].(float64) != 320 || ph["note"] != "M-Pesa QK12AB34CD from Jane" || ph["uploader_name"] != "Brian Otieno" {
		t.Fatalf("photo %v", ph)
	}
	if _, ok := ph["data"]; ok {
		t.Fatal("bytes in JSON")
	}
	pid := ph["id"].(string)
	if code, _ = e.do("POST", url(hers), "brian", "staff", photoBody(t, jp, "other", ""), ""); code != 403 {
		t.Fatal("staff photo on someone else's sale", code)
	}
	if code, _ = e.do("POST", url(old), "brian", "staff", photoBody(t, jp, "other", ""), ""); code != 403 {
		t.Fatal("staff photo on a 2-day-old sale", code)
	}
	if code, _ = e.do("POST", url(old), "mgr", "manager", photoBody(t, jp, "damage", "broken bottle"), ""); code != 201 {
		t.Fatal("manager photo on old sale", code)
	}
	if code, _ = e.do("POST", url(foreign), "mgr", "manager", photoBody(t, jp, "other", ""), ""); code != 404 {
		t.Fatal("cross-org photo", code)
	}
	png := []byte("\x89PNG\r\n\x1a\n0000")
	if code, _ = e.do("POST", url(mine), "brian", "staff", photoBody(t, png, "other", ""), ""); code != 415 {
		t.Fatal("png accepted", code)
	}
	fake := append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{1}, 100)...)
	if code, _ = e.do("POST", url(mine), "brian", "staff", photoBody(t, fake, "other", ""), ""); code != 415 {
		t.Fatal("broken jpeg accepted", code)
	}
	big := append(append([]byte{}, jp...), bytes.Repeat([]byte{0}, MaxPhotoBytes)...)
	if code, _ = e.do("POST", url(mine), "brian", "staff", photoBody(t, big, "other", ""), ""); code != 413 {
		t.Fatal("too large accepted", code)
	}
	for i := 0; i < 4; i++ {
		if code, _ = e.do("POST", url(mine), "mgr", "manager", photoBody(t, jp, "other", ""), ""); code != 201 {
			t.Fatal("photo", i, code)
		}
	}
	if code, _ = e.do("POST", url(mine), "mgr", "manager", photoBody(t, jp, "other", ""), ""); code != 409 {
		t.Fatal("6th photo accepted", code)
	}
	var n int
	e.db.Table("pos_transactions").Select("photo_count").Where("id = ?", mine).Scan(&n)
	if n != 5 {
		t.Fatal("photo_count", n)
	}
	// lists: staff see their own, managers all
	_, res = e.do("GET", url(mine), "brian", "staff", nil, "")
	if len(res["photos"].([]interface{})) != 1 || res["can_view_all"] != false {
		t.Fatal("staff list", res)
	}
	_, res = e.do("GET", url(mine), "mgr", "manager", nil, "")
	if len(res["photos"].([]interface{})) != 5 || res["total"].(float64) != 5 {
		t.Fatal("manager list", res)
	}
	// the image
	code, res = e.do("GET", "/api/v1/pos/sale-photos/"+pid, "mgr", "manager", nil, "")
	if code != 200 || int(res["_bytes"].(float64)) != len(jp) {
		t.Fatal("get photo", code)
	}
	if code, _ = e.do("GET", "/api/v1/pos/sale-photos/"+pid, "amina", "staff", nil, ""); code != 403 {
		t.Fatal("other staff opened the photo", code)
	}
	if code, _ = e.do("GET", "/api/v1/pos/sale-photos/"+pid, "mgr", "manager", nil, org2); code != 404 {
		t.Fatal("other org opened the photo", code)
	}
	if code, _ = e.do("DELETE", "/api/v1/pos/sale-photos/"+pid, "brian", "staff", nil, ""); code != 403 {
		t.Fatal("staff deleted", code)
	}
	if code, _ = e.do("DELETE", "/api/v1/pos/sale-photos/"+pid, "own", "admin", nil, ""); code != 200 {
		t.Fatal("delete", code)
	}
	e.db.Table("pos_transactions").Select("photo_count").Where("id = ?", mine).Scan(&n)
	if n != 4 {
		t.Fatal("photo_count after delete", n)
	}
}

// ------------------------------------------------------------------ idempotency index

func TestClientRefUnique(t *testing.T) {
	e := newEnv(t)
	mk := func(ref, org string) error {
		return e.db.Omit("Customer", "Cashier", "Items", "Payments").Create(&models.POSTransaction{OrganizationID: org, CashierID: "brian", SubTotal: 1, TotalAmount: 1, TenderAmount: 1, ClientRef: ref}).Error
	}
	if err := mk("sale-ref-0001", org1); err != nil {
		t.Fatal(err)
	}
	if err := mk("sale-ref-0001", org1); err == nil {
		t.Fatal("duplicate client_ref accepted")
	}
	if err := mk("sale-ref-0001", org2); err != nil {
		t.Fatal("same key in another org", err)
	}
	if mk("", org1) != nil || mk("", org1) != nil {
		t.Fatal("empty keys must not collide")
	}
}
