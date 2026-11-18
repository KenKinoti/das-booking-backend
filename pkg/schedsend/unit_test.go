package schedsend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

func TestRenderMergeFields(t *testing.T) {
	out, unk := Render("Hi {first_name}, {{ balance }} due {due_date}. {nope}", map[string]string{"first_name": "Wanjiru", "balance": "KSh 1,000.00", "due_date": "5 Dec 2026"})
	if out != "Hi Wanjiru, KSh 1,000.00 due 5 Dec 2026. {nope}" || len(unk) != 1 || unk[0] != "nope" {
		t.Fatalf("got %q %v", out, unk)
	}
	if u := UnknownFields("Hi {first_name} {balance}", true); len(u) != 1 || u[0] != "balance" {
		t.Fatalf("team messages have no customer fields: %v", u)
	}
	if u := UnknownFields("Hi {first_name} {pay_link}", false); len(u) != 0 {
		t.Fatalf("customer fields: %v", u)
	}
	if f := FieldsUsed("{first_name} {first_name} {amount_due}"); strings.Join(f, ",") != "first_name,amount_due" {
		t.Fatalf("fields used: %v", f)
	}
}

func TestBalanceText(t *testing.T) {
	got := balanceText([]OpenInvoice{{Currency: "USD", BalanceDue: 50}, {Currency: "KES", BalanceDue: 1000}, {Currency: "KES", BalanceDue: 500}}, "KES")
	if !strings.HasPrefix(got, "KSh") || !strings.Contains(got, "1,500.00") || !strings.Contains(got, "+") || !strings.Contains(got, "50.00") {
		t.Fatalf("home currency first, summed per currency: %q", got)
	}
}

func TestSignedTokens(t *testing.T) {
	s := &Service{Secret: []byte("secret-1")}
	tok := s.token("unsub", "org-1", "cust-9")
	if a, b, ok := s.readToken("unsub", tok); !ok || a != "org-1" || b != "cust-9" {
		t.Fatal("round trip")
	}
	if _, _, ok := s.readToken("open", tok); ok {
		t.Fatal("a token is only valid for its purpose")
	}
	if _, _, ok := (&Service{Secret: []byte("other")}).readToken("unsub", tok); ok {
		t.Fatal("another secret must not verify")
	}
	parts := strings.Split(tok, ".")
	parts[1] = "Y3VzdC0x" // cust-1
	if _, _, ok := s.readToken("unsub", strings.Join(parts, ".")); ok {
		t.Fatal("a changed customer id must not verify")
	}
}

func TestTemporaryErrors(t *testing.T) {
	cases := []struct {
		err   error
		retry bool
	}{
		{&wa.Error{Code: 130429, Message: "rate"}, true},
		{&wa.Error{Code: 131026}, false},
		{&SMSError{Msg: "x", Retryable: true}, true},
		{&APIError{Status: 502, Code: "EMAIL_FAILED", Message: "Email could not be sent"}, true},
		{&APIError{Status: 422, Code: "CHECKS_FAILED", Message: "blocked"}, false},
		{&APIError{Status: 503, Code: "SMTP_NOT_CONFIGURED", Message: "not set up"}, false},
		{&mail.Error{Code: "TIMEOUT", Msg: "timeout"}, true},
		{&mail.Error{Code: "AUTH_FAILED", Msg: "auth"}, false},
		{wa.ErrNotConfigured, false},
		{errors.New("dial tcp: connection refused"), true},
	}
	for _, c := range cases {
		if got, why := temporary("", c.err); got != c.retry || why == "" {
			t.Fatalf("%v: retry=%v (%q), want %v", c.err, got, why, c.retry)
		}
	}
	if _, why := temporary("", &wa.Error{Code: 131047}); !strings.Contains(why, "24 hours") {
		t.Fatalf("wa.Friendly is used: %q", why)
	}
}

func TestE164(t *testing.T) {
	for raw, want := range map[string]string{"0712 345 678": "+254712345678", "+61 412 345 678": "+61412345678", "254712345678": "+254712345678"} {
		if got, err := E164(raw, "KE"); err != nil || got != want {
			t.Fatalf("%s → %s %v", raw, got, err)
		}
	}
	if _, err := E164("12", "KE"); err == nil {
		t.Fatal("too short")
	}
}

// ---------------------------------------------------------------- SMS providers against fakes

