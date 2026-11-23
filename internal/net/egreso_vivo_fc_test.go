//go:build !darwin

package net

import (
	"errors"
	stdnet "net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRed instala los falsos de lo que lanza procesos y recuerda qué se pidió.
type fakeRed struct {
	mu       sync.Mutex
	presente map[string]bool // reglas "vivas" (clave de comoBorrado)
	restores []string
	cmds     []string
	forward  []string // cadena FORWARD de filter, en orden (clave de comoBorrado)
}

// aplicarForward aplica a la cadena FORWARD de filter lo que dice una entrada de
// iptables-restore (-A al final, -I n en su posición, -D la quita), como lo
// haría el kernel: el orden importa, la primera regla que casa gana.
func aplicarForward(cadena []string, entrada string) []string {
	enFilter := false
	for _, l := range strings.Split(entrada, "\n") {
		campos := strings.Fields(l)
		switch {
		case len(campos) == 0:
		case strings.HasPrefix(l, "*"):
			enFilter = l == "*filter"
		case !enFilter || len(campos) < 2 || campos[1] != "FORWARD":
		case campos[0] == "-A":
			cadena = append(cadena, strings.Join(campos[1:], " "))
		case campos[0] == "-I":
			pos, resto := 1, campos[1:]
			if p, err := strconv.Atoi(campos[2]); err == nil {
				pos, resto = p, append([]string{campos[1]}, campos[3:]...)
			}
			cadena = slices.Insert(cadena, min(pos-1, len(cadena)), strings.Join(resto, " "))
		case campos[0] == "-D":
			k := strings.Join(campos[1:], " ")
			if i := slices.Index(cadena, k); i >= 0 {
				cadena = slices.Delete(cadena, i, i+1)
			}
		}
	}
	return cadena
}

// posicionForward es el índice de la primera regla de FORWARD que contiene sub.
func (f *fakeRed) posicionForward(sub string) int {
	return slices.IndexFunc(f.forward, func(k string) bool {
		return strings.Contains(k, sub)
	})
}

func nuevoFakeRed(t *testing.T, n *Net, modo Egress) *fakeRed {
	t.Helper()
	f := &fakeRed{presente: map[string]bool{}}
	for _, r := range n.reglasDeModo(modo) {
		f.presente[claveRegla(comoBorrado(r))] = true
	}
	f.forward = aplicarForward(nil, restoreInput(n.reglasDeModo(modo)))
	oldE, oldR, oldP, oldS, oldRes, oldC, oldI := ejecutar, ejecutarRestore, reglaPresente, semillasDe, hayRestore, hayConntrack, hayIpset
	t.Cleanup(func() {
		ejecutar, ejecutarRestore, reglaPresente, semillasDe, hayRestore, hayConntrack, hayIpset = oldE, oldR, oldP, oldS, oldRes, oldC, oldI
		stopResolver(n.NS)
	})
	hayIpset = func() error { return nil }
	hayRestore = func() bool { return true }
	hayConntrack = func() bool { return true }
	semillasDe = func([]string) []string { return nil }
	ejecutar = func(args ...string) error {
		f.mu.Lock()
		f.cmds = append(f.cmds, strings.Join(args, " "))
		f.mu.Unlock()
		return nil
	}
	reglaPresente = func(_ string, r []string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c := comoBorrado(r); len(c) >= 2 && c[0] == "-D" && c[1] == "FORWARD" {
			// FORWARD de filter: lo que de verdad hay, en orden (los cambios
			// que aplicaron los restore anteriores incluidos).
			return slices.Contains(f.forward, strings.Join(c[1:], " "))
		}
		return f.presente[claveRegla(comoBorrado(r))]
	}
	ejecutarRestore = func(_, entrada string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.restores = append(f.restores, entrada)
		f.forward = aplicarForward(f.forward, entrada)
		return nil, nil
	}
	return f
}

func (f *fakeRed) vio(sub string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func redDePrueba() *Net {
	n := Plan(7, "aabbccdd-0000")
	// El resolver de verdad escucha ahí. No en 127.0.0.1: go test ./... corre
	// paquetes a la vez y otro puede tener un resolver en 127.0.0.1:5333 (el
	// puerto es fijo); en Linux todo 127.0.0.0/8 es loopback.
	n.HostIP = "127.0.83.33"
	return n
}

func TestComoBorrado(t *testing.T) {
	cases := map[string]string{
		"-A FORWARD -i tap0 -j DROP":               "-D FORWARD -i tap0 -j DROP",
		"-I FORWARD 1 -i tap0 -j ACCEPT":           "-D FORWARD -i tap0 -j ACCEPT",
		"-t nat -A POSTROUTING -o x -j MASQUERADE": "-t nat -D POSTROUTING -o x -j MASQUERADE",
	}
	for in, want := range cases {
		if got := strings.Join(comoBorrado(strings.Fields(in)), " "); got != want {
			t.Errorf("comoBorrado(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiferenciaDeReglasEntreModos(t *testing.T) {
	n := redDePrueba()
	for _, par := range [][2]Egress{
		{EgressNone, EgressAllowlist}, {EgressAllowlist, EgressNone}, {EgressInternet, EgressAllowlist},
		{EgressAllowlist, EgressInternet}, {EgressNone, EgressInternet}, {EgressInternet, EgressNone},
	} {
		borrar, anadir := diferenciaDeReglas(n.reglasDeModo(par[0]), n.reglasDeModo(par[1]))
		// Lo común (los topes de mangle) no se toca nunca.
		for _, r := range append(slices.Clone(borrar), anadir...) {
			if slices.Contains(r, "mangle") {
				t.Errorf("%v: no debería tocar %v", par, r)
			}
		}
		if len(borrar) == 0 || len(anadir) == 0 {
			t.Errorf("%v: borrar=%d anadir=%d", par, len(borrar), len(anadir))
		}
	}
	// Mismo modo: nada.
	b, a := diferenciaDeReglas(n.reglasDeModo(EgressInternet), n.reglasDeModo(EgressInternet))
	if len(b)+len(a) != 0 {
		t.Fatalf("mismo modo: %v %v", b, a)
	}
}

func TestEstrecha(t *testing.T) {
	cases := []struct {
		v    Egress
		vd   []string
		n    Egress
		nd   []string
		want bool
	}{
		{EgressInternet, nil, EgressNone, nil, true},
		{EgressAllowlist, []string{"a.com"}, EgressNone, nil, true},
		{EgressInternet, nil, EgressAllowlist, []string{"a.com"}, true},
		{EgressNone, nil, EgressAllowlist, []string{"a.com"}, false},
		{EgressNone, nil, EgressInternet, nil, false},
		{EgressAllowlist, []string{"a.com"}, EgressInternet, nil, false},
		{EgressAllowlist, []string{"a.com", "b.com"}, EgressAllowlist, []string{"a.com"}, true},
		{EgressAllowlist, []string{"a.com"}, EgressAllowlist, []string{"a.com", "b.com"}, false},
		{EgressAllowlist, []string{"x.a.com"}, EgressAllowlist, []string{"a.com"}, false}, // a.com cubre x.a.com
		{EgressAllowlist, []string{"a.com"}, EgressAllowlist, []string{"x.a.com"}, true},
	}
	for i, c := range cases {
		if got := estrecha(c.v, c.vd, c.n, c.nd); got != c.want {
			t.Errorf("caso %d: estrecha = %v, want %v", i, got, c.want)
		}
	}
}

func TestCambiarEgresoNoneAAllowlist(t *testing.T) {
	n := redDePrueba()
	f := nuevoFakeRed(t, n, EgressNone)
	res, err := n.CambiarEgreso(EgressNone, nil, EgressAllowlist, []string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Estrecha || res.ConexionesCortadas {
		t.Fatalf("none -> allowlist abre: %+v", res)
	}
	if len(f.restores) != 1 {
		t.Fatalf("restores = %d, quería una transacción", len(f.restores))
	}
	tx := f.restores[0]
	for _, quiero := range []string{"*filter", "-D FORWARD", "--match-set " + n.setName(), "-A FORWARD"} {
		if !strings.Contains(tx, quiero) {
			t.Errorf("la transacción no tiene %q:\n%s", quiero, tx)
		}
	}
	if strings.Contains(tx, "mangle") {
		t.Errorf("tocó los topes comunes:\n%s", tx)
	}
	if !f.vio("ipset create " + n.setName()) {
		t.Errorf("no creó el ipset: %v", f.cmds)
	}
	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r == nil || r.modo != modoAllowlist || !r.isAllowed("api.example.com") || r.isAllowed("otro.com") {
		t.Fatalf("resolver mal: %+v", r)
	}
}

func TestCambiarEgresoAllowlistANoneCierraConexiones(t *testing.T) {
	n := redDePrueba()
	f := nuevoFakeRed(t, n, EgressAllowlist)
	if err := startDNSResolver(n, []string{"api.example.com"}); err != nil {
		t.Fatal(err)
	}
	res, err := n.CambiarEgreso(EgressAllowlist, []string{"api.example.com"}, EgressNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Estrecha || !res.ConexionesCortadas || !f.vio("conntrack -D --orig-src "+GuestIP) {
		t.Fatalf("res=%+v cmds=%v", res, f.cmds)
	}
	// B1: también las entradas del host con el NSIP de origen o destino.
	if !f.vio("conntrack -D --orig-dst "+n.NSIP) || !f.vio("conntrack -D --orig-src "+n.NSIP) {
		t.Errorf("faltan los borrados del host: %v", f.cmds)
	}
	if !f.vio("ipset destroy") {
		t.Errorf("el ipset debía borrarse: %v", f.cmds)
	}
	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r == nil || r.modo != modoSoloGrafo {
		t.Fatalf("el resolver de none no arrancó: %+v", r)
	}
}

func TestCambiarEgresoSinConntrackSoloAfectaALasNuevas(t *testing.T) {
	n := redDePrueba()
	f := nuevoFakeRed(t, n, EgressInternet)
	hayConntrack = func() bool { return false }
	res, err := n.CambiarEgreso(EgressInternet, nil, EgressNone, nil)
	if err != nil || !res.Estrecha || res.ConexionesCortadas || f.vio("conntrack") {
		t.Fatalf("res=%+v err=%v cmds=%v", res, err, f.cmds)
	}
}

func TestCambiarEgresoMismaAllowlistVaciaElSetSoloSiEstrecha(t *testing.T) {
	n := redDePrueba()
	f := nuevoFakeRed(t, n, EgressAllowlist)
	if err := startDNSResolver(n, []string{"a.com", "b.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.CambiarEgreso(EgressAllowlist, []string{"a.com", "b.com"}, EgressAllowlist, []string{"a.com", "b.com", "c.com"}); err != nil {
		t.Fatal(err)
	}
	if f.vio("ipset flush") || len(f.restores) != 0 {
		t.Fatalf("ampliar no debía vaciar ni tocar reglas: %v %v", f.cmds, f.restores)
	}
	res, err := n.CambiarEgreso(EgressAllowlist, []string{"a.com", "b.com", "c.com"}, EgressAllowlist, []string{"a.com"})
	if err != nil || !res.Estrecha || !f.vio("ipset flush "+n.setName()) || !f.vio("conntrack -D") {
		t.Fatalf("res=%+v err=%v cmds=%v", res, err, f.cmds)
	}
	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r.isAllowed("b.com") || !r.isAllowed("www.a.com") {
		t.Fatal("la lista del resolver no cambió")
	}
}

func TestCambiarEgresoErrores(t *testing.T) {
	n := redDePrueba()
	nuevoFakeRed(t, n, EgressNone)
	if _, err := n.CambiarEgreso(EgressNone, nil, EgressAllowlist, nil); err == nil {
		t.Error("allowlist sin dominios debía fallar")
	}
	hayRestore = func() bool { return false }
	if _, err := n.CambiarEgreso(EgressNone, nil, EgressInternet, nil); err != ErrSinRestore {
		t.Errorf("sin iptables-restore: %v", err)
	}
	hayRestore = func() bool { return true }
	ejecutarRestore = func(_, _ string) ([]byte, error) { return []byte("boom"), errFalso }
	if _, err := n.CambiarEgreso(EgressNone, nil, EgressInternet, nil); err == nil {
		t.Error("un restore fallido debía devolver error")
	}
}

type errFalsoT struct{}

func (errFalsoT) Error() string { return "exit status 2" }

var errFalso error = errFalsoT{}

// consultaDNS manda una consulta A de name al resolver de n por UDP y devuelve
// el rcode de la respuesta.
func consultaDNS(t *testing.T, n *Net, name string) int {
	t.Helper()
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	c, err := stdnet.Dial("udp4", stdnet.JoinHostPort(n.HostIP, strconv.Itoa(dnsPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	_, err = c.Read(buf)
	if err != nil {
		t.Fatalf("consulta de %s sin respuesta: %v", name, err)
	}
	return int(buf[3] & 0x0f)
}

// Allowlist -> none -> allowlist (y arrancar en none): el resolver de la
// allowlist tiene que volver a contestar por los dominios permitidos (rcode
// distinto de NXDOMAIN/REFUSED; el upstream real no existe aquí, así que lo
// permitido acaba en SERVFAIL o en respuesta) y seguir rechazando el resto.
func TestCambiarEgresoVueltaAAllowlistResuelve(t *testing.T) {
	for _, inicio := range []Egress{EgressAllowlist, EgressNone} {
		t.Run(string(inicio), func(t *testing.T) {
			n := redDePrueba()
			f := nuevoFakeRed(t, n, inicio)
			// Antes de que el cleanup del falso restaure ejecutar: las
			// consultas siembran el ipset en goroutines del resolver.
			t.Cleanup(func() { stopResolver(n.NS) })
			dom := []string{"example.com"}
			// Upstream de juguete: sin él la respuesta depende de la red de la
			// máquina que corre el test.
			addr, cierra := fakeUpstream(t, func(q []byte) []byte { return buildAnswerFor(q, "93.184.216.34", 60) })
			defer cierra()
			viejoUp := upstreamDefecto
			upstreamDefecto = addr
			defer func() { upstreamDefecto = viejoUp }()
			var viejoDom []string
			if inicio == EgressAllowlist {
				viejoDom = dom
				if err := startDNSResolver(n, dom); err != nil {
					t.Fatal(err)
				}
			} else if err := n.arrancarRegistroNone(EgressNone); err != nil {
				t.Fatal(err)
			}
			if _, err := n.CambiarEgreso(inicio, viejoDom, EgressNone, nil); err != nil {
				t.Fatal(err)
			}
			if rc := consultaDNS(t, n, "example.com"); rc != 3 {
				t.Fatalf("en none example.com rcode = %d, quería NXDOMAIN", rc)
			}
			if _, err := n.CambiarEgreso(EgressNone, nil, EgressAllowlist, dom); err != nil {
				t.Fatal(err)
			}
			// El DNS y el ipset tienen que quedar ANTES del DROP final de FORWARD.
			cierre := f.posicionForward("-i tap0 -j DROP")
			for _, delante := range []string{"--dport 5333 -j ACCEPT", "--match-set " + n.setName()} {
				if p := f.posicionForward(delante); p < 0 || cierre < 0 || p > cierre {
					t.Fatalf("%q en posición %d y el DROP final en %d: queda detrás y nunca casa\n%q", delante, p, cierre, f.forward)
				}
			}
			if rc := consultaDNS(t, n, "example.com"); rc != 0 {
				t.Fatalf("tras volver a allowlist example.com rcode = %d, quería 0 (resuelto)", rc)
			}
			if rc := consultaDNS(t, n, "otro.org"); rc != 5 {
				t.Fatalf("otro.org rcode = %d, quería REFUSED", rc)
			}
		})
	}
}

// Un cambio de egress en caliente para el resolver y abre otro en el mismo
// puerto acto seguido: si el puerto aún está ocupado un instante, el arranque
// espera en vez de fallar con «address already in use».
func TestResolverEsperaAlPuertoRecienSoltado(t *testing.T) {
	n := redDePrueba()
	n.NS = "kl-test-rebind"
	n.HostIP = "127.0.83.34"
	ocupa, err := stdnet.ListenUDP("udp4", &stdnet.UDPAddr{IP: stdnet.ParseIP(n.HostIP), Port: dnsPort})
	if err != nil {
		t.Skipf("no puedo ocupar %s:%d: %v", n.HostIP, dnsPort, err)
	}
	go func() { time.Sleep(150 * time.Millisecond); ocupa.Close() }()
	t.Cleanup(func() { stopResolver(n.NS) })
	if err := startDNSResolver(n, []string{"a.com"}); err != nil {
		t.Fatalf("el resolver no esperó al puerto: %v", err)
	}
}

// B1: si falla el borrado del host, ConexionesCortadas no se promete.
func TestCortarConexionesFalloEnElHostNoPrometeCorte(t *testing.T) {
	n := redDePrueba()
	f := nuevoFakeRed(t, n, EgressInternet)
	base := ejecutar
	ejecutar = func(args ...string) error {
		_ = base(args...)
		if strings.Contains(strings.Join(args, " "), "--orig-dst "+n.NSIP) {
			return errors.New("boom")
		}
		return nil
	}
	if n.cortarConexiones() {
		t.Fatalf("no debía prometer el corte: %v", f.cmds)
	}
}

// B1: Teardown borra también el conntrack del host (la máquina que reuse el
// índice no hereda flujos).
func TestTeardownBorraConntrackDelHost(t *testing.T) {
	n := redDePrueba()
	f := nuevoFakeRed(t, n, EgressNone)
	n.Teardown()
	if !f.vio("conntrack -D --orig-dst "+n.NSIP) || !f.vio("conntrack -D --orig-src "+n.NSIP) {
		t.Fatalf("cmds=%v", f.cmds)
	}
}
