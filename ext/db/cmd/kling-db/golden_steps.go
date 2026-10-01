package main

// `kling db golden build ... -step "<cmd>" -agent <plantilla> -workdir <dir>`:
// un golden cuyo esquema lo hace un PROGRAMA (alembic, prisma, flyway, un
// manage.py migrate), no solo SQL.
//
// Antes había que encadenarlo a mano (nota 18, AuraCRM): golden base → copia
// → microVM del agente → attach → exec por servicio → detach → kling save; y el
// resultado se guardaba SIN los metadatos del golden (su contraseña y su
// conn.env), así que doctor y diff no sabían qué rol mirar. Aquí:
//
//  1. db-golden.sh construye <nombre>-base con todo lo de siempre (extensiones,
//     -migrations, -init...).
//  2. Una copia de ese golden (kling db up), con su clave rotada.
//     -super-step "<cmd>" corre DENTRO de la copia, como el superusuario por el
//     socket local (con -workdir en /tmp/kdb-work): lo que en Docker hacía un
//     script como POSTGRES_USER superusuario y el rol de la app no puede.
//  3. Si hay -step: una microVM del agente (-agent, con egress allowlist) a la
//     que se sube -workdir en /work y se le da la copia por attach. Cada paso
//     corre con `sh -c` en /work con PGHOST, PGPORT, PGUSER, PGDATABASE,
//     PGSSLMODE, PGPASSWORD (un MARCADOR: la clave no entra en el agente) y
//     DATABASE_URL, más lo de -env-file. -sql FILE corre SQL en la copia como
//     el rol de la aplicación. Pasos y SQL, en el orden en que se dieron.
//  4. detach, fuera el agente, VACUUM y CHECKPOINT, ningún cliente vivo, y la
//     copia se guarda como la plantilla <nombre>, con su contraseña y su
//     conn.env como los de cualquier golden.
//  5. Se borran la copia y <nombre>-base (con -keep se quedan).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

// goldenStep es un paso tras el golden base: un comando en el agente o un
// fichero SQL en la copia.
type goldenStep struct {
	cmd   string // -step (en el agente) o -super-step (en la copia)
	sql   string // -sql
	super bool   // -super-step
}

// stepOpts son los flags de los pasos, sacados de los de db-golden.sh.
type stepOpts struct {
	steps   []goldenStep
	agent   string
	workdir string
	envFile string
	timeout time.Duration
	keep    bool
	name    string // el golden final
}

// buildFlagsConValor son las banderas de `golden build` que llevan valor: lo
// necesita parseSteps para encontrar el nombre (el posicional) sin
// confundirlo con un valor.
var buildFlagsConValor = map[string]bool{
	"migrations": true, "seed": true, "seed-mb": true, "role": true, "database": true,
	"image": true, "from": true, "mem": true, "cpus": true, "state": true, "engine": true,
	"extension": true, "preload": true, "conf": true, "init": true, "env-file": true,
}

// stepTimeoutDefault es el plazo de cada -step.
const stepTimeoutDefault = 30 * time.Minute

