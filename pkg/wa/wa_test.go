package wa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeNumber(t *testing.T) {
	cases := []struct{ in, cc, want string }{
		{"0712 345 678", "KE", "254712345678"},
		{"+254 712-345-678", "KE", "254712345678"},
		{"254712345678", "KE", "254712345678"},
		{"712345678", "KE", "254712345678"},
		{"0412 345 678", "AU", "61412345678"},
		{"+61 412 345 678", "KE", "61412345678"},
		{"0044 20 7946 0958", "KE", "442079460958"},
	}
	for _, c := range cases {
		got, err := NormalizeNumber(c.in, c.cc)
		if err != nil || got != c.want {
			t.Errorf("NormalizeNumber(%q,%q) = %q, %v; want %q", c.in, c.cc, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "12", "abc"} {
		if _, err := NormalizeNumber(bad, "KE"); err == nil {
			t.Errorf("NormalizeNumber(%q) should fail", bad)
		}
	}
}

func TestCleanParam(t *testing.T) {
	got := CleanParam("Bills: 2 due\nJira:\t3 in progress      done")
	if strings.ContainsAny(got, "\n\t") || strings.Contains(got, "    ") {
		t.Fatalf("not clean: %q", got)
	}
	if got != "Bills: 2 due · Jira: 3 in progress   done" {
		t.Fatalf("got %q", got)
	}
	long := CleanParam(strings.Repeat("x", 2000))
	if n := len([]rune(long)); n != MaxParam {
		t.Fatalf("long param has %d runes", n)
	}
	if CleanParam("  ") != "-" {
		t.Fatal("empty param should become -")
	}
}

func fakeGraph(t *testing.T) (*httptest.Server, *[]map[string]any) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"Invalid OAuth access token","code":190}}`)
			return
		}
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/v21.0/123/messages"):
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			got = append(got, b)
			if b["type"] == "text" && b["to"] == "254700000001" {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"Re-engagement message","code":131047,"error_data":{"details":"More than 24 hours"}}}`)
				return
			}
			io.WriteString(w, `{"messaging_product":"whatsapp","contacts":[{"input":"x","wa_id":"254712345678"}],"messages":[{"id":"wamid.ABC"}]}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/v21.0/456/message_templates"):
			io.WriteString(w, `{"data":[{"name":"dasyin_morning_brief","status":"APPROVED","language":"en","category":"UTILITY","components":[{"type":"BODY","text":"Hi {{1}}, {{2}}: {{3}}"},{"type":"BUTTONS","buttons":[{"type":"URL","url":"https://x/brief/{{1}}"}]}]}],"paging":{}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/v21.0/123"):
			io.WriteString(w, `{"display_phone_number":"+254 700 000000","verified_name":"DASYIN"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WHATSAPP_API_BASE", srv.URL)
	return srv, &got
}

func TestSendTemplateAndText(t *testing.T) {
	_, got := fakeGraph(t)
	a := &Account{PhoneNumberID: "123", BusinessAccountID: "456", Token: "tok", APIVersion: "v21.0"}
	ctx := context.Background()
	res, err := a.SendTemplate(ctx, "254712345678", Template{Name: "dasyin_morning_brief", Language: "en",
		BodyParams: []string{"Ken", "Wed 18 Nov", "2 bills due\n3 Jira issues"}, URLButton: "2026-11-18"})
	if err != nil || res.MessageID != "wamid.ABC" || res.WaID != "254712345678" {
		t.Fatalf("SendTemplate: %+v %v", res, err)
	}
	tpl := (*got)[0]["template"].(map[string]any)
	comps := tpl["components"].([]any)
	body := comps[0].(map[string]any)["parameters"].([]any)
	if p := body[2].(map[string]any)["text"].(string); strings.Contains(p, "\n") {
		t.Fatalf("param not cleaned: %q", p)
	}
	btn := comps[1].(map[string]any)
	if btn["sub_type"] != "url" || btn["index"] != "0" {
		t.Fatalf("button component: %v", btn)
	}
	if _, err := a.SendText(ctx, "254712345678", "hello", false); err != nil {
		t.Fatal(err)
	}
	_, err = a.SendText(ctx, "254700000001", "hello", false)
	var we *Error
	if !errors.As(err, &we) || !we.OutsideWindow() || we.Retryable() {
		t.Fatalf("outside window error: %v", err)
	}
	if !strings.Contains(Friendly(err), "24 hours") {
		t.Fatalf("friendly: %s", Friendly(err))
	}
	list, err := a.Templates(ctx)
	if err != nil || len(list) != 1 || list[0].BodyParams != 3 || !list[0].URLButton || list[0].Status != "APPROVED" {
		t.Fatalf("templates: %+v %v", list, err)
	}
	if who, err := a.Check(ctx); err != nil || who != "DASYIN +254 700 000000" {
		t.Fatalf("check: %q %v", who, err)
	}
	bad := &Account{PhoneNumberID: "123", Token: "nope", APIVersion: "v21.0"}
	_, err = bad.SendText(ctx, "254712345678", "x", false)
	if !errors.As(err, &we) || we.Code != 190 || !strings.Contains(Friendly(err), "token") {
		t.Fatalf("bad token: %v / %s", err, Friendly(err))
	}
}
