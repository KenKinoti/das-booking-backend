package football

// Football v2.19.1 — locked picks you can check (and no more nonsense picks).
//
//   - lock_models.go: the platform's lock / pass state (fb_lock_state) and the
//     shapes added to rows, the match page, bets and results.
//   - lock_gate.go: the data gate ("no pick without real data"), how much the
//     model knows about two teams, typed-in prices.
//   - lock.go: picks locked early (every target fixture within the lookahead
//     days, refreshed whenever the inputs change, final at kickoff), lock on
//     view (throttled), the model's frozen prediction stored early, the labels
//     ("Locked 2 h 10 min before kick-off") and the bets' locked pick.
//   - lock_tick.go: GET|POST /public/football/tick, the start-up catch-up
//     pass, the background self-heal pass, the keyless free sources on by
//     default (migration-safe).
//   - lock_results.go: GET /football/results (finished matches with ✓ / ✗
//     and a one-line tally).
//
// Honesty rules: a lock is only ever made before kickoff, never changed after
// kickoff, graded once; a match without enough data gets no pick (and says
// so) instead of an unfitted default presented as a probability.

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockState is the platform's scheduler-pass and defaults bookkeeping (id 1,
// global: no organisation data).
type LockState struct {
	ID          int             `json:"-" gorm:"primaryKey"`
	LastPassAt  *time.Time      `json:"last_pass_at"`
	LastTrigger string          `json:"last_trigger" gorm:"type:varchar(20)"`
	LastSummary *PassSummary    `json:"last_summary" gorm:"type:text;serializer:json"`
	LastTickAt  *time.Time      `json:"last_tick_at"`
	Ticks       int             `json:"ticks"`
	KeylessAt   *time.Time      `json:"keyless_at"`                                  // when the keyless sources' defaults were applied (once)
	KeylessOn   []string        `json:"keyless_on" gorm:"type:text;serializer:json"` // sources the defaults switched on
	Chosen      map[string]bool `json:"chosen" gorm:"type:text;serializer:json"`     // sources a person switched on or off since
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (LockState) TableName() string { return "fb_lock_state" }

// LockModels lists the v2.19.1 tables.
func LockModels() []any { return []any{&LockState{}} }

// PassSummary is what one scheduler pass did, in counts only (the tick
// endpoint returns it; no keys, no provider messages).
type PassSummary struct {
	At         time.Time `json:"at"`
	Trigger    string    `json:"trigger"`
	DurationMS int64     `json:"duration_ms"`
	Planned    int       `json:"planned"`
	Done       int       `json:"done"`
	Skipped    int       `json:"skipped"`
	Failed     int       `json:"failed"`
	Fitted     int       `json:"fitted"`
	Predicted  int       `json:"predicted"`
	Locked     int       `json:"locked"`      // picks locked or refreshed before kickoff
	NoData     int       `json:"no_data"`     // matches locked as "not enough data"
	PredLocked int       `json:"pred_locked"` // model predictions stored / refreshed before kickoff
	Graded     int       `json:"graded"`      // picks marked ✓ / ✗ (or void)
	Settled    int       `json:"settled"`     // bets settled
	Notes      []string  `json:"notes"`
}

// ModelData is how much DASYIN's own model knows about a match's two teams.
type ModelData struct {
	EloHome   int    `json:"elo_matches_home"` // rated matches (every stored result, all competitions)
	EloAway   int    `json:"elo_matches_away"`
	DCFitted  bool   `json:"dc_fitted"` // Dixon–Coles fitted for the competition
	DCMatches int    `json:"dc_matches"`
	DCHome    int    `json:"dc_matches_home"` // matches of each team in that fit
	DCAway    int    `json:"dc_matches_away"`
	DCOK      bool   `json:"dc_ok"`    // fitted and both teams ≥ MinDCTeamMatches
	EloOK     bool   `json:"elo_ok"`   // both teams ≥ MinEloMatches
	Solid     bool   `json:"solid"`    // DCOK or EloOK: enough to back a pick
	HasData   bool   `json:"has_data"` // the model knows something (both teams ≥ MinEloShow rated matches, or in the fit); else its numbers are hidden
	Why       string `json:"why"`      // plain reason when not solid
}

// Data-gate thresholds (spec: Dixon–Coles with both teams ≥ 8 matches in the
// competition's fit, or Elo with both teams ≥ 15 rated matches).
const (
	MinDCTeamMatches = 8
	MinEloMatches    = 15
	// MinEloShow: below this many rated matches for either team (and no
	// Dixon–Coles fit) the model's numbers are hidden — they would be the
	// unfitted default (0.4573 / 0.2542 / 0.2885 with two unrated teams).
	MinEloShow = 5
)

// GateCheck is one kind of evidence the gate looked for.
type GateCheck struct {
	Key   string `json:"key"` // market | clubelo | apifootball_pred | model_dc | model_elo
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Why   string `json:"why"`
}

// GateOut says whether a match has at least one solid source.
type GateOut struct {
	OK        bool        `json:"ok"`
	Solid     []string    `json:"solid"`      // labels of the solid sources
	ModelOnly bool        `json:"model_only"` // no market price and no ClubElo: capped at Good
	Reason    string      `json:"reason"`     // plain reason when there's no pick
	Checks    []GateCheck `json:"checks"`
}

// LockOut is a fixture's lock as rows and pages show it.
type LockOut struct {
	// locked: a pick is locked (refreshes until kickoff, then final);
	// not_enough_data: locked as "no pick — not enough data";
	// not_locked: nothing locked (upcoming: not yet; after kickoff: never was).
	Status           string     `json:"status"`
	LockedAt         *time.Time `json:"locked_at"`       // when the current (after kickoff: final) pick was stored
	FirstLockedAt    *time.Time `json:"first_locked_at"` // the first lock of this match
	CheckedAt        *time.Time `json:"checked_at"`      // the last time the inputs were compared
	BeforeKickoffMin *int       `json:"before_kickoff_min"`
	Label            string     `json:"label"` // "Locked 2 h 10 min before kick-off" | "No pick — not enough data" | …
	Final            bool       `json:"final"` // kickoff passed: never changes again
	Refreshes        int        `json:"refreshes"`
	Reason           string     `json:"reason"` // not_enough_data: the plain reason
}

// NoPickOut explains why an upcoming match has no pick.
type NoPickOut struct {
	Status string      `json:"status"` // not_enough_data
	Label  string      `json:"label"`  // "No pick — not enough data"
	Reason string      `json:"reason"` // "No prices and too little match history for these teams"
	Checks []GateCheck `json:"checks"`
}

// LockedPick is DASYIN's locked pick next to a bet (My bets).
type LockedPick struct {
	Status      string     `json:"status"` // pending | correct | wrong | void | not_enough_data | not_locked
	Label       string     `json:"label"`  // "Correct" | "Wrong" | "Void" | "Not graded yet" | "No pick — not enough data" | "No pick — not locked before kick-off"
	Pick        string     `json:"pick,omitempty"`
	PickLabel   string     `json:"pick_label,omitempty"`
	P           float64    `json:"p,omitempty"`
	Tier        string     `json:"tier,omitempty"`
	TierLabel   string     `json:"tier_label,omitempty"`
	Score       string     `json:"score,omitempty"`
	LockedAt    *time.Time `json:"locked_at"`
	LockedLabel string     `json:"locked_label,omitempty"`
	Final       bool       `json:"final"`
	Reason      string     `json:"reason,omitempty"`
	Summary     string     `json:"summary,omitempty"`
}

// Keyless free sources switched on by default (v2.19.1): free, no key,
// polite budgets — never overriding a choice somebody made.
var KeylessDefaults = []string{ProviderClubElo, ProviderFDCOUK, ProviderOpenFootball}

func isKeyless(p string) bool {
	for _, k := range KeylessDefaults {
		if k == p {
			return true
		}
	}
	return false
}

func seedLock(db *gorm.DB) error {
	st := LockState{ID: 1, Chosen: map[string]bool{}, KeylessOn: []string{}}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&st).Error; err != nil {
		return err
	}
	return applyKeylessDefaults(db, time.Now().UTC())
}

func (s *Service) lockState() LockState {
	var st LockState
	if s.DB.Where("id = 1").Limit(1).Find(&st).RowsAffected == 0 {
		st = LockState{ID: 1, Chosen: map[string]bool{}, KeylessOn: []string{}}
		s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&st)
	}
	if st.Chosen == nil {
		st.Chosen = map[string]bool{}
	}
	if st.KeylessOn == nil {
		st.KeylessOn = []string{}
	}
	return st
}
