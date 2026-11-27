package credproxy

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Upstream fijado por el operador: loopback permitido, TLS verificado contra
// TLSServerName (o Domain), o sin TLS y solo SCRAM-SHA-256; nunca metadatos ni
// la red interna de kindling.

const pgNombreTLS = "pg.interno.lan"

func caDe(srv *servidorPG) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.cert.Certificate[0]}))
}

// Upstream en el loopback con verify-full contra TLSServerName: el proxy
// marca la dirección fijada (no el dialer de dominios), verifica el
// certificado como pg.interno.lan y el registro dice a dónde fue. Sin
// TLSServerName el mismo servidor no vale: su certificado no es de Domain.
func TestPGUpstreamLoopbackVerifyFull(t *testing.T) {
	srv := nuevoServidorPGNombre(t, "scram", pgNombreTLS)
	up := srv.ln.Addr().String()
	e := proxyPG(t, srv, func(c *Credential) {
		c.Upstream, c.TLSServerName, c.CAPEM = up, pgNombreTLS, caDe(srv)
	})
	k := conectarPG(t, e.addr)
	if tipo, msg := k.login(pgMarca); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	k.hastaListo()
	if tipo, _ := k.consulta("SELECT 1"); tipo != 'D' {
		t.Fatalf("consulta: %q", tipo)
	}
	k.leer()
	k.leer()
	k.c.Close()
	if !srv.sslVisto.Load() {
		t.Error("el servidor no vio SSLRequest")
	}
	e.mu.Lock()
	dialed := e.dialed
	e.mu.Unlock()
	if len(dialed) != 0 {
		t.Errorf("con upstream se usó el dialer de dominios: %v", dialed)
	}
	recs, crudo := e.registro(t)
	if len(recs) != 1 || recs[0].Reason != "" || recs[0].Upstream != up || recs[0].Auth != AuthSCRAM || recs[0].Host != pgDominio {
		t.Fatalf("registro %+v", recs)
	}
	if !strings.Contains(crudo, `"upstream":"`+up+`"`) {
		t.Errorf("el registro no lleva upstream: %s", crudo)
	}

	// Sin TLSServerName se verifica contra Domain: no casa.
	srv2 := nuevoServidorPGNombre(t, "scram", pgNombreTLS)
	e2 := proxyPG(t, srv2, func(c *Credential) { c.Upstream, c.CAPEM = srv2.ln.Addr().String(), caDe(srv2) })
	k = conectarPG(t, e2.addr)
	tipo, msg := k.login(pgMarca)
	if code, _ := k.esperarError(tipo, msg); code != "08006" {
		t.Errorf("certificado de otro nombre: SQLSTATE %q", code)
	}
	k.c.Close()
	if recs, _ := e2.registro(t); len(recs) != 1 || recs[0].Reason != ReasonUpstreamTLS {
		t.Errorf("registro %+v", recs)
	}
}

// Sin TLS: sin SSLRequest y con SCRAM-SHA-256 ("n", nada que atar); la
// consulta va y vuelve.
func TestPGUpstreamSinTLSSCRAM(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	srv.sinTLS = true
	up := srv.ln.Addr().String()
	e := proxyPG(t, srv, func(c *Credential) { c.Upstream, c.UpstreamTLS, c.CAPEM = up, UpstreamTLSDisable, "" })
	k := conectarPG(t, e.addr)
	if tipo, msg := k.login(pgMarca); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	k.hastaListo()
	if tipo, msg := k.consulta("SELECT 2"); tipo != 'D' || !strings.Contains(string(msg), "SELECT 2") {
		t.Fatalf("consulta: %q %q", tipo, msg)
	}
	k.leer()
	k.leer()
	k.c.Close()
	if srv.sslVisto.Load() {
		t.Error("con disable no debe mandarse SSLRequest")
	}
	recs, _ := e.registro(t)
	if len(recs) != 1 || recs[0].Reason != "" || recs[0].Auth != AuthSCRAM || recs[0].Upstream != up {
		t.Fatalf("registro %+v", recs)
	}
}

