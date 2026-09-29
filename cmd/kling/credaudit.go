package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/api"
)

// kling machine audit <ref>: el registro de auditoría del proxy de
// credenciales de una máquina (qué peticiones pasaron por él, cuáles se
// denegaron y con qué credencial). -f sigue el registro como `kling logs -f`
// sigue la consola: sondeando la ventana del final (followLogs), con un
// adaptador que convierte cada registro en una línea y la vuelve a pintar.

func cmdCredAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	host := hostFlag(fs)
	tail := fs.Int("tail", 200, "last N records (0 = all the daemon keeps)")
	follow := fs.Bool("f", false, "keep printing new records until Ctrl-C or until the machine stops")
	denied := fs.Bool("denied", false, "only requests denied by policy (no credential, -allow-request, ambiguous path)")
	since := fs.String("since", "", "only records since an RFC 3339 time or a duration back from now (10m, 2h)")
	asJSON := fs.Bool("json", false, "one JSON record per line")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *tail < 0 {
		return fmt.Errorf("usage: kling machine audit <ref> [-f] [-denied] [-since 10m|RFC3339] [-tail N] [-json]")
	}
	q := api.CredAuditQuery{Tail: *tail, Denied: *denied}
	if *since != "" {
		ts, err := parseSince(*since, time.Now())
		if err != nil {
			return err
		}
		q.Since = ts
	}
	ref := fs.Arg(0)

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	src := auditSource{c: c, ref: ref, q: q}
	out, err := src.fetch(ctx)
	if err != nil {
		return err
	}
	w := &auditWriter{out: os.Stdout, errOut: os.Stderr, json: *asJSON}
	if !*asJSON {
		w.header()
	}
	lines := splitLines(out)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	if !*follow {
		return nil
	}
	// Siguiendo, la ventana es la de followLogs: con -tail pequeño una ráfaga
	// podría pasar entera entre dos sondeos sin que se viera.
	src.q.Tail = followWindow
	err = followLogs(ctx, w, src, lines, followEvery)
	if ctx.Err() != nil {
		return nil // Ctrl-C es la forma normal de terminar
	}
	return err
}

// parseSince acepta un instante RFC 3339 o una duración hacia atrás.
func parseSince(s string, now time.Time) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid -since %q: use an RFC 3339 time or a duration like 10m", s)
}

// auditSource es el logFetcher de followLogs para el registro: cada registro
// es una línea JSON (única por su ts con nanosegundos), y eso es lo que
// followLogs compara para saber qué es nuevo.
type auditSource struct {
	c   *api.Client
	ref string
	q   api.CredAuditQuery
}

func (s auditSource) fetch(ctx context.Context) (string, error) {
	recs, err := s.c.CredAudit(ctx, s.ref, s.q)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range recs {
		j, err := json.Marshal(r)
		if err != nil {
			return "", err
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// running: a diferencia de la consola, una máquina congelada o en pausa sigue
// teniendo proxy (despierta sola con la siguiente petición en el gateway MCP),
// así que se sigue esperando; solo una parada o fallida termina.
func (s auditSource) running(ctx context.Context) (bool, string, error) {
	mc, err := s.c.Get(ctx, s.ref)
	if err != nil {
		return false, "", err
	}
	return mc.State != api.StateStopped && mc.State != api.StateFailed, string(mc.State), nil
}

// auditWriter recibe de followLogs una línea JSON por escritura y la pinta:
// tal cual con -json, como fila de tabla si no. Los descartados van a stderr
// en los dos modos: que nadie los pase por alto.
type auditWriter struct {
	out, errOut io.Writer
	json        bool
}

const auditRowFmt = "%-14s  %-7s  %-24s  %-32s  %6s  %-12s  %6s  %s\n"

func (w *auditWriter) header() {
	fmt.Fprintf(w.out, auditRowFmt, "TIME", "METHOD", "HOST", "PATH", "STATUS", "CREDS", "MS", "RESULT")
}

func (w *auditWriter) Write(p []byte) (int, error) {
	line := bytes.TrimSpace(p)
	if len(line) == 0 {
		return len(p), nil
	}
	var r api.CredAuditRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return 0, fmt.Errorf("credential audit record: %w", err)
	}
	if r.Dropped > 0 {
		fmt.Fprintf(w.errOut, "dropped %d records\n", r.Dropped)
	}
	if w.json {
		fmt.Fprintf(w.out, "%s\n", line)
		return len(p), nil
	}
	if r.Kind == "dropped" {
		return len(p), nil
	}
	creds := strings.Join(r.Creds, ",")
	if creds == "" {
		creds = "-"
	}
	method, path, status := r.Method, r.Path, fmt.Sprint(r.Status)
	if r.Kind == "postgres" {
		// Una conexión al proxy de Postgres: sin método ni ruta ni estado
		// HTTP; en su lugar el rol y la base de datos.
		method, path, status = "PG", r.User, "-"
		if r.Method == "cancel" {
			method = "CANCEL"
		}
		if r.Database != "" {
			path += "@" + r.Database
		}
		// Una credencial sin -database (o de un almacén antiguo) entra en
		// cualquier base: que se vea en cada fila, no solo en inspect.
		if r.AnyDatabase {
			path += " (any_database)"
		}
		if path == "" {
			path = "-"
		}
	}
	if r.Kind == "link" {
		// Una conexión por una arista link de un grafo: TCP crudo, sin
		// método ni estado; en la ruta, a qué máquina llegó.
		method, path, status = "LINK", r.Upstream, "-"
		if path == "" {
			path = "-"
		}
	}
	fmt.Fprintf(w.out, auditRowFmt, r.TS.Local().Format("01-02 15:04:05"), printable(method), printable(r.Host),
		printable(path), status, printable(creds), fmt.Sprint(r.MS), printable(auditResult(r)))
	return len(p), nil
}

// printable cambia por "?" los caracteres de control: la ruta y el host los
// eligió el invitado, y la tabla va a un terminal. El proxy ya los limpia al
// escribir; esto cubre un registro de otra versión o tocado a mano.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// auditResult es la última columna: ok, el motivo de un fallo o límite, o
// DENIED(motivo) cuando lo rechazó la política.
func auditResult(r api.CredAuditRecord) string {
	switch {
	case r.Denied:
		return "DENIED(" + r.Reason + ")"
	case r.Reason != "":
		return r.Reason
	case r.Auth != "":
		// Postgres: con qué se autenticó el proxy ante el servidor.
		return "ok(" + r.Auth + ")"
	}
	return "ok"
}
