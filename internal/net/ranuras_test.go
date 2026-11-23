//go:build !darwin

package net

import (
	"fmt"
	"maps"
	stdnet "net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// procesos sustituye todo lo que lanza procesos y apunta cada uno.
type procesos struct {
	mu  sync.Mutex
	log []string
}

func (p *procesos) add(s string) { p.mu.Lock(); p.log = append(p.log, s); p.mu.Unlock() }
func (p *procesos) n() int       { p.mu.Lock(); defer p.mu.Unlock(); return len(p.log) }
func (p *procesos) vaciar()      { p.mu.Lock(); p.log = nil; p.mu.Unlock() }

func sustituirProcesos(t *testing.T) *procesos {
	t.Helper()
	oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA := ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost
	t.Cleanup(func() {
		ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost = oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA
	})
	p := &procesos{}
	ordenEnNetns = func(ns string, a ...string) error { p.add("en-netns " + strings.Join(a, " ")); return nil }
	ejecutar = func(a ...string) error { p.add(strings.Join(a, " ")); return nil }
	ipBatch = func(ns string, _ ...string) error { p.add("ip-batch " + ns); return nil }
	ejecutarRestore = func(ns, _ string) ([]byte, error) { p.add("iptables-restore " + ns); return nil, nil }
	hayRestore = func() bool { return true }
	hayIp6tables = func() bool { return true }
	existeRed = func(*Net) bool { return false }
	asegurarHost = func() error { return nil }
	return p
}

var contadorRanura atomic.Int64

// ranuraFalsa monta una ranura de verdad (con montarBase y los procesos
// sustituidos) con un id y un índice únicos.
func ranuraFalsa() (*Ranura, error) {
	i := int(contadorRanura.Add(1))
	id := fmt.Sprintf("%08x", i)
	n := Plan(i, id)
	if err := n.MontarRanura(1000); err != nil {
		return nil, err
	}
	return &Ranura{ID: id, Net: n}, nil
}

func esperarListas(t *testing.T, f *Fondo, n int) {
	t.Helper()
	for i := 0; i < 2000 && f.Len() < n; i++ {
		time.Sleep(time.Millisecond)
	}
	if f.Len() < n {
		t.Fatalf("el fondo tiene %d ranuras, quería %d", f.Len(), n)
	}
}

func TestTomarRanuraNoLanzaProcesosDeMontarBase(t *testing.T) {
	p := sustituirProcesos(t)
	r, err := ranuraFalsa()
	if err != nil {
		t.Fatal(err)
	}
	f := NuevoFondo(0, ranuraFalsa, func(*Ranura) {}) // tam 0: no repone
	f.listas = []*Ranura{r}
	p.vaciar()
	if got := f.Tomar(); got != r {
		t.Fatal("no entregó la ranura lista")
	}
	if got := f.Entregar(r.ID); got != r.Net {
		t.Fatal("Entregar no devuelve la red de la ranura")
	}
	if p.n() != 0 {
		t.Fatalf("tomar una ranura lanzó procesos: %q", p.log)
	}
}

func TestAplicarPropioLanzaComoMuchoDos(t *testing.T) {
	for _, vecino := range []bool{false, true} {
		p := sustituirProcesos(t)
		r, err := ranuraFalsa()
		if err != nil {
			t.Fatal(err)
		}
		p.vaciar()
		r.Net.VecinoInvitado = vecino
		if err := r.Net.Setup(EgressInternet, nil, 1000); err != nil {
			t.Fatal(err)
		}
		if p.n() > 2 {
			t.Errorf("vecino=%v: aplicarPropio lanzó %d procesos: %q", vecino, p.n(), p.log)
		}
		if vecino != strings.Contains(strings.Join(p.log, "|"), "nud stale") {
			t.Errorf("vecino=%v: %q", vecino, p.log)
		}
		for _, l := range p.log {
			if strings.Contains(l, "netns add") || strings.HasPrefix(l, "ip-batch") {
				t.Errorf("aplicarPropio repitió el montaje de la base: %q", l)
			}
		}
	}
}

func TestRanuraNoPasaDosVecesPorAplicarPropio(t *testing.T) {
	p := sustituirProcesos(t)
	r, _ := ranuraFalsa()
	if err := r.Net.Setup(EgressInternet, nil, 1000); err != nil {
		t.Fatal(err)
	}
	p.vaciar()
	// Un segundo Setup del mismo *Net ya no es una ranura: monta todo de nuevo.
	if err := r.Net.Setup(EgressInternet, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.log, "|"), "netns add") {
		t.Fatalf("el segundo Setup no remontó la base: %q", p.log)
	}
}

