package daemon

import (
	"os/exec"
	"strings"
)

// MacNotify shows a macOS notification with osascript.
func MacNotify(title, message string) error {
	return exec.Command("osascript", "-e", notifyScript(title, message)).Run()
}

func notifyScript(title, message string) string {
	return "display notification " + appleString(message) + " with title " + appleString(title)
}

// appleString quotes s as an AppleScript string literal, so topic titles
// can't break out of it.
func appleString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	return `"` + s + `"`
}
