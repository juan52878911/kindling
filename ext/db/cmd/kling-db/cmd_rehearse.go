package main

// kling db rehearse <copia|golden> -migrations DIR: ensayo de migraciones.
//
// Se hace sobre una copia DESECHABLE (un fork de una copia lista, o un up de un
// golden), nunca sobre el origen. Cada .sql se aplica en orden, por stdin a
// psql con ON_ERROR_STOP, y se mide: duración, si esperó por bloqueos, y
// tamaño de la base antes y después.
//
// Como el rol de la aplicación DESDE LA AUTENTICACIÓN: el psql entra por el
// socket con -U <rol> gracias a un mapa peer (el mismo mecanismo que ask), no
// como postgres con role=<rol>, que un RESET ROLE en el .sql convertiría en
// superusuario. Lo que no se cierra: los metacomandos de psql (\!) ejecutan
// órdenes como el usuario del sistema postgres del invitado. Las migraciones
// son código de confianza; la copia es desechable (ver docs/db.md).
//
// Sobre los bloqueos, con honestidad: en una copia aislada nadie más toma
// locks, así que lo que se ve es lo que la propia migración provoca (p. ej. una
// sesión del agente que sigue abierta). Se detecta de dos formas: un muestreo
// de pg_stat_activity mientras corre cada fichero (wait_event_type = Lock) y el
// fallo por lock_timeout, que se informa como "would block N s in production".
// Sirve para descubrir qué sentencias PIDEN un lock fuerte; cuánto esperarían
// en producción depende del tráfico de allí.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/doctor"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const (
	maxMigrationFile  = 32 << 20 // un .sql más grande no es una migración
	maxMigrationFiles = 1000
	migrationTimeout  = "30m"
)

// Sustituibles en los tests.
var (
	lockSampleEvery = 500 * time.Millisecond
	nowFn           = time.Now
)

// migration es un .sql ya leído: se lee todo antes de crear nada, para fallar
// pronto y para que lo aplicado sea lo que se validó.
type migration struct {
	Name string
	SQL  []byte
}

type rehearseFile struct {
	File              string  `json:"file"`
	Status            string  `json:"status"` // ok | failed | skipped
	DurationMS        int64   `json:"duration_ms"`
	WaitedForLocks    bool    `json:"waited_for_locks"`
	LockWaitSeconds   float64 `json:"lock_wait_seconds,omitempty"`
	WouldBlockSeconds float64 `json:"would_block_seconds,omitempty"`
	SizeBefore        int64   `json:"size_before_bytes"`
	SizeAfter         int64   `json:"size_after_bytes"`
	Error             string  `json:"error,omitempty"`
	// StrongLocks son los locks fuertes que la migración tuvo concedidos
	// mientras corría (pg_locks, muestreado: ~segundos que se vio cada uno);
	// StrongStatements, las sentencias del fichero que los piden (también las
	// que duran menos que una muestra). En producción, mientras duran,
	// bloquean a las demás sesiones.
	StrongLocks      []string `json:"strong_locks,omitempty"`
	StrongStatements []string `json:"strong_statements,omitempty"`
}

type rehearseReport struct {
	Source             string         `json:"source"`
	SourceKind         string         `json:"source_kind"` // copy | template
	LockTimeoutSeconds float64        `json:"lock_timeout_seconds"`
	OK                 bool           `json:"ok"`
	Files              []rehearseFile `json:"files"`
	KeptCopy           string         `json:"kept_copy,omitempty"`
	Step               *stepRehearse  `json:"step,omitempty"`
}

