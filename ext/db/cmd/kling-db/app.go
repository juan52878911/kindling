package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas de una copia. Todas cumplen api.KeyPattern (sin '/'): el daemon
// las guarda con la máquina, Commit las copia a la plantilla y run -from y fork
// las heredan. Por eso state no se puede dar por bueno solo porque venga
// puesto: una copia de una copia lista nace con state=ready heredado, y lo que
// la delata es que no hay contraseña de SU id en el host (ver checkReady).
const (
	labelGolden   = "kling.db.golden"   // plantilla de la que sale
	labelOwner    = "kling.db.owner"    // quién la pidió ("local" en el CLI)
	labelState    = "kling.db.state"    // preparing | ready
	labelRole     = "kling.db.role"     // rol de la aplicación (app)
	labelDatabase = "kling.db.database" // base de la aplicación (appdb)
	labelRepo     = "kling.db.repo"     // hash del directorio git común del repo (kling db branch)
	labelBranch   = "kling.db.branch"   // clave estable de la rama (ver branchKey)
	labelUsed     = "kling.db.used"     // segundos unix de la última vez que fue la activa

	statePreparing = "preparing"
	stateReady     = "ready"

	defaultOwner    = "local"
	defaultRole     = "app"
	defaultDatabase = "appdb"
	pgPort          = 5432
)

// dbLabelKeys son las claves que escribe esta extensión, para el test de
// api.KeyPattern.
var dbLabelKeys = []string{labelGolden, labelOwner, labelState, labelRole, labelDatabase, labelRepo, labelBranch, labelUsed, labelEngine, labelClass, labelReport, api.LabelKind, api.LabelPorts}