// Sin TLS, todo lo que no sea SCRAM-SHA-256 se rechaza sin mandar la clave:
// contraseña en claro, md5, solo -PLUS y trust.
func TestPGUpstreamSinTLSRechazos(t *testing.T) {
	for _, modo := range []string{"password", "md5", "solo-plus", "trust"} {
		t.Run(modo, func(t *testing.T) {
			srv := nuevoServidorPG(t, modo)
			srv.sinTLS = true
			var mu sync.Mutex
			var logs []string
			e := proxyPG(t, srv, func(c *Credential) {
				c.Upstream, c.UpstreamTLS, c.CAPEM = srv.ln.Addr().String(), UpstreamTLSDisable, ""
			}, func(o *Options) {
				o.Logf = func(f string, a ...any) {
					mu.Lock()
					defer mu.Unlock()
					logs = append(logs, fmt.Sprintf(f, a...))
				}
			})
			k := conectarPG(t, e.addr)
			tipo, msg := k.login(pgMarca)
			if code, _ := k.esperarError(tipo, msg); code != "28P01" {
				t.Errorf("SQLSTATE %q", code)
			}
			k.c.Close()
			srv.mu.Lock()
			vistas := srv.clavesVis
			srv.mu.Unlock()
			if len(vistas) != 0 {
				t.Errorf("la contraseña cruzó sin TLS: %q", vistas)
			}
			recs, _ := e.registro(t)
			if len(recs) != 1 || recs[0].Reason != ReasonUpstreamAuth || recs[0].Auth != "" {
				t.Errorf("registro %+v", recs)
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(strings.Join(logs, "\n"), "upstream without TLS requires SCRAM-SHA-256") {
				t.Errorf("el log no lo explica: %q", logs)
			}
		})
	}
}

// Un servidor que contesta como sin TLS a quien espera verify-full sigue
// siendo un fallo con upstream fijado: disable hay que pedirlo.
func TestPGUpstreamVerifyFullNoCaeAClaro(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	srv.ssl, srv.sinTLS = 'N', true
	e := proxyPG(t, srv, func(c *Credential) { c.Upstream = srv.ln.Addr().String() })
	k := conectarPG(t, e.addr)
	tipo, msg := k.login(pgMarca)
	if code, _ := k.esperarError(tipo, msg); code != "08006" {
		t.Errorf("SQLSTATE %q", code)
	}
	k.c.Close()
	if recs, _ := e.registro(t); len(recs) != 1 || recs[0].Reason != ReasonUpstreamTLS {
		t.Errorf("registro %+v", recs)
	}
}

// Destinos prohibidos: un nombre que resuelve a los metadatos o a la red
// interna de kindling no se marca, aunque otra de sus IPs valga; el proxy ni
// intenta conectar.
func TestPGUpstreamNombreProhibido(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	srv.sinTLS = true
	_, puerto, _ := net.SplitHostPort(srv.ln.Addr().String())
	for nombre, ips := range map[string][]string{
		"metadatos":           {"169.254.169.254"},
		"metadatos mapeados":  {"::ffff:169.254.169.254"},
		"veth de kindling":    {"127.0.0.1", "172.30.0.1"},
		"enlace del invitado": {"172.16.0.1"},
		"esta red":            {"0.0.0.0"},
		"multicast":           {"224.0.0.1"},
		"metadatos v6 AWS":    {"fd00:ec2::254"},
	} {
		t.Run(nombre, func(t *testing.T) {
			var preguntados []string
			e := proxyPG(t, srv, func(c *Credential) {
				c.Upstream, c.UpstreamTLS, c.CAPEM = "db.trampa.lan:"+puerto, UpstreamTLSDisable, ""
			}, func(p *Proxy) {
				p.lookupUp = func(_ context.Context, host string) ([]netip.Addr, error) {
					preguntados = append(preguntados, host)
					var out []netip.Addr
					for _, s := range ips {
						out = append(out, netip.MustParseAddr(s))
					}
					return out, nil
				}
			})
			antes := srv.conns.Load()
			k := conectarPG(t, e.addr)
			tipo, msg := k.login(pgMarca)
			if code, _ := k.esperarError(tipo, msg); code != "08006" {
				t.Errorf("SQLSTATE %q", code)
			}
			k.c.Close()
			if srv.conns.Load() != antes {
				t.Error("el proxy conectó a pesar de la IP prohibida")
			}
			if len(preguntados) != 1 || preguntados[0] != "db.trampa.lan" {
				t.Errorf("resolvió %v", preguntados)
			}
			if recs, _ := e.registro(t); len(recs) != 1 || recs[0].Reason != ReasonUpstreamError {
				t.Errorf("registro %+v", recs)
			}
		})
	}
}

// dialFijado directamente: IP literal prohibida, nombre con una IP
// prohibida, nombre que no resuelve y el caso bueno.
func TestDialFijado(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, puerto, _ := net.SplitHostPort(ln.Addr().String())
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "bueno.lan":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "mixto.lan":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254")}, nil
		case "vacio.lan":
			return nil, nil
		}
		return nil, errors.New("no existe")
	}
	dial := dialFijado(lookup, &net.Dialer{Timeout: 2 * time.Second}, nil)
	ctx := context.Background()
	for _, addr := range []string{"169.254.169.254:" + puerto, "172.30.5.1:" + puerto, "[fe80::1]:" + puerto, "mixto.lan:" + puerto} {
		if c, err := dial(ctx, addr); err == nil || !errors.Is(err, errUpstreamProhibido) {
			if c != nil {
				c.Close()
			}
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"vacio.lan:" + puerto, "otro.lan:" + puerto} {
		if c, err := dial(ctx, addr); err == nil {
			c.Close()
			t.Errorf("%s: conectó", addr)
		}
	}
	// localhost no se pregunta al resolver (el falso no lo conoce): es el
	// loopback por definición.
	for _, addr := range []string{"bueno.lan:" + puerto, "127.0.0.1:" + puerto, "localhost:" + puerto} {
		c, err := dial(ctx, addr)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			continue
		}
		c.Close()
	}

	// Con DialLoopback (kling-vz confinado) el loopback se pide por él, con
	// el puerto y nada más; lo demás sigue igual y lo prohibido, prohibido.
	var pedidos []int
	loop := func(_ context.Context, p int) (net.Conn, error) {
		pedidos = append(pedidos, p)
		return nil, errors.New("del daemon")
	}
	dial = dialFijado(lookup, &net.Dialer{Timeout: 2 * time.Second}, loop)
	for _, addr := range []string{"127.0.0.1:" + puerto, "localhost:" + puerto, "[::1]:" + puerto} {
		if _, err := dial(ctx, addr); err == nil || err.Error() != "del daemon" {
			t.Errorf("%s: %v", addr, err)
		}
	}
	if _, err := dial(ctx, "169.254.169.254:"+puerto); !errors.Is(err, errUpstreamProhibido) {
		t.Errorf("prohibida con DialLoopback: %v", err)
	}
	if _, err := dial(ctx, fmt.Sprintf("127.0.0.1:%d", ForwardPortMin)); !errors.Is(err, errUpstreamProhibido) {
		t.Errorf("rango reservado con DialLoopback: %v", err)
	}
	if len(pedidos) != 3 || fmt.Sprint(pedidos[0]) != puerto {
		t.Errorf("pedidos al daemon: %v", pedidos)
	}
}