func cmdRehearse(args []string) error {
	fs, host, owner := newFlags("rehearse")
	dir := fs.String("migrations", "", "directory with the .sql files, applied in name order")
	step := fs.String("step", "", "instead of -migrations: a command that migrates (alembic upgrade head...), run in a -agent microVM against the copy")
	agent := fs.String("agent", "", "with -step: the template of the microVM where it runs")
	workdir := fs.String("workdir", "", "with -step: directory uploaded to /work in the agent")
	envFile := fs.String("env-file", "", "with -step: KEY=VALUE lines for its environment (0600)")
	stepTimeout := fs.Duration("step-timeout", stepTimeoutDefault, "with -step: how long it may run")
	lockTimeout := fs.Duration("lock-timeout", 5*time.Second, "lock_timeout for each statement (a hit is reported as \"would block\")")
	keep := fs.Bool("keep", false, "keep the throwaway copy instead of destroying it")
	asJSON := fs.Bool("json", false, "JSON report")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*dir == "") == (*step == "") {
		return usageErr("usage: kling db rehearse <copy|template> -migrations DIR [-lock-timeout 5s] [-keep] [-json]\n" +
			"       kling db rehearse <copy|template> -step CMD -agent T [-workdir DIR] [-env-file F] [-lock-timeout 5s] [-keep] [-json]")
	}
	var so *stepOpts
	if *step != "" {
		if *agent == "" || !namePattern.MatchString(*agent) {
			return usageErr("-step needs -agent <template>")
		}
		if *envFile != "" {
			if err := checkEnvFile(*envFile); err != nil {
				return err
			}
		}
		so = &stepOpts{steps: []goldenStep{{cmd: *step}}, agent: *agent, workdir: *workdir, envFile: *envFile,
			timeout: *stepTimeout, name: "rehearse-" + randomSuffix()}
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	rep, err := a.rehearse(ctx, pos[0], *owner, *dir, so, *lockTimeout, *keep)
	if rep != nil {
		if werr := writeRehearse(a.stdout, rep, *asJSON); werr != nil && err == nil {
			err = werr
		}
		if err == nil && !rep.OK {
			err = &plugin.ExitError{Code: 1, Err: errors.New("the rehearsal failed")}
		}
	}
	return err
}

// readMigrations lee los .sql de dir, en orden de nombre. Solo ficheros
// normales: un enlace simbólico podría sacar de dir lo que se envía a la base.
func readMigrations(dir string) ([]migration, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no .sql files in %s", dir)
	}
	if len(names) > maxMigrationFiles {
		return nil, fmt.Errorf("%d .sql files in %s (at most %d)", len(names), dir, maxMigrationFiles)
	}
	out := make([]migration, 0, len(names))
	for _, n := range names {
		p := filepath.Join(dir, n)
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file (links are not followed)", p)
		}
		if st.Size() > maxMigrationFile {
			return nil, fmt.Errorf("%s is larger than %d MiB", p, maxMigrationFile>>20)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{Name: n, SQL: b})
	}
	return out, nil
}

