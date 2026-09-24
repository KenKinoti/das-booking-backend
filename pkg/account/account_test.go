package account

import "testing"

func TestValidateNewPassword(t *testing.T) {
	if ValidateNewPassword("old", "short") == "" || ValidateNewPassword("same-password", "same-password") == "" {
		t.Fatal("weak or unchanged password accepted")
	}
	if msg := ValidateNewPassword("old", "a-long-enough-one"); msg != "" {
		t.Fatal(msg)
	}
}

func TestValidateOrg(t *testing.T) {
	in := orgInput{Name: " Acme ", ABN: "12 345 678 901", Website: "acme.com"}
	in.Settings.Timezone, in.Settings.Currency, in.Settings.DateFormat, in.Settings.TimeFormat = "Australia/Sydney", "aud", "DD/MM/YYYY", "24h"
	if msg := ValidateOrg(&in); msg != "" {
		t.Fatal(msg)
	}
	if in.Name != "Acme" || in.ABN != "12345678901" || in.Website != "https://acme.com" || in.Settings.Currency != "AUD" {
		t.Fatalf("%+v", in)
	}
	bad := in
	bad.Settings.Timezone = "Mars/Olympus"
	if ValidateOrg(&bad) == "" {
		t.Fatal("bad tz accepted")
	}
	bad = in
	bad.Email = "not-an-email"
	if ValidateOrg(&bad) == "" {
		t.Fatal("bad email accepted")
	}
	bad = in
	bad.Settings.Currency = "XXX"
	if ValidateOrg(&bad) == "" {
		t.Fatal("bad currency accepted")
	}
}
