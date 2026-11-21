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

// TTLPrecalentada es el plazo de las máquinas del fondo. Generoso y con
// on_ttl=freeze: una precalentada que nadie reclama se duerme —deja de costar
// RAM— y sigue ahí para el siguiente, que solo paga un thaw.
const TTLPrecalentada = 3600

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
	// maxCreaciones acota las creaciones simultáneas de una vuelta (0:
	// CreacionesALaVez). Solo lo cambian los tests.
	maxCreaciones int
}

// CreacionesALaVez es cuántas precalentadas crea una vuelta a la vez: la misma
// cifra que la puerta de creación del daemon, para que el relleno no haga cola
// delante de las peticiones reales.
const CreacionesALaVez = 4

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
// precalentada de esa plantilla en ese host: Vigilar hace ya una vuelta de ese
// host sin esperar al tic. No bloquea ni acumula: los avisos seguidos son una
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
				r.vueltaPedida(ctx, r.tomarPedidos())
			}
		}
	}()
}

// vueltaPedida rellena solo lo pedido (host -> plantillas).
func (r *Rellenador) vueltaPedida(ctx context.Context, pedido map[string]map[string]bool) {
	if len(pedido) == 0 || !r.mu.TryLock() {
		return
	}
	defer r.mu.Unlock()
	for _, h := range r.reg.Todos() {
		solo := pedido[h.Nombre]
		if solo == nil {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		r.vueltaHost(hctx, h, solo)
		cancel()
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
	defer r.mu.Unlock()

	for _, h := range r.reg.Todos() {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		r.vueltaHost(hctx, h, nil)
		r.vueltaGrafos(hctx, h)
		cancel()
	}
}

// vueltaHost deja el fondo del host como lo quieren sus plantillas; con solo
// no nil, crea únicamente las de esas plantillas (la limpieza es siempre de
// todo el host).
func (r *Rellenador) vueltaHost(ctx context.Context, h *hosts.Host, solo map[string]bool) {
	snaps, err := h.Cliente.Snapshots(ctx)
	if err != nil {
		return // el host no contesta; ya lo dice `kling sbx hosts`
	}
	vivas, err := h.Cliente.Sandboxes(ctx)
	if err != nil {
		return
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
		case rota(mc):
			// Una rota no la reclama el frontal (esPrecalentadaDe): contarla
			// dejaría la plaza ocupada para siempre y cada create caería al
			// restore. Se borra y se fabrica otra.
			motivo = string(mc.State)
		}
		if motivo == "" {
			hay[nombre]++
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
	tope := r.maxCreaciones
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
