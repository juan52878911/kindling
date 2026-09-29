package credproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	maqID    = "0123456789abcdef"
	maqOwner = "local"
)

// resolvedorFalso hace de daemon: la máquina maqID está disponible mientras
// vivo sea true y devuelve la dirección del servidor falso. Cuenta cuántas
// veces se le pregunta.
type resolvedorFalso struct {
	mu     sync.Mutex
	vivo   bool
	addr   string
	llamas atomic.Int32
	// visto es lo último que se preguntó.
	id, owner string
	port      int
}

func (r *resolvedorFalso) resolver(id, owner string, port int) (string, error) {
	r.llamas.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.id, r.owner, r.port = id, owner, port
	if !r.vivo || id != maqID {
		return "", errors.New("machine is frozen")
	}
	return r.addr, nil
}

func (r *resolvedorFalso) poner(vivo bool) {
	r.mu.Lock()
	r.vivo = vivo
	r.mu.Unlock()
}

// credMaquina convierte la credencial del entorno de pruebas en una hacia la
// máquina maqID.
func credMaquina(c *Credential) {
	c.UpstreamMachine, c.UpstreamOwner, c.UpstreamTLS, c.CAPEM = maqID, maqOwner, UpstreamTLSDisable, ""
}

// proxyMaquina monta el proxy con una credencial hacia maqID, un servidor
// SCRAM sin TLS y el resolvedor falso. destinoMaq se relaja: el servidor falso
// está en 127.0.0.1 fuera del rango reservado.
func proxyMaquina(t *testing.T) (*entornoPG, *servidorPG, *resolvedorFalso) {
	t.Helper()
	srv := nuevoServidorPG(t, "scram")
	srv.sinTLS = true
	r := &resolvedorFalso{vivo: true, addr: srv.ln.Addr().String()}
	e := proxyPG(t, srv, credMaquina,
		func(o *Options) { o.ResolveMachine = r.resolver },
		func(p *Proxy) { p.destinoMaq = func(netip.AddrPort) error { return nil } })
	return e, srv, r
}

// entrar hace login con el marcador y espera ReadyForQuery.
func entrar(t *testing.T, addr string) *clientePG {
	t.Helper()
	k := conectarPG(t, addr)
	if tipo, msg := k.login(pgMarca); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	k.hastaListo()
	return k
}

