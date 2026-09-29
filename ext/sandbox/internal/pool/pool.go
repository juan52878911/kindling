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
}

func Nuevo(reg *hosts.Registro, cada time.Duration, l *log.Logger) *Rellenador {
	if cada <= 0 {
		cada = 30 * time.Second
	}
	if l == nil {
		l = log.Default()
	}
	return &Rellenador{reg: reg, cada: cada, log: l}
}

// Vigilar rellena en segundo plano hasta que se cancele el contexto.
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
			}
		}
	}()
}

// Vuelta mira cada host y crea lo que falte. Nunca destruye: si sobran
// precalentadas —porque bajó el pool de la plantilla— las recoge su TTL.
func (r *Rellenador) Vuelta(ctx context.Context) {
	// Una vuelta cada vez: si una tarda más que el periodo, encadenar otra solo
	// dispararía arranques simultáneos contra el mismo host.
	if !r.mu.TryLock() {
		return
	}
	defer r.mu.Unlock()

	for _, h := range r.reg.Todos() {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		r.vueltaHost(hctx, h)
		r.vueltaGrafos(hctx, h)
		cancel()
	}
}

func (r *Rellenador) vueltaHost(ctx context.Context, h *hosts.Host) {
	snaps, err := h.Cliente.Snapshots(ctx)
	if err != nil {
		return // el host no contesta; ya lo dice `kling sbx hosts`
	}
	vivas, err := h.Cliente.Sandboxes(ctx)
	if err != nil {
		return
	}

	for _, s := range snaps {
		nombre, quiere := DeseoDe(s)
		if quiere <= 0 {
			continue
		}
		hay := 0
		for _, mc := range vivas {
			if mc.Labels[LabelTemplate] == nombre && mc.Labels[LabelTenant] == "" {
				hay++
			}
		}
		for i := hay; i < quiere; i++ {
			if ctx.Err() != nil {
				return
			}
			mc, err := h.Cliente.CreateSandbox(ctx, api.SandboxRequest{
				From:       s.Name,
				TTLSeconds: TTLPrecalentada,
				OnTTL:      api.OnTTLFreeze,
				Egress:     "none",
				Labels:     map[string]string{LabelTemplate: nombre},
			})
			if err != nil {
				// Que no quepa no es un fallo del fondo: es el host diciendo que
				// la RAM es para quien la pide de verdad. Se reintenta en la
				// vuelta siguiente, sin ruido en el log.
				if !api.IsInsufficientMemory(err) && !api.IsMachineLimit(err) {
					r.log.Printf("pool %s/%s: %v", h.Nombre, nombre, err)
				}
				return
			}
			r.log.Printf("pool %s/%s: prewarmed %s (%d/%d)", h.Nombre, nombre, mc.ID[:12], i+1, quiere)
		}
	}
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
