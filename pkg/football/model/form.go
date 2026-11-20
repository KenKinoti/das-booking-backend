package model

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// TeamResult is one past match from a team's point of view. OppRating is the
// opponent's Elo-scale rating before the match (league mean ≈ 1500, e.g.
// from Elo.Rating); 0 = unknown.
type TeamResult struct {
	Date      time.Time
	Home      bool
	GF, GA    int
	XGF, XGA  *float64
	Opp       string
	OppRating float64
}

// FormSummary summarises recent results. Points use 3/1/0.
type FormSummary struct {
	N           int
	W, D, L     int
	Points, PPG float64
	GF, GA      int
	XGF, XGA    float64 // sums over the matches that have xG (XGN of them)
	HasXG       bool
	Sequence    string  // newest last, e.g. "WWDLW"
	WeightedPPG float64 // exponential decay, half-life 4 matches (newest weighs 1)
	AdjPPG      float64 // opponent- and venue-adjusted PPG (see FormWith)
	Trend       float64 // OLS slope of the rolling PPG, in PPG per match (+ = improving)

	XGN           int
	GD            int
	CleanSheets   int
	FailedToScore int
	AdjN          int // matches whose opponent rating was known
}

// FormOptions tunes FormWith. Zero fields take the defaults shown.
type FormOptions struct {
	HalfLife  float64 // matches (default 4)
	RefRating float64 // Elo-scale league mean (default 1500)
	HomeAdv   float64 // Elo points (default 65)
	DrawNu    float64 // Davidson ν for expected points (default 0.7 ≈ 26% draws between equals)
	RollWin   int     // rolling window for Trend (default 5, or n/2 when fewer than 8 matches)
}

// Form summarises the last n results (rs sorted oldest → newest; n ≤ 0 =
// all). For home-only / away-only form, filter rs before calling.
func Form(rs []TeamResult, n int) FormSummary { return FormWith(rs, n, FormOptions{}) }

// FormWith is Form with explicit options.
//
// AdjPPG: each result's points are compared with what an average team would
// expect from that fixture (same opponent rating, same venue), using an
// Elo/Davidson model; the average difference is added to an average team's
// neutral-venue expectation (≈1.37 PPG). So AdjPPG > PPG means the results
// came against harder fixtures than average. Results without an opponent
// rating are adjusted for venue only.
func FormWith(rs []TeamResult, n int, o FormOptions) FormSummary {
	if o.HalfLife <= 0 {
		o.HalfLife = 4
	}
	if o.RefRating <= 0 {
		o.RefRating = 1500
	}
	if o.HomeAdv == 0 {
		o.HomeAdv = 65
	}
	if o.DrawNu <= 0 {
		o.DrawNu = 0.7
	}
	if n > 0 && len(rs) > n {
		rs = rs[len(rs)-n:]
	}
	var f FormSummary
	f.N = len(rs)
	if f.N == 0 {
		return f
	}
	pts := make([]float64, len(rs))
	var seq strings.Builder
	xpRef := davidsonXP(0, o.DrawNu)
	adjSum := 0.0
	for i, r := range rs {
		switch {
		case r.GF > r.GA:
			f.W++
			pts[i] = 3
			seq.WriteByte('W')
		case r.GF == r.GA:
			f.D++
			pts[i] = 1
			seq.WriteByte('D')
		default:
			f.L++
			seq.WriteByte('L')
		}
		f.GF += r.GF
		f.GA += r.GA
		if r.GA == 0 {
			f.CleanSheets++
		}
		if r.GF == 0 {
			f.FailedToScore++
		}
		if r.XGF != nil && r.XGA != nil {
			f.XGF += *r.XGF
			f.XGA += *r.XGA
			f.XGN++
		}
		opp := o.RefRating
		if r.OppRating > 0 {
			opp = r.OppRating
			f.AdjN++
		}
		dr := o.RefRating - opp
		if r.Home {
			dr += o.HomeAdv
		} else {
			dr -= o.HomeAdv
		}
		adjSum += pts[i] - davidsonXP(dr, o.DrawNu) + xpRef
	}
	f.HasXG = f.XGN > 0
	f.GD = f.GF - f.GA
	for _, p := range pts {
		f.Points += p
	}
	f.PPG = f.Points / float64(f.N)
	f.Sequence = seq.String()
	f.AdjPPG = adjSum / float64(f.N)
	// Exponentially weighted PPG.
	sw, sp := 0.0, 0.0
	for i := range pts {
		age := float64(len(pts) - 1 - i)
		w := math.Pow(0.5, age/o.HalfLife)
		sw += w
		sp += w * pts[i]
	}
	f.WeightedPPG = sp / sw
	// Trend: slope of the rolling PPG.
	win := o.RollWin
	if win <= 0 {
		win = 5
		if len(pts) < 8 {
			win = len(pts) / 2
		}
	}
	if win < 1 {
		win = 1
	}
	if len(pts)-win+1 >= 2 {
		roll := make([]float64, 0, len(pts)-win+1)
		s := 0.0
		for i, p := range pts {
			s += p
			if i >= win {
				s -= pts[i-win]
			}
			if i >= win-1 {
				roll = append(roll, s/float64(win))
			}
		}
		f.Trend = olsSlope(roll)
	}
	return f
}

