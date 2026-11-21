// Package pool mantiene listas unas cuantas instancias de cada plantilla.
//
// Crear un sandbox desde un snapshot ya cuesta ~300 ms, que para un agente que
// escribe código es poco pero no nada. Con el fondo cuesta lo que tarde una
// llamada HTTP: la microVM ya está restaurada y esperando, y reclamarla es
// ponerle una etiqueta.
//
// Vive fuera del frontal a propósito: el frontal RECLAMA precalentadas y este
// paquete las FABRICA, y son dos ritmos distintos. El contrato entre ambos son
// las etiquetas (kind=sandbox, template=X, sin tenant) y la política de la
// máquina (sin red, duerme al vencer); está escrito en esPrecalentadaDe.
//
// También fabrica GRAFOS enteros (plantilla.PlantillaGrafo): levanta la
// instancia con `graph up` y la congela con `graph freeze`, de modo que una
// instancia en el fondo no cuesta RAM y reclamarla es etiquetar y despertar.
// El contrato de esas instancias está en plantilla/grafo.go.
package pool

import (
	"context"
	"encoding/json"
	"log"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas que identifican una precalentada. Son las mismas que usa el frontal,
// que es quien las reclama.
const (
	LabelTemplate = "template"
	LabelTenant   = "tenant"
)

// TTLPrecalentada es el plazo de las máquinas del fondo. Con on_ttl=freeze, una
// precalentada vencida pasa a warm, y el reclamo atómico solo elige running:
// por eso el rellenador renueva el plazo de las running cuando les queda menos
// de un cuarto (el fondo cuesta su RAM siempre) y, con un host que anuncia
// sandbox-claim, repone las warm como si estuvieran rotas.
const TTLPrecalentada = 3600

// retrocesoCreacion es lo que descansa un host/plantilla tras un fallo de
// creación, para no machacar un host lleno o roto en cada vuelta.
const retrocesoCreacion = 5 * time.Second

// Rellenador mantiene el fondo de todas las plantillas de todos los hosts.
type Rellenador struct {
	reg  *hosts.Registro
	cada time.Duration
	log  *log.Logger
	mu   sync.Mutex

	// pedidos despierta a Vigilar fuera de su tic (búfer 1: varios Pedir
	// seguidos son una sola vuelta); pedido guarda qué host y qué plantillas
	// pidió cada uno, bajo pmu.
	pedidos chan struct{}
	pmu     sync.Mutex
	pedido  map[string]map[string]bool
	// ultimoPedido es el instante del último Pedir (bajo pmu).
	ultimoPedido time.Time
	// maxCreaciones acota las creaciones simultáneas de una vuelta (0:
	// CreacionesALaVez). Solo lo cambian los tests.
	maxCreaciones int
	// retroceso: host/plantilla -> hasta cuándo no se intenta crear.
	rmu       sync.Mutex
	retroceso map[string]time.Time
}

// CreacionesALaVez es cuántas precalentadas crea una vuelta a la vez: la misma
// cifra que la puerta de creación del daemon, para que el relleno no haga cola
// delante de las peticiones reales.
const CreacionesALaVez = 4

// CreacionesPedidas es cuántas crea a la vez la vuelta que dispara Pedir: tras
// un reclamo el usuario está haciendo sus primeras peticiones sobre la misma
// máquina, y el relleno no debe competir con ellas por la CPU.
const CreacionesPedidas = 2

// EsperaPedido es lo que se espera desde el último reclamo antes de rellenar.
const EsperaPedido = 300 * time.Millisecond

func Nuevo(reg *hosts.Registro, cada time.Duration, l *log.Logger) *Rellenador {
	if cada <= 0 {
		cada = 30 * time.Second
	}
	if l == nil {
		l = log.Default()
	}
	return &Rellenador{reg: reg, cada: cada, log: l, pedidos: make(chan struct{}, 1)}
}

// Pedir avisa de que el frontal acaba de gastar (o de no encontrar) una
// precalentada de esa plantilla en ese host: Vigilar hace una vuelta de ese
// host sin esperar al tic, pero tras EsperaPedido desde el último aviso (salvo
// que el fondo de la plantilla esté vacío). No bloquea ni acumula: los avisos seguidos son una
// sola vuelta.
func (r *Rellenador) Pedir(host, plantilla string) {
	r.pmu.Lock()
	if r.pedido == nil {
		r.pedido = map[string]map[string]bool{}
	}
	if r.pedido[host] == nil {
		r.pedido[host] = map[string]bool{}
	}
	r.pedido[host][plantilla] = true
	r.ultimoPedido = time.Now()
	r.pmu.Unlock()
	select {
	case r.pedidos <- struct{}{}:
	default:
	}
}

// tomarPedidos vacía y devuelve lo pedido desde la última vez.
func (r *Rellenador) tomarPedidos() map[string]map[string]bool {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	p := r.pedido
	r.pedido = nil
	return p
}

// Vigilar rellena en segundo plano hasta que se cancele el contexto: una vuelta
// entera por tic y, entre tics, la del host y la plantilla que pide Pedir.
func (r *Rellenador) Vigilar(ctx context.Context) {
	go func() {
		t := time.NewTicker(r.cada)
		defer t.Stop()
		r.Vuelta(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Vuelta(ctx)
			case <-r.pedidos:
				r.esperarPedidos(ctx)
				r.vueltaPedida(ctx, r.tomarPedidos())
			}
		}
	}()
}

