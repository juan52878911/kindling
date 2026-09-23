package main

// kling exec, kling cp y kling sandbox: ejecutar código dentro de una microVM y
// mover ficheros, que es lo que necesita un agente de código.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// stringsFlag es un flag repetible.
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

// cmdExec es `kling exec [-i] [-e K=V] [-w DIR] [-timeout D] <ref> [--] cmd...`.
//
// La salida remota va a la salida local según llega, stdout a stdout y stderr a
// stderr, y kling termina con el código del comando remoto: se puede usar en
// scripts igual que si el comando corriera aquí.
func cmdExec(args []string) (int, error) {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	host := hostFlag(fs)
	stdin := fs.Bool("i", false, "send local stdin to the command (up to 1 MiB)")
	var env stringsFlag
	fs.Var(&env, "e", "environment variable KEY=value (repeatable)")
	dir := fs.String("w", "", "working directory inside the machine")
	timeout := fs.Duration("timeout", 0, "kill the command after this long (default 5m, max 1h)")
	maxOut := fs.Int("max-output", 0, "bytes of stdout and of stderr to keep (default 8 MiB, max 64 MiB)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kling exec [-i] [-e K=V] [-w DIR] [-timeout 5m] <machine|sandbox> [--] <cmd> [args...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fs.Usage()
		return 2, errors.New("missing machine or command")
	}
	ref, cmd := rest[0], rest[1:]
	if cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return 2, errors.New("missing command")
	}
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			return 2, fmt.Errorf("-e %q: use KEY=value", kv)
		}
	}
	req := api.ExecRequest{Cmd: cmd, Dir: *dir, Env: env,
		TimeoutSeconds: int(timeout.Seconds()), MaxOutputBytes: *maxOut}
	if *timeout > 0 && req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 1
	}
	if *stdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, api.ExecMaxStdin+1))
		if err != nil {
			return 1, err
		}
		if len(b) > api.ExecMaxStdin {
			return 2, fmt.Errorf("stdin is over %d bytes; copy big inputs with kling cp", api.ExecMaxStdin)
		}
		req.Stdin = b
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	res, err := api.NewClient(hostOf(*host)).Exec(ctx, ref, req, func(stream string, data []byte) error {
		var err error
		if stream == "stderr" {
			_, err = os.Stderr.Write(data)
		} else {
			_, err = os.Stdout.Write(data)
		}
		return err
	})
	if err != nil {
		return 1, err
	}
	if res.TimedOut {
		fmt.Fprintf(os.Stderr, "kling: killed after %s (timeout)\n", time.Duration(res.DurationMS)*time.Millisecond)
	}
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "kling: output truncated at the limit (-max-output)")
	}
	return res.ExitCode, nil
}

// cmdCp es `kling cp <src> <dst>`, donde uno de los dos es <máquina>:<ruta>.
// "-" es la entrada o la salida estándar.
func cmdCp(args []string) error {
	fs := flag.NewFlagSet("cp", flag.ExitOnError)
	host := hostFlag(fs)
	mode := fs.String("mode", "", "permissions of the copied file inside the machine (octal, default 0644)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kling cp [-mode 0755] <local|-> <machine>:<path>\n       kling cp <machine>:<path> <local|->")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("need a source and a destination")
	}
	src, dst := fs.Arg(0), fs.Arg(1)
	srcRef, srcPath, srcRemote := splitRemote(src)
	dstRef, dstPath, dstRemote := splitRemote(dst)
	if srcRemote == dstRemote {
		return errors.New("exactly one side has to be <machine>:<path>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	if dstRemote {
		var m uint64
		if *mode != "" {
			if _, err := fmt.Sscanf(*mode, "%o", &m); err != nil || m > 0o7777 {
				return fmt.Errorf("invalid -mode %q", *mode)
			}
		}
		var r io.Reader = os.Stdin
		if src != "-" {
			f, err := os.Open(src)
			if err != nil {
				return err
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return fmt.Errorf("%s is a directory: copy a tar and unpack it with kling exec", src)
			}
			if *mode == "" {
				m = uint64(fi.Mode().Perm())
			}
			r = f
		}
		// Copiar a un directorio (acabado en /) deja el fichero con su nombre.
		if strings.HasSuffix(dstPath, "/") && src != "-" {
			dstPath += baseName(src)
		}
		st, err := c.WriteFile(ctx, dstRef, dstPath, uint32(m), false, r)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s:%s  %s  %s\n", dstRef, st.Path, human(st.Size), st.Mode)
		return nil
	}

	rc, err := c.ReadFile(ctx, srcRef, srcPath)
	if err != nil {
		return err
	}
	defer rc.Close()
	var w io.Writer = os.Stdout
	if dst != "-" {
		if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
			dst = strings.TrimSuffix(dst, "/") + "/" + baseName(srcPath)
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	_, err = io.Copy(w, rc)
	return err
}

// splitRemote separa "máquina:/ruta". Una ruta local con ':' se escribe con ./
// delante.
func splitRemote(s string) (ref, path string, remote bool) {
	if s == "-" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		return "", s, false
	}
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// cmdSandbox es `kling sandbox create|ls|renew|rm`.
func cmdSandbox(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sandbox <create|ls|renew|rm> [...]")
	}
	switch args[0] {
	case "create", "new":
		return sandboxCreate(args[1:])
	case "ls", "list":
		return sandboxList(args[1:])
	case "renew":
		return sandboxRenew(args[1:])
	case "rm", "remove":
		return sandboxRemove(args[1:])
	}
	return fmt.Errorf("unknown subcommand %q: use create, ls, renew or rm", args[0])
}

