package plans

// Default catalogue seeded on first run. After that the database is the
// source of truth and super admins edit it from the Plans & pricing page.

// DefaultModules are the entitlement units with their monthly list price (USD).
func DefaultModules() []Module {
	return []Module{
		{Key: "core", Name: "Core workspace", Description: "Dashboard, analytics, settings, profile and help — always included.", Icon: "fa-solid fa-house", Core: true, Active: true, SortOrder: 0},
		{Key: "smallbiz", Name: "Business hub", Description: "All-in-one small business hub: quick sales, expenses and tasks.", Icon: "fa-solid fa-briefcase", MonthlyPriceUSD: 9, Active: true, SortOrder: 1},
		{Key: "invoicing", Name: "Invoicing & quotes", Description: "Invoices, quotes, recurring billing, online payment links.", Icon: "fa-solid fa-file-invoice-dollar", MonthlyPriceUSD: 15, Active: true, SortOrder: 2},
		{Key: "pos", Name: "Point of sale", Description: "Checkout, cash drawer, discounts and POS reporting.", Icon: "fa-solid fa-cash-register", MonthlyPriceUSD: 25, Active: true, SortOrder: 3},
		{Key: "ecommerce", Name: "E-commerce", Description: "Online store channels, order sync and product listings.", Icon: "fa-solid fa-store", MonthlyPriceUSD: 29, Active: true, SortOrder: 4},
		{Key: "crm", Name: "Customers & CRM", Description: "Customer records, pipeline, deals and follow-ups.", Icon: "fa-solid fa-address-book", MonthlyPriceUSD: 19, Active: true, SortOrder: 5},
		{Key: "bookings", Name: "Bookings & services", Description: "Appointments, scheduling, services and categories.", Icon: "fa-solid fa-calendar-check", MonthlyPriceUSD: 19, Active: true, SortOrder: 6},
		{Key: "events", Name: "Events & ticketing", Description: "Events, ticket types, registrations and check-in.", Icon: "fa-solid fa-ticket", MonthlyPriceUSD: 19, Active: true, SortOrder: 7},
		{Key: "inventory", Name: "Inventory & suppliers", Description: "Stock, locations, suppliers and purchase orders.", Icon: "fa-solid fa-boxes-stacked", MonthlyPriceUSD: 25, Active: true, SortOrder: 8},
		{Key: "manufacturing", Name: "Manufacturing", Description: "Bills of materials, work orders and production output.", Icon: "fa-solid fa-industry", MonthlyPriceUSD: 49, Active: true, SortOrder: 9},
		{Key: "accounting", Name: "Accounting & banking", Description: "General ledger, bills, bank accounts and reconciliation.", Icon: "fa-solid fa-scale-balanced", MonthlyPriceUSD: 29, Active: true, SortOrder: 10},
		{Key: "hr", Name: "HR & payroll", Description: "Staff, time sheets, leave and payroll runs.", Icon: "fa-solid fa-id-card", MonthlyPriceUSD: 29, Active: true, SortOrder: 11},
		{Key: "projects", Name: "Projects", Description: "Boards, issues, sprints and time tracking.", Icon: "fa-solid fa-diagram-project", MonthlyPriceUSD: 19, Active: true, SortOrder: 12},
		{Key: "documents", Name: "Documents", Description: "Secure document storage and sharing.", Icon: "fa-solid fa-folder-open", MonthlyPriceUSD: 9, Active: true, SortOrder: 13},
		{Key: "communication", Name: "Communication", Description: "Meetings, video calls, messaging, SMS and WhatsApp.", Icon: "fa-solid fa-comments", MonthlyPriceUSD: 15, Active: true, SortOrder: 14},
	}
}

