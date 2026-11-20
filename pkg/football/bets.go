package football

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// BetInput is a bet to record or change.
type BetInput struct {
	FixtureID int64      `json:"fixture_id"`
	Bookmaker string     `json:"bookmaker"`
	Market    string     `json:"market"`
	Selection string     `json:"selection"`
	Line      *float64   `json:"line"`
	Odds      float64    `json:"odds"`
	Stake     float64    `json:"stake"`
	PlacedAt  *time.Time `json:"placed_at"`
	Notes     string     `json:"notes"`
	Tags      []string   `json:"tags"`
}

// BetView is a bet with its match.
type BetView struct {
	Bet
	Label           string   `json:"label"`
	MarketLabel     string   `json:"market_label"`
	Home            TeamRef  `json:"home"`
	Away            TeamRef  `json:"away"`
	Competition     string   `json:"competition"`
	Status90        string   `json:"fixture_status"`
	Score           ScoreOut `json:"score"`
	InPlay          bool     `json:"in_play_bet"`
	PotentialReturn float64  `json:"potential_return"`
}

func (s *Service) currencyOf(org string) string {
	if s.OrgCurrency != nil && org != "" {
		if c := strings.ToUpper(strings.TrimSpace(s.OrgCurrency(org))); len(c) == 3 {
			return c
		}
	}
	return "KES"
}

func (s *Service) zoneOf(org string) *time.Location {
	if s.OrgZone != nil && org != "" {
		if l := s.OrgZone(org); l != nil {
			return l
		}
	}
	l, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		return time.UTC
	}
	return l
}

func validateBet(in *BetInput) error {
	in.Market = strings.ToLower(strings.TrimSpace(in.Market))
	in.Selection = strings.ToLower(strings.TrimSpace(in.Selection))
	in.Bookmaker = strings.TrimSpace(in.Bookmaker)
	sels, ok := MarketSelections[in.Market]
	if !ok {
		return inputErr("Choose a market: 1x2, dc (double chance), dnb (draw no bet), ou (over/under), btts or ah (Asian handicap).")
	}
	found := false
	for _, x := range sels {
		if x == in.Selection {
			found = true
		}
	}
	if !found {
		return inputErr("For %s pick one of: %s.", MarketLabels[in.Market], strings.Join(sels, ", "))
	}
	if NeedsLine(in.Market) {
		if in.Line == nil || !ValidLine(in.Market, *in.Line) {
			if in.Market == MktOU {
				return inputErr("Give the goals line, e.g. 2.5 (quarter lines like 2.25 are fine).")
			}
			return inputErr("Give the handicap line for your team, e.g. -0.5 or +0.25 (between -5 and +5, steps of 0.25).")
		}
	} else {
		in.Line = nil
	}
	if in.Odds < 1.01 || in.Odds > 1000 || math.IsNaN(in.Odds) {
		return inputErr("Odds must be decimal odds between 1.01 and 1000 (e.g. 2.10).")
	}
	if in.Stake <= 0 || in.Stake > 100000000 || math.IsNaN(in.Stake) {
		return inputErr("Stake must be more than 0.")
	}
	in.Stake = round2(in.Stake)
	in.Odds = math.Round(in.Odds*1000) / 1000
	if in.Bookmaker == "" {
		return inputErr("Which bookmaker did you place it with? (e.g. SportPesa, Betika, Odibets)")
	}
	if len(in.Bookmaker) > 80 {
		return inputErr("Bookmaker name is too long.")
	}
	if len(in.Notes) > 2000 {
		return inputErr("Notes are too long (2,000 characters at most).")
	}
	tags := []string{}
	for _, t := range in.Tags {
		if t = strings.TrimSpace(t); t != "" && len(tags) < 10 {
			tags = append(tags, clip(t, 30))
		}
	}
	in.Tags = tags
	return nil
}

// CreateBet records a bet after checking the person's limits and break.
func (s *Service) CreateBet(org, user string, in BetInput) (*BetView, error) {
	if err := validateBet(&in); err != nil {
		return nil, err
	}
	var f Fixture
	if s.DB.Where("id = ?", in.FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
		return nil, inputErr("That match isn't in DASYIN's fixtures. Pick it from the fixtures list.")
	}
	now := s.Now()
	placed := now
	if in.PlacedAt != nil {
		placed = in.PlacedAt.UTC()
		if placed.After(now.Add(5 * time.Minute)) {
			return nil, inputErr("The time you placed the bet can't be in the future.")
		}
		if now.Sub(placed) > 30*24*time.Hour {
			return nil, inputErr("Bets older than 30 days can't be recorded.")
		}
	}
	if IsFinal(f.Status) && f.FinishedAt != nil && !placed.Before(f.Kickoff.Add(2*time.Hour)) {
		return nil, inputErr("This match has finished — bets placed after the end can't be recorded.")
	}
	if err := s.CheckLimits(org, user, in.Stake, placed); err != nil {
		return nil, err
	}
	b := Bet{ID: uuid.NewString(), OrganizationID: org, UserID: user, FixtureID: f.ID, CompetitionID: f.CompetitionID, Kickoff: f.Kickoff,
		Bookmaker: in.Bookmaker, Market: in.Market, Selection: in.Selection, Line: in.Line, Odds: in.Odds, Stake: in.Stake, Currency: s.currencyOf(org),
		PlacedAt: placed, Status: BetOpen, Notes: strings.TrimSpace(in.Notes), Tags: in.Tags, CreatedAt: now, UpdatedAt: now}
	// The model's and the market's view when the bet was placed (pre-match bets).
	if placed.Before(f.Kickoff) {
		if p, ok := s.CurrentPrediction(f); ok {
			if W, L, _, _, ok := modelWL(p, b.Market, b.Selection, b.Line); ok {
				b.ModelP = ptr(round4(EffP(W, L)))
			}
		}
		mv := s.marketView(f, nil, "")
		for _, so := range mv.Selections {
			if so.Market == b.Market && so.Selection == b.Selection && lineKey(so.Line) == lineKey(b.Line) && so.FairP != nil {
				b.MarketP = ptr(round4(*so.FairP))
			}
		}
	}
	if err := s.DB.Create(&b).Error; err != nil {
		return nil, err
	}
	// Already played: settle straight away.
	if IsFinal(f.Status) || voidStatuses[f.Status] {
		s.settleOne(&b, f)
	}
	if !f.Kickoff.After(now) {
		s.closingFor(&b, f)
	}
	return s.BetView(org, user, b.ID)
}

