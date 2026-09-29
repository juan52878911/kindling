package main

// kling db clone: un golden hecho de una base de producción, con los datos
// personales enmascarados. Lo más delicado de kling db: un enmascarado mal
// hecho filtra datos. Por eso el diseño va de fuera adentro así:
//
//  1. La contraseña de producción NO entra en ninguna microVM. Va al proxy de
//     credenciales de Postgres del host (pkg/credproxy) de una máquina de
//     construcción con -egress allowlist: el invitado recibe un marcador y el
//     proxy entra en producción con la clave real, por TLS verify-full por
//     defecto. El rol tiene que ser de solo lectura: se comprueba antes de
//     volcar y, si puede escribir, no se sigue.
//
//  2. El volcado sin enmascarar NUNCA toca el disco del host. pg_dump corre
//     DENTRO de la máquina de construcción y su salida va por una tubería a un
//     Postgres de preparación cuyo directorio de datos está en un tmpfs del
//     invitado (RAM de la microVM, sin swap): ni el overlay de la máquina (que
//     es un fichero del host) ni un fichero temporal del host ven un byte sin
//     enmascarar.
//
//  3. El enmascarado ocurre ahí mismo, en una transacción: UPDATE deterministas
//     con un hash con sal secreta (misma entrada, misma salida dentro de una
//     construcción: se conservan relaciones y joins), y una comprobación de que
//     ninguna columna enmascarada conserva un valor viejo. Si una regla falla,
//     la transacción se deshace, la máquina se destruye y no queda golden.
//
//  4. El golden NO es la máquina de construcción. Un UPDATE deja las versiones
//     viejas de las filas en las páginas y en el WAL, y congelar esa máquina
//     guardaría su RAM (con el tmpfs) en el disco del host. Así que se vuelca
//     el Postgres de preparación YA enmascarado, la máquina de construcción se
//     destruye, y el golden se construye con db-golden.sh desde ese volcado en
//     una máquina nueva con -egress none, que no ha visto nunca un dato sin
//     enmascarar ni una ruta a producción.
//
// Lo que sí toca el disco del host: el volcado enmascarado (0600, en un
// directorio 0700, borrado al terminar) mientras db-golden.sh lo carga. Es lo
// mismo que acabará en el disco del golden.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbmask"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// cloneDomain es el nombre por el que la máquina de construcción llega a
	// producción: su resolver lo desvía al proxy, que marca al upstream fijado.
	cloneDomain = "source.clone.internal"
	// cloneDir es el tmpfs del invitado donde vive todo lo que no está
	// enmascarado.
	cloneDir  = "/run/klingclone"
	clonePort = "5433"
	// labelClone marca la máquina de construcción (por si hubiera que buscarla).
	labelClone = "kling.db.clone"
	// maxPasswordBytes acota lo que se lee de stdin como contraseña.
	maxPasswordBytes = 4096
	// maxCAPEM acota el -ca.
	maxCAPEM = 1 << 20
	// pgDumpMaxServer es la versión más nueva que puede volcar el pg_dump 16
	// de la imagen pg16.
	pgDumpMaxServer = 169999
)

// goldenNamePattern es lo que acepta db-golden.sh como nombre.
var goldenNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

// cloneSource es la base de producción, sin contraseña.
type cloneSource struct {
	host, port, user, db string
	tls                  string // "" (verify-full) o "disable"
}

func (s *cloneSource) String() string {
	return s.user + "@" + net.JoinHostPort(s.host, s.port) + "/" + s.db
}

