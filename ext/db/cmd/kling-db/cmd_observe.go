package main

// kling db observe: el modo de observación de una copia (pensado para las de
// un golden de kling db slice). Activarlo pone, para la base de la
// aplicación, log_min_duration_statement = 0 (cada sentencia terminada va al
// log de Postgres con su duración) y log_parameter_max_length = 0 (sin los
// valores de los parámetros enlazados); el superusuario queda fuera (ALTER
// ROLE postgres IN DATABASE ... = -1), para que las operaciones de kling db
// (la rotación de la clave, por ejemplo) no acaben en el log. Son ajustes de
// la base (pg_db_role_setting): valen para las conexiones NUEVAS, sin
// reiniciar Postgres, y un fork o un snapshot de la copia los hereda.
//
// El informe (-report) lee el final del log DENTRO de la copia, se queda con
// las sentencias de la base de la aplicación que nombran la tabla, las
// normaliza (literales y números a ?, sin comentarios) y las agrupa: llamadas,
// tiempo total, medio y máximo. Nunca imprime un literal ni un parámetro.
//
// Lo que NO dice: cuánto tardaría en producción (ver statsWarning).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
)

const (
	// observeLogBytes es cuánto del final del log se lee para el informe.
	observeLogBytes = 32 << 20
	// observeMaxStatements acota las sentencias distintas que se agrupan.
	observeMaxStatements = 10000
	observeStmtWidth     = 400
	pgLogFile            = "/var/log/postgresql/pg.log"
)

