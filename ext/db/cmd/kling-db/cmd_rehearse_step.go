package main

// kling db rehearse <copia|golden> -step "<cmd>" -agent <plantilla>: ensayo de
// una migración que hace un PROGRAMA (alembic upgrade head, prisma migrate...)
// y no un .sql. AuraCRM no puede generar SQL offline (alembic --sql falla en
// sus 8 servicios), así que lo único ensayable es el programa mismo.
//
// Como -migrations, sobre una copia desechable. Además:
//   - la observación (kling db observe) se activa en la copia: cada sentencia
//     de las conexiones del agente va al log de Postgres con su duración;
//   - el comando corre en una microVM del agente con la copia por attach
//     (como golden build -step), con su tiempo de pared y el muestreo de
//     esperas por locks;
//   - el lock_timeout se fija para el rol en la copia: una sentencia que
//     esperaría por un lock falla como "would block";
//   - el informe agrupa las sentencias que se ejecutaron (sin literales) y
//     marca las que piden un lock fuerte (ACCESS EXCLUSIVE, SHARE...): las que
//     en producción bloquearían lecturas o escrituras mientras duran.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// strongLock dice qué lock pide una sentencia normalizada, si es de los que
// bloquean a otros (lecturas o escrituras) mientras dura; "" si no.
func strongLock(norm string) string {
	u := strings.ToUpper(strings.TrimSpace(norm))
	has := func(p string) bool { return strings.HasPrefix(u, p) }
	switch {
	case has("CREATE INDEX CONCURRENTLY"), has("CREATE UNIQUE INDEX CONCURRENTLY"),
		has("REINDEX") && strings.Contains(u, "CONCURRENTLY"),
		has("REFRESH MATERIALIZED VIEW CONCURRENTLY"),
		has("DROP INDEX CONCURRENTLY"):
		return ""
	case has("ALTER TABLE") && strings.Contains(u, "FOREIGN KEY") && !strings.Contains(u, "NOT VALID"):
		return "SHARE ROW EXCLUSIVE (blocks writes; validates every row)"
	case has("ALTER TABLE"), has("DROP TABLE"), has("TRUNCATE"), has("DROP INDEX"),
		has("REINDEX"), has("CLUSTER"), has("VACUUM FULL"), has("ALTER INDEX"),
		has("REFRESH MATERIALIZED VIEW"), has("DROP MATERIALIZED VIEW"), has("ALTER MATERIALIZED VIEW"):
		return "ACCESS EXCLUSIVE (blocks reads and writes)"
	case has("CREATE INDEX"), has("CREATE UNIQUE INDEX"):
		return "SHARE (blocks writes; use CREATE INDEX CONCURRENTLY)"
	case has("CREATE TRIGGER"), has("ALTER TRIGGER"), has("DROP TRIGGER"):
		return "SHARE ROW EXCLUSIVE (blocks writes)"
	case has("LOCK TABLE"), has("LOCK "):
		return "explicit LOCK"
	}
	return ""
}

// stepRehearse son los números de un -step para el informe.
type stepRehearse struct {
	Statements  []observeStmt `json:"statements"`
	StrongLocks []strongStmt  `json:"strong_locks"`
	Logged      int64         `json:"logged_statements"`
}

// strongStmt es una sentencia que pide un lock fuerte.
type strongStmt struct {
	observeStmt
	Lock string `json:"lock"`
}

