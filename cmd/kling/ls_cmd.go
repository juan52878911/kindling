package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdLs es `kling ls <máquina>:<directorio>`.
func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling ls [-json] <machine>:<dir>")
	}
	ref, dir, remote := splitRemote(fs.Arg(0))
	if !remote {
		return errUso("usage: kling ls [-json] <machine>:<dir>")
	}
	if dir == "" {
		dir = "/"
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	l, err := api.NewClient(hostOf(*host)).ListFiles(ctx, ref, dir)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(l)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, e := range l.Entries {
		name := e.Name
		switch e.Type {
		case "dir":
			name += "/"
		case "symlink":
			name += " -> " + e.Target
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Mode, human(e.Size), e.ModTime.Local().Format("2006-01-02 15:04"), name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if l.Truncated {
		fmt.Fprintf(os.Stderr, "kling: listing truncated at %d entries\n", len(l.Entries))
	}
	return nil
}