// El rango reservado a los reenvíos de kindling (macOS: el invitado de cada
// máquina en 127.0.0.1:<rango>): ningún upstream del loopback puede apuntar
// ahí, ni al validar ni al marcar, aunque haya alguien escuchando. Fuera del
// loopback el mismo puerto vale (es una base de datos de la LAN), y fuera del
// rango el loopback también (Docker).
func TestUpstreamRangoReenvios(t *testing.T) {
	var ln net.Listener
	for p := ForwardPortMin; p <= ForwardPortMax && ln == nil; p++ {
		ln, _ = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
	}
	if ln == nil {
		t.Skip("no free port in the reserved range")
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, puerto, _ := net.SplitHostPort(ln.Addr().String())

	base := func() Credential {
		return Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
			Kind: KindPostgres, User: pgUser, Database: pgDB}
	}
	for _, up := range []string{"127.0.0.1:" + puerto, "localhost:" + puerto, "LOCALHOST.:" + puerto, "[::1]:" + puerto,
		"127.0.0.2:" + puerto, "[::ffff:127.0.0.1]:" + puerto, fmt.Sprintf("127.0.0.1:%d", ForwardPortMin), fmt.Sprintf("127.0.0.1:%d", ForwardPortMax)} {
		c := base()
		c.Upstream = up
		if err := ValidarCredenciales([]Credential{c}); err == nil || !strings.Contains(err.Error(), "reserved forward range") {
			t.Errorf("%s: %v", up, err)
		}
	}
	for _, up := range []string{fmt.Sprintf("127.0.0.1:%d", ForwardPortMin-1), fmt.Sprintf("127.0.0.1:%d", ForwardPortMax+1),
		"10.0.0.5:" + puerto, "db.lan:" + puerto} {
		c := base()
		c.Upstream = up
		if err := ValidarCredenciales([]Credential{c}); err != nil {
			t.Errorf("%s: %v", up, err)
		}
	}

	// Al marcar: un nombre que resuelve al loopback, y las formas literales
	// (por si llegaran sin pasar por la validación, p. ej. un almacén viejo).
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	dial := dialFijado(lookup, &net.Dialer{Timeout: 2 * time.Second}, nil)
	for _, addr := range []string{"db.lan:" + puerto, "127.0.0.1:" + puerto, "localhost:" + puerto} {
		if c, err := dial(context.Background(), addr); err == nil || !errors.Is(err, errUpstreamProhibido) {
			if c != nil {
				c.Close()
			}
			t.Errorf("dial %s: %v", addr, err)
		}
	}

	for u, want := range map[string]int{"127.0.0.1:5432": 5432, "localhost:29001": 29001, "[::1]:7": 7} {
		if got, ok := UpstreamPuertoLoopback(u); !ok || got != want {
			t.Errorf("UpstreamPuertoLoopback(%q) = %d, %v", u, got, ok)
		}
	}
	for _, u := range []string{"", "10.0.0.5:5432", "db.lan:5432"} {
		if _, ok := UpstreamPuertoLoopback(u); ok {
			t.Errorf("UpstreamPuertoLoopback(%q) = true", u)
		}
	}
}

