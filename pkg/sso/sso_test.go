package sso

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ---------------------------------------------------------------- fake OIDC provider

type fakeIDP struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	key      *rsa.PrivateKey
	kid      string
	old      *rsa.PrivateKey // previous key still published (rotation)
	codes    map[string]fakeGrant
	issuer   string // overrides the discovery issuer ("" = server URL)
	jwksHits int
}

type fakeGrant struct {
	claims    map[string]interface{}
	challenge string
	clientID  string
	redirect  string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	f := &fakeIDP{t: t, codes: map[string]fakeGrant{}, kid: "k1"}
	f.key = genKey(t)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func genKey(t *testing.T) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwkOf(kid string, k *rsa.PublicKey) map[string]interface{} {
	return map[string]interface{}{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256", "n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
}

func (f *fakeIDP) iss() string {
	if f.issuer != "" {
		return f.issuer
	}
	return f.srv.URL
}

func (f *fakeIDP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.iss(), "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks"})
	case "/jwks":
		f.jwksHits++
		keys := []interface{}{jwkOf(f.kid, &f.key.PublicKey)}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"keys": keys})
	case "/token":
		_ = r.ParseForm()
		g, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		if !ok {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		if r.Form.Get("client_id") != g.clientID || r.Form.Get("client_secret") != "secret-"+g.clientID {
			w.WriteHeader(401) // as Google: {"error": "invalid_client", "error_description": "Unauthorized"}
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "Unauthorized"})
			return
		}
		if r.Form.Get("redirect_uri") != g.redirect {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "redirect_uri_mismatch", "error_description": "Bad Request"})
			return
		}
		if PKCEChallenge(r.Form.Get("code_verifier")) != g.challenge || r.Form.Get("grant_type") != "authorization_code" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "pkce mismatch"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "token_type": "Bearer", "id_token": f.sign(g.claims, f.key, f.kid)})
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeIDP) sign(claims map[string]interface{}, key *rsa.PrivateKey, kid string) string {
	hb, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	pb, _ := json.Marshal(claims)
	in := b64(hb) + "." + b64(pb)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

// ---------------------------------------------------------------- environment

func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS platform_org_status (organization_id varchar(255) primary key, status varchar(20))`)
	return db
}

type env struct {
	t    *testing.T
	db   *gorm.DB
	idp  *fakeIDP
	h    *Handler
	r    *gin.Engine
	org  string
	user *models.User // signed-in user for /api/v1 calls
	now  time.Time
}

const appURL = "https://app.example.test"

func newEnv(t *testing.T, provider string) *env {
	db := testDB(t)
	idp := newFakeIDP(t)
	t.Setenv("SSO_TEST_ISSUERS", "1")
	t.Setenv("SSO_GOOGLE_ISSUER", idp.srv.URL)
	t.Setenv("SSO_MICROSOFT_ISSUER", idp.srv.URL)
	for _, k := range []string{"GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "MICROSOFT_CLIENT_ID", "MICROSOFT_CLIENT_SECRET", "MICROSOFT_TENANT"} {
		t.Setenv(k, "")
	}
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "gid")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "secret-gid")
	t.Setenv("MICROSOFT_CLIENT_ID", "mid")
	t.Setenv("MICROSOFT_CLIENT_SECRET", "secret-mid")
	db.Where("1 = 1").Delete(&ProviderConfig{})
	e := &env{t: t, db: db, idp: idp, org: "sso-test-" + uuid.NewString(), now: time.Now()}
	e.h = &Handler{DB: db, OIDC: NewOIDC(), PublicAPI: "https://api.example.test", PublicApp: appURL, Now: func() time.Time { return e.now }}
	e.h.OIDC.Now = func() time.Time { return e.now }
	e.h.Issue = func(c *gin.Context, u *models.User) (interface{}, error) {
		return gin.H{"token": "tok-" + u.ID, "user": gin.H{"id": u.ID, "email": u.Email}}, nil
	}
	gin.SetMode(gin.TestMode)
	e.r = gin.New()
	auth := e.r.Group("/api/v1", func(c *gin.Context) {
		if e.user != nil {
			c.Set("user_id", e.user.ID)
			c.Set("org_id", e.user.OrganizationID)
			c.Set("user_role", e.user.Role)
		}
		c.Next()
	})
	e.h.RegisterPublic(e.r.Group("/api/v1/public"))
	e.h.RegisterAuth(auth)
	return e
}

func (e *env) addUser(email, role string, active bool) *models.User {
	u := &models.User{ID: uuid.NewString(), Email: email, PasswordHash: "x", FirstName: "T", LastName: "U", Role: role, OrganizationID: e.org, IsActive: true}
	if err := e.db.Omit("Organization", "Shifts", "UploadedDocs", "UserPermissions", "RefreshTokens").Create(u).Error; err != nil {
		e.t.Fatal(err)
	}
	if !active {
		e.db.Model(u).Update("is_active", false)
		u.IsActive = false
	}
	e.t.Cleanup(func() { e.db.Unscoped().Delete(u) })
	return u
}

func (e *env) do(method, path string, body interface{}, cookies []*http.Cookie) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "api.example.test" // the address PUBLIC_API_URL names
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// started is the provider redirect produced by /start.
type started struct {
	state, nonce, challenge, redirectURI, clientID string
	cookies                                        []*http.Cookie
}

func (e *env) start(provider, query string) started {
	w := e.do("GET", "/api/v1/public/sso/"+provider+"/start"+query, nil, nil)
	if w.Code != 302 {
		e.t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if !strings.HasPrefix(loc.String(), e.idp.srv.URL+"/authorize") {
		e.t.Fatalf("start redirected to %s", loc)
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("scope") != "openid email profile" || q.Get("response_type") != "code" {
		e.t.Fatalf("authorize params: %v", q)
	}
	return started{state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"),
		clientID: q.Get("client_id"), cookies: (&http.Response{Header: w.Header()}).Cookies()}
}

func (e *env) claims(s started, sub, email string, verified bool) map[string]interface{} {
	return map[string]interface{}{"iss": e.idp.iss(), "aud": s.clientID, "sub": sub, "email": email, "email_verified": verified,
		"nonce": s.nonce, "iat": e.now.Unix(), "exp": e.now.Add(time.Hour).Unix(), "given_name": "Ada", "family_name": "Lovelace"}
}

// callback completes the round trip with claims; returns the app redirect fragment.
func (e *env) callback(provider string, s started, claims map[string]interface{}, cookies []*http.Cookie) url.Values {
	code := "c-" + uuid.NewString()
	e.idp.mu.Lock()
	e.idp.codes[code] = fakeGrant{claims: claims, challenge: s.challenge, clientID: s.clientID, redirect: s.redirectURI}
	e.idp.mu.Unlock()
	w := e.do("GET", "/api/v1/public/sso/"+provider+"/callback?code="+code+"&state="+url.QueryEscape(s.state), nil, cookies)
	if w.Code != 302 {
		e.t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, appURL+CompletePath+"#") {
		e.t.Fatalf("callback redirected to %s", loc)
	}
	if strings.Contains(strings.SplitN(loc, "#", 2)[0], "?") {
		e.t.Fatalf("values must be in the fragment only: %s", loc)
	}
	v, _ := url.ParseQuery(strings.SplitN(loc, "#", 2)[1])
	return v
}

func (e *env) login(provider string, mutate func(s started, c map[string]interface{}), sub, email string, verified bool) url.Values {
	s := e.start(provider, "?redirect=/invoices")
	c := e.claims(s, sub, email, verified)
	if mutate != nil {
		mutate(s, c)
	}
	return e.callback(provider, s, c, s.cookies)
}

func (e *env) exchange(code string) (int, map[string]interface{}) {
	w := e.do("POST", "/api/v1/public/sso/exchange", map[string]string{"code": code}, nil)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ---------------------------------------------------------------- tests

func TestGoogleSignInExistingUser(t *testing.T) {
	e := newEnv(t, Google)
	u := e.addUser("Ada.Lovelace+"+uuid.NewString()[:8]+"@Example.com", "admin", true)
	sub := "g-" + uuid.NewString()
	v := e.login(Google, nil, sub, strings.ToUpper(u.Email), true) // case-insensitive
	if v.Get("code") == "" || v.Get("error") != "" {
		t.Fatalf("fragment: %v", v)
	}
	code, out := e.exchange(v.Get("code"))
	if code != 200 || out["redirect"] != "/invoices" {
		t.Fatalf("exchange: %d %v", code, out)
	}
	if d, _ := out["data"].(map[string]interface{}); d["token"] != "tok-"+u.ID {
		t.Fatalf("session for wrong user: %v", out)
	}
	// single use
	if code, _ := e.exchange(v.Get("code")); code != 401 {
		t.Fatalf("second exchange: %d", code)
	}
	// linked by sub
	var id Identity
	if err := e.db.Where("provider = ? AND subject = ?", Google, sub).Take(&id).Error; err != nil || id.UserID != u.ID || id.LastUsed == nil {
		t.Fatalf("identity: %+v %v", id, err)
	}
	var n int64
	e.db.Model(&models.AuditLog{}).Where("user_id = ? AND action = ?", u.ID, "auth.sso_login").Count(&n)
	if n != 1 {
		t.Fatalf("audit entries: %d", n)
	}
	// Later sign-in by sub even though the provider email changed.
	v = e.login(Google, nil, sub, "someone-else@other.test", true)
	if code, out := e.exchange(v.Get("code")); code != 200 || out["data"].(map[string]interface{})["token"] != "tok-"+u.ID {
		t.Fatalf("sub sign-in: %d %v", code, out)
	}
}

func TestRejectedTokens(t *testing.T) {
	e := newEnv(t, Google)
	u := e.addUser("rej-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	cases := map[string]func(s started, c map[string]interface{}){
		"wrong aud":   func(s started, c map[string]interface{}) { c["aud"] = "someone-else" },
		"wrong iss":   func(s started, c map[string]interface{}) { c["iss"] = "https://evil.example" },
		"wrong nonce": func(s started, c map[string]interface{}) { c["nonce"] = "nope" },
		"expired":     func(s started, c map[string]interface{}) { c["exp"] = e.now.Add(-10 * time.Minute).Unix() },
		"multi aud": func(s started, c map[string]interface{}) {
			c["aud"] = []string{s.clientID, "other"}
			c["azp"] = "other"
		},
		"future token": func(s started, c map[string]interface{}) { c["nbf"] = e.now.Add(time.Hour).Unix() },
	}
	for name, m := range cases {
		v := e.login(Google, m, "g-"+uuid.NewString(), u.Email, true)
		if v.Get("error") != "invalid_token" || v.Get("code") != "" {
			t.Errorf("%s: %v", name, v)
		}
	}
	// unverified email
	v := e.login(Google, nil, "g-"+uuid.NewString(), u.Email, false)
	if v.Get("error") != "email_unverified" {
		t.Errorf("unverified: %v", v)
	}
	// multi-aud with azp = client is fine
	v = e.login(Google, func(s started, c map[string]interface{}) {
		c["aud"] = []string{s.clientID, "other"}
		c["azp"] = s.clientID
	}, "g-"+uuid.NewString(), u.Email, true)
	if v.Get("code") == "" {
		t.Errorf("multi aud with azp: %v", v)
	}
}

func TestStateAndCookie(t *testing.T) {
	e := newEnv(t, Google)
	u := e.addUser("st-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	s := e.start(Google, "")
	// no browser cookie → rejected (login CSRF)
	if v := e.callback(Google, s, e.claims(s, "g-1", u.Email, true), nil); v.Get("error") != "browser_mismatch" {
		t.Fatalf("no cookie: %v", v)
	}
	// state is single use
	if v := e.callback(Google, s, e.claims(s, "g-1", u.Email, true), s.cookies); v.Get("error") != "expired" {
		t.Fatalf("reused state: %v", v)
	}
	// expired flow
	s = e.start(Google, "")
	e.now = e.now.Add(11 * time.Minute)
	if v := e.callback(Google, s, e.claims(s, "g-1", u.Email, true), s.cookies); v.Get("error") != "expired" {
		t.Fatalf("expired flow: %v", v)
	}
	// open redirects are dropped
	if safeRedirect("//evil.test/x") != "" || safeRedirect("https://evil.test") != "" || safeRedirect("/\\evil") != "" || safeRedirect("/invoices?a=1") != "/invoices?a=1" {
		t.Fatal("safeRedirect")
	}
}

func TestUnknownUserAndDomainJoin(t *testing.T) {
	e := newEnv(t, Google)
	dom := "join-" + uuid.NewString()[:8] + ".test"
	email := "new.person@" + dom
	v := e.login(Google, nil, "g-"+uuid.NewString(), email, true)
	if v.Get("error") != "no_account" || v.Get("email") != email {
		t.Fatalf("unknown user: %v", v)
	}
	var n int64
	e.db.Model(&models.User{}).Where("email = ?", email).Count(&n)
	if n != 0 {
		t.Fatal("user created without a domain rule")
	}
	// Admin adds the domain (own domain only).
	admin := e.addUser("boss@"+dom, "admin", true)
	e.user = admin
	if w := e.do("POST", "/api/v1/sso/domains", map[string]string{"domain": "gmail.com", "role": "staff"}, nil); w.Code != 400 {
		t.Fatalf("public domain: %d", w.Code)
	}
	if w := e.do("POST", "/api/v1/sso/domains", map[string]string{"domain": "other-company.test", "role": "staff"}, nil); w.Code != 403 {
		t.Fatalf("someone else's domain: %d", w.Code)
	}
	if w := e.do("POST", "/api/v1/sso/domains", map[string]string{"domain": "@" + strings.ToUpper(dom), "role": "admin"}, nil); w.Code != 400 {
		t.Fatalf("admin role: %d", w.Code)
	}
	w := e.do("POST", "/api/v1/sso/domains", map[string]string{"domain": "@" + strings.ToUpper(dom), "role": "staff"}, nil)
	if w.Code != 200 {
		t.Fatalf("add domain: %d %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() { e.db.Where("domain = ?", dom).Delete(&DomainRule{}) })
	e.user = nil
	v = e.login(Google, nil, "g-"+uuid.NewString(), email, true)
	if v.Get("code") == "" || v.Get("joined") != "1" {
		t.Fatalf("join: %v", v)
	}
	var nu models.User
	if err := e.db.Where("email = ?", email).Take(&nu).Error; err != nil || nu.Role != "staff" || nu.OrganizationID != e.org || nu.FirstName != "Ada" || !nu.IsActive {
		t.Fatalf("joined user: %+v %v", nu, err)
	}
	t.Cleanup(func() { e.db.Unscoped().Delete(&nu) })
	if code, _ := e.exchange(v.Get("code")); code != 200 {
		t.Fatalf("exchange: %d", code)
	}
	// Unverified address at the domain never joins.
	if v := e.login(Google, nil, "g-"+uuid.NewString(), "x@"+dom, false); v.Get("error") != "email_unverified" {
		t.Fatalf("unverified join: %v", v)
	}
	// Plan limit
	e.h.UserLimit = func(string) string { return "full" }
	if v := e.login(Google, nil, "g-"+uuid.NewString(), "y@"+dom, true); v.Get("error") != "user_limit" {
		t.Fatalf("limit: %v", v)
	}
}

func TestDisabledUserAndSuspendedOrg(t *testing.T) {
	e := newEnv(t, Google)
	u := e.addUser("dis-"+uuid.NewString()[:8]+"@example.com", "staff", false)
	if v := e.login(Google, nil, "g-"+uuid.NewString(), u.Email, true); v.Get("error") != "account_disabled" {
		t.Fatalf("disabled: %v", v)
	}
	u2 := e.addUser("sus-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	e.db.Exec(`INSERT INTO platform_org_status (organization_id, status) VALUES (?, 'suspended')`, e.org)
	t.Cleanup(func() { e.db.Exec(`DELETE FROM platform_org_status WHERE organization_id = ?`, e.org) })
	if v := e.login(Google, nil, "g-"+uuid.NewString(), u2.Email, true); v.Get("error") != "org_suspended" {
		t.Fatalf("suspended: %v", v)
	}
}

func TestCodeExpiry(t *testing.T) {
	e := newEnv(t, Google)
	u := e.addUser("exp-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	v := e.login(Google, nil, "g-"+uuid.NewString(), u.Email, true)
	e.now = e.now.Add(3 * time.Minute)
	if code, _ := e.exchange(v.Get("code")); code != 401 {
		t.Fatalf("expired code: %d", code)
	}
	if code, _ := e.exchange("made-up"); code != 401 {
		t.Fatalf("unknown code: %d", code)
	}
}

func TestLinkAndUnlink(t *testing.T) {
	e := newEnv(t, Microsoft)
	u := e.addUser("link-"+uuid.NewString()[:8]+"@example.com", "admin", true)
	e.user = u
	w := e.do("POST", "/api/v1/sso/microsoft/link", map[string]string{"redirect": "/profile"}, nil)
	if w.Code != 200 {
		t.Fatalf("link: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	lu, _ := url.Parse(out.Data.URL)
	if lu.Host != "api.example.test" || lu.Query().Get("ticket") == "" {
		t.Fatalf("link url: %s", out.Data.URL)
	}
	s := e.start(Microsoft, "?ticket="+url.QueryEscape(lu.Query().Get("ticket")))
	// the ticket is single use
	if w := e.do("GET", "/api/v1/public/sso/microsoft/start?ticket="+url.QueryEscape(lu.Query().Get("ticket")), nil, nil); !strings.Contains(w.Header().Get("Location"), "error=expired") {
		t.Fatalf("ticket reuse: %s", w.Header().Get("Location"))
	}
	c := e.claims(s, "ms-"+uuid.NewString(), "", false)
	delete(c, "email")
	c["tid"] = "11111111-2222-3333-4444-555555555555"
	c["preferred_username"] = "Ada@Contoso.test"
	v := e.callback(Microsoft, s, c, s.cookies)
	if v.Get("linked") != Microsoft || v.Get("email") != "ada@contoso.test" || v.Get("redirect") != "/profile" {
		t.Fatalf("linked: %v", v)
	}
	w = e.do("GET", "/api/v1/sso/identities", nil, nil)
	var list struct {
		Data struct {
			Identities []Identity `json:"identities"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Data.Identities) != 1 || list.Data.Identities[0].Provider != Microsoft {
		t.Fatalf("identities: %s", w.Body.String())
	}
	// Signing in with the linked Microsoft account → this user (email differs).
	e.user = nil
	v2 := e.login(Microsoft, func(s started, cl map[string]interface{}) {
		cl["sub"] = c["sub"]
		cl["tid"] = c["tid"]
		cl["email_verified"] = false
	}, "", "", false)
	if code, out := e.exchange(v2.Get("code")); code != 200 || out["data"].(map[string]interface{})["token"] != "tok-"+u.ID {
		t.Fatalf("linked sign-in: %d %v (%v)", code, out, v2)
	}
	// Another user can't link the same account.
	u2 := e.addUser("link2-"+uuid.NewString()[:8]+"@example.com", "staff", true)
	e.user = u2
	w = e.do("POST", "/api/v1/sso/microsoft/link", nil, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	lu, _ = url.Parse(out.Data.URL)
	s = e.start(Microsoft, "?ticket="+url.QueryEscape(lu.Query().Get("ticket")))
	c2 := e.claims(s, c["sub"].(string), "", false)
	if v := e.callback(Microsoft, s, c2, s.cookies); v.Get("error") != "identity_in_use" {
		t.Fatalf("in use: %v", v)
	}
	// Unlink
	e.user = u
	if w := e.do("DELETE", "/api/v1/sso/identities/"+list.Data.Identities[0].ID, nil, nil); w.Code != 200 {
		t.Fatalf("unlink: %d", w.Code)
	}
	var n int64
	e.db.Model(&Identity{}).Where("user_id = ?", u.ID).Count(&n)
	if n != 0 {
		t.Fatal("identity not removed")
	}
	// u2 can't unlink someone else's identity (already gone → 404 anyway)
	e.user = u2
	if w := e.do("DELETE", "/api/v1/sso/identities/"+list.Data.Identities[0].ID, nil, nil); w.Code != 404 {
		t.Fatalf("unlink other: %d", w.Code)
	}
}