// esperarPedidos deja pasar EsperaPedido desde el último Pedir (los reclamos
// seguidos la van alargando). Si el fondo de alguna plantilla pedida está
// vacío no espera: el siguiente create caería al restore.
func (r *Rellenador) esperarPedidos(ctx context.Context) {
	for {
		r.pmu.Lock()
		falta := EsperaPedido - time.Since(r.ultimoPedido)
		pedido := map[string]map[string]bool{}
		for h, ps := range r.pedido {
			pedido[h] = maps.Clone(ps)
		}
		r.pmu.Unlock()
		if falta <= 0 || r.fondoVacio(ctx, pedido) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(falta):
		}
	}
}

// fondoVacio dice si alguna de las plantillas pedidas no tiene ninguna
// precalentada reclamable en su host.
func (r *Rellenador) fondoVacio(ctx context.Context, pedido map[string]map[string]bool) bool {
	for _, h := range r.reg.Todos() {
		solo := pedido[h.Nombre]
		if len(solo) == 0 {
			continue
		}
		vivas, err := h.Cliente.Sandboxes(ctx)
		if err != nil {
			continue
		}
		hay := map[string]bool{}
		for _, mc := range vivas {
			if n := mc.Labels[LabelTemplate]; n != "" && mc.Labels[LabelTenant] == "" &&
				plantilla.DelFrontal(mc.Labels) && !rota(mc) && mc.State != api.StateWarm {
				hay[n] = true
			}
		}
		for n := range solo {
			if !hay[n] {
				return true
			}
		}
	}
	return false
}

