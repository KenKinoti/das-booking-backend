package expenses

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// fakeSynergy is a SOAP server answering like api.synergywholesale.com
// (rpc/encoded responses as PHP's SoapServer writes them).
type fakeSynergy struct {
	reseller, key string
	blocked       bool // simulate a caller IP that is not whitelisted
	fault         bool // answer with a SOAP fault instead
	balance       string
	calls         []string
}

const soapHead = `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="urn:WholesaleSystem" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:SOAP-ENC="http://schemas.xmlsoap.org/soap/encoding/" SOAP-ENV:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><SOAP-ENV:Body>`
const soapTail = `</SOAP-ENV:Body></SOAP-ENV:Envelope>`

func (f *fakeSynergy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	action := strings.Trim(r.Header.Get("SOAPAction"), `"`)
	cmd := strings.TrimPrefix(action, "urn:WholesaleSystem#")
	f.calls = append(f.calls, cmd)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	if !regexp.MustCompile(`<ns1:` + cmd + `><request>`).Match(body) {
		w.WriteHeader(500)
		fmt.Fprint(w, soapHead+`<SOAP-ENV:Fault><faultcode>SOAP-ENV:Client</faultcode><faultstring>Bad request body</faultstring></SOAP-ENV:Fault>`+soapTail)
		return
	}
	if f.fault {
		w.WriteHeader(500)
		fmt.Fprint(w, soapHead+`<SOAP-ENV:Fault><faultcode>SOAP-ENV:Server</faultcode><faultstring>Access denied: IP address 203.0.113.9 is not authorised</faultstring></SOAP-ENV:Fault>`+soapTail)
		return
	}
	resp := func(inner string) {
		fmt.Fprintf(w, soapHead+`<ns1:%sResponse><return xsi:type="ns1:%sResponse">%s</return></ns1:%sResponse>`+soapTail, cmd, cmd, inner, cmd)
	}
	if f.blocked || !strings.Contains(string(body), ">"+f.reseller+"<") || !strings.Contains(string(body), ">"+f.key+"<") {
		resp(`<status xsi:type="xsd:string">ERR_LOGIN_FAILED</status><errorMessage xsi:type="xsd:string">Unable to login to wholesale system</errorMessage>`)
		return
	}
	switch cmd {
	case "balanceQuery":
		resp(`<balance xsi:type="xsd:string">` + f.balance + `</balance><status xsi:type="xsd:string">OK</status>`)
	case "listDomains":
		resp(`<status xsi:type="xsd:string">OK</status><domainList SOAP-ENC:arrayType="ns1:bulkDomainInfoResponseSingleEntry[2]" xsi:type="ns1:bulkDomainInfoResponseArray">` +
			`<item xsi:type="ns1:bulkDomainInfoResponseSingleEntry"><domainName xsi:type="xsd:string">example.com.au</domainName><domain_status xsi:type="xsd:string">ok</domain_status><domain_expiry xsi:type="xsd:string">2026-10-20 10:00:00</domain_expiry><autoRenew xsi:type="xsd:string">on</autoRenew></item>` +
			`<item xsi:type="ns1:bulkDomainInfoResponseSingleEntry"><domainName xsi:type="xsd:string">client-site.com</domainName><domain_status xsi:type="xsd:string">ok</domain_status><domain_expiry xsi:type="xsd:string">2027-03-02 00:00:00</domain_expiry><autoRenew xsi:type="xsd:string">off</autoRenew></item>` +
			`</domainList>`)
	case "listHosting":
		resp(`<status xsi:type="xsd:string">OK</status><hoidList SOAP-ENC:arrayType="ns1:bulkHostingInfoResponseSingleEntry[1]" xsi:type="ns1:bulkHostingInfoResponseArray">` +
			`<item><hoid xsi:type="xsd:string">88123</hoid><domain>example.com.au</domain><plan>Economy cPanel</plan><serviceStatus>Active</serviceStatus><billingPeriod>monthly</billingPeriod><nextRenewalDue>2026-10-15</nextRenewalDue></item></hoidList>`)
	case "getDomainPricing":
		resp(`<status xsi:type="xsd:string">OK</status><pricing SOAP-ENC:arrayType="ns1:singlePricingResponse[2]">` +
			`<item><tld>com.au</tld><renew>11.95</renew><register_1_year>11.95</register_1_year></item><item><tld>com</tld><renew>15.50</renew></item></pricing>`)
	default:
		resp(`<status>ERR_UNKNOWN</status><errorMessage>unknown command</errorMessage>`)
	}
}

