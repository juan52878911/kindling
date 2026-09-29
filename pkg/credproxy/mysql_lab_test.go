//go:build mylab

package credproxy

// Prueba de laboratorio del proxy de MySQL contra un MySQL o MariaDB DE
// VERDAD. No corre con `go test` normal: necesita la etiqueta mylab y un
// servidor.
//
//	GOOS=linux go test -c -tags mylab -o credproxy-mylab ./pkg/credproxy/
//
//	KLING_MYLAB_ADDR=127.0.0.1:3306 \
//	KLING_MYLAB_DOMAIN=mysql.kindling.test \
//	KLING_MYLAB_USER=kling \
//	KLING_MYLAB_PASSWORD='la-clave' \
//	KLING_MYLAB_DATABASE=kling \
//	KLING_MYLAB_CA=/etc/mysql/mylab-ca.pem \
//	./credproxy-mylab -test.run MyLab -test.v
//
// Las variables son las de postgres_lab_test.go con MYLAB en vez de PGLAB:
// ADDR (a dónde marca de verdad el dialer inyectado), DOMAIN (el nombre del
// certificado), USER, PASSWORD, DATABASE, CA (PEM), y opcionales UPSTREAM
// (host:puerto como -upstream, con el dialer de producción), TLS=disable (sin
// TLS: solo native o la ruta rápida de caching_sha2) y SERVERNAME.
//
// Un Docker sin TLS, para lo que no manda la clave:
//
//	docker run -d --name mylab -p 127.0.0.1:53306:3306 -e MARIADB_USER=kling \
//	  -e MARIADB_PASSWORD='la-clave' -e MARIADB_DATABASE=kling \
//	  -e MARIADB_RANDOM_ROOT_PASSWORD=1 mariadb:11
//	KLING_MYLAB_UPSTREAM=127.0.0.1:53306 KLING_MYLAB_TLS=disable \
//	KLING_MYLAB_DOMAIN=mysql.kindling.test KLING_MYLAB_USER=kling \
//	KLING_MYLAB_DATABASE=kling KLING_MYLAB_PASSWORD='la-clave' \
//	./credproxy-mylab -test.run MyLab -test.v
//
// (Con MySQL 8 y caching_sha2_password sin TLS, la primera conexión tras
// arrancar el servidor pide la autenticación completa y el proxy la rechaza:
// es lo esperado. Con TLS la hace dentro del TLS.)
//
// Qué cubre: la autenticación con la clave real y una consulta de ida y
// vuelta, un marcador equivocado (sin conectar al servidor), que KILL QUERY
// con el id del saludo (0) NO cancela nada y que con el id real, sacado de
// CONNECTION_ID() y por el proxy, sí.

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func entornoMyLab(t *testing.T) (addr string, cred Credential) {
	t.Helper()
	upstream := os.Getenv("KLING_MYLAB_UPSTREAM")
	sinTLS := os.Getenv("KLING_MYLAB_TLS") == UpstreamTLSDisable
	opcional := map[string]bool{"KLING_MYLAB_ADDR": upstream != "", "KLING_MYLAB_CA": sinTLS}
	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" && !opcional[k] {
			t.Fatalf("falta %s (ver la cabecera de mysql_lab_test.go)", k)
		}
		return v
	}
	addr = get("KLING_MYLAB_ADDR")
	var ca []byte
	if f := get("KLING_MYLAB_CA"); f != "" && !sinTLS {
		var err error
		if ca, err = os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	cred = Credential{Env: "MYSQL_PWD", Domain: get("KLING_MYLAB_DOMAIN"), Placeholder: myMarca,
		Secret: get("KLING_MYLAB_PASSWORD"), Kind: KindMySQL, User: get("KLING_MYLAB_USER"),
		Database: get("KLING_MYLAB_DATABASE"), CAPEM: string(ca),
		Upstream: upstream, TLSServerName: os.Getenv("KLING_MYLAB_SERVERNAME")}
	if sinTLS {
		cred.UpstreamTLS = UpstreamTLSDisable
	}
	return addr, cred
}

// resultado lee un conjunto de resultados de una columna y devuelve las filas
// (o el código de un ERR).
func (k *clienteMy) resultado() ([]string, uint16) {
	k.t.Helper()
	p := k.leer()
	switch p[0] {
	case 0xff:
		code, _, _ := esperarErr(k.t, p)
		return nil, code
	case 0x00:
		return nil, 0
	}
	var filas []string
	columnas := true
	for {
		p = k.leer()
		if p[0] == 0xff {
			code, _, _ := esperarErr(k.t, p)
			return filas, code
		}
		if p[0] == 0xfe && len(p) < 0xffffff {
			if columnas {
				// EOF tras las definiciones (sin DEPRECATE_EOF); con él,
				// no hay y la primera fila llega directa.
				columnas = false
				continue
			}
			return filas, 0
		}
		if columnas && p[0] == 3 && strings.HasPrefix(string(p[1:4]), "def") {
			continue // definición de columna
		}
		columnas = false
		n, m, ok := lenencMy(p)
		if !ok || len(p) < m+int(n) {
			k.t.Fatalf("fila rara %q", p)
		}
		filas = append(filas, string(p[m:m+int(n)]))
	}
}

