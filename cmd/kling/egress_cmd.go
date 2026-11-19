package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdEgress es `kling egress set|show`.
func cmdEgress(args []string) error {
	if len(args) == 0 {
		return errUso("usage: kling egress <set|show> [...]")
	}
	switch args[0] {
	case "set":
		return egressSet(args[1:])
	case "show":
		return egressShow(args[1:])
	}
	return errUsoDe("egress", fmt.Errorf("unknown subcommand %q: use set or show", args[0]))
}

// egressSet cambia la política de salida de una máquina viva.
func egressSet(args []string) error {
	fs := flag.NewFlagSet("egress set", flag.ContinueOnError)
	host := hostFlag(fs)
	mode := fs.String("egress", "", "new mode: none | internet | allowlist (default: keep it)")
	allow := fs.String("allow", "", "comma-separated domains; replaces the list (allowlist)")
	asJSON := fs.Bool("json", false, "print the resulting policy as JSON")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || (*mode == "" && *allow == "") {
		return errUso("usage: kling egress set [-egress none|internet|allowlist] [-allow dom1,dom2] [-json] <machine>")
	}
	var doms []string
	for _, d := range strings.Split(*allow, ",") {
		if d = strings.TrimSpace(d); d != "" {
			doms = append(doms, d)
		}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	pol, err := api.NewClient(hostOf(*host)).SetEgress(ctx, fs.Arg(0), api.EgressRequest{Egress: *mode, AllowDomains: doms})
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(pol)
	}
	desc := pol.Egress
	if len(pol.AllowDomains) > 0 {
		desc += " (" + strings.Join(pol.AllowDomains, ", ") + ")"
	}
	switch pol.Applied {
	case api.EgressAppliedUnchanged:
		fmt.Fprintf(os.Stderr, "%s: egress already %s\n", fs.Arg(0), desc)
	case api.EgressAppliedNextStart:
		fmt.Fprintf(os.Stderr, "%s: egress %s; the machine has no network up, it applies at the next start\n", fs.Arg(0), desc)
	default:
		fmt.Fprintf(os.Stderr, "%s: egress %s, applied live\n", fs.Arg(0), desc)
		if pol.Narrowed && !pol.ConnectionsClosed {
			fmt.Fprintln(os.Stderr, "kling: connections already open to destinations that are no longer allowed stay until they end (the host has no conntrack)")
		}
	}
	return nil
}

// egressShow muestra la política y lo que cortó (GET /machines/{ref}/egress).
func egressShow(args []string) error {
	fs := flag.NewFlagSet("egress show", flag.ContinueOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling egress show [-json] <machine>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	e, err := api.NewClient(hostOf(*host)).EgressBlocked(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(e)
	}
	fmt.Printf("egress %s, %d blocked\n", e.Egress, e.Total)
	for _, d := range e.Destinations {
		fmt.Printf("  %-40s %-4s %d\n", d.Dest, d.Kind, d.Count)
	}
	return nil
}
