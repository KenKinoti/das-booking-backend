package expenses

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Synergy Wholesale reseller API (SOAP 1.1, rpc/encoded, namespace
// urn:WholesaleSystem). Verified against the published WSDL
// (https://api.synergywholesale.com/?wsdl, service location
// https://api.synergywholesale.com:443/server.php) and API documentation
// v3.11/3.12:
//
//	balanceQuery     {resellerID, apiKey}                      → status, balance, errorMessage
//	listDomains      {resellerID, apiKey, page?, limit?, status?} → status, domainList[] {domainName, domain_status, domain_expiry, autoRenew, createdDate, …}
//	listHosting      {resellerID, apiKey, status?, page?, limit?} → status, hoidList[] {hoid, domain, plan, serviceStatus, billingPeriod, nextRenewalDue, createdDate, …}
//	getDomainPricing {resellerID, apiKey}                      → status, pricing[] {tld, renew, register_1_year, …}
//
// The API has NO invoice or transaction listing: invoices come from the
// billing emails (Gmail), the monthly statement CSV, or uploads. Access is
// restricted to IP addresses whitelisted in the Synergy Wholesale console
// (Account → API Information); a login from another IP fails.

// DefaultSynergyURL is the SOAP endpoint (SYNERGY_API_URL overrides, for tests).
const DefaultSynergyURL = "https://api.synergywholesale.com:443/server.php"

// SynergyClient calls the API.
type SynergyClient struct {
	URL        string
	ResellerID string
	APIKey     string
	HTTP       *http.Client
}

// NewSynergyClient builds a client with the configured endpoint.
func NewSynergyClient(resellerID, apiKey string) *SynergyClient {
	u := strings.TrimSpace(os.Getenv("SYNERGY_API_URL"))
	if u == "" {
		u = DefaultSynergyURL
	}
	return &SynergyClient{URL: u, ResellerID: strings.TrimSpace(resellerID), APIKey: strings.TrimSpace(apiKey), HTTP: &http.Client{Timeout: 45 * time.Second}}
}

// SynergyError is an API failure with a human explanation.
type SynergyError struct {
	Status  string // ERR_LOGIN_FAILED, SOAP_FAULT, HTTP_403 …
	Message string
	Hint    string
	// IPBlocked is true when the failure looks like the caller's IP is not whitelisted.
	IPBlocked bool
}

func (e *SynergyError) Error() string {
	s := e.Message
	if e.Hint != "" {
		s += " — " + e.Hint
	}
	return s
}

// EgressIP is set by the service (best effort) so error messages can name the IP to whitelist.
var EgressIP func() string

func ipHint() string {
	ip := ""
	if EgressIP != nil {
		ip = EgressIP()
	}
	if ip != "" {
		return "make sure this server's IP address " + ip + " is whitelisted in Synergy Wholesale (Account → API Information → IP whitelist) and the reseller ID and API key are correct"
	}
	return "make sure this server's public IP address is whitelisted in Synergy Wholesale (Account → API Information → IP whitelist) and the reseller ID and API key are correct"
}

// Call runs one command and returns the decoded "return" value.
func (c *SynergyClient) Call(ctx context.Context, cmd string, params ...[2]string) (map[string]any, error) {
	if c.ResellerID == "" || c.APIKey == "" {
		return nil, &SynergyError{Status: "NOT_CONFIGURED", Message: "Enter your Synergy Wholesale reseller ID and API key"}
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="urn:WholesaleSystem" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" ` +
		`xmlns:SOAP-ENC="http://schemas.xmlsoap.org/soap/encoding/" SOAP-ENV:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<SOAP-ENV:Body><ns1:` + cmd + `><request>`)
	write := func(k, v, typ string) {
		b.WriteString("<" + k + ` xsi:type="xsd:` + typ + `">`)
		_ = xml.EscapeText(&b, []byte(v))
		b.WriteString("</" + k + ">")
	}
	write("resellerID", c.ResellerID, "string")
	write("apiKey", c.APIKey, "string")
	for _, p := range params {
		typ := "string"
		if _, err := strconv.Atoi(p[1]); err == nil && (p[0] == "page" || p[0] == "limit") {
			typ = "int"
		}
		write(p[0], p[1], typ)
	}
	b.WriteString(`</request></ns1:` + cmd + `></SOAP-ENV:Body></SOAP-ENV:Envelope>`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, &b)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", `"urn:WholesaleSystem#`+cmd+`"`)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &SynergyError{Status: "NETWORK", Message: "Could not reach the Synergy Wholesale API: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	root, perr := ParseXMLTree(raw)
	if perr == nil {
		if f := root.Find("Fault"); f != nil {
			msg := strings.TrimSpace(f.Text("faultstring"))
			if msg == "" {
				msg = "SOAP fault"
			}
			e := &SynergyError{Status: "SOAP_FAULT", Message: "Synergy Wholesale: " + msg}
			low := strings.ToLower(msg)
			if strings.Contains(low, "ip") || strings.Contains(low, "access") || strings.Contains(low, "denied") || strings.Contains(low, "authori") || strings.Contains(low, "login") {
				e.IPBlocked = true
				e.Hint = ipHint()
			}
			return nil, e
		}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, &SynergyError{Status: fmt.Sprintf("HTTP_%d", resp.StatusCode), Message: "Synergy Wholesale refused the connection (HTTP " + strconv.Itoa(resp.StatusCode) + ")", Hint: ipHint(), IPBlocked: true}
	}
	if resp.StatusCode >= 300 || perr != nil {
		snip := strings.TrimSpace(string(raw))
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return nil, &SynergyError{Status: fmt.Sprintf("HTTP_%d", resp.StatusCode), Message: "Unexpected response from Synergy Wholesale (HTTP " + strconv.Itoa(resp.StatusCode) + "): " + snip}
	}
	body := root.Find(cmd + "Response")
	if body == nil {
		body = root.Find("Body")
	}
	if body == nil {
		return nil, &SynergyError{Status: "BAD_RESPONSE", Message: "Unexpected response from Synergy Wholesale"}
	}
	ret := body.Find("return")
	if ret == nil && len(body.Children) == 1 {
		ret = body.Children[0]
	}
	if ret == nil {
		ret = body
	}
	m, _ := ret.Value().(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	status := strings.ToUpper(str(m["status"]))
	if status != "" && status != "OK" && !strings.HasPrefix(status, "OK_") {
		msg := str(m["errorMessage"])
		if msg == "" {
			msg = status
		}
		e := &SynergyError{Status: status, Message: "Synergy Wholesale: " + msg}
		if status == "ERR_LOGIN_FAILED" || strings.Contains(strings.ToLower(msg), "unable to login") || strings.Contains(strings.ToLower(msg), "ip") {
			e.IPBlocked = true
			e.Hint = ipHint()
		}
		return m, e
	}
	return m, nil
}

// Balance returns the account balance (AUD).
func (c *SynergyClient) Balance(ctx context.Context) (float64, error) {
	m, err := c.Call(ctx, "balanceQuery")
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(str(m["balance"])), ",", ""), 64)
	if err != nil {
		return 0, &SynergyError{Status: "BAD_RESPONSE", Message: "Synergy Wholesale returned an unreadable balance"}
	}
	return v, nil
}

