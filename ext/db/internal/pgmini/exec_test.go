package pgmini

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func rowDesc(cols ...string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(cols)))
	for _, c := range cols {
		b = append(b, c...)
		b = append(b, 0)
		b = append(b, make([]byte, 18)...)
	}
	return msg('T', b)
}

func dataRow(vals ...*string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(vals)))
	for _, v := range vals {
		if v == nil {
			b = binary.BigEndian.AppendUint32(b, 0xFFFFFFFF)
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(*v)))
		b = append(b, *v...)
	}
	return msg('D', b)
}

func ptr(s string) *string { return &s }

// conPipe es una Conn ya autenticada sobre un net.Pipe: el otro extremo lee
// la consulta y contesta resp tal cual.
func conPipe(t *testing.T, resp ...[]byte) *Conn {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	go func() {
		r := bufio.NewReader(c2)
		var h [5]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(h[1:]))-4)
		for _, m := range resp {
			c2.Write(m)
		}
	}()
	return &Conn{nc: c1, r: bufio.NewReader(c1), timeout: 5 * time.Second, stop: func() bool { return true }}
}

func TestExecColumnasNullYEtiquetas(t *testing.T) {
	c := conPipe(t,
		rowDesc("id", "name"),
		dataRow(ptr("1"), ptr("ana")),
		dataRow(ptr("2"), nil),
		msg('C', []byte("SELECT 2\x00")),
		msg('C', []byte("INSERT 0 1\x00")),
		msg('Z', []byte("I")),
	)
	res, err := c.Exec(context.Background(), "select ...; insert ...")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	r := res[0]
	if r.Command != "SELECT 2" || len(r.Columns) != 2 || r.Columns[1] != "name" || len(r.Rows) != 2 {
		t.Fatalf("first result: %+v", r)
	}
	if *r.Rows[0][1] != "ana" || r.Rows[1][1] != nil {
		t.Fatalf("rows: %q %v", *r.Rows[0][1], r.Rows[1][1])
	}
	if res[1].Command != "INSERT 0 1" || res[1].Columns != nil || res[1].Rows != nil {
		t.Fatalf("second result: %+v", res[1])
	}
}

func TestExecErrorDevuelveLoAnterior(t *testing.T) {
	c := conPipe(t,
		msg('C', []byte("CREATE TABLE\x00")),
		msg('E', []byte("SERROR\x00C42P01\x00Mrelation \"x\" does not exist\x00\x00")),
		msg('Z', []byte("I")),
	)
	res, err := c.Exec(context.Background(), "create table t(); select * from x")
	if err == nil {
		t.Fatal("no error")
	}
	if pe, ok := err.(*Error); !ok || pe.Code != "42P01" {
		t.Fatalf("error %v", err)
	}
	if len(res) != 1 || res[0].Command != "CREATE TABLE" {
		t.Fatalf("results before the error: %+v", res)
	}
}

func TestRowDescriptionTruncada(t *testing.T) {
	b := binary.BigEndian.AppendUint16(nil, 1)
	b = append(b, "id\x00"...)
	if _, err := parseRowDescription(b); err == nil {
		t.Fatal("a RowDescription without the 18 bytes was accepted")
	}
}