// parseSteps saca de los argumentos de `golden build` los de los pasos y
// devuelve el resto para db-golden.sh. Sin -step ni -sql, ok es false y rest
// son los mismos argumentos.
func parseSteps(args []string) (rest []string, o stepOpts, ok bool, err error) {
	o.timeout = stepTimeoutDefault
	if len(args) == 0 || args[0] != "build" {
		return args, o, false, nil
	}
	rest = []string{args[0]}
	val := func(i int, f string) (string, error) {
		if i+1 >= len(args) {
			return "", usageErr("-%s needs a value", f)
		}
		return args[i+1], nil
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		f, inline, hasInline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") {
			if o.name != "" {
				return nil, o, false, usageErr("extra argument %q", a)
			}
			o.name = a
			rest = append(rest, a)
			continue
		}
		get := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			v, err := val(i, f)
			i++
			return v, err
		}
		switch f {
		case "step", "super-step", "sql", "agent", "workdir", "step-timeout":
			v, err := get()
			if err != nil {
				return nil, o, false, err
			}
			switch f {
			case "step":
				if strings.TrimSpace(v) == "" {
					return nil, o, false, usageErr("-step needs a command")
				}
				o.steps = append(o.steps, goldenStep{cmd: v})
			case "super-step":
				if strings.TrimSpace(v) == "" {
					return nil, o, false, usageErr("-super-step needs a command")
				}
				o.steps = append(o.steps, goldenStep{cmd: v, super: true})
			case "sql":
				o.steps = append(o.steps, goldenStep{sql: v})
			case "agent":
				o.agent = v
			case "workdir":
				o.workdir = v
			case "step-timeout":
				d, err := time.ParseDuration(v)
				if err != nil || d <= 0 {
					return nil, o, false, usageErr("invalid -step-timeout %q", v)
				}
				o.timeout = d
			}
		case "env-file":
			// Lo usan los pasos y también -init (los .sh): va a los dos.
			v, err := get()
			if err != nil {
				return nil, o, false, err
			}
			o.envFile = v
			rest = append(rest, "-env-file", v)
		default:
			if f == "keep" {
				o.keep = true
			}
			rest = append(rest, a)
			if buildFlagsConValor[f] && !hasInline {
				if i+1 >= len(args) {
					return nil, o, false, usageErr("%s needs a value", a)
				}
				rest = append(rest, args[i+1])
				i++
			}
		}
	}
	if len(o.steps) == 0 {
		if o.agent != "" || o.workdir != "" {
			return nil, o, false, usageErr("-agent and -workdir go with -step")
		}
		return rest, o, false, nil
	}
	hayCmd := false
	for _, s := range o.steps {
		if s.cmd != "" && !s.super {
			hayCmd = true
		}
		if s.sql != "" {
			if st, err := os.Stat(s.sql); err != nil || !st.Mode().IsRegular() {
				return nil, o, false, fmt.Errorf("-sql %s: not a file", s.sql)
			}
		}
	}
	if hayCmd && o.agent == "" {
		return nil, o, false, usageErr("-step needs -agent <template>: the microVM where the command runs (e.g. a template with python and alembic)")
	}
	if o.agent != "" && !namePattern.MatchString(o.agent) {
		return nil, o, false, fmt.Errorf("invalid -agent %q", o.agent)
	}
	if o.workdir != "" {
		if st, err := os.Stat(o.workdir); err != nil || !st.IsDir() {
			return nil, o, false, fmt.Errorf("-workdir %s: not a directory", o.workdir)
		}
	}
	if o.envFile != "" {
		if err := checkEnvFile(o.envFile); err != nil {
			return nil, o, false, err
		}
	}
	if o.name == "" || !stepGoldenName.MatchString(o.name) {
		return nil, o, false, usageErr("golden build needs a name ([a-z0-9][a-z0-9_-]{0,34} with -step)")
	}
	// El golden base se construye con el nombre de la base.
	for i := len(rest) - 1; i > 0; i-- {
		if rest[i] == o.name {
			rest[i] = o.name + "-base"
			break
		}
	}
	return rest, o, true, nil
}

