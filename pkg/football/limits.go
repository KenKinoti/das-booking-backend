package football

import (
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// Responsible gambling: a person's own stake and loss limits per day, week
// and month (in the organisation's time zone; weeks start on Monday) and a
// break ("cool-off") that blocks recording new bets until it ends.
//
//   - Lowering a limit (or adding one) applies at once.
//   - Raising or removing a limit waits CoolOff (24 h) — the pending set
//     replaces the current one when the wait is over.
//   - A break can be made longer but never shorter.
//   - A loss limit counts the period's settled losses plus the stakes still
//     at risk plus the new stake.

// CoolOff is how long raising a limit waits.
const CoolOff = 24 * time.Hour

// LimitUse is one limit with what has been used.
type LimitUse struct {
	Key      string    `json:"key"` // daily_stake …
	Label    string    `json:"label"`
	Limit    *float64  `json:"limit"`
	Used     float64   `json:"used"`
	Left     *float64  `json:"left"`
	Pending  *float64  `json:"pending"`
	ResetsAt time.Time `json:"resets_at"`
}

// LimitsView is the limits page.
type LimitsView struct {
	Currency     string     `json:"currency"`
	Limits       []LimitUse `json:"limits"`
	PendingFrom  *time.Time `json:"pending_from"`
	CoolOffUntil *time.Time `json:"cool_off_until"`
	OnBreak      bool       `json:"on_break"`
	Note         string     `json:"note"`
	Help         string     `json:"help"`
}

// Help text (no legal advice).
const LimitsHelp = "DASYIN does not accept bets or hold money — you bet with your own bookmaker and record it here, so DASYIN is not acting as a bookmaker " +
	"(Kenya's gambling law, including the Gambling Control Act 2025, licenses bookmakers; this is general information, not legal advice). " +
	"Limits and breaks only stop you recording new bets here: set the same limits in your bookmaker's app. 18+ only. " +
	"If betting stops being fun, take a break and talk to someone you trust."

func (s *Service) loadLimits(org, user string) Limits {
	var l Limits
	if s.DB.Where("organization_id = ? AND user_id = ?", org, user).Limit(1).Find(&l).RowsAffected == 0 {
		l = Limits{OrganizationID: org, UserID: user}
	}
	// A pending raise takes effect once the cool-off is over.
	if l.Pending != nil && l.PendingFrom != nil && !s.Now().Before(*l.PendingFrom) {
		p := *l.Pending
		l.DailyStake, l.WeeklyStake, l.MonthlyStake, l.DailyLoss, l.WeeklyLoss, l.MonthlyLoss = p.DailyStake, p.WeeklyStake, p.MonthlyStake, p.DailyLoss, p.WeeklyLoss, p.MonthlyLoss
		l.Pending, l.PendingFrom = nil, nil
		l.UpdatedAt = s.Now()
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&l)
	}
	return l
}

// periodStart returns the start of the day / week / month containing t.
func periodStart(kind string, t time.Time, loc *time.Location) (time.Time, time.Time) {
	l := t.In(loc)
	d := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	switch kind {
	case "weekly":
		wd := (int(d.Weekday()) + 6) % 7 // Monday = 0
		s := d.AddDate(0, 0, -wd)
		return s, s.AddDate(0, 0, 7)
	case "monthly":
		s := time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 1, 0)
	}
	return d, d.AddDate(0, 0, 1)
}

// usage returns the stakes and the loss at risk (settled losses + open
// stakes) of bets placed in [from, to).
func (s *Service) usage(org, user string, from, to time.Time) (stake, risk float64) {
	var r struct {
		Stake   float64
		Settled float64
		Open    float64
	}
	s.DB.Raw(`SELECT COALESCE(SUM(stake),0) AS stake,
		COALESCE(SUM(CASE WHEN status <> 'open' THEN pnl ELSE 0 END),0) AS settled,
		COALESCE(SUM(CASE WHEN status = 'open' THEN stake ELSE 0 END),0) AS open
		FROM fb_bets WHERE organization_id = ? AND user_id = ? AND placed_at >= ? AND placed_at < ?`, org, user, from.UTC(), to.UTC()).Scan(&r)
	risk = -r.Settled + r.Open
	if risk < 0 {
		risk = 0
	}
	return r.Stake, risk
}