// BetView loads one of the person's bets.
func (s *Service) BetView(org, user, id string) (*BetView, error) {
	var b Bet
	if s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Limit(1).Find(&b).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	v := s.view(b)
	return &v, nil
}

func (s *Service) view(b Bet) BetView {
	var f Fixture
	s.DB.Where("id = ?", b.FixtureID).Limit(1).Find(&f)
	h, a := s.teamRef(f.HomeID), s.teamRef(f.AwayID)
	v := BetView{Bet: b, Label: SelectionLabel(b.Market, b.Selection, b.Line, h.Name, a.Name), MarketLabel: MarketLabels[b.Market], Home: h, Away: a,
		Competition: s.compName(f.CompetitionID), Status90: f.Status, Score: scoreOf(f), InPlay: !b.PlacedAt.Before(b.Kickoff), PotentialReturn: round2(b.Stake * b.Odds)}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	return v
}

// BetFilter filters the list.
type BetFilter struct {
	Status      string // open | settled | all | a status
	Market      string
	Bookmaker   string
	Competition int64
	From, To    *time.Time
	Limit       int
	Offset      int
}

// Bets lists the person's bets, newest first.
func (s *Service) Bets(org, user string, fl BetFilter) ([]BetView, int64) {
	q := s.DB.Model(&Bet{}).Where("organization_id = ? AND user_id = ?", org, user)
	switch fl.Status {
	case "", "all":
	case "settled":
		q = q.Where("status <> ?", BetOpen)
	default:
		q = q.Where("status = ?", fl.Status)
	}
	if fl.Market != "" {
		q = q.Where("market = ?", fl.Market)
	}
	if fl.Bookmaker != "" {
		q = q.Where("LOWER(bookmaker) = ?", strings.ToLower(fl.Bookmaker))
	}
	if fl.Competition != 0 {
		q = q.Where("competition_id = ?", fl.Competition)
	}
	if fl.From != nil {
		q = q.Where("placed_at >= ?", *fl.From)
	}
	if fl.To != nil {
		q = q.Where("placed_at < ?", *fl.To)
	}
	var n int64
	q.Count(&n)
	if fl.Limit <= 0 || fl.Limit > 500 {
		fl.Limit = 100
	}
	var bs []Bet
	q.Order("placed_at DESC, created_at DESC").Limit(fl.Limit).Offset(fl.Offset).Find(&bs)
	out := []BetView{}
	for _, b := range bs {
		out = append(out, s.view(b))
	}
	return out, n
}

// UpdateBet changes a bet. Settled bets keep their numbers; changing the
// odds, stake or selection of a settled bet re-settles it from the result.
func (s *Service) UpdateBet(org, user, id string, in BetInput) (*BetView, error) {
	var b Bet
	if s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Limit(1).Find(&b).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	in.FixtureID = b.FixtureID
	if in.Market == "" {
		in.Market = b.Market
	}
	if in.Selection == "" {
		in.Selection = b.Selection
	}
	if in.Line == nil {
		in.Line = b.Line
	}
	if in.Odds == 0 {
		in.Odds = b.Odds
	}
	if in.Stake == 0 {
		in.Stake = b.Stake
	}
	if in.Bookmaker == "" {
		in.Bookmaker = b.Bookmaker
	}
	if in.Tags == nil {
		in.Tags = b.Tags
	}
	if err := validateBet(&in); err != nil {
		return nil, err
	}
	if in.Stake > b.Stake {
		if err := s.CheckLimits(org, user, in.Stake-b.Stake, b.PlacedAt); err != nil {
			return nil, err
		}
	}
	up := map[string]any{"bookmaker": in.Bookmaker, "market": in.Market, "selection": in.Selection, "line": in.Line, "odds": in.Odds, "stake": in.Stake,
		"notes": strings.TrimSpace(in.Notes), "tags": mustJSON(in.Tags), "updated_at": s.Now()}
	if in.PlacedAt != nil && !in.PlacedAt.After(s.Now()) {
		up["placed_at"] = in.PlacedAt.UTC()
	}
	if err := s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(up).Error; err != nil {
		return nil, err
	}
	s.DB.Where("id = ?", b.ID).Limit(1).Find(&b)
	if b.Status != BetOpen && b.SettledBy == "auto" || b.Status == BetOpen {
		var f Fixture
		if s.DB.Where("id = ?", b.FixtureID).Limit(1).Find(&f).RowsAffected > 0 && (IsFinal(f.Status) || voidStatuses[f.Status]) {
			s.settleOne(&b, f)
		}
	}
	if b.ClosingOdds != nil {
		s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(map[string]any{"clv": round4(b.Odds / *b.ClosingOdds - 1)})
	}
	return s.BetView(org, user, b.ID)
}