func TestMicrosoftEmailRules(t *testing.T) {
	work := "11111111-2222-3333-4444-555555555555"
	cases := []struct {
		name string
		c    Claims
		want string
	}{
		{"consumer email", Claims{Email: "Me@Outlook.com", TenantID: microsoftConsumerTenant}, "me@outlook.com"},
		{"work email without edov falls back to UPN", Claims{Email: "spoof@victim.test", PreferredUsername: "ada@contoso.test", TenantID: work}, "ada@contoso.test"},
		{"work email with edov", Claims{Email: "ada@contoso.test", EmailDomainOwner: true, PreferredUsername: "ada@contoso.onmicrosoft.com", TenantID: work}, "ada@contoso.test"},
		{"guest UPN rejected", Claims{Email: "spoof@victim.test", PreferredUsername: "ada_gmail.com#EXT#@contoso.onmicrosoft.com", TenantID: work}, ""},
		{"no email at all", Claims{PreferredUsername: "ada", TenantID: work}, ""},
		{"consumer without email", Claims{PreferredUsername: "me@outlook.com", TenantID: microsoftConsumerTenant}, ""},
	}
	for _, tc := range cases {
		if got := VerifiedEmail(Microsoft, &tc.c); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	if VerifiedEmail(Google, &Claims{Email: "a@b.test"}) != "" || VerifiedEmail(Google, &Claims{Email: "A@b.test", EmailVerified: true}) != "a@b.test" {
		t.Error("google rules")
	}
	// Issuer template for the multi-tenant endpoints.
	rule := issuerRule(Microsoft, "https://login.microsoftonline.com/{tenantid}/v2.0")
	if !rule("https://login.microsoftonline.com/"+work+"/v2.0", &Claims{TenantID: work}) {
		t.Error("tenant issuer should match")
	}
	if rule("https://login.microsoftonline.com/"+work+"/v2.0", &Claims{TenantID: microsoftConsumerTenant}) || rule("https://login.microsoftonline.com/x/v2.0", &Claims{}) {
		t.Error("issuer mismatch accepted")
	}
	g := issuerRule(Google, "https://accounts.google.com")
	if !g("accounts.google.com", &Claims{}) || g("https://evil", &Claims{}) {
		t.Error("google issuer")
	}
}

func TestKeyRotationAndAlg(t *testing.T) {
	e := newEnv(t, Google)
	ctx := context.Background()
	disc, err := e.h.OIDC.Discover(ctx, e.idp.srv.URL+"/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	opt := VerifyOptions{JWKSURI: disc.JWKSURI, ClientID: "gid", Nonce: "n1", IssuerOK: issuerRule(Google, disc.Issuer)}
	cl := map[string]interface{}{"iss": e.idp.srv.URL, "aud": "gid", "sub": "s", "nonce": "n1", "exp": e.now.Add(time.Hour).Unix()}
	if _, err := e.h.OIDC.Verify(ctx, e.idp.sign(cl, e.idp.key, e.idp.kid), opt); err != nil {
		t.Fatal(err)
	}
	// Rotate: new key, new kid. The cached set doesn't have it → refetch.
	e.idp.mu.Lock()
	e.idp.key, e.idp.kid = genKey(t), "k2"
	e.idp.mu.Unlock()
	e.now = e.now.Add(time.Minute)
	if _, err := e.h.OIDC.Verify(ctx, e.idp.sign(cl, e.idp.key, "k2"), opt); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	// A token signed by an unknown key fails.
	if _, err := e.h.OIDC.Verify(ctx, e.idp.sign(cl, genKey(t), "k2"), opt); err != ErrSignature {
		t.Fatalf("forged: %v", err)
	}
	// alg none / HS256 refused.
	hb, _ := json.Marshal(map[string]string{"alg": "none"})
	pb, _ := json.Marshal(cl)
	if _, err := e.h.OIDC.Verify(ctx, b64(hb)+"."+b64(pb)+".", opt); err != ErrAlg {
		t.Fatalf("alg none: %v", err)
	}
}

func TestProvidersConfig(t *testing.T) {
	e := newEnv(t, Google)
	// Public list: both configured via env → enabled.
	w := e.do("GET", "/api/v1/public/sso/providers", nil, nil)
	if !strings.Contains(w.Body.String(), `"google"`) || !strings.Contains(w.Body.String(), `"microsoft"`) {
		t.Fatalf("providers: %s", w.Body.String())
	}
	// GOOGLE_CLIENT_ID fallback (the Gmail expenses client).
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("GOOGLE_CLIENT_ID", "gmail-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "gmail-secret")
	if s := Load(e.db, Google); s.ClientID != "gmail-id" || !s.Usable() {
		t.Fatalf("fallback: %+v", s)
	}
	// Super admin disables Microsoft and saves a Google secret (encrypted).
	e.user = e.addUser("root-"+uuid.NewString()[:8]+"@example.com", "super_admin", true)
	if w := e.do("PUT", "/api/v1/sso/admin/providers/microsoft", map[string]interface{}{"enabled": false}, nil); w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "/api/v1/sso/admin/providers/google", map[string]interface{}{"enabled": true, "client_id": "db-id", "client_secret": "db-secret"}, nil); w.Code != 200 {
		t.Fatalf("save google: %d %s", w.Code, w.Body.String())
	}
	var row ProviderConfig
	e.db.Where("provider = ?", Google).Take(&row)
	if row.ClientSecret == "db-secret" || !strings.HasPrefix(row.ClientSecret, "enc:") {
		t.Fatalf("secret not encrypted: %q", row.ClientSecret)
	}
	if s := Load(e.db, Google); s.ClientID != "db-id" || s.ClientSecret != "db-secret" {
		t.Fatalf("db settings: %+v", s)
	}
	w = e.do("GET", "/api/v1/public/sso/providers", nil, nil)
	if strings.Contains(w.Body.String(), `"microsoft"`) {
		t.Fatalf("disabled provider listed: %s", w.Body.String())
	}
	w = e.do("GET", "/api/v1/sso/admin/providers", nil, nil)
	if !strings.Contains(w.Body.String(), "https://api.example.test/api/v1/public/sso/google/callback") || strings.Contains(w.Body.String(), "db-secret") {
		t.Fatalf("admin view: %s", w.Body.String())
	}
	// start on a disabled provider → friendly error page
	w = e.do("GET", "/api/v1/public/sso/microsoft/start", nil, nil)
	if !strings.Contains(w.Header().Get("Location"), "error=provider_disabled") {
		t.Fatalf("disabled start: %s", w.Header().Get("Location"))
	}
	// non super admin can't change providers
	e.user.Role = "admin"
	if w := e.do("PUT", "/api/v1/sso/admin/providers/google", map[string]interface{}{"enabled": false}, nil); w.Code != 403 {
		t.Fatalf("admin changed providers: %d", w.Code)
	}
	// test issuers ignored without SSO_TEST_ISSUERS=1
	t.Setenv("SSO_TEST_ISSUERS", "")
	if s := Load(e.db, Google); s.TestIssuer || !strings.HasPrefix(s.DiscoveryURL, "https://accounts.google.com/") {
		t.Fatalf("test issuer leaked: %+v", s)
	}
	if s := Load(e.db, Microsoft); s.DiscoveryURL != "https://login.microsoftonline.com/common/v2.0/.well-known/openid-configuration" {
		t.Fatalf("ms discovery: %s", s.DiscoveryURL)
	}
	e.db.Where("1 = 1").Delete(&ProviderConfig{})
}
