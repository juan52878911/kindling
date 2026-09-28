// kling-dbbench mide, en el MISMO host, cuánto tarda en estar lista una base de
// datos Postgres desechable con su esquema y sus datos: desde "pido la base"
// hasta que SELECT count(*) sobre la tabla del seed devuelve el valor
// esperado, con N copias pedidas a la vez. Compara tres formas:
//
//	kindling-run   kling run -from <golden>       (restaurar una microVM)
//	kindling-fork  kling sandbox fork <src> -n N  (ramificar una microVM viva)
//	docker         docker run postgres + esperar + esquema y datos
//	template       CREATE DATABASE ... TEMPLATE ... en un Postgres ya arrancado
//
// El método y las reglas de honestidad son los de docs/thaw-at-scale.md: toda
// ronda va al informe, DEGRADED por encima del 1 % de fallos, las celdas que
// no caben se saltan diciendo por qué, y no se repite nada hasta que salga
// bonito. Solo biblioteca estándar; el cliente Postgres es internal/pgmini.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed seed.sql
var embeddedSeed string

// config es todo lo que se decide por línea de comandos.
type config struct {
	Modes   []string
	Ns      []int
	Reps    int
	Timeout time.Duration

	Kling        string
	Golden       string
	ForkSrc      string
	KlingRunArgs []string
	PGPort       int

	User        string
	PasswordEnv string
	DB          string
	Table       string
	Expect      int64
	SeedFile    string
	seedText    string

	Docker          string
	DockerImage     string
	TemplateAddr    string
	TemplateAdminDB string

	DiskPath   string
	EstMemMiB  int
	EstDiskMiB int
	PSIMax     float64
	Pause      time.Duration

	Prefix   string
	RunID    string
	JSONPath string
	MDPath   string
}

func (c *config) countSQL() string { return "SELECT count(*) FROM " + c.Table }
func (c *config) seedSQL() string  { return c.seedText }
func (c *config) password() string { return os.Getenv(c.PasswordEnv) }
func (c *config) target(addr string) target {
	return target{Addr: addr, User: c.User, Password: c.password(), Database: c.DB}
}

