package brief

import "strings"

// Covered returns the addresses (lower case) of the organisation's people
// whose morning brief already brings them a section — so the separate
// digest emails can skip them (pkg/recurbills' bills digest for "bills",
// pkg/subscriptions' client billing digest for "money"). A person counts when
// the brief is on with a channel that can reach them, the section is on and
// allowed for them, and "Also send the separate daily digests" is off.
func (s *Service) Covered(org, section string) map[string]bool {
	out := map[string]bool{}
	var reg *registration
	for _, r := range sections() {
		if r.Key == section {
			reg = r
		}
	}
	if reg == nil || !hasTable(s.DB, "brief_settings") {
		return out
	}
	var list []Settings
	s.DB.Where("organization_id = ? AND enabled AND NOT also_digests", org).Find(&list)
	for _, st := range list {
		if !st.SectionOn(section) {
			continue
		}
		v, err := s.Viewer(st.UserID)
		if err != nil || v.OrgID != org || v.Email == "" {
			continue
		}
		if reg.Allowed != nil && !reg.Allowed(v) {
			continue
		}
		if !(st.Email || (st.WhatsApp && strings.TrimSpace(v.Phone) != "")) {
			continue
		}
		out[lower(v.Email)] = true
	}
	return out
}
