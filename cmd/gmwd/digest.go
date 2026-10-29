package main

import (
	"errors"
	"fmt"
	"io"

	"gmwd/internal/rpc"
)

// cmdDigest prints a day's digest (today by default).
func cmdDigest(args []string, out io.Writer) error {
	fs := newFlags("digest")
	date := fs.String("date", "", "day to show, YYYY-MM-DD (default today)")
	list := fs.Bool("list", false, "list the days that have a digest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: gmwd digest [--date YYYY-MM-DD] [--list]")
	}
	var res rpc.DigestResult
	if err := call(rpc.MethodDigest, rpc.DigestParams{Date: *date}, &res); err != nil {
		return err
	}
	if *list {
		if len(res.Days) == 0 {
			fmt.Fprintln(out, "no digests yet")
		}
		for _, d := range res.Days {
			fmt.Fprintln(out, d)
		}
		return nil
	}
	fmt.Fprint(out, res.Markdown)
	return nil
}
