package frontal

// GET /v1/templates: qué plantillas hay listas para pedir y en qué hosts,
// para que un cliente (u operador) sepa qué -template puede pedir sin tener
// acceso directo a los daemons de kindling.
//
// De solo lectura y sin nada propio: lee los snapshots "sbx-*" de cada host y
// la receta de su anotación, la misma que escribe plantilla.Construir. Un
// snapshot sin esa anotación (de antes de que existiera, o corrupto) sigue
// apareciendo, solo que sin receta ni fecha.
//
// Las plantillas de grafo (plantilla/grafo.go) salen también, con kind
// "graph": se piden con POST /v1/graphs, no con POST /v1/sandboxes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// TemplateHost es lo que hay de una plantilla en un host concreto.
type TemplateHost struct {
	Host      string    `json:"host"`
	Snapshot  string    `json:"snapshot"`
	BuiltAt   time.Time `json:"built_at,omitempty"`
	Recipe    string    `json:"recipe_sha256,omitempty"`
	Instances int       `json:"instances,omitempty"`
}

// TemplateInfo es una plantilla vista desde el frontal: su nombre y en qué
// hosts está construida.
type TemplateInfo struct {
	Name string `json:"name"`
	// Kind es "graph" en una plantilla de grafo; vacío en una de máquina.
	Kind  string         `json:"kind,omitempty"`
	Hosts []TemplateHost `json:"hosts"`
}

func (s *Servidor) handleTemplates(w http.ResponseWriter, r *http.Request) {
	todos := s.reg.Todos()
	porHost := make([][]*api.Snapshot, len(todos))
	grafosPorHost := make([][]plantilla.PlantillaGrafo, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			snaps, err := h.Cliente.Snapshots(ctx)
			if err != nil {
				s.log.Printf("frontal: templates: %s: %v", h.Nombre, err)
				return
			}
			porHost[i] = snaps
			// Sin la capacidad "store" (o sin plantillas de grafo) no hay nada
			// que añadir: no es un fallo del host.
			grafosPorHost[i], _ = plantilla.Grafos(ctx, h.Cliente)
		}(i, h)
	}
	wg.Wait()

	porNombre := map[string]*TemplateInfo{}
	var orden []string
	for i, h := range todos {
		for _, sn := range porHost[i] {
			if !strings.HasPrefix(sn.Name, "sbx-") {
				continue
			}
			nombre := strings.TrimPrefix(sn.Name, "sbx-")
			ti, ok := porNombre[nombre]
			if !ok {
				ti = &TemplateInfo{Name: nombre}
				porNombre[nombre] = ti
				orden = append(orden, nombre)
			}
			th := TemplateHost{Host: h.Nombre, Snapshot: sn.Name, Instances: sn.Instances}
			var rec plantilla.Receta
			if ok, err := sn.Annotation(plantilla.AnotacionReceta, &rec); ok && err == nil {
				th.BuiltAt, th.Recipe = rec.Hecho, rec.Hash
			}
			ti.Hosts = append(ti.Hosts, th)
		}
		for _, pg := range grafosPorHost[i] {
			// Clave aparte: una plantilla de grafo puede llamarse como una de
			// máquina, y son dos cosas distintas.
			clave := claveGrafo(pg.Nombre)
			ti, ok := porNombre[clave]
			if !ok {
				ti = &TemplateInfo{Name: pg.Nombre, Kind: plantilla.KindGrafo}
				porNombre[clave] = ti
				orden = append(orden, clave)
			}
			ti.Hosts = append(ti.Hosts, TemplateHost{Host: h.Nombre})
		}
	}

	sort.Strings(orden)
	out := make([]TemplateInfo, 0, len(orden))
	for _, n := range orden {
		ti := porNombre[n]
		sort.Slice(ti.Hosts, func(i, j int) bool { return ti.Hosts[i].Host < ti.Hosts[j].Host })
		out = append(out, *ti)
	}
	writeJSON(w, http.StatusOK, out)
}

// PoolHost es el fondo de una plantilla en un host.
type PoolHost struct {
	Host  string `json:"host"`
	Size  int    `json:"size"`
	Ready int    `json:"ready"`
	// Error es lo que impidió fijar el fondo en este host.
	Error string `json:"error,omitempty"`
	// Stale y Error vienen del daemon (api.PoolInfo): el dorado no se puede
	// restaurar con el VMM de ahora.
	Stale string `json:"stale,omitempty"`
}

// PoolRespuesta es la respuesta de PUT /v1/templates/{name}/pool. Available
// es la suma de copias despiertas y reclamables en todos los hosts.
type PoolRespuesta struct {
	Template  string     `json:"template"`
	Size      int        `json:"size"`
	Available int        `json:"available"`
	Hosts     []PoolHost `json:"hosts"`
}

// handlePool sirve PUT /v1/templates/{name}/pool (solo admin): fija el tamaño
// del fondo precalentado de la plantilla en CADA host que la tiene (size 0 lo
// apaga). Es la ruta que usa el operador de Kubernetes para SandboxPool.
//
// Un host que no contesta no impide fijarlo en los demás: se informa en
// hosts[].error, y solo si ninguno lo consigue la respuesta es un 502.
func (s *Servidor) handlePool(w http.ResponseWriter, r *http.Request) {
	if !tenantDe(r).Admin {
		fail(w, http.StatusForbidden, errors.New("setting a template pool needs an admin token"))
		return
	}
	nombre := r.PathValue("name")
	var pet api.PoolRequest
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if pet.Size < 0 || pet.Size > api.PoolMax {
		fail(w, http.StatusBadRequest, fmt.Errorf("size must be between 0 and %d", api.PoolMax))
		return
	}
	snap := "sbx-" + nombre
	todos := s.reg.Todos()
	res := make([]*PoolHost, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			snaps, err := h.Cliente.Snapshots(ctx)
			if err != nil {
				res[i] = &PoolHost{Host: h.Nombre, Error: deHost(h, err).Error()}
				return
			}
			tiene := false
			for _, sn := range snaps {
				tiene = tiene || sn.Name == snap
			}
			if !tiene {
				return
			}
			info, err := h.Cliente.SetSnapshotPool(ctx, snap, pet.Size)
			if err != nil {
				res[i] = &PoolHost{Host: h.Nombre, Error: deHost(h, err).Error()}
				return
			}
			res[i] = &PoolHost{Host: h.Nombre, Size: info.Size, Ready: info.Ready, Stale: info.Stale}
			if info.Error != "" {
				res[i].Error = info.Error
			}
		}(i, h)
	}
	wg.Wait()
	out := PoolRespuesta{Template: nombre, Size: pet.Size, Hosts: []PoolHost{}}
	fallos := 0
	for _, ph := range res {
		if ph == nil {
			continue
		}
		out.Hosts = append(out.Hosts, *ph)
		out.Available += ph.Ready
		if ph.Error != "" && ph.Size != pet.Size {
			fallos++
		}
	}
	switch {
	case len(out.Hosts) == 0:
		fail(w, http.StatusNotFound, fmt.Errorf("no host has the template %q", nombre))
	case fallos == len(out.Hosts):
		fail(w, http.StatusBadGateway, fmt.Errorf("could not set the pool of %q on any host: %s", nombre, out.Hosts[0].Error))
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