// DeleteBet removes a bet.
func (s *Service) DeleteBet(org, user, id string) error {
	res := s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Delete(&Bet{})
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return res.Error
}

// SettleInput is a manual settlement.
type SettleInput struct {
	Status  string   `json:"status"` // won | lost | void | half_won | half_lost | cashed_out | open | auto
	CashOut *float64 `json:"cash_out"`
}

// SettleBet settles a bet by hand (e.g. the bookmaker voided it, or it was
// cashed out), "auto" re-settles it from the result, "open" reopens it.
func (s *Service) SettleBet(org, user, id string, in SettleInput) (*BetView, error) {
	var b Bet
	if s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Limit(1).Find(&b).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	now := s.Now()
	st := strings.ToLower(strings.TrimSpace(in.Status))
	switch st {
	case "auto":
		var f Fixture
		s.DB.Where("id = ?", b.FixtureID).Limit(1).Find(&f)
		if !IsFinal(f.Status) && !voidStatuses[f.Status] {
			return nil, inputErr("The result isn't in yet — it will settle by itself after full time.")
		}
		s.settleOne(&b, f)
	case BetOpen:
		s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(map[string]any{"status": BetOpen, "settled_at": nil, "settled_by": "", "return": 0, "pnl": 0, "cash_out": nil, "updated_at": now})
	case BetCashedOut:
		if in.CashOut == nil || *in.CashOut < 0 || *in.CashOut > b.Stake*b.Odds*1.0001 {
			return nil, inputErr("Give the cash-out amount you received (between 0 and the potential return).")
		}
		co := round2(*in.CashOut)
		s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(map[string]any{"status": BetCashedOut, "cash_out": co, "return": co, "pnl": round2(co - b.Stake),
			"settled_at": now, "settled_by": "manual", "updated_at": now})
	case BetWon, BetLost, BetVoid, BetHalfWon, BetHalfLost:
		ret, pnl := Payout(st, b.Odds, b.Stake)
		s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(map[string]any{"status": st, "return": ret, "pnl": pnl, "cash_out": nil, "settled_at": now, "settled_by": "manual", "updated_at": now})
	default:
		return nil, inputErr("Status must be won, lost, void, half_won, half_lost, cashed_out, open or auto.")
	}
	return s.BetView(org, user, b.ID)
}

// settleOne settles a bet from its fixture's 90-minute result (void when
// the match was cancelled or abandoned).
func (s *Service) settleOne(b *Bet, f Fixture) bool {
	now := s.Now()
	var st string
	switch {
	case voidStatuses[f.Status]:
		st = BetVoid
	case IsFinal(f.Status) && f.FTHome != nil && f.FTAway != nil:
		st = Settle(b.Market, b.Selection, b.Line, *f.FTHome, *f.FTAway)
	}
	if st == "" {
		return false
	}
	ret, pnl := Payout(st, b.Odds, b.Stake)
	err := s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(map[string]any{"status": st, "return": ret, "pnl": pnl, "cash_out": nil,
		"settled_at": now, "settled_by": "auto", "updated_at": now}).Error
	if err == nil {
		b.Status, b.Return, b.PnL, b.SettledBy = st, ret, pnl, "auto"
	}
	return err == nil
}

// AutoSettle settles open bets whose match has a result.
func (s *Service) AutoSettle() int {
	var bs []Bet
	s.DB.Raw(`SELECT b.* FROM fb_bets b JOIN fb_fixtures f ON f.id = b.fixture_id
		WHERE b.status = 'open' AND (f.status IN ('FT','AET','PEN') AND f.ft_home IS NOT NULL OR f.status IN ('CANC','ABD','AWD','WO'))`).Scan(&bs)
	n := 0
	for i := range bs {
		var f Fixture
		if s.DB.Where("id = ?", bs[i].FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
			continue
		}
		if s.settleOne(&bs[i], f) {
			n++
		}
	}
	return n
}

