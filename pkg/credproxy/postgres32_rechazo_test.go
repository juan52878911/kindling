package credproxy

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Un servidor que corta el arranque en 3.2 con 08P01 en vez de negociar
// (pgbouncer anterior a 1.25: "bad packet header"): el proxy repite una vez en
// 3.0, en otra conexión, y el invitado entra en 3.2 sin notarlo, con su clave
// falsa de 32 bytes y la cancelación con la real de 4. El rechazo se recuerda
// por upstream: la segunda conexión arranca ya en 3.0, con una sola marcación.
// El aviso sale una vez por credencial. Un invitado 3.0, o un servidor que también rechaza la 3.0,
// no repiten nada: un intento, o dos, y el error de siempre.
func TestPGServidorRechaza32RepiteEn30(t *testing.T) {
	t.Run("repite", func(t *testing.T) {
		var mu sync.Mutex
		var logs []string
		srv := nuevoServidorPG(t, "scram")
		srv.rechaza32 = true
		e := proxyPG(t, srv, nil, func(o *Options) {
			o.Logf = func(f string, a ...any) {
				mu.Lock()
				logs = append(logs, fmt.Sprintf(f, a...))
				mu.Unlock()
			}
		})
		for i := range 2 {
			k := conectarPG(t, e.addr)
			if v := k.loginVersion(2); v != pgProto32 {
				t.Fatalf("el proxy negoció %x con el invitado", v)
			}
			pid, falsa := k.hastaListoK()
			if pid != 42 || len(falsa) != pgClaveFalsa32 {
				t.Fatalf("BackendKeyData: pid %d, clave de %d bytes", pid, len(falsa))
			}
			if i == 0 {
				k.c.Write(mensajePG('Q', []byte("SELECT pg_sleep(60)\x00")))
				time.Sleep(50 * time.Millisecond)
				cancelarPG(t, e.addr, pid, falsa)
				if tipo, msg := k.leer(); tipo != 'E' || sqlstate(msg) != "57014" {
					t.Fatalf("la consulta no se canceló: %q %q", tipo, msg)
				}
			}
			k.c.Close()
		}
		recs, _ := e.registro(t)
		srv.mu.Lock()
		vers := append([]uint32(nil), srv.versiones...)
		canc := append([]cancelVisto(nil), srv.cancelado...)
		srv.mu.Unlock()
		if want := []uint32{pgProto32, pgProto30, pgProto30}; fmt.Sprint(vers) != fmt.Sprint(want) {
			t.Fatalf("arranques en el servidor %x, quería %x", vers, want)
		}
		if len(canc) != 1 || canc[0] != (cancelVisto{42, "\xde\xad\xbe\xef"}) {
			t.Fatalf("cancelaciones en el servidor: %q", canc)
		}
		buenas := 0
		for _, r := range recs {
			if r.Method != "cancel" && r.Reason == "" && strings.HasPrefix(r.Auth, AuthSCRAM) {
				buenas++
			}
		}
		if buenas != 2 {
			t.Errorf("registro: %+v", recs)
		}
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range logs {
			if strings.Contains(l, "answered the protocol 3.2 startup with SQLSTATE 08P01") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d avisos, quería 1: %q", n, logs)
		}
	})
	for _, c := range []struct {
		nombre   string
		minor    uint16
		intentos int
	}{
		{"invitado-3.0", 0, 1},
		{"rechaza-las-dos", 2, 2},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			srv := nuevoServidorPG(t, "scram")
			srv.rechaza = true
			e := proxyPG(t, srv, nil)
			k := conectarPG(t, e.addr)
			k.arranque(c.minor, "user", pgUser, "database", pgDB)
			if tipo, _ := k.leer(); tipo != 'R' {
				t.Fatalf("esperaba la petición de contraseña, llegó %q", tipo)
			}
			k.c.Write(mensajePG('p', append([]byte(pgMarca), 0)))
			tipo, msg := k.leer()
			if code, _ := k.esperarError(tipo, msg); code != "08006" {
				t.Fatalf("SQLSTATE %q", code)
			}
			recs, _ := e.registro(t)
			if len(recs) != 1 || recs[0].Reason != ReasonUpstreamAuth {
				t.Errorf("registro: %+v", recs)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.versiones) != c.intentos {
				t.Errorf("%d arranques en el servidor, quería %d: %x", len(srv.versiones), c.intentos, srv.versiones)
			}
		})
	}
}

// Marcaciones al servidor por conexión de un invitado 3.2 contra un servidor
// que corta la 3.2: antes eran 2 (marcar, TLS, 08P01, cerrar y otra vez en
// 3.0); recordando el rechazo, 1 más la primera vez (n+1 en n conexiones).
// Pasada la caducidad se vuelve a probar la 3.2, por si el servidor se
// actualizó.
func TestPGRechazo32SeRecuerdaPorUpstream(t *testing.T) {
	const conexiones = 20
	srv := nuevoServidorPG(t, "scram")
	srv.rechaza32 = true
	e := proxyPG(t, srv, nil)
	entrar := func() {
		t.Helper()
		k := conectarPG(t, e.addr)
		if v := k.loginVersion(2); v != pgProto32 {
			t.Fatalf("el proxy negoció %x con el invitado", v)
		}
		if _, falsa := k.hastaListoK(); len(falsa) != pgClaveFalsa32 {
			t.Fatalf("clave falsa de %d bytes", len(falsa))
		}
		k.c.Close()
	}
	marcaciones := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.dialed)
	}
	for range conexiones {
		entrar()
	}
	n := marcaciones()
	t.Logf("%d marcaciones en %d conexiones (%.2f por conexión)", n, conexiones, float64(n)/conexiones)
	if n != conexiones+1 {
		t.Fatalf("%d marcaciones en %d conexiones; quería %d", n, conexiones, conexiones+1)
	}

	// Caducado el recuerdo, la siguiente vuelve a probar la 3.2.
	e.p.pg32.mu.Lock()
	for d := range e.p.pg32.hasta {
		e.p.pg32.hasta[d] = time.Now().Add(-time.Second)
	}
	e.p.pg32.mu.Unlock()
	entrar()
	srv.mu.Lock()
	ultimas := append([]uint32(nil), srv.versiones[len(srv.versiones)-2:]...)
	srv.mu.Unlock()
	if want := []uint32{pgProto32, pgProto30}; fmt.Sprint(ultimas) != fmt.Sprint(want) {
		t.Fatalf("tras caducar, arranques %x; quería %x", ultimas, want)
	}
}