// stepGoldenName: el de db-golden.sh, con sitio para "-base" y "-agent".
var stepGoldenName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,34}$`)

// envLine es una línea KEY=VALUE de -env-file.
var envLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// checkEnvFile exige un fichero normal, 0600 como mucho (puede llevar claves)
// y solo con líneas KEY=VALUE, comentarios o vacías.
func checkEnvFile(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("-env-file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("-env-file %s is not a regular file", p)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("-env-file %s is readable by others (mode %04o): it may hold secrets; chmod 600 it", p, st.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	for n, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !envLine.MatchString(t) {
			// Sin citar la línea: puede llevar una clave.
			return fmt.Errorf("-env-file %s:%d: not KEY=VALUE", p, n+1)
		}
	}
	return nil
}

// buildWithSteps es `golden build` con pasos (ver arriba).
func (a *app) buildWithSteps(ctx context.Context, o stepOpts, runScript func([]string) error, scriptArgs []string) (errOut error) {
	base := o.name + "-base"
	t0 := time.Now()
	say := func(format string, args ...any) {
		fmt.Fprintf(a.stdout, "golden: [%.0fs] %s\n", time.Since(t0).Seconds(), fmt.Sprintf(format, args...))
	}
	say("base golden %s", base)
	if err := runScript(scriptArgs); err != nil {
		return err
	}
	var cp, agent *api.Machine
	defer func() {
		if o.keep && errOut == nil {
			return
		}
		cctx := context.WithoutCancel(ctx)
		if agent != nil {
			_, _ = a.k.Run(cctx, nil, "rm", "-f", agent.ID)
		}
		if cp != nil && (errOut == nil || !o.keep) {
			_ = a.remove(cctx, cp)
		}
		if errOut == nil || !o.keep {
			_, _ = a.k.Run(cctx, nil, "template", "rm", "-f", base)
			removeGoldenState(base)
		}
	}()

	var err error
	cp, err = a.up(ctx, base, o.name+"-mig", 0, defaultOwner)
	if err != nil {
		return fmt.Errorf("a copy of %s: %w", base, err)
	}
	say("copy %s ready", cp.Name)
	role, db, err := roleDB(cp.Labels)
	if err != nil {
		return err
	}

	hayCmd, haySuper := false, false
	for _, s := range o.steps {
		hayCmd = hayCmd || (s.cmd != "" && !s.super)
		haySuper = haySuper || s.super
	}
	if haySuper {
		if err := a.prepareSuperSteps(ctx, cp, o); err != nil {
			return err
		}
	}
	if hayCmd {
		if agent, err = a.startAgent(ctx, o, cp, say); err != nil {
			return err
		}
	}
	host := defaultAttachHost(cp)
	for i, s := range o.steps {
		st := time.Now()
		if s.sql != "" {
			if err := a.runStepSQL(ctx, cp, role, db, s.sql); err != nil {
				return err
			}
			say("step %d (sql %s): ok in %.1fs", i+1, filepath.Base(s.sql), time.Since(st).Seconds())
			continue
		}
		say("step %d: %s", i+1, s.cmd)
		if s.super {
			if err := a.runSuperStep(ctx, cp, o, s.cmd, db); err != nil {
				return fmt.Errorf("step %d (%s): %w; nothing was saved", i+1, s.cmd, err)
			}
			say("step %d: ok in %.1fs", i+1, time.Since(st).Seconds())
			continue
		}
		if err := a.runStepCmd(ctx, agent, o, s.cmd, host, role, db); err != nil {
			return fmt.Errorf("step %d (%s): %w; nothing was saved", i+1, s.cmd, err)
		}
		say("step %d: ok in %.1fs", i+1, time.Since(st).Seconds())
	}
	if agent != nil {
		if err := a.detach(ctx, agent.ID, cp.ID, defaultOwner, defaultAttachEnv); err != nil {
			return err
		}
		_, _ = a.k.Run(ctx, nil, "rm", "-f", agent.ID)
		agent = nil
	}

	say("VACUUM, CHECKPOINT and checks before freezing")
	if err := a.finishCopy(ctx, cp, db); err != nil {
		return err
	}
	pw, err := dbstate.ReadPassword(cp.ID)
	if err != nil {
		return err
	}
	// Una plantilla no es una copia: fuera sus etiquetas de copia (su golden,
	// su estado, su dueño). Las de rol y base se quedan: dicen cómo entrar.
	if err := a.k.SetLabels(ctx, cp.ID, map[string]string{labelGolden: "", labelState: "", labelOwner: ""}); err != nil {
		return err
	}
	say("saving %s as template %s", cp.Name, o.name)
	if _, err := a.k.Run(ctx, nil, "save", "-replace", "-warm=false", cp.ID, o.name); err != nil {
		return err
	}
	if err := writeGoldenState(o.name, base, role, db, pw); err != nil {
		return err
	}
	say("done: template %s (password and conn.env in the kling db state, like any golden)", o.name)
	return nil
}

// startAgent arranca la microVM del agente, le sube -workdir y -env-file y le
// da la copia por attach.
func (a *app) startAgent(ctx context.Context, o stepOpts, cp *api.Machine, say func(string, ...any)) (*api.Machine, error) {
	name := o.name + "-agent"
	_, _ = a.k.Run(ctx, nil, "rm", "-f", name)
	// allowlist: el proxy de credenciales lo exige; sin -allow no sale a
	// ningún sitio más que a la copia.
	if _, err := a.k.Run(ctx, nil, "run", "-from", o.agent, "-name", name, "-egress", "allowlist", "-allow-exec"); err != nil {
		return nil, fmt.Errorf("starting the agent from %s: %w", o.agent, err)
	}
	ag, err := a.inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := a.waitExec(ctx, ag.ID); err != nil {
		return ag, err
	}
	if _, err := a.k.Run(ctx, nil, "exec", ag.ID, "--", "mkdir", "-p", "/work"); err != nil {
		return ag, err
	}
	if o.workdir != "" {
		tgz, n, err := tarDir(o.workdir)
		if err != nil {
			return ag, err
		}
		defer os.Remove(tgz)
		say("uploading %s to the agent (%d files)", o.workdir, n)
		if _, err := a.k.Run(ctx, nil, "cp", tgz, ag.ID+":/work/.kdb-workdir.tgz"); err != nil {
			return ag, err
		}
		if _, err := a.k.Run(ctx, nil, "exec", ag.ID, "--", "sh", "-c", "cd /work && tar xzf .kdb-workdir.tgz && rm .kdb-workdir.tgz"); err != nil {
			return ag, err
		}
	}
	if o.envFile != "" {
		if _, err := a.k.Run(ctx, nil, "cp", o.envFile, ag.ID+":/run/kdb-step.env"); err != nil {
			return ag, err
		}
	}
	if err := a.attach(ctx, ag.ID, cp.ID, attachOpts{owner: defaultOwner, env: defaultAttachEnv}); err != nil {
		return ag, err
	}
	return ag, nil
}

// prepareSuperSteps sube -workdir y -env-file a la copia, para los
// -super-step: en /tmp/kdb-work y /tmp/kdb-step.env, del usuario postgres, y
// finishCopy los borra antes de congelar (todo /tmp/kdb-*).
func (a *app) prepareSuperSteps(ctx context.Context, cp *api.Machine, o stepOpts) error {
	if _, err := a.k.Run(ctx, nil, "exec", cp.ID, "--", "sh", "-c", "rm -rf /tmp/kdb-work && install -d -o postgres -g postgres -m 700 /tmp/kdb-work"); err != nil {
		return err
	}
	if o.workdir != "" {
		tgz, _, err := tarDir(o.workdir)
		if err != nil {
			return err
		}
		defer os.Remove(tgz)
		if _, err := a.k.Run(ctx, nil, "cp", tgz, cp.ID+":/tmp/kdb-work.tgz"); err != nil {
			return err
		}
		if _, err := a.k.Run(ctx, nil, "exec", cp.ID, "--", "sh", "-c",
			"cd /tmp/kdb-work && tar xzf /tmp/kdb-work.tgz && rm /tmp/kdb-work.tgz && chown -R postgres:postgres /tmp/kdb-work"); err != nil {
			return err
		}
	}
	if o.envFile != "" {
		if _, err := a.k.Run(ctx, nil, "cp", o.envFile, cp.ID+":/tmp/kdb-step.env"); err != nil {
			return err
		}
		if _, err := a.k.Run(ctx, nil, "exec", cp.ID, "--", "sh", "-c", "chown postgres:postgres /tmp/kdb-step.env && chmod 600 /tmp/kdb-step.env"); err != nil {
			return err
		}
	}
	return nil
}

// superStepScript corre un -super-step dentro de la copia, como el usuario
// del sistema postgres: psql entra como el superusuario por el socket local
// (pg_hba: local all postgres peer), como los scripts de init de Docker. Lo
// que el rol de la aplicación no puede hacer (ALTER ROLE ... NOSUPERUSER,
// por ejemplo), sin hacerlo superusuario a él.
const superStepScript = `set -e
cd /tmp/kdb-work
if [ -f /tmp/kdb-step.env ]; then
  while IFS= read -r l || [ -n "$l" ]; do
    case "$l" in ''|'#'*) continue ;; esac
    export "${l%%=*}=${l#*=}"
  done < /tmp/kdb-step.env