// closingFor sets a bet's closing odds and CLV: the sharp bookmaker's last
// price before kickoff (Pinnacle first), else the median of the bookmakers'
// last prices; CLVFair uses the de-margined closing probability.
func (s *Service) closingFor(b *Bet, f Fixture) bool {
	if !b.PlacedAt.Before(f.Kickoff) {
		return false // in-play bets have no pre-match closing line
	}
	ko := f.Kickoff
	mv := s.marketView(f, &ko, "")
	for _, so := range mv.Selections {
		if so.Market != b.Market || so.Selection != b.Selection || lineKey(so.Line) != lineKey(b.Line) {
			continue
		}
		var price float64
		src := ""
		for _, sb := range SharpBooks {
			for _, p := range so.Prices {
				if p.Bookmaker == sb && !strings.HasPrefix(p.Source, "manual") {
					price, src = p.Price, sb
					break
				}
			}
			if price > 0 {
				break
			}
		}
		if price == 0 {
			xs := []float64{}
			for _, p := range so.Prices {
				if !strings.HasPrefix(p.Source, "manual") {
					xs = append(xs, p.Price)
				}
			}
			if len(xs) == 0 {
				return false
			}
			price, src = median(xs), "consensus of "+itoa(len(xs))+" bookmakers"
		}
		up := map[string]any{"closing_odds": price, "closing_source": src, "clv": round4(b.Odds/price - 1)}
		if so.FairP != nil {
			up["closing_fair_p"] = round4(*so.FairP)
			up["clv_fair"] = round4(b.Odds**so.FairP - 1)
		}
		return s.DB.Model(&Bet{}).Where("id = ?", b.ID).Updates(up).Error == nil
	}
	return false
}

func (s *Service) captureBetClosing() int {
	now := s.Now()
	var bs []Bet
	s.DB.Where("closing_odds IS NULL AND kickoff <= ? AND kickoff > ? AND placed_at < kickoff", now, now.Add(-7*24*time.Hour)).Find(&bs)
	n := 0
	cache := map[int64]Fixture{}
	for i := range bs {
		f, ok := cache[bs[i].FixtureID]
		if !ok {
			if s.DB.Where("id = ?", bs[i].FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
				continue
			}
			cache[f.ID] = f
		}
		if s.closingFor(&bs[i], f) {
			n++
		}
	}
	return n
}

// KellyOut is the model's view of one selection at the odds offered.
type KellyOut struct {
	Label        string   `json:"label"`
	Odds         float64  `json:"odds"`
	ModelP       float64  `json:"model_p"`
	ModelPush    float64  `json:"model_push"`
	FairOdds     float64  `json:"model_fair_odds"`
	MarketP      *float64 `json:"market_p"`
	MarketSource string   `json:"market_source"`
	Edge         *float64 `json:"edge"`
	EV           float64  `json:"ev"`
	Fraction     float64  `json:"kelly_fraction"` // of the bankroll, ¼ Kelly capped
	Stake        *float64 `json:"kelly_stake"`
	Bankroll     *float64 `json:"bankroll"`
	Currency     string   `json:"currency"`
	Value        bool     `json:"value"`
	Note         string   `json:"note"`
}

// KellyFor gives the model's probability, EV and the fractional Kelly stake
// for a selection at given odds (e.g. a price seen on a Kenyan app).
func (s *Service) KellyFor(org, user string, fid int64, market, sel string, line *float64, odds float64) (*KellyOut, error) {
	in := BetInput{FixtureID: fid, Market: market, Selection: sel, Line: line, Odds: odds, Stake: 1, Bookmaker: "-"}
	if err := validateBet(&in); err != nil {
		return nil, err
	}
	var f Fixture
	if s.DB.Where("id = ?", fid).Limit(1).Find(&f).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	p, ok := s.CurrentPrediction(f)
	if !ok {
		return nil, inputErr("No model estimate for this match yet.")
	}
	W, L, V, _, ok := modelWL(p, in.Market, in.Selection, in.Line)
	if !ok {
		return nil, inputErr("The model has no estimate for this market (not enough data for goal markets in this competition).")
	}
	st, _ := s.Settings()
	pe := EffP(W, L)
	k := &KellyOut{Label: SelectionLabel(in.Market, in.Selection, in.Line, s.teamName(f.HomeID), s.teamName(f.AwayID)), Odds: in.Odds, ModelP: round4(pe),
		ModelPush: round4(V), EV: round4(EVOf(W, L, in.Odds)), Fraction: round4(model.Kelly(pe, in.Odds, st.KellyFraction, st.KellyCap)), Currency: s.currencyOf(org),
		Note: fmt.Sprintf("Fractional Kelly (%.0f%% of full Kelly, at most %.0f%% of your bankroll). An estimate, not a promise: never chase losses.", st.KellyFraction*100, st.KellyCap*100)}
	if pe > 0 {
		k.FairOdds = math.Round(1/pe*1000) / 1000
	}
	mv := s.marketView(f, nil, "")
	for _, so := range mv.Selections {
		if so.Market == in.Market && so.Selection == in.Selection && lineKey(so.Line) == lineKey(in.Line) && so.FairP != nil {
			k.MarketP, k.MarketSource = ptr(round4(*so.FairP)), so.FairSource
			k.Edge = ptr(round4(pe - *so.FairP))
		}
	}
	k.Value = k.EV > 0 && (k.Edge == nil || *k.Edge >= st.MinEdge)
	if bal := s.bankrollBalance(org, user); bal != nil {
		k.Bankroll = bal
		stake := math.Floor(k.Fraction**bal*100) / 100
		k.Stake = &stake
	}
	return k, nil
}