// rehearseStep ensaya un -step sobre la copia desechable mc (ver arriba).
func (a *app) rehearseStep(ctx context.Context, mc *api.Machine, o stepOpts, lockTimeout time.Duration, rep *rehearseReport) error {
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return err
	}
	// Observación y lock_timeout para las conexiones NUEVAS del rol: las del
	// agente. Es una copia desechable: no hay que deshacerlo.
	set := observeSQL(db, true) + fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET lock_timeout = '%dms';\n", role, db, lockTimeout.Milliseconds())
	if _, err := a.sqlSuper(ctx, mc.ID, set, "turning observation on in the throwaway copy"); err != nil {
		return err
	}
	logStart, err := a.logSize(ctx, mc.ID)
	if err != nil {
		return err
	}
	size, err := a.dbSize(ctx, mc.ID, db)
	if err != nil {
		return err
	}
	agent, err := a.startAgent(ctx, o, mc, func(f string, args ...any) { fmt.Fprintf(a.stderr, "rehearse: "+f+"\n", args...) })
	if agent != nil {
		defer a.k.Run(context.WithoutCancel(ctx), nil, "rm", "-f", agent.ID) //nolint:errcheck
	}
	if err != nil {
		return err
	}
	f := rehearseFile{File: "step: " + o.steps[0].cmd, SizeBefore: size, SizeAfter: size}
	stopSampling := a.lockSampler(ctx, mc.ID)
	start := nowFn()
	err = a.runStepCmd(ctx, agent, o, o.steps[0].cmd, defaultAttachHost(mc), role, db)
	f.DurationMS = nowFn().Sub(start).Milliseconds()
	if n := stopSampling(); n > 0 {
		f.WaitedForLocks = true
		f.LockWaitSeconds = float64(n) * lockSampleEvery.Seconds()
	}
	f.Status = "ok"
	if err != nil {
		f.Status = "failed"
		f.Error = "the step failed (its output is above)"
		if ce := ctxErr(err); ce != "" {
			f.Error = ce
		}
	}
	if after, serr := a.dbSize(ctx, mc.ID, db); serr == nil {
		f.SizeAfter = after
	}
	// Lo que corrió, del log de Postgres desde que empezó el paso.
	out, lerr := a.k.Run(ctx, nil, "exec", "-timeout", "60s", mc.ID, "--", "tail", "-c", "+"+strconv.FormatInt(logStart+1, 10), pgLogFile)
	if lerr == nil {
		obs := analyzeObserveLog(string(out), db, "", "", observeMaxStatements)
		st := &stepRehearse{Statements: obs.Statements, Logged: obs.Logged, StrongLocks: []strongStmt{}}
		for _, s := range obs.Statements {
			if l := strongLock(s.Statement); l != "" {
				st.StrongLocks = append(st.StrongLocks, strongStmt{s, l})
			}
		}
		if strings.Contains(string(out), "due to lock timeout") {
			f.WaitedForLocks = true
			f.WouldBlockSeconds = lockTimeout.Seconds()
		}
		if len(st.Statements) > 20 {
			st.Statements = st.Statements[:20]
		}
		rep.Step = st
	}
	rep.Files = append(rep.Files, f)
	rep.OK = f.Status == "ok"
	return nil
}

// logSize es el tamaño del log de Postgres de la copia, para leer después
// solo lo que se escribió desde ahora.
func (a *app) logSize(ctx context.Context, id string) (int64, error) {
	out, err := a.k.Run(ctx, nil, "exec", "-timeout", "30s", id, "--", "sh", "-c", "wc -c < "+pgLogFile)
	if err != nil {
		return 0, fmt.Errorf("reading the postgres log size of %s: %w", shortID(id), err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reading the postgres log size of %s: %q", shortID(id), strings.TrimSpace(string(out)))
	}
	return n, nil
}

// writeStepReport es la parte del informe de rehearse de un -step.
func writeStepReport(w io.Writer, st *stepRehearse) {
	if st == nil {
		return
	}
	fmt.Fprintf(w, "\n%d statements logged; the slowest:\n", st.Logged)
	for _, s := range st.Statements {
		fmt.Fprintf(w, "  %8.1f ms  x%-4d %s\n", s.TotalMS, s.Calls, clip(s.Statement, 120))
	}
	if len(st.StrongLocks) == 0 {
		fmt.Fprintln(w, "\nno statement takes a lock that blocks other sessions")
		return
	}
	fmt.Fprintln(w, "\nstatements that take a strong lock (in production they block others while they run):")
	for _, s := range st.StrongLocks {
		fmt.Fprintf(w, "  %8.1f ms  %s\n      %s\n", s.TotalMS, s.Lock, clip(s.Statement, 160))
	}
}
