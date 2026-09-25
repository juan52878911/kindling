package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

// cmdLines imprime las líneas de un log tal como las ve el ejemplo (sin ANSI,
// con su número desde 0 y su sección): es lo que hace falta para anotar a mano
// el oro de logs propios en un manifiesto logs-*.jsonl.
func cmdLines(args []string) error {
	fs := flag.NewFlagSet("lines", flag.ExitOnError)
	from := fs.Int("from", 0, "first line (0-based)")
	n := fs.Int("n", 0, "lines to print (0 = all)")
	width := fs.Int("width", 200, "clip each line to this many bytes")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: ci-triage lines [-from N] [-n N] <logfile>")
	}
	lg, err := triage.ReadFile(fs.Arg(0), triage.DefaultLimits)
	if err != nil {
		return err
	}
	fmt.Printf("# %d lines, format %s, truncated %v\n", len(lg.Lines), lg.Format, lg.Truncated)
	end := len(lg.Lines)
	if *n > 0 {
		end = min(end, *from+*n)
	}
	for i := max(0, *from); i < end; i++ {
		l := lg.Lines[i]
		mark := " "
		switch {
		case l.Marked:
			mark = "E"
		case l.Red:
			mark = "r"
		}
		fmt.Printf("%5d %s %-24s %s\n", i, mark, triage.Clip(l.Section, 24), triage.Clip(l.Text, *width))
	}
	return nil
}
