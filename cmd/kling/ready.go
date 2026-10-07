package main

// kling machine ready / kling machine hooks: el "listo" que define la imagen y
// sus ganchos tras restaurar (docs/api.md, "Listo y ganchos").

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdReady es `kling machine ready <ref> [-wait 60s] [-json]`.
func cmdReady(args []string) error {
	fs := flag.NewFlagSet("machine ready", flag.ExitOnError)
	host := hostFlag(fs)
	wait := fs.Duration("wait", 0, "wait up to this long for it to be ready (0 = just ask)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling machine ready <ref> [-wait 60s] [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	res, err := api.NewClient(hostOf(*host)).Ready(ctx, fs.Arg(0), *wait)
	if err != nil {
		return err
	}
	return printReady(os.Stdout, res, *asJSON)
}

// cmdHooks es `kling machine hooks <ref> [-wait 60s]`.
func cmdHooks(args []string) error {
	fs := flag.NewFlagSet("machine hooks", flag.ExitOnError)
	host := hostFlag(fs)
	wait := fs.Duration("wait", time.Minute, "wait up to this long for the hooks to finish (0 = start them and return)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling machine hooks <ref> [-wait 60s] [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	res, err := api.NewClient(hostOf(*host)).RunHooks(ctx, fs.Arg(0), *wait)
	if err != nil {
		return err
	}
	return printReady(os.Stdout, res, *asJSON)
}

// printReady cuenta un ReadyResult. Sale con error si no está listo, para que
// un script pueda encadenarlo (`kling machine ready m -wait 1m && ...`).
func printReady(out io.Writer, res *api.ReadyResult, asJSON bool) error {
	if asJSON {
		if err := json.NewEncoder(out).Encode(res); err != nil {
			return err
		}
		if !res.OK() {
			return &errConCodigo{code: 1, err: fmt.Errorf("%s is not ready", res.Name)}
		}
		return nil
	}
	que := "ready"
	switch {
	case res.Ready == api.ReadyUnknown && res.Guest == nil:
		que = "ready (no readiness probe: its guest agent predates it, or it has none)"
	case res.Ready == api.ReadyUnknown:
		que = "ready (its image declares no probe nor hooks)"
	case res.Ready != api.ReadyYes:
		que = res.Ready
	}
	fmt.Fprintf(out, "%s  %s", shortID(res.ID), que)
	if res.WaitedMS > 0 {
		fmt.Fprintf(out, "  (waited %d ms)", res.WaitedMS)
	}
	fmt.Fprintln(out)
	if g := res.Guest; g != nil {
		if g.Probe {
			fmt.Fprintf(out, "  probe  %s\n", api.GuestReadyProbe)
		}
		if g.HasHooks || g.Hooks != "" {
			estado := g.Hooks
			if estado == "" {
				estado = "not run yet"
			}
			fmt.Fprintf(out, "  hooks  %s/*  %s\n", api.GuestPostRestoreDir, estado)
		}
		if g.Detail != "" && !g.Ready {
			fmt.Fprintf(out, "  why    %s\n", g.Detail)
		}
	}
	if res.Detail != "" && (res.Guest == nil || res.Guest.Detail == "") {
		fmt.Fprintf(out, "  why    %s\n", res.Detail)
	}
	if !res.OK() {
		return &errConCodigo{code: 1, err: fmt.Errorf("%s is not ready", res.Name)}
	}
	return nil
}