func TestTwilioProvider(t *testing.T) {
	var got url.Values
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2010-04-01/Accounts/AC123/Messages.json" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		u, p, _ := r.BasicAuth()
		auth = u + ":" + p
		b, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(b))
		switch got.Get("To") {
		case "+254700000000":
			w.WriteHeader(400)
			io.WriteString(w, `{"code":21610,"message":"Attempt to send to unsubscribed recipient","status":400}`)
		case "+254700000500":
			w.WriteHeader(503)
			io.WriteString(w, `{"code":20503,"message":"Service unavailable","status":503}`)
		default:
			w.WriteHeader(201)
			io.WriteString(w, `{"sid":"SM42","status":"queued"}`)
		}
	}))
	defer srv.Close()
	t.Setenv("SMS_TWILIO_API_BASE", srv.URL)
	p := &SMSProvider{Kind: ProviderTwilio, TwSID: "AC123", TwToken: "tok", TwFrom: "+15005550006", StatusURL: "https://api.example/cb"}
	res, err := p.Send(context.Background(), "+254712345678", "Hello")
	if err != nil || res.ID != "SM42" || res.Provider != ProviderTwilio {
		t.Fatalf("%+v %v", res, err)
	}
	if auth != "AC123:tok" || got.Get("Body") != "Hello" || got.Get("From") != "+15005550006" || got.Get("StatusCallback") != "https://api.example/cb" {
		t.Fatalf("request: %s %v", auth, got)
	}
	p.TwService = "MG1"
	_, _ = p.Send(context.Background(), "+254712345678", "Hi")
	if got.Get("MessagingServiceSid") != "MG1" || got.Get("From") != "" {
		t.Fatalf("messaging service: %v", got)
	}
	_, err = p.Send(context.Background(), "+254700000000", "Hi")
	var se *SMSError
	if !errors.As(err, &se) || se.Retryable || !strings.Contains(se.Msg, "STOP") {
		t.Fatalf("21610: %v", err)
	}
	_, err = p.Send(context.Background(), "+254700000500", "Hi")
	if !errors.As(err, &se) || !se.Retryable {
		t.Fatalf("503 is temporary: %v", err)
	}
}

func TestAfricasTalkingProvider(t *testing.T) {
	var got url.Values
	var key, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version1/messaging" {
			http.NotFound(w, r)
			return
		}
		key, accept = r.Header.Get("apiKey"), r.Header.Get("Accept")
		if key == "bad" {
			w.WriteHeader(401)
			io.WriteString(w, "The supplied authentication is invalid")
			return
		}
		b, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(b))
		w.WriteHeader(201)
		switch got.Get("to") {
		case "+254712000403":
			io.WriteString(w, `{"SMSMessageData":{"Message":"Sent to 0/1","Recipients":[{"statusCode":403,"number":"+254712000403","status":"InvalidPhoneNumber","cost":"0","messageId":"None"}]}}`)
		case "+254712000500":
			io.WriteString(w, `{"SMSMessageData":{"Message":"Sent to 0/1","Recipients":[{"statusCode":500,"number":"+254712000500","status":"InternalServerError","cost":"0","messageId":"None"}]}}`)
		case "+254712000402":
			io.WriteString(w, `{"SMSMessageData":{"Message":"InvalidSenderId","Recipients":[]}}`)
		default:
			io.WriteString(w, `{"SMSMessageData":{"Message":"Sent to 1/1 Total Cost: KES 0.8000","Recipients":[{"statusCode":101,"number":"`+got.Get("to")+`","status":"Success","cost":"KES 0.8000","messageId":"ATXid_1"}]}}`)
		}
	}))
	defer srv.Close()
	t.Setenv("SMS_AFRICASTALKING_API_BASE", srv.URL)
	p := &SMSProvider{Kind: ProviderAT, ATUsername: "dasyin", ATKey: "k1", ATSender: "DASYIN"}
	res, err := p.Send(context.Background(), "+254712345678", "Jambo")
	if err != nil || res.ID != "ATXid_1" || res.Cost != "KES 0.8000" {
		t.Fatalf("%+v %v", res, err)
	}
	if key != "k1" || accept != "application/json" || got.Get("username") != "dasyin" || got.Get("message") != "Jambo" || got.Get("from") != "DASYIN" || got.Get("bulkSMSMode") != "1" {
		t.Fatalf("request: key=%s accept=%s %v", key, accept, got)
	}
	var se *SMSError
	if _, err := p.Send(context.Background(), "+254712000403", "x"); !errors.As(err, &se) || se.Retryable || se.Code != 403 {
		t.Fatalf("403: %v", err)
	}
	if _, err := p.Send(context.Background(), "+254712000500", "x"); !errors.As(err, &se) || !se.Retryable {
		t.Fatalf("500 is temporary: %v", err)
	}
	if _, err := p.Send(context.Background(), "+254712000402", "x"); !errors.As(err, &se) || !strings.Contains(se.Msg, "sender ID") {
		t.Fatalf("invalid sender: %v", err)
	}
	p.ATKey = "bad"
	if _, err := p.Send(context.Background(), "+254712345678", "x"); !errors.As(err, &se) || !strings.Contains(se.Msg, "API key") {
		t.Fatalf("401: %v", err)
	}
}

func TestTwilioSignature(t *testing.T) {
	form := url.Values{"MessageSid": {"SM1"}, "MessageStatus": {"delivered"}}
	a := TwilioSignature("tok", "https://api.example/cb", form)
	if a == "" || a != TwilioSignature("tok", "https://api.example/cb", url.Values{"MessageStatus": {"delivered"}, "MessageSid": {"SM1"}}) {
		t.Fatal("order of fields must not matter")
	}
	if a == TwilioSignature("tok", "https://api.example/cb", url.Values{"MessageSid": {"SM1"}, "MessageStatus": {"failed"}}) || a == TwilioSignature("other", "https://api.example/cb", form) {
		t.Fatal("signature must change with the data or the token")
	}
}
