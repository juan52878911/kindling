package credproxy

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
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
	dial := dialFijado(lookup, &net.Dialer{Timeout: 2 * time.Second})
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
	canc := append([][2]uint32(nil), srv.cancelado...)
	srv.mu.Unlock()
	if len(canc) != 1 || canc[0] != [2]uint32{42, 0xdeadbeef} {
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