fi
export PGHOST=/run/postgresql PGUSER=postgres PGDATABASE="$KDB_DB"
exec sh -c "$1"`

func (a *app) runSuperStep(ctx context.Context, cp *api.Machine, o stepOpts, cmd, db string) error {
	args := []string{"exec", "-timeout", strconv.Itoa(int(o.timeout.Seconds())) + "s", "-e", "KDB_DB=" + db,
		cp.ID, "--", "su", "-s", "/bin/sh", "postgres", "-c", `sh -c "$0" kdb-super "$1"`, superStepScript, cmd}
	return a.runKling(ctx, args)
}

// waitExec espera a que el agente del invitado conteste.
func (a *app) waitExec(ctx context.Context, id string) error {
	for i := 0; ; i++ {
		if _, err := a.k.Run(ctx, nil, "exec", "-timeout", "5s", id, "--", "true"); err == nil {
			return nil
		} else if i >= 60 || ctx.Err() != nil {
			return fmt.Errorf("the guest agent of %s does not answer: %w", shortID(id), err)
		}
		a.sleep(time.Second)
	}
}

// stepScript es lo que corre cada -step: el entorno de la conexión (PGPASSWORD
// ya lo pone el daemon en el exec: es el marcador del attach) y el comando,
// que va como $1 (sin entrecomillar nada dentro de otra cadena). -env-file se
// lee línea a línea y cada valor se toma literal, como `docker --env-file`:
// sin source, nada del fichero se ejecuta.
const stepScript = `set -e
cd /work
if [ -f /run/kdb-step.env ]; then
  while IFS= read -r l || [ -n "$l" ]; do
    case "$l" in ''|'#'*) continue ;; esac
    export "${l%%=*}=${l#*=}"
  done < /run/kdb-step.env
