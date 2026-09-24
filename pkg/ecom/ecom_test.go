package ecom

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
)

func TestNormalizeStoreURL(t *testing.T) {
	good := map[string][2]string{
		"acme.myshopify.com":                  {Shopify, "https://acme.myshopify.com"},
		"https://acme.myshopify.com/admin":    {Shopify, "https://acme.myshopify.com"},
		"shop.example.com":                    {WooCommerce, "https://shop.example.com"},
		"https://example.com/store/wp-admin/": {WooCommerce, "https://example.com/store"},
		"https://Example.com/wp-json/wc/v3":   {WooCommerce, "https://example.com"},
		"https://example.com:443/":            {WooCommerce, "https://example.com"},
	}
	for in, want := range good {
		got, err := NormalizeStoreURL(want[0], in, false)
		if err != nil || got != want[1] {
			t.Errorf("%s: got %q, %v want %q", in, got, err, want[1])
		}
	}
	bad := map[string]string{
		"http://shop.example.com":          WooCommerce, // not https
		"https://127.0.0.1":                WooCommerce, // IP literal
		"https://[::1]/":                   WooCommerce,
		"https://localhost":                WooCommerce,
		"https://intranet":                 WooCommerce, // single label
		"https://db.internal":              WooCommerce,
		"https://shop.example.com:8443":    WooCommerce, // custom port
		"https://user:pw@shop.example.com": WooCommerce,
		"https://shop.example.com":         Shopify, // not myshopify
		"ftp://shop.example.com":           WooCommerce,
		"":                                 WooCommerce,
	}
	for in, platform := range bad {
		if got, err := NormalizeStoreURL(platform, in, false); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.5.4", "169.254.169.254", "100.64.0.1", "::1", "fe80::1", "fd00::1", "0.0.0.0"} {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "23.227.38.65", "2606:4700::1"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as private", s)
		}
	}
}

func TestHardenedClientRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	cl := NewClient(false)
	_, err := cl.Test(context.Background(), Creds{Platform: Shopify, StoreURL: srv.URL, APIKey: "x"})
	if err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("expected private network refusal, got %v", err)
	}
}

func fakeClient(srv *httptest.Server) *Client {
	cl := NewClient(true)
	cl.HTTP = srv.Client()
	return cl
}

