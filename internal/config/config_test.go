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
	bad := func(f func(*Settings)) Settings { s := DefaultSettings(); f(&s); return s }
	for _, s := range []Settings{
		bad(func(s *Settings) { s.MaxBurnPct = 101 }),
		bad(func(s *Settings) { s.MaxBurnPct = -1 }),
		bad(func(s *Settings) { s.ReservePct = 120 }),
		bad(func(s *Settings) { s.IdleMode = "sometimes" }),
		bad(func(s *Settings) { s.IdleMode = "" }),
		bad(func(s *Settings) { s.QuietMinutes = -1 }),
	} {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}