var (
	// nombres de máquina y de plantilla (validName del núcleo).
	namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	// rol y base: identificadores simples de SQL, sin comillas que escapar.
	identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,30}$`)
)

// backend es lo que la extensión pide a kindling.
type backend interface {
	klingc.Kling
	klingc.Labeler
	klingc.Credentialer
}

// app lleva todo lo que los comandos tocan fuera de sí mismos, para poder
// sustituirlo en los tests.
type app struct {
	k      backend
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	// stdoutTTY dice si stdout es una terminal (ahí no se imprime una clave sin
	// confirmación).
	stdoutTTY func() bool
	// runPsql ejecuta el psql del host con ese entorno extra y esos argumentos;
	// runMysql, el cliente de MySQL/MariaDB del host; runRedis, el redis-cli.
	runPsql  func(ctx context.Context, env []string, args []string) error
	runMysql func(ctx context.Context, env []string, args []string) error
	runRedis func(ctx context.Context, env []string, args []string) error
	// runKling ejecuta kling con la terminal del usuario (kling shell, para
	// el sqlite3 de una copia SQLite).
	runKling func(ctx context.Context, args []string) error
	sleep    func(time.Duration)
	// readyWait es cuánto se espera a que Postgres acepte conexiones.
	readyWait time.Duration
	// cwd es el directorio del que kling db branch lee el repositorio git
	// (vacío: el del proceso). now es el reloj (sustituible en los tests).
	cwd string
	// hookForce permite instalar el hook fuera del directorio git del repo.
	hookForce bool
	now       func() time.Time
}

func newApp(host string) (*app, error) {
	cli, err := klingc.New(host)
	if err != nil {
		return nil, err
	}
	return &app{
		k: cli, stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin,
		stdoutTTY: func() bool { return isTerminal(os.Stdout) },
		runPsql:   runHostPsql,
		runMysql:  runHostMySQL,
		runRedis:  runHostRedis,
		runKling: func(ctx context.Context, args []string) error {
			return runInteractive(cli.Command(ctx, args...))
		},
		sleep:     time.Sleep,
		readyWait: 30 * time.Second,
	}, nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// ── lectura del estado ───────────────────────────────────────────────────────

// maxJSON acota lo que se decodifica de una salida de kling.
const maxJSON = 8 << 20

func (a *app) inspect(ctx context.Context, ref string) (*api.Machine, error) {
	out, err := a.k.Run(ctx, nil, "inspect", ref)
	if err != nil {
		return nil, fmt.Errorf("no machine %q: %w", ref, err)
	}
	if len(out) > maxJSON {
		return nil, errors.New("kling inspect: output too large")
	}
	var mc api.Machine
	if err := json.Unmarshal(out, &mc); err != nil {
		return nil, fmt.Errorf("kling inspect %s: %w", ref, err)
	}
	if mc.ID == "" {
		return nil, fmt.Errorf("kling inspect %s: no id in the answer", ref)
	}
	return &mc, nil
}

func (a *app) template(ctx context.Context, name string) (*api.Snapshot, error) {
	out, err := a.k.Run(ctx, nil, "template", "inspect", name, "-json")
	if err != nil {
		return nil, fmt.Errorf("no template %q: %w", name, err)
	}
	var s api.Snapshot
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("kling template inspect %s: %w", name, err)
	}
	return &s, nil
}

// owned exige que la máquina sea una copia de kling db de ese dueño.
func owned(mc *api.Machine, owner string) error {
	if mc.Labels[labelGolden] == "" {
		return fmt.Errorf("%s is not a kling db copy (no %s label)", mc.Name, labelGolden)
	}
	if got := mc.Labels[labelOwner]; got != owner {
		return fmt.Errorf("%s belongs to owner %q, not %q", mc.Name, got, owner)
	}
	return nil
}

// checkReady es la puerta de connect y fork, sobre UNA sola lectura de la
// máquina: existe (la lectura), corre, está lista, es de owner y este host
// tiene la contraseña de ESTE id. Lo último es lo que distingue una copia
// preparada aquí de una que heredó state=ready de su origen.
func checkReady(mc *api.Machine, owner string) error {
	if err := owned(mc, owner); err != nil {
		return err
	}
	if mc.State != api.StateRunning {
		return fmt.Errorf("%s is %s, not running (kling thaw %s)", mc.Name, mc.State, mc.Name)
	}
	if st := mc.Labels[labelState]; st != stateReady {
		return fmt.Errorf("%s is not ready (%s=%q): it was never finished or is being prepared", mc.Name, labelState, st)
	}
	if !hasPassword(engineOf(mc.Labels)) {
		// SQLite: no hay clave que distinguir; se entra por kling exec, que el
		// daemon ya reserva al dueño de la máquina.
		return nil
	}
	if err := dbstate.HasPassword(mc.ID); err != nil {
		if errors.Is(err, dbstate.ErrNoPassword) {
			return fmt.Errorf("%s: this host has no password for machine %s; it was not prepared here (kling db reset %s gives a fresh one)",
				mc.Name, shortID(mc.ID), mc.Name)
		}
		return err
	}
	return nil
}

// roleDB son el rol y la base de una copia, de sus etiquetas.
func roleDB(labels map[string]string) (role, db string, err error) {
	role = orDefault(labels[labelRole], defaultRole)
	db = orDefault(labels[labelDatabase], defaultDatabase)
	e := engineOf(labels)
	if !identPattern.MatchString(role) || role == "postgres" || (e == engineMySQL && myReservedUsers[role]) || (e == engineRedis && redisReservedUsers[role]) {
		return "", "", fmt.Errorf("invalid role %q", role)
	}
	if !identPattern.MatchString(db) {
		return "", "", fmt.Errorf("invalid database %q", db)
	}
	return role, db, nil
}

// goldenRoleDB lee rol y base de la plantilla: sus etiquetas y, si no las
// tiene (scripts/db-golden.sh no etiqueta), el conn.env que dejó ese script
// en este host. Si no hay nada, app/appdb, los de db-golden.sh.
func goldenRoleDB(s *api.Snapshot) (string, string, error) {
	role, db, _, err := goldenInfo(s)
	return role, db, err
}

// goldenInfo es goldenRoleDB con el motor de la plantilla: su etiqueta
// kling.db.engine (la ponen db-golden-mysql.sh, -redis.sh y -sqlite.sh) o
// ENGINE en su conn.env;
// Postgres si no dice nada.
func goldenInfo(s *api.Snapshot) (role, db, engine string, err error) {
	labels := map[string]string{}
	if d, derr := dbstate.Dir(); derr == nil && namePattern.MatchString(s.Name) {
		for k, v := range readEnvFile(d + "/" + s.Name + "/conn.env") {
			switch k {
			case "PGUSER", "DBUSER":
				labels[labelRole] = v
			case "PGDATABASE", "DBNAME":
				labels[labelDatabase] = v
			case "ENGINE":
				labels[labelEngine] = v
			}
		}
	}
	for _, k := range []string{labelRole, labelDatabase, labelEngine} {
		if v := s.Labels[k]; v != "" {
			labels[k] = v
		}
	}
	if e := labels[labelEngine]; e != "" && !knownEngine(e) {
		return "", "", "", fmt.Errorf("unknown engine %q", e)
	}
	engine = engineOf(labels)
	role, db, err = roleDB(labels)
	return role, db, engine, err
}

func readEnvFile(p string) map[string]string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			out[k] = v
		}
	}
	return out
}

// hostAddr es por dónde llega el host al 5432 (3306 en MySQL) de la copia:
// en macOS, el reenvío que abre el backend por kling.ports (loopback, con
// peercred); en Linux, la IP del netns de la máquina. En Linux no hay
// frontera: cualquier proceso del host llega a esa IP, y por eso la
// contraseña de cada copia es propia y solo está en este host.
func hostAddr(mc *api.Machine) (host string, port int, err error) {
	dbPort := enginePort(engineOf(mc.Labels))
	if len(mc.Forwards) > 0 {
		a, ok := mc.Forwards[strconv.Itoa(dbPort)]
		if !ok || a == "" {
			return "", 0, fmt.Errorf("%s has no forward for port %d (it needs the label %s=%d)", mc.Name, dbPort, api.LabelPorts, dbPort)
		}
		h, p, serr := net.SplitHostPort(a)
		n, perr := strconv.Atoi(p)
		if serr != nil || perr != nil {
			return "", 0, fmt.Errorf("%s: bad forward %q", mc.Name, a)
		}
		return h, n, nil
	}
	if mc.IP == "" {
		return "", 0, fmt.Errorf("%s has no address the host can reach", mc.Name)
	}
	return mc.IP, dbPort, nil
}

// ── preparar una copia ───────────────────────────────────────────────────────

// psqlSuper es el psql del superusuario dentro de la copia, por el socket
// local y como el usuario del sistema postgres (pg_hba: local all postgres
// peer). La orden es fija: lo variable va por stdin.
const psqlSuper = "psql -X -q -At -v ON_ERROR_STOP=1 -d postgres"

// waitPostgres espera a que el postmaster de la copia acepte conexiones.
func (a *app) waitPostgres(ctx context.Context, id string) error {
	deadline := time.Now().Add(a.readyWait)
	for {
		_, err := a.k.Run(ctx, nil, "exec", "-timeout", "10s", id, "--",
			"su", "-s", "/bin/sh", "postgres", "-c", "pg_isready -q -h /run/postgresql")
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres in %s does not accept connections after %s", shortID(id), a.readyWait)
		}
		a.sleep(time.Second)
	}
}

// rotate estrena la contraseña del rol de la aplicación en la copia id.
//
// La clave se genera aquí y NUNCA sale del host: al invitado va solo su
// verificador SCRAM, por stdin (ni argv ni pantalla). Se comprueba en la misma
// sesión que pg_authid guarda exactamente ese verificador. La clave se escribe
// en el host ANTES de tocar la base: si lo segundo falla, quien llama destruye
// la copia y el fichero; lo contrario dejaría una base con una clave perdida.
func (a *app) rotate(ctx context.Context, id, role string) error {
	pw, err := generatePassword()
	if err != nil {
		return err
	}
	ver, err := newVerifier(pw)
	if err != nil {
		return err
	}
	if err := dbstate.WritePassword(id, pw); err != nil {
		return fmt.Errorf("storing the password of %s: %w", shortID(id), err)
	}
	return a.setVerifier(ctx, id, role, ver)
}

// setVerifier pone el verificador ver al rol de la copia id y comprueba en la
// misma sesión que pg_authid lo guarda. Lo comparten la rotación al preparar y
// kling db rotate.
func (a *app) setVerifier(ctx context.Context, id, role, ver string) error {
	// role pasó identPattern y el verificador es base64 con '$' y ':': nada
	// que escapar dentro de las comillas.
	sql := fmt.Sprintf("ALTER ROLE %s PASSWORD '%s';\nSELECT rolpassword = '%s' FROM pg_authid WHERE rolname = '%s';\n",
		role, ver, ver, role)
	out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", "60s", id, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
	if err != nil {
		// Sin el detalle a propósito: el error de psql cita la sentencia.
		return fmt.Errorf("rotating the password of %s failed (output omitted: it may quote the statement)", shortID(id))
	}
	if strings.TrimSpace(string(out)) != "t" {
		return fmt.Errorf("rotating the password of %s: role %q does not hold the new verifier", shortID(id), role)
	}
	return nil
}

// prepare deja lista una copia que ya está en state=preparing: espera a
// Postgres, quita los roles de solo lectura heredados y rota la clave. No la
// marca ready: eso lo hace quien llama cuando todas las de la operación están
// preparadas. Una copia MySQL o Redis espera a su servidor y rota (kling db
// role no existe para ellos: no hay roles heredados que quitar); una SQLite
// solo comprueba que su base se abre.
func (a *app) prepare(ctx context.Context, mc *api.Machine) error {
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return err
	}
	switch engineOf(mc.Labels) {
	case engineMySQL:
		if err := a.waitMySQL(ctx, mc.ID); err != nil {
			return err
		}
		return a.rotateMySQL(ctx, mc.ID, role)
	case engineRedis:
		if err := a.waitRedis(ctx, mc.ID); err != nil {
			return err
		}
		return a.rotateRedis(ctx, mc.ID, role)
	case engineSQLite:
		// Sin clave que rotar: basta con que la base esté y se abra.
		return a.waitSQLite(ctx, mc.ID, db)
	}
	if err := a.waitPostgres(ctx, mc.ID); err != nil {
		return err
	}
	if err := a.purgeInheritedRoles(ctx, mc.ID, db); err != nil {
		return err
	}
	return a.rotate(ctx, mc.ID, role)
}

// purgeMarker va en lo que manda purgeInheritedRoles (los tests lo buscan).
const purgeMarker = "kling-db:purge-inherited"

// purgeROSQL quita de una copia recién nacida los roles de `kling db role`
// (COMMENT 'kling-db:ro'). Su verificador viaja en la RAM y el disco del origen
// (fork, punto de guardado, golden hecho de una copia): quien tuviera la clave
// de ese rol entraría en cada copia hija. Una copia nueva no hereda accesos: el
// rol se vuelve a crear con kling db role, con otra clave.
//
// kling_db_ro (el de ask) se conserva: no tiene clave ni línea de red, solo el
// mapa peer del socket para el usuario del sistema postgres, que ya es
// superusuario dentro. Por si alguien le hubiera puesto una, se le quita.
//
// Primero se cortan sus sesiones (una copia restaurada de memoria las trae),
// luego DROP OWNED BY en la base de la aplicación y en postgres (privilegios en
// objetos compartidos) y DROP ROLE. Lo último comprueba que no queda nada.
// %[1]s es la base (identPattern); %%I lo cita format().
const purgeROSQL = `-- ` + purgeMarker + `
SELECT count(pg_terminate_backend(a.pid)) FROM pg_stat_activity a JOIN pg_roles r ON r.rolname = a.usename
  WHERE shobj_description(r.oid, 'pg_authid') = '` + roleComment + `';