func TestRanuraDescartadaSeDestruyeYNoVuelve(t *testing.T) {
	sustituirProcesos(t)
	var destruidas sync.Map
	f := NuevoFondo(2, ranuraFalsa, func(r *Ranura) { destruidas.Store(r.ID, true) })
	f.Reponer()
	esperarListas(t, f, 2)
	r := f.Tomar()
	f.Descartar(r.ID)
	if _, ok := destruidas.Load(r.ID); !ok {
		t.Fatal("la ranura descartada no se destruyó")
	}
	if f.Entregar(r.ID) != nil {
		t.Fatal("una ranura descartada se pudo entregar")
	}
	esperarListas(t, f, 2)
	for i := 0; i < 2; i++ {
		if o := f.Tomar(); o == nil || o.ID == r.ID {
			t.Fatalf("la ranura descartada reapareció en el fondo: %v", o)
		}
	}
	f.Cerrar()
}

func TestTomasConcurrentesSinRepetidos(t *testing.T) {
	sustituirProcesos(t)
	f := NuevoFondo(16, ranuraFalsa, func(r *Ranura) {})
	f.Reponer()
	esperarListas(t, f, 16)
	var mu sync.Mutex
	ids, idx := map[string]bool{}, map[int]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.Tomar()
			if r == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ids[r.ID] || idx[r.Net.Index] {
				t.Errorf("ranura repetida: id %s índice %d", r.ID, r.Net.Index)
			}
			ids[r.ID], idx[r.Net.Index] = true, true
		}()
	}
	wg.Wait()
	f.Cerrar()
	if len(ids) < 16 {
		t.Errorf("solo %d tomas con ranura", len(ids))
	}
}

func TestRanuraTomadaYNoMontadaSeRecoge(t *testing.T) {
	sustituirProcesos(t)
	ahora := time.Now()
	var destruidas atomic.Int64
	f := NuevoFondo(1, ranuraFalsa, func(*Ranura) { destruidas.Add(1) })
	f.ahora = func() time.Time { return ahora }
	f.Reponer()
	esperarListas(t, f, 1)
	r := f.Tomar()
	if f.Recoger() != 0 {
		t.Fatal("recogió una ranura reciente")
	}
	ahora = ahora.Add(PlazoRanuraTomada + time.Second)
	if f.Recoger() != 1 || destruidas.Load() != 1 {
		t.Fatal("no recogió la ranura tomada y no montada en 60 s")
	}
	if f.Entregar(r.ID) != nil {
		t.Fatal("entregó una ranura ya recogida")
	}
	f.Cerrar()
}

func TestPropiaCubreEnEsperaYTomadas(t *testing.T) {
	sustituirProcesos(t)
	f := NuevoFondo(2, ranuraFalsa, func(*Ranura) {})
	f.Reponer()
	esperarListas(t, f, 2)
	r := f.Tomar()
	if !f.Propia(r.Net.NS) || !f.PrefijoEnUso(Corto(r.ID)) {
		t.Fatal("una ranura tomada debe seguir siendo del fondo")
	}
	if f.Propia("kl-ffffffff") {
		t.Fatal("un ns ajeno no es del fondo")
	}
	f.Cerrar()
}

