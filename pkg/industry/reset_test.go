package industry

import (
	"testing"
)

func TestResetSetup(t *testing.T) {
	db := testDB(t)
	svc := NewService(db)
	a := seedOrg(t, db, "nightclub")
	b := seedOrg(t, db, "nightclub")
	for _, f := range []fixture{a, b} {
		if err := svc.OnOrgCreated(f.org, "nightclub", "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SetSetup(f.org, SetupDone, "", f.admin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.SetExtras(a.org, []string{"crm"}, a.admin); err != nil {
		t.Fatal(err)
	}
	if err := ResetSetup(db, a.org); err != nil {
		t.Fatal(err)
	}
	svc.Invalidate(a.org)
	st, err := svc.Settings(a.org)
	if err != nil {
		t.Fatal(err)
	}
	if st.SetupStatus != SetupPending || st.SetupStep != "" || st.TypeKey != "nightclub" || !st.Confirmed || len(st.ExtraModules) != 1 {
		t.Fatalf("after reset: %+v", st)
	}
	if other, _ := svc.Settings(b.org); other.SetupStatus != SetupDone {
		t.Fatalf("other organisation: %+v", other)
	}
	// The owner keeps the Owner role.
	if r, err := svc.RoleFor(a.org, a.admin, "admin"); err != nil || r == nil || r.Key != "owner" {
		t.Fatalf("owner role after reset: %+v %v", r, err)
	}

	// An organisation without a settings row gets one, pending, for its type.
	c := seedOrg(t, db, "salon")
	if err := ResetSetup(db, c.org); err != nil {
		t.Fatal(err)
	}
	var row OrgSettings
	if err := db.Where("organization_id = ?", c.org).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.SetupStatus != SetupPending || row.TypeKey != "salon" || row.Confirmed || row.UpdatedBy != SetupResetBy {
		t.Fatalf("new settings row: %+v", row)
	}
	if err := ResetSetup(db, " "); err == nil {
		t.Fatal("an empty organisation id must be refused")
	}
}
