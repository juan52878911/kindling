package pgmini

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
)

// Result es un resultado de Exec: las columnas (de RowDescription), las filas
// (nil = NULL) y la etiqueta del comando ("SELECT 2", "INSERT 0 1").
type Result struct {
	Columns []string
	Rows    [][]*string
	Command string
}

// Exec es Query con lo que Query tira: los nombres de las columnas, los NULL y
// la etiqueta de cada sentencia, un Result por sentencia. Con un error del
// servidor devuelve los resultados de las sentencias anteriores y el error, y
// deja la conexión usable (lee hasta ReadyForQuery).
func (c *Conn) Exec(ctx context.Context, sql string) ([]Result, error) {
	c.plazo(ctx)
	if err := c.escribir('Q', append([]byte(sql), 0)); err != nil {
		return nil, err
	}
	var res []Result
	var cur *Result
	var qerr error
	for {
		t, body, err := c.leer()
		if err != nil {
			return res, err
		}
		switch t {
		case 'T':
			cols, err := parseRowDescription(body)
			if err != nil {
				return res, err
			}
			cur = &Result{Columns: cols}
		case 'D':
			if cur == nil {
				cur = &Result{}
			}
			row, err := parseDataRowNull(body)
			if err != nil {
				return res, err
			}
			cur.Rows = append(cur.Rows, row)
		case 'C':
			if cur == nil {
				cur = &Result{}
			}
			cur.Command = strings.TrimRight(string(body), "\x00")
			res = append(res, *cur)
			cur = nil
		case 'E':
			if qerr == nil {
				qerr = parseError(body)
			}
		case 'Z':
			return res, qerr
		}
	}
}

// parseRowDescription lee los nombres de las columnas de un RowDescription:
// por campo, el nombre (cstring) y 18 bytes de tabla, columna, tipo y formato.
func parseRowDescription(b []byte) ([]string, error) {
	if len(b) < 2 {
		return nil, errors.New("postgres: short RowDescription")
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	cols := make([]string, 0, n)
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(b, 0)
		if j < 0 || len(b) < j+1+18 {
			return nil, errors.New("postgres: truncated RowDescription")
		}
		cols = append(cols, string(b[:j]))
		b = b[j+1+18:]
	}
	return cols, nil
}

// parseDataRowNull es parseDataRow con los NULL como nil.
func parseDataRowNull(b []byte) ([]*string, error) {
	if len(b) < 2 {
		return nil, errors.New("postgres: short DataRow")
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	row := make([]*string, 0, n)
	for i := 0; i < n; i++ {
		if len(b) < 4 {
			return nil, errors.New("postgres: truncated DataRow")
		}
		l := int32(binary.BigEndian.Uint32(b))
		b = b[4:]
		if l < 0 {
			row = append(row, nil)
			continue
		}
		if int(l) > len(b) {
			return nil, errors.New("postgres: truncated DataRow value")
		}
		v := string(b[:l])
		row = append(row, &v)
		b = b[l:]
	}
	return row, nil
}
