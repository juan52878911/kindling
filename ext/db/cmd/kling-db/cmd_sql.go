package main

// `kling db sql <copia> "SELECT ..."`: una consulta desde el host sin psql.
//
// El camino de siempre era `connect -psql` (necesita el psql del host, que
// en un Mac no suele estar) o `kling exec <copia> -- su postgres -c psql...`
// (como superusuario y con las comillas de dos shells). Este habla el
// protocolo con pgmini, como el rol de la aplicación (o -role) y con su clave
// del host, que no sale del proceso. -json da columnas, filas (null = NULL) y
// la etiqueta de cada sentencia, para un programa o un agente.

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
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	usage := `usage: kling db sql <copy> "SQL" [-role R] [-json] [-timeout 30s]   or   kling db sql <copy> -f FILE|-`
	if len(pos) < 1 || len(pos) > 2 || (len(pos) == 2) == (*file != "") {
		return usageErr("%s", usage)
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
	return a.sql(ctx, pos[0], *owner, *role, sql, *asJSON, *timeout)
}

// sqlResult es un resultado en -json.
type sqlResult struct {
	Command string      `json:"command"`
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
}

func (a *app) sql(ctx context.Context, ref, owner, extraRole, sql string, asJSON bool, timeout time.Duration) error {
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
	res, qerr := c.Exec(ctx, sql)
	if asJSON {
		out := struct {
			Results []sqlResult `json:"results"`
			Error   string      `json:"error,omitempty"`
		}{Results: []sqlResult{}}
		for _, r := range res {
			out.Results = append(out.Results, sqlResult{Command: r.Command, Columns: orEmpty(r.Columns), Rows: orEmptyRows(r.Rows)})
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
// la etiqueta; NULL sale vacío.
func printTable(w io.Writer, r pgmini.Result) {
	if len(r.Columns) == 0 {
		fmt.Fprintln(w, r.Command)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(r.Columns, "\t"))
	for _, row := range r.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			if v != nil {
				vals[i] = strings.NewReplacer("\t", `\t`, "\n", `\n`).Replace(*v)
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
