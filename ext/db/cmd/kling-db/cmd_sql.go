package main

// `kling db sql <copia> "SELECT ..."`: una consulta desde el host sin psql.
//
// El camino de siempre era `connect -psql` (necesita el psql del host, que
// en un Mac no suele estar) o `kling exec <copia> -- su postgres -c psql...`
// (como superusuario y con las comillas de dos shells). Este habla el
// protocolo con pgmini, como el rol de la aplicación (o -role) y con su clave
// del host, que no sale del proceso. -json da columnas, filas (null = NULL) y
// la etiqueta de cada sentencia, para un programa o un agente.
//
// Lo que se guarda tiene tope (-max-rows y sqlMaxBytes): un SELECT sin LIMIT
// sobre una tabla grande no se come la memoria del host. Lo que pasa del tope
// se lee y se tira; el resultado sale marcado como truncado.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/pgmini"
	"github.com/juan52878911/kindling/pkg/plugin"
)

func cmdSQL(args []string) error {
	fs, host, owner := newFlags("sql")
	role := fs.String("role", "", "connect as this role (one made with kling db role); default: the application role")
	file := fs.String("f", "", "read the SQL from this file (- = stdin) instead of the argument")
	asJSON := fs.Bool("json", false, "print {results: [{command, columns, rows}]}; NULL is null")
	timeout := fs.Duration("timeout", 30*time.Second, "for connecting and for the whole query")
	maxRows := fs.Int("max-rows", defaultSQLMaxRows, "keep at most this many rows in all (the rest is read and dropped, and the result says truncated); 0 = no row limit")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	usage := `usage: kling db sql <copy> "SQL" [-role R] [-json] [-max-rows N] [-timeout 30s]   or   kling db sql <copy> -f FILE|-`
	if len(pos) < 1 || len(pos) > 2 || (len(pos) == 2) == (*file != "") {
		return usageErr("%s", usage)
	}
	if *maxRows < 0 {
		return usageErr("-max-rows must be 0 or more; %s", usage)
	}
	sql := ""
	if len(pos) == 2 {
		sql = pos[1]
	} else {
		var b []byte
		if *file == "-" {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<20))
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			return err
		}
		sql = string(b)
	}
	if strings.TrimSpace(sql) == "" {
		return usageErr("empty SQL; %s", usage)
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	return a.sql(ctx, pos[0], *owner, *role, sql, *asJSON, *maxRows, *timeout)
}

const (
	// defaultSQLMaxRows es el -max-rows por defecto.
	defaultSQLMaxRows = 10000
	// sqlMaxBytes es el tope de bytes de valores que se guardan, siempre.
	sqlMaxBytes = 64 << 20
)

// sqlResult es un resultado en -json.
type sqlResult struct {
	Command   string      `json:"command"`
	Columns   []string    `json:"columns"`
	Rows      [][]*string `json:"rows"`
	Truncated bool        `json:"truncated,omitempty"`
}

// sqlExecer es lo que usa printSQL de una conexión (pgmini.Conn).
type sqlExecer interface {
	ExecLimited(ctx context.Context, sql string, lim pgmini.Limits) ([]pgmini.Result, error)
}

func (a *app) sql(ctx context.Context, ref, owner, extraRole, sql string, asJSON bool, maxRows int, timeout time.Duration) error {
	if err := validOwner(owner); err != nil {
		return err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return fmt.Errorf("%w\n  your copies: kling db ls   ·   a new one: kling db up -name %s", err, ref)
	}
	if err := checkReady(mc, owner); err != nil {
		return err
	}
	a.warnInterruptedRotate(mc)
	if engine := engineOf(mc.Labels); engine != enginePostgres {
		return fmt.Errorf("%s is a %s copy: kling db sql only speaks Postgres; use kling db connect %s -%s", mc.Name, engine, mc.Name, clientMode(engine))
	}
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return err
	}
	readPW := func() (string, error) { return dbstate.ReadPassword(mc.ID) }
	if extraRole != "" {
		if err := validRoleName(extraRole, role); err != nil {
			return err
		}
		role = extraRole
		readPW = func() (string, error) {
			pw, err := dbstate.ReadRolePassword(mc.ID, role)
			if errors.Is(err, dbstate.ErrNoPassword) {
				return "", fmt.Errorf("this host has no password for role %s of %s (kling db role %s -ro -name %s creates a read-only one)", role, mc.Name, mc.Name, role)
			}
			return pw, err
		}
	}
	pw, err := readPW()
	if err != nil {
		return err
	}
	h, port, err := hostAddr(mc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := pgmini.Dial(ctx, pgmini.Config{
		Addr: net.JoinHostPort(h, strconv.Itoa(port)), User: role, Password: pw, Database: db,
		Timeout: timeout, NoCleartext: true,
	})
	if err != nil {
		return fmt.Errorf("connecting to %s as %s: %w (kling db doctor %s says why)", mc.Name, role, err, mc.Name)
	}
	defer c.Close()
	return a.printSQL(ctx, c, sql, asJSON, maxRows)
}

// printSQL ejecuta sql con los topes y escribe el resultado, en tabla o -json.
func (a *app) printSQL(ctx context.Context, c sqlExecer, sql string, asJSON bool, maxRows int) error {
	res, qerr := c.ExecLimited(ctx, sql, pgmini.Limits{MaxRows: maxRows, MaxBytes: sqlMaxBytes})
	if asJSON {
		out := struct {
			Results []sqlResult `json:"results"`
			Error   string      `json:"error,omitempty"`
		}{Results: []sqlResult{}}
		for _, r := range res {
			out.Results = append(out.Results, sqlResult{Command: r.Command, Columns: orEmpty(r.Columns), Rows: orEmptyRows(r.Rows), Truncated: r.Truncated})
		}
		if qerr != nil {
			out.Error = qerr.Error()
		}
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		if qerr != nil {
			return &plugin.ExitError{Code: 1, Err: qerr}
		}
		return nil
	}
	for i, r := range res {
		if i > 0 {
			fmt.Fprintln(a.stdout)
		}
		printTable(a.stdout, r)
	}
	return qerr
}

// printTable escribe un resultado como psql: cabecera, filas alineadas y
// la etiqueta; NULL sale vacío. Celdas, columnas y etiqueta vienen de la base:
// pasan por celda() para que ningún carácter de control llegue a la terminal.
func printTable(w io.Writer, r pgmini.Result) {
	if len(r.Columns) == 0 {
		fmt.Fprintln(w, celda(r.Command))
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	cols := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		cols[i] = celda(c)
	}
	fmt.Fprintln(tw, strings.Join(cols, "\t"))
	for _, row := range r.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			if v != nil {
				vals[i] = celda(*v)
			}
		}
		fmt.Fprintln(tw, strings.Join(vals, "\t"))
	}
	tw.Flush()
	n := len(r.Rows)
	if n == 1 {
		fmt.Fprintln(w, "(1 row)")
	} else {
		fmt.Fprintf(w, "(%d rows)\n", n)
	}
	if r.Truncated {
		fmt.Fprintf(w, "(truncated: the server sent %s; raise -max-rows or add a LIMIT)\n", celda(r.Command))
	}
}

// celda deja un valor en una línea (tabuladores y saltos como \t y \n) y sin
// caracteres de control.
func celda(s string) string {
	return printable(strings.NewReplacer("\t", `\t`, "\n", `\n`, "\r", `\r`).Replace(s))
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyRows(r [][]*string) [][]*string {
	if r == nil {
		return [][]*string{}
	}
	return r
}