func money(cur string, v float64) string {
	neg := v < 0
	v = math.Abs(v)
	whole := int64(v)
	frac := int64(math.Round((v - float64(whole)) * 100))
	if frac == 100 {
		whole++
		frac = 0
	}
	s := fmt.Sprintf("%d", whole)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if frac > 0 {
		s += fmt.Sprintf(".%02d", frac)
	}
	if neg {
		s = "-" + s
	}
	return cur + " " + s
}

// CheckLimits refuses a new stake that would break a limit or comes during
// a break.
func (s *Service) CheckLimits(org, user string, stake float64, placed time.Time) error {
	l := s.loadLimits(org, user)
	now := s.Now()
	loc := s.zoneOf(org)
	cur := s.currencyOf(org)
	if l.CoolOffUntil != nil && now.Before(*l.CoolOffUntil) {
		u := *l.CoolOffUntil
		return &LimitError{Limit: "break", Until: &u, Msg: fmt.Sprintf("You're taking a break from betting until %s. New bets can be recorded after that.",
			u.In(loc).Format("Mon 2 Jan 15:04"))}
	}
	type chk struct {
		kind, label string
		stakeL      *float64
		lossL       *float64
	}
	for _, c := range []chk{{"daily", "today", l.DailyStake, l.DailyLoss}, {"weekly", "this week", l.WeeklyStake, l.WeeklyLoss}, {"monthly", "this month", l.MonthlyStake, l.MonthlyLoss}} {
		if c.stakeL == nil && c.lossL == nil {
			continue
		}
		from, to := periodStart(c.kind, placed, loc)
		used, risk := s.usage(org, user, from, to)
		reset := to
		if c.stakeL != nil && used+stake > *c.stakeL+1e-9 {
			return &LimitError{Limit: c.kind + "_stake", Until: &reset, Msg: fmt.Sprintf("This bet would take your stakes %s to %s, over your %s stake limit of %s. You can record bets again from %s.",
				c.label, money(cur, used+stake), c.kind, money(cur, *c.stakeL), reset.In(loc).Format("Mon 2 Jan 15:04"))}
		}
		if c.lossL != nil && risk+stake > *c.lossL+1e-9 {
			return &LimitError{Limit: c.kind + "_loss", Until: &reset, Msg: fmt.Sprintf("This bet could take your losses %s to %s (losses so far plus stakes still open), over your %s loss limit of %s. You can record bets again from %s.",
				c.label, money(cur, risk+stake), c.kind, money(cur, *c.lossL), reset.In(loc).Format("Mon 2 Jan 15:04"))}
		}
	}
	return nil
}

// LimitsView returns the limits with what's used.
func (s *Service) LimitsView(org, user string) LimitsView {
	l := s.loadLimits(org, user)
	now := s.Now()
	loc := s.zoneOf(org)
	v := LimitsView{Currency: s.currencyOf(org), Limits: []LimitUse{}, PendingFrom: l.PendingFrom, Help: LimitsHelp}
	if l.CoolOffUntil != nil && now.Before(*l.CoolOffUntil) {
		v.CoolOffUntil, v.OnBreak = l.CoolOffUntil, true
	}
	var p LimitSet
	if l.Pending != nil {
		p = *l.Pending
	}
	type row struct {
		key, label, kind string
		lim, pend        *float64
		loss             bool
	}
	for _, r := range []row{
		{"daily_stake", "Daily stakes", "daily", l.DailyStake, p.DailyStake, false},
		{"weekly_stake", "Weekly stakes", "weekly", l.WeeklyStake, p.WeeklyStake, false},
		{"monthly_stake", "Monthly stakes", "monthly", l.MonthlyStake, p.MonthlyStake, false},
		{"daily_loss", "Daily losses", "daily", l.DailyLoss, p.DailyLoss, true},
		{"weekly_loss", "Weekly losses", "weekly", l.WeeklyLoss, p.WeeklyLoss, true},
		{"monthly_loss", "Monthly losses", "monthly", l.MonthlyLoss, p.MonthlyLoss, true},
	} {
		from, to := periodStart(r.kind, now, loc)
		st, risk := s.usage(org, user, from, to)
		u := LimitUse{Key: r.key, Label: r.label, Limit: r.lim, Used: round2(st), ResetsAt: to.UTC()}
		if r.loss {
			u.Used = round2(risk)
		}
		if r.lim != nil {
			left := math.Max(0, round2(*r.lim-u.Used))
			u.Left = &left
		}
		if l.Pending != nil && !sameLimit(r.lim, r.pend) {
			if r.pend == nil {
				u.Pending = ptr(-1.0) // removal pending
			} else {
				u.Pending = r.pend
			}
		}
		v.Limits = append(v.Limits, u)
	}
	if l.Pending != nil && l.PendingFrom != nil {
		v.Note = "Higher limits take effect on " + l.PendingFrom.In(loc).Format("Mon 2 Jan 15:04") + " (24-hour cool-off). Lower limits apply straight away."
	}
	return v
}