// cortada dice si la conexión del invitado se cerró (EOF) en poco tiempo.
func cortada(k *clientePG) bool {
	_ = k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := k.br.ReadByte()
	return errors.Is(err, io.EOF) || (err != nil && !isTimeout(err))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// La dirección se pide en CADA conexión: con la copia viva se entra; en
// cuanto el daemon dice que no (parada, congelada...), la siguiente falla sin
// marcar nada, y cuando vuelve, se entra otra vez.
func TestPGMaquinaResuelveEnCadaDial(t *testing.T) {
	e, srv, r := proxyMaquina(t)
	k := entrar(t, e.addr)
	k.c.Close()
	if r.id != maqID || r.owner != maqOwner || r.port != PGDefaultPort {
		t.Fatalf("resolvedor preguntado por %q %q %d", r.id, r.owner, r.port)
	}
	antes := srv.conns.Load()

	r.poner(false)
	k = conectarPG(t, e.addr)
	code, raw := k.esperarError(k.login(pgMarca))
	if code != "08006" || !strings.Contains(raw, "not available") {
		t.Fatalf("copia parada: %s %q", code, raw)
	}
	if srv.conns.Load() != antes {
		t.Fatal("con la copia no disponible el proxy no debe ni conectar")
	}

	r.poner(true)
	entrar(t, e.addr).c.Close()
	if n := r.llamas.Load(); n != 3 {
		t.Fatalf("el resolvedor se llamó %d veces, esperaba 3 (una por conexión)", n)
	}
	recs, _ := e.registro(t)
	var motivos []string
	for _, rec := range recs {
		motivos = append(motivos, rec.Reason)
		if rec.Upstream != "machine:"+maqID {
			t.Errorf("upstream auditado %q", rec.Upstream)
		}
	}
	if len(recs) != 3 || recs[1].Reason != ReasonMachineUnavailable || !recs[1].Denied {
		t.Fatalf("registro: %v", motivos)
	}
}

// Invalidar corta las sesiones vivas hacia ESA máquina, y solo esas.
func TestPGMaquinaInvalidar(t *testing.T) {
	e, _, _ := proxyMaquina(t)
	k := entrar(t, e.addr)
	if n := e.p.Invalidar("fedcba9876543210"); n != 0 {
		t.Fatalf("otra máquina cortó %d sesiones", n)
	}
	if cortada(k) {
		t.Fatal("la sesión se cortó invalidando otra máquina")
	}
	if n := e.p.Invalidar(maqID); n != 1 {
		t.Fatalf("Invalidar cortó %d sesiones, esperaba 1", n)
	}
	if !cortada(k) {
		t.Fatal("la sesión sigue viva tras Invalidar")
	}
	// Con "" se cortan todas.
	k = entrar(t, e.addr)
	if n := e.p.Invalidar(""); n != 1 || !cortada(k) {
		t.Fatalf("Invalidar(\"\") cortó %d", n)
	}
}

// Una invalidación que llega entre la resolución y el dial no deja pasar la
// sesión: se apuntó antes de resolver.
func TestPGMaquinaInvalidarDuranteElDial(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	srv.sinTLS = true
	var e *entornoPG
	resuelto := make(chan struct{})
	seguir := make(chan struct{})
	e = proxyPG(t, srv, credMaquina,
		func(o *Options) {
			o.ResolveMachine = func(string, string, int) (string, error) {
				close(resuelto)
				<-seguir
				return srv.ln.Addr().String(), nil
			}
		},
		func(p *Proxy) { p.destinoMaq = func(netip.AddrPort) error { return nil } })
	k := conectarPG(t, e.addr)
	go func() {
		<-resuelto
		e.p.Invalidar(maqID)
		close(seguir)
	}()
	k.arranque(0, "user", pgUser, "database", pgDB)
	k.leer()
	k.c.Write(mensajePG('p', append([]byte(pgMarca), 0)))
	_ = k.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		tipo, msg, err := leerMensaje(k.br, 1<<20)
		if err != nil {
			return // cortada: bien
		}
		if tipo == 'R' && binary.BigEndian.Uint32(msg) == 0 {
			t.Fatal("AuthenticationOk tras una invalidación durante el dial")
		}
	}
}

// Quitar la credencial (kling db detach) corta sus sesiones; rotar la clave
// de la misma variable (mismo marcador, misma máquina) no.
func TestPGMaquinaDetachCorta(t *testing.T) {
	e, _, _ := proxyMaquina(t)
	base := Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
		Kind: KindPostgres, User: pgUser, Database: pgDB}
	credMaquina(&base)
	k := entrar(t, e.addr)
	if _, err := e.p.SetCredentials([]Credential{base}); err != nil {
		t.Fatal(err)
	}
	if cortada(k) {
		t.Fatal("una rotación de la misma credencial no debe cortar la sesión")
	}
	otra := base
	otra.UpstreamMachine = "fedcba9876543210"
	if _, err := e.p.SetCredentials([]Credential{otra}); err != nil {
		t.Fatal(err)
	}
	if !cortada(k) {
		t.Fatal("cambiar la máquina de la credencial debe cortar la sesión")
	}
	if _, err := e.p.SetCredentials([]Credential{base}); err != nil {
		t.Fatal(err)
	}
	k = entrar(t, e.addr)
	if _, err := e.p.SetCredentials(nil); err != nil {
		t.Fatal(err)
	}
	if !cortada(k) {
		t.Fatal("quitar la credencial debe cortar la sesión")
	}
}

