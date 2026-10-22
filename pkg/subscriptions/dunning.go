package subscriptions

import (
	"fmt"
	"sort"
	"time"
)

// Dunning: what happens to an unpaid invoice, day by day, relative to its
// due date D and its suspension date S (defaults; all editable per
// organisation in Subscriptions → Settings):
//
//	D−3, D, D+3      client reminders (the invoice's reminder email with the pay link)
//	S−3, S−1         admin alerts: "<client> will be suspended in 3 days / tomorrow" with
//	                 one-click actions (give more time, mark as paid, suspend now, don't suspend)
//	S−2              final notice to the client: "your service will be suspended on S"
//	S = D + grace    suspended (grace 7 days by default, per subscription; more time given
//	                 on the invoice moves S and everything before it)
//
// The administrators always hear about a suspension at least MinNoticeDays
// (3) before it happens: when no alert went out that early (an outage, an
// invoice created late, a shorter grace period), S waits until 3 days after
// the first alert. Only the latest reminder per audience that has been
// reached is sent — after an outage the client gets one email, not a burst —
// while the final notice and the suspension always happen once reached.
// With auto-suspend off, or "don't suspend this time" on the invoice, there
// is no S: the client reminders still go out and the digest lists it.

// Step kinds.
const (
	KindReminder    = "reminder"     // client
	KindAlert       = "alert"        // admins
	KindFinalNotice = "final_notice" // client
	KindSuspend     = "suspend"      // both
)

// Policy is when the dunning steps happen.
type Policy struct {
	ClientDays      []int // relative to the due date
	FinalNoticeDays int   // before suspension (0 = none)
	AdminDays       []int // before suspension
}

// PolicyOf is the organisation's policy.
func PolicyOf(st Settings) Policy {
	return Policy{ClientDays: st.ClientDays, FinalNoticeDays: st.FinalNoticeDays, AdminDays: st.AdminDays}
}

// Dates are the key dates of one unpaid invoice.
type Dates struct {
	Due         time.Time
	Planned     *time.Time // due + grace + extra days (nil: no suspension)
	Suspend     *time.Time // when it is suspended (nil: never — auto-suspend off / don't suspend this time)
	Deferred    bool       // the suspension waits for the admins' 3-day notice
	FinalNotice *time.Time
}

// DatesFor works out the suspension date. suspend is false when the invoice
// never suspends the service; warnedOn is the day the admins were first
// alerted about this invoice's suspension (nil = not yet).
func (p Policy) DatesFor(due time.Time, grace, extra int, suspend bool, warnedOn *time.Time, today time.Time) Dates {
	due, today = Day(due), Day(today)
	d := Dates{Due: due}
	if !suspend {
		return d
	}
	if grace < 0 {
		grace = 0
	}
	planned := due.AddDate(0, 0, grace+extra)
	if planned.Before(due) {
		planned = due
	}
	s := planned
	switch {
	case warnedOn != nil:
		if min := Day(*warnedOn).AddDate(0, 0, MinNoticeDays); s.Before(min) {
			s = min
		}
	case !today.Before(planned.AddDate(0, 0, -MinNoticeDays)):
		// The 3-day alert should have gone out already: it goes out today and
		// the suspension waits three days for it.
		if min := today.AddDate(0, 0, MinNoticeDays); s.Before(min) {
			s = min
		}
	}
	d.Planned, d.Suspend, d.Deferred = &planned, &s, s.After(planned)
	if p.FinalNoticeDays > 0 {
		f := s.AddDate(0, 0, -p.FinalNoticeDays)
		if !f.Before(due) && f.Before(s) {
			d.FinalNotice = &f
		}
	}
	return d
}

// Step is one dunning step.
type Step struct {
	Key      string    `json:"key"`      // client_-3 | admin_3 | final_notice | suspend
	Kind     string    `json:"kind"`     // reminder | alert | final_notice | suspend
	Audience string    `json:"audience"` // client | admin | both
	Date     time.Time `json:"date"`
	Offset   int       `json:"offset"` // days from the due date
	Before   int       `json:"before"` // days before suspension (alerts)
}

// EventKey is the key of a step's event for one invoice: steps that depend
// on the suspension date carry it, so they happen again when it moves.
func (st Step) EventKey(periodID string, d Dates) string {
	switch st.Kind {
	case KindAlert, KindFinalNotice, KindSuspend:
		if d.Suspend != nil {
			return periodID + "|" + st.Key + "|" + DayStr(*d.Suspend)
		}
	}
	return periodID + "|" + st.Key
}

