package onboarding

import (
	"regexp"
	"strings"
	"unicode"
)

var placeholder = regexp.MustCompile(`\{(a_)?([A-Za-z_]+)\}`)

// lowerTerm is a term for running text: "Table reservations" → "table
// reservations", acronyms ("NDIS") stay as they are.
func lowerTerm(t string) string {
	words := strings.Fields(t)
	for i, w := range words {
		if len([]rune(w)) > 1 && w == strings.ToUpper(w) {
			continue
		}
		words[i] = strings.ToLower(w)
	}
	return strings.Join(words, " ")
}

func article(word string) string {
	for _, r := range word {
		if strings.ContainsRune("aeiouAEIOU", r) {
			return "an " + word
		}
		return "a " + word
	}
	return word
}

// FillTerms replaces {key}, {Key} and {a_key} placeholders with the
// organisation's words (see industry.Step). Unknown keys stay as they are.
func FillTerms(s string, terms map[string]string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		parts := placeholder.FindStringSubmatch(m)
		withArticle, name := parts[1] != "", parts[2]
		t, ok := terms[strings.ToLower(name)]
		if !ok || t == "" {
			return m
		}
		r := []rune(name)
		if !unicode.IsUpper(r[0]) {
			t = lowerTerm(t)
		}
		if withArticle {
			t = article(t)
		}
		return t
	})
}