// vueltaPedida rellena solo lo pedido (host -> plantillas), de
// CreacionesPedidas en CreacionesPedidas. Si hay otra vuelta en curso el pedido
// vuelve al mapa y se reintenta al acabar ésta.
func (r *Rellenador) vueltaPedida(ctx context.Context, pedido map[string]map[string]bool) {
	if len(pedido) == 0 {
		return
	}
	if !r.mu.TryLock() {
		r.devolverPedido(pedido)
		return
	}
	for _, h := range r.reg.Todos() {
		solo := pedido[h.Nombre]
		if solo == nil {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		r.vueltaHost(hctx, h, solo, CreacionesPedidas)
		cancel()
	}
	r.mu.Unlock()
	r.reavisar()
}

// devolverPedido reincorpora un pedido que no se pudo atender.
func (r *Rellenador) devolverPedido(pedido map[string]map[string]bool) {
	r.pmu.Lock()
	if r.pedido == nil {
		r.pedido = map[string]map[string]bool{}
	}
	for h, ps := range pedido {
		if r.pedido[h] == nil {
			r.pedido[h] = map[string]bool{}
		}
		for p := range ps {
			r.pedido[h][p] = true
		}
	}
	r.pmu.Unlock()
}

// reavisar despierta a Vigilar si quedó algún pedido sin atender. Se llama con
// el cerrojo ya suelto, para no girar mientras otra vuelta lo tiene.
func (r *Rellenador) reavisar() {
	r.pmu.Lock()
	hay := len(r.pedido) > 0
	r.pmu.Unlock()
	if hay {
		select {
		case r.pedidos <- struct{}{}:
		default:
		}
	}
}

// Vuelta mira cada host y crea lo que falte. Solo destruye las precalentadas
// rotas (failed, stopped); si sobran sanas —porque bajó el pool de la
// plantilla— las recoge su TTL.
func (r *Rellenador) Vuelta(ctx context.Context) {
	// Una vuelta cada vez: si una tarda más que el periodo, encadenar otra solo
	// dispararía arranques simultáneos contra el mismo host.
	if !r.mu.TryLock() {
		return
	}

	for _, h := range r.reg.Todos() {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		r.vueltaHost(hctx, h, nil, 0)
		r.vueltaGrafos(hctx, h)
		cancel()
	}
	r.mu.Unlock()
	r.reavisar()
}

// vueltaHost deja el fondo del host como lo quieren sus plantillas; con solo
// no nil, crea únicamente las de esas plantillas (la limpieza es siempre de
// todo el host). simultaneas acota las creaciones a la vez (0: el valor de
// maxCreaciones o CreacionesALaVez).
func (r *Rellenador) vueltaHost(ctx context.Context, h *hosts.Host, solo map[string]bool, simultaneas int) {
	snaps, err := h.Cliente.Snapshots(ctx)
	if err != nil {
		r.log.Printf("pool %s: listing snapshots: %v", h.Nombre, err)
		return
	}
	vivas, err := h.Cliente.Sandboxes(ctx)
	if err != nil {
		r.log.Printf("pool %s: listing sandboxes: %v", h.Nombre, err)
		return
	}
	// Con sandbox-claim el frontal solo reclama running: una warm no sirve.
	reclama := false
	if info, err := h.Cliente.Info(ctx); err == nil {
		reclama = info.Has(api.CapabilityClaim)
	}

	// La versión de cada plantilla que el host tiene ahora. Una precalentada sin
	// dueño de otra versión (un rebuild reemplazó el snapshot) o de una
	// plantilla que ya no está (template rm) no la va a reclamar nadie: el
	// frontal exige la versión de ahora. Se borra, y si la plantilla sigue, se
	// fabrica otra de su snapshot actual.
	version := map[string]string{}
	for _, s := range snaps {
		if nombre, _ := DeseoDe(s); nombre != "" {
			version[nombre] = plantilla.VersionDe(s)
		}
	}
	hay := map[string]int{}
	for _, mc := range vivas {
		nombre := mc.Labels[LabelTemplate]
		if nombre == "" || mc.Labels[LabelTenant] != "" || !plantilla.DelFrontal(mc.Labels) {
			continue
		}
		v, sigue := version[nombre]
		motivo := ""
		switch {
		case !sigue:
			motivo = "orphaned (its template is gone)"
		case mc.Labels[plantilla.LabelVersion] != v:
			motivo = "stale (its template was rebuilt)"
		case rota(mc) || (reclama && mc.State == api.StateWarm):
			// Una rota no la reclama el frontal (esPrecalentadaDe): contarla
			// dejaría la plaza ocupada para siempre y cada create caería al
			// restore. Se borra y se fabrica otra.
			motivo = string(mc.State)
		}
		if motivo == "" {
			hay[nombre]++
			if mc.State == api.StateRunning && seVaADormir(mc) {
				if _, err := h.Cliente.RenewSandbox(ctx, mc.ID, TTLPrecalentada); err != nil {
					r.log.Printf("pool %s/%s: renewing prewarmed %s: %v", h.Nombre, nombre, shortID(mc.ID), err)
				}
			}
			continue
		}
		if err := h.Cliente.Remove(ctx, mc.ID); err != nil {
			r.log.Printf("pool %s/%s: removing the %s prewarmed %s: %v", h.Nombre, nombre, motivo, shortID(mc.ID), err)
		} else {
			r.log.Printf("pool %s/%s: removed the %s prewarmed %s", h.Nombre, nombre, motivo, shortID(mc.ID))
		}
	}

	// Lo que falta se crea en paralelo, de CreacionesALaVez en
	// CreacionesALaVez. Cada plantilla se para al primer fallo (que no quepa
	// no es un fallo del fondo: es el host diciendo que la RAM es para quien
	// la pide de verdad; se reintenta en la vuelta siguiente, sin ruido en el
	// log) y se sigue con las demás: puede caber aunque esta no.
	tope := simultaneas
	if tope <= 0 {
		tope = r.maxCreaciones
	}
	if tope <= 0 {
		tope = CreacionesALaVez
	}
	sem := make(chan struct{}, tope)
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, s := range snaps {
		nombre, quiere := DeseoDe(s)
		if quiere <= 0 || (solo != nil && !solo[nombre]) {
			continue
		}
		etiquetas := map[string]string{LabelTemplate: nombre}
		if v := version[nombre]; v != "" {
			etiquetas[plantilla.LabelVersion] = v
		}
		if r.enRetroceso(h.Nombre + "/" + nombre) {
			continue
		}
		var parada atomic.Bool
		var hechas atomic.Int32
		hechas.Store(int32(hay[nombre]))
		for i := hay[nombre]; i < quiere; i++ {
			sem <- struct{}{}
			if ctx.Err() != nil {
				<-sem
				return
			}
			if parada.Load() {
				<-sem
				break
			}
			wg.Add(1)
			go func(from string) {
				defer wg.Done()
				defer func() { <-sem }()
				mc, err := h.Cliente.CreateSandbox(ctx, api.SandboxRequest{
					From:       from,
					TTLSeconds: TTLPrecalentada,
					OnTTL:      api.OnTTLFreeze,
					Egress:     "none",
					Labels:     etiquetas,
				})
				if err != nil {
					r.retroceder(h.Nombre + "/" + nombre)
					if !parada.Swap(true) && !api.IsInsufficientMemory(err) && !api.IsMachineLimit(err) {
						r.log.Printf("pool %s/%s: %v", h.Nombre, nombre, err)
					}
					return
				}
				r.log.Printf("pool %s/%s: prewarmed %s (%d/%d)", h.Nombre, nombre, shortID(mc.ID), hechas.Add(1), quiere)
			}(s.Name)
		}
	}
}

func (r *Rellenador) enRetroceso(clave string) bool {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	return time.Now().Before(r.retroceso[clave])
}

func (r *Rellenador) retroceder(clave string) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.retroceso == nil {
		r.retroceso = map[string]time.Time{}
	}
	r.retroceso[clave] = time.Now().Add(retrocesoCreacion)
}

