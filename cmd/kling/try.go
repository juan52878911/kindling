package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// kling try: ejecutar algo aislado en un solo comando.
//
// Sin él son cuatro: sandbox create, exec, leer el id, sandbox rm. Y el último
// es el que se olvida, así que el sandbox se queda consumiendo hasta su TTL.
// try los encadena y borra el sandbox pase lo que pase (también con Ctrl-C);
// con -keep lo deja vivo para seguir trabajando en él.

// tryDefaultImage es la imagen si no se da ni -image ni -from: la de
// `kling images toolchain`, que lleva el agente de invitado (sin él no hay
// exec) y además node y python, lo que suele querer probar quien llega.
const tryDefaultImage = "toolchain"

// tryOptions es lo que try pasa al daemon y cómo termina.
type tryOptions struct {
	req  api.SandboxRequest
	cmd  []string
	keep bool
}

func cmdTry(args []string) (int, error) {
	fs := flag.NewFlagSet("try", flag.ExitOnError)
	host := hostFlag(fs)
	image := fs.String("image", "", "image with the guest agent (default: "+tryDefaultImage+")")
	from := fs.String("from", "", "snapshot made from a machine with -allow-exec (~300 ms instead of a cold boot)")
	mem := fs.Int("mem", 0, "memory in MiB (default 256)")
	cpus := fs.Int("cpus", 0, "vCPUs (default 1)")
	egress := fs.String("egress", "", "network egress: none (default) | internet | allowlist")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist (comma-separated)")
	ttl := fs.Duration("ttl", 0, "lifetime if kling dies before removing it, or with -keep (default 10m)")
	keep := fs.Bool("keep", false, "keep the sandbox afterwards and print its id")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kling try [-image I | -from S] [-mem MiB] [-egress none|internet|allowlist] [-keep] [--] [cmd [args...]]")
		fmt.Fprintln(os.Stderr, "  creates a throwaway sandbox, runs cmd (or opens a shell) and removes it")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return 2, err
	}
	cmd := fs.Args()
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if *image != "" && *from != "" {
		return 2, errors.New("pass -image or -from, not both")
	}
	o := tryOptions{keep: *keep, cmd: cmd, req: api.SandboxRequest{
		Image: *image, From: *from, MemMiB: *mem, VCPUs: *cpus,
		Egress: *egress, AllowDomains: splitDomains(*allow), TTLSeconds: int(ttl.Seconds()),
	}}
	if o.req.Image == "" && o.req.From == "" {
		o.req.Image = tryDefaultImage
	}

	endpoint := hostOf(*host)
	c := api.NewClient(endpoint)
	if len(cmd) > 0 {
		ctx, stop := ctxWithSignals()
		defer stop()
		return runTry(ctx, c, o, os.Stdout, os.Stderr)
	}

	// Sin comando, una shell: necesita la terminal y su propio manejo de
	// Ctrl-C (que ha de llegar al programa de dentro), así que la lleva
	// cmdShell y aquí solo se crea y se borra.
	if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stdout.Fd()) {
		return 2, errors.New("kling try with no command opens a shell and needs a terminal; pass a command: kling try -- <cmd>")
	}
	ctx, stop := ctxWithSignals()
	mc, err := tryCreate(ctx, c, o.req, os.Stderr)
	stop()
	if err != nil {
		return 1, err
	}
	defer tryCleanup(c, mc, o.keep, os.Stderr)
	return cmdShell([]string{"-H", endpoint, mc.ID})
}

// runTry crea el sandbox, ejecuta o.cmd con la salida en streaming y lo borra.
// Devuelve el código del comando: un 1 de grep no es un fallo de kling.
func runTry(ctx context.Context, c *api.Client, o tryOptions, stdout, stderr io.Writer) (int, error) {
	mc, err := tryCreate(ctx, c, o.req, stderr)
	if err != nil {
		return 1, err
	}
	defer tryCleanup(c, mc, o.keep, stderr)

	res, err := c.Exec(ctx, mc.ID, api.ExecRequest{Cmd: o.cmd}, func(stream string, data []byte) error {
		var err error
		if stream == "stderr" {
			_, err = stderr.Write(data)
		} else {
			_, err = stdout.Write(data)
		}
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return 130, errors.New("interrupted")
		}
		return 1, err
	}
	if res.TimedOut {
		fmt.Fprintf(stderr, "kling: killed after %s (timeout)\n", time.Duration(res.DurationMS)*time.Millisecond)
	}
	if res.Truncated {
		fmt.Fprintln(stderr, "kling: output truncated at the limit")
	}
	return res.ExitCode, nil
}

// tryCreate crea el sandbox y lo cuenta por stderr: stdout es del comando, y
// quien hace `kling try -- cat x > y` no quiere nuestras líneas en y.
func tryCreate(ctx context.Context, c *api.Client, req api.SandboxRequest, stderr io.Writer) (*api.Machine, error) {
	start := time.Now()
	mc, err := c.CreateSandbox(ctx, req)
	if err != nil {
		return nil, err
	}
	src := mc.Image
	if mc.From != "" {
		src = mc.From
	}
	fmt.Fprintf(stderr, "kling: sandbox %s (%s) ready in %s\n", shortID(mc.ID), src, time.Since(start).Round(time.Millisecond))
	return mc, nil
}

// tryCleanup borra el sandbox con un contexto propio: el de la orden puede
// estar ya cancelado por el Ctrl-C que ha llevado hasta aquí, y es justo
// entonces cuando más importa no dejarlo vivo.
func tryCleanup(c *api.Client, mc *api.Machine, keep bool, stderr io.Writer) {
	id := shortID(mc.ID)
	if keep {
		fmt.Fprintf(stderr, "kling: kept sandbox %s\n  kling exec %s -- <cmd>\n  kling sandbox rm %s\n", id, id, id)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.RemoveSandbox(ctx, mc.ID); err != nil {
		fmt.Fprintf(stderr, "kling: sandbox %s not removed (%v); it expires with its TTL\ntry: kling sandbox rm %s\n", id, err, id)
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