// DefaultIndustries recommend a bundle per business type.
func DefaultIndustries() []Industry {
	return []Industry{
		{Key: "retail", Name: "Retail & e-commerce", Icon: "fa-solid fa-bag-shopping", Description: "Shops, boutiques and online stores.", Modules: []string{"pos", "inventory", "ecommerce", "invoicing", "crm"}, RecommendedTier: TierSME, Active: true, SortOrder: 1},
		{Key: "hospitality", Name: "Hospitality & food", Icon: "fa-solid fa-utensils", Description: "Cafés, restaurants, bars and catering.", Modules: []string{"pos", "inventory", "bookings", "hr", "invoicing"}, RecommendedTier: TierSME, Active: true, SortOrder: 2},
		{Key: "automotive", Name: "Automotive & garages", Icon: "fa-solid fa-car", Description: "Workshops, mechanics, detailing and tyre shops.", Modules: []string{"bookings", "invoicing", "inventory", "crm", "pos"}, RecommendedTier: TierSME, Active: true, SortOrder: 3},
		{Key: "beauty", Name: "Beauty & wellness", Icon: "fa-solid fa-spa", Description: "Salons, barbers, spas, nail studios and gyms.", Modules: []string{"bookings", "pos", "crm", "invoicing", "communication"}, RecommendedTier: TierSmall, Active: true, SortOrder: 4},
		{Key: "healthcare", Name: "Healthcare & NDIS / care", Icon: "fa-solid fa-hand-holding-medical", Description: "Clinics, allied health, NDIS and aged-care providers.", Modules: []string{"bookings", "hr", "documents", "invoicing", "communication"}, RecommendedTier: TierSME, Active: true, SortOrder: 5},
		{Key: "professional", Name: "Professional services & consulting", Icon: "fa-solid fa-briefcase", Description: "Consultants, agencies, accountants and law firms.", Modules: []string{"invoicing", "crm", "projects", "documents", "communication"}, RecommendedTier: TierSmall, Active: true, SortOrder: 6},
		{Key: "construction", Name: "Construction & trades", Icon: "fa-solid fa-helmet-safety", Description: "Builders, electricians, plumbers and contractors.", Modules: []string{"projects", "invoicing", "inventory", "hr", "documents"}, RecommendedTier: TierSME, Active: true, SortOrder: 7},
		{Key: "events", Name: "Events & venues", Icon: "fa-solid fa-champagne-glasses", Description: "Venues, event organisers and ticketed experiences.", Modules: []string{"events", "bookings", "invoicing", "crm", "communication"}, RecommendedTier: TierSME, Active: true, SortOrder: 8},
		{Key: "manufacturing", Name: "Manufacturing", Icon: "fa-solid fa-industry", Description: "Makers, factories and assembly operations.", Modules: []string{"manufacturing", "inventory", "accounting", "hr", "invoicing"}, RecommendedTier: TierLarge, Active: true, SortOrder: 9},
		{Key: "education", Name: "Education & training", Icon: "fa-solid fa-graduation-cap", Description: "Schools, tutors, RTOs and course providers.", Modules: []string{"bookings", "events", "invoicing", "communication", "documents"}, RecommendedTier: TierSME, Active: true, SortOrder: 10},
		{Key: "nonprofit", Name: "Non-profit", Icon: "fa-solid fa-hand-holding-heart", Description: "Charities, associations, clubs and community groups.", Modules: []string{"crm", "events", "accounting", "documents", "communication"}, RecommendedTier: TierSME, Active: true, SortOrder: 11},
		{Key: "general", Name: "General business", Icon: "fa-solid fa-building", Description: "Anything else — start small and add what you need.", Modules: []string{"smallbiz", "invoicing", "crm", "accounting"}, RecommendedTier: TierSmall, Active: true, SortOrder: 12},
	}
}

// DefaultTiers are the three packages.
func DefaultTiers() []Tier {
	return []Tier{
		{
			Key: TierSmall, Name: "Small", Tagline: "Pick up to 2 systems — pay only for what you use.",
			BasePriceUSD: 9, MaxModules: 2, MaxUsers: 5, AnnualDiscountPct: 15, TrialDays: 14, SortOrder: 1,
			Features: []string{"Up to 2 systems of your choice", "Core dashboard & analytics", "Up to 5 users", "Email support"},
		},
		{
			Key: TierSME, Name: "SME", Tagline: "Your industry bundle included, add more at a discount.",
			BasePriceUSD: 79, MaxModules: 0, MaxUsers: 50, AddonDiscountPct: 20, AnnualDiscountPct: 15,
			IncludesIndustryBundle: true, TrialDays: 14, SortOrder: 2,
			Features: []string{"Full industry bundle included", "Add-on systems at 20% off", "Up to 50 users", "Priority support"},
		},
		{
			Key: TierLarge, Name: "Large", Tagline: "Every module, unlimited users, enterprise options.",
			BasePriceUSD: 399, MaxModules: 0, MaxUsers: 0, AnnualDiscountPct: 20,
			IncludesAllModules: true, Enterprise: true, TrialDays: 30, SortOrder: 3,
			Features: []string{"All modules included", "Unlimited users", "Custom pricing & contracts", "Dedicated account manager"},
		},
	}
}

// DefaultFXRates are approximate units per 1 USD, used only when live rates
// are unavailable. Super admins can correct them.
func DefaultFXRates() []FXRate {
	rates := map[string]float64{
		"USD": 1, "EUR": 0.92, "GBP": 0.78, "JPY": 148, "CHF": 0.88, "CAD": 1.37,
		"AUD": 1.52, "NZD": 1.68, "CNY": 7.2, "INR": 84, "SGD": 1.33, "HKD": 7.8,
		"AED": 3.6725, "SAR": 3.75, "KWD": 0.307,
		"ZAR": 18, "NGN": 1550, "EGP": 48.5, "KES": 129, "GHS": 15.5, "MAD": 9.9, "TZS": 2700, "UGX": 3700,
	}
	out := make([]FXRate, 0, len(rates))
	for k, v := range rates {
		out = append(out, FXRate{Currency: k, RatePerUSD: v})
	}
	return out
}

// industryAliases maps legacy organisation business_type values to industries.
var industryAliases = map[string]string{
	"retail": "retail", "ecommerce": "retail", "shop": "retail", "store": "retail",
	"hospitality": "hospitality", "restaurant": "hospitality", "cafe": "hospitality", "food": "hospitality",
	"automotive": "automotive", "garage": "automotive", "mechanic": "automotive", "auto": "automotive",
	"beauty": "beauty", "salon": "beauty", "spa": "beauty", "wellness": "beauty", "barber": "beauty", "gym": "beauty",
	"healthcare": "healthcare", "ndis": "healthcare", "care": "healthcare", "clinic": "healthcare", "medical": "healthcare",
	"professional": "professional", "consulting": "professional", "agency": "professional", "services": "professional",
	"construction": "construction", "trades": "construction", "builder": "construction",
	"events": "events", "venue": "events",
	"manufacturing": "manufacturing", "factory": "manufacturing",
	"education": "education", "training": "education", "school": "education",
	"nonprofit": "nonprofit", "non-profit": "nonprofit", "charity": "nonprofit",
}