// SynergyDomain is one domain from listDomains.
type SynergyDomain struct {
	Name      string
	Status    string
	Expiry    *time.Time
	AutoRenew bool
}

// SynergyHosting is one hosting service from listHosting.
type SynergyHosting struct {
	ID, Domain, Plan, Status, BillingPeriod string
	NextRenewal                             *time.Time
	// Disk usage / limit in MB when the API returns them (not in the public
	// documentation; read defensively from the common field names).
	UsageMB, LimitMB *float64
}

// mbField reads the first numeric field among names (values in MB, or with
// a GB / MB suffix).
func mbField(h map[string]any, names ...string) *float64 {
	for _, n := range names {
		for k, v := range h {
			if !strings.EqualFold(k, n) {
				continue
			}
			t := strings.ToUpper(strings.TrimSpace(str(v)))
			mult := 1.0
			switch {
			case strings.HasSuffix(t, "GB"):
				mult, t = 1024, strings.TrimSpace(strings.TrimSuffix(t, "GB"))
			case strings.HasSuffix(t, "MB"):
				t = strings.TrimSpace(strings.TrimSuffix(t, "MB"))
			}
			if f, err := strconv.ParseFloat(t, 64); err == nil && f >= 0 {
				f *= mult
				return &f
			}
		}
	}
	return nil
}

// Domains lists every domain (pages of 500).
func (c *SynergyClient) Domains(ctx context.Context) ([]SynergyDomain, error) {
	out := []SynergyDomain{}
	for page := 1; page <= 40; page++ {
		m, err := c.Call(ctx, "listDomains", [2]string{"page", strconv.Itoa(page)}, [2]string{"limit", "500"})
		if err != nil {
			var se *SynergyError
			if page > 1 && errors.As(err, &se) && strings.Contains(strings.ToUpper(se.Status), "NO_DOMAINS") {
				break
			}
			return out, err
		}
		list := listOf(m["domainList"])
		for _, it := range list {
			d, _ := it.(map[string]any)
			if d == nil {
				continue
			}
			name := str(d["domainName"])
			if name == "" {
				continue
			}
			out = append(out, SynergyDomain{Name: strings.ToLower(name), Status: str(d["domain_status"]), Expiry: ParseDate(str(d["domain_expiry"])), AutoRenew: truthy(d["autoRenew"])})
		}
		if len(list) < 500 {
			break
		}
	}
	return out, nil
}