func sameLimit(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 1e-9
}

// tighter: b is no looser than a (nil = no limit).
func tighter(a, b *float64) bool {
	if b == nil {
		return a == nil
	}
	return a == nil || *b <= *a+1e-9
}

// SetLimits saves new limits: tighter ones apply now, looser ones after
// the cool-off.
func (s *Service) SetLimits(org, user string, in LimitSet) (LimitsView, error) {
	for _, v := range []*float64{in.DailyStake, in.WeeklyStake, in.MonthlyStake, in.DailyLoss, in.WeeklyLoss, in.MonthlyLoss} {
		if v != nil && (*v < 0 || *v > 1e10 || math.IsNaN(*v)) {
			return LimitsView{}, inputErr("Limits must be 0 or more (leave empty for no limit).")
		}
	}
	l := s.loadLimits(org, user)
	now := s.Now()
	cur := []*float64{l.DailyStake, l.WeeklyStake, l.MonthlyStake, l.DailyLoss, l.WeeklyLoss, l.MonthlyLoss}
	want := []*float64{in.DailyStake, in.WeeklyStake, in.MonthlyStake, in.DailyLoss, in.WeeklyLoss, in.MonthlyLoss}
	now2 := make([]*float64, 6) // applied now
	pend := make([]*float64, 6) // after the cool-off
	looser := false
	for i := range cur {
		if tighter(cur[i], want[i]) {
			now2[i], pend[i] = want[i], want[i]
		} else {
			now2[i], pend[i] = cur[i], want[i]
			looser = true
		}
	}
	l.DailyStake, l.WeeklyStake, l.MonthlyStake, l.DailyLoss, l.WeeklyLoss, l.MonthlyLoss = now2[0], now2[1], now2[2], now2[3], now2[4], now2[5]
	if looser {
		ps := LimitSet{DailyStake: pend[0], WeeklyStake: pend[1], MonthlyStake: pend[2], DailyLoss: pend[3], WeeklyLoss: pend[4], MonthlyLoss: pend[5]}
		from := now.Add(CoolOff)
		l.Pending, l.PendingFrom = &ps, &from
	} else {
		l.Pending, l.PendingFrom = nil, nil
	}
	l.OrganizationID, l.UserID, l.UpdatedAt = org, user, now
	if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&l).Error; err != nil {
		return LimitsView{}, err
	}
	return s.LimitsView(org, user), nil
}

// StartBreak blocks recording new bets for days (1–365); it can only be
// extended.
func (s *Service) StartBreak(org, user string, days int) (LimitsView, error) {
	if days < 1 || days > 365 {
		return LimitsView{}, inputErr("A break is 1 to 365 days.")
	}
	l := s.loadLimits(org, user)
	until := s.Now().Add(time.Duration(days) * 24 * time.Hour)
	if l.CoolOffUntil != nil && l.CoolOffUntil.After(until) {
		return LimitsView{}, inputErr("You're already on a longer break (until %s). A break can't be shortened.", l.CoolOffUntil.In(s.zoneOf(org)).Format("Mon 2 Jan 2006 15:04"))
	}
	l.OrganizationID, l.UserID, l.CoolOffUntil, l.UpdatedAt = org, user, &until, s.Now()
	if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&l).Error; err != nil {
		return LimitsView{}, err
	}
	return s.LimitsView(org, user), nil
}

var _ = strings.TrimSpace
