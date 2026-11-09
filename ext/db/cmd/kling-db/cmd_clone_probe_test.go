package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

// Un builder que miente sobre el rol no recibe la credencial: la comprobación
// la hace kling-db desde el host antes de arrancar la máquina, y al invitado
// no se le pregunta nada.
func TestCloneBuilderQueMienteNoRecibeCredencial(t *testing.T) {
	for name, probe := range map[string]string{
		"escribe":      "probe|f|3|160004\n",
		"superusuario": "probe|t|0|160004\n",
	} {
		t.Run(name, func(t *testing.T) {
			ct := newCloneTest(t)
			ct.f.probe = probe
			if _, err := ct.c.run(context.Background(), ct.opts(t, cloneRules)); err == nil {
				t.Fatal("a role that can write was accepted")
			}
			if len(ct.f.creds) != 0 {
				t.Fatalf("the credential reached the proxy: %d", len(ct.f.creds))
			}
			if ct.f.guestAsked {
				t.Fatal("the builder was asked about the role")
			}
			for _, c := range ct.f.calls {
				if c.args[0] == "run" {
					t.Fatalf("a builder was started before the check: %v", c.args)
				}
			}
			if ct.f.probeSaw != clonePW {
				t.Fatalf("the host probe did not get the source password")
			}
			ct.sinRastro(t)
		})
	}
}

// Si el host no llega al origen, no se arranca nada ni se entrega nada.
func TestCloneSondeoFallaNoArranca(t *testing.T) {
	ct := newCloneTest(t)
	ct.f.probeErr = errors.New("dial tcp: connection refused")
	_, err := ct.c.run(context.Background(), ct.opts(t, cloneRules))
	if err == nil || !strings.Contains(err.Error(), "db.example.com:6543") {
		t.Fatalf("err = %v", err)
	}
	if len(ct.f.creds) != 0 || len(ct.f.calls) != 1 { // solo el template inspect
		t.Fatalf("creds %d, calls %v", len(ct.f.creds), ct.f.calls)
	}
	ct.sinRastro(t)
}

// Sin sonda (un cloner a medias) no se construye.
func TestCloneSinSondaNoConstruye(t *testing.T) {
	ct := newCloneTest(t)
	ct.c.probe = nil
	if _, err := ct.c.run(context.Background(), ct.opts(t, cloneRules)); err == nil {
		t.Fatal("built without checking the role")
	}
	if len(ct.f.creds) != 0 {
		t.Fatal("the credential reached the proxy")
	}
}

// hostProbe habla de verdad con un servidor (falso, con trust) y devuelve la
// línea que checkProbe lee.
func TestHostProbeContraServidor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotSQL := make(chan string, 1)
	go fakePGProbeServer(ln, gotSQL)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	o := cloneOpts{source: &cloneSource{host: host, port: port, user: "reader", db: "shop", tls: "disable"}, password: clonePW}
	out, err := hostProbe(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "probe|f|2|160004\n" {
		t.Fatalf("out = %q", out)
	}
	if sql := <-gotSQL; !strings.Contains(sql, "rolsuper") || !strings.Contains(sql, "has_table_privilege") {
		t.Fatalf("sql = %q", sql)
	}
	if err := checkProbe(out, false); err == nil || !strings.Contains(err.Error(), "write in 2 table") {
		t.Fatalf("checkProbe = %v", err)
	}
}

// fakePGProbeServer acepta una conexión, la autentica con trust y contesta a
// una consulta con una fila de sonda.
func fakePGProbeServer(ln net.Listener, gotSQL chan<- string) {
	nc, err := ln.Accept()
	if err != nil {
		return
	}
	defer nc.Close()
	r := bufio.NewReader(nc)
	var n uint32
	if binary.Read(r, binary.BigEndian, &n) != nil || n < 8 {
		return
	}
	if _, err := io.CopyN(io.Discard, r, int64(n-4)); err != nil {
		return
	}
	send := func(typ byte, body []byte) {
		b := append([]byte{typ}, binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))...)
		nc.Write(append(b, body...))
	}
	send('R', binary.BigEndian.AppendUint32(nil, 0))
	send('Z', []byte{'I'})
	typ, err := r.ReadByte()
	if err != nil || typ != 'Q' {
		return
	}
	if binary.Read(r, binary.BigEndian, &n) != nil || n < 4 {
		return
	}
	q := make([]byte, n-4)
	if _, err := io.ReadFull(r, q); err != nil {
		return
	}
	gotSQL <- strings.TrimRight(string(q), "\x00")
	row := binary.BigEndian.AppendUint16(nil, 4)
	for _, v := range []string{"probe", "f", "2", "160004"} {
		row = binary.BigEndian.AppendUint32(row, uint32(len(v)))
		row = append(row, v...)
	}
	send('D', row)
	send('C', []byte("SELECT 1\x00"))
	send('Z', []byte{'I'})
}
