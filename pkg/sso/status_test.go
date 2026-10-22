package sso

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Tests for the "saved but never switched on" fix: auto-enable on save, the
// status checklist, credential cleaning, error mapping and multi-instance
// state.

type provView struct {
	Provider    string `json:"provider"`
	Enabled     bool   `json:"enabled"`
	Usable      bool   `json:"usable"`
	Switched    string `json:"switched"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
	AutoEnabled bool   `json:"auto_enabled"`
	KeptOff     bool   `json:"kept_off"`
	Status      Status `json:"status"`
}

func (e *env) save(provider string, body map[string]interface{}) (int, provView) {
	e.t.Helper()
	w := e.do("PUT", "/api/v1/sso/admin/providers/"+provider, body, nil)
	var out struct {
		Data provView `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Data
}

func (e *env) status(provider string, refresh bool, hdr map[string]string) provView {
	e.t.Helper()
	path := "/api/v1/sso/admin/providers/" + provider + "/status"
	if refresh {
		path += "?refresh=1"
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "api.example.test"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != 200 {
		e.t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Data provView `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.Data
}

func (e *env) publicIDs() []string {
	w := e.do("GET", "/api/v1/public/sso/providers", nil, nil)
	var out struct {
		Data struct {
			Providers []struct {
				ID       string `json:"id"`
				StartURL string `json:"start_url"`
			} `json:"providers"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	ids := []string{}
	for _, p := range out.Data.Providers {
		ids = append(ids, p.ID)
	}
	return ids
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func check(st Status, id string) Check {
	for _, c := range st.Checks {
		if c.ID == id {
			return c
		}
	}
	return Check{}
}

// noGoogleEnv: Google configured only through System settings.
func noGoogleEnv(t *testing.T, e *env) {
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "")
	e.db.Where("1 = 1").Delete(&ProviderConfig{})
	e.db.Where("1 = 1").Delete(&ProviderHealth{})
	t.Cleanup(func() {
		e.db.Where("1 = 1").Delete(&ProviderConfig{})
		e.db.Where("1 = 1").Delete(&ProviderHealth{})
	})
	e.user = e.addUser("root-"+uuid.NewString()[:8]+"@example.com", "super_admin", true)
}

const gID = "123456789012-abc123def456.apps.googleusercontent.com"

func TestAutoEnableOnSave(t *testing.T) {
	e := newEnv(t, Google)
	noGoogleEnv(t, e)
	if has(e.publicIDs(), Google) {
		t.Fatal("google listed without credentials")
	}
	st := e.status(Google, false, nil)
	if st.Status.ShowsOnLogin || !strings.Contains(st.Status.Reason, "add the client ID and client secret") || check(st.Status, "enabled").State != "info" {
		t.Fatalf("fresh status: %+v", st.Status)
	}
	// Only the ID: nothing to switch on yet.
	code, v := e.save(Google, map[string]interface{}{"client_id": gID})
	if code != 200 || v.Enabled || v.AutoEnabled || !strings.Contains(v.Status.Reason, "client secret") {
		t.Fatalf("id only: %d %+v", code, v)
	}
	// The secret arrives; the admin never touches the switch → on.
	code, v = e.save(Google, map[string]interface{}{"client_id": gID, "client_secret": "GOCSPX-first"})
	if code != 200 || !v.Enabled || !v.Usable || !v.AutoEnabled || v.Switched != "auto" || !v.Status.ShowsOnLogin || !strings.Contains(check(v.Status, "enabled").Detail, "automatically") {
		t.Fatalf("auto enable: %d %+v", code, v)
	}
	if !has(e.publicIDs(), Google) {
		t.Fatal("google not listed after saving credentials")
	}
	// Explicitly off → stays off when credentials change later.
	if code, v = e.save(Google, map[string]interface{}{"enabled": false}); code != 200 || v.Enabled || v.Switched != "off" {
		t.Fatalf("switch off: %d %+v", code, v)
	}
	code, v = e.save(Google, map[string]interface{}{"client_secret": "GOCSPX-second"})
	if code != 200 || v.Enabled || v.AutoEnabled || !v.KeptOff || has(e.publicIDs(), Google) {
		t.Fatalf("kept off: %d %+v", code, v)
	}
	if !strings.Contains(v.Status.Reason, "administrator switched it off") || check(v.Status, "enabled").State != "error" || v.Status.Ready {
		t.Fatalf("off status: %+v", v.Status)
	}
	// "Turn on".
	if code, v = e.save(Google, map[string]interface{}{"enabled": true}); code != 200 || !v.Usable || v.Switched != "on" || !has(e.publicIDs(), Google) {
		t.Fatalf("turn on: %d %+v", code, v)
	}
	// Turning on without credentials is refused.
	e.db.Where("provider = ?", Google).Delete(&ProviderConfig{})
	if code, _ := e.save(Google, map[string]interface{}{"enabled": true}); code != 400 {
		t.Fatalf("enable without credentials: %d", code)
	}
}

func TestLegacyRowAndEnvFirstSave(t *testing.T) {
	e := newEnv(t, Google)
	noGoogleEnv(t, e)
	// A row saved by the old page: credentials stored, switch left off, no history.
	enc, _ := encryptForTest("GOCSPX-legacy")
	e.db.Create(&ProviderConfig{Provider: Google, Enabled: false, ClientID: gID, ClientSecret: enc, UpdatedAt: e.now})
	v := e.status(Google, false, nil)
	if v.Status.ShowsOnLogin || !strings.Contains(v.Status.Reason, "never switched on") || check(v.Status, "enabled").State != "error" {
		t.Fatalf("legacy status: %+v", v.Status)
	}
	// Any save with the credentials present switches it on.
	if code, v := e.save(Google, map[string]interface{}{"client_id": gID}); code != 200 || !v.AutoEnabled || !v.Usable {
		t.Fatalf("legacy save: %d %+v", code, v)
	}
	// Env-only Microsoft (no row): saving the tenant keeps it on.
	if code, v := e.save(Microsoft, map[string]interface{}{"tenant": "organizations"}); code != 200 || !v.Enabled || v.AutoEnabled {
		t.Fatalf("env first save: %d %+v", code, v)
	}
}

func TestCredentialTrimming(t *testing.T) {
	cases := map[string]string{
		"  abc.apps.googleusercontent.com \r\n": "abc.apps.googleusercontent.com",
		"\"GOCSPX-abc\"\n":                      "GOCSPX-abc",
		"GOCSPX-ab\ncd":                         "GOCSPX-abcd", // wrapped line
		"\u200bGOCSPX-x\ufeff":                  "GOCSPX-x",
		"' quoted '":                            "quoted",
		"\u00a0tab\tsep\u00a0":                  "tabsep",
		"a~b.c_d-e":                             "a~b.c_d-e",
	}
	for in, want := range cases {
		if got := CleanCredential(in); got != want {
			t.Errorf("CleanCredential(%q) = %q, want %q", in, got, want)
		}
	}
	e := newEnv(t, Google)
	noGoogleEnv(t, e)
	code, v := e.save(Google, map[string]interface{}{"client_id": " \n" + gID + "\r\n", "client_secret": "  GOCSPX-pasted\n"})
	if code != 200 || v.ClientID != gID || !v.Usable {
		t.Fatalf("save: %d %+v", code, v)
	}
	var row ProviderConfig
	e.db.Where("provider = ?", Google).Take(&row)
	if row.ClientID != gID {
		t.Fatalf("stored id %q", row.ClientID)
	}
	if s := Load(e.db, Google); s.ClientSecret != "GOCSPX-pasted" {
		t.Fatalf("secret %q", s.ClientSecret)
	}
	// Rows and env values from before are cleaned when read.
	e.db.Model(&ProviderConfig{}).Where("provider = ?", Google).Update("client_id", gID+"\n")
	if s := Load(e.db, Google); s.ClientID != gID {
		t.Fatalf("loaded id %q", s.ClientID)
	}
	t.Setenv("MICROSOFT_CLIENT_ID", " mid\n")
	if s := Load(e.db, Microsoft); s.ClientID != "mid" {
		t.Fatalf("env id %q", s.ClientID)
	}
}

func TestStatusReasons(t *testing.T) {
	cases := []struct {
		s    Settings
		want string
	}{
		{Settings{Enabled: true, ClientID: "a", ClientSecret: "b"}, "Yes"},
		{Settings{Enabled: true, ClientID: "a"}, "switched on, but the client secret is missing"},
		{Settings{Enabled: true}, "client ID and client secret are missing"},
		{Settings{ClientID: "a", ClientSecret: "b", Switched: "off"}, "administrator switched it off"},
		{Settings{ClientID: "a", ClientSecret: "b"}, "never switched on"},
		{Settings{ClientID: "a", SecretUnreadable: true}, "can't be decrypted"},
		{Settings{Switched: "off", ClientID: "a"}, "switched off, and the client secret is missing"},
	}
	for _, tc := range cases {
		if got := showsReason(tc.s); !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q (want %q)", tc.s, got, tc.want)
		}
	}
	if st, _ := ClientIDProblem(Google, "GOCSPX-abc"); st != "error" {
		t.Error("secret as google id")
	}
	if st, _ := ClientIDProblem(Google, "1234-abc"); st != "warn" {
		t.Error("google id without suffix")
	}
	if st, _ := ClientIDProblem(Google, gID); st != "" {
		t.Error("good google id")
	}
	if st, _ := ClientIDProblem(Microsoft, "b3c1f0a2-5d4e-4c3b-9a8f-1e2d3c4b5a69"); st != "" {
		t.Error("good ms id")
	}
	if st, _ := SecretProblem(Microsoft, "b3c1f0a2-5d4e-4c3b-9a8f-1e2d3c4b5a69", "x"); st != "error" {
		t.Error("ms secret id instead of value")
	}
	if st, _ := SecretProblem(Google, gID, "x"); st != "error" {
		t.Error("google id as secret")
	}

	e := newEnv(t, Google)
	noGoogleEnv(t, e)
	e.save(Google, map[string]interface{}{"client_id": gID, "client_secret": "GOCSPX-ok"})
	v := e.status(Google, false, map[string]string{"Origin": appURL})
	for _, id := range []string{"enabled", "client_id", "client_secret", "redirect_uri", "app_url", "discovery"} {
		if c := check(v.Status, id); c.State != "ok" {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if !v.Status.Ready || v.Status.Errors != 0 || v.Status.Warnings != 0 || check(v.Status, "redirect_uri").Value != "https://api.example.test/api/v1/public/sso/google/callback" {
		t.Fatalf("all green: %+v", v.Status)
	}
	// A secret that can't be decrypted any more (key changed).
	e.db.Model(&ProviderConfig{}).Where("provider = ?", Google).Update("client_secret", "enc:v1:not-decryptable")
	v = e.status(Google, false, nil)
	if v.Usable || !strings.Contains(v.Status.Reason, "can't be decrypted") || check(v.Status, "client_secret").State != "error" {
		t.Fatalf("unreadable secret: %+v", v.Status)
	}
	if code, _ := e.save(Google, map[string]interface{}{"enabled": true}); code != 400 {
		t.Fatalf("enable with unreadable secret: %d", code)
	}
	// Microsoft: client ID not a GUID (warn), "Secret ID" pasted as the secret (error).
	t.Setenv("MICROSOFT_CLIENT_SECRET", "b3c1f0a2-5d4e-4c3b-9a8f-1e2d3c4b5a69")
	v = e.status(Microsoft, false, nil)
	if check(v.Status, "client_id").State != "warn" || check(v.Status, "client_secret").State != "error" || !strings.Contains(check(v.Status, "client_secret").Detail, "Value") {
		t.Fatalf("microsoft formats: %+v", v.Status.Checks)
	}
	// PUBLIC_API_URL missing → derived (warn); with /api/v1 → cleaned (warn);
	// plain http on a real host → error; PUBLIC_APP_URL missing with a
	// cross-origin app → error.
	e.h.PublicAPI = ""
	v = e.status(Google, false, nil)
	if c := check(v.Status, "redirect_uri"); c.State != "error" || !strings.Contains(c.Detail, "https") {
		t.Fatalf("http derived: %+v", c)
	}
	v = e.status(Google, false, map[string]string{"X-Forwarded-Proto": "https"})
	if c := check(v.Status, "redirect_uri"); c.State != "warn" || !strings.Contains(c.Detail, "PUBLIC_API_URL isn't set") || c.Value != "https://api.example.test/api/v1/public/sso/google/callback" {
		t.Fatalf("derived: %+v", c)
	}
	e.h.PublicAPI = "https://api.example.test/api/v1/"
	v = e.status(Google, false, nil)
	if c := check(v.Status, "redirect_uri"); c.State != "warn" || c.Value != "https://api.example.test/api/v1/public/sso/google/callback" {
		t.Fatalf("api suffix: %+v", c)
	}
	e.h.PublicApp = ""
	v = e.status(Google, false, map[string]string{"Origin": "https://erp.example.test"})
	if c := check(v.Status, "app_url"); c.State != "error" || !strings.Contains(c.Detail, "PUBLIC_APP_URL=https://erp.example.test") {
		t.Fatalf("app url: %+v", c)
	}
}

func TestDiscoveryCheck(t *testing.T) {
	var hits, slow int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&slow) == 1 {
			time.Sleep(400 * time.Millisecond)
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			atomic.AddInt32(&hits, 1)
			if atomic.LoadInt32(&slow) == 2 {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://issuer.test", "authorization_endpoint": "x", "token_endpoint": "y", "jwks_uri": "http://" + r.Host + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"keys": []interface{}{map[string]string{"kty": "RSA", "kid": "1"}}})
		}
	}))
	defer srv.Close()
	e := newEnv(t, Google)
	noGoogleEnv(t, e)
	t.Setenv("SSO_GOOGLE_ISSUER", srv.URL)
	e.h.CheckTimeout = 200 * time.Millisecond
	e.save(Google, map[string]interface{}{"client_id": gID, "client_secret": "GOCSPX-ok"})
	v := e.status(Google, false, nil)
	if c := check(v.Status, "discovery"); c.State != "ok" || !strings.Contains(c.Detail, "issuer https://issuer.test") || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("discovery: %+v hits=%d", c, hits)
	}
	// Cached…
	e.status(Google, false, nil)
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("not cached: %d", hits)
	}
	// …until "Check now".
	atomic.StoreInt32(&slow, 2)
	v = e.status(Google, true, nil)
	if c := check(v.Status, "discovery"); c.State != "error" || !strings.Contains(c.Detail, "HTTP 503") || atomic.LoadInt32(&hits) != 2 || v.Status.Ready {
		t.Fatalf("503: %+v", c)
	}
	// Slow provider → bounded by CheckTimeout.
	atomic.StoreInt32(&slow, 1)
	t0 := time.Now()
	v = e.status(Google, true, nil)
	if c := check(v.Status, "discovery"); c.State != "error" || !strings.Contains(c.Detail, "no answer within") || time.Since(t0) > 2*time.Second {
		t.Fatalf("timeout: %+v %s", c, time.Since(t0))
	}
	// Cache expiry.
	atomic.StoreInt32(&slow, 0)
	e.now = e.now.Add(checkTTL + time.Second)
	if c := check(e.status(Google, false, nil).Status, "discovery"); c.State != "ok" {
		t.Fatalf("after ttl: %+v", c)
	}
	// Not set up → not fetched.
	t.Setenv("MICROSOFT_CLIENT_ID", "")
	t.Setenv("MICROSOFT_CLIENT_SECRET", "")
	if c := check(e.status(Microsoft, false, nil).Status, "discovery"); c.State != "info" {
		t.Fatalf("not set up: %+v", c)
	}
}