// rehearse crea la copia desechable, aplica las migraciones y la destruye
// (salvo keep). Devuelve el informe aunque una migración falle: el error solo
// es no nil si no se pudo ni ensayar.
func (a *app) rehearse(ctx context.Context, ref, owner, dir string, so *stepOpts, lockTimeout time.Duration, keep bool) (*rehearseReport, error) {
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	if lockTimeout < 100*time.Millisecond || lockTimeout > time.Hour {
		return nil, errors.New("-lock-timeout must be between 100ms and 1h")
	}
	var migs []migration
	if so == nil {
		var err error
		if migs, err = readMigrations(dir); err != nil {
			return nil, err
		}
	}
	rep := &rehearseReport{Source: ref, LockTimeoutSeconds: lockTimeout.Seconds()}

	// ¿Copia o golden? Una máquina que sea copia de kling db manda; una
	// máquina ajena con ese nombre no impide que sea el nombre de un golden.
	var mc *api.Machine
	if src, err := a.inspect(ctx, ref); err == nil && src.Labels[labelGolden] != "" {
		if err := owned(src, owner); err != nil {
			return nil, err
		}
		if err := requirePostgres(src, "rehearse"); err != nil {
			return nil, err
		}
		if st := src.Labels[labelState]; st != stateReady {
			return nil, fmt.Errorf("%s is not ready (%s=%q)", src.Name, labelState, st)
		}
		// Ramificar una copia congelada la descongelaría: tocar el origen.
		if src.State != api.StateRunning {
			return nil, fmt.Errorf("%s is %s: thaw it first (kling thaw %s); rehearse never changes the source", src.Name, src.State, src.Name)
		}
		rep.SourceKind = "copy"
		copies, err := a.fork(ctx, src.ID, 1, owner)
		if err != nil {
			return nil, err
		}
		mc = copies[0]
	} else {
		rep.SourceKind = "template"
		if snap, err := a.template(ctx, ref); err == nil {
			if _, _, engine, err := goldenInfo(snap); err == nil && engine != enginePostgres {
				return nil, fmt.Errorf("kling db rehearse supports postgres templates only in this version; %s is %s (see %s)", ref, engine, engineDoc(engine))
			}
		}
		name := "rehearse-" + randomSuffix()
		mc, err = a.up(ctx, ref, name, 0, owner)
		if err != nil {
			return nil, err
		}
	}
	if keep {
		rep.KeptCopy = mc.Name
	} else {
		defer a.destroy(mc.ID)
	}
	if so != nil {
		return rep, a.rehearseStep(ctx, mc, *so, lockTimeout, rep)
	}
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return rep, err
	}
	// El usuario del sistema postgres puede entrar como el rol de la
	// aplicación por el socket (y solo por él): ver migrationCmd.
	if _, err := a.k.Run(ctx, strings.NewReader(fmt.Sprintf(allowPeerScript, role, rehearseMap, db)),
		"exec", "-i", "-timeout", "60s", mc.ID, "--", "sh", "-s"); err != nil {
		return rep, fmt.Errorf("letting the migrations log in as %s in %s: %w", role, mc.Name, err)
	}

	size, err := a.dbSize(ctx, mc.ID, db)
	if err != nil {
		return rep, err
	}
	failed := false
	for _, m := range migs {
		f := rehearseFile{File: m.Name, SizeBefore: size, SizeAfter: size}
		if failed {
			f.Status = "skipped"
			rep.Files = append(rep.Files, f)
			continue
		}
		a.runMigration(ctx, mc.ID, role, db, lockTimeout, m, &f)
		if f.Status == "ok" {
			if after, serr := a.dbSize(ctx, mc.ID, db); serr == nil {
				f.SizeAfter = after
				size = after
			}
		} else {
			failed = true
		}
		rep.Files = append(rep.Files, f)
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
	}
	rep.OK = !failed
	return rep, nil
}

// rehearseMap es el mapa de pg_ident.conf que deja al usuario del sistema
// postgres entrar como el rol de la aplicación en la copia desechable.
const rehearseMap = "kling_db_rehearse"

// migrationCmd es la orden de psql de una migración: entra COMO el rol por el
// socket (peer con rehearseMap), así que session_user es el rol y ni RESET
// ROLE ni SET ROLE llevan al superusuario. Todo lo variable pasó identPattern
// o es un entero: nada que escapar.
func migrationCmd(role, db string, lockTimeout time.Duration) string {
	return fmt.Sprintf("PGOPTIONS='-c lock_timeout=%d' psql -X -q -At -v ON_ERROR_STOP=1 -h /run/postgresql -U %s -d %s",
		lockTimeout.Milliseconds(), role, db)
}

// lockSample es lo que vio el muestreo de una migración: en cuántas muestras
// alguna sesión esperaba por un lock, y qué locks fuertes de relación tenían
// concedidos las sesiones de cliente ("AccessExclusiveLock public.orders"),
// con en cuántas muestras se vio cada uno.
type lockSample struct {
	waits  int
	strong map[string]int
}

// lockSampler muestrea, hasta que se llama a la función que devuelve, las
// esperas por locks y los locks fuertes concedidos en la copia id.
func (a *app) lockSampler(ctx context.Context, id string) func() lockSample {
	sampleCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var mu sync.Mutex
	res := lockSample{strong: map[string]int{}}
	if lockSampleEvery > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-sampleCtx.Done():
					return
				case <-time.After(lockSampleEvery):
				}
				out, err := a.k.Run(sampleCtx, strings.NewReader(lockWaitersSQL), "exec", "-i", "-timeout", "10s", id, "--",
					"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
				if err != nil {
					continue
				}
				n, locks := parseLockSample(string(out))
				mu.Lock()
				if n > 0 {
					res.waits++
				}
				for _, l := range locks {
					res.strong[l]++
				}
				mu.Unlock()
			}
		}()
	}
	return func() lockSample {
		stop()
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return res
	}
}