func sandboxCreate(args []string) error {
	fs := flag.NewFlagSet("sandbox create", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "sandbox name")
	image := fs.String("image", "", "image with the guest agent (e.g. toolchain)")
	from := fs.String("from", "", "snapshot made from a machine with -allow-exec")
	cpus := fs.Int("cpus", 0, "vCPUs (default 1)")
	mem := fs.Int("mem", 0, "memory in MiB (default 256)")
	ttl := fs.Duration("ttl", 0, "lifetime, or idle time with -on-ttl freeze (default 10m, max 24h)")
	onTTL := fs.String("on-ttl", "", "when the ttl runs out: remove (default) or freeze (sleeps at zero cost, the next exec wakes it in ms)")
	egress := fs.String("egress", "", "network egress: none (default) | internet | allowlist")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist (comma-separated)")
	cpuPct := fs.Int("cpu-pct", 0, "CPU ceiling as a percentage of one core")
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volume to mount: name[:/mount][:ro] (repeatable)")
	var shares shareFlag
	fs.Var(&shares, "share", shareUsage)
	quiet := fs.Bool("q", false, "print only the id")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	endpoint := hostOf(*host)
	client := api.NewClient(endpoint)
	start := time.Now()
	shareSpecs, err := prepareShares(ctx, client, endpoint, shares)
	if err != nil {
		return err
	}
	req := api.SandboxRequest{Name: *name, Image: *image, From: *from, VCPUs: *cpus, MemMiB: *mem,
		TTLSeconds: int(ttl.Seconds()), OnTTL: *onTTL, Egress: *egress, AllowDomains: splitDomains(*allow),
		CPUPct: *cpuPct, Volumes: []api.VolumeAttachment(volumes), Shares: shareSpecs}
	mc, err := client.CreateSandbox(ctx, req)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Println(mc.ID)
		return nil
	}
	// Lo que se enseña es hasta que el agente contesta, que es lo que espera
	// quien lo crea; BootMS solo mide el arranque del VMM.
	how := "booted cold"
	if mc.From != "" {
		how = "restored from " + mc.From
	}
	final := "destroyed"
	if mc.OnTTL == api.OnTTLFreeze {
		final = "frozen"
	}
	fmt.Printf("%s  %s  ready in %s (%s), %s after %s idle\n",
		mc.ID[:12], mc.Name, time.Since(start).Round(time.Millisecond), how,
		final, time.Duration(mc.TTLSeconds)*time.Second)
	fmt.Printf("\n  kling exec %s -- uname -a\n  kling cp ./script.py %s:/tmp/\n  kling sandbox rm %s\n",
		mc.Name, mc.Name, mc.Name)
	return nil
}

func sandboxList(args []string) error {
	fs := flag.NewFlagSet("sandbox ls", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	list, err := api.NewClient(hostOf(*host)).Sandboxes(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tEGRESS\tON TTL\tIN")
	for _, mc := range list {
		left := "—"
		// El reloj del TTL es TTLAt, no StartedAt: un sandbox que durmió y
		// despertó conserva su cuenta (ver api.Machine.TTLAt).
		desde := mc.TTLAt
		if desde == nil {
			desde = mc.StartedAt
		}
		if desde != nil && mc.TTLSeconds > 0 {
			d := time.Until(desde.Add(time.Duration(mc.TTLSeconds) * time.Second)).Round(time.Second)
			if d < 0 {
				d = 0
			}
			left = d.String()
		}
		fin := "remove"
		if mc.OnTTL == api.OnTTLFreeze {
			fin = "freeze"
		}
		src := mc.Image
		if mc.From != "" {
			src = mc.From + " (snapshot)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", mc.ID[:12], mc.Name, src, mc.State, mc.Egress, fin, left)
	}
	return tw.Flush()
}

func sandboxRenew(args []string) error {
	fs := flag.NewFlagSet("sandbox renew", flag.ExitOnError)
	host := hostFlag(fs)
	ttl := fs.Duration("ttl", 0, "new lifetime from now (default 10m)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling sandbox renew <sandbox> [-ttl 30m]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	mc, err := api.NewClient(hostOf(*host)).RenewSandbox(ctx, fs.Arg(0), int(ttl.Seconds()))
	if err != nil {
		return err
	}
	desde := mc.TTLAt
	if desde == nil {
		desde = mc.StartedAt
	}
	fmt.Printf("%s  %s at %s\n", mc.Name, map[bool]string{true: "freezes", false: "expires"}[mc.OnTTL == api.OnTTLFreeze],
		desde.Add(time.Duration(mc.TTLSeconds)*time.Second).Local().Format("15:04:05"))
	return nil
}

func sandboxRemove(args []string) error {
	fs := flag.NewFlagSet("sandbox rm", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: kling sandbox rm <sandbox>...")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	var failed int
	for _, ref := range fs.Args() {
		if err := c.RemoveSandbox(ctx, ref); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", ref, err)
			failed++
			continue
		}
		fmt.Println(ref)
	}
	if failed > 0 {
		return fmt.Errorf("%d sandbox(es) not removed", failed)
	}
	return nil
}