\connect %[1]s
SELECT format('DROP OWNED BY %%I', rolname) FROM pg_roles WHERE shobj_description(oid, 'pg_authid') = '` + roleComment + `' \gexec
\connect postgres
SELECT format('DROP OWNED BY %%I', rolname) FROM pg_roles WHERE shobj_description(oid, 'pg_authid') = '` + roleComment + `' \gexec
SELECT format('DROP ROLE %%I', rolname) FROM pg_roles WHERE shobj_description(oid, 'pg_authid') = '` + roleComment + `' \gexec
SELECT format('ALTER ROLE %%I PASSWORD NULL', rolname) FROM pg_authid WHERE rolname = '` + defaultRORole + `' AND rolpassword IS NOT NULL \gexec
SELECT count(*) FROM pg_authid WHERE shobj_description(oid, 'pg_authid') = '` + roleComment + `'
  OR (rolname = '` + defaultRORole + `' AND rolpassword IS NOT NULL);
`

// purgeHBAScript quita de pg_hba.conf las líneas de kling db role (acaban en
// "# kling-db") y recarga. Se busca el fichero con SHOW hba_file.
const purgeHBAScript = "# " + purgeMarker + "\n" + hbaPrelude + `if grep -q ' # kling-db$' "$F"; then
  T=$(mktemp)
  grep -v ' # kling-db$' "$F" > "$T" || true
  cat "$T" > "$F"
  rm -f "$T"
  q 'SELECT pg_reload_conf()' >/dev/null