// parseCloneURL lee postgres://usuario@host:puerto/base[?sslmode=...]. Una URL
// con contraseña se rechaza: acabaría en argv (ps) y en el historial.
func parseCloneURL(raw string) (*cloneSource, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// El error de url.Parse repite la URL entera: no se enseña.
		return nil, errors.New("the URL does not parse")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("the URL must start with postgres:// or postgresql://")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("the URL needs a user (postgres://readonly_role@host:port/db)")
	}
	if _, has := u.User.Password(); has {
		return nil, errors.New("remove the password from the URL: pass it in PGPASSWORD or with -password-stdin")
	}
	s := &cloneSource{host: u.Hostname(), port: u.Port(), user: u.User.Username(), db: strings.TrimPrefix(u.Path, "/")}
	if s.host == "" {
		return nil, errors.New("the URL needs a host")
	}
	if s.port == "" {
		s.port = "5432"
	}
	if n, err := strconv.Atoi(s.port); err != nil || n < 1 || n > 65535 {
		return nil, errors.New("bad port in the URL")
	}
	if s.db == "" {
		s.db = s.user
	}
	for _, v := range []string{s.user, s.db, s.host} {
		if len(v) > 255 || strings.ContainsAny(v, "\x00\n\r") {
			return nil, errors.New("user, host or database with control characters")
		}
	}
	for k, v := range u.Query() {
		if k != "sslmode" {
			return nil, fmt.Errorf("unsupported URL parameter %q (only sslmode)", k)
		}
		switch v[len(v)-1] {
		case "", "verify-full":
		case "disable":
			s.tls = "disable"
		default:
			return nil, fmt.Errorf("sslmode=%s: the proxy verifies the certificate fully (verify-full, the default) or not at all (disable)", dbmaskSafe(v[len(v)-1]))
		}
	}
	return s, nil
}

func dbmaskSafe(s string) string {
	if len(s) > 20 {
		s = s[:20]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// cloneOpts son los flags de clone.
type cloneOpts struct {
	source        *cloneSource
	rules         *dbmask.Rules
	golden        string
	password      string
	caPEM         string
	tlsServerName string
	mask          dbmask.Options
	allowWriter   bool
	replace       bool
	asSuper       bool
	image, from   string
	mem           string
	jsonOut       bool
}

func cmdClone(args []string) error {
	fs := flag.NewFlagSet("db clone", flag.ContinueOnError)
	host := fs.String("H", "", "daemon endpoint (socket or ssh://user@host); default: KLING_HOST or the active context")
	mask := fs.String("mask", "", "rules file (JSON or simple YAML): table.column -> email|name|phone|card|text|null|keep|fixed:<value>")
	golden := fs.String("golden", "", "name of the golden to build (default: <database>-masked)")
	allowUnmasked := fs.Bool("allow-unmasked", false, "build even if suspicious columns have no rule (they are copied as they are)")
	strict := fs.Bool("strict", false, "also treat every text, JSON, XML, bytea and array column as suspicious")
	allowWriter := fs.Bool("allow-writer", false, "accept a role that can write in the source (it is only read, but a read-only role is the safe choice)")
	pwStdin := fs.Bool("password-stdin", false, "read the password of the source from stdin (default: $PGPASSWORD)")
	ca := fs.String("ca", "", "CA certificate (PEM) of the source, added to the system roots")
	serverName := fs.String("tls-server-name", "", "name the source certificate is verified against (default: the URL host)")
	replace := fs.Bool("replace", false, "replace the golden if it already exists")
	asSuper := fs.Bool("as-super", false, "load the masked dump as superuser (CREATE EXTENSION of untrusted extensions); see docs/db.md")
	image := fs.String("image", "", "image with Postgres 16 for the builder and the golden (default pg16)")
	from := fs.String("from", "", "template with Postgres 16 installed, instead of -image (macOS)")
	mem := fs.String("mem", "2G", "memory of the builder: the unmasked copy lives in its RAM")
	script := fs.String("script", "", "path of scripts/db-golden.sh (default: $"+goldenScriptEnv+", or installed next to kling-db)")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *mask == "" {
		return usageErr("usage: kling db clone <postgres-url> -mask RULES [-golden NAME] [-allow-unmasked] [-strict] [-password-stdin] [-ca FILE]")
	}
	if *image != "" && *from != "" {
		return usageErr("-image and -from exclude each other")
	}
	src, err := parseCloneURL(pos[0])
	if err != nil {
		return usageErr("%v", err)
	}
	o := cloneOpts{source: src, golden: *golden, tlsServerName: *serverName,
		mask:        dbmask.Options{AllowUnmasked: *allowUnmasked, Strict: *strict},
		allowWriter: *allowWriter, replace: *replace, asSuper: *asSuper,
		image: *image, from: *from, mem: *mem, jsonOut: *jsonOut}
	data, err := readLimited(*mask, 1<<20+1)
	if err != nil {
		return err
	}
	if o.rules, err = dbmask.ParseRules(data); err != nil {
		return err
	}
	if *ca != "" {
		b, err := readLimited(*ca, maxCAPEM)
		if err != nil {
			return err
		}
		o.caPEM = string(b)
	}
	if *pwStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, maxPasswordBytes+1))
		if err != nil {
			return err
		}
		o.password = strings.TrimRight(string(b), "\r\n")
	} else {
		o.password = os.Getenv("PGPASSWORD")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	c := &cloner{a: a, buildGolden: func(ctx context.Context, args []string) error {
		// La salida del script va a stderr: stdout es del informe.
		return runGoldenScript(ctx, *script, *host, args, nil, os.Stderr, os.Stderr)
	}}
	ctx, stop := signalCtx()
	defer stop()
	rep, err := c.run(ctx, o)
	if err != nil {
		return err
	}
	if o.jsonOut {
		return rep.WriteJSON(a.stdout)
	}
	return rep.WriteText(a.stdout)
}

