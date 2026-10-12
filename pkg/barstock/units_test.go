package barstock

import (
	"math"
	"testing"
	"time"
)

var whisky = Spec{BaseUnit: BaseML, UnitSize: 750, UnitLabel: "bottle", PackSize: 12, PackLabel: "case", TotML: 25}
var beer = Spec{BaseUnit: BaseUnit, UnitSize: 1, UnitLabel: "can", PackSize: 24, PackLabel: "tray"}
var keg = Spec{BaseUnit: BaseML, UnitSize: 50000, UnitLabel: "keg", PackSize: 1, TotML: 25}

func TestBusinessDate(t *testing.T) {
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	cases := []struct {
		local  string
		cutoff string
		want   string
	}{
		{"2026-10-10 02:00", "06:00", "2026-10-09"}, // 02:00 Saturday belongs to Friday night
		{"2026-10-10 05:59", "06:00", "2026-10-09"},
		{"2026-10-10 06:00", "06:00", "2026-10-10"},
		{"2026-10-09 23:30", "06:00", "2026-10-09"},
		{"2026-10-10 00:00", "06:00", "2026-10-09"},
		{"2026-10-10 00:00", "00:00", "2026-10-10"}, // midnight cutoff = calendar days
		{"2026-11-01 01:30", "06:00", "2026-10-31"}, // month boundary
		{"2027-01-01 03:00", "04:30", "2026-12-31"}, // year boundary
		{"2026-10-10 04:45", "04:30", "2026-10-10"},
		{"2026-10-10 01:30", "bad", "2026-10-09"}, // invalid cutoff → 06:00
	}
	for _, c := range cases {
		lt, err := time.ParseInLocation("2006-01-02 15:04", c.local, nbo)
		if err != nil {
			t.Fatal(err)
		}
		// The same instant expressed in UTC must give the same answer.
		if got := BusinessDate(lt.UTC(), nbo, c.cutoff); got != c.want {
			t.Errorf("BusinessDate(%s, cutoff %s) = %s, want %s", c.local, c.cutoff, got, c.want)
		}
	}
	// DST zone: a night across the European clock change still maps by local time.
	ber, _ := time.LoadLocation("Europe/Berlin")
	lt := time.Date(2026, 10, 25, 2, 30, 0, 0, ber)
	if got := BusinessDate(lt, ber, "06:00"); got != "2026-10-24" {
		t.Errorf("DST night = %s", got)
	}
}

func TestToBaseAndBack(t *testing.T) {
	cases := []struct {
		s    Spec
		qty  float64
		unit string
		want float64
	}{
		{whisky, 25, "ml", 25},
		{whisky, 2, "tot", 50},
		{whisky, 1.5, "bottle", 1125},
		{whisky, 2, "unit", 1500},
		{whisky, 1, "pack", 9000},
		{whisky, 300, "", 300},
		{beer, 5, "unit", 5},
		{beer, 2, "pack", 48},
		{beer, 3, "bottle", 3},
		{keg, 0.5, "bottle", 25000},
	}
	for _, c := range cases {
		got, err := c.s.ToBase(c.qty, c.unit)
		if err != nil || got != c.want {
			t.Errorf("ToBase(%v %s) = %v, %v; want %v", c.qty, c.unit, got, err, c.want)
		}
		if back := c.s.FromBase(got, c.unit); c.unit != "" && math.Abs(back-c.qty) > 1e-9 {
			t.Errorf("FromBase(%v, %s) = %v, want %v", got, c.unit, back, c.qty)
		}
	}
	if _, err := beer.ToBase(1, "ml"); err == nil {
		t.Error("ml on a counted item should fail")
	}
	if _, err := beer.ToBase(1, "tot"); err == nil {
		t.Error("tot on a counted item should fail")
	}
	if _, err := whisky.ToBase(1, "gallon"); err == nil {
		t.Error("unknown unit should fail")
	}
	noTot := whisky
	noTot.TotML = 0
	if _, err := noTot.ToBase(1, "tot"); err == nil {
		t.Error("tot without a tot size should fail")
	}
}

