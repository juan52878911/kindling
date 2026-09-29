package machine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Pruebas de los grafos de microVMs (grafo*.go) sobre el Firecracker falso:
// sin KVM, sin red y sin root. Lo que arranca, despierta, pausa o vuelca una
// máquina de verdad se sustituye por funciones que registran lo que se les
// pidió; lo que se prueba es la orquestación y la puerta de cada arista.

// escenaGrafo es un manager de prueba con los ganchos de grafo sustituidos.
type escenaGrafo struct {
	t *testing.T
	m *Manager

	arrancadas atomic.Int32
	// retardo al arrancar: abre la ventana en la que varias conexiones
	// esperan al mismo despertar.
	retardo time.Duration
	// fallarArranque, si no es nil, es el error de arrancar el nodo con ese
	// nombre de máquina.
	fallarArranque func(nombre string) error

	mu       sync.Mutex
	eventos  []string
	redes    map[string]knet.GraphSpec
	despiert atomic.Int32
	// peticiones: el RunRequest de cada máquina arrancada, por nombre, y las
	// carpetas de grafo que su contexto permitía montar.
	peticiones map[string]api.RunRequest
	permitidas map[string][]string
	// puertos: las direcciones a las que se esperó (esperarPuertoGrafo).
	puertos []string
}

func nuevaEscenaGrafo(t *testing.T) *escenaGrafo {
	t.Helper()
	m := newTestManager(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	e := &escenaGrafo{t: t, m: m, redes: map[string]knet.GraphSpec{},
		peticiones: map[string]api.RunRequest{}, permitidas: map[string][]string{}}

	pArr, pDesp, pCong, pRed := arrancarNodoGrafo, despertarNodoGrafo, congelarNodoGrafo, montarRedGrafo
	pPaus, pReanu, pCommit, pBorrar, pPuerto := pausarNodoGrafo, reanudarNodoGrafo, commitNodoPausado, borrarSnapshotGrafo, esperarPuertoGrafo
	pReg, pInvC, pInvE := registrarCredenciales, invalidarCopia, invalidarEnlaces
	t.Cleanup(func() {
		arrancarNodoGrafo, despertarNodoGrafo, congelarNodoGrafo, montarRedGrafo = pArr, pDesp, pCong, pRed
		pausarNodoGrafo, reanudarNodoGrafo, commitNodoPausado, borrarSnapshotGrafo, esperarPuertoGrafo = pPaus, pReanu, pCommit, pBorrar, pPuerto
		registrarCredenciales, invalidarCopia, invalidarEnlaces = pReg, pInvC, pInvE
	})

	arrancarNodoGrafo = func(ctx context.Context, m *Manager, req api.RunRequest) (*api.Machine, error) {
		time.Sleep(e.retardo)
		if e.fallarArranque != nil {
			if err := e.fallarArranque(req.Name); err != nil {
				return nil, err
			}
		}
		e.arrancadas.Add(1)
		e.mu.Lock()
		e.peticiones[req.Name] = req
		for _, sh := range req.Shares {
			if carpetaDeGrafo(ctx, sh.Source) {
				e.permitidas[req.Name] = append(e.permitidas[req.Name], sh.Source)
			}
		}
		e.mu.Unlock()
		f := nuevoFcFalso(t)
		id := newID()
		if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
			return nil, err
		}
		egress := req.Egress
		if egress == "" {
			egress = "none"
		}
		m.mu.Lock()
		mc := &api.Machine{ID: id, Name: req.Name, Image: req.Image, From: req.From, State: api.StateRunning,
			NetIndex: len(m.byID) + 2, Egress: egress, Labels: req.Labels, CreatedAt: time.Now()}
		m.byID[id] = mc
		m.socket[id] = f.Sock
		out := mc.Clone()
		m.mu.Unlock()
		e.anotar("run " + req.Name)
		return out, nil
	}
	cambiarEstado := func(m *Manager, id string, st api.State) {
		m.mu.Lock()
		if mc := m.byID[id]; mc != nil {
			mc.State = st
		}
		m.mu.Unlock()
	}
	despertarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		time.Sleep(e.retardo)
		e.despiert.Add(1)
		cambiarEstado(m, id, api.StateRunning)
		e.anotar("thaw " + e.nombre(id))
		return nil
	}
	congelarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		cambiarEstado(m, id, api.StateWarm)
		m.invalidarSesiones(id, "frozen")
		e.anotar("freeze " + e.nombre(id))
		return nil
	}
	pausarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		cambiarEstado(m, id, api.StatePaused)
		e.anotar("pause " + e.nombre(id))
		return nil
	}
	reanudarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		cambiarEstado(m, id, api.StateRunning)
		e.anotar("resume " + e.nombre(id))
		return nil
	}
	commitNodoPausado = func(ctx context.Context, m *Manager, id, name string) error {
		if mc, _ := m.Get(id); mc == nil || mc.State != api.StatePaused {
			return fmt.Errorf("%s no estaba pausada al volcarla", id)
		}
		e.anotar("commit " + e.nombre(id) + " " + name)
		if strings.Contains(name, "rompe") {
			return errors.New("disco lleno")
		}
		return os.MkdirAll(m.snapDir(name), 0o755)
	}
	borrarSnapshotGrafo = func(m *Manager, name string) error {
		e.anotar("rmsnap " + name)
		return os.RemoveAll(m.snapDir(name))
	}
	esperarPuertoGrafo = func(_ context.Context, addr string) error {
		e.mu.Lock()
		e.puertos = append(e.puertos, addr)
		e.mu.Unlock()
		return nil
	}
	montarRedGrafo = func(n *knet.Net, spec knet.GraphSpec) error {
		e.mu.Lock()
		e.redes[n.NS] = spec
		e.mu.Unlock()
		return nil
	}
	registrarCredenciales = func(context.Context, *fc.Client, *knet.Net, []credproxy.Credential, string, credproxy.ResolveMachineFunc) error {
		return nil
	}
	invalidarEnlaces = func(...string) int { return 0 }
	return e
}

func (e *escenaGrafo) anotar(s string) {
	e.mu.Lock()
	e.eventos = append(e.eventos, s)
	e.mu.Unlock()
}

func (e *escenaGrafo) vistos() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.eventos...)
}

func (e *escenaGrafo) olvidar() {
	e.mu.Lock()
	e.eventos = nil
	e.mu.Unlock()
}