func readLimited(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", p, max)
	}
	return b, nil
}

// cloner hace una construcción. buildGolden ejecuta db-golden.sh (sustituible
// en los tests).
type cloner struct {
	a           *app
	buildGolden func(ctx context.Context, args []string) error
	// tmpDir es dónde va el directorio temporal del volcado enmascarado ("" =
	// el del sistema).
	tmpDir string
}

func (c *cloner) logf(format string, a ...any) {
	fmt.Fprintf(c.a.stderr, "clone: "+format+"\n", a...)
}

// validPassword: el proxy solo admite ASCII imprimible.
func validPassword(pw string) error {
	if pw == "" {
		return errors.New("no password for the source: set PGPASSWORD or use -password-stdin")
	}
	if len(pw) > maxPasswordBytes {
		return errors.New("the password is too long")
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x20 || pw[i] > 0x7e {
			return errors.New("the password must be printable ASCII (a limit of the credential proxy)")
		}
	}
	return nil
}

func defaultGoldenName(db string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(db) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	n := strings.Trim(b.String(), "-_")
	if len(n) > 34 {
		n = strings.Trim(n[:34], "-_")
	}
	if n == "" {
		return "clone-masked"
	}
	return n + "-masked"
}

// run es la construcción entera. Devuelve el informe; si algo falla, no queda
// ni máquina de construcción, ni volcado, ni golden nuevo.
func (c *cloner) run(ctx context.Context, o cloneOpts) (*dbmask.Report, error) {
	if o.golden == "" {
		o.golden = defaultGoldenName(o.source.db)
	}
	if !goldenNamePattern.MatchString(o.golden) {
		return nil, fmt.Errorf("invalid golden name %q: [a-z0-9][a-z0-9_-]{0,40}", o.golden)
	}
	if err := validPassword(o.password); err != nil {
		return nil, err
	}
	if o.source.tls == "disable" && (o.caPEM != "" || o.tlsServerName != "") {
		return nil, errors.New("sslmode=disable excludes -ca and -tls-server-name")
	}
	if !regexp.MustCompile(`^[0-9]+[MG]?$`).MatchString(o.mem) {
		return nil, fmt.Errorf("invalid -mem %q", o.mem)
	}
	if o.image != "" && !namePattern.MatchString(o.image) || o.from != "" && !namePattern.MatchString(o.from) {
		return nil, errors.New("invalid -image or -from")
	}
	if !o.replace {
		if _, err := c.a.k.Run(ctx, nil, "template", "inspect", o.golden, "-json"); err == nil {
			return nil, fmt.Errorf("a template %s already exists (-replace replaces it once the new one is built)", o.golden)
		}
	}

	builder := o.golden + "-clone-" + randomSuffix()
	c.logf("starting the builder %s (egress only to the credential proxy)", builder)
	runArgs := []string{"run", "-name", builder, "-egress", "allowlist", "-allow-exec",
		// Si este proceso muere, la máquina se BORRA al vencer (no se congela:
		// congelarla guardaría su RAM, con los datos sin enmascarar, en disco).
		"-ttl", "3h", "-on-ttl", "remove",
		"-label", labelClone + "=builder"}
	if o.from != "" {
		runArgs = append(runArgs, "-from", o.from)
	} else {
		runArgs = append(runArgs, "-image", orDefault(o.image, "pg16"), "-mem", o.mem, "-cpus", "2", "-cpu-pct", "100")
	}
	if _, err := c.a.k.Run(ctx, nil, runArgs...); err != nil {
		return nil, fmt.Errorf("starting the builder: %w", err)
	}
	// Pase lo que pase, la máquina de construcción se destruye (una vez).
	destroyed := false
	destroyBuilder := func() {
		if !destroyed {
			destroyed = true
			c.a.destroy(builder)
		}
	}
	defer destroyBuilder()

	if err := c.waitExec(ctx, builder); err != nil {
		return nil, err
	}
	spec := api.CredentialSpec{
		Type: "postgres", Domain: cloneDomain, Env: "PGPASSWORD", Secret: o.password,
		User: o.source.user, Database: o.source.db,
		// El upstream lo fija el operador: vale para una base de la LAN o del
		// propio host, y el invitado no lo elige.
		Upstream: net.JoinHostPort(o.source.host, o.source.port),
	}
	if o.source.tls == "disable" {
		spec.UpstreamTLS = "disable"
	} else {
		spec.CAPEM = o.caPEM
		spec.TLSServerName = orDefault(o.tlsServerName, o.source.host)
	}
	if err := c.a.k.SetCredential(ctx, builder, spec); err != nil {
		return nil, fmt.Errorf("handing the source credential to the proxy: %w", err)
	}
	o.password = "" // desde aquí, solo la tiene el proxy

	c.logf("preparing a Postgres in the builder's RAM (tmpfs)")
	if _, err := c.a.k.Run(ctx, strings.NewReader(cloneSetupScript), "exec", "-i", "-timeout", "5m", builder, "--", "sh", "-s"); err != nil {
		return nil, fmt.Errorf("preparing the builder: %w", err)
	}

	c.logf("checking the role %s on the source", o.source.user)
	out, err := c.a.k.Run(ctx, strings.NewReader(cloneProbeScript(o.source)), "exec", "-i", "-timeout", "2m", builder, "--", "sh", "-s")
	if err != nil {
		return nil, fmt.Errorf("connecting to the source through the proxy: %w", err)
	}
	if err := checkProbe(out, o.allowWriter); err != nil {
		return nil, err
	}

	c.logf("dumping %s into the builder (pg_dump inside the microVM; nothing on the host's disk)", o.source)
	if _, err := c.a.k.Run(ctx, strings.NewReader(cloneDumpScript(o.source)), "exec", "-i", "-timeout", "1h", builder, "--", "sh", "-s"); err != nil {
		return nil, fmt.Errorf("dumping the source: %w", err)
	}

	out, err = c.stagingPsql(ctx, builder, dbmask.CatalogSQL, "10m")
	if err != nil {
		return nil, fmt.Errorf("reading the catalog: %w", err)
	}
	cat, err := dbmask.ParseCatalog(out)
	if err != nil {
		return nil, err
	}
	plan, err := dbmask.NewPlan(cat, o.rules, o.mask)
	if err != nil {
		return nil, fmt.Errorf("nothing was built: %w", err)
	}

	salt, err := dbmask.NewSalt()
	if err != nil {
		return nil, err
	}
	sql, err := dbmask.MaskSQL(plan, salt)
	salt = ""
	if err != nil {
		return nil, err
	}
	if len(sql) > api.ExecMaxStdin {
		return nil, fmt.Errorf("the masking SQL is over %d bytes (too many masked columns)", api.ExecMaxStdin)
	}
	c.logf("masking %d column(s) in %d table(s)", countUpdates(plan), len(plan.Tables))
	out, err = c.stagingPsql(ctx, builder, sql, "1h")
	sql = ""
	res, perr := dbmask.ParseResult(plan, out)
	if perr == nil {
		if bad := res.Unchanged(plan); len(bad) > 0 {
			return nil, fmt.Errorf("nothing was built: masked column(s) kept old values: %s", strings.Join(bad, ", "))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("nothing was built: masking failed (a rule does not fit its column, or a unique constraint collided): %w", err)
	}
	if perr != nil {
		return nil, fmt.Errorf("nothing was built: %w", perr)
	}

	// El volcado YA enmascarado sale a un fichero 0600 de un directorio 0700.
	tmp, err := os.MkdirTemp(c.tmpDir, "kling-db-clone-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return nil, err
	}
	seed := filepath.Join(tmp, "masked.sql")
	f, err := os.OpenFile(seed, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	c.logf("dumping the masked copy")
	if _, err := c.a.k.Run(ctx, nil, "exec", "-timeout", "1h", builder, "--", "su", "-s", "/bin/sh", "postgres", "-c",
		"pg_dump -h "+cloneDir+" -p "+clonePort+" -d staging --no-owner --no-privileges -f "+cloneDir+"/masked.sql"); err != nil {
		return nil, fmt.Errorf("dumping the masked copy: %w", err)
	}
	// kling cp escribe con os.Create: sobre un fichero que ya existe conserva
	// su modo (0600).
	if _, err := c.a.k.Run(ctx, nil, "cp", builder+":"+cloneDir+"/masked.sql", seed); err != nil {
		return nil, fmt.Errorf("copying the masked dump: %w", err)
	}
	// La máquina de construcción sobra ya: se destruye ANTES de construir el
	// golden.
	destroyBuilder()

	c.logf("building the golden %s from the masked dump (a new machine, egress none)", o.golden)
	gargs := []string{"build", "-seed", seed}
	if o.asSuper {
		gargs = append(gargs, "-as-super")
	}
	if o.from != "" {
		gargs = append(gargs, "-from", o.from)
	} else if o.image != "" {
		gargs = append(gargs, "-image", o.image)
	}
	gargs = append(gargs, o.golden)
	if err := c.buildGolden(ctx, gargs); err != nil {
		return nil, fmt.Errorf("building the golden %s: %w", o.golden, err)
	}
	return dbmask.NewReport(o.golden, o.source.String(), plan, res), nil
}

func countUpdates(p *dbmask.Plan) int {
	n := 0
	for _, t := range p.Tables {
		n += len(t.Updates)
	}
	return n
}

// waitExec espera a que el agente del invitado conteste.
func (c *cloner) waitExec(ctx context.Context, m string) error {
	for i := 0; ; i++ {
		if _, err := c.a.k.Run(ctx, nil, "exec", "-timeout", "5s", m, "--", "true"); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i >= 60 {
			return fmt.Errorf("the builder %s does not answer", m)
		}
		c.a.sleep(time.Second)
	}
}

// stagingPsql corre SQL (por stdin) en el Postgres de preparación, como su
// superusuario, por el socket del tmpfs. VERBOSITY=sqlstate: un error dice su
// código y no el valor que lo provocó, así que el error de kling se puede
// enseñar tal cual.
func (c *cloner) stagingPsql(ctx context.Context, m, sql, timeout string) ([]byte, error) {
	return c.a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", timeout, m, "--",
		"su", "-s", "/bin/sh", "postgres", "-c",
		"psql -X -q -At -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h "+cloneDir+" -p "+clonePort+" -d staging")
}

// checkProbe lee la línea "probe|super|tablas escribibles|versión".
func checkProbe(out []byte, allowWriter bool) error {
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 4 || f[0] != "probe" {
			continue
		}
		writable, err1 := strconv.Atoi(f[2])
		ver, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			break
		}
		if ver > pgDumpMaxServer {
			return fmt.Errorf("the source runs Postgres %d; the builder's pg_dump 16 cannot dump it", ver/10000)
		}
		if f[1] == "t" {
			return errors.New("the source role is a superuser: give kling db clone a read-only role (see docs/db.md)")
		}
		if writable > 0 && !allowWriter {
			return fmt.Errorf("the source role can write in %d table(s): give kling db clone a read-only role, or pass -allow-writer", writable)
		}
		return nil
	}
	return errors.New("unexpected answer from the source")
}

