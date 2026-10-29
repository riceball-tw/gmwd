package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"gmwd/internal/config"
	"gmwd/internal/rpc"
)

// cmdConfig shows the daemon's settings, or changes the ones given.
func cmdConfig(args []string, out io.Writer) error {
	fs := newFlags("config")
	maxBurn := fs.Float64("max-burn", 0, "fill the week up to this % of the weekly cap (the usage slider)")
	reserve := fs.Float64("reserve", 0, "% of every 5h window always left for you")
	idleMode := fs.String("idle-mode", "", "not-using-claude (default) or away (also no keyboard/mouse input)")
	quiet := fs.Int("quiet-minutes", 0, "how long you must be idle before gmwd works; 0 = don't wait")
	notify := fs.String("notify", "", "on/off: macOS notification when a run leaves something to review")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: gmwd config [--max-burn N] [--reserve N] [--idle-mode M] [--quiet-minutes N] [--notify on|off]")
	}
	var p rpc.SettingsPatch
	var flagErr error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-burn":
			p.MaxBurnPct = maxBurn
		case "reserve":
			p.ReservePct = reserve
		case "idle-mode":
			m := config.IdleMode(*idleMode)
			p.IdleMode = &m
		case "quiet-minutes":
			p.QuietMinutes = quiet
		case "notify":
			v, ok := map[string]bool{"on": true, "true": true, "off": false, "false": false}[*notify]
			if !ok {
				flagErr = fmt.Errorf("--notify %q: want on or off", *notify)
			}
			p.Notify = &v
		}
	})
	if flagErr != nil {
		return flagErr
	}
	// An empty patch changes nothing and returns the current settings.
	var s config.Settings
	if err := call(rpc.MethodSettingsUpdate, p, &s); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, s)
	}
	state := "active"
	if s.Paused {
		state = "paused"
	}
	fmt.Fprintf(out, "state          %s\n", state)
	fmt.Fprintf(out, "max burn       %.0f%% of the weekly cap\n", s.MaxBurnPct)
	fmt.Fprintf(out, "reserve        %.0f%% of every 5h window\n", s.ReservePct)
	fmt.Fprintf(out, "idle mode      %s\n", s.IdleMode)
	if s.QuietMinutes == 0 {
		fmt.Fprintf(out, "quiet minutes  0 (doesn't wait for you to be idle)\n")
	} else {
		fmt.Fprintf(out, "quiet minutes  %d\n", s.QuietMinutes)
	}
	fmt.Fprintf(out, "notify         %s\n", map[bool]string{true: "on", false: "off"}[s.Notify])
	return nil
}