// nombre es el nodo de la máquina id (para los eventos).
func (e *escenaGrafo) nombre(id string) string {
	e.m.mu.RLock()
	defer e.m.mu.RUnlock()
	if mc := e.m.byID[id]; mc != nil {
		return mc.Labels[api.LabelGraphNode]
	}
	return id
}

// grafoTienda es web -> api:8081 (link) con db lazy detrás: el de los
// ejemplos. Sin aristas entre máquinas si sinAristas (lo que vale en macOS).
func grafoTienda(sinAristas bool) api.Graph {
	g := api.Graph{Name: "tienda", Nodes: map[string]api.GraphNode{
		"web": {Image: "min", Ports: []int{8000}},
		"api": {Image: "min", Ports: []int{8081}},
		"db":  {Image: "min", Ports: []int{5432}, Wake: api.GraphWakeLazy},
	}}
	if !sinAristas {
		g.Edges = []api.GraphEdge{{From: "web", To: "api", Kind: api.GraphEdgeLink, Port: 8081}}
	}
	return g
}

// montarGrafo registra g ya instanciado sin pasar por GraphUp (que en macOS
// rechaza las aristas): cada nodo eager con su máquina corriendo.
func (e *escenaGrafo) montarGrafo(g api.Graph) *api.Graph {
	e.t.Helper()
	if err := api.ValidateGraph(&g); err != nil {
		e.t.Fatal(err)
	}
	g.ID = newID()
	e.m.mu.Lock()
	if e.m.grafos == nil {
		e.m.grafos = map[string]*api.Graph{}
	}
	e.m.grafos[g.ID] = &g
	e.m.mu.Unlock()
	for _, n := range g.SortedNodeNames() {
		if g.Nodes[n].Wake == api.GraphWakeEager {
			if err := e.m.instanciarNodo(context.Background(), g.ID, n, ""); err != nil {
				e.t.Fatal(err)
			}
		}
	}
	e.m.mu.RLock()
	defer e.m.mu.RUnlock()
	return copiaGrafo(e.m.grafos[g.ID])
}

func (e *escenaGrafo) maquina(gid, nodo string) string {
	e.m.mu.RLock()
	defer e.m.mu.RUnlock()
	return e.m.grafos[gid].Nodes[nodo].MachineID
}

// direccionEsperada es lo que el resolvedor da para la máquina id: su netns
// en Linux; en macOS no hay aristas y el error es el de attach.
func (e *escenaGrafo) comprobarDireccion(addr string, err error, id string, port int) {
	e.t.Helper()
	if !modeloAPosible {
		if !errors.Is(err, errModeloASoloLinux) {
			e.t.Fatalf("en esta plataforma no hay aristas: %q %v", addr, err)
		}
		return
	}
	e.m.mu.RLock()
	want := net.JoinHostPort(knet.Plan(e.m.byID[id].NetIndex, id).NSIP, fmt.Sprint(port))
	e.m.mu.RUnlock()
	if err != nil || addr != want {
		e.t.Fatalf("addr=%q err=%v, esperaba %s", addr, err, want)
	}
}

// La puerta de cada conexión, caso a caso: solo la arista declarada, del
// grafo y el nodo exactos, a un destino que corre y expone el puerto.
func TestGrafoResolvedorRechaza(t *testing.T) {
	casos := []struct {
		nombre string
		mod    func(e *escenaGrafo, g *api.Graph)
		hacia  string
		port   int
		kind   string
		error  string
	}{
		{nombre: "todo en orden"},
		{nombre: "destino de otro grafo", mod: func(e *escenaGrafo, g *api.Graph) {
			e.m.byID[e.maquinaSinLock(g.ID, "api")].Labels[api.LabelGraph] = "0123456789abcdef"
		}, error: "is not node api"},
		{nombre: "destino que es otro nodo", mod: func(e *escenaGrafo, g *api.Graph) {
			e.m.byID[e.maquinaSinLock(g.ID, "api")].Labels[api.LabelGraphNode] = "db"
		}, error: "is not node api"},
		{nombre: "destino parado", mod: func(e *escenaGrafo, g *api.Graph) {
			e.m.byID[e.maquinaSinLock(g.ID, "api")].State = api.StateStopped
		}, error: "is stopped"},
		{nombre: "destino borrado", mod: func(e *escenaGrafo, g *api.Graph) {
			delete(e.m.byID, e.maquinaSinLock(g.ID, "api"))
		}, error: "no longer exists"},
		{nombre: "puerto sin arista", port: 9090, error: "has no link edge"},
		{nombre: "puerto que el destino ya no expone", mod: func(e *escenaGrafo, g *api.Graph) {
			e.m.byID[e.maquinaSinLock(g.ID, "api")].Labels[api.LabelPorts] = "9999"
		}, error: "does not expose port 8081"},
		{nombre: "arista de otro tipo", kind: api.GraphEdgeCredential, error: "has no credential edge"},
		{nombre: "sin arista hacia ese nodo", hacia: "db", error: "has no link edge"},
		{nombre: "origen reetiquetado", mod: func(e *escenaGrafo, g *api.Graph) {
			e.m.byID[e.maquinaSinLock(g.ID, "web")].Labels[api.LabelGraphNode] = "api"
		}, error: "is not node web"},
		{nombre: "grafo borrado", mod: func(e *escenaGrafo, g *api.Graph) {
			delete(e.m.grafos, g.ID)
		}, error: "no longer exists"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			e := nuevaEscenaGrafo(t)
			g := e.montarGrafo(grafoTienda(false))
			web, api8081 := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
			if c.mod != nil {
				e.m.mu.Lock()
				c.mod(e, g)
				e.m.mu.Unlock()
			}
			hacia, port, kind := "api", 8081, api.GraphEdgeLink
			if c.hacia != "" {
				hacia = c.hacia
			}
			if c.port != 0 {
				port = c.port
			}
			if c.kind != "" {
				kind = c.kind
			}
			addr, id, err := e.m.resolverArista(context.Background(), web, g.ID, "web", hacia, port, kind)
			if c.error != "" {
				if err == nil || !strings.Contains(err.Error(), c.error) {
					t.Fatalf("esperaba %q, llegó addr=%q err=%v", c.error, addr, err)
				}
				return
			}
			if modeloAPosible && id != api8081 {
				t.Fatalf("resolvió a la máquina %s, no a la de api (%s)", id, api8081)
			}
			e.comprobarDireccion(addr, err, api8081, 8081)
		})
	}
}

