package runner

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gmwd/internal/usage"
)

func TestMinimalEnvDropsSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GITHUB_TOKEN", "ghp-test")
	env := MinimalEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Errorf("leaked %s", kv)
		}
	}
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "HOME=") }) {
		t.Error("HOME missing; claude needs it for OAuth")
	}
}

func TestArgsAreUnattendedAndScoped(t *testing.T) {
	inv := Invocation{
		Prompt: "do it", Model: "sonnet", SessionID: "sid",
		Tools: []string{"Read", "Bash"}, AllowedTools: []string{"Write(./**)"},
		AddDirs: []string{"/ctx"}, Settings: `{"sandbox":{}}`, JSONSchema: `{}`,
	}
	args := inv.Args()
	want := map[string]string{
		"-p": "do it", "--model": "sonnet", "--session-id": "sid", "--tools": "Read,Bash",
		"--allowedTools": "Write(./**)", "--add-dir": "/ctx", "--settings": `{"sandbox":{}}`,
		"--json-schema": `{}`, "--permission-mode": "dontAsk", "--output-format": "stream-json",
		"--setting-sources": "project,local",
	}
	for flag, v := range want {
		if got := argAfter(args, flag); got != v {
			t.Errorf("%s = %q, want %q", flag, got, v)
		}
	}
	if slices.Contains(args, "--dangerously-skip-permissions") || slices.Contains(args, "--bare") {
		t.Errorf("unsafe args: %v", args)
	}
	// --tools "" must still be passed when no tools are wanted, or the
	// default set (with unsandboxed Bash) applies.
	if got := argAfter(Invocation{Prompt: "x"}.Args(), "--tools"); got != "" {
		t.Errorf("empty tools = %q", got)
	}
}

// A stream shaped like Claude Code 2.1.308 output (docs/spike.md §2).
const sampleStream = `{"type":"system","subtype":"hook_started","session_id":"s1"}
{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-5-5"}
{"type":"assistant","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":2,"output_tokens":3}},"session_id":"s1"}
not json at all
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793294400,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.05,"resetsAt":1793294400},"seven_day":{"utilization":0.28,"resetsAt":1793527200}}},"session_id":"s1"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793294400,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.07,"resetsAt":1793294400},"seven_day":{"utilization":0.29,"resetsAt":1793527200}}},"session_id":"s1"}
{"type":"result","subtype":"success","is_error":false,"api_error_status":null,"num_turns":4,"result":"text","structured_output":{"status":"progress","summary":"did x"},"total_cost_usd":0.12,"permission_denials":[{"tool_name":"Write"}],"usage":{"input_tokens":20,"output_tokens":300,"cache_creation_input_tokens":4000,"cache_read_input_tokens":90000},"session_id":"s1"}
`

func TestParseStream(t *testing.T) {
	var seen []float64
	res, err := parseStream(strings.NewReader(sampleStream), func(s usage.Snapshot) {
		seen = append(seen, s.FiveHour.Utilization)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []float64{0.05, 0.07}) {
		t.Errorf("rate limit callbacks = %v", seen)
	}
	if !res.GotResult || res.IsError || res.SessionID != "s1" || res.NumTurns != 4 || res.Denials != 1 || res.CostUSD != 0.12 {
		t.Errorf("result = %+v", res)
	}
	// The final result's usage wins over partial assistant usage.
	if res.Usage.Output != 300 || res.Usage.CacheCreate != 4000 || res.Usage.CacheRead != 90000 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.RateLimit == nil || res.RateLimit.SevenDay.Utilization != 0.29 || res.Rejected() {
		t.Errorf("rate limit = %+v", res.RateLimit)
	}
	var out struct{ Status string }
	if err := json.Unmarshal(res.Structured, &out); err != nil || out.Status != "progress" {
		t.Errorf("structured = %s", res.Structured)
	}
}

func TestParseStreamRejectedWithoutResult(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1793294400,"rateLimitType":"five_hour"}}` + "\n"
	res, err := parseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.GotResult || !res.Rejected() || res.RateLimit.FiveHour.Utilization != 1 {
		t.Errorf("result = %+v", res)
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