// The OAuth state lives in the database: /start on one instance, the
// callback and the code exchange on another (Cloud Run scales out).
func TestStateAcrossInstances(t *testing.T) {
	e := newEnv(t, Google)
	u := e.addUser("multi-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	s := e.start(Google, "?redirect=/invoices")

	h2 := &Handler{DB: e.db, OIDC: NewOIDC(), PublicAPI: "https://api.example.test", PublicApp: appURL, Now: e.h.Now, Issue: e.h.Issue}
	r2 := gin.New()
	h2.RegisterPublic(r2.Group("/api/v1/public"))
	e.r = r2 // every later request goes to the second instance
	v := e.callback(Google, s, e.claims(s, "g-"+uuid.NewString(), u.Email, true), s.cookies)
	if v.Get("code") == "" {
		t.Fatalf("callback on instance 2: %v", v)
	}
	if code, out := e.exchange(v.Get("code")); code != 200 || out["redirect"] != "/invoices" {
		t.Fatalf("exchange on instance 2: %d %v", code, out)
	}
}

// /start on another host than PUBLIC_API_URL moves there first, so the
// browser cookie comes back with the callback.
func TestStartMovesToRedirectHost(t *testing.T) {
	e := newEnv(t, Google)
	req := httptest.NewRequest("GET", "/api/v1/public/sso/google/start?redirect=/invoices", nil)
	req.Host = "das-erp-backend.run.app"
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != 302 || loc.Host != "api.example.test" || loc.Path != "/api/v1/public/sso/google/start" || loc.Query().Get("hop") != "1" || loc.Query().Get("redirect") != "/invoices" {
		t.Fatalf("move: %d %s", w.Code, loc)
	}
	// Never twice.
	req = httptest.NewRequest("GET", loc.RequestURI(), nil)
	req.Host = "das-erp-backend.run.app"
	w = httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if !strings.HasPrefix(w.Header().Get("Location"), e.idp.srv.URL+"/authorize") {
		t.Fatalf("second hop: %s", w.Header().Get("Location"))
	}
	// The login page gets start URLs on that host; never cached.
	w = e.do("GET", "/api/v1/public/sso/providers", nil, nil)
	if !strings.Contains(w.Body.String(), `"start_url":"https://api.example.test/api/v1/public/sso/google/start"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("providers: %s %v", w.Body.String(), w.Header())
	}
}

func TestProviderErrorsShownAndRecorded(t *testing.T) {
	e := newEnv(t, Google)
	e.db.Where("1 = 1").Delete(&ProviderHealth{})
	t.Cleanup(func() { e.db.Where("1 = 1").Delete(&ProviderHealth{}) })
	u := e.addUser("perr-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	cb := func(s started, q string) url.Values {
		w := e.do("GET", "/api/v1/public/sso/google/callback?state="+url.QueryEscape(s.state)+"&"+q, nil, s.cookies)
		loc := w.Header().Get("Location")
		v, _ := url.ParseQuery(strings.SplitN(loc, "#", 2)[1])
		return v
	}
	// Errors the provider sends back to the callback (through the browser:
	// shown, recorded as a warning only — anyone can craft one).
	for q, want := range map[string]string{
		"error=redirect_uri_mismatch":                         "redirect_uri_mismatch",
		"error=access_denied":                                 "cancelled",
		"error=invalid_client":                                "invalid_client",
		"error=org_internal":                                  "access_blocked",
		"error=invalid_request&error_description=AADSTS50011": "redirect_uri_mismatch",
		"error=server_error":                                  "provider_error",
	} {
		v := cb(e.start(Google, ""), q)
		if v.Get("error") != want || v.Get("provider") != Google {
			t.Errorf("%s: %v", q, v)
		}
		if want == "redirect_uri_mismatch" && v.Get("redirect_uri") != "https://api.example.test/api/v1/public/sso/google/callback" {
			t.Errorf("%s: redirect_uri %v", q, v)
		}
		if want == "provider_error" && v.Get("detail") != "server_error" {
			t.Errorf("%s: detail %v", q, v)
		}
	}
	e.user = e.addUser("root-"+uuid.NewString()[:8]+"@example.com", "super_admin", true)
	if c := check(e.status(Google, false, nil).Status, "last_attempt"); c.State != "warn" {
		t.Fatalf("browser-reported error: %+v", c)
	}
	e.user = nil
	// Wrong client secret → the token endpoint says invalid_client (certain → red).
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "wrong")
	v := e.login(Google, nil, "g-"+uuid.NewString(), u.Email, true)
	if v.Get("error") != "invalid_client" {
		t.Fatalf("wrong secret: %v", v)
	}
	e.user = e.addUser("root-"+uuid.NewString()[:8]+"@example.com", "super_admin", true)
	st := e.status(Google, false, nil)
	if c := check(st.Status, "last_attempt"); c.State != "error" || !strings.Contains(c.Detail, "invalid_client") || st.Status.Ready {
		t.Fatalf("last attempt: %+v", c)
	}
	// Fixed: the next sign-in goes through and the status turns green.
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "secret-gid")
	e.now = e.now.Add(time.Second)
	if v := e.login(Google, nil, "g-"+uuid.NewString(), u.Email, true); v.Get("code") == "" {
		t.Fatalf("after fix: %v", v)
	}
	if c := check(e.status(Google, false, nil).Status, "last_attempt"); c.State != "ok" {
		t.Fatalf("after fix: %+v", c)
	}
	// A provider error without a state still explains itself — but anyone
	// can open that URL, so it doesn't touch the status page.
	w := e.do("GET", "/api/v1/public/sso/google/callback?error=invalid_client", nil, nil)
	if !strings.Contains(w.Header().Get("Location"), "error=invalid_client") {
		t.Fatalf("no state: %s", w.Header().Get("Location"))
	}
	if c := check(e.status(Google, false, nil).Status, "last_attempt"); c.State != "ok" {
		t.Fatalf("stateless error recorded: %+v", c)
	}
	// Token endpoint error mapping.
	for te, want := range map[*TokenError]string{
		{Code: "invalid_client"}:                                      "invalid_client",
		{Code: "invalid_request", Description: "AADSTS7000215: bad"}:  "invalid_client",
		{Code: "redirect_uri_mismatch"}:                               "redirect_uri_mismatch",
		{Code: "invalid_grant", Description: "redirect_uri mismatch"}: "redirect_uri_mismatch",
		{Code: "invalid_grant", Description: "Bad Request"}:           "expired",
		{Code: "temporarily_unavailable", Description: "Bad Request"}: "provider_error",
	} {
		if got, _ := classifyVerifyError(te); got != want {
			t.Errorf("%+v: %s want %s", te, got, want)
		}
	}
	if got, _ := classifyVerifyError(ErrIssuer); got != "invalid_token" {
		t.Errorf("issuer: %s", got)
	}
}

func encryptForTest(v string) (string, error) { return mail.EncryptSecret(v) }