// shq cita para sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// cloneSetupScript prepara el Postgres de preparación en un tmpfs del
// invitado. Sin swap (si la hubiera, la RAM podría acabar en un disco) y sin
// TCP: solo el socket del tmpfs.
const cloneSetupScript = `# kling-db:clone-setup
set -eu
C=` + cloneDir + `
if [ "$(wc -l < /proc/swaps)" -gt 1 ]; then echo "the builder has swap: refusing (unmasked data could reach a disk)" >&2; exit 1; fi
mkdir -p "$C"
grep -q " $C tmpfs " /proc/mounts || mount -t tmpfs -o size=75%,mode=0700 klingclone "$C"
grep -q " $C tmpfs " /proc/mounts || { echo "no tmpfs at $C" >&2; exit 1; }
chown postgres:postgres "$C"
chmod 700 "$C"
su -s /bin/sh postgres -c "initdb -D $C/data -E UTF8 --locale=C.UTF-8 --auth-local=peer --auth-host=reject" >"$C/initdb.out" 2>&1 || { cat "$C/initdb.out" >&2; exit 1; }
cat >> "$C/data/postgresql.conf" <<'CONF'
listen_addresses = ''
port = ` + clonePort + `
unix_socket_directories = '` + cloneDir + `'
fsync = off
synchronous_commit = off
full_page_writes = off
wal_level = minimal
max_wal_senders = 0
shared_buffers = 64MB
max_connections = 10
dynamic_shared_memory_type = mmap
huge_pages = off
timezone = 'UTC'
log_timezone = 'UTC'
log_min_error_statement = panic
log_statement = none
logging_collector = off
CONF
su -s /bin/sh postgres -c "pg_ctl -D $C/data -l $C/pg.log -w -t 90 start" </dev/null >"$C/pgctl.out" 2>&1 || { cat "$C/pgctl.out" >&2; exit 1; }
su -s /bin/sh postgres -c "createdb -h $C -p ` + clonePort + ` staging"
`

