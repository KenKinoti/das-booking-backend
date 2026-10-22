package platform

// Reset organisation for handover: what happens to every organisation-scoped
// table of the ERP schema.
//
// Every table with an organization_id / org_id column (discovered from
// information_schema at run time) is either
//
//   - cleared or updated by a step below (child tables without an
//     organisation column are reached through their parent rows), or
//   - listed in resetNotTouched (settings, integrations, prefs …), or
//   - reported as "not touched (unclassified)" — never deleted silently.
//
// Steps run in order inside one transaction: children before their parents,
// business records first, then the optional customers / catalogue / staff
// groups and the activity log last. Conditions are SQL with the named
// parameters @org (the organisation), @owners (users kept: admins/owners) and
// @staff (every other user of the organisation). Subqueries always use an
// alias so a column can never bind to the outer table by accident.

// Optional group switches (orghooks.ResetOptions).
const (
	optCustomers = "customers"
	optCatalogue = "catalogue"
	optStaff     = "staff"
)

type resetStep struct {
	Table string
	Label string
	Where string
	// Set turns the step into an UPDATE (e.g. "current_stock = 0"); "" deletes.
	Set string
	// Soft marks an UPDATE that removes the rows (soft delete): it counts as removed.
	Soft bool
	Note string
}

type resetGroup struct {
	Key         string
	Label       string
	Description string
	Option      string // "" = always cleared; else optCustomers | optCatalogue | optStaff
	Steps       []resetStep
}

// child builds "col IN (SELECT a.pcol FROM parent a WHERE cond)"; cond uses alias a.
func child(col, parent, pcol, cond string) string {
	return col + " IN (SELECT a." + pcol + " FROM " + parent + " a WHERE " + cond + ")"
}

const (
	byOrg   = "organization_id = @org"
	byOrgID = "org_id = @org"
	aOrg    = "a.organization_id = @org"
	aOrgID  = "a.org_id = @org"
)

// Zoho / CSV import references point at local records; they are cleared with
// the records they describe.
const (
	refCustomerEntities  = "('contacts', 'contact_persons', 'customers')"
	refCatalogueEntities = "('items', 'services', 'products')"
	refKeptEntities      = "('settings')"
)

// Audit entries written by the platform itself (organisation created,
// impersonation, user created, integration settings …) survive a reset.
const platformAuditFilter = `(action LIKE 'organization.%' OR action LIKE 'user.%' OR action LIKE 'settings.%'
	OR action LIKE 'database.%' OR action LIKE 'platform.%' OR entity_type = 'ai_settings')`

