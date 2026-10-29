package daemon

import (
	"os/exec"
	"testing"
)

func TestAppleStringEscapes(t *testing.T) {
	cases := map[string]string{
		`plain`:                  `"plain"`,
		`say "hi"`:               `"say \"hi\""`,
		`back\slash`:             `"back\\slash"`,
		"two\nlines":             `"two lines"`,
		`" & do shell script "x`: `"\" & do shell script \"x"`,
	}
	for in, want := range cases {
		if got := appleString(in); got != want {
			t.Errorf("appleString(%q) = %s, want %s", in, got, want)
		}
	}
}

// osacompile parses the script without running it.
func TestNotifyScriptCompiles(t *testing.T) {
	if _, err := exec.LookPath("osacompile"); err != nil {
		t.Skip("no osacompile")
	}
	script := notifyScript("gmwd", `Fix "auth" \ tests — 2 commits`+"\n"+`" & do shell script "touch /tmp/pwned`)
	out := t.TempDir() + "/n.scpt"
	if b, err := exec.Command("osacompile", "-e", script, "-o", out).CombinedOutput(); err != nil {
		t.Errorf("osacompile: %v: %s", err, b)
	}
}
