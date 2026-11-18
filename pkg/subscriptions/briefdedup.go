package subscriptions

import "strings"

// BriefCovers, when set (cmd/production wires pkg/brief), returns the
// lower-case addresses of the organisation's people whose morning brief
// already lists its client subscriptions and who did not ask for the separate digests too.
// The automatic daily client billing digest skips them; a manual "Send now" does not.
var BriefCovers func(org string) map[string]bool

// withoutBriefed splits recipients into those still emailed and those who
// get the bills in their morning brief.
func withoutBriefed(org string, to []string) (kept, briefed []string) {
	if BriefCovers == nil || len(to) == 0 {
		return to, nil
	}
	covered := BriefCovers(org)
	if len(covered) == 0 {
		return to, nil
	}
	for _, a := range to {
		if covered[strings.ToLower(strings.TrimSpace(a))] {
			briefed = append(briefed, a)
		} else {
			kept = append(kept, a)
		}
	}
	return kept, briefed
}
