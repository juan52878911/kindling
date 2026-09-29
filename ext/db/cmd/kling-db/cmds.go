package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/scram"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// Sustituibles en los tests.
var (
	generatePassword = scram.GeneratePassword
	newVerifier      = scram.NewVerifier
)

// ── flags ────────────────────────────────────────────────────────────────────

// newFlags crea el FlagSet de un comando con los flags comunes.
func newFlags(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet("db "+name, flag.ContinueOnError)
	host := fs.String("H", "", "daemon endpoint (socket or ssh://user@host); default: KLING_HOST or the active context")
	owner := fs.String("owner", defaultOwner, "owner of the copies (label "+labelOwner+")")
	return fs, host, owner
}

// parse admite flags antes y después de los argumentos (`db up tpl -name x`).
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, &plugin.ExitError{Code: 2, Err: err}
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func usageErr(format string, a ...any) error {
	return &plugin.ExitError{Code: 2, Err: fmt.Errorf(format, a...)}
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ── up ───────────────────────────────────────────────────────────────────────

func cmdUp(args []string) error {
	fs, host, owner := newFlags("up")
	name := fs.String("name", "", "name of the copy (default: <template>-<random>)")
	ttl := fs.Duration("ttl", 0, "lifetime of the copy (freezes when it runs out; 0 = none)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db up <template> [-name N] [-ttl D] [-owner T]")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	mc, err := a.up(ctx, pos[0], *name, *ttl, *owner)
	if err != nil {
		return err
	}
	a.printReady(mc)
	return nil
}

// up crea una copia de golden y la deja lista. Si algo falla después de
// crearla, la destruye: una copia a medias no se entrega nunca.
func (a *app) up(ctx context.Context, golden, name string, ttl time.Duration, owner string) (*api.Machine, error) {
	return a.upFrom(ctx, golden, golden, name, ttl, owner, nil)
}

// upFrom es up con la plantilla de la que se instancia (tpl) separada de la
// etiqueta kling.db.golden (golden), y etiquetas extra: un punto de guardado
// (kling db undo) es una plantilla propia, pero la copia sigue siendo de su
// golden original.
func (a *app) upFrom(ctx context.Context, tpl, golden, name string, ttl time.Duration, owner string, extra [][2]string) (*api.Machine, error) {
	if !namePattern.MatchString(tpl) || !namePattern.MatchString(golden) {
		return nil, fmt.Errorf("invalid template name %q", tpl)
	}
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	if name == "" {
		base := golden
		if len(base) > 56 {
			base = base[:56]
		}
		name = base + "-" + randomSuffix()
	}
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid name %q", name)
	}
	if ttl < 0 || (ttl > 0 && ttl < time.Second) {
		return nil, errors.New("-ttl must be at least 1s")
	}
	snap, err := a.template(ctx, tpl)
	if err != nil {
		return nil, err
	}
	role, db, err := goldenRoleDB(snap)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", tpl, err)
	}

	// La copia NACE en preparing: run -from fusiona estas etiquetas sobre las
	// de la plantilla, y las de aquí ganan. kind=sandbox es lo que permite
	// ramificarla luego (sandbox fork solo acepta sandboxes); kling.ports hace
	// que el backend de macOS abra el reenvío del 5432.
	labels := [][2]string{
		{labelState, statePreparing},
		{labelOwner, owner},
		{labelGolden, golden},
		{labelRole, role},
		{labelDatabase, db},
		{api.LabelKind, api.KindSandbox},
		{api.LabelPorts, mergePorts(snap.Labels[api.LabelPorts])},
	}
	labels = append(labels, extra...)
	runArgs := []string{"run", "-from", tpl, "-name", name}
	if ttl > 0 {
		runArgs = append(runArgs, "-ttl", strconv.Itoa(int(ttl.Seconds())))
	}
	for _, l := range labels {
		runArgs = append(runArgs, "-label", l[0]+"="+l[1])
	}
	fmt.Fprintf(a.stderr, "creating %s from %s...\n", name, tpl)
	if _, err := a.k.Run(ctx, nil, runArgs...); err != nil {
		// No se borra nada por nombre: si el run falló porque el nombre ya
		// existía, esa máquina es de otro.
		return nil, err
	}
	mc, err := a.inspect(ctx, name)
	if err != nil {
		a.destroy(name)
		return nil, err
	}
	if mc.Name != name || mc.Labels[labelState] != statePreparing {
		a.destroy(mc.ID)
		return nil, fmt.Errorf("kling run -from %s did not return the copy it was asked for", tpl)
	}
	if err := a.prepare(ctx, mc); err != nil {
		a.destroy(mc.ID)
		return nil, fmt.Errorf("%s was removed: %w", name, err)
	}
	if err := a.setState(ctx, mc.ID, stateReady); err != nil {
		a.destroy(mc.ID)
		return nil, fmt.Errorf("%s was removed: marking it ready: %w", name, err)
	}
	mc.Labels[labelState] = stateReady
	return mc, nil
}