var resetGroups = []resetGroup{
	{Key: "sales", Label: "Point of sale", Description: "Sales with their lines and payments, cash drawer sessions and lay-bys.", Steps: []resetStep{
		{Table: "pos_items", Label: "Sale lines", Where: child("transaction_id", "pos_transactions", "id", aOrg)},
		{Table: "pos_payments", Label: "Sale payments", Where: child("transaction_id", "pos_transactions", "id", aOrg)},
		{Table: "pos_sale_photos", Label: "Sale photos", Where: byOrg},
		{Table: "pos_transactions", Label: "Sales", Where: byOrg},
		{Table: "cash_drawers", Label: "Cash drawer sessions", Where: byOrg},
		{Table: "layby_items", Label: "Lay-by lines", Where: child("layby_payment_id", "layby_payments", "id", aOrg)},
		{Table: "layby_payment_entries", Label: "Lay-by payments", Where: child("layby_payment_id", "layby_payments", "id", aOrg)},
		{Table: "layby_payments", Label: "Lay-bys", Where: byOrg},
		{Table: "discounts", Label: "Discount usage counts", Where: byOrg + " AND COALESCE(usage_count, 0) <> 0", Set: "usage_count = 0", Note: "Usage counts restart at 0"},
	}},
	{Key: "invoicing", Label: "Invoices, quotes & payments", Description: "Invoices, quotes and credit notes with their lines, payments, receipts, attachments, email log and public links. Numbering restarts.", Steps: []resetStep{
		{Table: "billing_invoice_items", Label: "Invoice & quote lines", Where: child("invoice_id", "billing_invoices", "id", aOrg)},
		{Table: "billing_payments", Label: "Invoice payments & receipts", Where: byOrg},
		{Table: "billing_activities", Label: "Invoice activity history", Where: byOrg},
		{Table: "billing_attachments", Label: "Invoice attachments", Where: byOrg},
		{Table: "billing_cancelled_receipts", Label: "Cancelled receipts", Where: byOrg},
		{Table: "invoice_email_recipients", Label: "Who opened / viewed each email", Where: byOrg},
		{Table: "invoice_email_log", Label: "Invoice & receipt email log", Where: byOrg},
		{Table: "pay_transactions", Label: "Online payments (Flutterwave)", Where: byOrg},
		{Table: "billing_invoices", Label: "Invoices, quotes & credit notes", Where: byOrg, Note: "Their public view / pay links stop working"},
		{Table: "invoice_line_items", Label: "Legacy invoice lines", Where: child("invoice_id", "invoices", "id", aOrg)},
		{Table: "invoices", Label: "Legacy invoices", Where: byOrg},
		{Table: "billing_settings", Label: "Invoice, quote & receipt numbering",
			Where: byOrg + " AND (COALESCE(next_invoice_number, 0) <> 1001 OR COALESCE(next_quote_number, 0) <> 1001 OR COALESCE(next_receipt_number, 0) <> 1)",
			Set:   "next_invoice_number = 1001, next_quote_number = 1001, next_receipt_number = 1", Note: "Numbering restarts; prefixes and other settings are kept"},
	}},
	{Key: "bookings", Label: "Bookings & shifts", Description: "Bookings with their services, calendar state, booking emails and payments, and rostered shifts.", Steps: []resetStep{
		{Table: "booking_services", Label: "Booking services", Where: child("booking_id", "bookings", "id", aOrg)},
		{Table: "booking_calendar_state", Label: "Calendar sync state", Where: byOrg},
		{Table: "booking_email_log", Label: "Booking email log", Where: byOrg},
		{Table: "payment_transactions", Label: "Booking payments", Where: byOrg},
		{Table: "shifts", Label: "Shifts", Where: child("participant_id", "participants", "id", aOrg) + " OR " + child("staff_id", "users", "id", aOrg)},
		{Table: "bookings", Label: "Bookings", Where: byOrg},
	}},
	{Key: "events", Label: "Events & registrations", Description: "Events with their ticket types, sessions, images, reviews and registrations.", Steps: []resetStep{
		{Table: "ticket_types", Label: "Ticket types", Where: child("event_id", "events", "id", aOrg)},
		{Table: "event_sessions", Label: "Event sessions", Where: child("event_id", "events", "id", aOrg)},
		{Table: "event_images", Label: "Event images", Where: child("event_id", "events", "id", aOrg)},
		{Table: "event_reviews", Label: "Event reviews", Where: child("event_id", "events", "id", aOrg)},
		{Table: "event_registrations", Label: "Registrations & tickets", Where: byOrg},
		{Table: "events", Label: "Events", Where: byOrg},
	}},
	{Key: "crm", Label: "CRM", Description: "Leads and deals (pipeline).", Steps: []resetStep{
		{Table: "opportunities", Label: "Deals", Where: byOrg},
		{Table: "leads", Label: "Leads", Where: byOrg},
	}},
	{Key: "cashbook", Label: "Cash book", Description: "Money in / out entries and reminders.", Steps: []resetStep{
		{Table: "smallbiz_entries", Label: "Cash book entries", Where: byOrg},
		{Table: "smallbiz_reminders", Label: "Reminders", Where: byOrg},
	}},
	{Key: "bills", Label: "Bills & recurring bills", Description: "Bills with their lines, recurring bills and their runs, snoozes and reminder log.", Steps: []resetStep{
		{Table: "bill_line_items", Label: "Bill lines", Where: child("bill_id", "bills", "id", aOrg)},
		{Table: "finance_bill_snoozes", Label: "Bill snoozes", Where: byOrg},
		{Table: "finance_bill_reminder_log", Label: "Bill reminder log", Where: byOrg},
		{Table: "finance_recurring_occurrences", Label: "Recurring bill runs", Where: byOrg},
		{Table: "finance_recurring_bills", Label: "Recurring bills", Where: byOrg},
		{Table: "bills", Label: "Bills", Where: byOrg},
	}},
	{Key: "accounting", Label: "Accounting entries", Description: "Journal entries, ledger, bank statement lines, payments and the accounting audit trail. Account and bank balances go back to their opening values.", Steps: []resetStep{
		{Table: "payments", Label: "Payments", Where: byOrg},
		{Table: "journal_entry_lines", Label: "Journal lines", Where: child("journal_entry_id", "journal_entries", "id", aOrg)},
		{Table: "journal_entries", Label: "Journal entries", Where: byOrg},
		{Table: "general_ledgers", Label: "General ledger", Where: byOrg},
		{Table: "account_balances", Label: "Account balances", Where: byOrg},
		{Table: "bank_transactions", Label: "Bank statement lines", Where: byOrg},
		{Table: "audit_trails", Label: "Accounting audit trail", Where: byOrg},
		{Table: "chart_of_accounts", Label: "Account balances (chart of accounts)", Where: byOrg + " AND (COALESCE(debit_balance, 0) <> 0 OR COALESCE(credit_balance, 0) <> 0)",
			Set: "debit_balance = 0, credit_balance = 0", Note: "Accounts are kept; balances go back to 0"},
		{Table: "bank_accounts", Label: "Bank account balances", Where: byOrg + " AND balance IS DISTINCT FROM COALESCE(opening_balance, 0)",
			Set: "balance = COALESCE(opening_balance, 0)", Note: "Bank accounts are kept; balances go back to the opening balance"},
	}},
	{Key: "expenses", Label: "Expenses & imports", Description: "Expenses and their documents, renewals, Synergy uploads and statements, and Zoho / CSV / Gmail import runs.", Steps: []resetStep{
		{Table: "expense_allocations", Label: "Expense allocations", Where: byOrg},
		{Table: "expense_items", Label: "Expenses (inbox)", Where: byOrg},
		{Table: "expense_documents", Label: "Expense documents", Where: byOrg},
		{Table: "expense_renewals", Label: "Renewals", Where: byOrg},
		{Table: "expense_alert_log", Label: "Expense alerts", Where: byOrg},
		{Table: "synergy_transactions", Label: "Synergy statement lines", Where: byOrg},
		{Table: "synergy_usage_snapshots", Label: "Synergy usage snapshots", Where: byOrg},
		{Table: "synergy_uploads", Label: "Synergy uploads", Where: byOrg},
		{Table: "import_errors", Label: "Import errors", Where: byOrg},
		{Table: "import_uploads", Label: "Import files", Where: byOrgID},
		{Table: "import_external_refs", Label: "Import links (documents & payments)",
			Where: byOrg + " AND entity NOT IN " + refCustomerEntities + " AND entity NOT IN " + refCatalogueEntities + " AND entity NOT IN " + refKeptEntities},
		{Table: "import_jobs", Label: "Import runs", Where: byOrg},
	}},
	{Key: "purchasing", Label: "Purchase orders", Description: "Purchase orders and their lines. PO numbering restarts.", Steps: []resetStep{
		{Table: "purchase_order_items", Label: "Purchase order lines", Where: child("purchase_order_id", "purchase_orders", "id", aOrg)},
		{Table: "purchase_orders", Label: "Purchase orders", Where: byOrg},
	}},
	{Key: "bar_stock", Label: "Bar stock activity", Description: "Stock ledger and levels, counts, transfers, wastage, deliveries, bar sale lines, night reports and supplier price history. Stock goes back to empty.", Steps: []resetStep{
		{Table: "bar_count_lines", Label: "Stock count lines", Where: byOrg},
		{Table: "bar_counts", Label: "Stock counts", Where: byOrg},
		{Table: "bar_transfer_lines", Label: "Transfer lines", Where: byOrg},
		{Table: "bar_transfers", Label: "Transfers", Where: byOrg},
		{Table: "bar_delivery_lines", Label: "Delivery lines", Where: byOrg},
		{Table: "bar_deliveries", Label: "Deliveries", Where: byOrg},
		{Table: "bar_wastage", Label: "Wastage", Where: byOrg},
		{Table: "bar_movements", Label: "Stock ledger", Where: byOrg},
		{Table: "bar_stock", Label: "Stock levels", Where: byOrg},
		{Table: "bar_sale_lines", Label: "Bar sale lines", Where: byOrg},
		{Table: "bar_night_reports", Label: "Night reports", Where: byOrg},
		{Table: "bar_price_history", Label: "Supplier price history", Where: byOrg},
	}},
	{Key: "inventory", Label: "Inventory movements & quantities", Description: "Stock movements and quantities per location; product stock goes back to 0.", Steps: []resetStep{
		{Table: "inventory_movements", Label: "Stock movements", Where: byOrg},
		{Table: "inventory_items", Label: "Stock per location", Where: byOrg},
		{Table: "products", Label: "Product stock on hand", Where: byOrg + " AND COALESCE(current_stock, 0) <> 0", Set: "current_stock = 0", Note: "Products are kept; stock on hand goes back to 0"},
	}},
	{Key: "communication", Label: "Notifications, messages & calls", Description: "Notifications, conversations with their messages, SMS / WhatsApp / email logs, video calls, secure call links and live streams.", Steps: []resetStep{
		{Table: "notifications", Label: "Notifications", Where: byOrg},
		{Table: "message_attachments", Label: "Message attachments", Where: "message_id IN (SELECT m.id FROM messages m WHERE " + child("m.thread_id", "message_threads", "id", aOrg) + ")"},
		{Table: "message_reactions", Label: "Message reactions", Where: "message_id IN (SELECT m.id FROM messages m WHERE " + child("m.thread_id", "message_threads", "id", aOrg) + ")"},
		{Table: "message_read_statuses", Label: "Read receipts", Where: "message_id IN (SELECT m.id FROM messages m WHERE " + child("m.thread_id", "message_threads", "id", aOrg) + ")"},
		{Table: "typing_indicators", Label: "Typing indicators", Where: child("thread_id", "message_threads", "id", aOrg)},
		{Table: "messages", Label: "Messages", Where: child("thread_id", "message_threads", "id", aOrg)},
		{Table: "message_participants", Label: "Conversation members", Where: child("thread_id", "message_threads", "id", aOrg)},
		{Table: "message_threads", Label: "Conversations", Where: byOrg},
		{Table: "communication_logs", Label: "SMS / WhatsApp / email log", Where: byOrg},
		{Table: "web_rtc_signals", Label: "Call signalling", Where: "session_id IN (SELECT a.call_id::text FROM video_calls a WHERE " + aOrg +
			" UNION SELECT a.session_id::text FROM screen_shares a WHERE " + aOrg + " UNION SELECT a.stream_id::text FROM live_streams a WHERE " + aOrg + ")"},
		{Table: "call_participants", Label: "Call participants", Where: child("call_id", "video_calls", "id", aOrg)},
		{Table: "call_recordings", Label: "Call recordings", Where: child("call_id", "video_calls", "id", aOrg)},
		{Table: "screen_shares", Label: "Screen shares", Where: byOrg},
		{Table: "video_calls", Label: "Video calls", Where: byOrg},
		{Table: "stream_viewers", Label: "Stream viewers", Where: child("stream_id", "live_streams", "id", aOrg)},
		{Table: "stream_recordings", Label: "Stream recordings", Where: child("stream_id", "live_streams", "id", aOrg)},
		{Table: "live_streams", Label: "Live streams", Where: byOrg},
		{Table: "seccall_messages", Label: "Secure call signalling", Where: child("room_id", "seccall_rooms", "id", aOrg)},
		{Table: "seccall_participants", Label: "Secure call participants", Where: child("room_id", "seccall_rooms", "id", aOrg)},
		{Table: "seccall_rooms", Label: "Secure call links", Where: byOrg},
		{Table: "web_socket_connections", Label: "Live connections", Where: byOrg},
	}},
	{Key: "meetings", Label: "Meetings", Description: "Meetings with their invitees and transcripts.", Steps: []resetStep{
		{Table: "meeting_participants", Label: "Meeting invitees", Where: byOrgID},
		{Table: "meeting_transcripts", Label: "Transcripts & summaries", Where: byOrgID},
		{Table: "meetings", Label: "Meetings", Where: byOrgID},
	}},
	{Key: "hr_records", Label: "HR records", Description: "Leave requests, timesheets and pay runs.", Steps: []resetStep{
		{Table: "hr_pay_run_lines", Label: "Pay run lines", Where: child("pay_run_id", "hr_pay_runs", "id", aOrgID)},
		{Table: "hr_pay_runs", Label: "Pay runs", Where: byOrgID},
		{Table: "hr_timesheets", Label: "Timesheets", Where: byOrgID},
		{Table: "hr_leave_requests", Label: "Leave requests", Where: byOrgID},
	}},
	{Key: "projects", Label: "Projects & tasks", Description: "Projects and their tasks (Jira projects sync again from a kept connection).", Steps: []resetStep{
		{Table: "pm_tasks", Label: "Tasks", Where: byOrgID},
		{Table: "pm_projects", Label: "Projects", Where: byOrgID},
	}},
	{Key: "production", Label: "Manufacturing work orders", Description: "Work orders (bills of materials are catalogue).", Steps: []resetStep{
		{Table: "mrp_work_orders", Label: "Work orders", Where: byOrgID},
	}},
	{Key: "online_orders", Label: "Online store orders", Description: "Orders imported from connected online stores (the connections are kept).", Steps: []resetStep{
		{Table: "ecom_orders", Label: "Online orders", Where: byOrgID},
	}},
	{Key: "insights", Label: "Insights feed & briefs", Description: "Industry-watch items and strategy briefs (feeds, rules and settings are kept).", Steps: []resetStep{
		{Table: "insights_watch_items", Label: "Watch items", Where: byOrg},
		{Table: "insights_briefs", Label: "Strategy briefs", Where: byOrg},
	}},
	{Key: "assistant", Label: "Assistant conversations & usage", Description: "Ask DASYIN conversations and the AI / MCP tool usage log.", Steps: []resetStep{
		{Table: "assistant_messages", Label: "Assistant messages", Where: byOrgID},
		{Table: "assistant_conversations", Label: "Assistant conversations", Where: byOrgID},
		{Table: "ai_activity", Label: "AI tool usage log", Where: byOrgID},
	}},
	{Key: "documents", Label: "Documents", Description: "Uploaded documents and their files.", Steps: []resetStep{
		{Table: "document_blobs", Label: "Document files", Where: byOrg},
		{Table: "documents", Label: "Documents", Where: child("uploaded_by", "users", "id", aOrg) + " OR " + child("participant_id", "participants", "id", aOrg)},
	}},
	{Key: "notices", Label: "Dismissed tips & notices", Description: "One-time notices that were dismissed while testing show again.", Steps: []resetStep{
		{Table: "org_notices", Label: "Dismissed notices", Where: byOrg},
	}},

	// ---- optional
	{Key: "customers", Option: optCustomers, Label: "Customers & guests", Description: "Customers with their contacts and invoice / receipt copy lists, vehicles, CRM customers, participants and hosted sites.", Steps: []resetStep{
		{Table: "import_external_refs", Label: "Import links (customers)", Where: byOrg + " AND entity IN " + refCustomerEntities},
		{Table: "customer_contacts", Label: "Customer contacts & cc lists", Where: byOrg},
		{Table: "vehicles", Label: "Vehicles", Where: byOrg},
		{Table: "synergy_sites", Label: "Hosted sites (customer links)", Where: byOrg},
		{Table: "crm_customers", Label: "CRM customers", Where: byOrg},
		{Table: "care_plans", Label: "Care plans", Where: child("participant_id", "participants", "id", aOrg)},
		{Table: "emergency_contacts", Label: "Emergency contacts", Where: child("participant_id", "participants", "id", aOrg)},
		{Table: "participants", Label: "Participants", Where: byOrg},
		{Table: "customers", Label: "Customers & guests", Where: byOrg},
	}},
	{Key: "catalogue", Option: optCatalogue, Label: "Catalogue", Description: "Products, categories, brands, services, stock items with recipes and par levels, locations and terminals, discounts, suppliers and price lists.", Steps: []resetStep{
		{Table: "import_external_refs", Label: "Import links (items & services)", Where: byOrg + " AND entity IN " + refCatalogueEntities},
		{Table: "bar_recipe_lines", Label: "Recipe lines", Where: byOrg},
		{Table: "bar_recipes", Label: "Recipes", Where: byOrg},
		{Table: "bar_par", Label: "Par levels", Where: byOrg},
		{Table: "bar_terminals", Label: "POS terminals → bars", Where: byOrg},
		{Table: "bar_location_staff", Label: "Staff assigned to bars", Where: byOrg},
		{Table: "bar_items", Label: "Stock items", Where: byOrg},
		{Table: "bar_settings", Label: "Default bar & main store", Where: byOrg + " AND (default_bar_id IS NOT NULL OR main_store_id IS NOT NULL)",
			Set: "default_bar_id = NULL, main_store_id = NULL", Note: "Bar stock settings are kept; the deleted locations are unset"},
		{Table: "mrp_bom_components", Label: "Bill of materials lines", Where: child("bom_id", "mrp_boms", "id", aOrgID)},
		{Table: "mrp_boms", Label: "Bills of materials", Where: byOrgID},
		{Table: "pricebook_price_changes", Label: "Price list changes", Where: byOrg},
		{Table: "pricebook_items", Label: "Price list items", Where: byOrg},
		{Table: "pricebook_scenarios", Label: "Price list scenarios", Where: byOrg},
		{Table: "pricebook_suppliers", Label: "Price list suppliers (own)", Where: byOrg + " AND COALESCE(system, '') = ''", Note: "The built-in suppliers are kept"},
		{Table: "synergy_plans", Label: "Hosting plans", Where: byOrg},
		{Table: "discounts", Label: "Discounts", Where: byOrg},
		{Table: "products", Label: "Products", Where: byOrg},
		{Table: "product_categories", Label: "Product categories", Where: byOrg},
		{Table: "brands", Label: "Brands", Where: byOrg},
		{Table: "services", Label: "Services", Where: byOrg},
		{Table: "inventory_locations", Label: "Stores & bars (locations)", Where: byOrg},
		{Table: "suppliers", Label: "Suppliers", Where: byOrg},
		{Table: "vendors", Label: "Vendors (bills)", Where: byOrg},
		{Table: "expense_vendors", Label: "Expense vendors (own)", Where: byOrg + " AND NOT COALESCE(builtin, false)", Note: "The built-in vendors are kept"},
	}},
	{Key: "staff", Option: optStaff, Label: "Staff accounts", Description: "Every user except the organisation's admins / owners: their accounts are deactivated and removed (soft delete, like deleting a user), sessions and tokens revoked, role assignments and personal settings removed; employee records and pending invitations are deleted.", Steps: []resetStep{
		{Table: "industry_user_roles", Label: "Role assignments", Where: "user_id IN @staff"},
		{Table: "bar_location_staff", Label: "Staff assigned to bars", Where: byOrg + " AND user_id IN @staff"},
		{Table: "booking_email_user_prefs", Label: "Booking email preferences", Where: "user_id IN @staff"},
		{Table: "email_recipient_prefs", Label: "Email copy preferences", Where: byOrg + " AND user_id IN @staff"},
		{Table: "assistant_user_settings", Label: "Assistant preferences", Where: "user_id IN @staff"},
		{Table: "booking_calendar_feeds", Label: "Calendar feed links", Where: byOrg + " AND user_id IN @staff"},
		{Table: "user_permissions", Label: "User permissions", Where: "user_id IN @staff"},
		{Table: "user_identities", Label: "Single sign-on links", Where: "user_id IN @staff"},
		{Table: "hr_employees", Label: "Employee records", Where: byOrgID + " AND COALESCE(user_id, '') NOT IN @owners"},
		{Table: "organization_invitations", Label: "Pending invitations", Where: byOrg},
		{Table: "mcp_tokens", Label: "API tokens (revoked)", Where: "user_id IN @staff AND revoked_at IS NULL", Set: "revoked_at = now()"},
		{Table: "mcp_oauth_grants", Label: "Connected apps (revoked)", Where: "user_id IN @staff AND revoked_at IS NULL", Set: "revoked_at = now()"},
		{Table: "refresh_tokens", Label: "Sessions (revoked)", Where: "user_id IN @staff AND NOT COALESCE(is_revoked, false)", Set: "is_revoked = true"},
		{Table: "users", Label: "Staff users", Where: byOrg + " AND deleted_at IS NULL AND id IN @staff", Set: "deleted_at = now(), is_active = false, updated_at = now()", Soft: true,
			Note: "Deactivated and removed like a deleted user"},
	}},

	{Key: "activity", Label: "Activity log", Description: "The organisation's activity log; platform entries (organisation created, sign-ins as the organisation, this reset) are kept.", Steps: []resetStep{
		{Table: "audit_logs", Label: "Activity log entries", Where: byOrg + " AND NOT " + platformAuditFilter},
	}},
}

