// Command gmwd spends leftover Claude subscription quota on a topic backlog.
package main

import (
	"fmt"
	"os"
)

const usageText = `gmwd — use leftover Claude quota on your backlog

Usage:
  gmwd status [--json] [--no-probe]   current 5h / weekly usage and reset times
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "status":
		err = cmdStatus(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "gmwd: unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmwd:", err)
		os.Exit(1)
	}
}
