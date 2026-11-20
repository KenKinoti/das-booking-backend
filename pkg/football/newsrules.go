package football

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

// News → team news with keyword rules (no AI cost):
//   1. headline + snippet are folded (lower case, no diacritics, ł → l …);
//   2. keyword rules per language and status match at word starts with a
//      little inflection tolerance (Polish "kontuzja"/"kontuzji", German
//      "verletzt"/"verletzten"; a trailing * allows any ending);
//   3. player names of the team's squad (API-Football players) are found
//      the same way: full name, "J. Surname", or a surname unique in the squad;
//   4. a candidate fact = team + player (or coach) + status; facts are deduped
//      by key, renewed by new items and expire (default 14 days);
//   5. confidence: confirmed = in API-Football's injury list, or ≥ 2
//      independent sites within 7 days, or a manager / super admin confirmed
//      it; otherwise "reported (1 source)". Only confirmed injured / suspended
//      players feed AvailabilityAdjust.

// DefaultNewsRules are the owner's keyword sets (editable in settings).
func DefaultNewsRules() []NewsRule {
	r := func(lang, status string, kw ...string) NewsRule {
		return NewsRule{Language: lang, Status: status, Keywords: kw, Negative: []string{}}
	}
	return []NewsRule{
		r("en", TNInjured, "injured", "injury", "ruled out", "out for", "sidelined"),
		r("en", TNDoubtful, "doubtful", "doubt", "fitness test", "major doubt"),
		r("en", TNSuspended, "suspended", "suspension", "red card ban", "yellow-card ban", "banned"),
		r("en", TNReturning, "returns", "back in training", "back from injury", "fit again"),
		r("en", TNCoach, "sacked", "appointed", "new coach", "new manager", "interim"),
		r("pl", TNInjured, "kontuzja", "kontuzjowany", "uraz", "pauza", "nie zagra"),
		r("pl", TNSuspended, "zawieszony", "za kartki", "czerwona kartka"),
		r("pl", TNReturning, "wraca"),
		r("pl", TNCoach, "trener", "zwolniony"),
		r("de", TNInjured, "verletzt", "Verletzung", "fällt aus"),
		r("de", TNSuspended, "gesperrt", "Sperre"),
		r("de", TNReturning, "zurück"),
		r("de", TNCoach, "Trainer", "entlassen"),
		r("nl", TNInjured, "geblesseerd", "blessure"),
		r("nl", TNSuspended, "geschorst", "schorsing"),
		r("nl", TNCoach, "trainer", "ontslagen"),
		r("da", TNInjured, "skadet", "skade"),
		r("da", TNSuspended, "karantæne"),
		r("da", TNCoach, "træner", "fyret"),
		r("ro", TNInjured, "accidentat"),
		r("ro", TNSuspended, "suspendat"),
		r("ro", TNCoach, "antrenor", "demis"),
		r("fr", TNInjured, "blessé"),
		r("fr", TNSuspended, "suspendu"),
		r("fr", TNCoach, "entraîneur"),
		r("sw", TNInjured, "majeruhi"),
		r("sw", TNSuspended, "amefungiwa"),
		r("sw", TNCoach, "kocha"),
	}
}

// NewsLanguages supported by the default rules.
var NewsLanguages = []string{"en", "pl", "de", "nl", "da", "ro", "fr", "sw"}

// coachRoleWords alone don't make a coach change ("the coach said …"):
// a coach-change fact needs another coach keyword or a strong one.
var coachRoleWords = map[string]bool{"trener": true, "trainer": true, "traener": true, "antrenor": true, "entraineur": true, "kocha": true, "coach": true, "manager": true}

// postponedWords flag a kickoff / venue change in the source check.
var postponedWords = []string{"postponed", "called off", "rescheduled", "przelozony", "przelozone", "odwolany", "verschoben", "abgesagt", "uitgesteld",
	"afgelast", "udsat", "aflyst", "amanat", "reporte", "imeahirishwa", "limeahirishwa"}