// cloneMarkerSnippet lee de MMDS (v2, con token) el marcador que el proxy dio
// a PGPASSWORD y lo exporta. Con nc de busybox: la imagen no trae curl.
const cloneMarkerSnippet = `mmds() {
  printf '%s %s HTTP/1.0\r\nHost: 169.254.169.254\r\n%s\r\n%s\r\nContent-Length: 0\r\n\r\n' "$1" "$2" "$3" "$4" \
    | nc -w 5 169.254.169.254 80 | tr -d '\r' | sed '1,/^$/d'
}
T=$(mmds PUT /latest/api/token 'X-metadata-token-ttl-seconds: 60' 'Accept: text/plain')
[ -n "$T" ] || { echo "no MMDS token in the builder (does the image have nc?)" >&2; exit 1; }
M=$(mmds GET / "X-metadata-token: $T" 'Accept: application/json' | tr -d '\n' \
  | sed -n 's/.*"PGPASSWORD"[[:space:]]*:[[:space:]]*"\(kling-cred-[0-9a-f]*\)".*/\1/p')
[ -n "$M" ] || { echo "no credential placeholder in the builder's MMDS" >&2; exit 1; }
export PGPASSWORD="$M" PGSSLMODE=disable PGCONNECT_TIMEOUT=20 PGAPPNAME=kling-db-clone
`