// parseLockSample lee la salida de lockWaitersSQL: el recuento de esperas y,
// en las líneas siguientes, un lock fuerte por línea.
func parseLockSample(out string) (int, []string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	n, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, nil
	}
	var locks []string
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" {
			locks = append(locks, doctor.Safe(l, 200))
		}
	}
	return n, locks
}

// runMigration aplica un fichero y rellena f.
func (a *app) runMigration(ctx context.Context, id, role, db string, lockTimeout time.Duration, m migration, f *rehearseFile) {
	stopSampling := a.lockSampler(ctx, id)
	start := nowFn()
	_, err := a.k.Run(ctx, strings.NewReader(string(m.SQL)), "exec", "-i", "-timeout", migrationTimeout, id, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", migrationCmd(role, db, lockTimeout))
	f.DurationMS = nowFn().Sub(start).Milliseconds()
	ls := stopSampling()
	if ls.waits > 0 {
		f.WaitedForLocks = true
		f.LockWaitSeconds = float64(ls.waits) * lockSampleEvery.Seconds()
	}
	f.StrongLocks = heldLocks(ls)
	f.StrongStatements = strongStatements(string(m.SQL))
	if err == nil {
		f.Status = "ok"
		return
	}
	f.Status = "failed"
	var ke *klingc.Error
	msg := ""
	if errors.As(err, &ke) {
		msg = ke.Stderr
	}
	if strings.Contains(msg, "lock timeout") {
		f.WaitedForLocks = true
		f.WouldBlockSeconds = lockTimeout.Seconds()
	}
	f.Error = sqlError(msg, err)
}

// sqlError deja solo la línea ERROR de psql: el resto cita el texto de la
// sentencia (LINE 1: ...), que puede llevar datos.
func sqlError(stderr string, err error) string {
	for _, l := range strings.Split(stderr, "\n") {
		if i := strings.Index(l, "ERROR:"); i >= 0 {
			l = strings.TrimSpace(l[i:])
			if len(l) > 300 {
				l = l[:300] + "..."
			}
			return l
		}
	}
	if ce := ctxErr(err); ce != "" {
		return ce
	}
	return "psql failed (details omitted: the output may quote the statement)"
}

func ctxErr(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "interrupted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return ""
}

const lockWaitersSQL = `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND backend_type = 'client backend';
SELECT DISTINCT l.mode || ' ' || n.nspname || '.' || c.relname
  FROM pg_locks l
  JOIN pg_stat_activity a ON a.pid = l.pid
  JOIN pg_class c ON c.oid = l.relation
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE l.granted AND l.locktype = 'relation' AND a.backend_type = 'client backend' AND a.pid <> pg_backend_pid()
   AND l.mode IN ('AccessExclusiveLock', 'ExclusiveLock', 'ShareRowExclusiveLock', 'ShareLock')
   AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema';
`

// heldLocks es la lista de locks fuertes vistos, con los segundos aproximados.
func heldLocks(ls lockSample) []string {
	var out []string
	for l, n := range ls.strong {
		out = append(out, fmt.Sprintf("%s (~%s s)", l, trimFloat(float64(n)*lockSampleEvery.Seconds())))
	}
	sort.Strings(out)
	return out
}

// strongStatements son las sentencias de sql que piden un lock fuerte (ver
// strongLock), normalizadas (sin literales) y con el lock que piden.
func strongStatements(sql string) []string {
	var out []string
	for _, st := range splitStatements(sql) {
		norm := normalizeSQL(st)
		if l := strongLock(norm); l != "" {
			out = append(out, clip(norm, 160)+"  ["+l+"]")
		}
	}
	return out
}

// splitStatements parte un .sql en sentencias por ';' fuera de comillas,
// comentarios y bloques $$: para clasificarlas, no para ejecutarlas.
func splitStatements(sql string) []string {
	var out []string
	var b strings.Builder
	inQ, inLine, inBlock := false, false, false
	dollar := ""
	r := []rune(sql)
	for i := 0; i < len(r); i++ {
		c := r[i]
		next := rune(0)
		if i+1 < len(r) {
			next = r[i+1]
		}
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
			continue
		case inBlock:
			if c == '*' && next == '/' {
				inBlock = false
				i++
			}
			continue
		case dollar != "":
			b.WriteRune(c)
			if c == '$' && strings.HasPrefix(string(r[i:]), dollar) {
				b.WriteString(dollar[1:])
				i += len([]rune(dollar)) - 1
				dollar = ""
			}
			continue
		case inQ:
			b.WriteRune(c)
			if c == '\'' {
				inQ = false
			}
			continue
		}
		switch {
		case c == '-' && next == '-':
			inLine = true
			i++
		case c == '/' && next == '*':
			inBlock = true
			i++
		case c == '\'':
			inQ = true
			b.WriteRune(c)
		case c == '$':
			j := i + 1
			for j < len(r) && (r[j] == '_' || r[j] >= 'a' && r[j] <= 'z' || r[j] >= 'A' && r[j] <= 'Z' || r[j] >= '0' && r[j] <= '9') {
				j++
			}
			if j < len(r) && r[j] == '$' {
				dollar = string(r[i : j+1])
				b.WriteString(dollar)
				i = j
			} else {
				b.WriteRune(c)
			}
		case c == ';':
			if t := strings.TrimSpace(b.String()); t != "" {
				out = append(out, t)
			}
			b.Reset()
		default:
			b.WriteRune(c)
		}
	}
	if t := strings.TrimSpace(b.String()); t != "" {
		out = append(out, t)
	}
	return out
}

