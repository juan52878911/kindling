package main

// kling proc y kling session: procesos en segundo plano y sesiones con estado
// dentro de una máquina con exec (docs/exec-sandbox.md). El lanzamiento es
// `kling exec -d` (exec.go); aquí se listan, se leen, se esperan y se señalan.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdProc es `kling proc ls|status|out|wait|signal|rm`. Devuelve el código de
// salida del proceso en wait y en out -f (como exec), y 0 en el resto.
func cmdProc(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUso("usage: kling proc <ls|status|out|wait|signal|rm> <machine> [pid] [...]   (start one with: kling exec -d <machine> -- <cmd>)")
	}
	rest := args[1:]
	switch args[0] {
	case "ls", "list":
		return 0, procList(rest)
	case "status", "get":
		return 0, procStatus(rest)
	case "out", "output", "logs":
		return procOut(rest)
	case "wait":
		return procWait(rest)
	case "signal", "kill":
		return 0, procSignal(rest)
	case "rm", "remove":
		return 0, procRemove(rest)
	}
	return 2, errUsoDe("proc", fmt.Errorf("unknown subcommand %q: use ls, status, out, wait, signal or rm", args[0]))
}

// refYPid separa "<máquina> <pid>" de lo que queda tras los flags.
func refYPid(fs *flag.FlagSet, uso string) (string, int, error) {
	a := fs.Args()
	if len(a) != 2 {
		return "", 0, errUso("usage: kling proc %s", uso)
	}
	pid, err := strconv.Atoi(a[1])
	if err != nil || pid <= 0 {
		return "", 0, errUso("invalid pid %q", a[1])
	}
	return a[0], pid, nil
}

func estadoCorto(p api.ProcessInfo) string {
	switch {
	case p.State == api.ProcessRunning:
		return "running"
	case p.Signal != "":
		return "killed " + p.Signal
	case p.ExitCode != nil:
		return "exit " + strconv.Itoa(*p.ExitCode)
	}
	return p.State
}

func procList(args []string) error {
	fs := flag.NewFlagSet("proc ls", flag.ContinueOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only pids")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling proc ls [-q] [-json] <machine>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	list, err := api.NewClient(hostOf(*host)).Processes(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		if list == nil {
			list = []api.ProcessInfo{}
		}
		return json.NewEncoder(os.Stdout).Encode(list)
	}
	if *quiet {
		for _, p := range list {
			fmt.Println(p.PID)
		}
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "PID\tSTATE\tSTARTED\tOUTPUT\tCOMMAND")
	for _, p := range list {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d B\t%s\n", p.PID, estadoCorto(p),
			time.Since(p.StartedAt).Round(time.Second).String()+" ago", p.OutputBytes, strings.Join(p.Cmd, " "))
	}
	return tw.Flush()
}

func procStatus(args []string) error {
	fs := flag.NewFlagSet("proc status", flag.ContinueOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	ref, pid, err := refYPid(fs, "status [-json] <machine> <pid>")
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	p, err := api.NewClient(hostOf(*host)).Process(ctx, ref, pid, 0)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(p)
	}
	fmt.Printf("pid %d: %s\n", p.PID, estadoCorto(*p))
	return nil
}

// volcarProceso imprime la salida de un proceso a stdout/stderr y devuelve el
// código de salida si el flujo lo trajo.
func volcarProceso(c *api.Client, ref string, pid int, offset int64, follow bool) (*int, int64, error) {
	ctx, stop := ctxWithSignals()
	defer stop()
	perdido := false
	exit, next, err := c.ProcessOutput(ctx, ref, pid, offset, follow, func(ev api.ProcessEvent) error {
		if ev.Dropped > 0 && !perdido {
			perdido = true
			fmt.Fprintf(os.Stderr, "kling: %d byte(s) of older output were dropped (the ring buffer is bounded)\n", ev.Dropped)
		}
		var err error
		if ev.Stream == "stderr" {
			_, err = os.Stderr.Write(ev.Data)
		} else {
			_, err = os.Stdout.Write(ev.Data)
		}
		return err
	})
	return exit, next, err
}

func procOut(args []string) (int, error) {
	fs := flag.NewFlagSet("proc out", flag.ContinueOnError)
	host := hostFlag(fs)
	follow := fs.Bool("f", false, "follow: keep reading until the process ends, then exit with its code")
	offset := fs.Int64("offset", 0, "start at this output offset (bytes; as printed by -next)")
	next := fs.Bool("next", false, "print the offset to continue from on stderr when done")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return codigoDeSalida(err), err
	}
	ref, pid, err := refYPid(fs, "out [-f] [-offset N] [-next] <machine> <pid>")
	if err != nil {
		return 2, err
	}
	exit, n, err := volcarProceso(api.NewClient(hostOf(*host)), ref, pid, *offset, *follow)
	if err != nil {
		return codigoDeSalida(err), err
	}
	if *next {
		fmt.Fprintf(os.Stderr, "next offset: %d\n", n)
	}
	if *follow && exit != nil {
		return *exit, nil
	}
	return 0, nil
}

