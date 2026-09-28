//go:build pglab

package credproxy

// Prueba de laboratorio del proxy de Postgres contra un PostgreSQL DE VERDAD.
// No corre con `go test` normal: necesita la etiqueta pglab y un servidor.
//
// Compilar (desde el Mac o donde sea) y llevar el binario al laboratorio:
//
//	GOOS=linux go test -c -tags pglab -o credproxy-pglab ./pkg/credproxy/
//
// Ejecutar allí:
//
//	KLING_PGLAB_ADDR=127.0.0.1:5432 \
//	KLING_PGLAB_DOMAIN=pg.kindling.test \
//	KLING_PGLAB_USER=kling \
//	KLING_PGLAB_PASSWORD='la-clave-del-rol' \
//	KLING_PGLAB_DATABASE=kling \
//	KLING_PGLAB_CA=/etc/postgresql/pglab-ca.pem \
//	./credproxy-pglab -test.run PGLab -test.v
//
// Las variables:
//
//	KLING_PGLAB_ADDR      host:puerto al que se conecta de verdad (el dialer
//	                      inyectado; el proxy de producción no llegaría a
//	                      127.0.0.1, y aquí no se relaja nada más).
//	KLING_PGLAB_DOMAIN    el nombre del certificado del servidor: es el Domain
//	                      de la credencial y el ServerName contra el que se
//	                      verifica el TLS. Tiene que estar en el SAN del
//	                      certificado y ser un nombre (no una IP).
//	KLING_PGLAB_USER      rol con contraseña SCRAM (password_encryption =
//	                      scram-sha-256) y permiso de CONNECT en la base.
//	KLING_PGLAB_PASSWORD  su contraseña (ASCII imprimible).
//	KLING_PGLAB_DATABASE  base de datos (opcional; por defecto, el rol).
//	KLING_PGLAB_CA        PEM del certificado autofirmado del servidor (o de
//	                      su CA).
//
// El servidor tiene que tener ssl = on con ese certificado, y en pg_hba.conf
// una línea "hostssl <base> <rol> 127.0.0.1/32 scram-sha-256". Con un
// certificado RSA o ECDSA el servidor ofrece SCRAM-SHA-256-PLUS y la prueba
// comprueba que el proxy lo usa.
//
// Qué cubre: SCRAM (y -PLUS si se ofrece) con la clave real, una consulta de
// ida y vuelta, un marcador equivocado (sin conectar al servidor) y un
// CancelRequest que corta un pg_sleep.

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func entornoLab(t *testing.T) (addr string, cred Credential) {
	t.Helper()
	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" && k != "KLING_PGLAB_DATABASE" {
			t.Fatalf("falta %s (ver la cabecera de postgres_lab_test.go)", k)
		}
		return v
	}
	addr = get("KLING_PGLAB_ADDR")
	ca, err := os.ReadFile(get("KLING_PGLAB_CA"))
	if err != nil {
		t.Fatal(err)
	}
	cred = Credential{Env: "PGPASSWORD", Domain: get("KLING_PGLAB_DOMAIN"), Placeholder: pgMarca,
		Secret: get("KLING_PGLAB_PASSWORD"), Kind: KindPostgres, User: get("KLING_PGLAB_USER"),
		Database: get("KLING_PGLAB_DATABASE"), CAPEM: string(ca)}
	return addr, cred
}

func TestPGLab(t *testing.T) {
	addr, cred := entornoLab(t)
	db := cred.Database
	if db == "" {
		db = cred.User
	}
	var dials atomic.Int32
	e := &entornoPG{audit: t.TempDir() + "/" + AuditFile}
	e.p = New(Options{AuditPath: e.audit, Logf: t.Logf, DialPG: func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}})
	if _, err := e.p.SetCredentials([]Credential{cred}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.ps = NewPGServer(e.p)
	e.addr = ln.Addr().String()
	go e.ps.Serve(ln)
	t.Cleanup(func() { e.ps.Close(); e.p.Close() })
	pares := []string{"user", cred.User, "database", db, "application_name", "kling-pglab"}

	t.Run("consulta", func(t *testing.T) {
		k := conectarPG(t, e.addr)
		tipo, msg := k.login(pgMarca, pares...)
		if tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
			code, crudo := k.esperarError(tipo, msg)
			t.Fatalf("sin AuthenticationOk: %s %q", code, crudo)
		}
		k.hastaListo()
		tipo, msg = k.consulta("SELECT 'kling-' || 41+1")
		if tipo != 'T' {
			t.Fatalf("esperaba RowDescription, llegó %q %q", tipo, msg)
		}
		tipo, msg = k.leer()
		if tipo != 'D' || !strings.Contains(string(msg), "kling-42") {
			t.Fatalf("fila %q %q", tipo, msg)
		}
		for tipo != 'Z' {
			tipo, _ = k.leer()
		}
		k.c.Write(mensajePG('X', nil))
		k.c.Close()
	})

	t.Run("marcador malo", func(t *testing.T) {
		antes := dials.Load()
		k := conectarPG(t, e.addr)
		tipo, msg := k.login(PlaceholderPrefix+"otro", pares...)
		if code, _ := k.esperarError(tipo, msg); code != "28P01" {
			t.Errorf("SQLSTATE %s", code)
		}
		if dials.Load() != antes {
			t.Error("el proxy conectó al servidor con un marcador malo")
		}
	})

	t.Run("cancelación", func(t *testing.T) {
		k := conectarPG(t, e.addr)
		if tipo, msg := k.login(pgMarca, pares...); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
			t.Fatalf("sin AuthenticationOk: %q", msg)
		}
		pid, clave := k.hastaListo()
		inicio := time.Now()
		k.c.Write(mensajePG('Q', []byte("SELECT pg_sleep(30)\x00")))
		time.Sleep(500 * time.Millisecond)
		c := conectarPG(t, e.addr)
		m := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 16}, pgCancelRequest)
		m = binary.BigEndian.AppendUint32(m, pid)
		m = binary.BigEndian.AppendUint32(m, clave)
		c.c.Write(m)
		for {
			tipo, msg := k.leer()
			if tipo == 'E' {
				if code := sqlstate(msg); code != "57014" {
					t.Fatalf("error %s, quería 57014 (query_canceled)", code)
				}
				break
			}
			if tipo == 'Z' {
				t.Fatal("pg_sleep terminó sin cancelarse")
			}
		}
		if d := time.Since(inicio); d > 10*time.Second {
			t.Errorf("la cancelación tardó %v", d)
		}
		k.c.Close()
	})

	recs, crudo := e.registro(t)
	var metodo string
	for _, r := range recs {
		t.Logf("registro: %+v", r)
		if r.Method == "" && r.Reason == "" {
			metodo = r.Auth
		}
	}
	t.Logf("autenticación con el servidor: %s", metodo)
	if metodo != AuthSCRAM && metodo != AuthSCRAMPlus {
		t.Errorf("método %q: el servidor debería pedir SCRAM (password_encryption = scram-sha-256)", metodo)
	}
	for _, prohibido := range []string{cred.Secret, pgMarca} {
		if strings.Contains(crudo, prohibido) {
			t.Errorf("el registro contiene la clave o el marcador")
		}
	}
}
