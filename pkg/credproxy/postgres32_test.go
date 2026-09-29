package credproxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Tests del protocolo 3.2 (PostgreSQL 18): claves de cancelación de longitud
// variable en BackendKeyData y CancelRequest, con invitados y servidores 3.0
// y 3.2 en todas las combinaciones.

// loginVersion hace el arranque con 3.minor y manda el marcador; devuelve la
// versión que el proxy negoció con el invitado (la pedida si no mandó
// NegotiateProtocolVersion).
func (k *clientePG) loginVersion(minor uint16, pares ...string) uint32 {
	k.t.Helper()
	if len(pares) == 0 {
		pares = []string{"user", pgUser, "database", pgDB}
	}
	k.arranque(minor, pares...)
	negociada := uint32(3<<16 | uint32(minor))
	tipo, msg := k.leer()
	if tipo == 'v' {
		if len(msg) != 8 || binary.BigEndian.Uint32(msg[4:]) != 0 {
			k.t.Fatalf("NegotiateProtocolVersion mal formado: %x", msg)
		}
		negociada = binary.BigEndian.Uint32(msg)
		tipo, msg = k.leer()
	}
	if tipo != 'R' || binary.BigEndian.Uint32(msg) != 3 {
		k.t.Fatalf("esperaba AuthenticationCleartextPassword, llegó %q %x", tipo, msg)
	}
	k.c.Write(mensajePG('p', append([]byte(pgMarca), 0)))
	if tipo, msg := k.leer(); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		k.t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	return negociada
}

// hastaListoK lee hasta ReadyForQuery y devuelve BackendKeyData entero.
func (k *clientePG) hastaListoK() (pid uint32, clave []byte) {
	k.t.Helper()
	for {
		tipo, msg := k.leer()
		switch tipo {
		case 'K':
			pid, clave = binary.BigEndian.Uint32(msg[:4]), msg[4:]
		case 'Z':
			return
		case 'E':
			k.t.Fatalf("error del proxy: %q", msg)
		}
	}
}

// cancelarPG manda un CancelRequest con una clave de cualquier longitud y
// espera a que el proxy cierre.
func cancelarPG(t *testing.T, addr string, pid uint32, clave []byte) {
	t.Helper()
	c := conectarPG(t, addr)
	m := binary.BigEndian.AppendUint32(nil, uint32(12+len(clave)))
	m = binary.BigEndian.AppendUint32(m, pgCancelRequest)
	m = binary.BigEndian.AppendUint32(m, pid)
	c.c.Write(append(m, clave...))
	io.ReadAll(c.br)
}