func procWait(args []string) (int, error) {
	fs := flag.NewFlagSet("proc wait", flag.ContinueOnError)
	host := hostFlag(fs)
	timeout := fs.Duration("timeout", 0, "give up after this long (exit 124); default: wait forever")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return codigoDeSalida(err), err
	}
	ref, pid, err := refYPid(fs, "wait [-timeout D] <machine> <pid>")
	if err != nil {
		return 2, err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	fin := time.Now().Add(*timeout)
	for {
		paso := 5 * time.Minute
		if *timeout > 0 {
			if left := time.Until(fin); left <= 0 {
				fmt.Fprintf(os.Stderr, "kling: pid %d still running after %s\n", pid, *timeout)
				return 124, nil
			} else if left < paso {
				paso = left
			}
		}
		p, err := c.Process(ctx, ref, pid, paso)
		if err != nil {
			return codigoDeSalida(err), err
		}
		if p.State == api.ProcessExited {
			if p.ExitCode != nil {
				return *p.ExitCode, nil
			}
			return 0, nil
		}
	}
}

func procSignal(args []string) error {
	fs := flag.NewFlagSet("proc signal", flag.ContinueOnError)
	host := hostFlag(fs)
	sig := fs.String("s", "TERM", "signal: name (TERM, KILL, INT, HUP, USR1...) or number; goes to the whole process group")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	ref, pid, err := refYPid(fs, "signal [-s TERM] <machine> <pid>")
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	_, err = api.NewClient(hostOf(*host)).SignalProcess(ctx, ref, pid, *sig)
	return err
}

func procRemove(args []string) error {
	fs := flag.NewFlagSet("proc rm", flag.ContinueOnError)
	host := hostFlag(fs)
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	ref, pid, err := refYPid(fs, "rm <machine> <pid>   (kills it if running, and forgets it with its output)")
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	return api.NewClient(hostOf(*host)).RemoveProcess(ctx, ref, pid)
}

// cmdSession es `kling session create|ls|show|rm`.
func cmdSession(args []string) error {
	if len(args) == 0 {
		return errUso("usage: kling session <create|ls|show|rm> <machine> [id] [...]   (use one with: kling exec -s <id> ...)")
	}
	rest := args[1:]
	switch args[0] {
	case "create", "new":
		return sessionCreate(rest)
	case "ls", "list":
		return sessionList(rest)
	case "show", "get":
		return sessionShow(rest)
	case "rm", "remove":
		return sessionRemove(rest)
	}
	return errUsoDe("session", fmt.Errorf("unknown subcommand %q: use create, ls, show or rm", args[0]))
}

func sessionCreate(args []string) error {
	fs := flag.NewFlagSet("session create", flag.ContinueOnError)
	host := hostFlag(fs)
	id := fs.String("id", "", "session id (default: generated)")
	dir := fs.String("w", "", "initial working directory")
	ttl := fs.Duration("ttl", 0, "idle time after which it expires (default 1h, max 24h; each exec renews it)")
	asJSON := fs.Bool("json", false, "print the session as JSON")
	var ef envFlags
	ef.register(fs)
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling session create [-id ID] [-w DIR] [-e K=V] [-ttl 1h] [-json] <machine>")
	}
	env, err := ef.resolve()
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	s, err := api.NewClient(hostOf(*host)).CreateSession(ctx, fs.Arg(0), api.SessionRequest{ID: *id, Dir: *dir, Env: env, TTLSeconds: int(ttl.Seconds())})
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	fmt.Println(s.ID)
	return nil
}

func sessionList(args []string) error {
	fs := flag.NewFlagSet("session ls", flag.ContinueOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling session ls [-json] <machine>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	list, err := api.NewClient(hostOf(*host)).Sessions(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		if list == nil {
			list = []api.Session{}
		}
		return json.NewEncoder(os.Stdout).Encode(list)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tDIR\tEXECS\tEXPIRES IN")
	for _, s := range list {
		dir := s.Dir
		if dir == "" {
			dir = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.ID, dir, s.Execs, time.Until(s.ExpiresAt).Round(time.Second))
	}
	return tw.Flush()
}

func sessionShow(args []string) error {
	fs := flag.NewFlagSet("session show", flag.ContinueOnError)
	host := hostFlag(fs)
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errUso("usage: kling session show <machine> <id>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	s, err := api.NewClient(hostOf(*host)).GetSession(ctx, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}

func sessionRemove(args []string) error {
	fs := flag.NewFlagSet("session rm", flag.ContinueOnError)
	host := hostFlag(fs)
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errUso("usage: kling session rm <machine> <id>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	return api.NewClient(hostOf(*host)).RemoveSession(ctx, fs.Arg(0), fs.Arg(1))
}

// lanzarEnSegundoPlano es `kling exec -d`: arranca el proceso y escribe su pid.
func lanzarEnSegundoPlano(c *api.Client, ref string, req api.ExecRequest, session string, asJSON bool) (int, error) {
	ctx, stop := ctxWithSignals()
	defer stop()
	p, err := c.StartProcess(ctx, ref, api.ProcessStartRequest{
		Cmd: req.Cmd, Dir: req.Dir, Env: req.Env, Session: session,
		TimeoutSeconds: req.TimeoutSeconds, MaxOutputBytes: req.MaxOutputBytes,
	})
	if err != nil {
		return codigoDeSalida(err), err
	}
	if asJSON {
		return 0, json.NewEncoder(os.Stdout).Encode(p)
	}
	fmt.Println(p.PID)
	return 0, nil
}