func (e *escenaGrafo) maquinaSinLock(gid, nodo string) string {
	return e.m.grafos[gid].Nodes[nodo].MachineID
}

// Un nodo lazy se instancia UNA vez aunque lo pidan diez conexiones a la vez,
// y todas acaban en la misma máquina.
func TestGrafoLazyUnaSolaInstancia(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := grafoTienda(false)
	g.Edges = append(g.Edges, api.GraphEdge{From: "api", To: "db", Kind: api.GraphEdgeLink, Port: 5432})
	gg := e.montarGrafo(g)
	if e.maquina(gg.ID, "db") != "" {
		t.Fatal("el lazy nació con máquina")
	}
	antes := e.arrancadas.Load()
	e.retardo = 50 * time.Millisecond
	apiID := e.maquina(gg.ID, "api")
	var wg sync.WaitGroup
	ids := make([]string, 10)
	errs := make([]error, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ids[i], errs[i] = e.m.resolverArista(context.Background(), apiID, gg.ID, "api", "db", 5432, api.GraphEdgeLink)
		}()
	}
	wg.Wait()
	if n := e.arrancadas.Load() - antes; n != 1 {
		t.Fatalf("el lazy se instanció %d veces", n)
	}
	db := e.maquina(gg.ID, "db")
	if db == "" {
		t.Fatal("el grafo no anotó la máquina del lazy")
	}
	for i := range 10 {
		if modeloAPosible && (errs[i] != nil || ids[i] != db) {
			t.Fatalf("conexión %d: id=%s err=%v, esperaba %s", i, ids[i], errs[i], db)
		}
	}
	mc, _ := e.m.Get(db)
	if mc.Labels[api.LabelGraph] != gg.ID || mc.Labels[api.LabelGraphNode] != "db" || mc.Labels[api.LabelPorts] != "5432" {
		t.Fatalf("etiquetas del lazy: %v", mc.Labels)
	}
	if v, ok := e.m.virtuales.Load(db); !ok || v.(string) != idVirtual(gg.ID, "db") {
		t.Fatalf("la máquina del lazy no quedó asociada a su nodo: %v", v)
	}
}

// Un destino congelado se despierta una vez por mucho que lo pidan a la vez.
func TestGrafoDespiertaCongeladoUnaVez(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	e.m.mu.Lock()
	e.m.byID[apiID].State = api.StateWarm
	e.m.mu.Unlock()
	e.retardo = 50 * time.Millisecond
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr, _, err := e.m.resolverArista(context.Background(), web, g.ID, "web", "api", 8081, api.GraphEdgeLink)
			if modeloAPosible && (err != nil || addr == "") {
				t.Errorf("tras despertar: %q %v", addr, err)
			}
		}()
	}
	wg.Wait()
	if n := e.despiert.Load(); n != 1 {
		t.Fatalf("se despertó %d veces", n)
	}
	if mc, _ := e.m.Get(apiID); mc.State != api.StateRunning {
		t.Fatalf("api quedó %s", mc.State)
	}
}

// Tormenta: por encima de despertarMaxEspera conexiones esperando al mismo
// despertar, la siguiente se rechaza en el acto (busy).
func TestGrafoTormentaDeDespertaresAcotada(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	e.m.mu.Lock()
	e.m.byID[apiID].State = api.StateWarm
	e.m.mu.Unlock()
	soltar := make(chan struct{})
	despertarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		<-soltar
		m.mu.Lock()
		m.byID[id].State = api.StateRunning
		m.mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for range despertarMaxEspera {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = e.m.resolverArista(context.Background(), web, g.ID, "web", "api", 8081, api.GraphEdgeLink)
		}()
	}
	k := g.ID + "/api"
	for i := 0; ; i++ {
		e.m.despMu.Lock()
		n := 0
		if d := e.m.desp[k]; d != nil {
			n = d.esperando
		}
		e.m.despMu.Unlock()
		if n == despertarMaxEspera {
			break
		}
		if i > 500 {
			t.Fatalf("solo %d esperando", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _, err := e.m.resolverArista(context.Background(), web, g.ID, "web", "api", 8081, api.GraphEdgeLink)
	if !errors.Is(err, credproxy.ErrEnlaceOcupado) {
		t.Fatalf("la conexión %d no se rechazó por ocupado: %v", despertarMaxEspera+1, err)
	}
	close(soltar)
	wg.Wait()
}

// Las conexiones que se cancelan mientras esperan un despertar dejan de
// contar: después de despertarMaxEspera+N abandonos, una conexión nueva sigue
// esperando al despertar en vez de rechazarse por ocupado.
func TestGrafoDespertarCanceladoNoCuenta(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	e.m.mu.Lock()
	e.m.byID[apiID].State = api.StateWarm
	e.m.mu.Unlock()
	soltar := make(chan struct{})
	despertarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		<-soltar
		m.mu.Lock()
		m.byID[id].State = api.StateRunning
		m.mu.Unlock()
		return nil
	}
	for i := range despertarMaxEspera + 8 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_, _, err := e.m.resolverArista(ctx, web, g.ID, "web", "api", 8081, api.GraphEdgeLink)
		cancel()
		if errors.Is(err, credproxy.ErrEnlaceOcupado) {
			t.Fatalf("la conexión %d se rechazó por ocupado sin nadie esperando", i+1)
		}
	}
	e.m.despMu.Lock()
	d := e.m.desp[g.ID+"/api"]
	n := -1
	if d != nil {
		n = d.esperando
	}
	e.m.despMu.Unlock()
	if n != 0 {
		t.Errorf("quedan %d esperando tras cancelarse todas", n)
	}
	// El despertar en vuelo termina antes de que la limpieza del test
	// restaure despertarNodoGrafo.
	close(soltar)
	if d != nil {
		<-d.hecho
	}
}