// Sin resolvedor (kling-vz) una credencial hacia una máquina no marca nunca,
// y una dirección resuelta que no es de kindling tampoco.
func TestPGMaquinaSinResolverYDestinoMalo(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	srv.sinTLS = true
	e := proxyPG(t, srv, credMaquina)
	k := conectarPG(t, e.addr)
	if code, _ := k.esperarError(k.login(pgMarca)); code != "08006" {
		t.Fatalf("sin resolvedor: %s", code)
	}

	var marcado atomic.Bool
	e = proxyPG(t, srv, credMaquina,
		func(o *Options) {
			o.ResolveMachine = func(string, string, int) (string, error) { return "169.254.169.254:5432", nil }
		},
		func(p *Proxy) {
			p.dialMaq = func(context.Context, string, string) (net.Conn, error) {
				marcado.Store(true)
				return nil, errors.New("no")
			}
		})
	k = conectarPG(t, e.addr)
	if code, _ := k.esperarError(k.login(pgMarca)); code != "08006" {
		t.Fatalf("destino malo: %s", code)
	}
	if marcado.Load() {
		t.Fatal("se marcó una dirección resuelta que no es de kindling")
	}
	if srv.conns.Load() != 0 {
		t.Fatal("el servidor recibió conexiones")
	}
}

func TestDestinoMaquinaValido(t *testing.T) {
	for addr, bien := range map[string]bool{
		"172.30.1.2:5432":          true,  // netns de Linux
		"127.0.0.1:29001":          true,  // reenvío del rango reservado (macOS)
		"[::ffff:172.30.0.6]:5432": true,  // IPv4 escrita como IPv6
		"127.0.0.1:5432":           false, // loopback fuera del rango
		"169.254.169.254:80":       false, // metadatos
		"10.0.0.1:5432":            false, // la LAN: un upstream fijado sí, esto no
		"172.16.0.2:5432":          false, // el enlace del invitado
		"[fe80::1%eth0]:5432":      false,
		"172.30.1.2:0":             false,
	} {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			if bien {
				t.Fatalf("%s: %v", addr, err)
			}
			continue
		}
		if got := destinoMaquinaValido(ap) == nil; got != bien {
			t.Errorf("%s: válido=%v, esperaba %v", addr, got, bien)
		}
	}
}

// UpstreamMachine es un ID, nunca una dirección, y lleva sus condiciones.
func TestValidarUpstreamMaquina(t *testing.T) {
	bien := func() Credential {
		c := Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
			Kind: KindPostgres, User: pgUser, Database: pgDB}
		credMaquina(&c)
		return c
	}
	c := bien()
	c.UpstreamTLS = "DISABLE"
	if err := ValidarCredenciales([]Credential{c}); err != nil {
		t.Fatalf("la buena: %v", err)
	}
	casos := map[string]func(*Credential){
		"una IP":            func(c *Credential) { c.UpstreamMachine = "172.30.0.2" },
		"ip:puerto":         func(c *Credential) { c.UpstreamMachine = "172.30.0.2:5432" },
		"loopback":          func(c *Credential) { c.UpstreamMachine = "127.0.0.1:29001" },
		"IPv6":              func(c *Credential) { c.UpstreamMachine = "::1" },
		"un nombre":         func(c *Credential) { c.UpstreamMachine = "copia-1" },
		"mayúsculas":        func(c *Credential) { c.UpstreamMachine = strings.ToUpper(maqID) },
		"con -upstream":     func(c *Credential) { c.Upstream = "127.0.0.1:55432" },
		"sin dueño":         func(c *Credential) { c.UpstreamOwner = "" },
		"dueño raro":        func(c *Credential) { c.UpstreamOwner = "Local/x" },
		"con TLS":           func(c *Credential) { c.UpstreamTLS = "" },
		"con CA":            func(c *Credential) { c.CAPEM = "x" },
		"con nombre TLS":    func(c *Credential) { c.TLSServerName = "db" },
		"cualquier base":    func(c *Credential) { c.Database, c.AnyDatabase = "", true },
		"dueño sin máquina": func(c *Credential) { c.UpstreamMachine = "" },
		"HTTP con máquina":  func(c *Credential) { c.Kind, c.User, c.Database, c.UpstreamTLS, c.Port = "", "", "", "", 0 },
	}
	for nombre, mod := range casos {
		c := bien()
		mod(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: aceptada", nombre)
		}
	}
}
