package ecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Platforms.
const (
	Shopify     = "shopify"
	WooCommerce = "woocommerce"
)

// ShopifyAPIVersion is the Admin REST API version we call.
const ShopifyAPIVersion = "2024-10"

// Creds are the decrypted connection details.
type Creds struct {
	Platform  string
	StoreURL  string // normalised https base URL, no trailing slash
	APIKey    string // Shopify Admin API access token, or WooCommerce consumer key
	APISecret string // WooCommerce consumer secret
}

// ImportedOrder is a store order mapped to our schema.
type ImportedOrder struct {
	ExternalID     string
	Number         string
	CustomerName   string
	CustomerEmail  string
	Status         string // pending | processing | completed | cancelled | refunded
	PlatformStatus string
	PaymentStatus  string
	Currency       string
	Subtotal       float64
	Tax            float64
	Shipping       float64
	Total          float64
	ItemsCount     int
	Items          []OrderItem
	PlacedAt       time.Time
}

// OrderItem is one order line.
type OrderItem struct {
	Name     string  `json:"name"`
	SKU      string  `json:"sku"`
	Quantity int     `json:"quantity"`
	Total    float64 `json:"total"`
}

// Client talks to store APIs with SSRF protection: only https, only public
// addresses (checked at dial time, so DNS rebinding can't bypass it).
type Client struct {
	HTTP         *http.Client
	AllowPrivate bool // tests / local development only
	MaxPages     int
}

// NewClient returns a hardened client. allowPrivate permits http and private
// addresses and must only be used for tests or local development.
func NewClient(allowPrivate bool) *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !isPublicIP(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	if !allowPrivate {
		// A proxy would bypass the dial-time address check.
		tr.Proxy = nil
	}
	c := &Client{AllowPrivate: allowPrivate, MaxPages: 20}
	c.HTTP = &http.Client{
		Transport: tr,
		Timeout:   45 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !allowPrivate {
				return errors.New("store redirected to a non-https address")
			}
			req.Header.Del("Authorization")
			req.Header.Del("X-Shopify-Access-Token")
			if len(via) > 0 && req.URL.Host == via[0].URL.Host {
				for k, v := range via[0].Header {
					req.Header[k] = v
				}
			}
			return nil
		},
	}
	return c
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() ||
		// carrier-grade NAT 100.64.0.0/10
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xC0 == 64))
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// NormalizeStoreURL validates a store address and returns its https base URL.
// Bare domains get https:// added. Shopify stores must use their
// .myshopify.com domain. allowPrivate relaxes the checks for tests.
func NormalizeStoreURL(platform, raw string, allowPrivate bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("Enter your store address")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("That doesn't look like a web address")
	}
	if u.Scheme != "https" && !(allowPrivate && u.Scheme == "http") {
		return "", errors.New("The store address must start with https://")
	}
	if u.User != nil {
		return "", errors.New("Don't include a username or password in the address")
	}
	host := strings.ToLower(u.Hostname())
	if !allowPrivate {
		if u.Port() != "" && u.Port() != "443" {
			return "", errors.New("Custom ports aren't supported; use the store's normal https address")
		}
		if net.ParseIP(host) != nil {
			return "", errors.New("Use the store's domain name, not an IP address")
		}
		if !hostRe.MatchString(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
			strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
			return "", errors.New("Enter a public store domain, e.g. shop.example.com")
		}
	}
	path := strings.TrimRight(u.Path, "/")
	if platform == Shopify {
		if !allowPrivate && !strings.HasSuffix(host, ".myshopify.com") {
			return "", errors.New("Use your store's .myshopify.com address (Settings → Domains in Shopify)")
		}
		path = "" // the Admin API lives at the root
	}
	if platform == WooCommerce {
		path = strings.TrimSuffix(path, "/wp-admin")
		path = strings.TrimSuffix(path, "/wp-json/wc/v3")
	}
	hostport := host
	if u.Port() != "" && u.Port() != "443" {
		hostport = net.JoinHostPort(host, u.Port())
	}
	return u.Scheme + "://" + hostport + path, nil
}