// cloneProbeScript comprueba el rol en producción: si es superusuario, en
// cuántas tablas puede escribir y la versión del servidor. Solo lee.
func cloneProbeScript(s *cloneSource) string {
	return "# kling-db:clone-probe\nset -eu\n" + cloneMarkerSnippet +
		"U=" + shq(s.user) + "\nD=" + shq(s.db) + "\n" +
		`psql -X -At -v ON_ERROR_STOP=1 -h ` + cloneDomain + ` -p 5432 -U "$U" -d "$D" <<'SQL'
SELECT 'probe', r.rolsuper,
  (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname NOT LIKE 'pg\_toast%'
      AND (has_table_privilege(c.oid, 'INSERT') OR has_table_privilege(c.oid, 'UPDATE')
        OR has_table_privilege(c.oid, 'DELETE') OR has_table_privilege(c.oid, 'TRUNCATE'))),
  current_setting('server_version_num')
FROM pg_roles r WHERE r.rolname = current_user;
SQL
`
}

// cloneDumpScript vuelca producción DENTRO de la máquina y lo restaura en el
// Postgres del tmpfs, por una tubería: el volcado sin enmascarar no se
// escribe en ningún fichero. De los errores solo se enseñan los de pg_dump
// (conexión, permisos: sin datos) y los códigos SQLSTATE de la restauración.
func cloneDumpScript(s *cloneSource) string {
	return "# kling-db:clone-dump\nset -eu\nset -o pipefail\n" + cloneMarkerSnippet +
		"C=" + cloneDir + "\nU=" + shq(s.user) + "\nD=" + shq(s.db) + "\n" +
		`rc=0
pg_dump -h ` + cloneDomain + ` -p 5432 -U "$U" -d "$D" --no-owner --no-privileges --no-blobs \
    --no-publications --no-subscriptions --no-tablespaces --no-security-labels 2>"$C/dump.err" \
  | su -s /bin/sh postgres -c "psql -X -q -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h $C -p ` + clonePort + ` -d staging" >/dev/null 2>"$C/restore.err" || rc=$?
if [ "$rc" -ne 0 ]; then
  grep '^pg_dump:' "$C/dump.err" | head -n 20 >&2 || true
  grep -E 'ERROR: +[0-9A-Z]{5}$' "$C/restore.err" | head -n 5 >&2 || true
  exit 1
fi
`
}