func TestSynergyClient(t *testing.T) {
	f := &fakeSynergy{reseller: "12345", key: "sekret-key", balance: "802.25"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	cl := &SynergyClient{URL: srv.URL, ResellerID: "12345", APIKey: "sekret-key", HTTP: srv.Client()}
	ctx := context.Background()
	bal, err := cl.Balance(ctx)
	if err != nil || bal != 802.25 {
		t.Fatalf("balance %v %v", bal, err)
	}
	ds, err := cl.Domains(ctx)
	if err != nil || len(ds) != 2 || ds[0].Name != "example.com.au" || ds[0].Expiry.Format(dayFmt) != "2026-10-20" || !ds[0].AutoRenew || ds[1].AutoRenew {
		t.Fatalf("domains %+v %v", ds, err)
	}
	hs, err := cl.Hosting(ctx)
	if err != nil || len(hs) != 1 || hs[0].ID != "88123" || hs[0].NextRenewal.Format(dayFmt) != "2026-10-15" || hs[0].Plan != "Economy cPanel" {
		t.Fatalf("hosting %+v %v", hs, err)
	}
	p, err := cl.RenewPrices(ctx)
	if err != nil || p["com.au"] != 11.95 || TLDOf("www.example.com.au", p) != "com.au" || TLDOf("client-site.com", p) != "com" {
		t.Fatalf("prices %v %v", p, err)
	}
}

func TestSynergyIPNotWhitelisted(t *testing.T) {
	EgressIP = func() string { return "203.0.113.9" }
	defer func() { EgressIP = nil }()
	f := &fakeSynergy{reseller: "12345", key: "k", blocked: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	cl := &SynergyClient{URL: srv.URL, ResellerID: "12345", APIKey: "k"}
	_, err := cl.Balance(context.Background())
	var se *SynergyError
	if !errors.As(err, &se) || se.Status != "ERR_LOGIN_FAILED" || !se.IPBlocked {
		t.Fatalf("err %#v", err)
	}
	if !strings.Contains(se.Error(), "203.0.113.9") || !strings.Contains(se.Error(), "whitelisted") {
		t.Fatalf("message should name the IP to whitelist: %s", se.Error())
	}
	// SOAP fault variant.
	f.blocked, f.fault = false, true
	_, err = cl.Balance(context.Background())
	if !errors.As(err, &se) || se.Status != "SOAP_FAULT" || !se.IPBlocked || !strings.Contains(se.Error(), "203.0.113.9") {
		t.Fatalf("fault %#v", err)
	}
	// Plain HTTP 403 from a firewall.
	srv403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); fmt.Fprint(w, "Forbidden") }))
	defer srv403.Close()
	cl.URL = srv403.URL
	_, err = cl.Balance(context.Background())
	if !errors.As(err, &se) || !se.IPBlocked || se.Status != "HTTP_403" {
		t.Fatalf("403 %#v", err)
	}
}

func TestXMLMapEncoding(t *testing.T) {
	x := `<Envelope><Body><r><return><item><key>status</key><value>OK</value></item><item><key>balance</key><value>12.50</value></item></return></r></Body></Envelope>`
	root, err := ParseXMLTree([]byte(x))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := root.Find("return").Value().(map[string]any)
	if m["status"] != "OK" || m["balance"] != "12.50" {
		t.Fatalf("%v", m)
	}
}