var (
	reIdent  = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,30}$`)
	reModes  = map[string]bool{"kindling-run": true, "kindling-fork": true, "docker": true, "template": true}
	reRunTag = regexp.MustCompile(`[^a-z0-9]`)
)

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseFlags(args []string, stderr io.Writer) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("kling-dbbench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	modes := fs.String("modes", "kindling-run,docker,template", "modes to measure: kindling-run, kindling-fork, docker, template")
	ns := fs.String("n", "1,8,32", "copies asked at once (comma-separated)")
	fs.IntVar(&c.Reps, "r", 3, "repetitions per cell (every one goes in the report)")
	fs.DurationVar(&c.Timeout, "timeout", 60*time.Second, "per-copy deadline, from the release of the burst")
	fs.StringVar(&c.Kling, "kling", "kling", "kling binary")
	fs.StringVar(&c.Golden, "golden", "", "kindling-run: template with Postgres running and the seed loaded")
	fs.StringVar(&c.ForkSrc, "fork-src", "", "kindling-fork: live sandbox (with the seed) to branch")
	runArgs := fs.String("kling-run-args", "", "extra flags for `kling run`, comma-separated (e.g. -mem,256M)")
	fs.IntVar(&c.PGPort, "pg-port", 5432, "Postgres port inside the kindling guest")
	fs.StringVar(&c.User, "user", "postgres", "database role")
	fs.StringVar(&c.PasswordEnv, "password-env", "DBBENCH_PASSWORD", "environment variable holding the password (never a flag)")
	fs.StringVar(&c.DB, "db", "postgres", "database that holds the seed table")
	fs.StringVar(&c.Table, "table", "items", "seed table counted by the first query")
	fs.Int64Var(&c.Expect, "expect", 100000, "expected count(*) of the seed table")
	fs.StringVar(&c.SeedFile, "seed-sql", "", "schema+seed SQL for docker and template (default: embedded, 100000 rows in items)")
	fs.StringVar(&c.Docker, "docker", "docker", "docker binary")
	fs.StringVar(&c.DockerImage, "docker-image", "postgres:16-alpine", "image for the docker mode")
	fs.StringVar(&c.TemplateAddr, "template-addr", "", "template: host:port of a running Postgres")
	fs.StringVar(&c.TemplateAdminDB, "template-admin-db", "postgres", "template: database used to issue CREATE DATABASE")
	fs.StringVar(&c.DiskPath, "disk-path", "", "a path on the filesystem where copies live, to measure disk per copy (empty: not measured)")
	fs.IntVar(&c.EstMemMiB, "est-mem-mib", 0, "estimated RAM per copy for the skip rule (0: per-mode default)")
	fs.IntVar(&c.EstDiskMiB, "est-disk-mib", 0, "estimated disk per copy for the skip rule (0: per-mode default)")
	fs.Float64Var(&c.PSIMax, "psi-max", 5, "memory PSI some avg10 needed to start (and noted per round)")
	fs.DurationVar(&c.Pause, "pause", 2*time.Second, "pause between rounds")
	fs.StringVar(&c.Prefix, "prefix", "dbb", "prefix of everything the bench creates (lowercase, digits, _)")
	fs.StringVar(&c.JSONPath, "json", "dbbench.json", "JSON report path")
	fs.StringVar(&c.MDPath, "md", "", "also write the Markdown table to this file (it always goes to stdout)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	for _, m := range splitList(*modes) {
		if !reModes[m] {
			return nil, fmt.Errorf("unknown mode %q", m)
		}
		c.Modes = append(c.Modes, m)
	}
	if len(c.Modes) == 0 {
		return nil, errors.New("no modes")
	}
	for _, s := range splitList(*ns) {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 64 { // 64 = api.ForkMax
			return nil, fmt.Errorf("invalid N %q (1..64)", s)
		}
		c.Ns = append(c.Ns, n)
	}
	if len(c.Ns) == 0 || c.Reps < 1 {
		return nil, errors.New("need at least one N and -r >= 1")
	}
	if !reIdent.MatchString(c.Table) || !reIdent.MatchString(c.Prefix) {
		return nil, errors.New("-table and -prefix must match [a-z_][a-z0-9_]*")
	}
	c.KlingRunArgs = splitList(*runArgs)
	c.seedText = embeddedSeed
	if c.SeedFile != "" {
		b, err := os.ReadFile(c.SeedFile)
		if err != nil {
			return nil, err
		}
		c.seedText = string(b)
	}
	c.RunID = "t" + reRunTag.ReplaceAllString(strings.ToLower(time.Now().UTC().Format("0102150405")), "")
	return c, nil
}

// estimates devuelve la RAM y el disco estimados por copia. Son suposiciones
// conservadoras, no medidas: se pueden cambiar con -est-mem-mib y
// -est-disk-mib, y viajan en el informe.
func (c *config) estimates(mode string) (memMiB, diskMiB int) {
	memMiB, diskMiB = c.EstMemMiB, c.EstDiskMiB
	switch mode {
	case "kindling-run", "kindling-fork":
		if memMiB == 0 {
			memMiB = 256
		}
		if diskMiB == 0 {
			diskMiB = 128
		}
	case "docker":
		if memMiB == 0 {
			memMiB = 200
		}
		if diskMiB == 0 {
			diskMiB = 300
		}
	default: // template: la memoria es de un servidor que ya estaba
		if diskMiB == 0 {
			diskMiB = 64
		}
	}
	return
}

// skipReason dice por qué una celda no cabe, o "" si cabe. Reglas de
// docs/thaw-at-scale.md: la estimación no puede pasar del 70 % de lo que
// haya. availMiB o diskAvailMiB < 0 significan "no medible": esa regla no
// se aplica (y el preflight lo dice).
func skipReason(n, estMemMiB, estDiskMiB int, availMiB, diskAvailMiB int64) string {
	if need := int64(n) * int64(estMemMiB); availMiB >= 0 && need*10 > availMiB*7 {
		return fmt.Sprintf("N=%d × %d MiB = %d MiB > 70%% of MemAvailable %d MiB", n, estMemMiB, need, availMiB)
	}
	if need := int64(n) * int64(estDiskMiB); diskAvailMiB >= 0 && need*10 > diskAvailMiB*7 {
		return fmt.Sprintf("N=%d × %d MiB = %d MiB > 70%% of free disk %d MiB", n, estDiskMiB, need, diskAvailMiB)
	}
	return ""
}

// newBackend elige la implementación de un modo.
func newBackend(mode string, c *config) backend {
	switch mode {
	case "kindling-run":
		return &klingBackend{cfg: c, run: runCmd}
	case "kindling-fork":
		return &klingBackend{cfg: c, run: runCmd, fork: true}
	case "docker":
		return &dockerBackend{cfg: c, run: runCmd}
	}
	return &templateBackend{cfg: c}
}

// waitCalm espera a que el PSI baje de max; false si no bajó en d. Sin PSI
// (no es Linux) devuelve true.
func waitCalm(ctx context.Context, max float64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		p := psiSome()
		if p < 0 || p < max {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

func preflight(ctx context.Context, c *config, rep *Report) error {
	note := func(f string, a ...any) { rep.Preflight = append(rep.Preflight, fmt.Sprintf(f, a...)) }
	for _, m := range c.Modes {
		bin := ""
		switch m {
		case "kindling-run", "kindling-fork":
			bin = c.Kling
		case "docker":
			bin = c.Docker
		}
		if bin != "" {
			if _, err := exec.LookPath(bin); err != nil {
				return fmt.Errorf("mode %s: %w", m, err)
			}
		}
	}
	if a := memAvailableMiB(); a >= 0 {
		note("MemAvailable %d MiB", a)
	} else {
		note("MemAvailable not readable (not Linux): the RAM skip rule and RAM columns are off")
	}
	if c.DiskPath != "" {
		_, av, err := diskUsage(c.DiskPath)
		if err != nil {
			return fmt.Errorf("-disk-path: %w", err)
		}
		note("disk-path %s: %d MiB free", c.DiskPath, av>>20)
	} else {
		note("no -disk-path: disk per copy is not measured and the disk skip rule is off")
	}
	if p := psiSome(); p < 0 {
		note("PSI not readable: pressure is not checked")
	} else if !waitCalm(ctx, c.PSIMax, 2*time.Minute) {
		return fmt.Errorf("memory PSI some avg10 stays at %.2f (>= %.2f): the host is not calm enough to measure", psiSome(), c.PSIMax)
	} else {
		note("PSI some avg10 %.2f (limit %.2f)", psiSome(), c.PSIMax)
	}
	if !strings.Contains(c.seedText, c.Table) && (contains(c.Modes, "docker") || contains(c.Modes, "template")) {
		note("WARNING: the seed SQL does not mention table %q", c.Table)
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// runAll recorre modos × N × repeticiones. Devuelve lo hecho hasta el final o
// hasta que se cancele el contexto.
func runAll(ctx context.Context, c *config, rep *Report) {
	for _, mode := range c.Modes {
		b := newBackend(mode, c)
		if err := b.setup(ctx); err != nil {
			for _, n := range c.Ns {
				rep.Rounds = append(rep.Rounds, Round{Mode: mode, N: n, Status: "skipped", Reason: "setup failed: " + err.Error()})
			}
			warnf("%s: setup failed: %v", mode, err)
			continue
		}
		func() {
			defer func() {
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
				defer cancel()
				b.teardown(cctx)
			}()
			for _, n := range c.Ns {
				em, ed := c.estimates(mode)
				diskAvail := int64(-1)
				if c.DiskPath != "" {
					if _, av, err := diskUsage(c.DiskPath); err == nil {
						diskAvail = av >> 20
					}
				}
				if why := skipReason(n, em, ed, memAvailableMiB(), diskAvail); why != "" {
					rep.Rounds = append(rep.Rounds, Round{Mode: mode, N: n, Status: "skipped", Reason: why})
					warnf("%s N=%d: skipped: %s", mode, n, why)
					continue
				}
				for r := 1; r <= c.Reps; r++ {
					if ctx.Err() != nil {
						rep.Interrupted = true
						return
					}
					waitCalm(ctx, c.PSIMax, 2*time.Minute) // si no baja, la ronda lo anota
					round := runRound(ctx, b, c, n, r)
					rep.Rounds = append(rep.Rounds, round)
					warnf("%s N=%d rep %d: %s ok=%d failed=%d p50=%.1fms max=%.1fms", mode, n, r, round.Status, round.OK, round.Failed, round.Latency.P50, round.Latency.Max)
					time.Sleep(c.Pause)
				}
			}
		}()
	}
	if ctx.Err() != nil {
		rep.Interrupted = true
	}
}

func writeFile(path string, f func(io.Writer)) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	f(fh)
	return fh.Close()
}

func run(args []string) int {
	c, err := parseFlags(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep := &Report{Schema: schemaVersion, Started: time.Now(), Host: hostInfo(c.Kling, c.Docker)}
	seed := "embedded"
	if c.SeedFile != "" {
		seed = c.SeedFile
	}
	rep.Config = Public{Modes: c.Modes, Ns: c.Ns, Reps: c.Reps, TimeoutS: c.Timeout.Seconds(), Golden: c.Golden, ForkSrc: c.ForkSrc,
		DockerImage: c.DockerImage, Table: c.Table, Expect: c.Expect, SeedSQL: seed, DiskPaths: c.DiskPath,
		PSIMax: c.PSIMax, EstMemMiB: c.EstMemMiB, EstDiskMiB: c.EstDiskMiB}
	if err := preflight(ctx, c, rep); err != nil {
		fmt.Fprintln(os.Stderr, "preflight:", err)
		return 1
	}
	runAll(ctx, c, rep)
	rep.Finished = time.Now()
	rep.Cells = buildCells(rep.Rounds)

	code := 0
	if err := writeFile(c.JSONPath, func(w io.Writer) {
		e := json.NewEncoder(w)
		e.SetIndent("", "  ")
		_ = e.Encode(rep)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		code = 1
	}
	writeMarkdown(os.Stdout, rep)
	if c.MDPath != "" {
		if err := writeFile(c.MDPath, func(w io.Writer) { writeMarkdown(w, rep) }); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			code = 1
		}
	}
	if rep.Interrupted && code == 0 {
		code = 130
	}
	return code
}

func main() { os.Exit(run(os.Args[1:])) }
