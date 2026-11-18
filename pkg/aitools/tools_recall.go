package aitools

import (
	"net/url"
	"strings"
)

// invoices_recall_email — undo a held email or recall a sent one
// (pkg/invoicing hold.go, recall.go).
func init() {
	Register(Tool{
		Name:  "invoices_recall_email",
		Title: "Recall an invoice email",
		Description: "Recalls an invoice, quote, payment-reminder or receipt email. An email still held in the undo window is simply " +
			"cancelled — nothing is sent and the document stays as it was. A sent email can't be deleted from anyone's inbox " +
			"(and an attached PDF stays where it landed — say so), but this switches off its links: those people's View / Pay / " +
			"accept links show a 'withdrawn' page and online payment through them is refused. all_links=true also switches off " +
			"every link to the document (other emails, the PDF, shared links; a later send issues new ones). send_notice (default " +
			"true) emails the same people a short 'please disregard' notice with an optional reason and an optional link to the " +
			"corrected document (replacement_number, which must already be sent). Pick the email by email_log_id " +
			"(email_status.summary_log_id / held_log_id from invoices_get) or leave it out for the latest one; recipients limits " +
			"it to some people. Always confirm with the user first, then tell them what can't be taken back.",
		InputSchema: tObj(map[string]any{
			"invoice_id":         tStr("Invoice or quote id"),
			"number":             tStr("Document number, e.g. INV-1043 (instead of invoice_id)"),
			"email_log_id":       tStr("The email to recall (default: the one still held, else the latest sent invoice / quote / reminder / receipt email)"),
			"recipients":         tArr("Only these addresses (default: everyone the email went to)", tStr("email")),
			"all_links":          tBool("Also switch off every link to the document, not just this email's (default false)"),
			"send_notice":        tBool("Email the recipients a 'please disregard' notice (default true)"),
			"reason":             tStr("Short reason shown in the notice, e.g. 'the amount was wrong'"),
			"replacement_number": tStr("Number of the corrected document (already sent) to link in the notice, e.g. INV-1044"),
		}),
		Destructive: true,
		Module:      "invoicing",
		Handler:     invoicesRecallEmail,
	})
	idempotentTools["invoices_recall_email"] = false
}

func invoicesRecallEmail(c *Ctx, a map[string]any) (any, error) {
	inv, err := c.loadInvoice(a, "invoice_id")
	if err != nil {
		return nil, err
	}
	id := tS(inv, "id")
	v, err := c.get("/invoicing/invoices/"+url.PathEscape(id)+"/email-log", nil)
	if err != nil {
		return nil, err
	}
	logs := tList(v, "log")
	var pick map[string]any
	if want := argStr(a, "email_log_id"); want != "" {
		for _, e := range logs {
			if tS(e, "id") == want {
				pick = e
			}
		}
		if pick == nil {
			return nil, tErr("No email %s on %s", want, tS(inv, "number"))
		}
	} else {
		for _, e := range logs { // newest first: a held email wins, else the latest sent one
			if tS(e, "status") == "held" {
				pick = e
				break
			}
		}
		if pick == nil {
			for _, e := range logs {
				if tS(e, "status") == "sent" && tS(e, "mode") == "email" && tS(e, "kind") != "notice" {
					pick = e
					break
				}
			}
		}
		if pick == nil {
			return nil, tErr("%s has no email sent from DASYIN to recall", tS(inv, "number"))
		}
	}
	base := "/invoicing/invoices/" + url.PathEscape(id) + "/emails/" + url.PathEscape(tS(pick, "id"))
	out := map[string]any{"invoice_id": id, "number": tS(inv, "number"), "url": tURL(docPath(inv)), "email_log_id": tS(pick, "id"),
		"kind": tS(pick, "kind"), "to": tS(pick, "to")}

	if tS(pick, "status") == "held" {
		if _, err := c.api("POST", base+"/undo", nil, map[string]any{}); err != nil {
			return nil, err
		}
		out["undone"] = true
		out["message"] = "Undone before it was sent — nothing reached " + tS(pick, "to") + " and the document's status is unchanged."
		return out, nil
	}

	body := map[string]any{"notice": true}
	if b, ok := argBool(a, "send_notice"); ok {
		body["notice"] = b
	}
	if b, ok := argBool(a, "all_links"); ok {
		body["all_links"] = b
	}
	copyArgs(body, a, "reason")
	if argHas(a, "recipients") {
		body["recipients"] = argStrList(a, "recipients")
	}
	if n := argStr(a, "replacement_number"); n != "" {
		r, err := c.loadInvoice(map[string]any{"number": n}, "replacement_id")
		if err != nil {
			return nil, err
		}
		body["replacement_id"] = tS(r, "id")
	}
	if AppURL() != "" {
		body["app_url"] = AppURL()
	}
	res, err := c.api("POST", base+"/recall", nil, body)
	if err != nil {
		return nil, err
	}
	d := tObjOf(res)
	var who []string
	for _, r := range tList(d, "recalled") {
		who = append(who, tS(r, "email"))
	}
	out["recalled"] = tList(d, "recalled")
	out["all_links"] = d["all_links"]
	if s, ok := d["skipped"].([]any); ok && len(s) > 0 {
		out["skipped"] = s
	}
	msg := "Links switched off for " + strings.Join(who, ", ") + "."
	if b, _ := d["all_links"].(bool); b {
		msg += " Every link to " + tS(inv, "number") + " is off too; sending it again issues new links."
	}
	if n := tObjOf(d, "notice"); len(n) > 0 {
		out["notice"] = tPick(n, "sent", "to", "cc", "subject", "error")
		if b, _ := n["sent"].(bool); b {
			msg += " A 'please disregard' notice was emailed."
		} else if e := tS(n, "error"); e != "" {
			msg += " The notice was not sent: " + e
		}
	}
	if note := tS(d, "attachment_note"); note != "" {
		out["attachment_note"] = note
		msg += " " + note
	}
	msg += " The email itself can't be deleted from their inbox."
	out["message"] = msg
	return out, nil
}