// Un lazy que no cabe: la conexión se rechaza y el motivo es no_capacity.
func TestGrafoLazySinCapacidad(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := grafoTienda(false)
	g.Edges = append(g.Edges, api.GraphEdge{From: "api", To: "db", Kind: api.GraphEdgeLink, Port: 5432})
	gg := e.montarGrafo(g)
	e.fallarArranque = func(nombre string) error {
		return &api.StatusError{Code: api.StatusInsufficientMemory, Message: "not enough memory"}
	}
	_, _, err := e.m.resolverArista(context.Background(), e.maquina(gg.ID, "api"), gg.ID, "api", "db", 5432, api.GraphEdgeLink)
	if !errors.Is(err, credproxy.ErrSinCapacidad) {
		t.Fatalf("esperaba ErrSinCapacidad, llegó %v", err)
	}
	if e.maquina(gg.ID, "db") != "" {
		t.Fatal("un lazy que no cupo quedó anotado con máquina")
	}
}

// Snapshot: pausa todos los que corren, vuelca todos pausados, reanuda los
// que pausó (y deja pausado el que ya lo estaba); todas las plantillas de la
// misma generación.
func TestGrafoSnapshotPausaVuelcaReanuda(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	e.m.mu.Lock()
	e.m.byID[e.maquinaSinLock(g.ID, "web")].State = api.StatePaused
	e.m.mu.Unlock()
	e.olvidar()
	snap, err := e.m.GraphSnapshot(context.Background(), "tienda", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pause api",
		"commit api tienda-api-1", "commit web tienda-web-1",
		"resume api",
	}
	if got := e.vistos(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("orden:\n got %v\nwant %v", got, want)
	}
	if snap.Generation != 1 || len(snap.Templates) != 2 || snap.Templates["web"] != "tienda-web-1" {
		t.Fatalf("resultado: %+v", snap)
	}
	if mc, _ := e.m.Get(e.maquina(g.ID, "web")); mc.State != api.StatePaused {
		t.Fatalf("web estaba pausada y quedó %s", mc.State)
	}
	gv, _ := e.m.Graph(g.ID)
	if gv.Generation != 1 {
		t.Fatalf("generación %d", gv.Generation)
	}
	// La siguiente, generación 2; repetir el mismo prefijo y generación no
	// pisa nada.
	e.olvidar()
	snap, err = e.m.GraphSnapshot(context.Background(), g.ID, "otro")
	if err != nil || snap.Generation != 2 || snap.Templates["api"] != "otro-api-2" {
		t.Fatalf("segunda: %+v %v", snap, err)
	}
}

// Si un volcado falla, se borran las plantillas ya hechas y se reanuda todo:
// el grafo queda como estaba.
func TestGrafoSnapshotDeshaceEnFallo(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	e.olvidar()
	// "rompe" en el nombre de la segunda plantilla (web): el falso falla.
	prev := commitNodoPausado
	commitNodoPausado = func(ctx context.Context, m *Manager, id, name string) error {
		if strings.HasPrefix(name, "tienda-web") {
			name = "rompe"
		}
		return prev(ctx, m, id, name)
	}
	if _, err := e.m.GraphSnapshot(context.Background(), "tienda", ""); err == nil || !strings.Contains(err.Error(), "disco lleno") {
		t.Fatalf("esperaba el fallo del volcado, llegó %v", err)
	}
	want := []string{
		"pause api", "pause web",
		"commit api tienda-api-1", "commit web rompe",
		"rmsnap tienda-api-1",
		"resume api", "resume web",
	}
	if got := e.vistos(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("orden:\n got %v\nwant %v", got, want)
	}
	for _, n := range []string{"web", "api"} {
		if mc, _ := e.m.Get(e.maquina(g.ID, n)); mc.State != api.StateRunning {
			t.Fatalf("%s quedó %s", n, mc.State)
		}
	}
	if gv, _ := e.m.Graph(g.ID); gv.Generation != 0 {
		t.Fatalf("un snapshot fallido subió la generación a %d", gv.Generation)
	}
	if _, err := os.Stat(e.m.snapDir("tienda-api-1")); !os.IsNotExist(err) {
		t.Fatalf("la plantilla parcial sigue ahí: %v", err)
	}
}

// Un nodo congelado no se vuelca: se pide despertar el grafo antes.
func TestGrafoSnapshotConNodoCongelado(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	e.m.mu.Lock()
	e.m.byID[e.maquinaSinLock(g.ID, "api")].State = api.StateWarm
	e.m.mu.Unlock()
	_, err := e.m.GraphSnapshot(context.Background(), "tienda", "")
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != 409 || !strings.Contains(err.Error(), "thaw the graph") {
		t.Fatalf("esperaba 409 con consejo, llegó %v", err)
	}
}

