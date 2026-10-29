package usage

import (
	"strings"
	"testing"
	"time"
)

var taipei = mustLoc("Asia/Taipei")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func TestParseRateLimitEvent(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793294400,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.05,"resetsAt":1793294400},"seven_day":{"utilization":0.28,"resetsAt":1793527200}}},"uuid":"x","session_id":"y"}`
	now := time.Unix(1793280000, 0)
	s, err := ParseRateLimitEvent([]byte(line), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "allowed" {
		t.Errorf("status = %q", s.Status)
	}
	if s.FiveHour.Utilization != 0.05 || s.FiveHour.ResetsAt.Unix() != 1793294400 {
		t.Errorf("five_hour = %+v", s.FiveHour)
	}
	if s.SevenDay.Utilization != 0.28 || s.SevenDay.ResetsAt.Unix() != 1793527200 {
		t.Errorf("seven_day = %+v", s.SevenDay)
	}
	if got := s.FiveHour.WindowStart(); got.Unix() != 1793294400-5*3600 {
		t.Errorf("window start = %v", got)
	}
}

func TestRejectedWithoutUnifiedWindows(t *testing.T) {
	info := RateLimitInfo{Status: "rejected", ResetsAt: 1792062600, RateLimitType: "five_hour"}
	s := info.Snapshot(time.Unix(1792060000, 0))
	if s.FiveHour.Utilization != 1 || s.FiveHour.ResetsAt.Unix() != 1792062600 {
		t.Errorf("five_hour = %+v", s.FiveHour)
	}
}

const usageText = "You are currently using your subscription to power your Claude Code usage\n\n" +
	"Current session: 6% used · resets Oct 30 at 1:20am (Asia/Taipei)\n" +
	"Current week (all models): 29% used · resets Nov 1 at 6pm (Asia/Taipei)\n\n" +
	"What's contributing to your limits usage?\n"

func TestParseUsageText(t *testing.T) {
	now := time.Date(2026, 10, 29, 21, 35, 0, 0, taipei)
	s, err := ParseUsageText(usageText, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.FiveHour.Utilization != 0.06 {
		t.Errorf("five_hour util = %v", s.FiveHour.Utilization)
	}
	if want := time.Date(2026, 10, 30, 1, 20, 0, 0, taipei); !s.FiveHour.ResetsAt.Equal(want) {
		t.Errorf("five_hour reset = %v, want %v", s.FiveHour.ResetsAt, want)
	}
	if s.SevenDay.Utilization != 0.29 {
		t.Errorf("seven_day util = %v", s.SevenDay.Utilization)
	}
	if want := time.Date(2026, 11, 1, 18, 0, 0, 0, taipei); !s.SevenDay.ResetsAt.Equal(want) {
		t.Errorf("seven_day reset = %v, want %v", s.SevenDay.ResetsAt, want)
	}
}

func TestParseUsageTextYearRollover(t *testing.T) {
	now := time.Date(2026, 12, 30, 22, 0, 0, 0, taipei)
	text := "Current session: 1% used · resets Dec 31 at 2am (Asia/Taipei)\n" +
		"Current week (all models): 10% used · resets Jan 3 at 6pm (Asia/Taipei)\n"
	s, err := ParseUsageText(text, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.SevenDay.ResetsAt.Year() != 2027 {
		t.Errorf("seven_day reset = %v, want 2027", s.SevenDay.ResetsAt)
	}
}

func TestParseUsageTextExtraWeek(t *testing.T) {
	now := time.Date(2026, 10, 29, 21, 35, 0, 0, taipei)
	text := "Current session: 6% used\n" +
		"Current week (all models): 29% used · resets Nov 1 at 6pm (Asia/Taipei)\n" +
		"Current week (Sonnet only): 41% used · resets Nov 1 at 6pm (Asia/Taipei)\n"
	s, err := ParseUsageText(text, now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.FiveHour.ResetsAt.IsZero() {
		t.Errorf("five_hour reset should be unknown, got %v", s.FiveHour.ResetsAt)
	}
	if s.SevenDay.Utilization != 0.41 {
		t.Errorf("binding week util = %v, want 0.41", s.SevenDay.Utilization)
	}
	if w, ok := s.Extra["all models"]; !ok || w.Utilization != 0.29 {
		t.Errorf("extra = %+v", s.Extra)
	}
}

func TestParseResetTimeOnly(t *testing.T) {
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, taipei)
	got, err := parseResetTime("7:10pm (Asia/Taipei)", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 15, 19, 10, 0, 0, taipei); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScanFile(t *testing.T) {
	since := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	// Two records for one message (one per content block), one old record,
	// one headless record, one rate-limit record, one interactive user record.
	log := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-29T12:30:00Z","entrypoint":"cli","requestId":"r1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000}}}`,
		`{"type":"assistant","timestamp":"2026-10-29T12:30:01Z","entrypoint":"cli","requestId":"r1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"tool_use"}],"usage":{"input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000}}}`,
		`{"type":"assistant","timestamp":"2026-10-29T11:00:00Z","entrypoint":"cli","requestId":"r0","message":{"id":"m0","model":"claude-opus-5-5","usage":{"input_tokens":999}}}`,
		`{"type":"assistant","timestamp":"2026-10-29T12:40:00Z","entrypoint":"sdk-cli","requestId":"r2","message":{"id":"m2","model":"claude-haiku-5-5","usage":{"input_tokens":1,"output_tokens":5}}}`,
		`{"type":"assistant","timestamp":"2026-10-29T12:50:00Z","entrypoint":"cli","error":"rate_limit","isApiErrorMessage":true,"quotaLimits":{"status":"rejected","resetsAt":1793294400,"rateLimitType":"five_hour"},"message":{"id":"m3","model":"<synthetic>","usage":{}}}`,
		`{"type":"user","timestamp":"2026-10-29T12:55:00Z","entrypoint":"cli","message":{"role":"user","content":"x"}}`,
		`{"type":"attachment","timestamp":"2026-10-29T12:59:00Z","entrypoint":"cli"}`,
		`not json`,
	}, "\n")
	tl := Tally{Since: since, ByModel: map[string]TokenCounts{}}
	if err := scanFile(strings.NewReader(log), since, map[string]bool{}, &tl); err != nil {
		t.Fatal(err)
	}
	opus := tl.ByModel["claude-opus-5-5"]
	if opus.Requests != 1 || opus.Input != 2 || opus.Output != 10 || opus.CacheCreate != 100 || opus.CacheRead != 1000 {
		t.Errorf("opus = %+v", opus)
	}
	if tl.Headless.Requests != 1 || tl.Headless.Output != 5 {
		t.Errorf("headless = %+v", tl.Headless)
	}
	if tl.Interactive.Requests != 1 {
		t.Errorf("interactive = %+v", tl.Interactive)
	}
	if len(tl.RateLimitHits) != 1 || tl.RateLimitHits[0].Info.ResetsAt != 1793294400 {
		t.Errorf("hits = %+v", tl.RateLimitHits)
	}
	if want := time.Date(2026, 10, 29, 12, 55, 0, 0, time.UTC); !tl.LastInteractive.Equal(want) {
		t.Errorf("last interactive = %v", tl.LastInteractive)
	}
	if got := tl.Total().Total(); got != 1112+6 {
		t.Errorf("total = %d", got)
	}
}

func TestParseUsageTextSnapsReset(t *testing.T) {
	now := time.Date(2026, 10, 29, 21, 35, 0, 0, taipei)
	text := "Current session: 8% used · resets Oct 30 at 1:19am (Asia/Taipei)\n" +
		"Current week (all models): 29% used · resets Nov 1 at 5:59pm (Asia/Taipei)\n"
	s, err := ParseUsageText(text, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.FiveHour.ResetsAt.Unix() != 1793294400 || s.SevenDay.ResetsAt.Unix() != 1793527200 {
		t.Errorf("resets = %v / %v, want the rate_limit_event values", s.FiveHour.ResetsAt, s.SevenDay.ResetsAt)
	}
}