fi
export PGSSLMODE=disable
export DATABASE_URL="postgresql://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/$PGDATABASE?sslmode=disable"
exec sh -c "$1"`

func (a *app) runStepCmd(ctx context.Context, agent *api.Machine, o stepOpts, cmd, host, role, db string) error {
	args := []string{"exec", "-timeout", strconv.Itoa(int(o.timeout.Seconds())) + "s",
		"-e", "PGHOST=" + host, "-e", "PGPORT=" + strconv.Itoa(pgPort),
		"-e", "PGUSER=" + role, "-e", "PGDATABASE=" + db,
		agent.ID, "--", "sh", "-c", stepScript, "kdb-step", cmd}
	// Con la terminal: la salida del programa (alembic...) se ve al momento.
	return a.runKling(ctx, args)
}

// runStepSQL corre un fichero SQL en la copia como el rol de la aplicación,
// con el error de psql apuntando al fichero de verdad.
func (a *app) runStepSQL(ctx context.Context, cp *api.Machine, role, db, file string) error {
	dest := "/tmp/kdb-step.sql"
	if _, err := a.k.Run(ctx, nil, "cp", file, cp.ID+":"+dest); err != nil {
		return err
	}
	defer func() { _, _ = a.k.Run(context.WithoutCancel(ctx), nil, "exec", cp.ID, "--", "rm", "-f", dest) }()
	if _, err := a.k.Run(ctx, nil, "exec", cp.ID, "--", "chmod", "644", dest); err != nil {
		return err
	}
	out, err := a.k.Run(ctx, nil, "exec", "-timeout", "30m", cp.ID, "--", "su", "-s", "/bin/sh", "postgres", "-c",
		fmt.Sprintf("PGOPTIONS='-c role=%s' psql -X -q -v ON_ERROR_STOP=1 -d %s -f %s 2>&1", role, db, dest))
	if err != nil {
		msg := strings.ReplaceAll(string(out), "psql:"+dest+":", file+":")
		fmt.Fprint(a.stderr, strings.ReplaceAll(msg, dest, file))
		return fmt.Errorf("-sql %s failed (file and line above); nothing was saved", file)
	}
	return nil
}

// finishCopy deja la copia como la deja db-golden.sh antes de congelar.
func (a *app) finishCopy(ctx context.Context, cp *api.Machine, db string) error {
	psql := func(d, sql string) (string, error) {
		out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", "30m", cp.ID, "--",
			"su", "-s", "/bin/sh", "postgres", "-c", "psql -X -q -At -v ON_ERROR_STOP=1 -d "+d)
		return strings.TrimSpace(string(out)), err
	}
	for _, d := range []string{db, "postgres", "template1"} {
		if _, err := psql(d, "VACUUM (ANALYZE);"); err != nil {
			return fmt.Errorf("VACUUM in %s: %w", d, err)
		}
	}
	// detach cortó las sesiones del agente; las que queden tardan un momento.
	q := "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid();"
	for i := 0; ; i++ {
		n, err := psql("postgres", q)
		if err != nil {
			return err
		}
		if n == "0" {
			break
		}
		if i >= 20 {
			return fmt.Errorf("%s client connections still open in the copy; not freezing", n)
		}
		a.sleep(500 * time.Millisecond)
	}
	if _, err := psql("postgres", "CHECKPOINT;"); err != nil {
		return err
	}
	_, err := a.k.Run(ctx, nil, "exec", cp.ID, "--", "sh", "-c", "rm -f /tmp/kdb-*; : > /var/log/postgresql/pg.log; sync")
	return err
}

// writeGoldenState escribe la contraseña y el conn.env del golden name (como
// los deja db-golden.sh), con la IP del invitado del de base.
func writeGoldenState(name, base, role, db, pw string) error {
	root, err := dbstate.Dir()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	ip := readEnvFile(filepath.Join(root, base, "conn.env"))["GUEST_IP"]
	if err := dbstate.WritePrivate(filepath.Join(dir, "password"), []byte(pw+"\n")); err != nil {
		return err
	}
	env := fmt.Sprintf("PGUSER=%s\nPGDATABASE=%s\nGUEST_IP=%s\nTEMPLATE=%s\n", role, db, ip, name)
	return dbstate.WritePrivate(filepath.Join(dir, "conn.env"), []byte(env))
}

// removeGoldenState borra el estado de un golden intermedio.
func removeGoldenState(name string) {
	if root, err := dbstate.Dir(); err == nil && stepGoldenName.MatchString(strings.TrimSuffix(name, "-base")) {
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}

// tarMax es el tope de lo que se sube al agente: lo que admite kling cp.
const tarMax = api.FileMaxUpload

// tarDir empaqueta dir (sin .git, ni enlaces que salgan de él, ni ficheros
// especiales) en un .tgz temporal y devuelve su ruta y cuántos ficheros lleva.
func tarDir(dir string) (string, int, error) {
	f, err := os.CreateTemp("", "kdb-workdir-*.tgz")
	if err != nil {
		return "", 0, err
	}
	name := f.Name()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	n := 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "__pycache__" || d.Name() == ".venv") {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil // enlaces, sockets...: fuera
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			n++
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			return err
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if st, serr := os.Stat(name); serr == nil && st.Size() > tarMax {
			err = fmt.Errorf("-workdir %s is %d MiB compressed; the limit is %d MiB (kling cp): point -workdir at the part the steps need", dir, st.Size()>>20, tarMax>>20)
		}
	}
	if err != nil {
		os.Remove(name)
		return "", 0, err
	}
	return name, n, nil
}
