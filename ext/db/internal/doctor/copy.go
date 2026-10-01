package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

// Revisión de una máquina: las comprobaciones A por psql dentro del invitado
// (como el superusuario postgres, por el socket local) y, si es una copia de
// kling db (lleva kling.db.golden), las B: estado, rotación de la contraseña,
// reloj y clientes heredados.

var (
	reDB     = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	reRole   = reDB
	reGolden = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// maxSkew es el desvío de reloj tolerado entre invitado y host.
const maxSkew = 2.0

// stateDir es donde kling db guarda contraseñas y verificadores en el host.
func stateDir() (string, error) {
	if d := os.Getenv("KLING_DB_STATE"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "kling-db"), nil
}

// execQuerier lanza psql dentro de la máquina. La SQL va por stdin: nada de
// lo que viene de la base ni de la consulta pasa por una línea de shell; lo
// único que se interpola, el nombre de la base, está validado.
type execQuerier struct {
	k  klingc.Kling
	id string
}

func (e *execQuerier) query(ctx context.Context, db, name, sql string) (string, error) {
	if !reDB.MatchString(db) {
		return "", fmt.Errorf("database name %q not supported", safe(db, 64))
	}
	in := strings.NewReader("/* doctor:" + name + " */ " + sql + ";\n")
	out, err := e.k.Run(ctx, in, "exec", "-i", e.id, "--", "su", "-s", "/bin/sh", "postgres", "-c",
		"psql -X -At -q -v ON_ERROR_STOP=1 -d "+db)
	if err != nil {
		return "", fmt.Errorf("psql in %s: %w", e.id, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

type clientRow struct {
	PID   int     `json:"pid"`
	User  string  `json:"user"`
	Start float64 `json:"start"`
}

const (
	qDatabases = `SELECT coalesce(jsonb_agg(datname::text ORDER BY datname), '[]'::jsonb)::text
  FROM pg_database WHERE datallowconn AND NOT datistemplate`
	// %s es el literal del rol, ya validado.
	qVerifier = `SELECT jsonb_build_object('exists', count(*) > 0, 'verifier', max(coalesce(rolpassword, '')))::text
  FROM pg_authid WHERE rolname = %s`
	qClock   = `SELECT to_jsonb(extract(epoch FROM clock_timestamp())::float8)::text`
	qClients = `SELECT coalesce(jsonb_agg(t ORDER BY t.pid), '[]'::jsonb)::text FROM (
  SELECT pid, coalesce(usename::text, '') AS user, extract(epoch FROM backend_start)::float8 AS start
  FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()) t`
)

func runCopy(ctx context.Context, k klingc.Kling, ref string, r *report) error {
	out, err := k.Run(ctx, nil, "inspect", ref)
	if err != nil {
		return fmt.Errorf("doctor: kling inspect %s: %w", safe(ref, 64), err)
	}
	var mc api.Machine
	if err := json.Unmarshal(out, &mc); err != nil {
		return fmt.Errorf("doctor: kling inspect %s: %w", safe(ref, 64), err)
	}
	if !reID.MatchString(mc.ID) {
		return fmt.Errorf("doctor: kling inspect %s: unexpected machine id %q", safe(ref, 64), safe(mc.ID, 64))
	}
	r.target = fmt.Sprintf("machine %s (%s)", safe(mc.Name, 64), mc.ID)
	diskChecks(&mc, r)
	if mc.Hold != "" {
		r.add("DB055", High, "make room in the copy-on-write store (kling cow grow +4G): the copy resumes on its own",
			"the copy is on hold: %s; the rest of the checks need it running", safe(mc.Hold, 64))
		return nil
	}
	if mc.State != "" && mc.State != api.StateRunning && mc.DiskErrors > 0 {
		r.add("DB059", Info, "", "machine is %s: the rest of the checks need it running", safe(string(mc.State), 32))
		return nil
	}
	if mc.State != "" && mc.State != api.StateRunning {
		return fmt.Errorf("doctor: machine %s is %s; the doctor needs it running", safe(mc.Name, 64), safe(string(mc.State), 32))
	}

	golden := mc.Labels[LabelGolden]
	state, err := stateDir()
	if err != nil {
		return err
	}
	switch mc.Labels[LabelEngine] {
	case "mysql":
		return mysqlCopy(ctx, k, &mc, state, r)
	case "redis":
		return redisCopy(ctx, k, &mc, state, r)
	case "sqlite":
		return sqliteCopy(ctx, k, &mc, state, r)
	}
	appRole, err := appRoleOf(state, golden, mc.Labels)
	if err != nil {
		return err
	}
	qr := &execQuerier{k: k, id: mc.ID}

	var dbs []string
	if err := queryJSON(ctx, qr, "postgres", "databases", qDatabases, &dbs); err != nil {
		return fmt.Errorf("doctor: cannot query Postgres in %s: %w", safe(mc.Name, 64), err)
	}
	env := &sqlEnv{local: true, who: "'" + appRole + "'", appRole: appRole, dbs: []string{"postgres"}}
	for _, d := range dbs {
		if d == "postgres" {
			continue
		}
		if !reDB.MatchString(d) {
			r.add("DB019", Warn, "rename the database to a plain lowercase identifier",
				"database %s skipped: its name is not a plain identifier", q(d))
			continue
		}
		env.dbs = append(env.dbs, d)
	}
	sqlChecks(ctx, qr, env, r)

	if golden == "" {
		r.add("DB059", Info, "", "not a kling db copy (no %s label): copy checks skipped", LabelGolden)
		return nil
	}
	if !reGolden.MatchString(golden) {
		r.add("DB059", High, "recreate the copy from a golden with a plain name",
			"label %s has an unexpected value %s: copy checks skipped", LabelGolden, q(golden))
		return nil
	}
	copyChecks(ctx, k, qr, &mc, state, golden, appRole, r)
	return nil
}

// diskChecks avisa de los errores de disco que vio el invitado de la copia
// (api.Machine.DiskErrors): un almacén de copia al escribir lleno, o un disco
// del host que falla, le llegan como EIO, y Postgres hace PANIC o deja datos
// a medias.
func diskChecks(mc *api.Machine, r *report) {
	if mc.DiskErrors == 0 {
		return
	}
	cuando := ""
	if mc.DiskErrorAt != nil {
		cuando = " (last at " + mc.DiskErrorAt.Format("2006-01-02 15:04:05") + ")"
	}
	r.add("DB055", Critical, "check the copy-on-write store (kling cow; kling cow grow +4G) and the host disk, then destroy this copy and create it again: its data may be damaged",
		"the copy's guest got %d disk I/O error(s)%s: \"%s\"", mc.DiskErrors, cuando, safe(mc.DiskError, 160))
}

// appRoleOf es el rol de la aplicación: la etiqueta kling.db.role de la copia
// (la pone kling db up desde la plantilla, y la lleva también un golden
// guardado con kling save, que no tiene conn.env), o PGUSER del conn.env del
// dorado, o "app". Antes solo miraba el conn.env: con un golden sin él,
// "app", y DB052/DB020 falsos (role "app" does not exist).
func appRoleOf(state, golden string, labels map[string]string) (string, error) {
	role := "app"
	if golden != "" && reGolden.MatchString(golden) {
		b, err := readSmall(filepath.Join(state, golden, "conn.env"))
		if err == nil {
			sc := bufio.NewScanner(bytes.NewReader(b))
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "PGUSER="); ok {
					role = strings.TrimSpace(v)
				}
			}
		}
	}
	if v := labels["kling.db.role"]; v != "" {
		role = v
	}
	if !reRole.MatchString(role) {
		return "", fmt.Errorf("doctor: application role %q is not a plain identifier", safe(role, 64))
	}
	return role, nil
}

func copyChecks(ctx context.Context, k klingc.Kling, qr querier, mc *api.Machine, state, golden, appRole string, r *report) {
	ready := mc.Labels[LabelState] == StateReady
	if !ready {
		r.add("DB053", High, "finish preparing the copy (password rotation) or destroy it; kling db connect refuses it until then",
			"%s is %s, not %q", LabelState, q(mc.Labels[LabelState]), StateReady)
	}

	// Rotación: el verificador del rol de la app en ESTA copia.
	var vr struct {
		Exists   bool   `json:"exists"`
		Verifier string `json:"verifier"`
	}
	if err := queryJSON(ctx, qr, "postgres", "app_verifier", fmt.Sprintf(qVerifier, "'"+appRole+"'"), &vr); err != nil {
		checkFailed(r, "DB052", "the application role password", err)
	} else {
		passwordChecks(vr.Exists, vr.Verifier, mc.ID, state, golden, appRole, ready, r)
	}

	clockChecks(ctx, k, qr, mc.ID, r)

	var cl []clientRow
	if err := queryJSON(ctx, qr, "postgres", "clients", qClients, &cl); err != nil {
		checkFailed(r, "DB051", "client connections", err)
	} else {
		// Heredado = abierto antes de que existiera la copia (con 2 s de margen
		// para el reloj); las conexiones del agente llegan después.
		cut := math.Inf(1)
		if !mc.CreatedAt.IsZero() {
			cut = float64(mc.CreatedAt.UnixNano())/1e9 - maxSkew
		}
		n := 0
		for _, c := range cl {
			if c.Start < cut {
				n++
			}
		}
		if n > 0 {
			r.add("DB051", Warn, "rebuild the golden with no open clients; in this copy terminate them (pg_terminate_backend) and restart Postgres to reseed cancel keys (scripts/db-post-thaw.sh -restart)",
				"%d client connection(s) inherited from the golden: they share connection state and cancel keys with every other copy", n)
		}
	}
}

func passwordChecks(exists bool, v, id, state, golden, appRole string, ready bool, r *report) {
	if !exists {
		r.add("DB052", High, "recreate the copy from a golden built by scripts/db-golden.sh",
			"application role %s does not exist in the copy", q(appRole))
		return
	}
	if v == "" {
		r.add("DB052", Warn, "rotate the password (kling db rotates it when it prepares the copy)",
			"application role %s has no password: it cannot log in over the network", q(appRole))
		return
	}
	ver, perr := parseVerifier(v)
	if perr != nil {
		r.add("DB052", High, "rotate the password with a SCRAM-SHA-256 verifier",
			"the password of %s is not stored as a usable SCRAM-SHA-256 verifier (%s)", q(appRole), perr)
	}

	// ¿Sigue siendo la del dorado? Primero el verificador que guardó el host;
	// si no, la contraseña del dorado contra el verificador de la copia.
	gdir := filepath.Join(state, golden)
	verified, same := false, false
	if gv, err := readSmall(filepath.Join(gdir, "verifier")); err == nil {
		verified = true
		same = strings.TrimSpace(string(gv)) == v
	}
	if gp, err := readSmall(filepath.Join(gdir, "password")); err == nil && ver != nil && !same {
		verified = true
		same = ver.matches(strings.TrimRight(string(gp), "\r\n"))
	}
	switch {
	case same:
		r.add("DB052", Critical, "destroy this copy; kling db must rotate the password before marking a copy ready",
			"the password of %s is still the golden's: whoever knows %s's password can log in to this copy", q(appRole), q(golden))
	case !verified:
		r.add("DB052", Warn, fmt.Sprintf("keep the golden's verifier or password in %s so the rotation can be checked", filepath.Join(gdir, "verifier")),
			"cannot verify that the password of %s was rotated away from the golden's", q(appRole))
	}

	// La contraseña de ESTA copia en el host (la que usa kling db connect).
	// Misma ruta que dbstate (la que escribe kling db y lee connect): copies/<id>.
	p := filepath.Join(state, dbstate.CopiesDir, id, "password")
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		sev := Info
		if ready {
			sev = High
		}
		r.add("DB054", sev, "destroy the copy and create it again: the rotated password is only known to the host that rotated it",
			"no host password file for this copy (%s): kling db connect refuses it", p)
		return
	case err != nil:
		checkFailed(r, "DB054", "the host password file", err)
		return
	case !fi.Mode().IsRegular():
		r.add("DB054", High, "replace it with a regular file (0600)", "host password file %s is not a regular file", p)
		return
	case fi.Mode().Perm()&0o077 != 0:
		r.add("DB054", High, fmt.Sprintf("chmod 600 %s", p),
			"host password file %s is readable by other users (mode %04o)", p, fi.Mode().Perm())
	}
	if ver == nil {
		return
	}
	cp, err := readSmall(p)
	if err != nil {
		checkFailed(r, "DB054", "the host password file", err)
		return
	}
	if !ver.matches(strings.TrimRight(string(cp), "\r\n")) {
		r.add("DB054", High, "rotate the password again or recreate the copy",
			"host password file %s does not match the role's verifier: kling db connect will fail", p)
	}
}

func clockChecks(ctx context.Context, k klingc.Kling, qr querier, id string, r *report) {
	// El reloj del kernel del invitado (entero, `date +%s` trunca).
	before := time.Now()
	out, err := k.Run(ctx, nil, "exec", id, "--", "date", "+%s")
	after := time.Now()
	if err != nil {
		checkFailed(r, "DB050", "the guest clock", err)
	} else if g, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr != nil {
		checkFailed(r, "DB050", "the guest clock", fmt.Errorf("unexpected date output"))
	} else if sk := skew(float64(g), float64(g)+1, before, after); sk > maxSkew {
		r.add("DB050", High, "restart the copy; if it persists the guest agent did not resync the clock after the thaw (see the daemon log)",
			"guest clock is %.1fs off the host clock", sk)
	}

	before = time.Now()
	var pg float64
	err = queryJSON(ctx, qr, "postgres", "clock", qClock, &pg)
	after = time.Now()
	if err != nil {
		checkFailed(r, "DB050", "the Postgres clock", err)
	} else if sk := skew(pg, pg, before, after); sk > maxSkew {
		r.add("DB050", High, "restart the copy; Postgres reads the guest clock, which did not resync after the thaw",
			"Postgres clock_timestamp() is %.1fs off the host clock", sk)
	}
}

// skew es la distancia entre el intervalo del invitado [glo, ghi] y el
// intervalo del host en que se leyó [before, after]: 0 si se solapan.
func skew(glo, ghi float64, before, after time.Time) float64 {
	b := float64(before.UnixNano()) / 1e9
	a := float64(after.UnixNano()) / 1e9
	switch {
	case ghi < b:
		return b - ghi
	case glo > a:
		return glo - a
	}
	return 0
}

// readSmall lee un fichero pequeño de estado sin seguir enlaces simbólicos.
func readSmall(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4096))
}
