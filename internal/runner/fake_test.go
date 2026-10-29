package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The test binary doubles as a fake `claude`: with GMWD_FAKE_CLAUDE set it
// plays one step of a script instead of running tests. Each call records its
// args, cwd and env so tests can check what the runner asked for.

// fakeStep is what one fake claude call does.
type fakeStep struct {
	Status  string `json:"status,omitempty"` // structured_output status; "" = none
	Summary string `json:"summary,omitempty"`
	IsError bool   `json:"isError,omitempty"`
	// APIErrorStatus is reported in the result (e.g. 429).
	APIErrorStatus int `json:"apiErrorStatus,omitempty"`
	// Write creates files relative to cwd: name → content.
	Write map[string]string `json:"write,omitempty"`
	// RateLimit, if set, is emitted before the result.
	RateLimit *fakeRateLimit `json:"rateLimit,omitempty"`
	// SleepAfterRateLimit delays the result (to test mid-iteration kills).
	SleepAfterRateLimit time.Duration `json:"sleep,omitempty"`
	Tokens              int64         `json:"tokens,omitempty"` // output tokens
	NoResult            bool          `json:"noResult,omitempty"`
	ExitCode            int           `json:"exitCode,omitempty"`
}

type fakeRateLimit struct {
	Status   string  `json:"status"`
	FiveHour float64 `json:"fiveHour"`
	SevenDay float64 `json:"sevenDay"`
	ResetsAt int64   `json:"resetsAt"`
}

// fakeCall is what the fake recorded about one invocation.
type fakeCall struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
}

func TestMain(m *testing.M) {
	if dir := os.Getenv("GMWD_FAKE_CLAUDE"); dir != "" {
		os.Exit(fakeClaude(dir))
	}
	os.Exit(m.Run())
}

func fakeClaude(dir string) int {
	var steps []fakeStep
	data, err := os.ReadFile(filepath.Join(dir, "script.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.Unmarshal(data, &steps); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	n := 0
	if b, err := os.ReadFile(filepath.Join(dir, "count")); err == nil {
		n, _ = strconv.Atoi(string(b))
	}
	_ = os.WriteFile(filepath.Join(dir, "count"), []byte(strconv.Itoa(n+1)), 0o600)
	step := steps[min(n, len(steps)-1)]

	cwd, _ := os.Getwd()
	call, _ := json.Marshal(fakeCall{Args: os.Args[1:], Dir: cwd, Env: os.Environ()})
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("call-%d.json", n+1)), call, 0o600)

	for name, content := range step.Write {
		_ = os.MkdirAll(filepath.Dir(name), 0o700)
		_ = os.WriteFile(name, []byte(content), 0o600)
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	sid := argAfter(os.Args, "--session-id")
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid})
	if rl := step.RateLimit; rl != nil {
		emit(map[string]any{"type": "rate_limit_event", "session_id": sid, "rate_limit_info": map[string]any{
			"status": rl.Status, "resetsAt": rl.ResetsAt, "rateLimitType": "five_hour",
			"unifiedWindows": map[string]any{
				"five_hour": map[string]any{"utilization": rl.FiveHour, "resetsAt": rl.ResetsAt},
				"seven_day": map[string]any{"utilization": rl.SevenDay, "resetsAt": rl.ResetsAt + 3*86400},
			},
		}})
	}
	time.Sleep(step.SleepAfterRateLimit)
	if !step.NoResult {
		res := map[string]any{
			"type": "result", "subtype": "success", "is_error": step.IsError, "session_id": sid,
			"num_turns": 1, "total_cost_usd": 0.01, "result": step.Summary,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": step.Tokens,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 500},
			"permission_denials": []any{},
		}
		if step.APIErrorStatus != 0 {
			res["api_error_status"] = step.APIErrorStatus
		}
		if step.Status != "" {
			res["structured_output"] = map[string]any{"status": step.Status, "summary": step.Summary}
		}
		emit(res)
	}
	return step.ExitCode
}

// fake is a scripted claude for one test.
type fake struct {
	t   *testing.T
	dir string
}

func newFake(t *testing.T, steps ...fakeStep) *fake {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return &fake{t: t, dir: dir}
}

func (f *fake) claude() Claude {
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	return Claude{Bin: exe, Env: append(MinimalEnv(), "GMWD_FAKE_CLAUDE="+f.dir)}
}

// call returns what the n-th (1-based) invocation received.
func (f *fake) call(n int) fakeCall {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, fmt.Sprintf("call-%d.json", n)))
	if err != nil {
		f.t.Fatalf("call %d: %v", n, err)
	}
	var c fakeCall
	if err := json.Unmarshal(data, &c); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fake) calls() int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "count"))
	n, _ := strconv.Atoi(string(b))
	return n
}