// dbSize es pg_database_size de la base de la copia, en bytes.
func (a *app) dbSize(ctx context.Context, id, db string) (int64, error) {
	sql := fmt.Sprintf("SELECT pg_database_size('%s');\n", db) // db pasó identPattern
	out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", "60s", id, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
	if err != nil {
		return 0, fmt.Errorf("measuring the size of %s: %w", shortID(id), err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("measuring the size of %s: unexpected answer %q", shortID(id), strings.TrimSpace(string(out)))
	}
	return n, nil
}

func writeRehearse(w io.Writer, rep *rehearseReport, asJSON bool) error {
	if asJSON {
		if rep.Files == nil {
			rep.Files = []rehearseFile{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "FILE\tSTATUS\tTIME\tLOCKS\tSIZE BEFORE\tSIZE AFTER")
	for _, f := range rep.Files {
		locks := "-"
		switch {
		case f.WouldBlockSeconds > 0:
			locks = fmt.Sprintf("would block %s s in production", trimFloat(f.WouldBlockSeconds))
		case f.WaitedForLocks:
			locks = fmt.Sprintf("waited ~%s s", trimFloat(f.LockWaitSeconds))
		}
		took, before, after := "-", "-", "-"
		if f.Status != "skipped" {
			took = (time.Duration(f.DurationMS) * time.Millisecond).String()
			before, after = humanBytes(f.SizeBefore), humanBytes(f.SizeAfter)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.File, f.Status, took, locks, before, after)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, f := range rep.Files {
		if f.Error != "" {
			fmt.Fprintf(w, "\n%s: %s\n", f.File, f.Error)
		}
		if len(f.StrongLocks)+len(f.StrongStatements) > 0 {
			fmt.Fprintf(w, "\n%s takes strong locks (in production they block other sessions while they last):\n", f.File)
			for _, l := range f.StrongLocks {
				fmt.Fprintf(w, "  held: %s\n", l)
			}
			for _, st := range f.StrongStatements {
				fmt.Fprintf(w, "  %s\n", st)
			}
		}
	}
	writeStepReport(w, rep.Step)
	verdict := "OK"
	if !rep.OK {
		verdict = "FAILED"
	}
	fmt.Fprintf(w, "\nrehearsal on %s %s (lock_timeout %s s): %s\n", rep.SourceKind, rep.Source, trimFloat(rep.LockTimeoutSeconds), verdict)
	if rep.KeptCopy != "" {
		fmt.Fprintf(w, "kept the copy %s: kling db connect %s -psql   ·   remove it with kling db rm %s\n", rep.KeptCopy, rep.KeptCopy, rep.KeptCopy)
	}
	return nil
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