// ---------------------------------------------------------------- summary

// Breakdown is P&L for one market / bookmaker / competition / month.
type Breakdown struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Bets       int      `json:"bets"`
	Settled    int      `json:"settled"`
	Staked     float64  `json:"staked"`
	PnL        float64  `json:"pnl"`
	Yield      float64  `json:"yield"`
	StrikeRate float64  `json:"strike_rate"`
	AvgOdds    float64  `json:"avg_odds"`
	AvgCLV     *float64 `json:"avg_clv"`
}

// BankPoint is the bankroll after an event.
type BankPoint struct {
	At      time.Time `json:"at"`
	Balance float64   `json:"balance"`
	Event   string    `json:"event"` // start | deposit | withdrawal | bet
	Ref     string    `json:"ref,omitempty"`
	Change  float64   `json:"change"`
}

// Summary is GET /bets/summary.
type Summary struct {
	Currency      string      `json:"currency"`
	Bets          int         `json:"bets"`
	Open          int         `json:"open"`
	OpenStake     float64     `json:"open_stake"`
	Settled       int         `json:"settled"`
	Won           int         `json:"won"`
	Lost          int         `json:"lost"`
	Void          int         `json:"void"`
	HalfWon       int         `json:"half_won"`
	HalfLost      int         `json:"half_lost"`
	CashedOut     int         `json:"cashed_out"`
	Staked        float64     `json:"staked"`   // settled bets
	Returned      float64     `json:"returned"` // settled bets
	PnL           float64     `json:"pnl"`
	Yield         float64     `json:"yield"`       // P&L / staked (settled)
	ROI           *float64    `json:"roi"`         // P&L / money put in (starting bankroll + deposits)
	StrikeRate    float64     `json:"strike_rate"` // (won + half won) / settled excl. void
	AvgOdds       float64     `json:"avg_odds"`
	AvgStake      float64     `json:"avg_stake"`
	AvgCLV        *float64    `json:"avg_clv"`
	AvgCLVFair    *float64    `json:"avg_clv_fair"`
	BeatClosing   *float64    `json:"beat_closing_rate"`
	CLVBets       int         `json:"clv_bets"`
	ExpectedPnL   *float64    `json:"model_expected_pnl"` // Σ stake × (model p × odds − 1) where the model had a view
	ByMarket      []Breakdown `json:"by_market"`
	ByBookmaker   []Breakdown `json:"by_bookmaker"`
	ByCompetition []Breakdown `json:"by_competition"`
	ByMonth       []Breakdown `json:"by_month"`
	Bankroll      struct {
		Start          float64     `json:"start"`
		Deposits       float64     `json:"deposits"`
		Withdrawals    float64     `json:"withdrawals"`
		Balance        *float64    `json:"balance"`   // start + deposits − withdrawals + P&L
		Available      *float64    `json:"available"` // balance − open stakes
		Series         []BankPoint `json:"series"`
		MaxDrawdown    float64     `json:"max_drawdown"`
		MaxDrawdownPct float64     `json:"max_drawdown_pct"`
		Drawdown       float64     `json:"current_drawdown"`
		Peak           float64     `json:"peak"`
		Set            bool        `json:"set"`
	} `json:"bankroll"`
	Limits LimitsView `json:"limits"`
	From   *time.Time `json:"from"`
	To     *time.Time `json:"to"`
	Note   string     `json:"note"`
}

