package industry

// The seeded registry. After the first run the database is the source of
// truth; super admins edit it from Platform admin → Business types. Seeding
// never overwrites edits (missing types are added on start).

// DefaultTerms are the words every type starts from.
func DefaultTerms() map[string]string {
	return map[string]string{
		"customer": "Customer", "customers": "Customers",
		"booking": "Booking", "bookings": "Bookings",
		"service": "Service", "services": "Services",
		"staff_member": "Staff member", "staff": "Staff",
		"inventory": "Inventory", "inventory_item": "Product",
		"project": "Project", "projects": "Projects",
		"event": "Event", "events": "Events",
	}
}

// action sets
var (
	full  = []string{ActView, ActCreate, ActEdit, ActDelete, ActApprove}
	crud  = []string{ActView, ActCreate, ActEdit, ActDelete}
	work  = []string{ActView, ActCreate, ActEdit}
	upd   = []string{ActView, ActEdit}
	add   = []string{ActView, ActCreate}
	view  = []string{ActView}
	nopes = []string{}
)

// perms builds a Perms from module, actions pairs.
func perms(pairs ...interface{}) Perms {
	p := Perms{}
	for i := 0; i+1 < len(pairs); i += 2 {
		p[pairs[i].(string)] = append([]string{}, pairs[i+1].([]string)...)
	}
	return p
}

func owner(name string) RoleTemplate {
	if name == "" {
		name = "Owner / Admin"
	}
	return RoleTemplate{Key: "owner", Name: name, Description: "Full access to everything, including settings, billing and users.",
		Base: BaseAdmin, Scope: ScopeAll, Perms: perms(AllModules, full), Legacy: "admin"}
}

func manager(key, name, desc string) RoleTemplate {
	return RoleTemplate{Key: key, Name: name, Description: desc, Base: BaseManager, Scope: ScopeAll,
		Perms: perms(AllModules, full, PermTeam, view), Legacy: "manager"}
}