// APIError is a readable failure from a store API.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func (cl *Client) get(ctx context.Context, cr Creds, full string, out interface{}) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DASYIN-ERP/1.0")
	switch cr.Platform {
	case Shopify:
		req.Header.Set("X-Shopify-Access-Token", cr.APIKey)
	case WooCommerce:
		req.SetBasicAuth(cr.APIKey, cr.APISecret)
	}
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && strings.Contains(ue.Error(), "non-public address") {
			return nil, &APIError{Msg: "That store address points to a private network and can't be used"}
		}
		if errors.As(err, &ue) && ue.Timeout() {
			return nil, &APIError{Msg: "The store took too long to respond"}
		}
		return nil, &APIError{Msg: "Couldn't reach the store: " + shortErr(err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, &APIError{Msg: "The store's response was cut off"}
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, &APIError{Status: resp.StatusCode, Msg: "The store rejected the API credentials. Check the key and its permissions (read orders)."}
	case resp.StatusCode == 404:
		return nil, &APIError{Status: 404, Msg: "The store API wasn't found at that address. Check the URL" + map[bool]string{true: " and that the WooCommerce REST API is enabled", false: ""}[cr.Platform == WooCommerce] + "."}
	case resp.StatusCode == 429:
		return nil, &APIError{Status: 429, Msg: "The store is rate-limiting requests. Try again in a minute."}
	case resp.StatusCode >= 400:
		return nil, &APIError{Status: resp.StatusCode, Msg: fmt.Sprintf("The store returned an error (HTTP %d)", resp.StatusCode)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, &APIError{Msg: "The store sent a response we couldn't read. Is this the right platform?"}
	}
	return resp.Header, nil
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	return s
}

// Test checks the credentials and returns the store's display name when known.
func (cl *Client) Test(ctx context.Context, cr Creds) (string, error) {
	switch cr.Platform {
	case Shopify:
		var out struct {
			Shop struct {
				Name     string `json:"name"`
				Currency string `json:"currency"`
			} `json:"shop"`
		}
		if _, err := cl.get(ctx, cr, cr.StoreURL+"/admin/api/"+ShopifyAPIVersion+"/shop.json", &out); err != nil {
			return "", err
		}
		return out.Shop.Name, nil
	case WooCommerce:
		var out []json.RawMessage
		if _, err := cl.get(ctx, cr, cr.StoreURL+"/wp-json/wc/v3/orders?per_page=1", &out); err != nil {
			return "", err
		}
		return "", nil
	}
	return "", errors.New("unknown platform")
}

// FetchOrders imports orders changed since `since` (all when nil).
func (cl *Client) FetchOrders(ctx context.Context, cr Creds, since *time.Time) ([]ImportedOrder, error) {
	switch cr.Platform {
	case Shopify:
		return cl.shopifyOrders(ctx, cr, since)
	case WooCommerce:
		return cl.wooOrders(ctx, cr, since)
	}
	return nil, errors.New("unknown platform")
}

// ---------------------------------------------------------------------------
// Shopify

type money string

func (m money) f() float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(m)), 64)
	return v
}

type shopifyOrder struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	OrderNumber       int64      `json:"order_number"`
	Email             string     `json:"email"`
	CreatedAt         time.Time  `json:"created_at"`
	CancelledAt       *time.Time `json:"cancelled_at"`
	Currency          string     `json:"currency"`
	TotalPrice        money      `json:"total_price"`
	SubtotalPrice     money      `json:"subtotal_price"`
	TotalTax          money      `json:"total_tax"`
	FinancialStatus   string     `json:"financial_status"`
	FulfillmentStatus *string    `json:"fulfillment_status"`
	Customer          *struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Email     string `json:"email"`
	} `json:"customer"`
	LineItems []struct {
		Title    string `json:"title"`
		SKU      string `json:"sku"`
		Quantity int    `json:"quantity"`
		Price    money  `json:"price"`
	} `json:"line_items"`
	ShippingLines []struct {
		Price money `json:"price"`
	} `json:"shipping_lines"`
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (cl *Client) shopifyOrders(ctx context.Context, cr Creds, since *time.Time) ([]ImportedOrder, error) {
	q := url.Values{"status": {"any"}, "limit": {"250"}}
	if since != nil {
		q.Set("updated_at_min", since.UTC().Format(time.RFC3339))
	}
	next := cr.StoreURL + "/admin/api/" + ShopifyAPIVersion + "/orders.json?" + q.Encode()
	base, _ := url.Parse(cr.StoreURL)
	var out []ImportedOrder
	for page := 0; next != "" && page < cl.maxPages(); page++ {
		var body struct {
			Orders []shopifyOrder `json:"orders"`
		}
		hdr, err := cl.get(ctx, cr, next, &body)
		if err != nil {
			return out, err
		}
		for _, o := range body.Orders {
			out = append(out, mapShopify(o))
		}
		next = ""
		if m := linkNextRe.FindStringSubmatch(hdr.Get("Link")); m != nil {
			if u, err := url.Parse(m[1]); err == nil && u.Host == base.Host {
				next = u.String()
			}
		}
	}
	return out, nil
}

