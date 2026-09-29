package main

// kling sandbox fork: ramificar un sandbox vivo en N copias. El daemon lo pausa
// un instante, lo congela en un snapshot temporal y restaura las copias desde
// él (ver internal/machine/fork.go).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/units"
)

const sandboxForkUsage = "usage: kling sandbox fork <sandbox> [-n N] [-ttl 10m] [-on-ttl remove|freeze] [-label k=v]... [-skip-ready] [-q] [-json]"

// forkOptions es `kling sandbox fork` ya interpretado.
type forkOptions struct {
	host   string
	ref    string
	req    api.ForkRequest
	quiet  bool
	asJSON bool
}

// parseSandboxFork interpreta los argumentos. handling es flag.ExitOnError en
// el CLI y flag.ContinueOnError en los tests.
func parseSandboxFork(args []string, handling flag.ErrorHandling) (forkOptions, error) {
	fs := flag.NewFlagSet("sandbox fork", handling)
	if handling == flag.ContinueOnError {
		fs.SetOutput(io.Discard)
	}
	host := hostFlag(fs)
	n := fs.Int("n", 1, fmt.Sprintf("how many copies (1-%d)", api.ForkMax))
	ttl := units.DurationVar(fs, "ttl", 0, "lifetime of each copy: 10m, 1h (bare number = seconds; default: the original's)")
	onTTL := fs.String("on-ttl", "", "when a copy's ttl runs out: remove or freeze (default: the original's)")
	var labels labelFlag
	fs.Var(&labels, "label", "label for every copy, k=v (repeatable)")
	quiet := fs.Bool("q", false, "print only the ids of the copies")
	asJSON := fs.Bool("json", false, "JSON output: the snapshot and the copies")
	skipReady := fs.Bool("skip-ready", false, "don't wait for the sandbox (and each copy) to be ready by its image's probe and hooks")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return forkOptions{}, err
	}
	if fs.NArg() != 1 {
		return forkOptions{}, errors.New(sandboxForkUsage)
	}
	if *n < 1 || *n > api.ForkMax {
		return forkOptions{}, fmt.Errorf("-n must be between 1 and %d", api.ForkMax)
	}
	if *ttl < 0 || (*ttl > 0 && *ttl < time.Second) {
		return forkOptions{}, errors.New("-ttl must be at least 1s")
	}
	switch *onTTL {
	case "", api.OnTTLRemove, api.OnTTLFreeze:
	default:
		return forkOptions{}, fmt.Errorf("invalid -on-ttl %q: use %s or %s", *onTTL, api.OnTTLRemove, api.OnTTLFreeze)
	}
	if err := api.ValidateForkLabels(map[string]string(labels)); err != nil {
		return forkOptions{}, fmt.Errorf("-label: %w", err)
	}
	return forkOptions{
		host: *host, ref: fs.Arg(0), quiet: *quiet, asJSON: *asJSON,
		req: api.ForkRequest{Count: *n, TTLSeconds: int(ttl.Seconds()), OnTTL: *onTTL, Labels: map[string]string(labels),
			SkipReady: *skipReady},
	}, nil
}

func sandboxFork(args []string) error {
	o, err := parseSandboxFork(args, flag.ExitOnError)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	return runSandboxFork(ctx, api.NewClient(hostOf(o.host)), o, os.Stdout)
}

// runSandboxFork pide el fork y lo cuenta en out.
func runSandboxFork(ctx context.Context, c *api.Client, o forkOptions, out io.Writer) error {
	start := time.Now()
	res, err := c.ForkSandbox(ctx, o.ref, o.req)
	if err != nil {
		var se *api.StatusError
		if errors.As(err, &se) && se.Code == 404 && se.Message == "404 Not Found" {
			// La ruta no existe, no el sandbox: el daemon es anterior al fork.
			return &errWithHint{err: errors.New("this daemon can't fork sandboxes"),
				hint: "upgrade kindling on the host (the daemon needs the \"fork\" capability)"}
		}
		return err
	}
	if o.asJSON {
		if res.Sandboxes == nil {
			res.Sandboxes = []*api.Machine{} // [] y no null: más fácil para jq
		}
		return json.NewEncoder(out).Encode(res)
	}
	if o.quiet {
		for _, mc := range res.Sandboxes {
			fmt.Fprintln(out, shortID(mc.ID))
		}
		return nil
	}
	fmt.Fprintf(out, "%s branched into %d in %s (snapshot %s, removed with the last copy)\n",
		o.ref, len(res.Sandboxes), time.Since(start).Round(time.Millisecond), res.Snapshot)
	for _, mc := range res.Sandboxes {
		fmt.Fprintf(out, "  %s  %s\n", shortID(mc.ID), mc.Name)
	}
	if len(res.Sandboxes) > 0 {
		first := res.Sandboxes[0].Name
		next("kling exec %s -- ...   ·   kling sandbox ls   ·   kling sandbox rm %s", first, first)
	}
	return nil
}
