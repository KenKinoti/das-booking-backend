package scout

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/crm"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

var jsonMarshal = json.Marshal

func countryName(code string) string { return currency.CountryName(code) }

// PursueResult is what Pursue created.
type PursueResult struct {
	Opportunity *Opportunity `json:"opportunity"`
	Kind        string       `json:"kind"` // deal | lead
	ID          string       `json:"id"`
	Link        string       `json:"link"` // in-app path of the CRM view
	Name        string       `json:"name"`
}

// DefaultPursueKind: a lead (a specific organisation to approach) becomes a
// CRM lead; everything else a deal in the pipeline.
func DefaultPursueKind(t string) string {
	if t == TypeLead {
		return "lead"
	}
	return "deal"
}

// crmNotes is the CRM record's notes: the scout's details and the source links.
func (s *Service) crmNotes(o *Opportunity, today time.Time) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...); b.WriteByte('\n') }
	w("Found by Market scout on %s (fit %d/100).", today.Format("2 Jan 2006"), o.Fit)
	where := []string{TypeLabel(o.Type)}
	if o.Country != "" {
		where = append(where, countryLabel(o.Country))
	}
	if o.Location != "" {
		where = append(where, o.Location)
	}
	w("%s", strings.Join(where, " · "))
	if o.Buyer != "" {
		w("Buyer / funder: %s", o.Buyer)
	}
	if o.EstValue != nil {
		w("Estimated value: %s", money(*o.EstValue, o.Currency))
	}
	if o.Deadline != nil {
		w("Deadline: %s", o.Deadline.Format("2 Jan 2006"))
	} else if o.DeadlineText != "" {
		w("Deadline: %s", o.DeadlineText)
	}
	if o.WhyItFits != "" {
		w("Why it fits: %s", o.WhyItFits)
	}
	if o.NextStep != "" {
		w("Next step: %s", o.NextStep)
	}
	if o.Notes != "" {
		w("Notes: %s", o.Notes)
	}
	w("Sources:")
	for _, src := range o.Sources {
		w("- %s — %s", src.Title, src.URL)
	}
	if s.AppURL != "" {
		w("Scout card: %s", s.link(CardPath(o.ID)))
	}
	return strings.TrimSpace(b.String())
}

// Pursue creates a CRM deal (or lead) from a card with its details and
// source links, and marks the card pursued. A card already pursued returns
// the record created then.
func (s *Service) Pursue(org, id, user, role, as string) (*PursueResult, error) {
	o, err := s.Get(org, id)
	if err != nil {
		return nil, err
	}
	if o.CRMID != "" {
		link := "/crm"
		if o.CRMKind == "lead" {
			link = "/crm?tab=leads"
		}
		return &PursueResult{Opportunity: o, Kind: o.CRMKind, ID: o.CRMID, Link: link, Name: o.Title}, nil
	}
	if as == "" {
		as = DefaultPursueKind(o.Type)
	}
	if as != "deal" && as != "lead" {
		return nil, bad("Pursue as a deal or a lead.")
	}
	if s.CRMAllowed != nil {
		if ok, why := s.CRMAllowed(org, user, role); !ok {
			if why == "" {
				why = "You can't add CRM records."
			}
			return nil, bad("%s Save the card or set a reminder instead.", why)
		}
	}
	loc := s.Loc(org)
	today := localDay(s.now(), loc)
	notes := s.crmNotes(o, today)
	value := 0.0
	if o.ValueHome != nil {
		value = *o.ValueHome
	}
	res := &PursueResult{Kind: as, Name: clip(o.Title, 255)}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		svc := crm.NewService(tx)
		if as == "lead" {
			who := o.Buyer
			if who == "" {
				who = clip(o.Title, 100)
			}
			rating := "cold"
			switch {
			case o.Fit >= 70:
				rating = "hot"
			case o.Fit >= 45:
				rating = "warm"
			}
			l := &crm.Lead{OrganizationID: org, FirstName: clip(who, 100), LastName: "", Company: clip(who, 255), Source: "market_scout", Status: "new",
				Rating: rating, EstimatedValue: value, Notes: notes}
			if err := svc.CreateLead(l); err != nil {
				return err
			}
			res.ID, res.Link = l.ID, "/crm?tab=leads"
		} else {
			d := &crm.Opportunity{OrganizationID: org, Name: clip(o.Title, 255), Description: clip(o.Summary, 2000), Value: value, Stage: "prospecting",
				Probability: crm.DefaultProbability["prospecting"], Company: clip(o.Buyer, 255), Notes: notes}
			if o.Deadline != nil {
				dl := *o.Deadline
				d.ExpectedCloseDate = &dl
			}
			if err := svc.CreateOpportunity(d); err != nil {
				return err
			}
			res.ID, res.Link = d.ID, "/crm"
		}
		now := s.now()
		return tx.Model(&Opportunity{}).Where("id = ?", id).Updates(map[string]any{"status": StatusPursued, "crm_kind": as, "crm_id": res.ID, "pursued_at": now, "pursued_by": user}).Error
	})
	if err != nil {
		return nil, err
	}
	res.Opportunity, _ = s.Get(org, id)
	return res, nil
}