func cmdObserve(args []string) error {
	fs, host, owner := newFlags("observe")
	off := fs.Bool("off", false, "turn the observation off")
	report := fs.Bool("report", false, "report the observed statements that touch the table")
	table := fs.String("table", "", "table of the report, [schema.]name (default: the table of the slice the copy comes from)")
	limit := fs.Int("limit", 20, "statements in the report, by total time")
	asJSON := fs.Bool("json", false, "JSON report")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *off && *report {
		return usageErr("usage: kling db observe <copy> | -off <copy> | -report <copy> [-table T] [-limit N] [-json]")
	}
	if *limit < 1 {
		return usageErr("-limit must be at least 1")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	switch {
	case *off:
		return a.observeSet(ctx, pos[0], *owner, false)
	case *report:
		rep, err := a.observeReport(ctx, pos[0], *owner, *table, *limit)
		if err != nil {
			return err
		}
		return writeObserveReport(a.stdout, rep, *asJSON)
	default:
		return a.observeSet(ctx, pos[0], *owner, true)
	}
}

// observeSQL activa o quita la observación en la base db (pasó identPattern).
func observeSQL(db string, on bool) string {
	if on {
		return fmt.Sprintf(`ALTER DATABASE %[1]s SET log_min_duration_statement = 0;
ALTER DATABASE %[1]s SET log_parameter_max_length = 0;
ALTER ROLE postgres IN DATABASE %[1]s SET log_min_duration_statement = -1;
SELECT 'ok';
`, db)
	}
	return fmt.Sprintf(`ALTER DATABASE %[1]s RESET log_min_duration_statement;
ALTER DATABASE %[1]s RESET log_parameter_max_length;
ALTER ROLE postgres IN DATABASE %[1]s RESET log_min_duration_statement;
SELECT 'ok';
`, db)
}

func (a *app) observeTarget(ctx context.Context, ref, owner string) (id, name, db, golden string, err error) {
	if err := validOwner(owner); err != nil {
		return "", "", "", "", err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return "", "", "", "", err
	}
	if err := checkReady(mc, owner); err != nil {
		return "", "", "", "", err
	}
	if err := requirePostgres(mc, "observe"); err != nil {
		return "", "", "", "", err
	}
	_, db, err = roleDB(mc.Labels)
	if err != nil {
		return "", "", "", "", err
	}
	return mc.ID, mc.Name, db, mc.Labels[labelGolden], nil
}

func (a *app) observeSet(ctx context.Context, ref, owner string, on bool) error {
	id, name, db, _, err := a.observeTarget(ctx, ref, owner)
	if err != nil {
		return err
	}
	out, err := a.sqlSuper(ctx, id, observeSQL(db, on), "changing the observation settings")
	if err != nil {
		return err
	}
	if lastLine(out) != "ok" {
		return errors.New("changing the observation settings: unexpected answer")
	}
	if !on {
		fmt.Fprintf(a.stdout, "observation off in %s (new connections are no longer logged)\n", name)
		return nil
	}
	fmt.Fprintf(a.stdout, "observing %s: every statement of NEW connections to %s goes to its Postgres log, with its duration (no bound parameters; the superuser is left out)\n", name, db)
	fmt.Fprintf(a.stdout, "  report:  kling db observe -report %s\n  stop:    kling db observe -off %s\n", name, name)
	fmt.Fprintln(a.stdout, "  the log keeps the SQL as sent, literals included: observe copies of masked goldens")
	_, err = fmt.Fprintf(a.stdout, "warning: %s\n", statsWarning)
	return err
}

// observeStmt es una sentencia normalizada y sus números.
type observeStmt struct {
	Statement string  `json:"statement"`
	Calls     int64   `json:"calls"`
	TotalMS   float64 `json:"total_ms"`
	MeanMS    float64 `json:"mean_ms"`
	MaxMS     float64 `json:"max_ms"`
}

// observeReport es el informe de -report.
type observeReport struct {
	Copy       string        `json:"copy"`
	Table      string        `json:"table"`
	Logged     int64         `json:"logged_statements"` // de la base, toquen o no la tabla
	Matching   int64         `json:"matching_statements"`
	Distinct   int           `json:"distinct_statements"`
	Truncated  bool          `json:"truncated,omitempty"`
	Statements []observeStmt `json:"statements"`
	Warning    string        `json:"warning"`
}

func (a *app) observeReport(ctx context.Context, ref, owner, table string, limit int) (*observeReport, error) {
	id, name, db, golden, err := a.observeTarget(ctx, ref, owner)
	if err != nil {
		return nil, err
	}
	if table == "" {
		m, err := readSliceMeta(golden)
		if err != nil {
			return nil, err
		}
		if m == nil || m.Table == "" {
			return nil, fmt.Errorf("%s does not come from a kling db slice golden known to this host: pass -table", name)
		}
		table = m.Table
	}
	schema, tname, ok := splitTable(table)
	if !ok {
		return nil, fmt.Errorf("invalid table %q", dbmaskSafe(table))
	}
	out, err := a.k.Run(ctx, nil, "exec", "-timeout", "60s", id, "--", "tail", "-c", strconv.Itoa(observeLogBytes), pgLogFile)
	if err != nil {
		return nil, fmt.Errorf("reading the postgres log of %s: %w", name, err)
	}
	rep := analyzeObserveLog(string(out), db, schema, tname, limit)
	rep.Copy, rep.Table = name, table
	return rep, nil
}

// splitTable parte [esquema.]nombre. Del slice llega esquema.nombre tal cual
// está en el catálogo (puede no ser un identificador simple).
func splitTable(t string) (schema, name string, ok bool) {
	if t == "" || len(t) > 200 || !nombreSeguro(t) {
		return "", "", false
	}
	if s, n, found := strings.Cut(t, "."); found {
		return s, n, s != "" && n != ""
	}
	return "", t, true
}

// Una entrada del log: log_line_prefix de db-golden.sh ('%m [%p] u=%u d=%d
// h=%h ') y el mensaje de log_min_duration_statement. De la sentencia, solo
// "statement" (protocolo simple) y "execute" (extendido); parse y bind se
// descartan para no contar tres veces la misma.
var (
	logPrefixRe = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)? \S+ \[\d+\] `)
	logStmtRe   = regexp.MustCompile(`(?s)^\S+ \S+ \S+ \[\d+\] u=\S* d=(\S*) h=\S* LOG:  duration: ([0-9.]+) ms  (?:statement|execute [^:]*): (.*)$`)
)

// analyzeObserveLog agrupa las sentencias de la base db que nombran la tabla.
func analyzeObserveLog(log, db, schema, table string, limit int) *observeReport {
	rep := &observeReport{Statements: []observeStmt{}, Warning: statsWarning}
	byText := map[string]*observeStmt{}
	var entry strings.Builder
	flush := func() {
		if entry.Len() == 0 {
			return
		}
		m := logStmtRe.FindStringSubmatch(entry.String())
		entry.Reset()
		if m == nil || m[1] != db {
			return
		}
		rep.Logged++
		norm := normalizeSQL(m[3])
		if !touchesTable(norm, schema, table) {
			return
		}
		ms, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return
		}
		rep.Matching++
		st := byText[norm]
		if st == nil {
			if len(byText) >= observeMaxStatements {
				rep.Truncated = true
				return
			}
			st = &observeStmt{Statement: norm}
			byText[norm] = st
		}
		st.Calls++
		st.TotalMS += ms
		st.MaxMS = max(st.MaxMS, ms)
	}
	for _, line := range strings.Split(log, "\n") {
		if logPrefixRe.MatchString(line) {
			flush()
			entry.WriteString(line)
			continue
		}
		// Continuación de una sentencia de varias líneas.
		if entry.Len() > 0 {
			entry.WriteByte('\n')
			entry.WriteString(line)
		}
	}
	flush()
	for _, st := range byText {
		st.MeanMS = st.TotalMS / float64(st.Calls)
		rep.Statements = append(rep.Statements, *st)
	}
	rep.Distinct = len(rep.Statements)
	sort.Slice(rep.Statements, func(i, j int) bool {
		a, b := rep.Statements[i], rep.Statements[j]
		if a.TotalMS != b.TotalMS {
			return a.TotalMS > b.TotalMS
		}
		return a.Statement < b.Statement
	})
	if len(rep.Statements) > limit {
		rep.Statements = rep.Statements[:limit]
	}
	for i := range rep.Statements {
		rep.Statements[i].Statement = clip(rep.Statements[i].Statement, observeStmtWidth)
	}
	return rep
}

// normalizeSQL quita de una sentencia lo que puede llevar datos: literales de
// cadena (también E'...' y $tag$...$tag$) y números pasan a ?, los comentarios se van,
// los espacios se juntan y las listas de ? se resumen. Los identificadores
// entre comillas dobles se quedan. Nada de control sale de aquí.
func normalizeSQL(s string) string {
	var b strings.Builder
	space := false
	emit := func(x string) {
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteString(x)
	}
	prevIdent := func() bool {
		if b.Len() == 0 || space {
			return false
		}
		c := b.String()[b.Len()-1]
		return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			space = true
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// Comentarios anidados, como en Postgres.
			depth := 0
			for i < len(s) {
				if s[i] == '/' && i+1 < len(s) && s[i+1] == '*' {
					depth++
					i += 2
				} else if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
			space = true
		case c == '\'' || (c == 'E' || c == 'e') && i+1 < len(s) && s[i+1] == '\'' && !prevIdent():
			esc := c != '\''
			if esc {
				i++
			}
			i++
			for i < len(s) {
				if esc && s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			emit("?")
		case c == '$' && !prevIdent():
			// $1 (parámetro) o $tag$...$tag$ (cadena).
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9') {
				j++
			}
			if j > i+1 {
				emit(s[i:j])
				i = j
				continue
			}
			j = i + 1
			for j < len(s) && (s[j] == '_' || unicode.IsLetter(rune(s[j])) || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			if j < len(s) && s[j] == '$' {
				tag := s[i : j+1]
				if k := strings.Index(s[j+1:], tag); k >= 0 {
					i = j + 1 + k + len(tag)
				} else {
					i = len(s)
				}
				emit("?")
				continue
			}
			emit("$")
			i++
		case c >= '0' && c <= '9' && !prevIdent():
			for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == 'e' || s[i] == 'E' || s[i] == '_' ||
				(s[i] == '+' || s[i] == '-') && (s[i-1] == 'e' || s[i-1] == 'E')) {
				i++
			}
			emit("?")
		case c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			emit(s[i:j])
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			emit(s[i : i+1])
			i++
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimSuffix(out, ";")
	out = listRe.ReplaceAllString(out, "?, ...")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(out))
}

var listRe = regexp.MustCompile(`\?(?: ?, ?\?)+`)

// touchesTable dice si la sentencia normalizada nombra la tabla: como
// identificador (sin comillas, sin distinguir mayúsculas; o entre comillas,
// exacto) y, si lleva esquema delante, que sea el de la tabla.
func touchesTable(norm, schema, table string) bool {
	toks := identTokens(norm)
	for i, t := range toks {
		if !identEq(t, table) {
			continue
		}
		if i >= 2 && toks[i-1] == "." {
			if schema == "" || identEq(toks[i-2], schema) {
				return true
			}
			continue
		}
		return true
	}
	return false
}

// identTokens parte en identificadores (con sus comillas) y en "." sueltos.
func identTokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			out = append(out, s[i:j])
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '$' || s[j] >= '0' && s[j] <= '9' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= 0x80) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		case c == '.':
			out = append(out, ".")
			i++
		case c == ' ':
			i++
		default:
			out = append(out, string(c))
			i++
		}
	}
	return out
}

// identEq compara un identificador de la sentencia con un nombre del
// catálogo: con comillas, exacto; sin ellas, Postgres lo pasa a minúsculas.
func identEq(tok, name string) bool {
	if strings.HasPrefix(tok, `"`) {
		return len(tok) >= 2 && strings.ReplaceAll(tok[1:len(tok)-1], `""`, `"`) == name
	}
	return strings.ToLower(tok) == name
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func writeObserveReport(w io.Writer, rep *observeReport, asJSON bool) error {
	if asJSON {
		e := json.NewEncoder(w)
		e.SetIndent("", "  ")
		return e.Encode(rep)
	}
	fmt.Fprintf(w, "%s: %d statement(s) touching %s (%d distinct) out of %d logged\n", rep.Copy, rep.Matching, rep.Table, rep.Distinct, rep.Logged)
	if rep.Logged == 0 {
		fmt.Fprintf(w, "nothing logged yet: is the observation on? (kling db observe %s; only new connections are logged)\n", rep.Copy)
	}
	if len(rep.Statements) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CALLS\tTOTAL_MS\tMEAN_MS\tMAX_MS\tSTATEMENT")
		for _, s := range rep.Statements {
			fmt.Fprintf(tw, "%d\t%.3f\t%.3f\t%.3f\t%s\n", s.Calls, s.TotalMS, s.MeanMS, s.MaxMS, s.Statement)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if rep.Truncated {
		fmt.Fprintf(w, "note: more than %d distinct statements; the rest were not grouped\n", observeMaxStatements)
	}
	_, err := fmt.Fprintf(w, "warning: %s\n", rep.Warning)
	return err
}