func (a *app) printReady(mc *api.Machine) {
	_, db, _ := roleDB(mc.Labels)
	fmt.Fprintf(a.stdout, "%s  ready  (template %s, machine %s)\n", mc.Name, mc.Labels[labelGolden], shortID(mc.ID))
	fmt.Fprintf(a.stdout, "  inside the copy:  su -s /bin/sh postgres -c 'psql -h /run/postgresql %s'\n", db)
	fmt.Fprintf(a.stdout, "  from this host:   kling db connect %s [-psql | -dsn]\n", mc.Name)
}

// ── fork ─────────────────────────────────────────────────────────────────────

func cmdFork(args []string) error {
	fs, host, owner := newFlags("fork")
	n := fs.Int("n", 1, fmt.Sprintf("how many copies (1-%d)", api.ForkMax))
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db fork <copy> [-n N]")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	copies, err := a.fork(ctx, pos[0], *n, *owner)
	if err != nil {
		return err
	}
	for _, mc := range copies {
		a.printReady(mc)
	}
	return nil
}

// fork ramifica una copia lista en n copias listas. Todo o nada: si una no se
// puede preparar, se borran todas las creadas.
func (a *app) fork(ctx context.Context, src string, n int, owner string) ([]*api.Machine, error) {
	return a.forkWith(ctx, src, n, owner, nil)
}

