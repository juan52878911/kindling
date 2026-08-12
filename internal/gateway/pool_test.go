package gateway

import (
	"context"
	"github.com/juan52878911/kindling/internal/api"
	"sync"
	"testing"
	"time"
)

// El pool real llama a warm(), que habla con el daemon. Para los tests se
// sustituye por un doble que cuenta lo que crea y lo que se retira, que es lo
// único que hace falta para saber si queda una microVM huérfana.
type fakeWarmer struct {
	mu      sync.Mutex
	created []string
	removed []string
	delay   time.Duration
	fail    bool
}

func (f *fakeWarmer) warm(ctx context.Context, service, snapshot string) (*warmVM, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := service + "-" + time.Now().Format("150405.000000000")
	f.created = append(f.created, id)
	return &warmVM{id: id, ip: "10.0.0.1", session: "s"}, nil
}

func (f *fakeWarmer) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
}

func (f *fakeWarmer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.removed)
}

// newTestPool arma un pool con el doble inyectado, sin gateway ni daemon.
func newTestPool(t *testing.T, size int, f *fakeWarmer) *pool {
	t.Helper()
	p := &pool{
		size:    size,
		ready:   map[string][]*warmVM{},
		filling: map[string]bool{},
	}
	p.warmFn = f.warm
	p.removeFn = func(ctx context.Context, id string) error { f.remove(id); return nil }
	return p
}

// El caso que el test de la rama no cubría: el fill empieza DESPUÉS de que
// drain haya mirado el contador. Con wg.Add fuera del mutex, drain veía cero,
// vaciaba `ready`, y esta microVM quedaba viva sin que nadie pudiera retirarla.
func TestDrainNoDejaHuerfanaLaQueLlegaTarde(t *testing.T) {
	f := &fakeWarmer{delay: 50 * time.Millisecond}
	p := newTestPool(t, 1, f)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Justo mientras drain está esperando.
		time.Sleep(10 * time.Millisecond)
		p.fill(context.WithoutCancel(context.Background()), "eco", "eco")
	}()

	p.drain(context.Background())
	wg.Wait()

	// Puede que el fill llegara antes o después del cierre; en cualquiera de los
	// dos casos, todo lo creado tiene que acabar retirado.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		created, removed := f.counts()
		if created == removed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	created, removed := f.counts()
	t.Errorf("quedaron microVMs huérfanas: creadas %d, retiradas %d", created, removed)
}

// Tras drain no debe pre-calentarse nada nuevo: el gateway se está apagando.
func TestFillNoArrancaTrasDrain(t *testing.T) {
	f := &fakeWarmer{}
	p := newTestPool(t, 2, f)
	p.drain(context.Background())

	p.fill(context.Background(), "eco", "eco")
	time.Sleep(100 * time.Millisecond)

	if created, _ := f.counts(); created != 0 {
		t.Errorf("pre-calentó %d instancias con el pool ya cerrado", created)
	}
}

// drain espera a las reposiciones en vuelo, para que se retiren antes de salir.
func TestDrainEsperaLoQueEstaEnVuelo(t *testing.T) {
	f := &fakeWarmer{delay: 80 * time.Millisecond}
	p := newTestPool(t, 1, f)
	p.fill(context.WithoutCancel(context.Background()), "eco", "eco")
	time.Sleep(10 * time.Millisecond) // que entre en warm

	start := time.Now()
	p.drain(context.Background())
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("drain volvió en %s: no esperó al fill en vuelo", elapsed)
	}
	if created, removed := f.counts(); created != removed {
		t.Errorf("creadas %d, retiradas %d", created, removed)
	}
}