// Validación de Upstream, UpstreamTLS y TLSServerName.
func TestValidarUpstream(t *testing.T) {
	base := func() Credential {
		return Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
			Kind: KindPostgres, User: pgUser, Database: pgDB}
	}
	for nombre, mod := range map[string]func(*Credential){
		"disable sin upstream":  func(c *Credential) { c.UpstreamTLS = "disable" },
		"tls raro":              func(c *Credential) { c.Upstream, c.UpstreamTLS = "127.0.0.1:5432", "require" },
		"disable con CA":        func(c *Credential) { c.Upstream, c.UpstreamTLS, c.CAPEM = "127.0.0.1:5432", "disable", "x" },
		"disable con nombre":    func(c *Credential) { c.Upstream, c.UpstreamTLS, c.TLSServerName = "127.0.0.1:5432", "disable", "a.lan" },
		"sin puerto":            func(c *Credential) { c.Upstream = "127.0.0.1" },
		"puerto 0":              func(c *Credential) { c.Upstream = "db.lan:0" },
		"puerto grande":         func(c *Credential) { c.Upstream = "db.lan:99999" },
		"puerto con nombre":     func(c *Credential) { c.Upstream = "db.lan:postgres" },
		"host con espacio":      func(c *Credential) { c.Upstream = "db lan:5432" },
		"host vacío":            func(c *Credential) { c.Upstream = ":5432" },
		"metadatos":             func(c *Credential) { c.Upstream = "169.254.169.254:80" },
		"metadatos mapeados":    func(c *Credential) { c.Upstream = "[::ffff:169.254.169.254]:5432" },
		"enlace del invitado":   func(c *Credential) { c.Upstream = "172.16.0.1:5432" },
		"veth de kindling":      func(c *Credential) { c.Upstream = "172.30.0.1:5432" },
		"esta red":              func(c *Credential) { c.Upstream = "0.0.0.0:5432" },
		"broadcast":             func(c *Credential) { c.Upstream = "255.255.255.255:5432" },
		"link-local v6":         func(c *Credential) { c.Upstream = "[fe80::1]:5432" },
		"zona v6":               func(c *Credential) { c.Upstream = "[fe80::1%en0]:5432" },
		"nombre TLS basura":     func(c *Credential) { c.TLSServerName = "no vale" },
		"http con upstream":     func(c *Credential) { c.Kind, c.User, c.Upstream = "", "", "127.0.0.1:5432" },
		"http con nombre TLS":   func(c *Credential) { c.Kind, c.User, c.TLSServerName = "", "", "a.lan" },
		"http con upstream tls": func(c *Credential) { c.Kind, c.User, c.UpstreamTLS = "", "", "verify-full" },
	} {
		c := base()
		mod(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: aceptada", nombre)
		}
	}
	for _, caso := range []struct{ up, tls, nombre, wantUp, wantTLS, wantNombre string }{
		{"LocalHost:5432", "", "", "localhost:5432", "", ""},
		{"127.0.0.1:5432", "DISABLE", "", "127.0.0.1:5432", "disable", ""},
		{"10.0.0.5:6432", "verify-full", "PG.Interno.LAN.", "10.0.0.5:6432", "", "pg.interno.lan"},
		{"[::1]:5432", "disable", "", "[::1]:5432", "disable", ""},
		{"host.docker.internal:5432", "", "", "host.docker.internal:5432", "", ""},
		{"", "", "db.otro.example.com", "", "", "db.otro.example.com"},
	} {
		c := []Credential{base()}
		c[0].Upstream, c[0].UpstreamTLS, c[0].TLSServerName = caso.up, caso.tls, caso.nombre
		if err := ValidarCredenciales(c); err != nil {
			t.Errorf("%+v: %v", caso, err)
			continue
		}
		if c[0].Upstream != caso.wantUp || c[0].UpstreamTLS != caso.wantTLS || c[0].TLSServerName != caso.wantNombre {
			t.Errorf("%+v: normalizada a %q %q %q", caso, c[0].Upstream, c[0].UpstreamTLS, c[0].TLSServerName)
		}
	}
	for u, want := range map[string]bool{"127.0.0.1:5432": false, "[::1]:5432": false, "localhost:5432": false,
		"10.0.0.5:5432": false, "": false, "db.lan:5432": true, "host.docker.internal:5432": true} {
		if got := UpstreamNecesitaDNS(u); got != want {
			t.Errorf("UpstreamNecesitaDNS(%q) = %v", u, got)
		}
	}
	for u, want := range map[string]bool{"127.0.0.1:5432": true, "localhost:5432": true, "[::1]:5432": true,
		"127.1.2.3:5432": true, "10.0.0.5:5432": false, "db.lan:5432": false, "basura": false} {
		if got := UpstreamLoopback(u); got != want {
			t.Errorf("UpstreamLoopback(%q) = %v", u, got)
		}
	}
}