func TestText(t *testing.T) {
	cases := []struct {
		s    Spec
		base float64
		want string
	}{
		{whisky, 2670, "3 bottles + 420 ml"},
		{whisky, 750, "1 bottle"},
		{whisky, 0, "0 bottles"},
		{whisky, 420, "420 ml"},
		{whisky, 12.5, "12.5 ml"},
		{whisky, -1125, "-1 bottle + 375 ml"},
		{beer, 53, "2 trays + 5 cans"},
		{beer, 24, "1 tray"},
		{beer, 5, "5 cans"},
		{beer, 1, "1 can"},
		{beer, 0, "0 cans"},
		{beer, -3, "-3 cans"},
		{keg, 62500, "1 keg + 12500 ml"},
		{Spec{BaseUnit: BaseUnit, UnitSize: 1, UnitLabel: "box", PackSize: 1}, 2, "2 boxes"},
		{Spec{BaseUnit: BaseUnit, UnitSize: 1, UnitLabel: "piece", PackSize: 1}, 3, "3 pieces"},
		{Spec{BaseUnit: BaseUnit, UnitSize: 1, UnitLabel: "glass", PackSize: 1}, 2, "2 glasses"},
		{Spec{BaseUnit: BaseUnit, UnitSize: 1, UnitLabel: "battery", PackSize: 1}, 2, "2 batteries"},
	}
	for _, c := range cases {
		if got := c.s.Text(c.base); got != c.want {
			t.Errorf("Text(%v) = %q, want %q", c.base, got, c.want)
		}
	}
	if got := whisky.QtyText(50, "tot"); got != "2 tots" {
		t.Errorf("QtyText tots = %q", got)
	}
	if got := beer.QtyText(48, "pack"); got != "2 trays" {
		t.Errorf("QtyText packs = %q", got)
	}
}

func TestCountedBase(t *testing.T) {
	full, empty := 1250.0, 500.0
	kegFull, kegEmpty := 60000.0, 10000.0
	ok := []struct {
		s      Spec
		full   float64
		mode   string
		value  float64
		fw, ew *float64
		want   float64
	}{
		{whisky, 3, PartialTenths, 0.4, nil, nil, 2550},            // 3 bottles + 4 tenths = 2250 + 300
		{whisky, 0, PartialTenths, 1, nil, nil, 750},               // ten tenths
		{whisky, 2, PartialML, 120, nil, nil, 1620},                // 2 bottles + 120 ml
		{whisky, 1, PartialWeight, 875, &full, &empty, 1125},       // (875−500)/(1250−500)×750 = 375
		{whisky, 0, PartialWeight, 500, &full, &empty, 0},          // empty bottle on the scale
		{whisky, 0, PartialWeight, 1250, &full, &empty, 750},       // full bottle on the scale
		{whisky, 4, PartialNone, 0, nil, nil, 3000},                // whole bottles only
		{beer, 47, PartialNone, 0, nil, nil, 47},                   // cans
		{beer, 47, PartialTenths, 0, nil, nil, 47},                 // partial ignored for counted items when 0
		{keg, 1, PartialWeight, 30000, &kegFull, &kegEmpty, 70000}, // 1 keg + (30−10)/(60−10) × 50 L
	}
	for i, c := range ok {
		got, err := c.s.CountedBase(c.full, c.mode, c.value, c.fw, c.ew)
		if err != nil || math.Abs(got-c.want) > 1e-6 {
			t.Errorf("case %d: CountedBase = %v, %v; want %v", i, got, err, c.want)
		}
	}
	bad := []struct {
		s     Spec
		full  float64
		mode  string
		value float64
		fw    *float64
	}{
		{whisky, 1, PartialTenths, 0.45, &full}, // not a 0.1 step
		{whisky, 1, PartialTenths, 4, &full},    // tenths must be 0..1
		{whisky, 1, PartialML, 800, &full},      // more than a bottle
		{whisky, 1, PartialML, -1, &full},
		{whisky, 1, PartialWeight, 1300, &full}, // above full
		{whisky, 1, PartialWeight, 450, &full},  // below empty
		{whisky, 1, PartialWeight, 800, nil},    // no weights set
		{whisky, -1, PartialNone, 0, &full},     // negative units
		{beer, 1, PartialML, 100, &full},        // partial on counted item
		{whisky, 1, "litres", 1, &full},         // unknown mode
	}
	for i, c := range bad {
		var ew *float64
		if c.fw != nil {
			ew = &empty
		}
		if _, err := c.s.CountedBase(c.full, c.mode, c.value, c.fw, ew); err == nil {
			t.Errorf("bad case %d should fail", i)
		}
	}
}

func TestServings(t *testing.T) {
	if got := whisky.Servings(75); got != 3 {
		t.Errorf("servings ml = %v", got)
	}
	if got := beer.Servings(2); got != 2 {
		t.Errorf("servings units = %v", got)
	}
}

func TestRoundMoney(t *testing.T) {
	if got := RoundMoney(10.005, "KES"); got != 10.01 {
		t.Errorf("KES %v", got)
	}
	if got := RoundMoney(1234.5, "UGX"); got != 1235 {
		t.Errorf("UGX %v", got)
	}
	if got := RoundMoney(1.2345, "KWD"); got != 1.235 && got != 1.234 {
		t.Errorf("KWD %v", got)
	}
}
