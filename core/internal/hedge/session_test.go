package hedge

import (
	"testing"
	"time"
)

// TestUSSession pins the ET boundary arithmetic: September is EDT (UTC-4), a
// US session spans two UTC days (the β-refresh key must be the ET date), and
// regular hours are [09:30, 16:00) ET.
func TestUSSession(t *testing.T) {
	must := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) // a Friday
	cases := []struct {
		utc   time.Time
		key   string
		reg   bool
		label string
	}{
		{must.Add(14*time.Hour + 0*time.Minute), "20260918", true, "10:00 EDT mid-session"},
		{must.Add(13*time.Hour + 30*time.Minute), "20260918", true, "09:30 open boundary (inclusive)"},
		{must.Add(12*time.Hour + 0*time.Minute), "20260918", false, "08:00 pre-market"},
		{must.Add(20*time.Hour + 0*time.Minute), "20260918", false, "16:00 close boundary (exclusive)"},
		{must.Add(21*time.Hour + 0*time.Minute), "20260918", false, "17:00 after hours"},
		{must.Add(2 * time.Hour), "20260917", false, "22:00 EDT previous day — key is the ET date, not UTC"},
	}
	for _, c := range cases {
		key, reg := USSession(c.utc)
		if key != c.key || reg != c.reg {
			t.Fatalf("%s: USSession = (%q, %v), want (%q, %v)", c.label, key, reg, c.key, c.reg)
		}
	}
}

func TestLegValidate(t *testing.T) {
	ok := []Leg{
		{Kind: LegShare, Shares: -100},
		{Kind: LegOption, Right: "C", Expiry: "20261218", Strike: 400, Contracts: -3},
		{Kind: LegOption, Right: "P", Expiry: "20261218", Strike: 400, Contracts: 2, ConId: 486153, TradingClass: "TSLA", Note: "covered"},
	}
	for _, l := range ok {
		if err := l.Validate(); err != nil {
			t.Fatalf("%+v: must validate: %v", l, err)
		}
	}
	bad := map[string]Leg{
		"zero shares":    {Kind: LegShare, Shares: 0},
		"bad kind":       {Kind: "bond", Shares: 1},
		"bad right":      {Kind: LegOption, Right: "X", Expiry: "20261218", Strike: 1, Contracts: 1},
		"bad expiry":     {Kind: LegOption, Right: "C", Expiry: "2026-12-18", Strike: 1, Contracts: 1},
		"zero strike":    {Kind: LegOption, Right: "C", Expiry: "20261218", Strike: 0, Contracts: 1},
		"zero contracts": {Kind: LegOption, Right: "C", Expiry: "20261218", Strike: 1, Contracts: 0},
		"note too long":  {Kind: LegShare, Shares: 1, Note: string(make([]byte, 65))},
	}
	for name, l := range bad {
		if err := l.Validate(); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
	shareLeg := Leg{Kind: LegShare, Shares: 100}
	if shareLeg.Label() != "+100 shares" {
		t.Fatalf("share label = %q", shareLeg.Label())
	}
}