// CancelRequest con upstream fijado y sin TLS: la cancelación va a la misma
// dirección, también sin TLS, y queda en el registro con su upstream.
func TestPGUpstreamCancel(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	srv.sinTLS = true
	up := srv.ln.Addr().String()
	e := proxyPG(t, srv, func(c *Credential) { c.Upstream, c.UpstreamTLS, c.CAPEM = up, UpstreamTLSDisable, "" })
	k := conectarPG(t, e.addr)
	k.login(pgMarca)
	pid, falsa := k.hastaListo()
	k.c.Write(mensajePG('Q', []byte("SELECT pg_sleep(60)\x00")))
	time.Sleep(50 * time.Millisecond)
	c := conectarPG(t, e.addr)
	m := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 16}, pgCancelRequest)
	m = binary.BigEndian.AppendUint32(m, pid)
	m = binary.BigEndian.AppendUint32(m, falsa)
	c.c.Write(m)
	io.ReadAll(c.br)
	tipo, msg := k.leer()
	if tipo != 'E' || sqlstate(msg) != "57014" {
		t.Fatalf("la consulta no se canceló: %q %q", tipo, msg)
	}
	srv.mu.Lock()
	canc := append([]cancelVisto(nil), srv.cancelado...)
	srv.mu.Unlock()
	if len(canc) != 1 || canc[0] != (cancelVisto{42, "\xde\xad\xbe\xef"}) {
		t.Fatalf("cancelaciones en el servidor: %x", canc)
	}
	if srv.sslVisto.Load() {
		t.Error("la cancelación mandó SSLRequest con disable")
	}
	k.c.Close()
	recs, _ := e.registro(t)
	hecha := false
	for _, r := range recs {
		if r.Method == "cancel" && r.Reason == "" && r.Upstream == up {
			hecha = true
		}
	}
	if !hecha {
		t.Errorf("registros: %+v", recs)
	}
}

// --- Credenciales HTTP con upstream fijado por el operador ---
//
// La réplica habla HTTP con un nombre reservado que su resolver desvía al
// proxy; el proxy cambia el marcador por el token y marca la dirección que
// fijó el operador (el gateway de IA en el loopback del host, o un servicio
// de la VPC por TLS verificado), nunca el dominio.