// Y no espera para siempre: un warm colgado no puede secuestrar el apagado.
func TestDrainNoSeCuelgaConUnWarmEterno(t *testing.T) {
	f := &fakeWarmer{delay: time.Hour}
	p := newTestPool(t, 1, f)
	p.fill(context.WithoutCancel(context.Background()), "eco", "eco")
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { p.drain(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain se colgó esperando a un warm que no termina")
	}
}

// Un servicio no debe tener dos reposiciones a la vez: es lo que evita levantar
// veinte microVMs del mismo servicio ante una ráfaga.
func TestFillNoDuplicaReposicion(t *testing.T) {
	f := &fakeWarmer{delay: 40 * time.Millisecond}
	p := newTestPool(t, 3, f)
	for range 5 {
		p.fill(context.Background(), "eco", "eco")
	}
	time.Sleep(400 * time.Millisecond)

	if created, _ := f.counts(); created > 3 {
		t.Errorf("pre-calentó %d, más que el tamaño del fondo (3)", created)
	}
}

// Una llamada en vuelo impide que el segador congele su microVM.
//
// Este es el fallo que hacía parecer roto un servicio meramente lento: la
// inactividad se medía por la LLEGADA de peticiones, así que una herramienta
// que tardaba más que el plazo de inactividad se quedaba sin máquina a media
// faena. El cliente recibía un "connection timed out" de TCP —la conexión con
// el invitado congelado— que no señala a la causa por ningún lado.
func TestNoSeCongelaLoQueEstaTrabajando(t *testing.T) {
	g := &Gateway{
		idle:     time.Millisecond, // ya vencido para cualquier lastUse
		services: map[string]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	e := &entry{machineID: "m1", ip: "172.30.0.2", lastUse: time.Now().Add(-time.Hour)}
	g.services["lento"] = e

	// Con trabajo en vuelo: intocable, por vieja que sea su última petición.
	g.begin(e)
	g.mu.Lock()
	victima := e.inflight == 0 && time.Since(e.lastUse) > g.idle
	g.mu.Unlock()
	if victima {
		t.Fatal("iba a congelar una instancia que está atendiendo una petición")
	}
	if e.inflight != 1 {
		t.Errorf("inflight = %d, want 1", e.inflight)
	}

	// Al terminar, lastUse se refresca: el plazo cuenta desde que salió la
	// respuesta, no desde que entró la petición. Si contase desde la entrada,
	// una llamada larga dejaría la máquina lista para el sacrificio justo al
	// devolver el resultado.
	g.end(e)
	g.mu.Lock()
	inflight, reciente := e.inflight, time.Since(e.lastUse) < time.Second
	g.mu.Unlock()
	if inflight != 0 {
		t.Errorf("inflight = %d tras terminar, want 0", inflight)
	}
	if !reciente {
		t.Error("end() debería refrescar lastUse")
	}

	// Y end() no baja de cero aunque se llame de más: un contador negativo
	// volvería inmortal a la instancia.
	g.end(e)
	g.end(e)
	g.mu.Lock()
	n := e.inflight
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("inflight = %d tras end() de más, want 0", n)
	}
}

// El contador de trabajo en vuelo se decrementa SIEMPRE, también si el proxy
// entra en pánico.
//
// Sin el defer, una instancia con inflight alto no vuelve a congelarse nunca:
// el segador la respeta precisamente porque cree que está trabajando. Un solo
// pánico la dejaba consumiendo RAM para siempre.
func TestElContadorEnVueloBajaAunqueHayaPanico(t *testing.T) {
	g := &Gateway{
		idle:     time.Millisecond,
		services: map[string]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	e := &entry{machineID: "m1", lastUse: time.Now().Add(-time.Hour)}
	g.services["svc"] = e

	func() {
		defer func() { _ = recover() }()
		g.begin(e)
		defer g.end(e)
		panic("el proxy se fue al garete")
	}()

	g.mu.Lock()
	n := e.inflight
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("inflight = %d tras un pánico, want 0: esa instancia no se congelaría jamás", n)
	}
	// end() refresca lastUse a propósito: el plazo de inactividad cuenta desde
	// que SALE la respuesta, no desde que entró la petición. Así que ahora mismo
	// la instancia es reciente — lo que importa es que ya no es intocable por
	// culpa del contador, que era el fallo.
	time.Sleep(2 * time.Millisecond)
	g.mu.Lock()
	reclamable := e.inflight == 0 && time.Since(e.lastUse) > g.idle
	g.mu.Unlock()
	if !reclamable {
		t.Error("la instancia sigue siendo intocable para el segador")
	}
}

// Las máquinas del fondo y las efímeras NO son adoptables como instancia
// persistente del servicio.
//
// Ya tienen dueño, y ese dueño las destruye al terminar. Adoptarlas creaba una
// máquina con dos dueños: una acción efímera la borraba debajo de las sesiones
// que el gateway había fijado a ella, y esas sesiones morían sin que nada
// apuntara a la causa.
func TestLasMaquinasConDuenoNoSeAdoptan(t *testing.T) {
	// La misma condición que usa acquire(), aislada.
	adoptable := func(m *api.Machine) bool {
		if m.Labels["pool"] == "true" || m.Labels["ephemeral"] == "true" {
			return false
		}
		return m.Service() == "svc" || m.From == "svc"
	}

	casos := []struct {
		nombre string
		m      *api.Machine
		want   bool
	}{
		{"instancia normal del servicio",
			&api.Machine{Labels: map[string]string{api.LabelService: "svc"}}, true},
		{"restaurada del snapshot", &api.Machine{From: "svc"}, true},
		{"del fondo: la entrega y la destruye el pool",
			&api.Machine{Labels: map[string]string{api.LabelService: "svc", "pool": "true"}}, false},
		{"efímera: muere al acabar la llamada",
			&api.Machine{Labels: map[string]string{api.LabelService: "svc", "ephemeral": "true"}}, false},
		{"de otro servicio", &api.Machine{Labels: map[string]string{api.LabelService: "otro"}}, false},
	}
	for _, c := range casos {
		if got := adoptable(c.m); got != c.want {
			t.Errorf("%s: adoptable = %v, want %v", c.nombre, got, c.want)
		}
	}
}

// Cuando no cabe una instancia más, se hace sitio congelando la ociosa más
// antigua — no se falla.
//
// Un anfitrión justo no puede tener todos los servicios despiertos a la vez, y
// eso no debería significar que el último que llega no funcione NUNCA. Antes
// devolvía un 502 perfectamente explicado y perfectamente inútil: el usuario no
// tiene forma de saber que la solución es esperar a que otro se enfríe.
func TestSeHaceSitioCongelandoLaMasAntigua(t *testing.T) {
	var congelada string
	g := &Gateway{
		services: map[string]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	g.freezeFn = func(id string) error { congelada = id; return nil }

	ahora := time.Now()
	g.services["viejo"] = &entry{machineID: "m-viejo", lastUse: ahora.Add(-time.Hour)}
	g.services["medio"] = &entry{machineID: "m-medio", lastUse: ahora.Add(-time.Minute)}
	g.services["ocupado"] = &entry{machineID: "m-ocupado", lastUse: ahora.Add(-2 * time.Hour), inflight: 1}
	g.services["quiere"] = &entry{machineID: "m-quiere", lastUse: ahora}

	got := g.evictLRU(t.Context(), "quiere")
	if got != "viejo" {
		t.Fatalf("sacrificó %q, quería \"viejo\"", got)
	}
	if congelada != "m-viejo" {
		t.Errorf("congeló %q", congelada)
	}
	// "ocupado" es MÁS antiguo pero tiene trabajo en vuelo: congelarlo debajo de
	// una llamada en curso convierte un "espera un poco" en un fallo para quien
	// ya estaba siendo atendido.
	g.mu.Lock()
	_, sigue := g.services["ocupado"]
	_, fuera := g.services["viejo"]
	g.mu.Unlock()
	if !sigue {
		t.Error("sacrificó una instancia con peticiones en vuelo")
	}
	if fuera {
		t.Error("dejó en el mapa una instancia que acaba de congelar")
	}

	// Sin nadie a quien sacrificar, se rinde de verdad en vez de mentir.
	g2 := &Gateway{services: map[string]*entry{}, routes: map[string]*sessionRoute{}}
	g2.services["unico"] = &entry{machineID: "m", lastUse: ahora}
	if v := g2.evictLRU(t.Context(), "unico"); v != "" {
		t.Errorf("sacrificó %q sin haber candidatos", v)
	}
}

// Si una sola víctima no basta, se congela más de una: el bucle sigue hasta que
// quepa o no queden candidatos. Un anfitrión muy justo puede necesitar liberar
// varios servicios para uno grande.
func TestElDesalojoLiberaVariasSiHaceFalta(t *testing.T) {
	g := &Gateway{services: map[string]*entry{}, routes: map[string]*sessionRoute{}}
	var congeladas []string
	g.freezeFn = func(id string) error { congeladas = append(congeladas, id); return nil }

	ahora := time.Now()
	for i, n := range []string{"a", "b", "c"} {
		g.services[n] = &entry{machineID: "m-" + n, lastUse: ahora.Add(-time.Duration(i) * time.Hour)}
	}
	// Se sacrifican de una en una, siempre la más antigua de las que quedan, sin
	// tocar al que pide sitio.
	for i := 0; i < 3; i++ {
		v := g.evictLRU(t.Context(), "quiere")
		if v == "" {
			break
		}
	}
	if len(congeladas) != 3 {
		t.Fatalf("congeló %d instancias, quería 3: %v", len(congeladas), congeladas)
	}
	// La primera en caer es la MÁS antigua (c, con -2h).
	if congeladas[0] != "m-c" {
		t.Errorf("la primera víctima fue %q, quería la más antigua (m-c)", congeladas[0])
	}
	// Y una vez vacío, se rinde.
	if v := g.evictLRU(t.Context(), "quiere"); v != "" {
		t.Errorf("sacrificó %q con el mapa ya vacío", v)
	}
}

// Una ruta fijada a una instancia que se congeló por debajo debe reconstruirse,
// no enrutar a un cadáver.
//
// Es el cuelgue permanente de context7: evictLRU o el segador congelaban la
// instancia, la ruta seguía apuntando a su proxy, y como route() refresca
// lastUse en cada intento fallido, la ruta no expiraba jamás. La sesión quedaba
// soldada a un invitado pausado.
func TestUnaRutaHuerfanaSeDetecta(t *testing.T) {
	g := &Gateway{services: map[string]*entry{}, routes: map[string]*sessionRoute{}}

	// Sesión fijada a la instancia m1 de "svc".
	e := &entry{machineID: "m1", ip: "172.30.0.2"}
	g.services["svc"] = e
	g.routes["sesion-1"] = &sessionRoute{service: "svc", machineID: "m1", ip: "172.30.0.2"}

	// La instancia sigue siendo la misma: la ruta es válida.
	g.mu.Lock()
	cur := g.services["svc"]
	valida := cur != nil && cur.machineID == g.routes["sesion-1"].machineID
	g.mu.Unlock()
	if !valida {
		t.Fatal("una ruta a la instancia actual debería ser válida")
	}

	// Ahora el servicio se reconstruye con OTRA instancia (m2): la ruta vieja
	// apunta a un cadáver y hay que detectarlo.
	g.services["svc"] = &entry{machineID: "m2", ip: "172.30.0.9"}
	g.mu.Lock()
	cur = g.services["svc"]
	huerfana := cur == nil || cur.machineID != g.routes["sesion-1"].machineID
	g.mu.Unlock()
	if !huerfana {
		t.Error("no detectó que la ruta apunta a una instancia que ya no existe")
	}

	// rebind la reengancha a la instancia viva conservando la sesión.
	g.rebind("sesion-1", g.services["svc"])
	g.mu.Lock()
	rt := g.routes["sesion-1"]
	g.mu.Unlock()
	if rt.machineID != "m2" || rt.ip != "172.30.0.9" {
		t.Errorf("rebind no reapuntó la ruta: %+v", rt)
	}
}

// Cuando no hay instancia de servicio que sacrificar, evictOne libera una del
// fondo: retiene RAM sin atender a nadie, y en un anfitrión justo es la
// diferencia entre que el siguiente servicio arranque o reciba un 507.
func TestElFondoCedeUnaMaquinaBajoPresion(t *testing.T) {
	var quitadas []string
	p := &pool{
		size:  2,
		ready: map[string][]*warmVM{},
		removeFn: func(_ context.Context, id string) error {
			quitadas = append(quitadas, id)
			return nil
		},
	}
	ahora := time.Now()
	p.ready["a"] = []*warmVM{{id: "a-vieja", born: ahora.Add(-time.Hour)}, {id: "a-nueva", born: ahora}}
	p.ready["b"] = []*warmVM{{id: "b-media", born: ahora.Add(-time.Minute)}}

	// Retira la MÁS antigua de todo el fondo.
	if !p.evictOne(t.Context()) {
		t.Fatal("había máquinas en el fondo y no retiró ninguna")
	}
	if len(quitadas) != 1 || quitadas[0] != "a-vieja" {
		t.Errorf("retiró %v, quería [a-vieja]", quitadas)
	}
	// Y la sacó de la cola sin tocar las demás.
	p.mu.Lock()
	na, nb := len(p.ready["a"]), len(p.ready["b"])
	p.mu.Unlock()
	if na != 1 || nb != 1 {
		t.Errorf("colas tras evictOne: a=%d b=%d, quería 1 y 1", na, nb)
	}

	// Con el fondo vacío, evictOne no miente: devuelve false para que el
	// llamador se rinda de verdad.
	p.ready = map[string][]*warmVM{}
	if p.evictOne(t.Context()) {
		t.Error("dijo que retiró algo con el fondo vacío")
	}
}