// resetNotTouched are organisation tables a reset never changes, with the reason.
var resetNotTouched = map[string]string{
	"organization_settings":          "Organisation preferences",
	"organization_brandings":         "Branding",
	"organization_modules":           "Modules",
	"organization_subscriptions":     "Subscription",
	"plan_subscriptions":             "Plan & subscription",
	"platform_org_status":            "Platform status",
	"industry_org_settings":          "Business type (only the setup wizard is reopened)",
	"industry_org_roles":             "Roles & permissions",
	"roles":                          "Roles & permissions",
	"org_logos":                      "Logo",
	"billing_tax_rates":              "Tax rates",
	"tax_rates":                      "Tax rates",
	"fx_manual_rates":                "Exchange rates",
	"expense_fx_months":              "Exchange rates",
	"smallbiz_settings":              "Cash book settings",
	"smallbiz_categories":            "Cash book categories",
	"accounting_periods":             "Accounting periods",
	"hr_departments":                 "Departments",
	"pay_settings":                   "Payment gateway (Flutterwave keys)",
	"integration_settings":           "Email / SMTP & integrations",
	"integration_logs":               "Email / SMTP & integrations",
	"mail_send_status":               "Email / SMTP & integrations",
	"message_settings":               "Messaging settings",
	"message_integrations":           "Messaging integrations",
	"message_templates":              "Message templates",
	"video_settings":                 "Video settings",
	"booking_email_settings":         "Booking email settings",
	"email_recipient_policies":       "Email copy settings",
	"finance_bill_reminder_settings": "Bill reminder settings",
	"pricebook_settings":             "Price list settings",
	"expense_prefs":                  "Expense settings",
	"expense_connections":            "Gmail / Synergy connections",
	"zoho_connections":               "Zoho connection",
	"zoho_oauth_states":              "Zoho connection",
	"ecom_connections":               "Online store connections",
	"pm_jira_connections":            "Jira connection",
	"insights_settings":              "Insights settings",
	"insights_watch_feeds":           "Insights feeds",
	"insights_watch_rules":           "Insights rules",
	"insights_custom_indicators":     "Insights indicators",
	"insights_datasets":              "Insights datasets",
	"insights_service_rules":         "Insights service rules",
	"invoice_email_tracking":         "Email tracking settings",
	"occasion_org_settings":          "Seasonal theme settings",
	"broadcast_deliveries":           "Platform announcements (sent by the platform)",
	"broadcast_unsubscribes":         "Platform announcement email choices",
	"sso_domain_rules":               "Single sign-on rules",
}

// resetHookTables are organisation tables cleared by an orghooks reset hook
// of the package that owns them (table → hook name). While that hook is not
// registered the table is reported as unclassified.
var resetHookTables = map[string]string{
	"onboarding_progress": "onboarding", // pkg/onboarding: getting-started progress
	// pkg/subscriptions: client subscriptions (subscription_settings: numbering restarts, settings stay)
	"client_subscriptions": "subscriptions", "client_subscription_lines": "subscriptions", "client_subscription_periods": "subscriptions",
	"client_subscription_tasks": "subscriptions", "client_subscription_events": "subscriptions", "client_subscription_plan_map": "subscriptions",
	"subscription_settings": "subscriptions",
}