fi
if grep -q ' # kling-db$' "$F"; then echo "pg_hba.conf still has kling-db lines" >&2; exit 1; fi
`

// purgeInheritedRoles quita los roles de solo lectura heredados y sus líneas
// de pg_hba.conf. Si no puede, la copia no se da por lista.
func (a *app) purgeInheritedRoles(ctx context.Context, id, db string) error {
	out, err := a.k.Run(ctx, strings.NewReader(fmt.Sprintf(purgeROSQL, db)), "exec", "-i", "-timeout", "60s", id, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
	if err != nil {
		return fmt.Errorf("removing the inherited read-only roles of %s: %w", shortID(id), err)
	}
	if n := lastLine(string(out)); n != "0" {
		return fmt.Errorf("removing the inherited read-only roles of %s: %q left", shortID(id), n)
	}
	if _, err := a.k.Run(ctx, strings.NewReader(purgeHBAScript), "exec", "-i", "-timeout", "60s", id, "--", "sh", "-s"); err != nil {
		return fmt.Errorf("removing the inherited pg_hba.conf lines of %s: %w", shortID(id), err)
	}
	return nil
}

// destroy borra una copia y su contraseña. Se usa al deshacer: los errores se
// cuentan, no se devuelven, para que el del motivo original no se pierda.
func (a *app) destroy(id string) {
	// Con un contexto propio: si el original se canceló (Ctrl-C), deshacer
	// tiene que poder ejecutarse igual.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := a.k.Run(ctx, nil, "rm", "-f", id); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not remove %s: %v (remove it with kling rm -f %s)\n", shortID(id), err, id)
	}
	if _, err := dbstate.CopyDir(id); err != nil {
		return // un nombre, no un id: no tiene contraseña guardada
	}
	if err := dbstate.Remove(id); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not remove the password of %s: %v\n", shortID(id), err)
	}
}

func (a *app) setState(ctx context.Context, id, state string) error {
	return a.k.SetLabels(ctx, id, map[string]string{labelState: state})
}

// ── utilidades ───────────────────────────────────────────────────────────────

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// randomSuffix para los nombres por defecto de las copias.
func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// mergePorts añade el puerto de la base (5432, o 3306 con mergePortsFor) a la
// lista de kling.ports sin perder la que hubiera.
func mergePorts(cur string) string { return mergePortsFor(cur, pgPort) }

func mergePortsFor(cur string, dbPort int) string {
	seen := map[int]bool{}
	var ports []int
	for _, p := range strings.Split(cur, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 && n < 65536 && !seen[n] {
			seen[n] = true
			ports = append(ports, n)
		}
	}
	if !seen[dbPort] {
		ports = append(ports, dbPort)
	}
	sort.Ints(ports)
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

func validOwner(o string) error {
	if !api.KeyPattern.MatchString(o) {
		return fmt.Errorf("invalid owner %q: lowercase letters, digits, '.', '_' and '-'", o)
	}
	return nil
}