// forkWith es fork con etiquetas extra que nacen con las copias (pisan las
// heredadas del origen: kling db branch pone así su repo y su rama).
func (a *app) forkWith(ctx context.Context, src string, n int, owner string, extra [][2]string) ([]*api.Machine, error) {
	if n < 1 || n > api.ForkMax {
		return nil, fmt.Errorf("-n must be between 1 and %d", api.ForkMax)
	}
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	mc, err := a.inspect(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := owned(mc, owner); err != nil {
		return nil, err
	}
	if st := mc.Labels[labelState]; st != stateReady {
		return nil, fmt.Errorf("%s is not ready (%s=%q)", mc.Name, labelState, st)
	}
	// El núcleo solo ramifica máquinas en marcha (puedeRamificarse).
	switch mc.State {
	case api.StateRunning:
	case api.StateWarm, api.StatePaused:
		fmt.Fprintf(a.stderr, "%s is %s: thawing it to fork it\n", mc.Name, mc.State)
		if _, err := a.k.Run(ctx, nil, "thaw", mc.ID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s is %s: only a running or frozen copy can be forked", mc.Name, mc.State)
	}

	forkArgs := []string{"sandbox", "fork", mc.ID, "-n", strconv.Itoa(n), "-label", labelState + "=" + statePreparing}
	for _, l := range extra {
		forkArgs = append(forkArgs, "-label", l[0]+"="+l[1])
	}
	out, err := a.k.Run(ctx, nil, append(forkArgs, "-json")...)
	if err != nil {
		// El daemon deshace el fork entero si una copia falla.
		return nil, err
	}
	var res api.ForkResult
	if err := json.Unmarshal(out, &res); err != nil || len(res.Sandboxes) == 0 {
		return nil, fmt.Errorf("kling sandbox fork: unexpected answer (%v); the copies of %s, if any, were not rotated "+
			"and have no password here, so connect refuses them: remove them with kling rm", err, mc.Name)
	}
	copies := res.Sandboxes
	undo := func(cause error) ([]*api.Machine, error) {
		for _, c := range copies {
			a.destroy(c.ID)
		}
		return nil, fmt.Errorf("fork of %s undone, %d copies removed: %w", mc.Name, len(copies), cause)
	}
	// Las copias ya nacen en preparing (el fork lleva la etiqueta desde su
	// nacimiento). Si un daemon antiguo la ignorase, se marca aquí, antes de
	// nada más: fuera el ready heredado del origen.
	for _, c := range copies {
		if c.Labels[labelState] != statePreparing {
			if err := a.setState(ctx, c.ID, statePreparing); err != nil {
				return undo(fmt.Errorf("marking %s as preparing: %w", c.Name, err))
			}
		}
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		for k, v := range mc.Labels {
			if _, ok := c.Labels[k]; !ok {
				c.Labels[k] = v
			}
		}
		c.Labels[labelState] = statePreparing
		for _, l := range extra {
			c.Labels[l[0]] = l[1]
		}
	}
	for _, c := range copies {
		if err := a.prepare(ctx, c); err != nil {
			return undo(err)
		}
	}
	for _, c := range copies {
		if err := a.setState(ctx, c.ID, stateReady); err != nil {
			return undo(fmt.Errorf("marking %s ready: %w", c.Name, err))
		}
		c.Labels[labelState] = stateReady
	}
	return copies, nil
}

// ── connect ──────────────────────────────────────────────────────────────────

func cmdConnect(args []string) error {
	fs, host, owner := newFlags("connect")
	dsn := fs.Bool("dsn", false, "print a DSN WITH the password (asks first if stdout is a terminal)")
	psql := fs.Bool("psql", false, "open the host's psql on the copy (password through the environment)")
	role := fs.String("role", "", "connect as this role made by kling db role (default: the application role)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db connect <copy> [-role R] [-dsn | -psql]")
	}
	if *dsn && *psql {
		return usageErr("-dsn and -psql exclude each other")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	mode := "info"
	switch {
	case *dsn:
		mode = "dsn"
	case *psql:
		mode = "psql"
	}
	return a.connectAs(ctx, pos[0], *owner, mode, *role)
}

func (a *app) connect(ctx context.Context, ref, owner, mode string) error {
	return a.connectAs(ctx, ref, owner, mode, "")
}

// connectAs es connect con un rol elegido ("" = el de la aplicación).
func (a *app) connectAs(ctx context.Context, ref, owner, mode, extraRole string) error {
	if err := validOwner(owner); err != nil {
		return err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if err := checkReady(mc, owner); err != nil {
		return err
	}
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return err
	}
	h, port, err := hostAddr(mc)
	if err != nil {
		return err
	}
	pwPath, _ := dbstate.PasswordPath(mc.ID)
	readPW := func() (string, error) { return dbstate.ReadPassword(mc.ID) }
	if extraRole != "" {
		if err := validRoleName(extraRole, role); err != nil {
			return err
		}
		role = extraRole
		pwPath, _ = dbstate.RolePasswordPath(mc.ID, role)
		readPW = func() (string, error) {
			pw, err := dbstate.ReadRolePassword(mc.ID, role)
			if errors.Is(err, dbstate.ErrNoPassword) {
				return "", fmt.Errorf("this host has no password for role %s of %s (kling db role %s -ro -name %s creates it)", role, mc.Name, mc.Name, role)
			}
			return pw, err
		}
		// Sin contraseña no hay nada que entregar: falla antes de preguntar.
		if _, err := readPW(); err != nil {
			return err
		}
	}

	switch mode {
	case "dsn":
		if a.stdoutTTY() && !a.confirm(fmt.Sprintf("This prints the password of %s to the terminal. Continue? [y/N] ", mc.Name)) {
			return errors.New("aborted: nothing printed (pipe it, e.g. kling db connect " + mc.Name + " -dsn | pbcopy)")
		}
		pw, err := readPW()
		if err != nil {
			return err
		}
		u := url.URL{Scheme: "postgres", User: url.UserPassword(role, pw),
			Host: net.JoinHostPort(h, strconv.Itoa(port)), Path: "/" + db, RawQuery: "sslmode=disable"}
		fmt.Fprintln(a.stdout, u.String())
		return nil
	case "psql":
		pw, err := readPW()
		if err != nil {
			return err
		}
		// La clave por el entorno del hijo, nunca en argv (se ve en ps).
		return a.runPsql(ctx, []string{"PGPASSWORD=" + pw, "PGSSLMODE=disable"},
			[]string{"-h", h, "-p", strconv.Itoa(port), "-U", role, "-d", db})
	}
	fmt.Fprintf(a.stdout, "%s  ready  (machine %s)\n", mc.Name, shortID(mc.ID))
	fmt.Fprintf(a.stdout, "  host      %s\n  port      %d\n  user      %s\n  database  %s\n  password  %s\n",
		h, port, role, db, pwPath)
	flagRole := ""
	if extraRole != "" {
		flagRole = " -role " + extraRole
	}
	fmt.Fprintf(a.stdout, "  kling db connect %s%s -psql   ·   kling db connect %s%s -dsn | <your tool>\n", mc.Name, flagRole, mc.Name, flagRole)
	return nil
}

// confirm pregunta por stderr y lee la respuesta de stdin.
func (a *app) confirm(q string) bool {
	fmt.Fprint(a.stderr, q)
	line, _ := bufio.NewReader(a.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "s", "si", "sí":
		return true
	}
	return false
}

func runHostPsql(ctx context.Context, env, args []string) error {
	p, err := exec.LookPath("psql")
	if err != nil {
		return errors.New("psql is not installed on this host: use kling db connect -dsn with your client")
	}
	c := exec.CommandContext(ctx, p, args...)
	c.Env = append(os.Environ(), env...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C es de psql (cancela la consulta), no nuestro.
	signal.Ignore(os.Interrupt)
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &plugin.ExitError{Code: ee.ExitCode()}
	}
	return err
}

// ── reset y rm ───────────────────────────────────────────────────────────────

func cmdReset(args []string) error {
	fs, host, owner := newFlags("reset")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db reset <copy>")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	mc, err := a.reset(ctx, pos[0], *owner)
	if err != nil {
		return err
	}
	a.printReady(mc)
	return nil
}

// reset cambia una copia por otra nueva de la misma plantilla, con el mismo
// nombre, dueño y ttl. La nueva tiene otro id y otra contraseña.
func (a *app) reset(ctx context.Context, ref, owner string) (*api.Machine, error) {
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := owned(mc, owner); err != nil {
		return nil, err
	}
	golden := mc.Labels[labelGolden]
	ttl := time.Duration(mc.TTLSeconds) * time.Second
	if err := a.remove(ctx, mc); err != nil {
		return nil, err
	}
	return a.up(ctx, golden, mc.Name, ttl, owner)
}

func cmdRm(args []string) error {
	fs, host, owner := newFlags("rm")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("usage: kling db rm <copy>...")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	for _, ref := range pos {
		mc, err := a.inspect(ctx, ref)
		if err != nil {
			return err
		}
		if err := owned(mc, *owner); err != nil {
			return err
		}
		if err := a.remove(ctx, mc); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, mc.Name)
	}
	return nil
}

// remove borra la máquina y, solo si eso fue bien, su contraseña.
func (a *app) remove(ctx context.Context, mc *api.Machine) error {
	if _, err := a.k.Run(ctx, nil, "rm", "-f", mc.ID); err != nil {
		return err
	}
	return dbstate.Remove(mc.ID)
}

// ── doctor y audit ───────────────────────────────────────────────────────────

func cmdDoctor(args []string) error {
	fs, host, owner := newFlags("doctor")
	u := fs.String("url", "", "check this Postgres instead of a copy (postgres://user:pass@host:port/db)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if (len(pos) == 1) == (*u != "") || len(pos) > 1 {
		return usageErr("usage: kling db doctor <copy> | -url postgres://...")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	t := doctorTarget{URL: *u}
	if len(pos) == 1 {
		mc, err := a.inspect(ctx, pos[0])
		if err != nil {
			return err
		}
		if err := owned(mc, *owner); err != nil {
			return err
		}
		t.Machine = mc.Name
	}
	problems, err := runDoctor(ctx, a.k, t, a.stdout)
	if err != nil {
		return err
	}
	if problems > 0 {
		return &plugin.ExitError{Code: 1, Err: fmt.Errorf("%d problem(s) found", problems)}
	}
	return nil
}

func cmdAudit(args []string) error {
	fs, host, owner := newFlags("audit")
	since := fs.Duration("since", 24*time.Hour, "how far back")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db audit <copy> [-since D] [-json]")
	}
	if *since <= 0 {
		return usageErr("-since must be positive")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	mc, err := a.inspect(ctx, pos[0])
	if err != nil {
		return err
	}
	if err := owned(mc, *owner); err != nil {
		return err
	}
	return runAudit(ctx, a.k, mc.Name, *since, *asJSON, a.stdout)
}