// Fork: grafos nuevos cuyas aristas llevan a SUS copias y nunca al original,
// ni el original a ellas.
func TestGrafoForkNoResuelveAlOriginal(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(false))
	e.olvidar()
	copias, err := e.m.GraphFork(context.Background(), "tienda", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(copias) != 2 || copias[0].ID == copias[1].ID || copias[0].ForkOf != g.ID {
		t.Fatalf("copias: %+v", copias)
	}
	origWeb, origAPI := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	for _, c := range copias {
		web, apiID := e.maquina(c.ID, "web"), e.maquina(c.ID, "api")
		if web == "" || apiID == "" || web == origWeb || apiID == origAPI {
			t.Fatalf("copia %s: máquinas %s %s (originales %s %s)", c.Name, web, apiID, origWeb, origAPI)
		}
		if e.maquina(c.ID, "db") != "" {
			t.Fatal("el lazy sin instancia del original nació instanciado en la copia")
		}
		mc, _ := e.m.Get(apiID)
		if mc.Labels[api.LabelForkOf] != origAPI || !strings.HasPrefix(mc.From, "gfork-") {
			t.Fatalf("la copia de api no dice de dónde sale: from=%s labels=%v", mc.From, mc.Labels)
		}
		e.m.mu.RLock()
		// La copia llega a SU api.
		dest, dormido, err := e.m.comprobarAristaLocked(web, c.ID, "web", "api", 8081, api.GraphEdgeLink)
		if err != nil || dormido || dest.ID != apiID {
			e.m.mu.RUnlock()
			t.Fatalf("la copia no llega a su api: %v %v %v", dest, dormido, err)
		}
		// Con el ID del grafo original, la máquina de la copia no es nadie.
		if _, _, err := e.m.comprobarAristaLocked(web, g.ID, "web", "api", 8081, api.GraphEdgeLink); err == nil {
			e.m.mu.RUnlock()
			t.Fatal("la web de la copia resolvió por el grafo original")
		}
		// Y el original no llega a la copia.
		if _, _, err := e.m.comprobarAristaLocked(origWeb, c.ID, "web", "api", 8081, api.GraphEdgeLink); err == nil {
			e.m.mu.RUnlock()
			t.Fatal("la web original resolvió por el grafo de la copia")
		}
		e.m.mu.RUnlock()
	}
	// El original sigue corriendo y apuntando a lo suyo.
	e.m.mu.RLock()
	dest, _, err := e.m.comprobarAristaLocked(origWeb, g.ID, "web", "api", 8081, api.GraphEdgeLink)
	e.m.mu.RUnlock()
	if err != nil || dest.ID != origAPI {
		t.Fatalf("el original ya no llega a su api: %v %v", dest, err)
	}
	// Cada plantilla temporal lleva la marca de fork.
	for _, nodo := range []string{"web", "api"} {
		mc, _ := e.m.Get(e.maquina(copias[0].ID, nodo))
		if !esSnapshotDeFork(e.m.snapDir(mc.From)) {
			t.Fatalf("la plantilla %s no está marcada como de fork", mc.From)
		}
	}
	// Borrar una copia no toca el original.
	if err := e.m.GraphRemove(context.Background(), copias[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.Get(origAPI); !ok {
		t.Fatal("borrar la copia se llevó el original")
	}
	if _, ok := e.m.Get(e.maquina(copias[1].ID, "api")); !ok {
		t.Fatal("borrar una copia se llevó la otra")
	}
}

// Las credenciales de un fork son las del original (mismo marcador: el
// invitado lo tiene en memoria) apuntadas al nodo del grafo nuevo.
func TestGrafoCredencialesReescritas(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	nuevo := e.montarGrafo(api.Graph{Name: "copia", Nodes: map[string]api.GraphNode{
		"web": {Image: "min"}, "api": {Image: "min"}, "db": {Image: "min", Wake: api.GraphWakeLazy}}})
	creds := []credproxy.Credential{{Env: "PGPASSWORD", Domain: "db.graph", Placeholder: "kling-cred-x", Secret: "s",
		Kind: credproxy.KindPostgres, UpstreamMachine: idVirtual(g.ID, "db"), UpstreamOwner: g.ID}}
	out, err := e.m.credencialesReescritas(g.ID, nuevo.ID, creds)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].UpstreamMachine != idVirtual(nuevo.ID, "db") || out[0].UpstreamOwner != nuevo.ID || out[0].Placeholder != "kling-cred-x" {
		t.Fatalf("reescrita: %+v", out[0])
	}
	if creds[0].UpstreamOwner != g.ID {
		t.Fatal("se tocó la credencial original")
	}
	creds[0].UpstreamOwner = "otro"
	if _, err := e.m.credencialesReescritas(g.ID, nuevo.ID, creds); err == nil {
		t.Fatal("una credencial que no es de una arista del grafo se reescribió")
	}
}

// Un fork no se lleva credenciales que no sean de las aristas del grafo.
func TestGrafoForkRechazaCredencialesSueltas(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	web := e.maquina(g.ID, "web")
	if err := e.m.guardarCredenciales(web, []credproxy.Credential{{Env: "API_KEY", Domain: "api.example.com",
		Placeholder: "kling-cred-y", Secret: "k"}}); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.GraphFork(context.Background(), "tienda", 1)
	if err == nil || !strings.Contains(err.Error(), "not one of its graph edges") {
		t.Fatalf("esperaba el rechazo, llegó %v", err)
	}
	if n := len(e.m.Graphs()); n != 1 {
		t.Fatalf("quedaron %d grafos", n)
	}
}

// up sin aristas (lo que vale en todas las plataformas): arranca los eager,
// deja el lazy sin máquina, etiqueta cada máquina, guarda el grafo y lo
// vuelve a leer un daemon nuevo; rm lo borra todo.
func TestGrafoUpPersistenciaYRm(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g, err := e.m.GraphUp(context.Background(), grafoTienda(true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.arrancadas.Load() != 2 || g.State != api.GraphStateRunning || g.Nodes["db"].MachineID != "" {
		t.Fatalf("up: %d arrancadas, %+v", e.arrancadas.Load(), g)
	}
	web := g.Nodes["web"].MachineID
	mc, _ := e.m.Get(web)
	if mc.Name != "tienda-web" || mc.Labels[api.LabelGraph] != g.ID || mc.Labels[api.LabelGraphNode] != "web" || mc.Labels[api.LabelPorts] != "8000" {
		t.Fatalf("máquina de web: %s %v", mc.Name, mc.Labels)
	}
	if _, err := e.m.GraphUp(context.Background(), grafoTienda(true), nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("dos grafos con el mismo nombre: %v", err)
	}
	// Un daemon nuevo sobre la misma raíz lo lee igual.
	m2 := &Manager{root: e.m.root, byID: map[string]*api.Machine{}}
	m2.cargarGrafos()
	g2, err := m2.Graph("tienda")
	if err != nil || g2.ID != g.ID || g2.Nodes["web"].MachineID != web || len(g2.Nodes) != 3 {
		t.Fatalf("releído: %+v %v", g2, err)
	}
	if v, ok := m2.virtuales.Load(web); !ok || v.(string) != idVirtual(g.ID, "web") {
		t.Fatal("al releer, la máquina de web no quedó asociada a su nodo")
	}
	// freeze y thaw pasan por todos los nodos con máquina.
	e.olvidar()
	if fg, err := e.m.GraphFreeze(context.Background(), "tienda"); err != nil || fg.State != api.GraphStateFrozen {
		t.Fatalf("freeze: %+v %v", fg, err)
	}
	if tg, err := e.m.GraphThaw(context.Background(), "tienda"); err != nil || tg.State != api.GraphStateRunning {
		t.Fatalf("thaw: %+v %v", tg, err)
	}
	if got := strings.Join(e.vistos(), "|"); got != "freeze api|freeze web|thaw api|thaw web" {
		t.Fatalf("freeze/thaw: %s", got)
	}
	if err := e.m.GraphRemove(context.Background(), "tienda"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.Get(web); ok {
		t.Fatal("rm dejó la máquina de web")
	}
	if _, err := os.Stat(e.m.rutaGrafo(g.ID)); !os.IsNotExist(err) {
		t.Fatalf("rm dejó el grafo en el almacén: %v", err)
	}
	if _, err := e.m.Graph("tienda"); err == nil {
		t.Fatal("el grafo sigue ahí")
	}
}

// up es todo o nada: si un eager no arranca, se borra lo creado.
func TestGrafoUpTodoONada(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	e.fallarArranque = func(nombre string) error {
		if nombre == "tienda-web" {
			return errors.New("imagen rota")
		}
		return nil
	}
	if _, err := e.m.GraphUp(context.Background(), grafoTienda(true), nil); err == nil || !strings.Contains(err.Error(), "imagen rota") {
		t.Fatalf("esperaba el fallo de web, llegó %v", err)
	}
	if n := len(e.m.List()); n != 0 {
		t.Fatalf("quedaron %d máquinas", n)
	}
	if n := len(e.m.Graphs()); n != 0 {
		t.Fatalf("quedaron %d grafos", n)
	}
	ents, _ := os.ReadDir(e.m.dirGrafos())
	if len(ents) != 0 {
		t.Fatalf("quedaron ficheros en el almacén: %v", ents)
	}
}

// Aristas entre máquinas en una plataforma sin ellas: 501 con el motivo.
// Y en Linux, una arista credential sin su clave no pasa.
func TestGrafoUpAristasSegunPlataforma(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	_, err := e.m.GraphUp(context.Background(), grafoTienda(false), nil)
	var se *api.StatusError
	if !modeloAPosible {
		if !errors.As(err, &se) || se.Code != 501 || !strings.Contains(err.Error(), "Linux-only") {
			t.Fatalf("macOS: esperaba 501, llegó %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Linux: %v", err)
	}
	if spec, ok := e.redes["kl-"+e.maquina(e.grafoPorNombre("tienda"), "web")[:8]]; !ok || len(spec.Links) != 1 || spec.Links[0].Host != "api.graph" {
		t.Fatalf("la red de web no llevaba su enlace: %+v", e.redes)
	} else if spec.Credentials {
		t.Error("un nodo con solo aristas link pidió los proxies de credenciales")
	}
	g := grafoTienda(true)
	g.Name = "conclave"
	g.Edges = []api.GraphEdge{{From: "api", To: "db", Kind: api.GraphEdgeCredential, Env: "PGPASSWORD", User: "app", Database: "shop"}}
	if _, err := e.m.GraphUp(context.Background(), g, nil); !errors.As(err, &se) || se.Code != 400 || !strings.Contains(err.Error(), "no secret") {
		t.Fatalf("credential sin clave: %v", err)
	}
	cg, err := e.m.GraphUp(context.Background(), g, map[string]string{"api/PGPASSWORD": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	apiID := cg.Nodes["api"].MachineID
	if spec, ok := e.redes["kl-"+apiID[:8]]; !ok || !spec.Credentials {
		t.Errorf("el nodo con la arista credential no pidió sus proxies: %+v", spec)
	}
	creds, err := e.m.cargarCredenciales(apiID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("credenciales de api: %v %v", creds, err)
	}
	c := creds[0]
	if c.Domain != "db.graph" || c.UpstreamMachine != idVirtual(cg.ID, "db") || c.UpstreamOwner != cg.ID || c.Secret != "s3cret" || c.Port != 5432 {
		t.Fatalf("credencial de la arista: %+v", c)
	}
	// Por la API nunca sale la clave.
	if strings.Contains(fmt.Sprintf("%+v", cg), "s3cret") {
		t.Fatal("la clave aparece en el grafo")
	}
	b, _ := os.ReadFile(e.m.rutaGrafo(cg.ID))
	if strings.Contains(string(b), "s3cret") {
		t.Fatal("la clave está en claro en el almacén del grafo")
	}
	// El resolvedor de la credencial va por la arista (db es lazy: se
	// instancia).
	addr, err := e.m.resolverCopia(apiID)(c.UpstreamMachine, c.UpstreamOwner, 5432)
	db := e.maquina(cg.ID, "db")
	if db == "" {
		t.Fatalf("la credencial no despertó al lazy: %q %v", addr, err)
	}
	e.comprobarDireccion(addr, err, db, 5432)
}

func (e *escenaGrafo) grafoPorNombre(nombre string) string {
	for _, g := range e.m.Graphs() {
		if g.Name == nombre {
			return g.ID
		}
	}
	e.t.Fatalf("no hay grafo %s", nombre)
	return ""
}

// Congelar, parar o borrar la máquina de un nodo corta también lo que va a su
// nodo: las credenciales de las aristas llevan el ID del nodo.
func TestGrafoInvalidaPorNodo(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	var cortados []string
	var mu sync.Mutex
	invalidarCopia = func(id string) int {
		mu.Lock()
		cortados = append(cortados, id)
		mu.Unlock()
		return 0
	}
	var enlaces []string
	invalidarEnlaces = func(ids ...string) int {
		mu.Lock()
		enlaces = append(enlaces, ids...)
		mu.Unlock()
		return 0
	}
	apiID := e.maquina(g.ID, "api")
	e.m.invalidarSesiones(apiID, "frozen")
	want := apiID + "," + idVirtual(g.ID, "api")
	if strings.Join(cortados, ",") != want || strings.Join(enlaces, ",") != want {
		t.Fatalf("cortó %v y %v, quería %s", cortados, enlaces, want)
	}
}

// Nadie más que el daemon pone o cambia las etiquetas de grafo.
func TestGrafoEtiquetasReservadas(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	web := e.maquina(g.ID, "web")
	for _, k := range []string{api.LabelGraph, api.LabelGraphNode, "kling.graph.otra"} {
		if err := e.m.SetLabels(web, map[string]string{k: "x"}); err == nil {
			t.Fatalf("SetLabels cambió %s", k)
		}
	}
	if mc, _ := e.m.Get(web); mc.Labels[api.LabelGraph] != g.ID {
		t.Fatal("la etiqueta de grafo cambió")
	}
	if err := e.m.SetLabels(web, map[string]string{"equipo": "a"}); err != nil {
		t.Fatalf("una etiqueta normal: %v", err)
	}
	// Una plantilla no es de ningún grafo.
	if l := sinEtiquetasGrafo(map[string]string{api.LabelGraph: "g", api.LabelGraphNode: "n", "service": "s"}); len(l) != 1 || l["service"] != "s" {
		t.Fatalf("sinEtiquetasGrafo: %v", l)
	}
}

// Las claves de las aristas viajan cifradas y atadas a su grafo.
func TestGrafoSecretosCifrados(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	if err := e.m.guardarSecretosGrafo("aaaa000000000001", map[string]string{"api/PG": "clave"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.m.rutaSecretosGrafo("aaaa000000000001"))
	if strings.Contains(string(b), "clave") {
		t.Fatal("la clave está en claro")
	}
	got, err := e.m.cargarSecretosGrafo("aaaa000000000001")
	if err != nil || got["api/PG"] != "clave" {
		t.Fatalf("releída: %v %v", got, err)
	}
	// Copiada a otro grafo no se abre.
	if err := os.Rename(e.m.rutaSecretosGrafo("aaaa000000000001"), e.m.rutaSecretosGrafo("aaaa000000000002")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.cargarSecretosGrafo("aaaa000000000002"); err == nil {
		t.Fatal("las claves de un grafo se abrieron como de otro")
	}
	if _, err := secretosDeAristas(&api.Graph{}, map[string]string{"x/Y": "z"}); err == nil {
		t.Fatal("una clave sin arista se aceptó")
	}
}

// ── aristas depends y share ──────────────────────────────────────────────────

// grafoCadena: a depende de b, b de c (lazy); d suelto. Sin puertos en las
// depends, que es lo que vale en todas las plataformas.
func grafoCadena() api.Graph {
	return api.Graph{Name: "cadena", Nodes: map[string]api.GraphNode{
		"a": {Image: "min"},
		"b": {Image: "min", Ports: []int{8081}},
		"c": {Image: "min", Wake: api.GraphWakeLazy},
		"d": {Image: "min"},
	}, Edges: []api.GraphEdge{
		{From: "a", To: "b", Kind: api.GraphEdgeDepends},
		{From: "b", To: "c", Kind: api.GraphEdgeDepends},
	}}
}

// up arranca en orden de depends, y un lazy del que depende un eager arranca
// con él; freeze va al revés y thaw otra vez al derecho; la pausa de un
// snapshot, como freeze.
func TestGrafoDependsOrden(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g, err := e.m.GraphUp(context.Background(), grafoCadena(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.vistos(), "|"); got != "run cadena-c|run cadena-b|run cadena-a|run cadena-d" {
		t.Fatalf("up: %s", got)
	}
	if g.Nodes["c"].MachineID == "" {
		t.Fatal("el lazy del que depende b se quedó sin máquina")
	}
	e.olvidar()
	if _, err := e.m.GraphFreeze(context.Background(), "cadena"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.GraphThaw(context.Background(), "cadena"); err != nil {
		t.Fatal(err)
	}
	want := "freeze a|freeze b|freeze c|freeze d|thaw c|thaw b|thaw a|thaw d"
	if got := strings.Join(e.vistos(), "|"); got != want {
		t.Fatalf("freeze/thaw:\n got %s\nwant %s", got, want)
	}
	e.olvidar()
	if _, err := e.m.GraphSnapshot(context.Background(), "cadena", ""); err != nil {
		t.Fatal(err)
	}
	var pausas, reanudas []string
	for _, v := range e.vistos() {
		if n, ok := strings.CutPrefix(v, "pause "); ok {
			pausas = append(pausas, n)
		}
		if n, ok := strings.CutPrefix(v, "resume "); ok {
			reanudas = append(reanudas, n)
		}
	}
	if strings.Join(pausas, ",") != "a,b,c,d" || strings.Join(reanudas, ",") != "c,b,a,d" {
		t.Fatalf("snapshot: pausas %v, reanudaciones %v", pausas, reanudas)
	}
}

// Un nodo que despierta por su primera conexión despierta antes lo que
// depende: web -> api (link), api lazy depende de db lazy.
func TestGrafoDependsAlDespertar(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := grafoTienda(false)
	n := g.Nodes["api"]
	n.Wake = api.GraphWakeLazy
	g.Nodes["api"] = n
	g.Edges = append(g.Edges, api.GraphEdge{From: "api", To: "db", Kind: api.GraphEdgeDepends})
	gg := e.montarGrafo(g)
	if e.maquina(gg.ID, "api") != "" || e.maquina(gg.ID, "db") != "" {
		t.Fatal("los lazy nacieron con máquina")
	}
	e.olvidar()
	web := e.maquina(gg.ID, "web")
	addr, _, err := e.m.resolverArista(context.Background(), web, gg.ID, "web", "api", 8081, api.GraphEdgeLink)
	if got := strings.Join(e.vistos(), "|"); got != "run tienda-db|run tienda-api" {
		t.Fatalf("despertar: %s", got)
	}
	e.comprobarDireccion(addr, err, e.maquina(gg.ID, "api"), 8081)

	// Congelado todo, un despertar de api descongela db antes.
	for _, nodo := range []string{"api", "db"} {
		e.m.mu.Lock()
		e.m.byID[e.maquinaSinLock(gg.ID, nodo)].State = api.StateWarm
		e.m.mu.Unlock()
	}
	e.olvidar()
	_, _, _ = e.m.resolverArista(context.Background(), web, gg.ID, "web", "api", 8081, api.GraphEdgeLink)
	if got := strings.Join(e.vistos(), "|"); got != "thaw db|thaw api" {
		t.Fatalf("descongelar: %s", got)
	}
}

// Si la dependencia no arranca, el nodo que depende de ella tampoco.
func TestGrafoDependsFallaLaDependencia(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	e.fallarArranque = func(nombre string) error {
		if nombre == "cadena-c" {
			return errors.New("imagen rota")
		}
		return nil
	}
	_, err := e.m.GraphUp(context.Background(), grafoCadena(), nil)
	if err == nil || !strings.Contains(err.Error(), "imagen rota") {
		t.Fatalf("esperaba el fallo de c, llegó %v", err)
	}
	for _, v := range e.vistos() {
		if v == "run cadena-a" || v == "run cadena-b" {
			t.Fatalf("arrancó %s sin su dependencia", v)
		}
	}
	if n := len(e.m.List()); n != 0 {
		t.Fatalf("quedaron %d máquinas", n)
	}
}

// Una depends con puerto espera a que ese puerto conteste (Linux); en macOS
// el host no marca a los invitados y es un 501.
func TestGrafoDependsConPuerto(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := grafoCadena()
	g.Edges[0].Port = 8081
	out, err := e.m.GraphUp(context.Background(), g, nil)
	if !modeloAPosible {
		var se *api.StatusError
		if !errors.As(err, &se) || se.Code != 501 || !strings.Contains(err.Error(), "Linux-only") {
			t.Fatalf("macOS: esperaba 501, llegó %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	b := out.Nodes["b"].MachineID
	e.m.mu.RLock()
	want := net.JoinHostPort(knet.Plan(e.m.byID[b].NetIndex, b).NSIP, "8081")
	e.m.mu.RUnlock()
	e.mu.Lock()
	puertos := append([]string(nil), e.puertos...)
	e.mu.Unlock()
	if len(puertos) != 1 || puertos[0] != want {
		t.Fatalf("esperó a %v, esperaba %s", puertos, want)
	}
	// Si el puerto no contesta, a no arranca.
	esperarPuertoGrafo = func(context.Context, string) error { return errors.New("sin respuesta") }
	g.Name = "cadena2"
	if _, err := e.m.GraphUp(context.Background(), g, nil); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("puerto mudo: %v", err)
	}
}

// grafoTaller: web (ro) y worker (rw, lazy) ven la carpeta /data de files.
func grafoTaller() api.Graph {
	return api.Graph{Name: "taller", Nodes: map[string]api.GraphNode{
		"files":  {Image: "min"},
		"web":    {Image: "min"},
		"worker": {Image: "min", Wake: api.GraphWakeLazy},
	}, Edges: []api.GraphEdge{
		{From: "web", To: "files", Kind: api.GraphEdgeShare, Mount: "/data"},
		{From: "worker", To: "files", Kind: api.GraphEdgeShare, Mount: "/data", Mode: "rw"},
	}}
}

// share: la carpeta es del grafo (en su directorio, 0700), el dueño la monta
// rw y los demás con su modo; solo el contexto del nodo permite montarla sin
// daemon.share_roots; rm la borra; snapshot y fork se niegan.
func TestGrafoShare(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g, err := e.m.GraphUp(context.Background(), grafoTaller(), nil)
	if err != nil {
		t.Fatal(err)
	}
	carpeta := e.m.carpetaGrafo(g.ID, "files", "/data")
	if !strings.HasPrefix(carpeta, e.m.dirCarpetasGrafo(g.ID)+string(os.PathSeparator)) || strings.HasSuffix(carpeta, "data") {
		t.Fatalf("carpeta: %s", carpeta)
	}
	fi, err := os.Stat(carpeta)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("la carpeta del grafo: %v %v", fi, err)
	}
	e.mu.Lock()
	files, web, permWeb := e.peticiones["taller-files"], e.peticiones["taller-web"], e.permitidas["taller-web"]
	_, worker := e.peticiones["taller-worker"]
	e.mu.Unlock()
	if len(files.Shares) != 1 || files.Shares[0] != (api.ShareSpec{Mode: "rw", Mount: "/data", Source: carpeta}) {
		t.Fatalf("shares del dueño: %+v", files.Shares)
	}
	if len(web.Shares) != 1 || web.Shares[0] != (api.ShareSpec{Mode: "ro", Mount: "/data", Source: carpeta}) {
		t.Fatalf("shares de web: %+v", web.Shares)
	}
	if len(permWeb) != 1 || permWeb[0] != carpeta {
		t.Fatalf("el contexto de web no permitía su carpeta: %v", permWeb)
	}
	if worker {
		t.Fatal("el lazy arrancó con up")
	}
	// El nodo lazy la monta rw al instanciarse.
	if err := e.m.despertarNodoLocked(context.Background(), g.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	wr := e.peticiones["taller-worker"]
	e.mu.Unlock()
	if len(wr.Shares) != 1 || wr.Shares[0].Mode != "rw" || wr.Shares[0].Source != carpeta {
		t.Fatalf("shares de worker: %+v", wr.Shares)
	}

	var se *api.StatusError
	if _, err := e.m.GraphSnapshot(context.Background(), "taller", ""); !errors.As(err, &se) || se.Code != 409 || !strings.Contains(err.Error(), "share edges") {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := e.m.GraphFork(context.Background(), "taller", 1); !errors.As(err, &se) || se.Code != 409 {
		t.Fatalf("fork: %v", err)
	}
	if err := e.m.GraphRemove(context.Background(), "taller"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.m.dirCarpetasGrafo(g.ID)); !os.IsNotExist(err) {
		t.Fatalf("rm dejó las carpetas del grafo: %v", err)
	}
}

// La carpeta de un grafo se acepta sin daemon.share_roots SOLO con el
// contexto que pone instanciarNodo: una petición de fuera con la misma ruta se
// rechaza como cualquier otra.
func TestGrafoShareSoloConSuContexto(t *testing.T) {
	m := newTestManager(t)
	m.SetShareConfig(func() ShareConfig { return ShareConfig{} })
	carpeta := m.carpetaGrafo("0123456789abcdef", "files", "/data")
	if err := os.MkdirAll(carpeta, 0o700); err != nil {
		t.Fatal(err)
	}
	req := api.RunRequest{Shares: []api.ShareSpec{{Mode: "rw", Mount: "/data", Source: carpeta}}}
	if _, err := m.resolveShares(context.Background(), req, nil); err == nil {
		t.Fatal("una petición sin el contexto del grafo montó su carpeta")
	}
	// Otra ruta con el contexto de esta carpeta tampoco.
	otra := api.RunRequest{Shares: []api.ShareSpec{{Mode: "rw", Mount: "/data", Source: t.TempDir()}}}
	if _, err := m.resolveShares(conCarpetasGrafo(context.Background(), []string{carpeta}), otra, nil); err == nil {
		t.Fatal("el contexto de una carpeta dejó pasar otra")
	}
	rs, err := m.resolveShares(conCarpetasGrafo(context.Background(), []string{carpeta}), req, nil)
	if err != nil || len(rs) != 1 || rs[0].att.Source != carpeta || rs[0].att.Mode != "rw" {
		t.Fatalf("con su contexto: %+v %v", rs, err)
	}
}