// Las cuatro combinaciones de invitado y servidor (3.0/3.2): la versión que
// ve cada lado, la longitud de la clave falsa (la de la versión del
// invitado), que la falsa no es la real, y que la cancelación llega al
// servidor con SU clave real, de su longitud.
func TestPGProtocolo32Cancelacion(t *testing.T) {
	casos := []struct {
		invitado, pedidaServidor uint16
		v32                      bool
		largoFalsa               int
		real                     string
	}{
		{0, 0, false, 4, "\xde\xad\xbe\xef"},
		{0, 0, true, 4, "\xde\xad\xbe\xef"},
		{2, 2, false, pgClaveFalsa32, "\xde\xad\xbe\xef"},
		{2, 2, true, pgClaveFalsa32, string(pgClaveReal32)},
	}
	for _, c := range casos {
		t.Run(fmt.Sprintf("invitado3.%d-servidor32=%v", c.invitado, c.v32), func(t *testing.T) {
			srv := nuevoServidorPG(t, "scram")
			srv.v32 = c.v32
			e := proxyPG(t, srv, nil)
			k := conectarPG(t, e.addr)
			if v := k.loginVersion(c.invitado); v != 3<<16|uint32(c.invitado) {
				t.Fatalf("el proxy negoció %x con el invitado", v)
			}
			pid, falsa := k.hastaListoK()
			if pid != 42 || len(falsa) != c.largoFalsa {
				t.Fatalf("BackendKeyData: pid %d, clave de %d bytes", pid, len(falsa))
			}
			if bytes.Contains([]byte(c.real), falsa) || bytes.Equal(falsa, pgClaveReal32) {
				t.Fatal("el invitado ve la clave real")
			}
			srv.mu.Lock()
			vers := append([]uint32(nil), srv.versiones...)
			srv.mu.Unlock()
			if len(vers) != 1 || vers[0] != 3<<16|uint32(c.pedidaServidor) {
				t.Fatalf("el servidor recibió el arranque %x", vers)
			}

			k.c.Write(mensajePG('Q', []byte("SELECT pg_sleep(60)\x00")))
			time.Sleep(50 * time.Millisecond)
			// La falsa recortada, alargada o cambiada no vale.
			otra := append([]byte(nil), falsa...)
			otra[len(otra)-1] ^= 1
			cancelarPG(t, e.addr, pid, otra)
			cancelarPG(t, e.addr, pid, falsa[:len(falsa)-1])
			cancelarPG(t, e.addr, pid, append(append([]byte(nil), falsa...), 0))
			cancelarPG(t, e.addr, pid+1, falsa)
			cancelarPG(t, e.addr, pid, falsa)
			tipo, msg := k.leer()
			if tipo != 'E' || sqlstate(msg) != "57014" {
				t.Fatalf("la consulta no se canceló: %q %q", tipo, msg)
			}
			srv.mu.Lock()
			canc := append([]cancelVisto(nil), srv.cancelado...)
			srv.mu.Unlock()
			if len(canc) != 1 || canc[0] != (cancelVisto{42, c.real}) {
				t.Fatalf("cancelaciones en el servidor: %q", canc)
			}
			k.c.Close()
			recs, _ := e.registro(t)
			// Las cuatro malas se rechazan (desconocidas, o mal formadas si la
			// recortada de 3.0 se queda en 3 bytes) y la buena no.
			malas, buenas := 0, 0
			for _, r := range recs {
				switch {
				case r.Method != "cancel":
				case r.Reason == ReasonUnknownCancel || r.Reason == ReasonBadStartup:
					malas++
				case r.Reason == "":
					buenas++
				}
			}
			if malas != 4 || buenas != 1 {
				t.Errorf("cancelaciones: %d malas y %d buenas: %+v", malas, buenas, recs)
			}
			e.p.cancelMu.Lock()
			n := len(e.p.cancelaciones)
			e.p.cancelMu.Unlock()
			if n != 0 {
				t.Errorf("quedan %d claves de cancelación", n)
			}
		})
	}
}

// Versiones que el proxy baja: 3.1 (nunca se usó) a 3.0 y 3.3+ a 3.2, con
// NegotiateProtocolVersion; al servidor se le pide la negociada.
func TestPGProtocoloNegociaALaBaja(t *testing.T) {
	for _, c := range []struct{ pide, queda uint16 }{{1, 0}, {3, 2}, {0xffff, 2}} {
		t.Run(fmt.Sprintf("3.%d", c.pide), func(t *testing.T) {
			srv := nuevoServidorPG(t, "scram")
			srv.v32 = true
			e := proxyPG(t, srv, nil)
			k := conectarPG(t, e.addr)
			if v := k.loginVersion(c.pide); v != 3<<16|uint32(c.queda) {
				t.Fatalf("negociada %x, quería 3.%d", v, c.queda)
			}
			if _, falsa := k.hastaListoK(); (c.queda == 2) != (len(falsa) == pgClaveFalsa32) {
				t.Fatalf("clave falsa de %d bytes con 3.%d", len(falsa), c.queda)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.versiones) != 1 || srv.versiones[0] != 3<<16|uint32(c.queda) {
				t.Fatalf("el servidor recibió %x", srv.versiones)
			}
		})
	}
}