const (
	httpUpDominio = "l3.kindling.internal"
	httpUpClave   = "tok_inquilino_0123456789abcdef"
	httpUpMarca   = "kling-cred-0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b"
	httpUpRegla   = "POST /v1/chat/completions"
)

func credHTTPUp(up string) Credential {
	return Credential{Env: "L3_TOKEN", Domain: httpUpDominio, Placeholder: httpUpMarca, Secret: httpUpClave,
		Upstream: up, Allow: []string{httpUpRegla}}
}

// transporteProhibido es la salida hacia dominios públicos de los tests de
// upstream: con la dirección fijada no debe usarse nunca.
type transporteProhibido struct{ t *testing.T }

func (tp transporteProhibido) RoundTrip(r *http.Request) (*http.Response, error) {
	tp.t.Errorf("se usó la salida a dominios públicos para %s", r.URL.Host)
	return nil, errors.New("public egress used")
}

// proxyHTTPUp sirve un proxy con creds y registro de auditoría; ajustar, si
// no es nil, toca el proxy antes de fijar las credenciales.
func proxyHTTPUp(t *testing.T, creds []Credential, ajustar func(*Proxy)) (*Proxy, *httptest.Server, string) {
	t.Helper()
	audit := filepath.Join(t.TempDir(), "credaudit.jsonl")
	p := New(Options{Transport: transporteProhibido{t}, AuditPath: audit})
	if ajustar != nil {
		ajustar(p)
	}
	if _, err := p.SetCredentials(creds); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv, audit
}

func pedirHTTPUp(t *testing.T, srv *httptest.Server, method, ruta string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+ruta, strings.NewReader(`{"model":"m"}`))
	req.Host = httpUpDominio
	req.Header.Set("Authorization", "Bearer "+httpUpMarca)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// La petición permitida sale a la dirección fijada con la clave, verificando
// el certificado contra TLSServerName y con el Host de la credencial; la
// respuesta que la devuelve en eco llega con el marcador; una ruta que Allow
// no cubre no sale; el registro dice a dónde fue. Sin TLSServerName el mismo
// servidor no vale: su certificado no es del dominio de la credencial, y que
// valga para la IP del upstream no cuenta.
func TestCredencialHTTPUpstream(t *testing.T) {
	var hits atomic.Int32
	var vistoAuth, vistoHost atomic.Value
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		vistoAuth.Store(r.Header.Get("Authorization"))
		vistoHost.Store(r.Host)
		fmt.Fprintf(w, `{"eco":%q}`, r.Header.Get("Authorization"))
	}))
	defer up.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}))
	dir := up.Listener.Addr().String()

	c := credHTTPUp(dir)
	c.TLSServerName, c.CAPEM = "example.com", ca
	p, srv, audit := proxyHTTPUp(t, []Credential{c}, nil)

	code, cuerpo := pedirHTTPUp(t, srv, "POST", "/v1/chat/completions")
	if code != http.StatusOK {
		t.Fatalf("POST permitido: %d %s", code, cuerpo)
	}
	if got, _ := vistoAuth.Load().(string); got != "Bearer "+httpUpClave {
		t.Fatalf("el upstream recibió Authorization %q", got)
	}
	if got, _ := vistoHost.Load().(string); got != httpUpDominio {
		t.Errorf("el upstream recibió Host %q, want %q", got, httpUpDominio)
	}
	if strings.Contains(cuerpo, httpUpClave) || !strings.Contains(cuerpo, httpUpMarca) {
		t.Fatalf("el eco no se redactó: %s", cuerpo)
	}
	if code, _ := pedirHTTPUp(t, srv, "GET", "/v1/models"); code != http.StatusForbidden || hits.Load() != 1 {
		t.Fatalf("ruta fuera de Allow: %d, %d peticiones al upstream", code, hits.Load())
	}
	recs, crudo := leerRegistro(t, p, audit)
	if len(recs) != 2 || recs[0].Upstream != dir || recs[0].Status != http.StatusOK || recs[1].Reason != ReasonNotAllowed {
		t.Fatalf("registro %+v", recs)
	}
	if strings.Contains(crudo, httpUpClave) || strings.Contains(crudo, httpUpMarca) {
		t.Fatal("el registro lleva la clave o el marcador")
	}

	// Verificación contra el dominio de la credencial: el certificado de
	// httptest vale para example.com y 127.0.0.1, no para l3.kindling.internal.
	sin := credHTTPUp(dir)
	sin.CAPEM = ca
	_, srv2, _ := proxyHTTPUp(t, []Credential{sin}, nil)
	antes := hits.Load()
	if code, cuerpo := pedirHTTPUp(t, srv2, "POST", "/v1/chat/completions"); code != http.StatusBadGateway || hits.Load() != antes {
		t.Fatalf("sin TLSServerName: %d %s (el upstream vio %d peticiones más)", code, cuerpo, hits.Load()-antes)
	}

	// Sin la CA propia, las raíces del sistema no conocen el certificado.
	sinCA := credHTTPUp(dir)
	sinCA.TLSServerName = "example.com"
	_, srv3, _ := proxyHTTPUp(t, []Credential{sinCA}, nil)
	if code, _ := pedirHTTPUp(t, srv3, "POST", "/v1/chat/completions"); code != http.StatusBadGateway || hits.Load() != antes {
		t.Fatalf("sin CA: %d", code)
	}
}

