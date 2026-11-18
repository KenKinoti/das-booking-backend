package brief

import (
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/subscriptions"
)

// sources are the other modules' services the built-in sections read, all on
// the brief's clock (so the test clock moves them too).
type sources struct {
	exp   *expenses.Service
	bills *recurbills.Service
	subs  *subscriptions.Service
	ins   *insights.Service
}

func (s *Service) sources() *sources {
	s.srcOnce.Do(func() {
		exp := expenses.NewService(s.DB, s.FX)
		exp.Now = s.now
		bills := recurbills.New(s.DB, s.FX, exp)
		bills.Now = s.now
		subs := subscriptions.New(s.DB, s.FX, exp)
		subs.Now = s.now
		ins := insights.New(s.DB, s.FX, exp, bills, nil)
		ins.Now = s.now
		s.src = &sources{exp: exp, bills: bills, subs: subs, ins: ins}
	})
	return s.src
}
