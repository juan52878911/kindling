package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/units"
)

// kling try: ejecutar algo aislado en un solo comando.
//
// Sin él son cuatro: sandbox create, exec, leer el id, sandbox rm. Y el último
// es el que se olvida, así que el sandbox se queda consumiendo hasta su TTL.
// try los encadena y borra el sandbox pase lo que pase (también con Ctrl-C);
// con -keep lo deja vivo para seguir trabajando en él.

// tryDefaultImage es la imagen si no se da ni -image ni -from: la de
// `kling image toolchain`, que lleva el agente de invitado (sin él no hay
// exec) y además node y python, lo que suele querer probar quien llega.
const tryDefaultImage = "toolchain"

// tryOptions es lo que try pasa al daemon y cómo termina. msgs recibe lo que
// cuenta kling (sandbox listo, borrado, avisos); nil = el stderr del comando.
type tryOptions struct {
	req  api.SandboxRequest
	cmd  []string
	keep bool
	msgs io.Writer
}

// tryJSON es lo que imprime try -json: el comando, su salida y su código, y
// cuánto tardó cada parte. Un programa (o un agente) no tiene que separar la
// salida del comando de lo que cuenta kling.
type tryJSON struct {
	Sandbox    string `json:"sandbox"`
	Image      string `json:"image,omitempty"`
	From       string `json:"from,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	ReadyMS    int64  `json:"ready_ms"`
	DurationMS int64  `json:"duration_ms"`
	Kept       bool   `json:"kept,omitempty"`
	Messages   string `json:"messages,omitempty"`
}

func cmdTry(args []string) (int, error) {
	fs := flag.NewFlagSet("try", flag.ExitOnError)
	host := hostFlag(fs)
	image := fs.String("image", "", "image with the guest agent, or a Docker reference (alpine:3.20, imported the first time; default: "+tryDefaultImage+")")
	from := fs.String("from", "", "snapshot made from a machine with -allow-exec (~300 ms instead of a cold boot)")
	mem := units.MiBVar(fs, "mem", 0, "memory: 512M, 2G (bare number = MiB; default 256)")
	cpus := fs.Int("cpus", 0, "vCPUs (default 1)")
	egress := fs.String("egress", "", "network egress: none (default) | internet | allowlist")
	var allow domainsFlag
	fs.Var(&allow, "allow", "domain allowed with -egress allowlist (repeatable, or comma-separated)")
	ttl := units.DurationVar(fs, "ttl", 0, "lifetime if kling dies before removing it, or with -keep: 10m, 1h (bare number = seconds; default 10m)")
	keep := fs.Bool("keep", false, "keep the sandbox afterwards and print its id")
	asJSON := fs.Bool("json", false, "print {sandbox, exit_code, stdout, stderr, ready_ms, duration_ms} instead of streaming (needs a command)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kling try [-image I | -from T] [-mem 256M] [-egress none|internet|allowlist] [-keep] [--] [cmd [args...]]")
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
	if *asJSON && len(cmd) == 0 {
		return 2, errors.New("-json needs a command: kling try -json -image alpine:3.20 -- uname -a")
	}
	o := tryOptions{keep: *keep, cmd: cmd, req: api.SandboxRequest{
		Image: *image, From: *from, MemMiB: *mem, VCPUs: *cpus,
		Egress: *egress, AllowDomains: splitDomains(allow.String()), TTLSeconds: int(ttl.Seconds()),
	}}
	avisarAllow(os.Stderr, o.req.AllowDomains)
	porDefecto := o.req.Image == "" && o.req.From == ""
	if porDefecto {
		o.req.Image = tryDefaultImage
	}

	endpoint := hostOf(*host)
	c := api.NewClient(endpoint)
	// Una referencia de Docker se importa la primera vez, como en run -image.
	if esRefDocker(o.req.Image) {
		ctx, stop := ctxWithSignals()
		name, err := asegurarImagenDocker(ctx, c, o.req.Image)
		stop()
		if err != nil {
			return 1, err
		}
		o.req.Image = name
	}
	if *asJSON {
		ctx, stop := ctxWithSignals()
		defer stop()
		return runTryJSON(ctx, c, o, os.Stdout)
	}
	if porDefecto {
		ctx, stop := ctxWithSignals()
		err := imagenTryDisponible(ctx, c)
		stop()
		if err != nil {
			return 1, err
		}
	}
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
	r, err := runTryCon(ctx, c, o, stdout, stderr)
	return r.code, err
}

// tryRun es lo que pasó en un try: el código, lo que contestó el exec (nil
// si no llegó a ejecutarse), el sandbox y cuánto tardó en estar listo.
type tryRun struct {
	code  int
	res   *api.ExecResult
	mc    *api.Machine
	ready time.Duration
}

// runTryCon es runTry con todo lo que pasó (para -json).
func runTryCon(ctx context.Context, c *api.Client, o tryOptions, stdout, stderr io.Writer) (tryRun, error) {
	msgs := o.msgs
	if msgs == nil {
		msgs = stderr
	}
	start := time.Now()
	mc, err := tryCreate(ctx, c, o.req, msgs)
	if err != nil {
		return tryRun{code: 1}, err
	}
	r := tryRun{mc: mc, ready: time.Since(start)}
	defer tryCleanup(c, mc, o.keep, msgs)

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
			r.code = 130
			return r, errors.New("interrupted")
		}
		avisarEgressA(ctx, msgs, c, mc.ID)
		r.code = 1
		return r, err
	}
	if res.TimedOut {
		fmt.Fprintf(msgs, "kling: killed after %s (timeout)\n", time.Duration(res.DurationMS)*time.Millisecond)
	}
	if res.Truncated {
		fmt.Fprintln(msgs, "kling: output truncated at the limit")
	}
	if res.ExitCode != 0 {
		// Antes del borrado (diferido): después ya no hay máquina a la que
		// preguntar. Un npx que no llega al registro con egress none falla
		// con un error de red que no nombra el egress.
		avisarEgressA(ctx, msgs, c, mc.ID)
	}
	r.code, r.res = res.ExitCode, res
	return r, nil
}

// runTryJSON es try -json: el comando corre con su salida en memoria y al
// final sale un solo objeto. El código de salida es el del comando, como sin
// -json; un fallo de kling sale como error (y JSON vacío no).
func runTryJSON(ctx context.Context, c *api.Client, o tryOptions, w io.Writer) (int, error) {
	var out, errb, msgs bytes.Buffer
	o.msgs = &msgs
	start := time.Now()
	r, err := runTryCon(ctx, c, o, &out, &errb)
	if err != nil {
		if msgs.Len() > 0 {
			os.Stderr.Write(msgs.Bytes())
		}
		return r.code, err
	}
	j := tryJSON{
		Sandbox: shortID(r.mc.ID), Image: r.mc.Image, From: r.mc.From, ExitCode: r.code,
		Stdout: out.String(), Stderr: errb.String(), Kept: o.keep,
		TimedOut: r.res.TimedOut, Truncated: r.res.Truncated,
		ReadyMS:    r.ready.Milliseconds(),
		DurationMS: time.Since(start).Milliseconds(),
		Messages:   strings.TrimSpace(msgs.String()),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(j); err != nil {
		return 1, err
	}
	return r.code, nil
}

// imagenTryDisponible comprueba, antes de crear nada, que la imagen por
// defecto existe: una instalación recién hecha no la trae, y el error del
// daemon ("image does not exist") llegaba después de pedir el sandbox y sin
// decir que hay que construirla. No la construye: tarda minutos y descarga
// node y python, y eso lo decide quien lo pide. Si no se puede listar (un
// daemon viejo, un fallo de red) se sigue como antes: el daemon dirá lo suyo.
func imagenTryDisponible(ctx context.Context, c *api.Client) error {
	imgs, err := c.Images(ctx)
	if err != nil {
		return nil
	}
	hayMin := false
	for _, im := range imgs {
		switch im.Name {
		case tryDefaultImage:
			return nil
		case "min":
			hayMin = true
		}
	}
	hint := "kling image toolchain   (builds it: node, python and the guest agent; takes a few minutes)"
	if hayMin {
		hint += "\n     or use the small image you already have:  kling try -image min -- <cmd>"
	}
	return &errWithHint{
		err:  fmt.Errorf("kling try uses the image %q by default, and this daemon doesn't have it", tryDefaultImage),
		hint: hint,
	}
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