// DefaultTypes is the seeded registry.
func DefaultTypes() []BusinessType {
	t := []BusinessType{
		{
			Key: "mechanic", Name: "Mechanic / auto repair", Icon: "fa-solid fa-car", PlanIndustry: "automotive",
			Description: "Workshops, garages, tyre and auto-electrical shops.",
			Modules:     []string{"bookings", "invoicing", "inventory", "pos", "crm"},
			HiddenPaths: []string{"/crm"},
			Terms: map[string]string{"bookings": "Job cards", "booking": "Job card", "services": "Labour & services", "service": "Labour item",
				"staff": "Technicians", "staff_member": "Technician", "inventory": "Parts", "inventory_item": "Part", "operations": "Workshop"},
			Roles: []RoleTemplate{
				owner(""),
				manager("workshop_manager", "Workshop manager", "Runs the workshop: job cards, technicians, parts and invoices."),
				{Key: "service_advisor", Name: "Service advisor", Description: "Books vehicles in, writes job cards and quotes, invoices customers.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "bookings", work, "crm", work, "invoicing", work, "pos", add, "inventory", view)},
				{Key: "technician", Name: "Technician", Description: "Works on the job cards assigned to them and looks up parts.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings",
					Perms: perms("bookings", upd, "inventory", view)},
				{Key: "parts_clerk", Name: "Parts clerk", Description: "Manages parts stock, suppliers and purchase orders.", Base: BaseStaff, Scope: ScopeAll, Home: "/inventory",
					Perms: perms(PermCore, view, "inventory", crud, "bookings", view, "pos", view)},
				{Key: "cashier", Name: "Cashier", Description: "Takes payments at the counter and issues invoices and receipts.", Base: BaseStaff, Scope: ScopeAll, Home: "/pos",
					Perms: perms(PermCore, view, "pos", add, "invoicing", work, "crm", add, "bookings", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "services", Label: "Add your labour rates and services", To: "/services"},
				{Key: "parts", Label: "Load your parts list", To: "/inventory"},
				{Key: "team", Label: "Invite your technicians", To: "/staff"},
				{Key: "booking", Label: "Open your first job card", To: "/bookings"},
				{Key: "invoice", Label: "Send your first invoice", To: "/invoices/new"},
			},
			SampleServices: []SampleService{
				{Name: "Oil & filter change", Category: "Servicing", Duration: 45, Example: true},
				{Name: "Logbook service", Category: "Servicing", Duration: 120, Example: true},
				{Name: "Brake pads (per axle)", Category: "Brakes", Duration: 90, Example: true},
				{Name: "Diagnostic scan", Category: "Diagnostics", Duration: 60, Example: true},
				{Name: "Wheel alignment", Category: "Tyres & wheels", Duration: 60, Example: true},
				{Name: "Battery test & replace", Category: "Electrical", Duration: 30, Example: true},
			},
			BookingDefaults:  map[string]any{"vehicle_required": true, "resource_label": "Bay", "default_duration": 60},
			DashboardWidgets: []string{"jobs_today", "jobs_in_progress", "revenue", "parts_low_stock", "unpaid_invoices"},
			Features:         []string{"vehicles"},
		},
		{
			Key: "salon", Name: "Salon & beauty", Icon: "fa-solid fa-scissors", PlanIndustry: "beauty",
			Description: "Hair salons, barbers, nail and beauty studios.",
			Modules:     []string{"bookings", "pos", "crm", "invoicing", "communication", "inventory"},
			HiddenPaths: []string{"/crm"},
			Terms: map[string]string{"bookings": "Appointments", "booking": "Appointment", "services": "Treatments", "service": "Treatment",
				"staff": "Stylists", "staff_member": "Stylist", "customers": "Clients", "customer": "Client", "inventory": "Retail products", "operations": "Appointments"},
			Roles: []RoleTemplate{
				owner("Owner"),
				manager("salon_manager", "Salon manager", "Runs the floor: appointments, stylists, stock and takings."),
				{Key: "receptionist", Name: "Receptionist", Description: "Books appointments, checks clients in and takes payment.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "bookings", work, "crm", work, "pos", add, "invoicing", add, "communication", work, "inventory", view)},
				{Key: "stylist", Name: "Stylist", Description: "Sees and updates their own appointments.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings",
					Perms: perms("bookings", upd, "crm", view, "pos", add)},
				{Key: "beautician", Name: "Beautician", Description: "Sees and updates their own treatments.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings",
					Perms: perms("bookings", upd, "crm", view, "pos", add)},
			},
			Checklist: []ChecklistItem{
				{Key: "services", Label: "Set up your treatment menu", To: "/services"},
				{Key: "team", Label: "Invite your stylists", To: "/staff"},
				{Key: "booking", Label: "Take your first appointment", To: "/bookings"},
				{Key: "pos", Label: "Ring up a sale at the till", To: "/pos"},
			},
			SampleServices: []SampleService{
				{Name: "Women's cut & blow-dry", Category: "Hair", Duration: 60, Example: true},
				{Name: "Men's cut", Category: "Hair", Duration: 30, Example: true},
				{Name: "Full colour", Category: "Colour", Duration: 120, Example: true},
				{Name: "Gel manicure", Category: "Nails", Duration: 45, Example: true},
				{Name: "Brow shape & tint", Category: "Beauty", Duration: 30, Example: true},
			},
			BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "Chair / station", "default_duration": 45},
			DashboardWidgets: []string{"appointments_today", "utilisation", "revenue", "rebooking_rate", "top_services"},
			Features:         []string{"stations"},
		},
		{
			Key: "spa", Name: "Spa & wellness", Icon: "fa-solid fa-spa", PlanIndustry: "beauty",
			Description: "Day spas, massage, wellness and fitness studios.",
			Modules:     []string{"bookings", "pos", "crm", "invoicing", "communication", "inventory"},
			HiddenPaths: []string{"/crm"},
			Terms: map[string]string{"bookings": "Appointments", "booking": "Appointment", "services": "Treatments", "service": "Treatment",
				"staff": "Therapists", "staff_member": "Therapist", "customers": "Guests", "customer": "Guest", "inventory": "Retail products", "operations": "Appointments"},
			Roles: []RoleTemplate{
				owner("Owner"),
				manager("spa_manager", "Spa manager", "Runs the spa: appointments, therapists, rooms and takings."),
				{Key: "receptionist", Name: "Receptionist", Description: "Books treatments, checks guests in and takes payment.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "bookings", work, "crm", work, "pos", add, "invoicing", add, "communication", work, "inventory", view)},
				{Key: "therapist", Name: "Therapist", Description: "Sees and updates their own treatments.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings",
					Perms: perms("bookings", upd, "crm", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "services", Label: "Set up your treatment menu", To: "/services"},
				{Key: "team", Label: "Invite your therapists", To: "/staff"},
				{Key: "booking", Label: "Take your first booking", To: "/bookings"},
			},
			SampleServices: []SampleService{
				{Name: "Relaxation massage (60 min)", Category: "Massage", Duration: 60, Example: true},
				{Name: "Deep tissue massage (90 min)", Category: "Massage", Duration: 90, Example: true},
				{Name: "Signature facial", Category: "Skin", Duration: 75, Example: true},
				{Name: "Body scrub & wrap", Category: "Body", Duration: 60, Example: true},
			},
			BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "Room", "default_duration": 60},
			DashboardWidgets: []string{"appointments_today", "utilisation", "revenue", "top_services"},
			Features:         []string{"stations"},
		},
		{
			Key: "clinic", Name: "Clinic / health", Icon: "fa-solid fa-stethoscope", PlanIndustry: "healthcare",
			Description: "GPs, dental, physio, allied health and specialist practices.",
			Modules:     []string{"bookings", "invoicing", "crm", "documents", "communication", "hr"},
			HiddenPaths: []string{"/crm"},
			Terms: map[string]string{"customers": "Patients", "customer": "Patient", "bookings": "Appointments", "booking": "Appointment",
				"services": "Consultations", "service": "Consultation", "staff": "Practitioners", "staff_member": "Practitioner", "inventory": "Supplies", "operations": "Appointments"},
			Roles: []RoleTemplate{
				owner(""),
				manager("practice_manager", "Practice manager", "Runs the practice: rosters, appointments, billing and records."),
				{Key: "practitioner", Name: "Doctor / Practitioner", Description: "Sees their own appointments and patient records.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings",
					Perms: perms(PermCore, view, "bookings", work, "crm", work, "documents", work, "communication", work)},
				{Key: "nurse", Name: "Nurse", Description: "Supports appointments and keeps patient notes.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "bookings", upd, "crm", upd, "documents", add, "communication", work)},
				{Key: "receptionist", Name: "Receptionist", Description: "Books appointments, registers patients and takes payment.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "bookings", work, "crm", work, "invoicing", work, "communication", work, "documents", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "services", Label: "List your consultation types", To: "/services"},
				{Key: "team", Label: "Invite practitioners and reception", To: "/staff"},
				{Key: "booking", Label: "Book the first appointment", To: "/bookings"},
			},
			SampleServices: []SampleService{
				{Name: "Standard consultation", Category: "Consultations", Duration: 15, Example: true},
				{Name: "Long consultation", Category: "Consultations", Duration: 30, Example: true},
				{Name: "Initial assessment", Category: "Assessments", Duration: 45, Example: true},
				{Name: "Follow-up review", Category: "Consultations", Duration: 20, Example: true},
			},
			BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "Room", "default_duration": 15},
			DashboardWidgets: []string{"appointments_today", "no_shows", "revenue", "unpaid_invoices"},
		},
		{
			Key: "ndis", Name: "NDIS / care provider", Icon: "fa-solid fa-hand-holding-medical", PlanIndustry: "healthcare",
			Description: "NDIS, disability support, aged and home-care providers.",
			Modules:     []string{"bookings", "hr", "documents", "invoicing", "communication", "crm"},
			HiddenPaths: []string{"/crm"},
			Terms: map[string]string{"customers": "Participants", "customer": "Participant", "bookings": "Shifts", "booking": "Shift",
				"services": "Supports", "service": "Support", "staff": "Support workers", "staff_member": "Support worker", "operations": "Shifts & supports"},
			Roles: []RoleTemplate{
				owner(""),
				manager("coordinator", "Coordinator", "Plans supports, rosters workers and manages participants."),
				{Key: "support_worker", Name: "Support worker", Description: "Sees and completes the shifts assigned to them.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings", Legacy: "staff",
					Perms: perms("bookings", upd, "crm", view, "documents", add, "communication", work)},
				{Key: "admin_officer", Name: "Admin / finance officer", Description: "Invoices the plan managers and keeps records.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "bookings", view, "crm", work, "invoicing", work, "documents", work)},
			},
			Checklist: []ChecklistItem{
				{Key: "services", Label: "Add your support items and rates", To: "/services"},
				{Key: "team", Label: "Invite your support workers", To: "/staff"},
				{Key: "participants", Label: "Add your participants", To: "/customers"},
				{Key: "booking", Label: "Roster the first shift", To: "/bookings"},
			},
			SampleServices: []SampleService{
				{Name: "Assistance with self-care (weekday)", Category: "Core supports", Duration: 120, Example: true},
				{Name: "Community participation", Category: "Core supports", Duration: 180, Example: true},
				{Name: "Support coordination", Category: "Capacity building", Duration: 60, Example: true},
			},
			BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "", "default_duration": 120},
			DashboardWidgets: []string{"shifts_today", "unfilled_shifts", "revenue", "unpaid_invoices"},
		},
		{
			Key: "consulting", Name: "Professional services / consulting", Icon: "fa-solid fa-briefcase", PlanIndustry: "professional",
			Description: "Consultants, accountants, lawyers and advisory firms.",
			Modules:     []string{"invoicing", "crm", "projects", "documents", "communication", "accounting"},
			Terms:       map[string]string{"customers": "Clients", "customer": "Client", "projects": "Engagements", "project": "Engagement", "staff": "Team"},
			Roles: []RoleTemplate{
				owner("Partner / Owner"),
				manager("engagement_manager", "Engagement manager", "Runs client engagements, budgets and billing."),
				{Key: "consultant", Name: "Consultant", Description: "Works on engagements and client records.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "projects", work, "crm", work, "documents", work, "communication", work, "invoicing", view)},
				{Key: "bookkeeper", Name: "Bookkeeper", Description: "Invoices clients and keeps the books.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "invoicing", crud, "accounting", work, "crm", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "clients", Label: "Import your clients", To: "/customers"},
				{Key: "projects", Label: "Set up your first engagement", To: "/projects"},
				{Key: "invoice", Label: "Send your first invoice", To: "/invoices/new"},
			},
			DashboardWidgets: []string{"pipeline", "unbilled_time", "revenue", "unpaid_invoices"},
		},
		{
			Key: "agency", Name: "Web & IT agency", Icon: "fa-solid fa-laptop-code", PlanIndustry: "professional",
			Description: "Web, software, design and managed IT agencies.",
			Modules:     []string{"projects", "invoicing", "crm", "documents", "communication", "accounting"},
			Terms:       map[string]string{"customers": "Clients", "customer": "Client", "staff": "Team"},
			Roles: []RoleTemplate{
				owner("Director"),
				manager("project_manager", "Project manager", "Plans projects, assigns work and bills clients."),
				{Key: "developer", Name: "Developer", Description: "Works on projects and issues.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/projects",
					Perms: perms(PermCore, view, "projects", work, "documents", work, "communication", work)},
				{Key: "designer", Name: "Designer", Description: "Works on projects and design files.", Base: BaseStaff, Scope: ScopeAll, Home: "/projects",
					Perms: perms(PermCore, view, "projects", work, "documents", work, "communication", work)},
				{Key: "support", Name: "Support", Description: "Handles client requests and logs issues.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "crm", work, "projects", add, "communication", work, "documents", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "clients", Label: "Add your clients", To: "/customers"},
				{Key: "projects", Label: "Create a project board", To: "/projects"},
				{Key: "team", Label: "Invite your team", To: "/staff"},
				{Key: "invoice", Label: "Send your first invoice", To: "/invoices/new"},
			},
			SampleServices: []SampleService{
				{Name: "Website maintenance (monthly)", Category: "Retainers", Duration: 60, Example: true},
				{Name: "Development hour", Category: "Development", Duration: 60, Example: true},
				{Name: "Design hour", Category: "Design", Duration: 60, Example: true},
				{Name: "IT support call-out", Category: "Support", Duration: 60, Example: true},
			},
			DashboardWidgets: []string{"open_issues", "sprint_progress", "revenue", "unpaid_invoices"},
		},
		{
			Key: "retail", Name: "Retail shop", Icon: "fa-solid fa-bag-shopping", PlanIndustry: "retail",
			Description: "Shops, boutiques, hardware and online stores.",
			Modules:     []string{"pos", "inventory", "ecommerce", "invoicing", "crm", "accounting"},
			HiddenPaths: []string{"/crm"},
			Terms:       map[string]string{"inventory": "Stock", "inventory_item": "Product", "staff": "Shop staff"},
			Roles: []RoleTemplate{
				owner(""),
				manager("store_manager", "Store manager", "Runs the shop: stock, staff, till and suppliers."),
				{Key: "cashier", Name: "Cashier", Description: "Sells at the till and looks up stock.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/pos",
					Perms: perms(PermCore, view, "pos", add, "crm", add, "inventory", view)},
				{Key: "stock_clerk", Name: "Stock clerk", Description: "Receives deliveries and counts stock.", Base: BaseStaff, Scope: ScopeAll, Home: "/inventory",
					Perms: perms(PermCore, view, "inventory", crud, "pos", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "products", Label: "Load your products", To: "/inventory"},
				{Key: "pos", Label: "Make your first sale", To: "/pos"},
				{Key: "team", Label: "Invite your shop staff", To: "/staff"},
			},
			DashboardWidgets: []string{"sales_today", "top_products", "low_stock", "revenue"},
		},
		{
			Key: "restaurant", Name: "Restaurant / café", Icon: "fa-solid fa-mug-hot", PlanIndustry: "hospitality",
			Description: "Restaurants, cafés, takeaways and catering.",
			Modules:     []string{"pos", "inventory", "bookings", "hr", "invoicing"},
			Terms: map[string]string{"bookings": "Reservations", "booking": "Reservation", "customers": "Guests", "customer": "Guest",
				"inventory": "Stock & ingredients", "inventory_item": "Ingredient", "services": "Menu", "service": "Menu item", "operations": "Reservations"},
			Roles: []RoleTemplate{
				owner(""),
				manager("restaurant_manager", "Restaurant manager", "Runs service: reservations, rosters, stock and takings."),
				{Key: "server", Name: "Server", Description: "Takes orders and payments and manages reservations.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/pos",
					Perms: perms(PermCore, view, "pos", add, "bookings", work)},
				{Key: "host", Name: "Host", Description: "Manages reservations and seating.", Base: BaseStaff, Scope: ScopeAll, Home: "/bookings",
					Perms: perms("bookings", work, "pos", view)},
				{Key: "kitchen", Name: "Chef / kitchen", Description: "Checks reservations and keeps stock up to date.", Base: BaseStaff, Scope: ScopeAll, Home: "/inventory",
					Perms: perms("inventory", upd, "bookings", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "menu", Label: "Load your menu into the POS", To: "/inventory"},
				{Key: "team", Label: "Invite your staff", To: "/staff"},
				{Key: "pos", Label: "Ring up your first order", To: "/pos"},
			},
			BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "Table", "default_duration": 90},
			DashboardWidgets: []string{"sales_today", "covers", "reservations_today", "low_stock"},
		},
		nightclub(),
		{
			Key: "events", Name: "Events & venues", Icon: "fa-solid fa-champagne-glasses", PlanIndustry: "events",
			Description: "Venues, event organisers and ticketed experiences.",
			Modules:     []string{"events", "bookings", "invoicing", "crm", "communication"},
			Terms:       map[string]string{"bookings": "Venue bookings", "booking": "Venue booking", "customers": "Clients", "customer": "Client"},
			Roles: []RoleTemplate{
				owner(""),
				manager("event_manager", "Event manager", "Plans events, venue bookings and ticketing."),
				{Key: "coordinator", Name: "Event coordinator", Description: "Runs events and venue bookings day to day.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "events", work, "bookings", work, "crm", work, "communication", work, "invoicing", view)},
				{Key: "box_office", Name: "Box office / check-in", Description: "Sells tickets and checks guests in.", Base: BaseStaff, Scope: ScopeAll, Home: "/events",
					Perms: perms("events", upd, "crm", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "event", Label: "Create your first event", To: "/events/create"},
				{Key: "team", Label: "Invite your crew", To: "/staff"},
			},
			DashboardWidgets: []string{"upcoming_events", "tickets_sold", "revenue"},
		},
		{
			Key: "construction", Name: "Construction & trades", Icon: "fa-solid fa-helmet-safety", PlanIndustry: "construction",
			Description: "Builders, electricians, plumbers and contractors.",
			Modules:     []string{"projects", "invoicing", "inventory", "hr", "documents", "crm"},
			Terms:       map[string]string{"projects": "Jobs", "project": "Job", "customers": "Clients", "customer": "Client", "inventory": "Materials", "inventory_item": "Material", "staff": "Crew"},
			Roles: []RoleTemplate{
				owner(""),
				manager("site_manager", "Site manager", "Runs jobs, crews, materials and progress claims."),
				{Key: "estimator", Name: "Estimator", Description: "Quotes jobs and keeps client records.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "invoicing", work, "crm", work, "projects", view, "inventory", view)},
				{Key: "tradesperson", Name: "Tradesperson", Description: "Works on assigned jobs and checks materials.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/projects",
					Perms: perms("projects", upd, "documents", view, "inventory", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "clients", Label: "Add your clients", To: "/customers"},
				{Key: "quote", Label: "Send your first quote", To: "/quotes/new"},
				{Key: "projects", Label: "Set up your first job", To: "/projects"},
			},
			DashboardWidgets: []string{"active_jobs", "quotes_pending", "revenue", "unpaid_invoices"},
		},
		{
			Key: "manufacturer", Name: "Manufacturer", Icon: "fa-solid fa-industry", PlanIndustry: "manufacturing",
			Description: "Makers, factories and assembly operations.",
			Modules:     []string{"manufacturing", "inventory", "accounting", "hr", "invoicing", "crm"},
			Terms:       map[string]string{"inventory": "Stock & materials", "inventory_item": "Item", "staff": "Workforce"},
			Roles: []RoleTemplate{
				owner(""),
				manager("production_manager", "Production manager", "Plans production, work orders and materials."),
				{Key: "operator", Name: "Machine operator", Description: "Works on production orders.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/production",
					Perms: perms("manufacturing", upd, "inventory", view)},
				{Key: "warehouse", Name: "Warehouse", Description: "Receives and issues stock.", Base: BaseStaff, Scope: ScopeAll, Home: "/inventory",
					Perms: perms(PermCore, view, "inventory", crud, "manufacturing", view)},
				{Key: "sales", Name: "Sales", Description: "Quotes and invoices customers.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "crm", work, "invoicing", work, "inventory", view)},
			},
			Checklist: []ChecklistItem{
				{Key: "materials", Label: "Load your materials and products", To: "/inventory"},
				{Key: "production", Label: "Create a work order", To: "/production"},
			},
			DashboardWidgets: []string{"work_orders", "output", "low_stock", "revenue"},
		},
		{
			Key: "education", Name: "Education / training", Icon: "fa-solid fa-graduation-cap", PlanIndustry: "education",
			Description: "Schools, tutors, RTOs and course providers.",
			Modules:     []string{"bookings", "events", "invoicing", "communication", "documents", "crm"},
			Terms: map[string]string{"customers": "Students", "customer": "Student", "bookings": "Sessions", "booking": "Session",
				"services": "Courses", "service": "Course", "staff": "Trainers", "staff_member": "Trainer", "events": "Classes & events", "operations": "Sessions & courses"},
			Roles: []RoleTemplate{
				owner("Principal / Owner"),
				manager("coordinator", "Course coordinator", "Schedules courses, trainers and enrolments."),
				{Key: "trainer", Name: "Trainer / tutor", Description: "Runs their own sessions and keeps student notes.", Base: BaseStaff, Scope: ScopeOwn, Home: "/bookings",
					Perms: perms("bookings", upd, "events", view, "crm", view, "documents", add)},
				{Key: "admin_officer", Name: "Admin officer", Description: "Enrols students and invoices fees.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(PermCore, view, "bookings", work, "events", work, "crm", work, "invoicing", work, "documents", work, "communication", work)},
			},
			Checklist: []ChecklistItem{
				{Key: "services", Label: "List your courses", To: "/services"},
				{Key: "team", Label: "Invite your trainers", To: "/staff"},
				{Key: "students", Label: "Add your students", To: "/customers"},
			},
			SampleServices: []SampleService{
				{Name: "One-to-one tutoring", Category: "Tutoring", Duration: 60, Example: true},
				{Name: "Group class", Category: "Classes", Duration: 90, Example: true},
				{Name: "Assessment session", Category: "Assessments", Duration: 120, Example: true},
			},
			BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "Classroom", "default_duration": 60},
			DashboardWidgets: []string{"sessions_today", "enrolments", "revenue"},
		},
		{
			Key: "nonprofit", Name: "Non-profit / CBO", Icon: "fa-solid fa-hand-holding-heart", PlanIndustry: "nonprofit",
			Description: "Charities, community-based organisations, associations and clubs.",
			Modules:     []string{"crm", "events", "accounting", "documents", "communication", "invoicing"},
			Terms:       map[string]string{"customers": "Members & donors", "customer": "Member", "staff": "Staff & volunteers", "staff_member": "Volunteer", "events": "Programmes & events"},
			Roles: []RoleTemplate{
				owner("Chair / Director"),
				manager("programme_manager", "Programme manager", "Runs programmes, events and volunteers."),
				{Key: "treasurer", Name: "Treasurer", Description: "Keeps the books and reports to the board.", Base: BaseStaff, Scope: ScopeAll,
					Perms: perms(PermCore, view, "accounting", crud, "invoicing", work, "crm", view, "documents", view)},
				{Key: "volunteer", Name: "Volunteer", Description: "Helps at events and keeps member records up to date.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/events",
					Perms: perms("events", upd, "crm", upd, "communication", work)},
			},
			Checklist: []ChecklistItem{
				{Key: "members", Label: "Import members and donors", To: "/customers"},
				{Key: "event", Label: "Plan your next programme or event", To: "/events/create"},
			},
			DashboardWidgets: []string{"members", "upcoming_events", "donations", "cash_position"},
		},
		{
			Key: "general", Name: "General business", Icon: "fa-solid fa-building", PlanIndustry: "general",
			Description: "Anything else — every module in your plan stays available.",
			Modules:     []string{AllModules},
			Roles: []RoleTemplate{
				owner(""),
				manager("manager", "Manager", "Runs day-to-day operations across every module."),
				{Key: "staff", Name: "Staff member", Description: "Works across modules; cannot delete records or approve.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff",
					Perms: perms(AllModules, work)},
				{Key: "viewer", Name: "Read-only", Description: "Can look but not change anything.", Base: BaseReadOnly, Scope: ScopeAll,
					Perms: perms(AllModules, view)},
			},
			Checklist: []ChecklistItem{
				{Key: "type", Label: "Choose your business type", To: "/settings#business-type"},
				{Key: "team", Label: "Invite your team", To: "/staff"},
				{Key: "invoice", Label: "Send your first invoice", To: "/invoices/new"},
			},
			DashboardWidgets: []string{"revenue", "unpaid_invoices", "expenses", "cash_position"},
		},
	}
	_ = nopes
	for i := range t {
		for j := range t[i].Roles {
			if s := roleSteps[t[i].Key][t[i].Roles[j].Key]; len(s) > 0 {
				t[i].Roles[j].Steps = append([]Step{}, s...)
			}
		}
		t[i].Active = true
		t[i].SortOrder = i + 1
		if t[i].HiddenPaths == nil {
			t[i].HiddenPaths = []string{}
		}
		if t[i].SampleServices == nil {
			t[i].SampleServices = []SampleService{}
		}
		if t[i].BookingDefaults == nil {
			t[i].BookingDefaults = map[string]any{"vehicle_required": false}
		}
		if t[i].Features == nil {
			t[i].Features = []string{}
		}
	}
	return t
}

// nightclub is the "Nightclub / bar / lounge" type: drinks sold at the till
// (POS) are deducted from bar stock by the tot or the bottle (pkg/bar, the
// /bar-stock pages), stock is held in a main store and one or more bars, and
// guests book tables. Bar stock permissions are the inventory module's
// actions: view = stock at their bars without money values, create = counts,
// wastage and transfer requests, edit = issue/receive transfers and
// deliveries, approve = approve counts and wastage, see costs and variance
// money, settings and purchase orders, delete = remove items and recipes.
func nightclub() BusinessType {
	return BusinessType{
		Key: "nightclub", Name: "Nightclub / bar / lounge", Icon: "fa-solid fa-martini-glass-citrus", PlanIndustry: "hospitality",
		Description: "Nightclubs, bars, lounges and pubs — tots, bottles, stock takes and tables.",
		Modules:     []string{"pos", "inventory", "events", "bookings", "hr", "crm"},
		// The deals pipeline is not for a club (guests stay under Guests), and
		// the generic product list gives way to the Bar stock area
		// (/bar-stock), which reads the same products as stock items.
		HiddenPaths: []string{"/crm", "/inventory"},
		Terms: map[string]string{"customers": "Guests", "customer": "Guest", "bookings": "Table reservations", "booking": "Reservation",
			"inventory": "Bar stock", "inventory_item": "Stock item", "services": "Drinks menu", "service": "Menu item",
			"operations": "Tables & events"},
		Roles: []RoleTemplate{
			owner("Owner"),
			{Key: "bar_manager", Name: "Bar manager", Description: "Runs the bar: stock, stock takes and variance, purchasing, the till, reservations and events.", Base: BaseManager, Scope: ScopeAll, Legacy: "manager",
				Perms: perms(PermCore, view, PermTeam, view, "inventory", full, "pos", full, "bookings", crud, "events", crud, "crm", work, "hr", view)},
			{Key: "store_keeper", Name: "Store keeper", Description: "Receives deliveries, issues stock to the bars and counts stock. Cannot approve counts or see costs.", Base: BaseStaff, Scope: ScopeAll, Home: "/bar-stock",
				Perms: perms("inventory", work)},
			{Key: "bartender", Name: "Bartender", Description: "Sells drinks at the till, records wastage and requests stock for their bar.", Base: BaseStaff, Scope: ScopeAll, Legacy: "staff", Home: "/pos",
				Perms: perms("pos", add, "inventory", add)},
			{Key: "cashier", Name: "Cashier / waiter", Description: "Takes orders and payments at the till and checks table reservations.", Base: BaseStaff, Scope: ScopeAll, Home: "/pos",
				Perms: perms("pos", add, "bookings", view)},
			{Key: "host", Name: "Host / door", Description: "Takes table reservations, keeps the guest list and checks guests in at events.", Base: BaseStaff, Scope: ScopeAll, Home: "/bookings",
				Perms: perms("bookings", work, "events", upd, "crm", add)},
		},
		Checklist: []ChecklistItem{
			{Key: "bars", Label: "Set up your bars and store", Hint: "Where stock is kept and where it is sold from.", To: "/bar-stock?tab=setup"},
			{Key: "stock_items", Label: "Add your stock items", Hint: "Bottles, kegs, cans and mixers — start from the starter list.", To: "/bar-stock?tab=stock"},
			{Key: "menu", Label: "Create tots and menu items", Hint: "What guests buy, and how much stock each one uses.", To: "/bar-stock?tab=recipes"},
			{Key: "delivery", Label: "Receive your first delivery", To: "/bar-stock?tab=purchasing"},
			{Key: "count", Label: "Do your first stock take", To: "/bar-stock?tab=counts"},
			{Key: "pos", Label: "Ring up your first sale", To: "/pos"},
		},
		BookingDefaults:  map[string]any{"vehicle_required": false, "resource_label": "Table", "default_duration": 240},
		DashboardWidgets: []string{"bar_tonight", "sales_today", "reservations_today", "low_stock"},
		Features:         []string{"bar_stock"},
	}
}

// typeAliases maps legacy organisations.business_type values to type keys.
var typeAliases = map[string]string{
	"mechanic": "mechanic", "garage": "mechanic", "automotive": "mechanic", "auto": "mechanic", "workshop": "mechanic",
	"salon": "salon", "beauty": "salon", "barber": "salon", "hair": "salon", "nails": "salon",
	"spa": "spa", "wellness": "spa", "massage": "spa", "gym": "spa",
	"clinic": "clinic", "healthcare": "clinic", "medical": "clinic", "health": "clinic", "dental": "clinic",
	"ndis": "ndis", "care": "ndis", "aged_care": "ndis", "disability": "ndis",
	"consulting": "consulting", "professional": "consulting", "accounting": "consulting", "legal": "consulting",
	"agency": "agency", "it": "agency", "web": "agency", "software": "agency",
	"retail": "retail", "shop": "retail", "store": "retail", "ecommerce": "retail",
	"restaurant": "restaurant", "cafe": "restaurant", "hospitality": "restaurant", "food": "restaurant",
	"nightclub": "nightclub", "night_club": "nightclub", "night-club": "nightclub", "bar": "nightclub", "lounge": "nightclub", "pub": "nightclub", "nightlife": "nightclub",
	"events": "events", "venue": "events",
	"construction": "construction", "trades": "construction", "builder": "construction",
	"manufacturer": "manufacturer", "manufacturing": "manufacturer", "factory": "manufacturer",
	"education": "education", "training": "education", "school": "education",
	"nonprofit": "nonprofit", "non-profit": "nonprofit", "charity": "nonprofit", "cbo": "nonprofit", "ngo": "nonprofit",
	"general": "general", "": "general", "transportation": "general",
}

// TypeKeyFor maps any business_type string to a registry key ("general" when unknown).
func TypeKeyFor(bt string) string {
	if k, ok := typeAliases[normKey(bt)]; ok {
		return k
	}
	return normKey(bt)
}

// ---------------------------------------------------------------- getting started

// Step is one "Getting started" step shown to a person with a role
// (pkg/onboarding): what to do, in their own words, and where to do it.
//
// Title and Description may use the organisation's words as placeholders:
// {customers} → "guests", {Customers} → "Guests", {a_booking} → "a
// reservation" (lower-case keys give running-text words, a capital keeps the
// term's capitals, "a_" adds "a"/"an"). Keys are those of DefaultTerms.
type Step struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"` // plain words, at most 140 characters
	To          string `json:"to"`          // app route (query / hash allowed)
	// DoneWhen names a check pkg/onboarding runs to tick the step by itself
	// (e.g. "my_pos_sale"); empty = the person ticks it.
	DoneWhen string `json:"done_when,omitempty"`
}

func st(key, title, desc, to, doneWhen string) Step {
	return Step{Key: key, Title: title, Description: desc, To: to, DoneWhen: doneWhen}
}

// Steps shared by several roles.
var (
	stLogo       = st("logo", "Upload your logo", "Your logo goes on invoices, receipts, emails and the menu bar. A wide logo on a clear background looks best.", "/settings#business", "logo")
	stDetails    = st("details", "Add your business details", "Your name, phone and address are printed on invoices, receipts and emails. Check them in Settings.", "/settings#business", "business_details")
	stMyPhone    = st("phone", "Add your phone number", "Put your phone number on your profile so the team can reach you.", "/profile", "my_phone")
	stDashboard  = st("dashboard", "Check today's numbers", "The dashboard shows today's sales, money owed and what needs your attention.", "/dashboard", "")
	stTeamReview = st("team", "Meet your team", "See who works here and the role each person has.", "/staff", "")
	stOpenDrawer = st("drawer", "Open your cash drawer", "Count your float and open the drawer at the start of your shift, so takings add up at the end.", "/pos", "my_drawer")
)

// roleSteps are the built-in steps per business type and role key (3–6
// each), attached to the role templates by DefaultTypes. Roles without an
// entry (custom roles, new types) fall back to their type's owner or manager
// steps or to BaseSteps (see StepsFor).
var roleSteps = map[string]map[string][]Step{
	"mechanic": {
		"owner": {
			st("services", "Set your labour rates", "Add the jobs you do and what you charge, so job cards and invoices fill in by themselves.", "/services", "service"),
			st("parts", "Load your parts list", "Add the parts you keep with their prices. Technicians can look them up on a job.", "/inventory", "product"),
			st("team", "Add your technicians", "Give each person a role. Technicians only see the job cards assigned to them.", "/staff", "staff"),
			st("jobcard", "Open your first job card", "Book a vehicle in, pick the work to be done and assign it to a technician.", "/bookings", "booking"),
			st("invoice", "Invoice a finished job", "Turn the work into an invoice and send it to the customer.", "/invoices/new", "invoice"),
			stLogo,
		},
		"workshop_manager": {
			st("today", "Plan today's job cards", "See every vehicle booked in today and who is working on it.", "/bookings", ""),
			st("assign", "Assign a job to a technician", "Open a job card and pick the technician. It shows on their list straight away.", "/bookings", "booking"),
			st("parts", "Check parts running low", "Parts below their reorder point show first. Order more before a job stalls.", "/inventory", ""),
			st("invoice", "Invoice a finished job", "When a job is done, send the invoice so the customer can pay on collection.", "/invoices/new", "invoice"),
			stDashboard,
		},
		"service_advisor": {
			st("customer", "Add a customer and their vehicle", "Save the owner's details, then add the car with its plate so you find it fast next time.", "/customers", "customer"),
			st("jobcard", "Write up a job card", "Book the vehicle in, note the problem and pick the work to be done.", "/bookings", "booking"),
			st("quote", "Send a quote", "Price the work before you start and send it for the customer to accept.", "/quotes/new", "quote"),
			st("invoice", "Invoice the customer", "When the job is done, create the invoice and send it or print it at the counter.", "/invoices/new", "my_invoice"),
		},
		"technician": {
			st("list", "See your job cards", "Only the vehicles assigned to you show here, with what needs doing.", "/bookings", ""),
			st("start", "Start a job", "Open a job card and tap In progress when you begin, so the office knows.", "/bookings", "my_booking_progress"),
			st("parts", "Look up a part", "Search the parts list to check what's in stock before you start.", "/inventory", ""),
			st("finish", "Mark a job as done", "Tap Completed when the work is finished so the office can invoice it.", "/bookings", "my_booking_done"),
		},
		"parts_clerk": {
			st("part", "Add a part", "Enter the part with its number, price and how many you have on the shelf.", "/inventory", "product"),
			st("supplier", "Add your suppliers", "Keep each supplier's contacts in one place so ordering parts is quick.", "/suppliers", "supplier"),
			st("order", "Order parts from a supplier", "Create a purchase order, send it and mark it received when the parts arrive.", "/suppliers", "purchase_order"),
			st("low", "Check what's running low", "Parts at or below their reorder point are flagged so nothing runs out.", "/inventory", ""),
		},
		"cashier": {
			stOpenDrawer,
			st("sale", "Take a payment at the counter", "Add parts or labour on the till, take cash, card or mobile money and hand over the receipt.", "/pos", "my_pos_sale"),
			st("invoice", "Invoice a customer", "Create an invoice for the work done and send it, or print it at the counter.", "/invoices/new", "my_invoice"),
			st("jobs", "See today's job cards", "Check which vehicles are ready for collection and payment.", "/bookings", ""),
		},
	},
	"salon": {
		"owner": {
			st("treatments", "Set up your treatment menu", "Add each treatment with how long it takes and its price, so booking is quick.", "/services", "service"),
			st("team", "Add your stylists", "Give each person a role. Stylists only see their own appointments.", "/staff", "staff"),
			st("clients", "Add your clients", "Save their phone and email so you can send reminders and rebook them.", "/customers", "customer"),
			st("booking", "Take your first appointment", "Pick a client, a treatment, a stylist and a time.", "/bookings", "booking"),
			st("sale", "Ring up a sale at the till", "Charge for treatments and retail products and hand over a receipt.", "/pos", "pos_sale"),
			stLogo,
		},
		"salon_manager": {
			st("day", "Plan the day", "See today's appointments for every stylist and fill the gaps.", "/bookings", ""),
			st("booking", "Book an appointment", "Pick the client, the treatment, the stylist and a free time.", "/bookings", "booking"),
			st("sale", "Take a payment", "Ring up the treatment and any products at the till.", "/pos", "my_pos_sale"),
			st("stock", "Check retail stock", "See which products are running low and need ordering.", "/inventory", ""),
			st("numbers", "Look at the takings", "The dashboard shows today's sales and which treatments sell best.", "/dashboard", ""),
		},
		"receptionist": {
			st("client", "Add a new client", "Save their name, phone and email when they first call or walk in.", "/customers", "customer"),
			st("booking", "Book an appointment", "Pick the client, the treatment, the stylist and a time that's free.", "/bookings", "booking"),
			st("checkin", "Check a client in", "When they arrive, open the appointment and mark it in progress.", "/bookings", ""),
			st("sale", "Take payment", "Ring up the treatment and any products, then hand over the receipt.", "/pos", "my_pos_sale"),
		},
		"stylist": {
			st("list", "See your appointments", "Only your own clients and times show here.", "/bookings", ""),
			st("client", "Look up a client", "Check their notes and past visits before you start.", "/customers", ""),
			st("start", "Start an appointment", "Mark it in progress when the client sits down, so reception knows.", "/bookings", "my_booking_progress"),
			st("sale", "Ring up a sale", "Charge for the treatment and any products you sold.", "/pos", "my_pos_sale"),
		},
		"beautician": {
			st("list", "See your treatments", "Only your own clients and times show here.", "/bookings", ""),
			st("client", "Check client notes", "Look up allergies and past treatments before you begin.", "/customers", ""),
			st("start", "Start a treatment", "Mark the appointment in progress when you begin, so reception knows.", "/bookings", "my_booking_progress"),
			st("sale", "Sell a product", "Ring up the products a client takes home.", "/pos", "my_pos_sale"),
		},
	},
	"spa": {
		"owner": {
			st("treatments", "Set up your treatment menu", "Add each treatment with its length and price, so booking is quick.", "/services", "service"),
			st("team", "Add your therapists", "Give each person a role. Therapists only see their own treatments.", "/staff", "staff"),
			st("guests", "Add your guests", "Save their phone and email so you can send reminders and rebook them.", "/customers", "customer"),
			st("booking", "Take your first booking", "Pick a guest, a treatment, a therapist and a time.", "/bookings", "booking"),
			st("sale", "Take your first payment", "Charge for treatments and retail products at the till.", "/pos", "pos_sale"),
		},
		"spa_manager": {
			st("day", "Plan the day", "See every room's and therapist's treatments for today.", "/bookings", ""),
			st("booking", "Book a treatment", "Pick the guest, the treatment, a therapist and a free time.", "/bookings", "booking"),
			st("stock", "Check retail products", "Spot what's running low before it runs out.", "/inventory", ""),
			st("numbers", "Look at the takings", "The dashboard shows today's sales and the busiest treatments.", "/dashboard", ""),
		},
		"receptionist": {
			st("guest", "Add a new guest", "Save their name, phone and email when they first call or walk in.", "/customers", "customer"),
			st("booking", "Book a treatment", "Pick the guest, the treatment, the therapist and a time that's free.", "/bookings", "booking"),
			st("checkin", "Check a guest in", "When they arrive, open the booking and mark it in progress.", "/bookings", ""),
			st("sale", "Take payment", "Ring up the treatment and any products, then hand over the receipt.", "/pos", "my_pos_sale"),
		},
		"therapist": {
			st("list", "See your treatments", "Only your own guests and times show here.", "/bookings", ""),
			st("guest", "Read guest notes", "Check allergies and preferences before the treatment.", "/customers", ""),
			st("start", "Start a treatment", "Mark it in progress when the guest is in the room.", "/bookings", "my_booking_progress"),
			st("finish", "Finish a treatment", "Mark it completed when you're done so reception can take payment.", "/bookings", "my_booking_done"),
		},
	},
	"clinic": {
		"owner": {
			st("consults", "List your consultation types", "Add each type of appointment with its length and fee.", "/services", "service"),
			st("team", "Add your practitioners and reception", "Give each person a role. Practitioners only see their own appointments.", "/staff", "staff"),
			st("patients", "Add your patients", "Register patients with their contacts so booking and billing are quick.", "/customers", "customer"),
			st("booking", "Book the first appointment", "Pick a patient, a consultation, a practitioner and a time.", "/bookings", "booking"),
			st("invoice", "Bill a patient", "Create an invoice after a consultation and send it.", "/invoices/new", "invoice"),
			stLogo,
		},
		"practice_manager": {
			st("day", "Check today's appointments", "See every practitioner's list for today and spot the gaps.", "/bookings", ""),
			stTeamReview,
			st("unpaid", "Follow up unpaid invoices", "See which patients still owe and send a reminder.", "/invoices", ""),
			st("numbers", "Look at the practice numbers", "The dashboard shows appointments, no-shows and money owed.", "/dashboard", ""),
		},
		"practitioner": {
			st("list", "See your appointments", "Your patients for today, in time order.", "/bookings", ""),
			st("patient", "Open a patient's record", "Read their details and notes before they come in.", "/customers", ""),
			st("start", "Start a consultation", "Mark the appointment in progress when the patient comes in.", "/bookings", "my_booking_progress"),
			st("docs", "Add a document", "Attach a referral, result or letter so the team can find it later.", "/documents", ""),
		},
		"nurse": {
			st("day", "Check today's appointments", "See who's coming in and what each appointment is for.", "/bookings", ""),
			st("patient", "Update a patient's notes", "Keep allergies, contacts and notes up to date.", "/customers", ""),
			st("docs", "Upload a document", "Add results or forms to the right patient so nothing gets lost.", "/documents", ""),
			st("messages", "Message the team", "Send a quick note to a practitioner or to reception.", "/messages", ""),
		},
		"receptionist": {
			st("patient", "Register a new patient", "Save their name, phone, email and date of birth.", "/customers", "customer"),
			st("booking", "Book an appointment", "Pick the patient, the consultation, a practitioner and a free time.", "/bookings", "booking"),
			st("checkin", "Check a patient in", "When they arrive, open the appointment and mark it in progress.", "/bookings", ""),
			st("invoice", "Take payment", "Create the invoice for the visit and record how they paid.", "/invoices/new", "my_invoice"),
		},
	},
	"ndis": {
		"owner": {
			st("supports", "Add your support items and rates", "List the supports you provide and what you charge for each.", "/services", "service"),
			st("team", "Add your support workers", "Give each person a role. Workers only see the shifts assigned to them.", "/staff", "staff"),
			st("participants", "Add your participants", "Save their details, plan manager and emergency contacts.", "/customers", "customer"),
			st("shift", "Roster the first shift", "Pick a participant, a support, a worker and a time.", "/bookings", "booking"),
			st("invoice", "Invoice a plan manager", "Bill completed supports to the participant's plan manager.", "/invoices/new", "invoice"),
		},
		"coordinator": {
			st("week", "Plan this week's shifts", "See every participant's supports and who is rostered on.", "/bookings", ""),
			st("shift", "Roster a shift", "Pick the participant, the support, a worker and a time.", "/bookings", "booking"),
			st("participant", "Add a participant", "Save their details and plan manager so shifts and billing are ready.", "/customers", "customer"),
			stTeamReview,
		},
		"support_worker": {
			st("shifts", "See your shifts", "Only the shifts assigned to you show here.", "/bookings", ""),
			st("start", "Start a shift", "Mark the shift in progress when you arrive.", "/bookings", "my_booking_progress"),
			st("finish", "Finish a shift", "Mark it completed when you leave, so it can be billed.", "/bookings", "my_booking_done"),
			st("notes", "Upload a shift note", "Add a note or document about the support you gave.", "/documents", ""),
		},
		"admin_officer": {
			st("participant", "Add a participant", "Save their details and plan manager so invoices go to the right place.", "/customers", "customer"),
			st("invoice", "Invoice completed supports", "Create the invoice for the plan manager from the supports delivered.", "/invoices/new", "my_invoice"),
			st("unpaid", "Chase unpaid invoices", "See which invoices are overdue and send a reminder.", "/invoices", ""),
			st("docs", "File a document", "Keep service agreements and plans with the right participant.", "/documents", ""),
		},
	},
	"consulting": {
		"owner": {
			stDetails,
			st("clients", "Add your clients", "Add or import the people and companies you work for.", "/customers", "customer"),
			st("engagement", "Set up your first engagement", "Create a board for the work, with tasks and due dates.", "/projects", "project"),
			st("invoice", "Send your first invoice", "Bill a client for your time or a fixed fee and see when it's paid.", "/invoices/new", "invoice"),
			st("team", "Invite your team", "Give each person a role so they see only what they need.", "/staff", "staff"),
		},
		"engagement_manager": {
			st("engagements", "Review your engagements", "See every engagement, what's due and what's stuck.", "/projects", ""),
			st("task", "Add a task to an engagement", "Break the work into tasks with an owner and a due date.", "/projects", "task"),
			st("quote", "Send a proposal", "Price the work as a quote the client can accept online.", "/quotes/new", "quote"),
			st("invoice", "Bill a client", "Invoice the work done this month and track payment.", "/invoices/new", "invoice"),
		},
		"consultant": {
			st("engagements", "Open your engagements", "See the engagements you work on and what's due next.", "/projects", ""),
			st("task", "Update a task", "Move your tasks along so everyone sees where the work is.", "/projects", ""),
			st("client", "Look up a client", "Find a client's contacts and history in one place.", "/customers", ""),
			st("docs", "Save a document", "Keep reports and files with the client so the team can find them.", "/documents", ""),
		},
		"bookkeeper": {
			st("invoice", "Create an invoice", "Bill a client and send the invoice by email.", "/invoices/new", "my_invoice"),
			st("payment", "Record a payment", "When a client pays, record it against their invoice.", "/invoices", "payment"),
			st("bill", "Enter a supplier bill", "Add bills you need to pay so nothing is missed.", "/bills", "bill"),
			st("pnl", "Read the profit & loss", "See what came in and went out for any month.", "/reports/profit-loss", ""),
		},
	},
	"agency": {
		"owner": {
			st("clients", "Add your clients", "Add or import the companies you work for.", "/customers", "customer"),
			st("project", "Create a project board", "Set up a board with tasks for your first client project.", "/projects", "project"),
			st("team", "Invite your team", "Give each person a role so they see only what they need.", "/staff", "staff"),
			st("invoice", "Send your first invoice", "Bill a client for a project or a monthly retainer.", "/invoices/new", "invoice"),
			stLogo,
		},
		"project_manager": {
			st("projects", "Review your projects", "See every project, what's due and what's blocked.", "/projects", ""),
			st("task", "Add a task and assign it", "Create a task, pick who does it and set a due date.", "/projects", "task"),
			st("quote", "Send a quote", "Price the next piece of work for the client to accept.", "/quotes/new", "quote"),
			st("invoice", "Bill a client", "Invoice finished work and see when it's paid.", "/invoices/new", "invoice"),
		},
		"developer": {
			st("board", "Open your project board", "See the tasks for each project and what's assigned to you.", "/projects", ""),
			st("task", "Move a task along", "Update the status as you work so the team knows where things are.", "/projects", ""),
			st("docs", "Find a project document", "Specs, logins and notes live in Documents.", "/documents", ""),
			st("messages", "Message the team", "Ask a question or share an update without leaving DASYIN.", "/messages", ""),
		},
		"designer": {
			st("board", "Open your project board", "See the tasks for each project and what's assigned to you.", "/projects", ""),
			st("upload", "Upload a design", "Save design files with the project so everyone has the latest.", "/documents", ""),
			st("task", "Update a task", "Move your tasks along so the team sees what's ready.", "/projects", ""),
			st("messages", "Share an update", "Send the team a quick message when a design is ready.", "/messages", ""),
		},
		"support": {
			st("client", "Look up a client", "Find a client's contacts and history before you reply.", "/customers", ""),
			st("issue", "Log a client's request", "Add it as a task on the right project so the team picks it up.", "/projects", "task"),
			st("messages", "Reply to a message", "Answer clients and teammates from one inbox.", "/messages", ""),
			st("docs", "Find a document", "Guides and client notes live in Documents.", "/documents", ""),
		},
	},
	"retail": {
		"owner": {
			st("products", "Load your products", "Add what you sell with prices and barcodes, or import a list.", "/inventory", "product"),
			st("sale", "Make your first sale", "Ring up a sale at the till and hand over the receipt.", "/pos", "pos_sale"),
			st("team", "Add your shop staff", "Give each person a role. Cashiers only see the till and stock.", "/staff", "staff"),
			st("supplier", "Add your suppliers", "Keep their contacts so reordering is quick.", "/suppliers", "supplier"),
			st("logo", "Upload your logo", "It's printed on every receipt and invoice.", "/settings#business", "logo"),
		},
		"store_manager": {
			st("sales", "Check today's sales", "See every sale made today and who rang it up.", "/pos-transactions", ""),
			st("low", "Reorder low stock", "Products at or below their reorder point show first.", "/inventory", ""),
			st("order", "Order from a supplier", "Create a purchase order and mark it received when it arrives.", "/suppliers", "purchase_order"),
			stTeamReview,
			stDashboard,
		},
		"cashier": {
			stOpenDrawer,
			st("sale", "Ring up a sale", "Scan or tap the items, take payment and hand over the receipt.", "/pos", "my_pos_sale"),
			st("stock", "Check if something's in stock", "Search the stock list when a customer asks.", "/inventory", ""),
			st("customer", "Add a customer", "Save a regular's details so their sales are linked to them.", "/customers", "customer"),
		},
		"stock_clerk": {
			st("product", "Add a product", "Enter the product with its price, barcode and how many you have.", "/inventory", "product"),
			st("supplier", "Add a supplier", "Keep supplier contacts so reordering is quick.", "/suppliers", "supplier"),
			st("count", "Fix a stock level after a count", "Correct the quantity when the shelf doesn't match the system.", "/inventory", ""),
			st("low", "Check low stock", "Products at or below their reorder point show first.", "/inventory", ""),
		},
	},
	"restaurant": {
		"owner": {
			st("menu", "Load your menu into the POS", "Add your dishes and drinks with prices so they show on the till.", "/inventory", "product"),
			st("team", "Add your staff", "Give each person a role: servers, hosts and the kitchen each see what they need.", "/staff", "staff"),
			st("order", "Ring up your first order", "Take an order on the till and take payment.", "/pos", "pos_sale"),
			st("reservation", "Take a reservation", "Book a table for a guest with the time and party size.", "/bookings", "booking"),
			stLogo,
		},
		"restaurant_manager": {
			st("tonight", "Check tonight's reservations", "See who's booked in and plan the floor.", "/bookings", ""),
			st("sales", "Look at today's sales", "See every order rung up today and who took it.", "/pos-transactions", ""),
			st("stock", "Check stock and ingredients", "Spot what's running low before service.", "/inventory", ""),
			st("roster", "Check the roster", "See who's working and their hours.", "/hcm", ""),
			stDashboard,
		},
		"server": {
			stOpenDrawer,
			st("order", "Ring up an order", "Tap the dishes and drinks, take payment and hand over the receipt.", "/pos", "my_pos_sale"),
			st("reservation", "Book a table", "Take a reservation with the guest's name, time and party size.", "/bookings", "booking"),
			st("tonight", "See tonight's reservations", "Check who's coming and when, before service starts.", "/bookings", ""),
		},
		"host": {
			st("tonight", "See tonight's reservations", "Check who's coming and when, before service starts.", "/bookings", ""),
			st("reservation", "Book a table", "Take a reservation with the guest's name, time and party size.", "/bookings", "booking"),
			st("seat", "Seat a guest", "When guests arrive, open their reservation and mark it in progress.", "/bookings", ""),
		},
		"kitchen": {
			st("covers", "Check tonight's covers", "See how many guests are booked so you can prep.", "/bookings", ""),
			st("stock", "Update a stock level", "Correct an ingredient's quantity after a delivery or at close.", "/inventory", ""),
			st("low", "See what's running low", "Ingredients at or below their reorder point show first.", "/inventory", ""),
		},
	},
	"nightclub": {
		"owner": {
			st("setup", "Finish the club setup", "Your bars, store, tot size and starter stock list. You can come back and change them any time.", "/setup", "setup_done"),
			st("prices", "Put prices on the drinks menu", "Set what a tot, a bottle and each cocktail costs, so the till charges the right amount.", "/bar-stock?tab=recipes", "menu_prices"),
			st("team", "Add your staff", "Add bartenders, a store keeper and cashiers. Each one sees only what their job needs.", "/staff", "staff"),
			st("delivery", "Receive your first delivery", "Enter what the supplier dropped off, so it's added to the main store.", "/bar-stock?tab=purchasing", "delivery"),
			st("count", "Do your first stock take", "Count every bar and the store, then approve the count to set your starting stock.", "/bar-stock?tab=counts", "count_approved"),
			st("variance", "Read the variance report", "See what each bar should have sold against what's missing: lost tots, comps and breakages.", "/bar-stock?tab=reports", ""),
		},
		"bar_manager": {
			st("tonight", "Check tonight's bar", "See sales, open counts and stock requests for every bar.", "/bar-stock", ""),
			st("prices", "Check prices on the drinks menu", "Make sure every tot, bottle and cocktail has the right price on the till.", "/bar-stock?tab=recipes", "menu_prices"),
			st("order", "Order from a supplier", "Create a purchase order and receive it into the main store when it arrives.", "/bar-stock?tab=purchasing", "purchase_order"),
			st("approve", "Approve a stock take", "Check a bar's count, recount anything odd and approve it.", "/bar-stock?tab=counts", "count_approved"),
			st("wastage", "Approve breakages and comps", "Bartenders log what was broken, spilt or given away. Approve or reject each one.", "/bar-stock?tab=wastage", "wastage_decided"),
			st("variance", "Read the variance report", "See which bar is short and on which drinks, after each approved count.", "/bar-stock?tab=reports", ""),
		},
		"store_keeper": {
			st("delivery", "Receive a delivery", "Enter what the supplier dropped off so it's added to the main store.", "/bar-stock?tab=purchasing", "my_delivery"),
			st("issue", "Issue stock a bar asked for", "Open the bar's request and send the bottles. The bar confirms what arrived.", "/bar-stock?tab=transfers", "my_transfer_issued"),
			st("count", "Count the main store", "Count what's on the shelves and submit it for a manager to approve.", "/bar-stock?tab=counts", "my_count"),
			st("levels", "Check what's running low", "Items below their par level show first, so you can tell the manager what to order.", "/bar-stock?tab=stock", ""),
		},
		"bartender": {
			st("bar", "Pick your bar on the till", "Choose the bar you're serving from at the top of the till. Drinks come out of that bar's stock.", "/pos", ""),
			st("sale", "Ring up a tot, a bottle and a cocktail", "Tap the drink, take payment and hand over the receipt.", "/pos", "my_pos_sale"),
			st("request", "Ask the store for stock", "Running low? Send a request and the store keeper sends the bottles over.", "/bar-stock?tab=transfers", "my_transfer_request"),
			st("count", "Count your bar at closing", "Count the bottles left at the end of the night. A manager approves it.", "/bar-stock?tab=counts", "my_count"),
			st("wastage", "Log a breakage or a comp", "Broken bottle, spill or a free drink? Log it so your bar's stock adds up.", "/bar-stock?tab=wastage", "my_wastage"),
		},
		"cashier": {
			stOpenDrawer,
			st("order", "Ring up an order", "Tap the drinks, take cash, card or mobile money and hand over the receipt.", "/pos", "my_pos_sale"),
			st("tables", "Check tonight's table reservations", "See which tables are booked and when the guests arrive.", "/bookings", ""),
		},
		"host": {
			st("reservation", "Take a table reservation", "Book a table with the guest's name, phone, time and party size.", "/bookings", "booking"),
			st("guest", "Add a guest to the list", "Save a guest's name and phone so they're on the list next time.", "/customers", "customer"),
			st("tonight", "See tonight's reservations", "Check who's booked a table tonight and when they arrive.", "/bookings", ""),
			st("checkin", "Check guests in at an event", "Open tonight's event and tick guests off as they come through the door.", "/events", "my_checkin"),
		},
	},
	"events": {
		"owner": {
			st("event", "Create your first event", "Add the date, venue and tickets, then share the link.", "/events/create", "event"),
			st("team", "Invite your crew", "Give each person a role. Box office staff only see check-in.", "/staff", "staff"),
			st("clients", "Add your clients", "Save the people and companies who book your venue.", "/customers", "customer"),
			st("invoice", "Invoice a venue booking", "Bill a client for the venue, catering or extras.", "/invoices/new", "invoice"),
			stLogo,
		},
		"event_manager": {
			st("upcoming", "Review upcoming events", "See every event, tickets sold and what still needs doing.", "/events", ""),
			st("event", "Create an event", "Add the date, venue and ticket types.", "/events/create", "event"),
			st("booking", "Book the venue", "Take a venue booking for a client with the date and times.", "/bookings", "booking"),
			stDashboard,
		},
		"coordinator": {
			st("event", "Set up an event", "Add the date, venue and ticket types.", "/events/create", "event"),
			st("booking", "Take a venue booking", "Book the venue for a client with the date and times.", "/bookings", "booking"),
			st("client", "Add a client", "Save a client's contacts so bookings and invoices are quick.", "/customers", "customer"),
			st("message", "Message a client", "Send a client an update without leaving DASYIN.", "/messages", ""),
		},
		"box_office": {
			st("events", "Open today's event", "Find tonight's event and its guest list.", "/events", ""),
			st("checkin", "Check a guest in", "Find the guest or scan their ticket and tick them off at the door.", "/events", "my_checkin"),
			st("guest", "Look up a guest", "Search by name, phone or email when a ticket can't be found.", "/customers", ""),
		},
	},
	"construction": {
		"owner": {
			st("clients", "Add your clients", "Save each client's contacts and site address.", "/customers", "customer"),
			st("quote", "Send your first quote", "Price a job and send it for the client to accept online.", "/quotes/new", "quote"),
			st("job", "Set up your first job", "Create a job board with tasks and due dates.", "/projects", "project"),
			st("materials", "Load your materials", "Add the materials you buy with their prices.", "/inventory", "product"),
			st("team", "Add your crew", "Give each person a role. Tradespeople see the jobs they're on.", "/staff", "staff"),
		},
		"site_manager": {
			st("jobs", "Review your jobs", "See every job, what's due and what's held up.", "/projects", ""),
			st("task", "Plan the next task", "Add a task to a job with who does it and when.", "/projects", "task"),
			st("materials", "Check materials", "See what's in the yard before you order more.", "/inventory", ""),
			st("claim", "Send a progress claim", "Invoice the client for the work done so far.", "/invoices/new", "invoice"),
		},
		"estimator": {
			st("client", "Add a client", "Save their contacts and site address.", "/customers", "customer"),
			st("quote", "Write a quote", "Price the labour and materials and send it to the client.", "/quotes/new", "quote"),
			st("materials", "Price materials", "Look up material prices to build an accurate quote.", "/inventory", ""),
			st("jobs", "See jobs in progress", "Check how won jobs are going.", "/projects", ""),
		},
		"tradesperson": {
			st("jobs", "See your jobs", "Find the jobs you're working on and what's next.", "/projects", ""),
			st("task", "Update a task", "Mark tasks done as you go so the office knows.", "/projects", ""),
			st("plans", "Open the job's documents", "Plans, permits and notes for the job live in Documents.", "/documents", ""),
			st("materials", "Check materials", "See what's in stock before you head to the supplier.", "/inventory", ""),
		},
	},
	"manufacturer": {
		"owner": {
			st("materials", "Load your materials and products", "Add the materials you buy and the products you make.", "/inventory", "product"),
			st("order", "Create a work order", "Plan a production run for a product and quantity.", "/production", "work_order"),
			st("supplier", "Add your suppliers", "Keep supplier contacts so ordering materials is quick.", "/suppliers", "supplier"),
			st("team", "Add your workforce", "Give each person a role. Operators only see production orders.", "/staff", "staff"),
			st("invoice", "Send your first invoice", "Bill a customer for an order and see when it's paid.", "/invoices/new", "invoice"),
		},
		"production_manager": {
			st("plan", "Plan production", "See every work order and what's due this week.", "/production", ""),
			st("order", "Create a work order", "Plan a production run for a product and quantity.", "/production", "work_order"),
			st("materials", "Check material levels", "Make sure you have enough before a run starts.", "/inventory", ""),
			st("po", "Order materials", "Create a purchase order for what's running low.", "/suppliers", "purchase_order"),
		},
		"operator": {
			st("orders", "See today's work orders", "Find the production runs you're working on.", "/production", ""),
			st("start", "Start a work order", "Mark it started when the run begins.", "/production", "work_order_started"),
			st("finish", "Finish a work order", "Mark it done when the run is complete.", "/production", "work_order_done"),
			st("materials", "Look up materials", "Check what's in stock for the next run.", "/inventory", ""),
		},
		"warehouse": {
			st("item", "Add an item", "Enter a material or product with its unit and quantity.", "/inventory", "product"),
			st("receive", "Receive a supplier order", "Mark a purchase order received when the goods arrive.", "/suppliers", ""),
			st("count", "Fix a stock level after a count", "Correct the quantity when the shelf doesn't match the system.", "/inventory", ""),
			st("production", "Check what production needs", "See upcoming work orders so materials are ready.", "/production", ""),
		},
		"sales": {
			st("customer", "Add a customer", "Save their contacts and delivery address.", "/customers", "customer"),
			st("quote", "Send a quote", "Price an order and send it for the customer to accept.", "/quotes/new", "quote"),
			st("invoice", "Invoice an order", "Bill the customer when the order ships.", "/invoices/new", "my_invoice"),
			st("stock", "Check what's in stock", "See what's ready to sell before you promise a date.", "/inventory", ""),
		},
	},
	"education": {
		"owner": {
			st("courses", "List your courses", "Add each course or class with its length and fee.", "/services", "service"),
			st("team", "Add your trainers", "Give each person a role. Trainers only see their own sessions.", "/staff", "staff"),
			st("students", "Add your students", "Add or import students with their contacts.", "/customers", "customer"),
			st("session", "Schedule the first session", "Pick a student, a course, a trainer and a time.", "/bookings", "booking"),
			st("invoice", "Invoice your fees", "Send a student or parent the invoice for their course.", "/invoices/new", "invoice"),
		},
		"coordinator": {
			st("week", "Check this week's sessions", "See every trainer's sessions and spot the gaps.", "/bookings", ""),
			st("session", "Schedule a session", "Pick the student, the course, a trainer and a time.", "/bookings", "booking"),
			st("class", "Set up a class or event", "Add a group class or open day with a date and places.", "/events/create", "event"),
			st("student", "Enrol a student", "Add a student with their contacts.", "/customers", "customer"),
		},
		"trainer": {
			st("sessions", "See your sessions", "Only your own students and times show here.", "/bookings", ""),
			st("start", "Start a session", "Mark the session in progress when it begins.", "/bookings", "my_booking_progress"),
			st("student", "Look up a student", "Check a student's details and notes before class.", "/customers", ""),
			st("notes", "Upload class notes", "Share handouts and notes so students and staff can find them.", "/documents", ""),
		},
		"admin_officer": {
			st("student", "Enrol a student", "Add a student with their contacts.", "/customers", "customer"),
			st("session", "Book a session", "Pick the student, the course, a trainer and a time.", "/bookings", "booking"),
			st("invoice", "Invoice fees", "Send the invoice for a course and track payment.", "/invoices/new", "my_invoice"),
			st("message", "Send a reminder", "Message a student or trainer about an upcoming session.", "/messages", ""),
		},
	},
	"nonprofit": {
		"owner": {
			stDetails,
			st("members", "Import members and donors", "Add or import your members and donors with their contacts.", "/customers", "customer"),
			st("event", "Plan your next programme or event", "Add the date, place and how people sign up.", "/events/create", "event"),
			st("team", "Invite your staff and volunteers", "Give each person a role so they see only what they need.", "/staff", "staff"),
			stLogo,
		},
		"programme_manager": {
			st("programmes", "Review upcoming programmes", "See every programme and event and who's signed up.", "/events", ""),
			st("event", "Plan a programme or event", "Add the date, place and how people sign up.", "/events/create", "event"),
			st("member", "Add a member", "Save a member's or donor's contacts.", "/customers", "customer"),
			st("volunteers", "Check your volunteers", "See your staff and volunteers and their roles.", "/staff", ""),
		},
		"treasurer": {
			st("bill", "Enter a bill", "Add bills the organisation has to pay so nothing is missed.", "/bills", "bill"),
			st("invoice", "Send an invoice", "Bill a member, partner or sponsor and track payment.", "/invoices/new", "my_invoice"),
			st("bank", "Check the bank account", "See money in and out and match it to your records.", "/banking", ""),
			st("pnl", "Read the profit & loss", "See income and spending for any period, ready for the board.", "/reports/profit-loss", ""),
		},
		"volunteer": {
			st("events", "See upcoming events", "Find the events you're helping at.", "/events", ""),
			st("checkin", "Check people in at an event", "Tick people off as they arrive.", "/events", "my_checkin"),
			st("member", "Update a member's details", "Fix a phone number or email when someone tells you it changed.", "/customers", ""),
			st("message", "Message the team", "Send a quick note to the organisers.", "/messages", ""),
		},
	},
	"general": {
		"owner": {
			st("type", "Choose your business type", "Tell DASYIN what kind of business you run, and it shows the right tools and words.", "/settings#business-type", "type_chosen"),
			stDetails,
			stLogo,
			st("customer", "Add your first {customer}", "Keep their phone and email in one place so you find them fast next time.", "/customers", "customer"),
			st("invoice", "Send your first invoice", "Pick a {customer}, add what you sold and send it. You'll see when it's viewed and paid.", "/invoices/new", "invoice"),
			st("team", "Invite your team", "Add your {staff} and give each person a role. They only see what their role needs.", "/staff", "staff"),
		},
		"manager": baseManagerSteps,
		"staff":   baseStaffSteps,
		"viewer":  baseReadOnlySteps,
	},
}

// Per-base defaults for roles without steps of their own.
var (
	baseAdminSteps = []Step{
		stDetails,
		stLogo,
		st("customer", "Add your first {customer}", "Keep their phone and email in one place so you find them fast next time.", "/customers", "customer"),
		st("invoice", "Send your first invoice", "Pick a {customer}, add what you sold and send it. You'll see when it's viewed and paid.", "/invoices/new", "invoice"),
		st("team", "Invite your team", "Add your {staff} and give each person a role. They only see what their role needs.", "/staff", "staff"),
	}
	baseManagerSteps = []Step{
		stDashboard,
		stTeamReview,
		st("customer", "Add {a_customer}", "Save their phone and email so the whole team can find them.", "/customers", "customer"),
		st("invoice", "Send an invoice", "Pick {a_customer}, add the work or items and send it.", "/invoices/new", "invoice"),
		stMyPhone,
	}
	baseStaffSteps = []Step{
		stMyPhone,
		st("customers", "Find {a_customer}", "Search by name, phone or email to see their details and history.", "/customers", ""),
		st("bookings", "Look at the {bookings}", "See what's coming up today and open one for the details.", "/bookings", ""),
		st("sale", "Ring up a sale", "Add items on the till, take payment and hand over the receipt.", "/pos", "my_pos_sale"),
		st("invoice", "Create an invoice", "Pick {a_customer}, add the work or items and save it or send it.", "/invoices/new", "my_invoice"),
	}
	baseReadOnlySteps = []Step{
		st("dashboard", "Look around the dashboard", "The dashboard shows how the business is doing today. You can look but not change anything.", "/dashboard", ""),
		st("customers", "Look up {a_customer}", "Search by name, phone or email to see their details.", "/customers", ""),
		st("reports", "Open the profit & loss", "See what came in and went out for any period.", "/reports/profit-loss", ""),
		stMyPhone,
	}
)

// BaseSteps are the default steps for a base level (admin, manager, staff,
// read_only) when neither the role nor its type has steps.
func BaseSteps(base string) []Step {
	var s []Step
	switch base {
	case BaseAdmin:
		s = baseAdminSteps
	case BaseManager:
		s = baseManagerSteps
	case BaseReadOnly:
		s = baseReadOnlySteps
	default:
		s = baseStaffSteps
	}
	return append([]Step{}, s...)
}

// roleTemplateSteps returns the steps of role key in t, else of the same
// role in the built-in type (registry rows seeded before steps existed, or
// edited without them).
func roleTemplateSteps(t BusinessType, key string) []Step {
	for _, r := range t.Roles {
		if r.Key == key && len(r.Steps) > 0 {
			return append([]Step{}, r.Steps...)
		}
	}
	if s := roleSteps[t.Key][key]; len(s) > 0 {
		return append([]Step{}, s...)
	}
	return nil
}

// StepsFor resolves the "Getting started" steps of a role of type t:
//
//  1. the role template's own steps (registry, then built-in);
//  2. a role without steps (custom, or a new type) borrows from its type —
//     admins get the type's owner steps, managers its first manager role's;
//  3. otherwise the base level's defaults (BaseSteps).
func StepsFor(t BusinessType, roleKey, base string) []Step {
	if roleKey != "" {
		if s := roleTemplateSteps(t, roleKey); len(s) > 0 {
			return s
		}
	}
	switch base {
	case BaseAdmin:
		if s := roleTemplateSteps(t, "owner"); len(s) > 0 {
			return s
		}
	case BaseManager:
		for _, r := range t.Roles {
			if r.Base == BaseManager {
				if s := roleTemplateSteps(t, r.Key); len(s) > 0 {
					return s
				}
			}
		}
	}
	return BaseSteps(base)
}

// DefaultType returns the built-in business type key (zero value when unknown).
func DefaultType(key string) BusinessType {
	for _, t := range DefaultTypes() {
		if t.Key == key {
			return t
		}
	}
	return BusinessType{}
}

// RoleTemplateByKey returns the role template key of t.
func RoleTemplateByKey(t BusinessType, key string) (RoleTemplate, bool) {
	for _, r := range t.Roles {
		if r.Key == key {
			return r, true
		}
	}
	return RoleTemplate{}, false
}