// tokenize folds text into words (letters/digits).
func tokenize(s string) []string {
	f := Fold(s)
	return strings.FieldsFunc(f, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// wordMatch: a text word matches a keyword word at the start with a small
// inflection tolerance (keyword < 6 letters: up to 2 extra letters; longer:
// the keyword's last letter may change and up to 3 extra letters); "*" at
// the end of the keyword allows any ending.
func wordMatch(word, kw string) bool {
	if strings.HasSuffix(kw, "*") {
		return strings.HasPrefix(word, strings.TrimSuffix(kw, "*"))
	}
	if word == kw {
		return true
	}
	if len(kw) < 6 {
		return strings.HasPrefix(word, kw) && len(word)-len(kw) <= 2
	}
	stem := kw[:len(kw)-1]
	return strings.HasPrefix(word, stem) && len(word)-len(kw) <= 3
}

// phraseIn reports whether a (multi-word) keyword occurs in the words:
// earlier words exactly, the last one with inflection tolerance.
func phraseIn(words []string, kw string) bool {
	kws := tokenize(kw)
	star := strings.HasSuffix(strings.TrimSpace(kw), "*")
	if len(kws) == 0 {
		return false
	}
	if star {
		kws[len(kws)-1] += "*"
	}
	for i := 0; i+len(kws) <= len(words); i++ {
		ok := true
		for j, k := range kws {
			w := words[i+j]
			if j < len(kws)-1 {
				if w != k {
					ok = false
					break
				}
			} else if !wordMatch(w, k) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// KeywordHit is a matched keyword.
type KeywordHit struct {
	Status   string `json:"status"`
	Keyword  string `json:"keyword"`
	Language string `json:"language"`
}

// MatchKeywords finds the rules' keywords in a text. lang restricts to one
// language ("" = all languages, e.g. GDELT items of unknown language).
func MatchKeywords(text, lang string, rules []NewsRule) []KeywordHit {
	words := tokenize(text)
	out := []KeywordHit{}
	seen := map[string]bool{}
	for _, r := range rules {
		if lang != "" && r.Language != lang && r.Language != "en" && lang != "en" {
			// Other languages' rules are skipped, but English is always tried
			// (English words are common in every sports press).
			continue
		}
		if lang == "en" && r.Language != "en" {
			continue
		}
		neg := false
		for _, n := range r.Negative {
			if phraseIn(words, n) {
				neg = true
			}
		}
		if neg {
			continue
		}
		for _, k := range r.Keywords {
			if strings.TrimSpace(k) == "" {
				continue
			}
			if phraseIn(words, k) {
				key := r.Status + "|" + strings.ToLower(k)
				if !seen[key] {
					seen[key] = true
					out = append(out, KeywordHit{Status: r.Status, Keyword: k, Language: r.Language})
				}
			}
		}
	}
	return out
}

// DetectLanguage guesses a headline's language from the rules' keywords and
// a few common words (used when the provider doesn't say).
func DetectLanguage(text string, hint string) string {
	if hint != "" {
		return hint
	}
	words := tokenize(text)
	common := map[string][]string{
		"pl": {"nie", "sie", "przed", "meczem", "mecz", "trener", "wraca", "oraz", "jest", "bez", "dla"},
		"de": {"der", "die", "das", "und", "nicht", "gegen", "spiel", "fur", "mit", "beim"},
		"nl": {"het", "een", "niet", "tegen", "wedstrijd", "voor", "met", "van"},
		"da": {"og", "ikke", "mod", "kamp", "efter", "med", "til"},
		"ro": {"si", "cu", "nu", "meci", "pentru", "este", "dupa"},
		"fr": {"le", "la", "les", "et", "contre", "pour", "avec", "est"},
		"sw": {"na", "kwa", "ya", "wa", "mechi", "timu", "dhidi"},
		"en": {"the", "and", "for", "with", "against", "match", "after", "will", "out"},
	}
	best, bn := "", 0
	for _, l := range NewsLanguages {
		n := 0
		for _, w := range words {
			for _, c := range common[l] {
				if w == c {
					n++
				}
			}
		}
		if n > bn {
			best, bn = l, n
		}
	}
	return best
}

// SquadPlayer is a player the matcher looks for.
type SquadPlayer struct {
	ID        int64
	Name      string // API-Football display name, often "J. Félix"
	Firstname string
	Lastname  string
}

// surnamesOf lists the surnames a player may be called by: the display
// name's last word ("J. Félix" → felix) and the first and last words of
// the full last name ("Félix Muñoz García" → felix, garcia).
func surnamesOf(p SquadPlayer) []string {
	out := []string{}
	add := func(s string) {
		if len(s) >= 3 {
			for _, x := range out {
				if x == s {
					return
				}
			}
			out = append(out, s)
		}
	}
	nm := p.Name
	if i := strings.LastIndex(nm, "."); i >= 0 {
		nm = nm[i+1:]
	}
	if ts := tokenize(nm); len(ts) > 0 {
		add(ts[len(ts)-1])
	}
	if ts := tokenize(p.Lastname); len(ts) > 0 {
		add(ts[0])
		add(ts[len(ts)-1])
	}
	return out
}

// MatchPlayers finds squad players named in a text: the full name, the
// first initial + surname, or a surname (≥ 4 letters) that only one player
// of the squad has. Names are diacritics-insensitive and Polish-style
// endings ("Błanika") are tolerated.
func MatchPlayers(text string, squad []SquadPlayer) []SquadPlayer {
	words := tokenize(text)
	bySurname := map[string]int{}
	for _, p := range squad {
		for _, sn := range surnamesOf(p) {
			bySurname[sn]++
		}
	}
	nameWord := func(w, n string) bool {
		if w == n {
			return true
		}
		return len(n) >= 4 && strings.HasPrefix(w, n) && len(w)-len(n) <= 2
	}
	out := []SquadPlayer{}
	for _, p := range squad {
		first := ""
		if ft := tokenize(p.Firstname); len(ft) > 0 {
			first = ft[0]
		} else if nt := tokenize(p.Name); len(nt) > 1 {
			first = nt[0]
		}
		found := false
		for _, sn := range surnamesOf(p) {
			for i, w := range words {
				if !nameWord(w, sn) {
					continue
				}
				if i > 0 && first != "" {
					prev := words[i-1]
					if prev == first || (len(prev) == 1 && strings.HasPrefix(first, prev)) || (len(first) == 1 && strings.HasPrefix(prev, first)) {
						found = true
						break
					}
				}
				if bySurname[sn] == 1 && len(sn) >= 4 && !commonWord[sn] {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			out = append(out, p)
		}
	}
	return out
}

// commonWord: surnames that are also everyday words are only matched with a first name.
var commonWord = map[string]bool{"gol": true, "mecz": true, "king": true, "young": true, "white": true, "black": true, "brown": true, "green": true,
	"best": true, "long": true, "little": true, "fernandes": false, "will": true, "mark": true, "back": true, "nowak": false, "pauza": true, "uraz": true}

// teamMentioned: a distinctive word of the team's name appears in the text.
func teamMentioned(text, team string) bool {
	words := tokenize(text)
	for _, t := range freeTokens(team) {
		if len(t) < 4 {
			continue
		}
		for _, w := range words {
			if w == t || (strings.HasPrefix(w, t) && len(w)-len(t) <= 3) || (len(t) >= 6 && strings.HasPrefix(w, t[:len(t)-1]) && len(w)-len(t) <= 3) {
				return true
			}
		}
	}
	return false
}

// CandidateFact is what one news item says about one team.
type CandidateFact struct {
	TeamID   int64
	PlayerID int64
	Player   string
	Role     string
	Status   string
	Keywords []string
}

// statusPriority resolves several statuses in one headline ("back in
// training after his injury" is a return, "doubtful with a knee injury" is
// doubtful).
var statusPriority = []string{TNReturning, TNDoubtful, TNSuspended, TNInjured}

// ExtractFacts turns one headline + snippet into candidate facts for a team.
func ExtractFacts(text, lang string, team int64, squad []SquadPlayer, rules []NewsRule) ([]CandidateFact, []KeywordHit) {
	hits := MatchKeywords(text, lang, rules)
	if len(hits) == 0 {
		return nil, hits
	}
	byStatus := map[string][]string{}
	for _, h := range hits {
		byStatus[h.Status] = append(byStatus[h.Status], h.Keyword)
	}
	out := []CandidateFact{}
	// Players.
	var pstatus string
	for _, st := range statusPriority {
		if len(byStatus[st]) > 0 {
			pstatus = st
			break
		}
	}
	if pstatus != "" {
		for _, p := range MatchPlayers(text, squad) {
			kw := append([]string(nil), byStatus[pstatus]...)
			out = append(out, CandidateFact{TeamID: team, PlayerID: p.ID, Player: p.Name, Role: "player", Status: pstatus, Keywords: kw})
		}
	}
	// Coach change: a strong keyword, or two coach keywords.
	if ck := byStatus[TNCoach]; len(ck) > 0 {
		strong := 0
		for _, k := range ck {
			if !coachRoleWords[strings.Join(tokenize(k), " ")] {
				strong++
			}
		}
		if strong > 0 || len(ck) >= 2 {
			out = append(out, CandidateFact{TeamID: team, Role: "coach", Status: TNCoach, Keywords: ck})
		}
	}
	return out, hits
}

func factKey(c CandidateFact) string {
	who := "coach"
	if c.Role == "player" {
		who = fmt.Sprintf("p%d", c.PlayerID)
	}
	return fmt.Sprintf("%d|%s|%s", c.TeamID, who, c.Status)
}

// squadOf lists a team's players (latest season with stats).
func (s *Service) squadOf(team int64) []SquadPlayer {
	var rows []SquadPlayer
	s.DB.Raw(`SELECT DISTINCT p.id, p.name, p.firstname, p.lastname FROM fb_player_seasons ps JOIN fb_players p ON p.id = ps.player_id
		WHERE ps.team_id = ? AND ps.season >= (SELECT COALESCE(MAX(season), 0) - 1 FROM fb_player_seasons WHERE team_id = ?)`, team, team).Scan(&rows)
	return rows
}

func (s *Service) newsRules() []NewsRule {
	var rs []NewsRule
	s.DB.Order("language, status").Find(&rs)
	if len(rs) == 0 {
		return DefaultNewsRules()
	}
	return rs
}

// recordFacts upserts the candidate facts of a news item (renewing expiry).
func (s *Service) recordFacts(item *NewsItem, cands []CandidateFact) []string {
	st := s.FreeSettingsGet()
	now := s.Now()
	seenAt := item.PublishedAt
	if seenAt.IsZero() || seenAt.After(now) {
		seenAt = now
	}
	ids := []string{}
	for _, c := range cands {
		key := factKey(c)
		var f TeamNewsFact
		exists := s.DB.Where("key = ?", key).Limit(1).Find(&f).RowsAffected > 0
		if exists && f.ExpiresAt.Before(now) {
			// An expired fact starts again (old sources no longer count).
			f.NewsIDs, f.Domains, f.Keywords, f.Languages, f.FirstSeen = []string{}, []string{}, []string{}, []string{}, seenAt
			f.AdminAction, f.AdminBy, f.AdminAt, f.AdminNote = "", "", nil, ""
		}
		if !exists {
			f = TeamNewsFact{ID: uuid.NewString(), Key: key, TeamID: c.TeamID, PlayerID: c.PlayerID, PlayerName: clip(c.Player, 120), Role: c.Role, Status: c.Status,
				FirstSeen: seenAt, NewsIDs: []string{}, Domains: []string{}, Keywords: []string{}, Languages: []string{}}
		}
		if seenAt.After(f.LastSeen) {
			f.LastSeen = seenAt
		}
		if seenAt.Before(f.FirstSeen) {
			f.FirstSeen = seenAt
		}
		f.ExpiresAt = f.LastSeen.AddDate(0, 0, st.NewsExpiryDays)
		f.NewsIDs = addUniq(f.NewsIDs, item.ID)
		f.Domains = addUniq(f.Domains, item.Domain)
		for _, k := range c.Keywords {
			f.Keywords = addUniq(f.Keywords, k)
		}
		f.Languages = addUniq(f.Languages, item.Language)
		f.UpdatedAt = now
		if len(f.NewsIDs) > 50 {
			f.NewsIDs = f.NewsIDs[len(f.NewsIDs)-50:]
		}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&f)
		ids = append(ids, f.ID)
	}
	return ids
}

func addUniq(xs []string, x string) []string {
	if strings.TrimSpace(x) == "" {
		return xs
	}
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// ---------------------------------------------------------------- confidence

// factState is a fact with its confidence for one organisation's view.
type factState struct {
	TeamNewsFact
	confirmed   bool
	confirmedBy string
	note        string
	dismissed   bool
	review      *FactReview
	domains7    int
	items       []NewsItem
}

// FactReview is the review shown with a fact.
type FactReview struct {
	Action string    `json:"action"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Note   string    `json:"note"`
	Scope  string    `json:"scope"`
}

// inAFInjuries: the player is in API-Football's injury list for one of the
// team's fixtures in the last 3 / next 7 days with a matching status.
func (s *Service) inAFInjuries(team, player int64, status string) bool {
	if player == 0 {
		return false
	}
	sts := []string{"injured", "suspended", "doubtful"}
	switch status {
	case TNInjured:
		sts = []string{"injured"}
	case TNSuspended:
		sts = []string{"suspended"}
	case TNDoubtful:
		sts = []string{"doubtful", "injured"}
	case TNReturning, TNCoach:
		return false
	}
	var n int64
	s.DB.Raw(`SELECT COUNT(*) FROM fb_injuries i JOIN fb_fixtures f ON f.id = i.fixture_id WHERE i.team_id = ? AND i.player_id = ? AND i.status IN ?
		AND f.kickoff BETWEEN ? AND ?`, team, player, sts, s.Now().AddDate(0, 0, -3), s.Now().AddDate(0, 0, 7)).Scan(&n)
	return n > 0
}

// activeFacts loads a team's unexpired facts with their confidence for an
// organisation ("" = the global view: only source / platform confirmations).
func (s *Service) activeFacts(team int64, org string) []factState {
	st := s.FreeSettingsGet()
	now := s.Now()
	var fs []TeamNewsFact
	s.DB.Where("team_id = ? AND expires_at > ?", team, now).Order("last_seen DESC").Limit(60).Find(&fs)
	if len(fs) == 0 {
		return nil
	}
	ids := []string{}
	newsIDs := []string{}
	for _, f := range fs {
		ids = append(ids, f.ID)
		newsIDs = append(newsIDs, f.NewsIDs...)
	}
	reviews := map[string]TeamNewsReview{}
	if org != "" {
		var rs []TeamNewsReview
		s.DB.Where("organization_id = ? AND fact_id IN ?", org, ids).Find(&rs)
		for _, r := range rs {
			reviews[r.FactID] = r
		}
	}
	items := map[string]NewsItem{}
	if len(newsIDs) > 0 {
		var ns []NewsItem
		s.DB.Where("id IN ?", newsIDs).Find(&ns)
		for _, n := range ns {
			items[n.ID] = n
		}
	}
	out := []factState{}
	for _, f := range fs {
		fsx := factState{TeamNewsFact: f}
		doms := map[string]bool{}
		for _, nid := range f.NewsIDs {
			n, ok := items[nid]
			if !ok {
				continue
			}
			fsx.items = append(fsx.items, n)
			if !n.PublishedAt.Before(f.LastSeen.AddDate(0, 0, -7)) {
				doms[n.Domain] = true
			}
		}
		fsx.domains7 = len(doms)
		sort.Slice(fsx.items, func(i, j int) bool { return fsx.items[i].PublishedAt.After(fsx.items[j].PublishedAt) })
		switch {
		case f.AdminAction == "dismiss":
			fsx.dismissed = true
			fsx.note = "dismissed by a platform admin"
			if f.AdminAt != nil {
				fsx.review = &FactReview{Action: "dismiss", By: f.AdminBy, At: *f.AdminAt, Note: f.AdminNote, Scope: "platform"}
			}
		case f.AdminAction == "confirm":
			fsx.confirmed, fsx.confirmedBy, fsx.note = true, "admin", "confirmed by a platform admin"
			if f.AdminAt != nil {
				fsx.review = &FactReview{Action: "confirm", By: f.AdminBy, At: *f.AdminAt, Note: f.AdminNote, Scope: "platform"}
			}
		case s.inAFInjuries(f.TeamID, f.PlayerID, f.Status):
			fsx.confirmed, fsx.confirmedBy, fsx.note = true, ProviderAPIFootball, "in API-Football injuries"
		case fsx.domains7 >= st.NewsConfirmDomains:
			fsx.confirmed, fsx.confirmedBy, fsx.note = true, "sources", fmt.Sprintf("%d independent sites within 7 days", fsx.domains7)
		default:
			fsx.note = fmt.Sprintf("reported (%d source%s)", maxInt(1, fsx.domains7), plural(maxInt(1, fsx.domains7)))
		}
		if r, ok := reviews[f.ID]; ok && f.AdminAction == "" {
			fsx.review = &FactReview{Action: r.Action, By: r.UserName, At: r.At, Note: r.Note, Scope: "organisation"}
			switch r.Action {
			case "confirm":
				if !fsx.confirmed {
					fsx.confirmed, fsx.confirmedBy, fsx.note = true, "manager", "confirmed by a manager in your organisation"
				}
			case "dismiss":
				fsx.dismissed, fsx.confirmed = true, false
				fsx.note = "dismissed by a manager in your organisation"
			}
		}
		out = append(out, fsx)
	}
	return out
}

// usedInModel: confirmed, not dismissed, a matched player, injured or suspended.
func (f factState) usedInModel() bool {
	return f.confirmed && !f.dismissed && f.PlayerID != 0 && (f.Status == TNInjured || f.Status == TNSuspended)
}

// newsMissing: players of a team that confirmed team news says miss the
// fixture (only facts seen before kickoff count).
func (s *Service) newsMissing(f Fixture, team int64, org string) []int64 {
	out := []int64{}
	for _, fs := range s.activeFacts(team, org) {
		if fs.usedInModel() && fs.LastSeen.Before(f.Kickoff) {
			out = append(out, fs.PlayerID)
		}
	}
	return out
}

// TeamNewsSource is a news item behind a fact.
type TeamNewsSource struct {
	NewsID      string    `json:"news_id"`
	Title       string    `json:"title"`
	Domain      string    `json:"domain"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Language    string    `json:"language"`
	Provider    string    `json:"provider"`
	Reliability *float64  `json:"reliability"`
}

// TeamNewsOut is a fact in a payload.
type TeamNewsOut struct {
	ID             string           `json:"id"`
	TeamID         int64            `json:"team_id"`
	Team           string           `json:"team"`
	Side           string           `json:"side"`
	PlayerID       *int64           `json:"player_id"`
	Player         string           `json:"player"`
	Role           string           `json:"role"`
	Status         string           `json:"status"`
	Confidence     string           `json:"confidence"`
	ConfirmedBy    *string          `json:"confirmed_by"`
	ConfidenceNote string           `json:"confidence_note"`
	UsedInModel    bool             `json:"used_in_model"`
	FirstSeen      time.Time        `json:"first_seen"`
	LastSeen       time.Time        `json:"last_seen"`
	ExpiresAt      time.Time        `json:"expires_at"`
	Expired        bool             `json:"expired"`
	Keywords       []string         `json:"keywords"`
	Languages      []string         `json:"languages"`
	Sources        []TeamNewsSource `json:"sources"`
	Review         *FactReview      `json:"review"`
	Dismissed      bool             `json:"dismissed"`
}

func (s *Service) factOut(f factState, side string, rel map[string]float64) TeamNewsOut {
	o := TeamNewsOut{ID: f.ID, TeamID: f.TeamID, Team: s.teamName(f.TeamID), Side: side, Player: f.PlayerName, Role: f.Role, Status: f.Status,
		Confidence: "reported", ConfidenceNote: f.note, UsedInModel: f.usedInModel(), FirstSeen: f.FirstSeen, LastSeen: f.LastSeen, ExpiresAt: f.ExpiresAt,
		Expired: f.ExpiresAt.Before(s.Now()), Keywords: nonNil(f.Keywords), Languages: nonNil(f.Languages), Sources: []TeamNewsSource{}, Review: f.review, Dismissed: f.dismissed}
	if f.PlayerID != 0 {
		id := f.PlayerID
		o.PlayerID = &id
	}
	if f.confirmed && !f.dismissed {
		o.Confidence = "confirmed"
		cb := f.confirmedBy
		o.ConfirmedBy = &cb
	}
	for _, n := range f.items {
		prov := n.Provider
		if prov == ProviderFeeds {
			prov = "feed"
		}
		src := TeamNewsSource{NewsID: n.ID, Title: n.Title, Domain: n.Domain, URL: n.URL, PublishedAt: n.PublishedAt, Language: n.Language, Provider: prov}
		if r, ok := rel[n.Domain]; ok {
			v := r
			src.Reliability = &v
		}
		o.Sources = append(o.Sources, src)
	}
	return o
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// ---------------------------------------------------------------- reliability

// NewsSourceStat is a site's track record.
type NewsSourceStat struct {
	Domain      string    `json:"domain"`
	Facts       int       `json:"facts"`
	Confirmed   int       `json:"confirmed"`
	Dismissed   int       `json:"dismissed"`
	Reliability float64   `json:"reliability"`
	LastSeen    time.Time `json:"last_seen"`
}

// SourceReliability: per site, how often its facts were later confirmed by
// API-Football, other sites, a manager or an admin (Laplace-smoothed:
// (confirmed + 1) / (facts + 2)), over the last 180 days.
func (s *Service) SourceReliability() []NewsSourceStat {
	var fs []TeamNewsFact
	s.DB.Where("last_seen > ?", s.Now().AddDate(0, 0, -180)).Find(&fs)
	var reviews []TeamNewsReview
	s.DB.Find(&reviews)
	orgConfirm, orgDismiss := map[string]bool{}, map[string]bool{}
	for _, r := range reviews {
		if r.Action == "confirm" {
			orgConfirm[r.FactID] = true
		} else if r.Action == "dismiss" {
			orgDismiss[r.FactID] = true
		}
	}
	m := map[string]*NewsSourceStat{}
	for _, f := range fs {
		confirmed := f.AdminAction == "confirm" || orgConfirm[f.ID] || len(f.Domains) >= 2 || s.inAFInjuries(f.TeamID, f.PlayerID, f.Status)
		dismissed := f.AdminAction == "dismiss" || orgDismiss[f.ID]
		for _, d := range f.Domains {
			st := m[d]
			if st == nil {
				st = &NewsSourceStat{Domain: d}
				m[d] = st
			}
			st.Facts++
			if confirmed && !dismissed {
				st.Confirmed++
			}
			if dismissed {
				st.Dismissed++
			}
			if f.LastSeen.After(st.LastSeen) {
				st.LastSeen = f.LastSeen
			}
		}
	}
	out := []NewsSourceStat{}
	for _, st := range m {
		st.Reliability = round4(float64(st.Confirmed+1) / float64(st.Facts+2))
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Facts != out[j].Facts {
			return out[i].Facts > out[j].Facts
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

var (
	relMu sync.Mutex
	relC  map[string]float64
	relAt time.Time
)

// reliabilityMap is SourceReliability by domain, cached for 10 minutes
// (it is shown with every team-news source).
func (s *Service) reliabilityMap() map[string]float64 {
	relMu.Lock()
	if relC != nil && time.Since(relAt) < 10*time.Minute {
		c := relC
		relMu.Unlock()
		return c
	}
	relMu.Unlock()
	out := map[string]float64{}
	for _, r := range s.SourceReliability() {
		if r.Facts >= 1 {
			out[r.Domain] = r.Reliability
		}
	}
	relMu.Lock()
	relC, relAt = out, time.Now()
	relMu.Unlock()
	return out
}

func resetReliabilityCache() {
	relMu.Lock()
	relC = nil
	relMu.Unlock()
}

// ---------------------------------------------------------------- reviews

// ReviewFact confirms / dismisses / clears a fact. Super admins review it
// platform-wide; managers for their organisation. Every review is audited.
func (s *Service) ReviewFact(org, user, userName, role, factID, action, note string) (*TeamNewsOut, *TeamNewsAudit, error) {
	if action != "confirm" && action != "dismiss" && action != "clear" {
		return nil, nil, inputErr("Action must be confirm, dismiss or clear.")
	}
	var f TeamNewsFact
	if s.DB.Where("id = ?", factID).Limit(1).Find(&f).RowsAffected == 0 {
		return nil, nil, ErrNotFound
	}
	now := s.Now()
	note = clip(note, 500)
	scope := "organisation"
	if strings.EqualFold(role, "super_admin") {
		scope = "platform"
		up := map[string]any{"admin_action": "", "admin_by": "", "admin_at": nil, "admin_note": "", "updated_at": now}
		if action != "clear" {
			up = map[string]any{"admin_action": action, "admin_by": clip(userName, 160), "admin_at": now, "admin_note": note, "updated_at": now}
		}
		if err := s.DB.Model(&TeamNewsFact{}).Where("id = ?", f.ID).Updates(up).Error; err != nil {
			return nil, nil, err
		}
		// The global prediction of the team's upcoming fixtures is recomputed.
		s.DB.Exec(`DELETE FROM fb_predictions WHERE kind = 'current' AND fixture_id IN (SELECT id FROM fb_fixtures WHERE (home_id = ? OR away_id = ?) AND kickoff > ?)`, f.TeamID, f.TeamID, now)
	} else {
		if action == "clear" {
			s.DB.Where("organization_id = ? AND fact_id = ?", org, f.ID).Delete(&TeamNewsReview{})
		} else {
			r := TeamNewsReview{OrganizationID: org, FactID: f.ID, Action: action, UserID: user, UserName: clip(userName, 160), Note: note, At: now}
			if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error; err != nil {
				return nil, nil, err
			}
		}
	}
	a := TeamNewsAudit{ID: uuid.NewString(), OrganizationID: org, FactID: f.ID, Action: action, Scope: scope, UserID: user, UserName: clip(userName, 160), Note: note, At: now}
	s.DB.Create(&a)
	resetReliabilityCache()
	viewOrg := org
	for _, fs := range s.activeFacts(f.TeamID, viewOrg) {
		if fs.ID == f.ID {
			o := s.factOut(fs, "", s.reliabilityMap())
			return &o, &a, nil
		}
	}
	// Expired fact: return it plainly.
	o := TeamNewsOut{ID: f.ID, TeamID: f.TeamID, Team: s.teamName(f.TeamID), Player: f.PlayerName, Role: f.Role, Status: f.Status, Confidence: "reported",
		Expired: true, Keywords: nonNil(f.Keywords), Languages: nonNil(f.Languages), Sources: []TeamNewsSource{}, FirstSeen: f.FirstSeen, LastSeen: f.LastSeen, ExpiresAt: f.ExpiresAt}
	return &o, &a, nil
}

// FactAudit lists a fact's audit rows (the organisation's, or all for super admins).
func (s *Service) FactAudit(org, role, factID string) []TeamNewsAudit {
	out := []TeamNewsAudit{}
	q := s.DB.Where("fact_id = ?", factID)
	if !strings.EqualFold(role, "super_admin") {
		q = q.Where("organization_id = ? OR scope = 'platform'", org)
	}
	q.Order("at DESC").Limit(200).Find(&out)
	return out
}

// TestRules runs the keyword rules on a text (settings page "try it").
func (s *Service) TestRules(text, lang string, team int64) map[string]any {
	rules := s.newsRules()
	lang = DetectLanguage(text, lang)
	var squad []SquadPlayer
	if team != 0 {
		squad = s.squadOf(team)
	}
	cands, hits := ExtractFacts(text, lang, team, squad, rules)
	if lang == "" && len(hits) > 0 {
		lang = hits[0].Language
	}
	players := []map[string]any{}
	for _, p := range MatchPlayers(text, squad) {
		players = append(players, map[string]any{"player_id": p.ID, "name": p.Name})
	}
	var fact any
	if len(cands) > 0 {
		c := cands[0]
		fact = map[string]any{"status": c.Status, "player": c.Player, "player_id": c.PlayerID, "role": c.Role, "team": s.teamName(team), "team_id": team}
	}
	ms := []map[string]any{}
	for _, h := range hits {
		ms = append(ms, map[string]any{"status": h.Status, "keyword": h.Keyword, "language": h.Language})
	}
	return map[string]any{"language": lang, "matches": ms, "players": players, "fact": fact}
}