// seVaADormir dice si a una precalentada running le queda menos de un cuarto
// de su plazo. Sin reloj conocido (TTLAt) no se toca.
func seVaADormir(mc *api.Machine) bool {
	if mc.TTLSeconds <= 0 || mc.TTLAt == nil {
		return false
	}
	plazo := time.Duration(mc.TTLSeconds) * time.Second
	return time.Until(mc.TTLAt.Add(plazo)) < plazo/4
}

// rota dice si una precalentada ya no sirve: ni corre ni duerme, y no va a
// volver sola a ninguno de los dos.
func rota(mc *api.Machine) bool {
	return mc.State == api.StateFailed || mc.State == api.StateStopped
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// DeseoDe saca de la anotación del snapshot cuántas precalentadas quiere esa
// plantilla. La receta viaja con el snapshot, así que el fondo no necesita que
// nadie le pase una lista de plantillas: la lee de donde ya está.
func DeseoDe(s *api.Snapshot) (nombre string, quiere int) {
	if !strings.HasPrefix(s.Name, "sbx-") {
		return "", 0
	}
	raw, ok := s.Annotations[plantilla.AnotacionReceta]
	if !ok {
		return "", 0
	}
	var r struct {
		Plantilla plantilla.Plantilla `json:"template"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return "", 0
	}
	return r.Plantilla.Nombre, r.Plantilla.Pool
}

// vueltaGrafos fabrica las instancias de grafo que falten en el host. Como
// con las máquinas, nunca destruye lo que sobra: una instancia libre de más se
// queda congelada (disco, no RAM) hasta que alguien la reclame, o hasta que
// `kling sbx template rm` borre la plantilla y sus instancias libres.
func (r *Rellenador) vueltaGrafos(ctx context.Context, h *hosts.Host) {
	tpls, err := plantilla.Grafos(ctx, h.Cliente)
	if err != nil || len(tpls) == 0 {
		return
	}
	gs, err := h.Cliente.Graphs(ctx)
	if err != nil {
		return // un daemon sin la capacidad "graphs" no tiene grafos que llenar
	}
	ms, err := h.Cliente.List(ctx)
	if err != nil {
		return
	}
	libres := map[string]int{}
	for _, in := range plantilla.Instancias(gs, ms) {
		if in.Libre() {
			libres[in.Plantilla]++
		}
	}
	for _, p := range tpls {
		for i := libres[p.Nombre]; i < p.Pool; i++ {
			if ctx.Err() != nil {
				return
			}
			g, err := plantilla.FabricarGrafo(ctx, h.Cliente, p)
			if err != nil {
				if !api.IsInsufficientMemory(err) && !api.IsMachineLimit(err) {
					r.log.Printf("pool %s/graph %s: %v", h.Nombre, p.Nombre, err)
				}
				break // la siguiente plantilla puede caber aunque esta no
			}
			r.log.Printf("pool %s/graph %s: prewarmed %s (%d/%d)", h.Nombre, p.Nombre, g.Name, i+1, p.Pool)
		}
	}
}