func TestSetupSinRanuraConCuatroMontajesSimultaneos(t *testing.T) {
	sustituirProcesos(t)
	var enCurso, maximo atomic.Int64
	ipBatch = func(ns string, _ ...string) error {
		if ns == "" {
			v := enCurso.Add(1)
			for {
				m := maximo.Load()
				if v <= m || maximo.CompareAndSwap(m, v) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			enCurso.Add(-1)
		}
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := Plan(1000+i, fmt.Sprintf("%08x", 5000+i))
			if err := n.Setup(EgressInternet, nil, 1000); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if maximo.Load() > 4 {
		t.Fatalf("%d montajes completos a la vez; el tope es 4", maximo.Load())
	}
}

func TestRecogerConCerrojoOcupadoNoDestruye(t *testing.T) {
	sustituirProcesos(t)
	ahora := time.Now()
	var destruidas atomic.Int64
	f := NuevoFondo(1, ranuraFalsa, func(*Ranura) { destruidas.Add(1) })
	f.ahora = func() time.Time { return ahora }
	var ocupado atomic.Bool
	ocupado.Store(true)
	var soltadas atomic.Int64
	f.Bloquear = func(string) (func(), bool) {
		if ocupado.Load() {
			return nil, false
		}
		return func() { soltadas.Add(1) }, true
	}
	f.Reponer()
	esperarListas(t, f, 1)
	r := f.Tomar()
	ahora = ahora.Add(PlazoRanuraTomada + time.Second)
	if f.Recoger() != 0 || destruidas.Load() != 0 {
		t.Fatal("destruyó una ranura cuyo id tiene el cerrojo ocupado")
	}
	ocupado.Store(false)
	if f.Recoger() != 1 || destruidas.Load() != 1 || soltadas.Load() != 1 {
		t.Fatal("a la vuelta siguiente debía destruirla y soltar el cerrojo")
	}
	if f.Entregar(r.ID) != nil {
		t.Fatal("entregó una ranura recogida")
	}
	f.Cerrar()
}

func TestCerrarDestruyeTodasYParaLaRecogida(t *testing.T) {
	sustituirProcesos(t)
	var destruidas atomic.Int64
	f := NuevoFondo(3, ranuraFalsa, func(*Ranura) { destruidas.Add(1) })
	f.IniciarRecogida(time.Millisecond)
	f.Reponer()
	esperarListas(t, f, 3)
	f.Cerrar() // espera a la goroutine de recogida: si no acaba, el test se cuelga
	if destruidas.Load() != 3 {
		t.Fatalf("destruidas = %d, quería 3", destruidas.Load())
	}
	f.Cerrar() // idempotente
}

// Una ranura entregada con none ya tiene las reglas de none montadas (S5):
// aplicarPropio no lanza ningún iptables-restore.
func TestRanuraConNoneNoLanzaRestoreEnAplicarPropio(t *testing.T) {
	p := sustituirProcesos(t)
	r, err := ranuraFalsa()
	if err != nil {
		t.Fatal(err)
	}
	viejo := iniciarResolverNone
	t.Cleanup(func() { iniciarResolverNone = viejo })
	iniciarResolverNone = func(*Net) error { return nil }
	p.vaciar()
	if err := r.Net.Setup(EgressNone, nil, 1000); err != nil {
		t.Fatal(err)
	}
	for _, l := range p.log {
		if strings.Contains(l, "iptables") {
			t.Errorf("Setup en none con ranura lanzó %q", l)
		}
	}
}

// tablasDe aplica la entrada de varios iptables-restore --noflush (-A, -I, -D)
// y devuelve el conjunto final: "tabla/CADENA" → reglas en orden.
func tablasDe(entradas []string) map[string][]string {
	est := map[string][]string{}
	for _, e := range entradas {
		tabla := ""
		for _, l := range strings.Split(e, "\n") {
			f := strings.Fields(l)
			if len(f) == 0 || f[0] == "COMMIT" {
				continue
			}
			if strings.HasPrefix(f[0], "*") {
				tabla = f[0][1:]
				continue
			}
			if len(f) < 2 {
				continue
			}
			k := tabla + "/" + f[1]
			switch f[0] {
			case "-A":
				est[k] = append(est[k], strings.Join(f[2:], " "))
			case "-I":
				pos, resto := 1, f[2:]
				if p, err := strconv.Atoi(f[2]); err == nil {
					pos, resto = p, f[3:]
				}
				est[k] = slices.Insert(est[k], min(pos-1, len(est[k])), strings.Join(resto, " "))
			case "-D":
				if i := slices.Index(est[k], strings.Join(f[2:], " ")); i >= 0 {
					est[k] = slices.Delete(est[k], i, i+1)
				} else {
					est[k] = append(est[k], "!!-D sin regla: "+l)
				}
			}
		}
	}
	return est
}

// capturarRestores guarda la entrada de cada iptables-restore.
func capturarRestores(t *testing.T) *[]string {
	t.Helper()
	var entradas []string
	ejecutarRestore = func(_, e string) ([]byte, error) { entradas = append(entradas, e); return nil, nil }
	return &entradas
}

// Una ranura con none + lo que aplica aplicarSobreRanura deja EXACTAMENTE las
// reglas del montaje completo, en los tres modos, regla a regla y en orden.
func TestRanuraEquivaleAlMontajeCompleto(t *testing.T) {
	for _, modo := range []Egress{EgressNone, EgressInternet, EgressAllowlist} {
		t.Run(string(modo), func(t *testing.T) {
			p := sustituirProcesos(t)
			_ = p
			oldI, oldR, oldS, oldIp := iniciarResolverNone, hayIpset, semillasDe, ipsDelHost
			t.Cleanup(func() { iniciarResolverNone, hayIpset, semillasDe, ipsDelHost = oldI, oldR, oldS, oldIp })
			iniciarResolverNone = func(*Net) error { return nil }
			hayIpset = func() error { return nil }
			ipsDelHost = func() []stdnet.IP { return []stdnet.IP{stdnet.ParseIP("203.0.113.9"), stdnet.ParseIP("10.1.2.3")} }
			oldA := iniciarResolverAllowlist
			t.Cleanup(func() { iniciarResolverAllowlist = oldA })
			iniciarResolverAllowlist = func(*Net, []string) error { return nil }
			var dominios []string

			// Montaje completo: sin ranura.
			completo := capturarRestores(t)
			n1 := Plan(7, "abcd1234")
			if err := n1.Setup(modo, dominios, 1000); err != nil {
				t.Fatal(err)
			}
			// Ranura.
			ranura := capturarRestores(t)
			n2 := Plan(7, "abcd1234")
			if err := n2.MontarRanura(1000); err != nil {
				t.Fatal(err)
			}
			if err := n2.Setup(modo, dominios, 1000); err != nil {
				t.Fatal(err)
			}
			a, b := tablasDe(*completo), tablasDe(*ranura)
			if len(a["filter/FORWARD"]) < 6 || len(a["nat/PREROUTING"]) < 1 {
				t.Fatalf("el montaje completo no puso lo esperado: %q", a)
			}
			if !maps.EqualFunc(a, b, slices.Equal[[]string]) {
				for k := range a {
					if !slices.Equal(a[k], b[k]) {
						t.Errorf("%s difiere:\n completo: %q\n ranura:   %q", k, a[k], b[k])
					}
				}
				for k := range b {
					if _, ok := a[k]; !ok {
						t.Errorf("%s solo en la ranura: %q", k, b[k])
					}
				}
			}
			if modo == EgressNone && len(*ranura) != 1 {
				t.Errorf("none: %d restores en total con la ranura, quería 1 (el de MontarRanura)", len(*ranura))
			}
			if modo != EgressNone && len(*ranura) != 2 {
				t.Errorf("%s: %d restores con la ranura, quería 2 (montaje + una transacción)", modo, len(*ranura))
			}
		})
	}
}

// A-1 paso 4: con creaciones esperando hueco, un reponedor no lo toma.
func TestReponedorCedeElHuecoALasCreaciones(t *testing.T) {
	for range cap(montajesCompletos) {
		montajesCompletos <- struct{}{}
	}
	t.Cleanup(func() {
		for len(montajesCompletos) > 0 {
			<-montajesCompletos
		}
	})
	orden := make(chan string, 2)
	go func() { tomarMontaje(false); orden <- "reponedor" }()
	time.Sleep(20 * time.Millisecond) // el reponedor ya está esperando hueco
	go func() { tomarMontaje(true); orden <- "creacion" }()
	for creacionesEsperando.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	<-montajesCompletos // se libera UN hueco
	select {
	case quien := <-orden:
		if quien != "creacion" {
			t.Fatalf("el hueco lo tomó el %s con una creación esperando", quien)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nadie tomó el hueco")
	}
	select {
	case quien := <-orden:
		t.Fatalf("%s tomó hueco sin que se liberara otro", quien)
	case <-time.After(30 * time.Millisecond):
	}
	<-montajesCompletos // sin creaciones esperando, el reponedor pasa
	select {
	case <-orden:
	case <-time.After(2 * time.Second):
		t.Fatal("el reponedor no tomó el hueco al quedar libre")
	}
}

// Una ranura descartada (alta rechazada antes de la red) se destruye en el
// acto y deja sitio: el fondo se repone sin esperar al plazo de Recoger.
func TestDescartarDevuelveElHuecoYRepone(t *testing.T) {
	var i atomic.Int64
	var destruidas atomic.Int32
	f := NuevoFondo(2, func() (*Ranura, error) {
		k := int(i.Add(1))
		id := fmt.Sprintf("%08x00000000", 0xb0000000+k)
		return &Ranura{ID: id, Net: Plan(k, id)}, nil
	}, func(*Ranura) { destruidas.Add(1) })
	t.Cleanup(f.Cerrar)
	f.Reponer()
	for n := 0; n < 2000 && f.Len() < 2; n++ {
		time.Sleep(time.Millisecond)
	}
	r := f.Tomar()
	if r == nil {
		t.Fatal("sin ranura")
	}
	f.Descartar(r.ID)
	if destruidas.Load() != 1 || f.Entregar(r.ID) != nil || f.Propia(r.Net.NS) {
		t.Fatalf("la ranura descartada sigue viva (destruidas=%d)", destruidas.Load())
	}
	for n := 0; n < 2000 && f.Len() < 2; n++ {
		time.Sleep(time.Millisecond)
	}
	if f.Len() != 2 {
		t.Fatalf("el fondo no se repuso: %d", f.Len())
	}
	f.Descartar(r.ID) // idempotente
	if destruidas.Load() != 1 {
		t.Fatal("Descartar dos veces destruyó dos")
	}
}

// A-1 paso 5: el registro nft de none lo instala el reponedor (MontarRanura);
// crear la máquina en none no lo repite, y en otro modo lo retira.
func TestRegistroDeNoneVieneEnLaRanura(t *testing.T) {
	sustituirProcesos(t)
	viejo, viejoH := iniciarResolverNone, instalarRegistroHook
	t.Cleanup(func() { iniciarResolverNone, instalarRegistroHook = viejo, viejoH })
	iniciarResolverNone = func(*Net) error { return nil }
	var instalaciones atomic.Int32
	instalarRegistroHook = func(*Net) { instalaciones.Add(1) }

	r, err := ranuraFalsa()
	if err != nil {
		t.Fatal(err)
	}
	if instalaciones.Load() != 1 {
		t.Fatalf("MontarRanura instaló el registro %d veces, quería 1", instalaciones.Load())
	}
	if err := r.Net.Setup(EgressNone, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if instalaciones.Load() != 1 {
		t.Fatalf("Setup en none repitió el registro (%d instalaciones)", instalaciones.Load())
	}
	if _, queda := registroEnRanura.Load(r.Net.NS); queda {
		t.Fatal("la marca de registro de la ranura no se consumió")
	}

	// Otro modo: se retira la marca (y la tabla, en segundo plano).
	r2, err := ranuraFalsa()
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Net.Setup(EgressInternet, nil, 1000); err != nil {
		t.Fatal(err)
	}
	registrosEnVuelo.Wait()
	if _, queda := registroEnRanura.Load(r2.Net.NS); queda {
		t.Fatal("una ranura entregada en internet conserva el registro de none")
	}
	// Sin ranura, en none, se instala como siempre.
	n := Plan(900, "feedface00000009")
	if err := n.Setup(EgressNone, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if instalaciones.Load() != 3 { // r, r2 y el de n
		t.Fatalf("instalaciones = %d, quería 3", instalaciones.Load())
	}
}
