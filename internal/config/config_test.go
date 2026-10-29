package config

import "testing"

func TestHomeOverride(t *testing.T) {
	t.Setenv("GMWD_HOME", "/x/y")
	if h, err := Home(); err != nil || h != "/x/y" {
		t.Errorf("Home() = %q, %v", h, err)
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Settings{{MaxBurnPct: 101}, {MaxBurnPct: -1}, {MaxBurnPct: 50, ReservePct: 120}} {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}