// Hosting lists hosting services.
func (c *SynergyClient) Hosting(ctx context.Context) ([]SynergyHosting, error) {
	out := []SynergyHosting{}
	for page := 1; page <= 20; page++ {
		m, err := c.Call(ctx, "listHosting", [2]string{"page", strconv.Itoa(page)}, [2]string{"limit", "500"})
		if err != nil {
			return out, err
		}
		list := listOf(m["hoidList"])
		for _, it := range list {
			h, _ := it.(map[string]any)
			if h == nil {
				continue
			}
			id := str(h["hoid"])
			if id == "" {
				id = str(h["domain"]) + "|" + str(h["plan"])
			}
			st := str(h["serviceStatus"])
			if st == "" {
				st = str(h["status"])
			}
			out = append(out, SynergyHosting{ID: id, Domain: strings.ToLower(str(h["domain"])), Plan: str(h["plan"]), Status: st,
				BillingPeriod: str(h["billingPeriod"]), NextRenewal: ParseDate(str(h["nextRenewalDue"])),
				UsageMB: mbField(h, "diskUsage", "disk_usage", "diskUsed", "disk_used", "usage"),
				LimitMB: mbField(h, "diskLimit", "disk_limit", "diskQuota", "disk_quota", "diskSpace", "limit")})
		}
		if len(list) < 500 {
			break
		}
	}
	return out, nil
}

// RenewPrices returns the renewal price per TLD (".com.au" → 1-year renew price).
func (c *SynergyClient) RenewPrices(ctx context.Context) (map[string]float64, error) {
	m, err := c.Call(ctx, "getDomainPricing")
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, it := range listOf(m["pricing"]) {
		p, _ := it.(map[string]any)
		if p == nil {
			continue
		}
		tld := strings.ToLower(strings.TrimPrefix(str(p["tld"]), "."))
		v, err := strconv.ParseFloat(str(p["renew"]), 64)
		if err != nil || tld == "" {
			continue
		}
		out[tld] = v
	}
	return out, nil
}

// TLDOf returns the longest priced suffix of a domain ("x.com.au" → "com.au").
func TLDOf(domain string, prices map[string]float64) string {
	parts := strings.Split(strings.ToLower(domain), ".")
	for i := 1; i < len(parts); i++ {
		t := strings.Join(parts[i:], ".")
		if _, ok := prices[t]; ok {
			return t
		}
	}
	return ""
}

func listOf(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		// A single-entry array sometimes decodes as an object.
		if it, ok := t["item"]; ok {
			return listOf(it)
		}
		return []any{t}
	}
	return nil
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case nil:
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func truthy(v any) bool {
	s := strings.ToLower(str(v))
	return s == "1" || s == "true" || s == "yes" || s == "on"
}

// ---------- generic XML tree (SOAP-encoded responses) ----------

// XMLNode is an element with its children.
type XMLNode struct {
	Name     string
	Attrs    map[string]string
	Children []*XMLNode
	Content  string
}

// ParseXMLTree parses a document into a tree (namespaces dropped).
func ParseXMLTree(data []byte) (*XMLNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	root := &XMLNode{Name: "#root"}
	stack := []*XMLNode{root}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &XMLNode{Name: t.Name.Local, Attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.Attrs[a.Name.Local] = a.Value
			}
			p := stack[len(stack)-1]
			p.Children = append(p.Children, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			stack[len(stack)-1].Content += string(t)
		}
	}
	if len(root.Children) == 0 {
		return nil, errors.New("empty XML")
	}
	return root, nil
}

// Find returns the first descendant with the local name (case-insensitive).
func (n *XMLNode) Find(name string) *XMLNode {
	for _, c := range n.Children {
		if strings.EqualFold(c.Name, name) {
			return c
		}
		if f := c.Find(name); f != nil {
			return f
		}
	}
	return nil
}

// Text returns the text of a descendant.
func (n *XMLNode) Text(name string) string {
	if f := n.Find(name); f != nil {
		return strings.TrimSpace(f.Content)
	}
	return ""
}

// Value converts the node to string / map / slice.
func (n *XMLNode) Value() any {
	if len(n.Children) == 0 {
		if n.Attrs["nil"] == "true" || n.Attrs["nil"] == "1" {
			return nil
		}
		return strings.TrimSpace(n.Content)
	}
	// Apache/PHP Map encoding: <item><key>k</key><value>v</value></item>…
	isMap := true
	for _, c := range n.Children {
		if !strings.EqualFold(c.Name, "item") || c.Find("key") == nil || len(c.Children) != 2 {
			isMap = false
			break
		}
	}
	if isMap {
		m := map[string]any{}
		for _, c := range n.Children {
			var k string
			var v any
			for _, cc := range c.Children {
				if strings.EqualFold(cc.Name, "key") {
					k = strings.TrimSpace(cc.Content)
				} else {
					v = cc.Value()
				}
			}
			m[k] = v
		}
		return m
	}
	// Array: arrayType attribute or repeated child names.
	_, isArr := n.Attrs["arrayType"]
	if !isArr && len(n.Children) > 1 {
		isArr = true
		for _, c := range n.Children[1:] {
			if c.Name != n.Children[0].Name {
				isArr = false
				break
			}
		}
	}
	if !isArr && len(n.Children) == 1 && strings.EqualFold(n.Children[0].Name, "item") {
		isArr = true
	}
	if isArr {
		out := make([]any, 0, len(n.Children))
		for _, c := range n.Children {
			out = append(out, c.Value())
		}
		return out
	}
	m := map[string]any{}
	for _, c := range n.Children {
		m[c.Name] = c.Value()
	}
	return m
}