// davidsonXP is the expected points (3/1/0) of a side with rating advantage
// dr (Elo points, venue included) under Davidson's ties model.
func davidsonXP(dr, nu float64) float64 {
	r := math.Pow(10, dr/800)
	den := r + 1/r + nu
	return (3*r + nu) / den
}

func olsSlope(y []float64) float64 {
	n := float64(len(y))
	if n < 2 {
		return 0
	}
	mx := (n - 1) / 2
	my := 0.0
	for _, v := range y {
		my += v
	}
	my /= n
	num, den := 0.0, 0.0
	for i, v := range y {
		dx := float64(i) - mx
		num += dx * (v - my)
		den += dx * dx
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// PlayerContribution is a squad member's season output (for the team the
// forecast is about). Position is free text ("GK", "Defender", "M", "FW"…).
type PlayerContribution struct {
	ID, Name       string
	Minutes        int
	Goals, Assists int
	XG, XA         *float64
	Rating         *float64
	Position       string
}

// Availability mapping constants (documented in README.md).
const (
	// AvailReplacement is the share of a missing player's attacking output
	// that his replacement is assumed to recover.
	AvailReplacement = 0.6
	// AvailMinAttack floors the attack multiplier (at most −20%).
	AvailMinAttack = 0.80
	// AvailGK / AvailDef: added to goals conceded for a missing regular
	// goalkeeper / defender (scaled by how regular he is: minutes / squad max).
	AvailGK  = 0.06
	AvailDef = 0.03
	// AvailMaxDefence caps the defence multiplier (at most +15% conceded).
	AvailMaxDefence = 1.15
)

// PositionGroup maps free-text positions to "GK", "DEF", "MID", "FWD" or "".
func PositionGroup(pos string) string {
	p := strings.ToUpper(strings.TrimSpace(pos))
	switch {
	case p == "":
		return ""
	case p == "G" || p == "GK" || strings.HasPrefix(p, "GOAL") || p == "KEEPER":
		return "GK"
	case p == "D" || p == "DF" || strings.HasPrefix(p, "DEF") || p == "CB" || p == "LB" || p == "RB" ||
		p == "WB" || p == "LWB" || p == "RWB" || strings.Contains(p, "BACK"):
		return "DEF"
	case p == "M" || p == "MF" || strings.HasPrefix(p, "MID") || p == "CM" || p == "DM" || p == "CDM" ||
		p == "AM" || p == "CAM" || p == "LM" || p == "RM":
		return "MID"
	case p == "F" || p == "FW" || strings.HasPrefix(p, "FOR") || strings.HasPrefix(p, "ATT") || p == "ST" ||
		p == "CF" || p == "LW" || p == "RW" || strings.Contains(p, "STRIKER") || strings.Contains(p, "WING"):
		return "FWD"
	}
	return ""
}

// AvailabilityAdjust estimates how missing players change a team's expected
// goals. attackMult multiplies the goals the team is expected to score
// (≤ 1); defenceMult multiplies the goals it is expected to concede (≥ 1) —
// plug them into Adjust.HomeAttack / Adjust.HomeDefence (or the Away pair).
//
// Mapping (deliberately conservative — absences are noisy and partly priced
// in already):
//   - Attacking involvement of a player = xG + ½·xA when the squad has xG data
//     (else goals + ½·assists); his share = involvement / team total.
//   - A replacement recovers 60% of the missing output, so
//     attackMult = 1 − 0.4·(sum of missing shares), floored at 0.80.
//   - A missing goalkeeper adds 6% and each missing defender 3% to goals
//     conceded, scaled by regularity (minutes / most minutes in the squad);
//     defenceMult is capped at 1.15. Midfielders' defensive work is ignored.
//
// why explains the result in plain words. Ids not in team are ignored (and
// mentioned).
func AvailabilityAdjust(team []PlayerContribution, missingIDs []string) (attackMult, defenceMult float64, why string) {
	attackMult, defenceMult = 1, 1
	if len(missingIDs) == 0 {
		return 1, 1, "No missing players listed."
	}
	missing := map[string]bool{}
	for _, id := range missingIDs {
		if id != "" {
			missing[id] = true
		}
	}
	byID := map[string]PlayerContribution{}
	for _, p := range team {
		if p.ID != "" {
			byID[p.ID] = p
		}
	}
	// xG coverage decides which involvement measure to use.
	totMin, xgMin, maxMin := 0, 0, 0
	for _, p := range team {
		m := p.Minutes
		if m < 0 {
			m = 0
		}
		totMin += m
		if p.XG != nil {
			xgMin += m
		}
		if m > maxMin {
			maxMin = m
		}
	}
	useXG := totMin > 0 && float64(xgMin) >= 0.5*float64(totMin)
	inv := func(p PlayerContribution) float64 {
		if useXG {
			v := 0.0
			if p.XG != nil && *p.XG > 0 {
				v += *p.XG
			}
			if p.XA != nil && *p.XA > 0 {
				v += 0.5 * *p.XA
			}
			return v
		}
		v := 0.0
		if p.Goals > 0 {
			v += float64(p.Goals)
		}
		if p.Assists > 0 {
			v += 0.5 * float64(p.Assists)
		}
		return v
	}
	total := 0.0
	for _, p := range team {
		total += inv(p)
	}
	measure := "goals+½assists"
	if useXG {
		measure = "xG+½xA"
	}
	var parts []string
	share := 0.0
	defAdd := 0.0
	unknown := 0
	ids := sortedKeys(missing)
	type item struct {
		name string
		s    float64
		text string
	}
	var items []item
	for _, id := range ids {
		p, ok := byID[id]
		if !ok {
			unknown++
			continue
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		grp := PositionGroup(p.Position)
		s := 0.0
		if total > 0 {
			s = inv(p) / total
		}
		share += s
		reg := 0.0
		if maxMin > 0 && p.Minutes > 0 {
			reg = math.Min(1, float64(p.Minutes)/float64(maxMin))
		}
		var bits []string
		if s >= 0.005 {
			bits = append(bits, fmt.Sprintf("%.0f%% of the team's %s", 100*s, measure))
		}
		dAdd := 0.0
		switch grp {
		case "GK":
			dAdd = AvailGK * reg
			bits = append(bits, fmt.Sprintf("goalkeeper, %s", regularity(reg)))
		case "DEF":
			dAdd = AvailDef * reg
			bits = append(bits, fmt.Sprintf("defender, %s", regularity(reg)))
		}
		defAdd += dAdd
		label := name
		if grp != "" {
			label += " (" + grp + ")"
		}
		if len(bits) > 0 {
			label += ": " + strings.Join(bits, "; ")
		} else {
			label += ": little measured impact"
		}
		items = append(items, item{name, (1-AvailReplacement)*s + dAdd, label})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].s > items[j].s })
	for _, it := range items {
		parts = append(parts, it.text)
	}
	attackMult = math.Max(AvailMinAttack, 1-(1-AvailReplacement)*share)
	defenceMult = math.Min(AvailMaxDefence, 1+defAdd)
	if len(items) == 0 {
		why = "None of the missing players are in the squad list, so no adjustment."
		return 1, 1, why
	}
	why = fmt.Sprintf("Missing: %s. Expected goals scored ×%.2f, conceded ×%.2f (conservative: a replacement is assumed to recover %.0f%% of a missing player's attacking output; a regular goalkeeper adds %.0f%% and a regular defender %.0f%% to goals conceded; limits −%.0f%% / +%.0f%%).",
		strings.Join(parts, "; "), attackMult, defenceMult, 100*AvailReplacement, 100*AvailGK, 100*AvailDef,
		100*(1-AvailMinAttack), 100*(AvailMaxDefence-1))
	if unknown > 0 {
		why += fmt.Sprintf(" %d missing player(s) not in the squad list were ignored.", unknown)
	}
	return attackMult, defenceMult, why
}

func regularity(r float64) string {
	switch {
	case r >= 0.7:
		return "regular starter"
	case r >= 0.35:
		return "rotation player"
	case r > 0:
		return "fringe player"
	}
	return "no minutes"
}

// ResultsFor extracts one team's results from ms, sorted oldest → newest,
// ready for Form. Matches at or after `before` are skipped (zero = keep
// all). rating, when non-nil, supplies the opponent's Elo-scale rating at
// the time (return 0 when unknown). For home-only or away-only form, filter
// the result on TeamResult.Home.
func ResultsFor(team string, ms []Match, before time.Time, rating func(opp string, at time.Time) float64) []TeamResult {
	var out []TeamResult
	for _, m := range ms {
		if !before.IsZero() && !m.Date.Before(before) {
			continue
		}
		var r TeamResult
		switch team {
		case m.Home:
			r = TeamResult{Date: m.Date, Home: !m.Neutral, GF: m.HG, GA: m.AG, XGF: m.HxG, XGA: m.AxG, Opp: m.Away}
		case m.Away:
			r = TeamResult{Date: m.Date, Home: false, GF: m.AG, GA: m.HG, XGF: m.AxG, XGA: m.HxG, Opp: m.Home}
		default:
			continue
		}
		if rating != nil {
			r.OppRating = rating(r.Opp, m.Date)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}
