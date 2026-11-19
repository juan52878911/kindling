//go:build !darwin

package net

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeRed instala los falsos de lo que lanza procesos y recuerda qué se pidió.
type fakeRed struct {
	mu       sync.Mutex
	presente map[string]bool // reglas "vivas" (clave de comoBorrado)
	restores []string
	cmds     []string
}

func nuevoFakeRed(t *testing.T, n *Net, modo Egress) *fakeRed {
	t.Helper()
	f := &fakeRed{presente: map[string]bool{}}
	for _, r := range n.reglasDeModo(modo) {
		f.presente[claveRegla(comoBorrado(r))] = true
	}
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
		return f.presente[claveRegla(comoBorrado(r))]
	}
	ejecutarRestore = func(_, entrada string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.restores = append(f.restores, entrada)
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
	n.HostIP = "127.0.0.1" // el resolver de verdad escucha ahí
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