// Con upstream, Allow es obligatorio: el proxy es un oráculo de la clave y,
// hacia un servicio del host o de la VPC, lo único que acota qué se hace con
// ella es la lista de peticiones. Vale para la validación del proxy y para la
// del manager (ValidarTipo, con lo que llega de la API).
func TestCredencialHTTPUpstreamSinAllowRechaza(t *testing.T) {
	c := credHTTPUp("127.0.0.1:8080")
	c.Allow = nil
	if err := ValidarCredenciales([]Credential{c}); err == nil || !strings.Contains(err.Error(), "allow-request") {
		t.Fatalf("sin Allow: %v", err)
	}
	if err := ValidarTipo(&c); err == nil {
		t.Fatal("ValidarTipo aceptó un upstream HTTP sin Allow")
	}
	if _, err := New(Options{}).SetCredentials([]Credential{c}); err == nil {
		t.Fatal("SetCredentials aceptó un upstream HTTP sin Allow")
	}
	// Los campos del upstream sin upstream no significan nada en HTTP.
	for nombre, mod := range map[string]func(*Credential){
		"tls sin upstream":    func(c *Credential) { c.UpstreamTLS = "verify-full" },
		"nombre sin upstream": func(c *Credential) { c.TLSServerName = "gw.lan" },
		"CA sin upstream":     func(c *Credential) { c.CAPEM = "x" },
		"máquina":             func(c *Credential) { c.UpstreamMachine = "abcdef0123" },
		"puerto":              func(c *Credential) { c.Port = 8080 },
	} {
		c := credHTTPUp("")
		mod(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: aceptada", nombre)
		}
	}
	// Dos credenciales del mismo dominio van al mismo sitio: el destino es
	// del dominio, no de cada clave.
	a := credHTTPUp("127.0.0.1:8080")
	b := Credential{Env: "ORG", Domain: httpUpDominio, Placeholder: testPlace2, Secret: testSecret2, Allow: []string{httpUpRegla}}
	if err := ValidarCredenciales([]Credential{a, b}); err == nil {
		t.Error("un dominio con y sin upstream: aceptado")
	}
	b.Upstream = "127.0.0.1:8081"
	if err := ValidarCredenciales([]Credential{a, b}); err == nil {
		t.Error("un dominio con dos upstreams: aceptado")
	}
	b.Upstream = "127.0.0.1:8080"
	if err := ValidarCredenciales([]Credential{a, b}); err != nil {
		t.Errorf("un dominio, mismo upstream: %v", err)
	}
	// Bien formada, se normaliza como la de Postgres.
	ok := []Credential{credHTTPUp("LocalHost:8080")}
	ok[0].UpstreamTLS = "VERIFY-FULL"
	if err := ValidarCredenciales(ok); err != nil || ok[0].Upstream != "localhost:8080" || ok[0].UpstreamTLS != "" {
		t.Fatalf("normalizada a %q %q: %v", ok[0].Upstream, ok[0].UpstreamTLS, err)
	}
}