func TestMyLab(t *testing.T) {
	addr, cred := entornoMyLab(t)
	var dials atomic.Int32
	e := &entornoMy{audit: t.TempDir() + "/" + AuditFile}
	e.p = New(Options{AuditPath: e.audit, Logf: t.Logf, DialPG: func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}})
	dialUp := e.p.dialUp
	e.p.dialUp = func(ctx context.Context, a string) (net.Conn, error) {
		dials.Add(1)
		return dialUp(ctx, a)
	}
	if _, err := e.p.SetCredentials([]Credential{cred}); err != nil {
		t.Fatal(err)
	}
	t.Logf("destino %s, TLS %q, nombre TLS %q", cred.destinoPG(), cred.UpstreamTLS, cred.TLSServerName)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.ps = NewPGServer(e.p)
	e.addr = ln.Addr().String()
	go e.ps.Serve(ln)
	t.Cleanup(func() { e.ps.Close(); e.p.Close() })

	login := func(t *testing.T, plugin, pass string) *clienteMy {
		k := conectarMy(t, e.addr)
		k.saludo()
		k.responder(capsCliente&^(myLocalFiles|myConnectAttrs), cred.User, cred.Database, plugin, pass)
		p := k.leer()
		if plugin == pluginSHA2 && len(p) == 2 && p[0] == 1 {
			p = k.leer()
		}
		if p[0] != 0 {
			code, st, msg := esperarErr(t, p)
			t.Fatalf("sin OK: %d %s %q", code, st, msg)
		}
		return k
	}
	consulta := func(k *clienteMy, q string) ([]string, uint16) {
		k.c.Write(paqueteMy(0, append([]byte{0x03}, q...)))
		return k.resultado()
	}

	for _, plugin := range []string{pluginNative, pluginSHA2} {
		t.Run("consulta "+plugin, func(t *testing.T) {
			k := login(t, plugin, myMarca)
			filas, code := consulta(k, "SELECT CONCAT('kling-', 41+1)")
			if code != 0 || len(filas) != 1 || filas[0] != "kling-42" {
				t.Fatalf("filas %q, error %d", filas, code)
			}
			k.c.Write(paqueteMy(0, []byte{0x01}))
			k.c.Close()
		})
	}

	t.Run("marcador malo", func(t *testing.T) {
		antes := dials.Load()
		k := conectarMy(t, e.addr)
		if code, _, _ := esperarErr(t, k.login(pluginNative, PlaceholderPrefix+"otro")); code != 1045 {
			t.Errorf("código %d", code)
		}
		if dials.Load() != antes {
			t.Error("el proxy conectó al servidor con un marcador malo")
		}
	})

	t.Run("kill query", func(t *testing.T) {
		k := login(t, pluginNative, myMarca)
		filas, code := consulta(k, "SELECT CONNECTION_ID()")
		if code != 0 || len(filas) != 1 {
			t.Fatalf("CONNECTION_ID: %q %d", filas, code)
		}
		id, err := strconv.ParseUint(filas[0], 10, 64)
		if err != nil || id == 0 {
			t.Fatalf("id %q", filas[0])
		}
		otra := login(t, pluginNative, myMarca)
		// Con el id del saludo (0) no se cancela nada: lo que documenta
		// mysql.go.
		if _, code := consulta(otra, "KILL QUERY 0"); code == 0 {
			t.Error("KILL QUERY 0 no dio error")
		}
		inicio := time.Now()
		k.c.Write(paqueteMy(0, append([]byte{0x03}, "SELECT SLEEP(30)"...)))
		time.Sleep(500 * time.Millisecond)
		if _, code := consulta(otra, "KILL QUERY "+filas[0]); code != 0 {
			t.Fatalf("KILL QUERY %s: error %d", filas[0], code)
		}
		filas, code = k.resultado()
		t.Logf("SLEEP cortado: filas %q, error %d", filas, code)
		if d := time.Since(inicio); d > 10*time.Second {
			t.Errorf("KILL QUERY con el id real no cortó (tardó %v)", d)
		}
		k.c.Close()
		otra.c.Close()
	})

	recs, crudo := e.registro(t)
	for _, r := range recs {
		t.Logf("registro: %+v", r)
		if r.Host != "" && r.Upstream != cred.Upstream {
			t.Errorf("el registro dice upstream %q, la credencial %q", r.Upstream, cred.Upstream)
		}
	}
	for _, prohibido := range []string{cred.Secret, myMarca} {
		if strings.Contains(crudo, prohibido) {
			t.Errorf("el registro contiene la clave o el marcador")
		}
	}
}
