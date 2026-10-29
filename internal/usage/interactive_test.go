package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func rec(typ, entry string, ts time.Time) string {
	return fmt.Sprintf(`{"type":%q,"entrypoint":%q,"timestamp":%q,"message":{"content":"x"}}`, typ, entry, ts.UTC().Format(time.RFC3339Nano))
}

func TestLastInteractive(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	t1, t2, t3 := now.Add(-30*time.Minute), now.Add(-5*time.Minute), now.Add(-time.Minute)
	// An interactive session, its subagent, and a gmwd job (sdk-cli) that is newer.
	writeLog(t, filepath.Join(root, "proj", "a.jsonl"), t2, rec("user", "cli", t1), rec("attachment", "cli", t2))
	writeLog(t, filepath.Join(root, "proj", "a", "subagents", "agent-1.jsonl"), t2, rec("assistant", "cli", t2.Add(-time.Second)))
	writeLog(t, filepath.Join(root, "gmwd-job", "b.jsonl"), t3, rec("assistant", "sdk-cli", t3))
	// An old file is skipped by mtime even though it claims a new record.
	writeLog(t, filepath.Join(root, "old", "c.jsonl"), now.Add(-48*time.Hour), rec("user", "cli", now))

	got, err := LastInteractive(root, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(t2.UTC().Truncate(time.Nanosecond)) && got.Sub(t2).Abs() > time.Millisecond {
		t.Errorf("last = %v, want %v", got, t2)
	}
	if got, _ := LastInteractive(root, now.Add(-2*time.Minute)); !got.IsZero() {
		t.Errorf("only sdk-cli activity recently, got %v", got)
	}
	if got, err := LastInteractive(filepath.Join(root, "missing"), now.Add(-time.Hour)); err != nil || !got.IsZero() {
		t.Errorf("missing root: %v %v", got, err)
	}
}

func TestLastInteractiveReadsOnlyTheTail(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	// A big log: the newest interactive record is at the end, behind more
	// than tailBytes of older records.
	var lines []string
	pad := strings.Repeat("y", 1000)
	for i := 0; len(lines)*1100 < 2*tailBytes; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"user","entrypoint":"cli","timestamp":%q,"pad":%q}`, now.Add(-2*time.Hour).UTC().Format(time.RFC3339), pad))
	}
	lines = append(lines, rec("user", "cli", now.Add(-3*time.Minute)))
	writeLog(t, filepath.Join(root, "p", "big.jsonl"), now, lines...)
	got, err := LastInteractive(root, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Sub(now.Add(-3*time.Minute)).Abs() > time.Second {
		t.Errorf("last = %v", got)
	}
}
