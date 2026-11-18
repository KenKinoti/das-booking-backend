package payments

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// A recalled email's pay link (pkg/invoicing recall.go): the pay page shows
// the withdrawn notice without amounts and checkout is refused; the other
// recipient's link still pays. With every link switched off the old link is
// withdrawn too, while a payment started before that is still confirmed.
func TestPayRefusedThroughRecalledLink(t *testing.T) {
	e := setup(t, false, BearerBusiness)
	now := time.Now()
	tok := func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }
	tokA, tokB := tok(), tok()
	logID := uuid.NewString()
	rowA := invoicing.EmailRecipient{ID: uuid.NewString(), OrganizationID: e.org, InvoiceID: e.invID, EmailLogID: logID, Kind: "invoice", Email: "a@x.test",
		Role: "to", Tracked: true, Token: &tokA, Status: "sent", SentAt: &now, CreatedAt: now}
	rowB := rowA
	rowB.ID, rowB.Email, rowB.Role, rowB.Token = uuid.NewString(), "b@x.test", "cc", &tokB
	if err := e.db.Create(&[]invoicing.EmailRecipient{rowA, rowB}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.db.Where("organization_id = ?", e.org).Delete(&invoicing.EmailRecipient{})
		e.db.Where("organization_id = ?", e.org).Delete(&invoicing.EmailRecall{})
	})
	if err := e.db.Create(&invoicing.EmailRecall{ID: uuid.NewString(), OrganizationID: e.org, InvoiceID: e.invID, EmailLogID: logID, RecipientID: rowB.ID,
		Email: rowB.Email, Role: "cc", Kind: "invoice", LinksOff: true, RecalledAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	code, out := e.call(t, "GET", "/public/pay/"+e.token+"?r="+tokB, nil, nil)
	raw, _ := json.Marshal(out)
	if code != 410 || out["error"].(map[string]interface{})["code"] != "WITHDRAWN" || strings.Contains(string(raw), "10000") || strings.Contains(string(raw), "10,000") {
		t.Fatalf("recalled pay page: %d %s", code, raw)
	}
	if code, _ := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa", "r": tokB}, nil); code != 410 || len(e.flw.created) != 0 {
		t.Fatalf("checkout through a recalled link: %d (%d checkouts started)", code, len(e.flw.created))
	}
	code, page := e.call(t, "GET", "/public/pay/"+e.token+"?r="+tokA, nil, nil)
	if code != 200 || page["payable"] != true {
		t.Fatalf("the other recipient's pay page: %d %v", code, page)
	}
	code, co := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa", "r": tokA}, nil)
	if code != 200 {
		t.Fatalf("checkout through a live link: %d %v", code, co)
	}
	ref := co["tx_ref"].(string)

	// Every link switched off while A is at the checkout.
	old := e.token
	e.db.Model(&invoicing.Invoice{}).Where("id = ?", e.invID).Update("public_token", invoicing.NewPublicToken())
	e.db.Create(&invoicing.EmailRecall{ID: uuid.NewString(), OrganizationID: e.org, InvoiceID: e.invID, EmailLogID: logID, RecipientID: rowA.ID,
		Email: rowA.Email, Role: "to", Kind: "invoice", LinksOff: true, AllLinks: true, RetiredToken: old, RecalledAt: now})
	if code, _ := e.call(t, "GET", "/public/pay/"+old, nil, nil); code != 410 {
		t.Fatalf("old pay link after every link was switched off: %d", code)
	}
	if code, _ := e.call(t, "POST", "/public/pay/"+old+"/checkout", map[string]interface{}{"method": "mpesa"}, nil); code != 410 {
		t.Fatalf("checkout on a switched-off link: %d", code)
	}
	id := e.flw.pay(ref, 10000, "KES", "successful")
	code, v := e.call(t, "POST", "/public/pay/"+old+"/verify", map[string]interface{}{"tx_ref": ref, "transaction_id": fmt.Sprint(id), "status": "successful"}, nil)
	if code != 200 || v["status"] != TxSuccessful {
		t.Fatalf("a payment started before the switch-off must still be confirmed: %d %v", code, v)
	}
	if inv := e.invoice(t); inv.Status != invoicing.StatusPaid || len(inv.Payments) != 1 {
		t.Fatalf("invoice after the confirmed payment: %s %d", inv.Status, len(inv.Payments))
	}
}