func TestShopifyImport(t *testing.T) {
	var srvURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Shopify-Access-Token") != "shpat_secret" {
			w.WriteHeader(401)
			w.Write([]byte(`{"errors":"[API] Invalid API key or access token"}`))
			return
		}
		switch r.URL.Path {
		case "/admin/api/2024-10/shop.json":
			w.Write([]byte(`{"shop":{"name":"Acme Store","currency":"AUD"}}`))
		case "/admin/api/2024-10/orders.json":
			if r.URL.Query().Get("page_info") == "" {
				if r.URL.Query().Get("status") != "any" {
					t.Error("status=any missing")
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s/admin/api/2024-10/orders.json?limit=250&page_info=abc>; rel="next"`, srvURL))
				w.Write([]byte(`{"orders":[{"id":450789469,"name":"#1001","email":"bob@example.com","created_at":"2026-09-20T10:00:00+10:00",
					"currency":"aud","total_price":"115.00","subtotal_price":"100.00","total_tax":"10.00","financial_status":"paid","fulfillment_status":null,
					"customer":{"first_name":"Bob","last_name":"Norman"},
					"line_items":[{"title":"Mug","sku":"MUG-1","quantity":2,"price":"50.00"}],"shipping_lines":[{"price":"5.00"}]}]}`))
				return
			}
			w.Write([]byte(`{"orders":[
				{"id":2,"name":"#1002","created_at":"2026-09-21T10:00:00Z","currency":"AUD","total_price":"10.00","financial_status":"paid","fulfillment_status":"fulfilled","line_items":[]},
				{"id":3,"name":"#1003","created_at":"2026-09-21T11:00:00Z","cancelled_at":"2026-09-21T12:00:00Z","currency":"AUD","total_price":"5.00","financial_status":"voided","line_items":[]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	cl := fakeClient(srv)
	cr := Creds{Platform: Shopify, StoreURL: srv.URL, APIKey: "shpat_secret"}

	name, err := cl.Test(context.Background(), cr)
	if err != nil || name != "Acme Store" {
		t.Fatal(name, err)
	}
	orders, err := cl.FetchOrders(context.Background(), cr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 3 {
		t.Fatalf("got %d orders", len(orders))
	}
	o := orders[0]
	if o.ExternalID != "450789469" || o.Number != "#1001" || o.CustomerName != "Bob Norman" || o.Currency != "AUD" ||
		o.Total != 115 || o.Subtotal != 100 || o.Tax != 10 || o.Shipping != 5 || o.ItemsCount != 2 || o.Status != "processing" || o.Items[0].Total != 100 {
		t.Fatalf("mapping %+v", o)
	}
	if !o.PlacedAt.Equal(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("placed", o.PlacedAt)
	}
	if orders[1].Status != "completed" || orders[2].Status != "cancelled" {
		t.Fatal("status mapping", orders[1].Status, orders[2].Status)
	}

	_, err = cl.Test(context.Background(), Creds{Platform: Shopify, StoreURL: srv.URL, APIKey: "wrong"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(ae.Msg, "rejected the API credentials") {
		t.Fatal("expected credential error, got", err)
	}
}

func TestWooCommerceImport(t *testing.T) {
	var sawModified string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, okAuth := r.BasicAuth()
		if !okAuth || u != "ck_key" || p != "cs_secret" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/shop/wp-json/wc/v3/orders" {
			w.WriteHeader(404)
			return
		}
		sawModified = r.URL.Query().Get("modified_after")
		w.Header().Set("X-WP-TotalPages", "2")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Write([]byte(`[{"id":727,"number":"727","status":"processing","currency":"USD","date_created_gmt":"2026-09-22T08:30:00","date_paid_gmt":"2026-09-22T08:31:00",
				"total":"29.35","total_tax":"1.35","shipping_total":"10.00","billing":{"first_name":"John","last_name":"Doe","email":"john.doe@example.com"},
				"line_items":[{"name":"Woo Single #1","sku":"","quantity":2,"total":"6.00"},{"name":"Ninja Silhouette","sku":"NS","quantity":1,"total":"12.00"}]}]`))
		case "2":
			w.Write([]byte(`[{"id":728,"number":"728","status":"on-hold","currency":"USD","date_created_gmt":"2026-09-23T08:30:00","total":"5.00","line_items":[]},
				{"id":729,"number":"729","status":"refunded","currency":"USD","date_created_gmt":"2026-09-23T09:30:00","total":"7.00","line_items":[]}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	cl := fakeClient(srv)
	cr := Creds{Platform: WooCommerce, StoreURL: srv.URL + "/shop", APIKey: "ck_key", APISecret: "cs_secret"}
	if _, err := cl.Test(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	orders, err := cl.FetchOrders(context.Background(), cr, &since)
	if err != nil {
		t.Fatal(err)
	}
	if sawModified != "2026-09-01T00:00:00" {
		t.Fatal("modified_after", sawModified)
	}
	if len(orders) != 3 {
		t.Fatalf("got %d", len(orders))
	}
	o := orders[0]
	if o.ExternalID != "727" || o.Number != "#727" || o.CustomerName != "John Doe" || o.Total != 29.35 || o.Subtotal != 18 ||
		o.ItemsCount != 3 || o.Status != "processing" || o.PaymentStatus != "paid" || o.Shipping != 10 {
		t.Fatalf("mapping %+v", o)
	}
	if orders[1].Status != "pending" || orders[1].PaymentStatus != "unpaid" || orders[2].Status != "refunded" {
		t.Fatal("status mapping")
	}
	cr.APISecret = "nope"
	if _, err := cl.Test(context.Background(), cr); err == nil {
		t.Fatal("bad secret accepted")
	}
}

func TestMask(t *testing.T) {
	if Mask("shpat_1234567890abcd") != "shpat_••••abcd" || Mask("ck_99998888") != "ck_••••8888" || Mask("abc") != "••••" {
		t.Fatal(Mask("shpat_1234567890abcd"), Mask("ck_99998888"))
	}
}

func TestSyncUpsertsWithoutDuplicates(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skip(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	status := "processing"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-WP-TotalPages", "1")
		fmt.Fprintf(w, `[{"id":1,"number":"1","status":%q,"currency":"AUD","date_created_gmt":"2026-09-22T08:30:00","total":"10.00","line_items":[]},
			{"id":2,"number":"2","status":"completed","currency":"AUD","date_created_gmt":"2026-09-22T09:30:00","total":"20.00","line_items":[]}]`, status)
	}))
	defer srv.Close()
	enc := encryption.NewEncryptionService()
	h := &Handler{DB: db, Client: fakeClient(srv), Enc: enc}
	k, _ := enc.Encrypt("ck_a")
	s, _ := enc.Encrypt("cs_b")
	conn := Connection{ID: "ecom-test-conn", OrgID: "ecom-test-org", Platform: WooCommerce, Name: "T", StoreURL: srv.URL, APIKeyEnc: k, APISecretEnc: s}
	db.Exec("DELETE FROM ecom_orders WHERE connection_id = ?", conn.ID)
	db.Exec("DELETE FROM ecom_connections WHERE id = ?", conn.ID)
	db.Create(&conn)

	created, updated, err := h.Sync(context.Background(), &conn)
	if err != nil || created != 2 || updated != 0 {
		t.Fatal(created, updated, err)
	}
	status = "completed"
	created, updated, err = h.Sync(context.Background(), &conn)
	if err != nil || created != 0 || updated != 2 {
		t.Fatal(created, updated, err)
	}
	var rows []Order
	db.Where("connection_id = ?", conn.ID).Order("external_id").Find(&rows)
	if len(rows) != 2 || rows[0].Status != "completed" || conn.OrdersImported != 2 || conn.LastSyncAt == nil {
		t.Fatalf("rows %+v conn %+v", rows, conn)
	}
}