// Un servidor que negocia mal (versión más alta que la pedida, otra versión
// mayor, opciones que no se mandaron, mensaje mal formado) o que manda un
// BackendKeyData con una clave que no es de su versión, o dos, no llega al
// invitado: error propio o conexión cerrada antes de ReadyForQuery.
func TestPGProtocoloServidorMalo(t *testing.T) {
	npv := func(v, n uint32) []byte {
		return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, v), n)
	}
	for _, c := range []struct {
		nombre string
		minor  uint16
		mod    func(*servidorPG)
	}{
		{"npv-mas-alta", 0, func(s *servidorPG) { s.negociaMal = npv(pgProto32, 0) }},
		{"npv-igual", 2, func(s *servidorPG) { s.negociaMal = npv(pgProto32, 0) }},
		{"npv-mayor-4", 2, func(s *servidorPG) { s.negociaMal = npv(4<<16, 0) }},
		{"npv-opciones", 2, func(s *servidorPG) { s.negociaMal = npv(pgProto30, 1) }},
		{"npv-corto", 2, func(s *servidorPG) { s.negociaMal = []byte{0, 3} }},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			srv := nuevoServidorPG(t, "scram")
			srv.v32 = true
			c.mod(srv)
			e := proxyPG(t, srv, nil)
			k := conectarPG(t, e.addr)
			k.arranque(c.minor, "user", pgUser, "database", pgDB)
			tipo, _ := k.leer()
			if tipo == 'v' {
				tipo, _ = k.leer()
			}
			if tipo != 'R' {
				t.Fatalf("esperaba la petición de contraseña, llegó %q", tipo)
			}
			k.c.Write(mensajePG('p', append([]byte(pgMarca), 0)))
			tipo, msg := k.leer()
			if code, _ := k.esperarError(tipo, msg); code != "28P01" {
				t.Fatalf("SQLSTATE %q", code)
			}
			recs, _ := e.registro(t)
			if len(recs) != 1 || recs[0].Reason != ReasonUpstreamAuth {
				t.Errorf("registro: %+v", recs)
			}
		})
	}

	for _, c := range []struct {
		nombre string
		minor  uint16
		v32    bool
		clave  []byte
		veces  int
	}{
		{"3.0-clave-larga", 0, false, pgClaveReal32, 1},
		{"3.0-clave-corta", 0, false, []byte{1, 2, 3}, 1},
		{"3.2-clave-257", 2, true, bytes.Repeat([]byte{7}, pgMaxClaveCancel+1), 1},
		{"3.2-clave-corta", 2, true, []byte{1, 2, 3}, 1},
		{"3.2-dos-claves", 2, true, pgClaveReal32, 2},
		{"3.0-dos-claves", 0, false, []byte{1, 2, 3, 4}, 2},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			srv := nuevoServidorPG(t, "scram")
			srv.v32, srv.claveMal, srv.claveMalVeces = c.v32, c.clave, c.veces
			e := proxyPG(t, srv, nil)
			k := conectarPG(t, e.addr)
			k.loginVersion(c.minor)
			for {
				tipo, _, err := leerMensaje(k.br, 1<<20)
				if err != nil {
					break
				}
				if tipo == 'Z' || tipo == 'K' && c.veces == 1 {
					t.Fatalf("llegó %q al invitado", tipo)
				}
			}
			k.c.Close()
			e.registro(t)
			e.p.cancelMu.Lock()
			n := len(e.p.cancelaciones)
			e.p.cancelMu.Unlock()
			if n != 0 {
				t.Errorf("quedan %d claves de cancelación", n)
			}
		})
	}
}

// Un CancelRequest más largo que el de 3.2 (clave de más de 256 bytes) o más
// corto que el de 3.0 es un arranque mal formado, sin llegar a buscar la clave.
func TestPGCancelRequestTamanos(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	e := proxyPG(t, srv, nil)
	cancelarPG(t, e.addr, 42, bytes.Repeat([]byte{1}, pgMaxClaveCancel+1))
	cancelarPG(t, e.addr, 42, []byte{1, 2, 3})
	cancelarPG(t, e.addr, 42, bytes.Repeat([]byte{1}, pgMaxClaveCancel))
	recs, _ := e.registro(t)
	var motivos []string
	for _, r := range recs {
		motivos = append(motivos, r.Reason)
	}
	got := strings.Join(motivos, ",")
	// El orden de los registros depende de cuándo cierra cada sesión.
	if strings.Count(got, ReasonBadStartup) != 2 || strings.Count(got, ReasonUnknownCancel) != 1 {
		t.Errorf("motivos: %s", got)
	}
	if srv.conns.Load() != 0 {
		t.Errorf("el proxy conectó %d veces con el servidor", srv.conns.Load())
	}
}
