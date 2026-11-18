package scout

import (
	"strings"
	"unicode"
)

// Duplicates: the same tender or grant shows up again in later scans (often
// with a slightly different title or from another page). Two items are the
// same when
//
//   - they share a source URL and their titles are alike (similarity ≥ 0.5 —
//     a portal's listing page can be the source of many different tenders), or
//   - their titles are near-identical (similarity ≥ 0.82).
//
// Similarity is the larger of the Dice coefficient over character bigrams and
// the Jaccard index over words, on titles with punctuation, case and filler
// words removed.

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "for": true, "and": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"with": true, "from": true, "&": true, "invitation": true, "notice": true, "request": true, "tender": true, "rfp": true, "rfq": true,
	"call": true, "proposals": true, "proposal": true, "no": true, "ref": true,
}

// titleKey is the comparable form of a title.
func titleKey(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	words := []string{}
	for _, w := range strings.Fields(b.String()) {
		if !stopWords[w] {
			words = append(words, w)
		}
	}
	k := strings.Join(words, " ")
	if len(k) > 290 {
		k = k[:290]
	}
	return k
}

func bigrams(s string) map[string]int {
	s = strings.ReplaceAll(s, " ", "")
	r := []rune(s)
	m := map[string]int{}
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])]++
	}
	return m
}

func dice(a, b string) float64 {
	ba, bb := bigrams(a), bigrams(b)
	na, nb := 0, 0
	for _, n := range ba {
		na += n
	}
	for _, n := range bb {
		nb += n
	}
	if na+nb == 0 {
		return 0
	}
	inter := 0
	for k, n := range ba {
		if m := bb[k]; m > 0 {
			if m < n {
				inter += m
			} else {
				inter += n
			}
		}
	}
	return 2 * float64(inter) / float64(na+nb)
}

func jaccard(a, b string) float64 {
	wa, wb := map[string]bool{}, map[string]bool{}
	for _, w := range strings.Fields(a) {
		wa[w] = true
	}
	for _, w := range strings.Fields(b) {
		wb[w] = true
	}
	if len(wa)+len(wb) == 0 {
		return 0
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(wa)+len(wb)-inter)
}

// similar compares two title keys (0–1).
func similar(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	d, j := dice(a, b), jaccard(a, b)
	if j > d {
		return j
	}
	return d
}

func shareSource(a, b []SourceRef) bool {
	seen := map[string]bool{}
	for _, s := range a {
		if n := NormURL(s.URL); n != "" {
			seen[n] = true
		}
	}
	for _, s := range b {
		if seen[NormURL(s.URL)] {
			return true
		}
	}
	return false
}

// SameOpportunity reports whether two items are the same opportunity.
func SameOpportunity(a, b *Opportunity) bool {
	ka, kb := a.TitleKey, b.TitleKey
	if ka == "" {
		ka = titleKey(a.Title)
	}
	if kb == "" {
		kb = titleKey(b.Title)
	}
	s := similar(ka, kb)
	if s >= 0.82 {
		return true
	}
	return s >= 0.5 && shareSource(a.Sources, b.Sources)
}