func mapShopify(o shopifyOrder) ImportedOrder {
	io := ImportedOrder{
		ExternalID: strconv.FormatInt(o.ID, 10), Number: o.Name, CustomerEmail: o.Email,
		Currency: strings.ToUpper(o.Currency), Total: o.TotalPrice.f(), Subtotal: o.SubtotalPrice.f(), Tax: o.TotalTax.f(),
		PaymentStatus: o.FinancialStatus, PlacedAt: o.CreatedAt,
	}
	if io.Number == "" {
		io.Number = "#" + strconv.FormatInt(o.OrderNumber, 10)
	}
	if o.Customer != nil {
		io.CustomerName = strings.TrimSpace(o.Customer.FirstName + " " + o.Customer.LastName)
		if io.CustomerEmail == "" {
			io.CustomerEmail = o.Customer.Email
		}
	}
	for _, s := range o.ShippingLines {
		io.Shipping += s.Price.f()
	}
	for _, li := range o.LineItems {
		io.ItemsCount += li.Quantity
		io.Items = append(io.Items, OrderItem{Name: li.Title, SKU: li.SKU, Quantity: li.Quantity, Total: round2(li.Price.f() * float64(li.Quantity))})
	}
	fs := ""
	if o.FulfillmentStatus != nil {
		fs = *o.FulfillmentStatus
	}
	io.PlatformStatus = strings.Trim(o.FinancialStatus+"/"+fs, "/")
	switch {
	case o.CancelledAt != nil:
		io.Status = "cancelled"
	case o.FinancialStatus == "refunded":
		io.Status = "refunded"
	case fs == "fulfilled":
		io.Status = "completed"
	case o.FinancialStatus == "paid" || o.FinancialStatus == "partially_paid" || o.FinancialStatus == "partially_refunded":
		io.Status = "processing"
	default:
		io.Status = "pending"
	}
	return io
}

// ---------------------------------------------------------------------------
// WooCommerce

type wooOrder struct {
	ID             int64  `json:"id"`
	Number         string `json:"number"`
	Status         string `json:"status"`
	Currency       string `json:"currency"`
	DateCreatedGMT string `json:"date_created_gmt"`
	DatePaidGMT    string `json:"date_paid_gmt"`
	Total          money  `json:"total"`
	TotalTax       money  `json:"total_tax"`
	ShippingTotal  money  `json:"shipping_total"`
	Billing        struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Email     string `json:"email"`
	} `json:"billing"`
	LineItems []struct {
		Name     string `json:"name"`
		SKU      string `json:"sku"`
		Quantity int    `json:"quantity"`
		Total    money  `json:"total"`
	} `json:"line_items"`
}

func (cl *Client) wooOrders(ctx context.Context, cr Creds, since *time.Time) ([]ImportedOrder, error) {
	var out []ImportedOrder
	totalPages := 1
	for page := 1; page <= totalPages && page <= cl.maxPages(); page++ {
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "orderby": {"date"}, "order": {"desc"}}
		if since != nil {
			q.Set("modified_after", since.UTC().Format("2006-01-02T15:04:05"))
			q.Set("dates_are_gmt", "true")
		}
		var body []wooOrder
		hdr, err := cl.get(ctx, cr, cr.StoreURL+"/wp-json/wc/v3/orders?"+q.Encode(), &body)
		if err != nil {
			return out, err
		}
		for _, o := range body {
			out = append(out, mapWoo(o))
		}
		if tp, err := strconv.Atoi(hdr.Get("X-WP-TotalPages")); err == nil {
			totalPages = tp
		}
		if len(body) == 0 {
			break
		}
	}
	return out, nil
}

func mapWoo(o wooOrder) ImportedOrder {
	io := ImportedOrder{
		ExternalID: strconv.FormatInt(o.ID, 10), Number: "#" + o.Number,
		CustomerName: strings.TrimSpace(o.Billing.FirstName + " " + o.Billing.LastName), CustomerEmail: o.Billing.Email,
		Currency: strings.ToUpper(o.Currency), Total: o.Total.f(), Tax: o.TotalTax.f(), Shipping: o.ShippingTotal.f(),
		PlatformStatus: o.Status,
	}
	if o.Number == "" {
		io.Number = "#" + io.ExternalID
	}
	if t, err := time.Parse("2006-01-02T15:04:05", o.DateCreatedGMT); err == nil {
		io.PlacedAt = t.UTC()
	}
	for _, li := range o.LineItems {
		io.ItemsCount += li.Quantity
		io.Subtotal += li.Total.f()
		io.Items = append(io.Items, OrderItem{Name: li.Name, SKU: li.SKU, Quantity: li.Quantity, Total: li.Total.f()})
	}
	io.Subtotal = round2(io.Subtotal)
	switch o.Status {
	case "processing":
		io.Status = "processing"
	case "completed":
		io.Status = "completed"
	case "cancelled", "failed", "trash":
		io.Status = "cancelled"
	case "refunded":
		io.Status = "refunded"
	default: // pending, on-hold, checkout-draft
		io.Status = "pending"
	}
	if o.DatePaidGMT != "" {
		io.PaymentStatus = "paid"
	} else {
		io.PaymentStatus = "unpaid"
	}
	if io.Status == "refunded" {
		io.PaymentStatus = "refunded"
	}
	return io
}

func (cl *Client) maxPages() int {
	if cl.MaxPages <= 0 {
		return 20
	}
	return cl.MaxPages
}

func round2(v float64) float64 {
	if v < 0 {
		return -float64(int64(-v*100+0.5)) / 100
	}
	return float64(int64(v*100+0.5)) / 100
}
