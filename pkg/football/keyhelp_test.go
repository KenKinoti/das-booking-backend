package football

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeKey(t *testing.T) {
	k := "0123456789abcdef0123456789abcdef"
	cases := map[string]string{
		k:                                    k,
		"  " + k + "  ":                      k,
		"\u200b" + k + "\ufeff":              k, // zero-width characters from email / chat apps
		"\u00a0" + k + "\u00a0":              k, // non-breaking spaces
		`"` + k + `"`:                        k,
		"apiKey=" + k:                        k,
		"x-api-key: " + k:                    k,
		"https://api.the-odds-api.com/v4/sports/?apiKey=" + k + "&regions=uk": k,
		"<" + k + ">.":                       k,
	}
	for in, want := range cases {
		if got := NormalizeKey(in); got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
	if h := KeyHint(k); h != "…cdef (32 characters)" {
		t.Errorf("KeyHint = %q", h)
	}
	if KeyShapeHint(ProviderOddsAPI, k) != "" {
		t.Error("a 32-hex key should not warn")
	}
	if KeyShapeHint(ProviderOddsAPI, "pk_live_12345678901234567890") == "" {
		t.Error("a non-hex key should warn")
	}
}

// The Odds API connection test: a pasted key with invisible characters is
// cleaned before it is saved, and a refused key reports the provider's own
// error code, the key's last four characters and a shape hint.
func TestOddsAPIKeyDiagnostics(t *testing.T) {
	db := testDB(t)
	s := newSvc(t, db, time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	good := "0123456789abcdef0123456789abcdef"
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("apiKey")
		w.Header().Set("Content-Type", "application/json")
		if seen != good {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"API key is not valid. Check the key in your account.","error_code":"INVALID_KEY"}`))
			return
		}
		w.Header().Set("x-requests-remaining", "500")
		w.Write([]byte(`[{"key":"soccer_epl","group":"Soccer","title":"EPL","active":true}]`))
	}))
	defer srv.Close()
	s.OABase = srv.URL
	ctx := context.Background()
	if err := s.SaveSettings(SettingsPatch{OddsAPIKey: ptr("\u200b" + good + "\u00a0")}, "test"); err != nil {
		t.Fatal(err)
	}
	tr := s.TestConnection(ctx, ProviderOddsAPI)
	if !tr.OK || seen != good || tr.KeyShape != "" {
		t.Fatalf("clean key should connect: %+v (seen %q)", tr, seen)
	}
	bad := "pk_9f8e7d6c5b4a39281706"
	if err := s.SaveSettings(SettingsPatch{OddsAPIKey: ptr(bad)}, "test"); err != nil {
		t.Fatal(err)
	}
	tr = s.TestConnection(ctx, ProviderOddsAPI)
	if tr.OK || tr.Code != "bad_key" || tr.ProviderCode != "INVALID_KEY" || tr.HTTPStatus != 401 {
		t.Fatalf("refused key: %+v", tr)
	}
	if !strings.Contains(tr.Message, "INVALID_KEY") || tr.KeyHint != "…1706 (23 characters)" {
		t.Fatalf("diagnostics: %+v", tr)
	}
	if tr.KeyShape == "" || strings.Contains(tr.Message+tr.KeyHint+tr.ProviderMessage, bad) {
		t.Fatalf("shape hint missing or key leaked: %+v", tr)
	}
}