// BetSummary computes the person's numbers (all time or a placed_at range).
func (s *Service) BetSummary(org, user string, from, to *time.Time) Summary {
	q := s.DB.Where("organization_id = ? AND user_id = ?", org, user)
	if from != nil {
		q = q.Where("placed_at >= ?", *from)
	}
	if to != nil {
		q = q.Where("placed_at < ?", *to)
	}
	var bs []Bet
	q.Order("placed_at").Find(&bs)
	sm := Summary{Currency: s.currencyOf(org), Bets: len(bs), From: from, To: to,
		Note: "Yield is profit ÷ amount staked on settled bets; ROI is profit ÷ money put in (starting bankroll + deposits). CLV compares your odds with the closing price (Pinnacle when available, otherwise the bookmakers' median): beating the close consistently is the best sign of skill."}
	type acc struct {
		label                  string
		n, settled, wins, nonv int
		staked, pnl, odds      float64
		clv                    float64
		nclv                   int
	}
	groups := map[string]map[string]*acc{"market": {}, "bookmaker": {}, "competition": {}, "month": {}}
	add := func(g, k, label string, b Bet) {
		a := groups[g][k]
		if a == nil {
			a = &acc{label: label}
			groups[g][k] = a
		}
		a.n++
		if b.Status == BetOpen {
			return
		}
		a.settled++
		a.staked += b.Stake
		a.pnl += b.PnL
		if b.Status != BetVoid {
			a.nonv++
			a.odds += b.Odds
			if b.Status == BetWon || b.Status == BetHalfWon {
				a.wins++
			}
		}
		if b.CLV != nil {
			a.clv += *b.CLV
			a.nclv++
		}
	}
	var oddsSum, clvSum, clvFairSum, expSum float64
	nOdds, nClvFair, beat, nExp := 0, 0, 0, 0
	loc := s.zoneOf(org)
	for _, b := range bs {
		add("market", b.Market, MarketLabels[b.Market], b)
		bk := strings.ToLower(strings.TrimSpace(b.Bookmaker))
		add("bookmaker", bk, b.Bookmaker, b)
		add("competition", tid(b.CompetitionID), s.compName(b.CompetitionID), b)
		add("month", b.PlacedAt.In(loc).Format("2006-01"), b.PlacedAt.In(loc).Format("Jan 2006"), b)
		if b.Status == BetOpen {
			sm.Open++
			sm.OpenStake += b.Stake
			continue
		}
		sm.Settled++
		sm.Staked += b.Stake
		sm.Returned += b.Return
		sm.PnL += b.PnL
		switch b.Status {
		case BetWon:
			sm.Won++
		case BetLost:
			sm.Lost++
		case BetVoid:
			sm.Void++
		case BetHalfWon:
			sm.HalfWon++
		case BetHalfLost:
			sm.HalfLost++
		case BetCashedOut:
			sm.CashedOut++
		}
		if b.Status != BetVoid {
			oddsSum += b.Odds
			nOdds++
		}
		if b.CLV != nil {
			clvSum += *b.CLV
			sm.CLVBets++
			if *b.CLV > 0 {
				beat++
			}
		}
		if b.CLVFair != nil {
			clvFairSum += *b.CLVFair
			nClvFair++
		}
		if b.ModelP != nil {
			expSum += b.Stake * (*b.ModelP*b.Odds - 1)
			nExp++
		}
	}
	sm.OpenStake, sm.Staked, sm.Returned, sm.PnL = round2(sm.OpenStake), round2(sm.Staked), round2(sm.Returned), round2(sm.PnL)
	if sm.Staked > 0 {
		sm.Yield = round4(sm.PnL / sm.Staked)
	}
	if d := sm.Settled - sm.Void; d > 0 {
		sm.StrikeRate = round4(float64(sm.Won+sm.HalfWon) / float64(d))
	}
	if nOdds > 0 {
		sm.AvgOdds = round4(oddsSum / float64(nOdds))
	}
	if sm.Bets > 0 {
		var tot float64
		for _, b := range bs {
			tot += b.Stake
		}
		sm.AvgStake = round2(tot / float64(sm.Bets))
	}
	if sm.CLVBets > 0 {
		sm.AvgCLV = ptr(round4(clvSum / float64(sm.CLVBets)))
		sm.BeatClosing = ptr(round4(float64(beat) / float64(sm.CLVBets)))
	}
	if nClvFair > 0 {
		sm.AvgCLVFair = ptr(round4(clvFairSum / float64(nClvFair)))
	}
	if nExp > 0 {
		sm.ExpectedPnL = ptr(round2(expSum))
	}
	conv := func(g string) []Breakdown {
		out := []Breakdown{}
		for k, a := range groups[g] {
			bd := Breakdown{Key: k, Label: a.label, Bets: a.n, Settled: a.settled, Staked: round2(a.staked), PnL: round2(a.pnl)}
			if a.staked > 0 {
				bd.Yield = round4(a.pnl / a.staked)
			}
			if a.nonv > 0 {
				bd.StrikeRate = round4(float64(a.wins) / float64(a.nonv))
				bd.AvgOdds = round4(a.odds / float64(a.nonv))
			}
			if a.nclv > 0 {
				bd.AvgCLV = ptr(round4(a.clv / float64(a.nclv)))
			}
			out = append(out, bd)
		}
		sort.Slice(out, func(i, j int) bool {
			if g == "month" {
				return out[i].Key < out[j].Key
			}
			if out[i].Bets != out[j].Bets {
				return out[i].Bets > out[j].Bets
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	sm.ByMarket, sm.ByBookmaker, sm.ByCompetition, sm.ByMonth = conv("market"), conv("bookmaker"), conv("competition"), conv("month")
	// Bankroll series: money in / out and settled bets in time order.
	var entries []BankrollEntry
	s.DB.Where("organization_id = ? AND user_id = ?", org, user).Order("at, created_at").Find(&entries)
	type ev struct {
		at     time.Time
		change float64
		kind   string
		ref    string
	}
	evs := []ev{}
	for _, e := range entries {
		switch e.Kind {
		case "start":
			sm.Bankroll.Start += e.Amount
			evs = append(evs, ev{e.At, e.Amount, "start", e.ID})
		case "deposit":
			sm.Bankroll.Deposits += e.Amount
			evs = append(evs, ev{e.At, e.Amount, "deposit", e.ID})
		case "withdrawal":
			sm.Bankroll.Withdrawals += e.Amount
			evs = append(evs, ev{e.At, -e.Amount, "withdrawal", e.ID})
		}
	}
	// All settled bets (not only the date range) move the bankroll.
	var all []Bet
	s.DB.Where("organization_id = ? AND user_id = ? AND status <> ?", org, user, BetOpen).Order("settled_at, placed_at, created_at, id").Find(&all)
	var allPnL float64
	for _, b := range all {
		at := b.PlacedAt
		if b.SettledAt != nil {
			at = *b.SettledAt
		}
		evs = append(evs, ev{at, b.PnL, "bet", b.ID})
		allPnL += b.PnL
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })
	sm.Bankroll.Set = len(entries) > 0
	bal, peak := 0.0, math.Inf(-1)
	sm.Bankroll.Series = []BankPoint{}
	for _, e := range evs {
		bal += e.change
		sm.Bankroll.Series = append(sm.Bankroll.Series, BankPoint{At: e.at, Balance: round2(bal), Event: e.kind, Ref: e.ref, Change: round2(e.change)})
		if e.kind == "withdrawal" || e.kind == "deposit" || e.kind == "start" {
			// Money moving in or out is not a drawdown: rebase the peak.
			if bal > peak || e.kind != "bet" {
				peak = bal
			}
			continue
		}
		if bal > peak {
			peak = bal
		}
		if dd := peak - bal; dd > sm.Bankroll.MaxDrawdown {
			sm.Bankroll.MaxDrawdown = round2(dd)
			if peak > 0 {
				sm.Bankroll.MaxDrawdownPct = round4(dd / peak)
			}
		}
	}
	if !math.IsInf(peak, -1) {
		sm.Bankroll.Peak = round2(peak)
		sm.Bankroll.Drawdown = round2(peak - bal)
	}
	if len(sm.Bankroll.Series) > 400 {
		sm.Bankroll.Series = sm.Bankroll.Series[len(sm.Bankroll.Series)-400:]
	}
	sm.Bankroll.Start, sm.Bankroll.Deposits, sm.Bankroll.Withdrawals = round2(sm.Bankroll.Start), round2(sm.Bankroll.Deposits), round2(sm.Bankroll.Withdrawals)
	if sm.Bankroll.Set {
		b := round2(sm.Bankroll.Start + sm.Bankroll.Deposits - sm.Bankroll.Withdrawals + allPnL)
		sm.Bankroll.Balance = &b
		var open float64
		s.DB.Model(&Bet{}).Where("organization_id = ? AND user_id = ? AND status = ?", org, user, BetOpen).Select("COALESCE(SUM(stake),0)").Scan(&open)
		av := round2(b - open)
		sm.Bankroll.Available = &av
		if in := sm.Bankroll.Start + sm.Bankroll.Deposits; in > 0 {
			sm.ROI = ptr(round4(sm.PnL / in))
		}
	}
	sm.Limits = s.LimitsView(org, user)
	return sm
}

// bankrollBalance is the person's current bankroll (nil when not set).
func (s *Service) bankrollBalance(org, user string) *float64 {
	if org == "" || user == "" {
		return nil
	}
	var n int64
	s.DB.Model(&BankrollEntry{}).Where("organization_id = ? AND user_id = ?", org, user).Count(&n)
	if n == 0 {
		return nil
	}
	var in, out, pnl float64
	s.DB.Model(&BankrollEntry{}).Where("organization_id = ? AND user_id = ? AND kind IN ?", org, user, []string{"start", "deposit"}).Select("COALESCE(SUM(amount),0)").Scan(&in)
	s.DB.Model(&BankrollEntry{}).Where("organization_id = ? AND user_id = ? AND kind = ?", org, user, "withdrawal").Select("COALESCE(SUM(amount),0)").Scan(&out)
	s.DB.Model(&Bet{}).Where("organization_id = ? AND user_id = ? AND status <> ?", org, user, BetOpen).Select("COALESCE(SUM(pnl),0)").Scan(&pnl)
	b := round2(in - out + pnl)
	return &b
}

// ---------------------------------------------------------------- bankroll

// BankrollInput is a bankroll entry.
type BankrollInput struct {
	Kind   string     `json:"kind"` // start | deposit | withdrawal
	Amount float64    `json:"amount"`
	At     *time.Time `json:"at"`
	Note   string     `json:"note"`
}

// AddBankroll records the starting bankroll or a deposit / withdrawal.
func (s *Service) AddBankroll(org, user string, in BankrollInput) (*BankrollEntry, error) {
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if in.Kind != "start" && in.Kind != "deposit" && in.Kind != "withdrawal" {
		return nil, inputErr("Kind must be start, deposit or withdrawal.")
	}
	if in.Amount <= 0 || in.Amount > 1e10 || math.IsNaN(in.Amount) {
		return nil, inputErr("Amount must be more than 0.")
	}
	now := s.Now()
	at := now
	if in.At != nil && !in.At.After(now) {
		at = in.At.UTC()
	}
	if in.Kind == "start" {
		var n int64
		s.DB.Model(&BankrollEntry{}).Where("organization_id = ? AND user_id = ? AND kind = 'start'", org, user).Count(&n)
		if n > 0 {
			return nil, inputErr("Your starting bankroll is already set. Record a deposit or withdrawal instead (or delete the starting entry first).")
		}
	}
	e := BankrollEntry{ID: uuid.NewString(), OrganizationID: org, UserID: user, Kind: in.Kind, Amount: round2(in.Amount), Currency: s.currencyOf(org),
		At: at, Note: clip(in.Note, 500), CreatedAt: now}
	return &e, s.DB.Create(&e).Error
}

// Bankroll lists the person's entries.
func (s *Service) Bankroll(org, user string) []BankrollEntry {
	out := []BankrollEntry{}
	s.DB.Where("organization_id = ? AND user_id = ?", org, user).Order("at, created_at").Find(&out)
	return out
}

// DeleteBankroll removes an entry.
func (s *Service) DeleteBankroll(org, user, id string) error {
	res := s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Delete(&BankrollEntry{})
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return res.Error
}

// ---------------------------------------------------------------- watchlist & manual odds

// Watch adds a fixture, team or competition to the person's watchlist.
func (s *Service) Watch(org, user, kind string, ref int64, note string) (*WatchItem, error) {
	switch kind {
	case "fixture":
		if !s.fixtureKnown(ref) {
			return nil, inputErr("Unknown match.")
		}
	case "team":
		var n int64
		s.DB.Model(&Team{}).Where("id = ?", ref).Count(&n)
		if n == 0 {
			return nil, inputErr("Unknown team.")
		}
	case "competition":
		var n int64
		s.DB.Model(&Competition{}).Where("id = ?", ref).Count(&n)
		if n == 0 {
			return nil, inputErr("Unknown competition.")
		}
	default:
		return nil, inputErr("Watch a fixture, team or competition.")
	}
	var w WatchItem
	if s.DB.Where("organization_id = ? AND user_id = ? AND kind = ? AND ref_id = ?", org, user, kind, ref).Limit(1).Find(&w).RowsAffected > 0 {
		return &w, nil
	}
	w = WatchItem{ID: uuid.NewString(), OrganizationID: org, UserID: user, Kind: kind, RefID: ref, Note: clip(note, 500), CreatedAt: s.Now()}
	return &w, s.DB.Create(&w).Error
}

// Watchlist lists the person's items.
func (s *Service) Watchlist(org, user string) []map[string]any {
	var ws []WatchItem
	s.DB.Where("organization_id = ? AND user_id = ?", org, user).Order("created_at DESC").Find(&ws)
	out := []map[string]any{}
	for _, w := range ws {
		m := map[string]any{"id": w.ID, "kind": w.Kind, "ref_id": w.RefID, "note": w.Note, "created_at": w.CreatedAt}
		switch w.Kind {
		case "fixture":
			var f Fixture
			if s.DB.Where("id = ?", w.RefID).Limit(1).Find(&f).RowsAffected > 0 {
				m["title"] = s.teamName(f.HomeID) + " v " + s.teamName(f.AwayID)
				m["kickoff"], m["status"] = f.Kickoff, f.Status
			}
		case "team":
			m["title"] = s.teamName(w.RefID)
		case "competition":
			m["title"] = s.compName(w.RefID)
		}
		out = append(out, m)
	}
	return out
}

// Unwatch removes an item.
func (s *Service) Unwatch(org, user, id string) error {
	res := s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Delete(&WatchItem{})
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return res.Error
}

// ManualOddInput is a price seen on a bookmaker's app.
type ManualOddInput struct {
	FixtureID int64      `json:"fixture_id"`
	Bookmaker string     `json:"bookmaker"`
	Market    string     `json:"market"`
	Selection string     `json:"selection"`
	Line      *float64   `json:"line"`
	Price     float64    `json:"price"`
	SeenAt    *time.Time `json:"seen_at"`
}

// AddManualOdd stores a hand-typed price ("manual: <bookmaker>"), visible
// to the person's organisation only.
func (s *Service) AddManualOdd(org, user string, in ManualOddInput) (*ManualOdd, error) {
	bi := BetInput{FixtureID: in.FixtureID, Bookmaker: in.Bookmaker, Market: in.Market, Selection: in.Selection, Line: in.Line, Odds: in.Price, Stake: 1}
	if err := validateBet(&bi); err != nil {
		return nil, err
	}
	var f Fixture
	if s.DB.Where("id = ?", in.FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
		return nil, inputErr("Unknown match.")
	}
	now := s.Now()
	seen := now
	if in.SeenAt != nil && !in.SeenAt.After(now) {
		seen = in.SeenAt.UTC()
	}
	m := ManualOdd{ID: uuid.NewString(), OrganizationID: org, UserID: user, FixtureID: f.ID, Bookmaker: bi.Bookmaker, Market: bi.Market, Selection: bi.Selection,
		Line: bi.Line, Price: bi.Odds, SeenAt: seen, CreatedAt: now}
	return &m, s.DB.Create(&m).Error
}

// ManualOdds lists the organisation's hand-typed prices for a fixture.
func (s *Service) ManualOdds(org string, fixture int64) []ManualOdd {
	out := []ManualOdd{}
	q := s.DB.Where("organization_id = ?", org)
	if fixture != 0 {
		q = q.Where("fixture_id = ?", fixture)
	}
	q.Order("seen_at DESC").Limit(500).Find(&out)
	return out
}

// DeleteManualOdd removes one of the person's own prices.
func (s *Service) DeleteManualOdd(org, user, id string) error {
	res := s.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, org, user).Delete(&ManualOdd{})
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return res.Error
}

var _ = errors.New
var _ = gorm.ErrRecordNotFound