// Steps lists every step for an unpaid invoice, in date order.
func (p Policy) Steps(d Dates) []Step {
	out := []Step{}
	seen := map[int]bool{}
	cd := append([]int{}, p.ClientDays...)
	sort.Ints(cd)
	for _, off := range cd {
		date := d.Due.AddDate(0, 0, off)
		if seen[off] || (d.Suspend != nil && !date.Before(*d.Suspend)) || (d.FinalNotice != nil && !date.Before(*d.FinalNotice)) {
			continue
		}
		seen[off] = true
		out = append(out, Step{Key: fmt.Sprintf("client_%d", off), Kind: KindReminder, Audience: ToClient, Date: date, Offset: off})
	}
	if d.Suspend != nil {
		s := *d.Suspend
		ad := append([]int{}, p.AdminDays...)
		sort.Sort(sort.Reverse(sort.IntSlice(ad)))
		seenA := map[int]bool{}
		for _, before := range ad {
			if before < 1 || seenA[before] {
				continue
			}
			seenA[before] = true
			date := s.AddDate(0, 0, -before)
			out = append(out, Step{Key: fmt.Sprintf("admin_%d", before), Kind: KindAlert, Audience: ToAdmin, Date: date, Offset: DaysBetween(d.Due, date), Before: before})
		}
		if d.FinalNotice != nil {
			out = append(out, Step{Key: "final_notice", Kind: KindFinalNotice, Audience: ToClient, Date: *d.FinalNotice, Offset: DaysBetween(d.Due, *d.FinalNotice),
				Before: DaysBetween(*d.FinalNotice, s)})
		}
		out = append(out, Step{Key: "suspend", Kind: KindSuspend, Audience: "both", Date: s, Offset: DaysBetween(d.Due, s)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

// DuePlan is what the engine does today for one unpaid invoice.
type DuePlan struct {
	Client      *Step  // latest client reminder reached
	Admin       *Step  // latest admin alert reached
	FinalNotice *Step  // the final notice, when reached
	Suspend     *Step  // the suspension, when reached
	Stale       []Step // reminders / alerts superseded by a later one
}

// Due decides the steps for today. suspended says the service is already
// suspended (nothing but the state change is left, and that has happened).
func (p Policy) Due(d Dates, today time.Time, suspended bool) DuePlan {
	today = Day(today)
	out := DuePlan{}
	if suspended {
		return out
	}
	for _, s := range p.Steps(d) {
		if s.Date.After(today) {
			break
		}
		st := s
		switch s.Kind {
		case KindReminder:
			if out.Client != nil {
				out.Stale = append(out.Stale, *out.Client)
			}
			out.Client = &st
		case KindAlert:
			if out.Admin != nil {
				out.Stale = append(out.Stale, *out.Admin)
			}
			out.Admin = &st
		case KindFinalNotice:
			out.FinalNotice = &st
		case KindSuspend:
			out.Suspend = &st
		}
	}
	// The final notice supersedes the plain reminders; on the suspension day
	// reminders, alerts and the notice are over.
	if out.FinalNotice != nil && out.Client != nil {
		out.Stale = append(out.Stale, *out.Client)
		out.Client = nil
	}
	if out.Suspend != nil {
		for _, x := range []**Step{&out.Client, &out.Admin, &out.FinalNotice} {
			if *x != nil {
				out.Stale = append(out.Stale, **x)
				*x = nil
			}
		}
	}
	return out
}

// OpenPeriod is the oldest unpaid invoice of a subscription with its dunning dates.
type OpenPeriod struct {
	PeriodID      string  `json:"period_id"`
	InvoiceID     string  `json:"invoice_id"`
	InvoiceNumber string  `json:"invoice_number"`
	DueDate       string  `json:"due_date"`
	Balance       float64 `json:"balance"`
	Currency      string  `json:"currency"`
	DaysOverdue   int     `json:"days_overdue"`
	SuspendOn     string  `json:"suspend_on,omitempty"` // "" = it won't be suspended
	PlannedOn     string  `json:"planned_on,omitempty"`
	Deferred      bool    `json:"deferred,omitempty"`
	DaysToSuspend *int    `json:"days_to_suspend,omitempty"`
	FinalNoticeOn string  `json:"final_notice_on,omitempty"`
	ExtraDays     int     `json:"extra_days"`
	NoSuspend     bool    `json:"no_suspend"`
	Sent          bool    `json:"sent"`
	Draft         bool    `json:"draft"`
	Periods       int     `json:"periods"` // unpaid invoices in all
}

// StepLabel is a step in words.
func StepLabel(s Step) string {
	switch s.Kind {
	case KindReminder:
		switch {
		case s.Offset < 0:
			return fmt.Sprintf("Reminder to the client (%d day%s before due)", -s.Offset, plural(-s.Offset))
		case s.Offset == 0:
			return "Reminder to the client (due today)"
		}
		return fmt.Sprintf("Reminder to the client (%d day%s overdue)", s.Offset, plural(s.Offset))
	case KindAlert:
		if s.Before == 1 {
			return "Admin alert: suspended tomorrow"
		}
		return fmt.Sprintf("Admin alert: suspended in %d days", s.Before)
	case KindFinalNotice:
		return "Final notice to the client: service will be suspended"
	case KindSuspend:
		return "Service suspended"
	}
	return s.Key
}

func plural(n int) string {
	if n == 1 || n == -1 {
		return ""
	}
	return "s"
}
