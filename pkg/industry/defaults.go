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