// Los mismos vetos que un upstream de Postgres: metadatos y link-local, la red
// interna de kindling y el rango de reenvíos del loopback, al validar y al
// marcar (un nombre que resuelve ahí no se marca).
func TestUpstreamHTTPVetados(t *testing.T) {
	for _, up := range []string{
		"169.254.169.254:80", "[::ffff:169.254.169.254]:80", "[fd00:ec2::254]:80", "[fe80::1]:80",
		"172.16.0.1:8080", "172.30.0.1:8080", "0.0.0.0:8080", "224.0.0.1:80", "255.255.255.255:80",
		fmt.Sprintf("127.0.0.1:%d", ForwardPortMin), fmt.Sprintf("localhost:%d", ForwardPortMax),
		fmt.Sprintf("[::1]:%d", ForwardPortMin+1),
	} {
		c := credHTTPUp(up)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: aceptado", up)
		}
	}

	respuestas := map[string][]netip.Addr{
		"metadatos.lan": {netip.MustParseAddr("169.254.169.254")},
		"mixto.lan":     {netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("169.254.169.254")},
		"interno.lan":   {netip.MustParseAddr("172.30.0.2")},
		"bucle.lan":     {netip.MustParseAddr("127.0.0.1")},
	}
	for _, up := range []string{"metadatos.lan:80", "mixto.lan:80", "interno.lan:8080", fmt.Sprintf("bucle.lan:%d", ForwardPortMin)} {
		var resueltos atomic.Int32
		p, srv, audit := proxyHTTPUp(t, []Credential{credHTTPUp(up)}, func(p *Proxy) {
			p.lookupUp = func(_ context.Context, host string) ([]netip.Addr, error) {
				resueltos.Add(1)
				if ips, ok := respuestas[host]; ok {
					return ips, nil
				}
				return nil, errors.New("no such host")
			}
		})
		code, cuerpo := pedirHTTPUp(t, srv, "POST", "/v1/chat/completions")
		if code != http.StatusBadGateway || resueltos.Load() == 0 {
			t.Errorf("%s: %d %s (resoluciones %d)", up, code, cuerpo, resueltos.Load())
		}
		if strings.Contains(cuerpo, httpUpClave) {
			t.Errorf("%s: el error lleva la clave", up)
		}
		if recs, _ := leerRegistro(t, p, audit); len(recs) != 1 || recs[0].Reason != ReasonUpstreamError {
			t.Errorf("%s: registro %+v", up, recs)
		}
	}
}

// upstream_tls disable solo hacia el loopback del host (el gateway, que corre
// en la misma máquina): hacia la LAN o la VPC la clave cruzaría la red en
// claro, y aquí no hay SCRAM que la proteja. Un nombre tampoco vale: no se
// sabe al validar a dónde resolverá.
func TestUpstreamHTTPSinTLSSoloLoopback(t *testing.T) {
	for _, up := range []string{"10.0.0.5:8080", "192.168.1.10:80", "gw.lan:8080", "host.docker.internal:8080", "[fd12::5]:80"} {
		c := credHTTPUp(up)
		c.UpstreamTLS = UpstreamTLSDisable
		if err := ValidarCredenciales([]Credential{c}); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("disable hacia %s: %v", up, err)
		}
	}
	for nombre, mod := range map[string]func(*Credential){
		"con nombre TLS": func(c *Credential) { c.TLSServerName = "gw.lan" },
		"con CA":         func(c *Credential) { c.CAPEM = "x" },
	} {
		c := credHTTPUp("127.0.0.1:8080")
		c.UpstreamTLS = UpstreamTLSDisable
		mod(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("disable %s: aceptado", nombre)
		}
	}
	for _, up := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.2:8080"} {
		c := []Credential{credHTTPUp(up)}
		c[0].UpstreamTLS = "Disable"
		if err := ValidarCredenciales(c); err != nil || c[0].UpstreamTLS != UpstreamTLSDisable {
			t.Errorf("disable hacia %s: %v (%q)", up, err, c[0].UpstreamTLS)
		}
	}

	// Y funciona: el gateway en claro en el loopback recibe la clave.
	var vista atomic.Value
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vista.Store(r.Header.Get("Authorization"))
		io.WriteString(w, "ok")
	}))
	defer gw.Close()
	c := credHTTPUp(gw.Listener.Addr().String())
	c.UpstreamTLS = UpstreamTLSDisable
	_, srv, _ := proxyHTTPUp(t, []Credential{c}, nil)
	if code, cuerpo := pedirHTTPUp(t, srv, "POST", "/v1/chat/completions"); code != http.StatusOK || cuerpo != "ok" {
		t.Fatalf("hacia el gateway en claro: %d %s", code, cuerpo)
	}
	if got, _ := vista.Load().(string); got != "Bearer "+httpUpClave {
		t.Fatalf("el gateway recibió %q", got)
	}
}
