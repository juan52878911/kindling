package frontal

// Grafos precalentados: un entorno entero (el sandbox del agente, su Postgres,
// su caché...) que se reclama como una máquina.
//
// El fondo (paquete pool) levanta instancias de cada plantilla de grafo y las
// deja congeladas; reclamar una es etiquetar todas sus máquinas con el tenant,
// comprobar que la etiqueta quedó (otro frontal pudo reclamarla a la vez) y
// despertarla con `graph thaw`. Si no hay ninguna libre se levanta una nueva ya
// etiquetada. El contrato de etiquetas está en plantilla/grafo.go.
//
// PROPIEDAD. Igual que con los sandboxes: /v1/graphs/{host}/{id} solo resuelve
// si TODAS las máquinas del grafo llevan el tenant que llama, y cualquier otra
// cosa es el mismo 404. Los nodos se entregan con un id "host/máquina" que las
// rutas exec, files y shell de /v1/sandboxes aceptan (buscarEjecutable); el
// resto de rutas de sandbox (ver, renovar, borrar) no, porque un nodo solo se
// suelta con su grafo entero.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// margenGrafoRoto es cuánto tiene que haber vivido un grafo roto (a medias o
// con etiquetas mezcladas) antes de que la limpieza lo considere: un `graph
// up` en curso también está a medias, y tarda lo que tarden sus nodos.
const margenGrafoRoto = 10 * time.Minute

// Grafo es lo que ve el cliente de un grafo reclamado.
type Grafo struct {
	ID        string               `json:"id"`
	Host      string               `json:"host"`
	Template  string               `json:"template"`
	State     string               `json:"state"`
	ClaimedAt *time.Time           `json:"claimed_at,omitempty"`
	Nodes     map[string]NodoGrafo `json:"nodes"`
}

// NodoGrafo es un nodo de un grafo reclamado. Su ID vale para
// /v1/sandboxes/{id}/exec, /files y /shell.
type NodoGrafo struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Ports []int  `json:"ports,omitempty"`
}

// CrearGrafoPeticion es el cuerpo de POST /v1/graphs.
type CrearGrafoPeticion struct {
	Template string `json:"template"`
}

// claveGrafo es la clave de cuota de una plantilla de grafo. Los nombres de
// plantilla no llevan ':', así que no choca con ninguna de máquina.
func claveGrafo(nombre string) string { return "graph:" + nombre }

func vistaGrafo(h *hosts.Host, in plantilla.Instancia) Grafo {
	g := Grafo{
		ID: h.Nombre + "/" + in.Grafo.ID, Host: h.Nombre, Template: in.Plantilla,
		State: in.Grafo.State, Nodes: map[string]NodoGrafo{},
	}
	if ts := in.ReclamadaEn(); ts > 0 {
		c := time.Unix(ts, 0).UTC()
		g.ClaimedAt = &c
	}
	for nombre, n := range in.Grafo.Nodes {
		nd := NodoGrafo{State: n.State, Ports: n.Ports}
		if mc := in.Maquinas[nombre]; mc != nil {
			nd.ID = h.Nombre + "/" + mc.ID
		}
		g.Nodes[nombre] = nd
	}
	return g
}

// instancias lee los grafos del pool de un host.
func instancias(ctx context.Context, h *hosts.Host) ([]plantilla.Instancia, error) {
	gs, err := h.Cliente.Graphs(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := h.Cliente.List(ctx)
	if err != nil {
		return nil, err
	}
	return plantilla.Instancias(gs, ms), nil
}

// instanciasEnCadaHost es instancias() contra todos los hosts a la vez.
func (s *Servidor) instanciasEnCadaHost(ctx context.Context) (map[*hosts.Host][]plantilla.Instancia, []error) {
	todos := s.reg.Todos()
	out := make([][]plantilla.Instancia, len(todos))
	errs := make([]error, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out[i], errs[i] = instancias(ctx, h)
		}(i, h)
	}
	wg.Wait()
	res := map[*hosts.Host][]plantilla.Instancia{}
	var fallos []error
	for i, h := range todos {
		if errs[i] != nil {
			// Un daemon sin la capacidad "graphs" no tiene grafos: no es un fallo.
			if !api.IsUnsupported(errs[i]) {
				fallos = append(fallos, fmt.Errorf("%s: %w", h.Nombre, errs[i]))
			}
			continue
		}
		res[h] = out[i]
	}
	return res, fallos
}

// grafosDe lista los grafos del tenant en todos los hosts.
func (s *Servidor) grafosDe(ctx context.Context, t *Tenant) ([]Grafo, []error) {
	porHost, errs := s.instanciasEnCadaHost(ctx)
	var out []Grafo
	for h, ins := range porHost {
		for _, in := range ins {
			if in.De(t.Nombre) {
				out = append(out, vistaGrafo(h, in))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}

// buscarGrafo resuelve un id a su host y su instancia, SOLO si es del tenant.
func (s *Servidor) buscarGrafo(ctx context.Context, t *Tenant, id string) (*hosts.Host, plantilla.Instancia, error) {
	nombre, ref, ok := partirID(id)
	if !ok {
		return nil, plantilla.Instancia{}, noSandbox(id)
	}
	h, ok := s.reg.Host(nombre)
	if !ok {
		return nil, plantilla.Instancia{}, noSandbox(id)
	}
	ins, err := instancias(ctx, h)
	if err != nil {
		return nil, plantilla.Instancia{}, fmt.Errorf("%s: %w", h.Nombre, err)
	}
	for _, in := range ins {
		if in.Grafo.ID == ref && in.De(t.Nombre) {
			return h, in, nil
		}
	}
	return nil, plantilla.Instancia{}, noSandbox(id)
}

// buscarEjecutable es buscar() más los nodos de los grafos del tenant: lo que
// usan exec, files y shell. Un nodo es suyo si su GRAFO entero es suyo
// (Instancia.De): no basta con la etiqueta de la máquina, porque en un grafo
// mezclado por dos reclamaciones a la vez alguna máquina lleva el tenant del
// que perdió.
func (s *Servidor) buscarEjecutable(ctx context.Context, t *Tenant, id string) (*hosts.Host, *api.Machine, error) {
	h, mc, err := s.buscar(ctx, t, id)
	var ns *errNoSandbox
	if err == nil || !errors.As(err, &ns) {
		return h, mc, err
	}
	nombre, ref, _ := partirID(id)
	h, ok := s.reg.Host(nombre)
	if !ok {
		return nil, nil, err
	}
	ins, ierr := instancias(ctx, h)
	if ierr != nil {
		if api.IsUnsupported(ierr) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%s: %w", h.Nombre, ierr)
	}
	for _, in := range ins {
		if !in.De(t.Nombre) {
			continue
		}
		for _, m := range in.Maquinas {
			if m.ID == ref {
				return h, m, nil
			}
		}
	}
	return nil, nil, err
}

// ---- rutas

func (s *Servidor) handleListarGrafos(w http.ResponseWriter, r *http.Request) {
	out, errs := s.grafosDe(r.Context(), tenantDe(r))
	if out == nil {
		out = []Grafo{}
	}
	if len(errs) > 0 {
		w.Header().Set("X-Kindling-Partial", strings.Join(mensajes(errs), "; "))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGrafo despacha /v1/graphs/{host}/{id}.
func (s *Servidor) handleGrafo(w http.ResponseWriter, r *http.Request) {
	resto := strings.TrimPrefix(r.URL.Path, "/v1/graphs/")
	partes := strings.Split(resto, "/")
	if len(partes) != 2 || partes[0] == "" || partes[1] == "" {
		http.NotFound(w, r)
		return
	}
	id := partes[0] + "/" + partes[1]
	switch r.Method {
	case http.MethodGet:
		h, in, err := s.buscarGrafo(r.Context(), tenantDe(r), id)
		if err != nil {
			fail(w, codigoBuscar(err), err)
			return
		}
		writeJSON(w, http.StatusOK, vistaGrafo(h, in))
	case http.MethodDelete:
		h, in, err := s.buscarGrafo(r.Context(), tenantDe(r), id)
		if err != nil {
			fail(w, codigoBuscar(err), err)
			return
		}
		if err := h.Cliente.GraphRemove(r.Context(), in.Grafo.ID); err != nil {
			fail(w, http.StatusBadGateway, fmt.Errorf("%s: %w", h.Nombre, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	}
}

func (s *Servidor) handleCrearGrafo(w http.ResponseWriter, r *http.Request) {
	t := tenantDe(r)
	var pet CrearGrafoPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if pet.Template == "" {
		fail(w, http.StatusBadRequest, errors.New("missing template"))
		return
	}
	if !nombreValido.MatchString(pet.Template) {
		fail(w, http.StatusBadRequest, fmt.Errorf("template name %q is not valid", pet.Template))
		return
	}
	// Un grafo cuenta como un sandbox en la cuota del tenant, y por su
	// plantilla en MaxPorPlantilla.
	clave := claveGrafo(pet.Template)
	if err := s.reservar(r.Context(), t, clave); err != nil {
		fail(w, http.StatusTooManyRequests, err)
		return
	}
	defer s.liberar(t, clave)

	h, in, err := s.crearGrafo(r.Context(), t, pet.Template)
	if err != nil {
		fail(w, codigoCrear(err), err)
		return
	}
	g := vistaGrafo(h, in)
	w.Header().Set("Location", "/v1/graphs/"+g.ID)
	writeJSON(w, http.StatusCreated, g)
}

// crearGrafo reclama una instancia congelada de la plantilla o, si no hay
// ninguna, levanta una nueva ya a nombre del tenant.
func (s *Servidor) crearGrafo(ctx context.Context, t *Tenant, tpl string) (*hosts.Host, plantilla.Instancia, error) {
	var mu sync.Mutex
	defs := map[string]*plantilla.PlantillaGrafo{}
	tienen := s.hostsCon(ctx, func(ctx context.Context, h *hosts.Host) (bool, error) {
		p, err := plantilla.Grafo(ctx, h.Cliente, tpl)
		if err != nil || p == nil {
			return false, err
		}
		mu.Lock()
		defs[h.Nombre] = p
		mu.Unlock()
		return true, nil
	})
	if len(tienen) == 0 {
		return nil, plantilla.Instancia{}, &errSinPlantilla{tpl}
	}
	sirve := func(h *hosts.Host) bool { return tienen[h.Nombre] }

	if h, in, ok := s.reclamarGrafo(ctx, t, tpl, sirve); ok {
		return h, in, nil
	}

	etiquetas := map[string]string{
		LabelTenant:                 t.Nombre,
		plantilla.EtiquetaReclamado: strconv.FormatInt(time.Now().Unix(), 10),
	}
	in, h, err := hosts.Intentar(ctx, s.reg, sirve, func(ctx context.Context, h *hosts.Host) (plantilla.Instancia, error) {
		g, err := h.Cliente.GraphUp(ctx, plantilla.PeticionGrafo(*defs[h.Nombre], plantilla.NombreInstancia(), etiquetas))
		if err != nil {
			return plantilla.Instancia{}, err
		}
		in, err := s.instanciaDe(ctx, h, t, g.ID)
		if err != nil {
			// No se entrega algo que no se pudo comprobar entero, ni se deja.
			s.soltarGrafo(ctx, h, g.ID, err.Error())
		}
		return in, err
	})
	return h, in, err
}

// instanciaDe relee el grafo gid del host y exige que sea entero del tenant.
func (s *Servidor) instanciaDe(ctx context.Context, h *hosts.Host, t *Tenant, gid string) (plantilla.Instancia, error) {
	ins, err := instancias(ctx, h)
	if err != nil {
		return plantilla.Instancia{}, err
	}
	for _, in := range ins {
		if in.Grafo.ID == gid {
			if in.De(t.Nombre) && !in.Rota() {
				return in, nil
			}
			break
		}
	}
	return plantilla.Instancia{}, fmt.Errorf("graph %s is not whole or not yours after creating it", gid)
}

// reclamarGrafo busca, en los hosts que sirven y por orden de hueco, una
// instancia libre de la plantilla y la hace del tenant.
//
// Etiquetar y comprobar va bajo s.mu, como reclamarPrecalentada: son las
// etiquetas las que la reservan (una instancia con tenant ya no es Libre), así
// que dos reclamaciones de este frontal no se llevan la misma. Despertarla y
// releerla va FUERA del candado: un thaw lento no para las demás reclamaciones
// ni la creación de sandboxes.
//
// Contra OTRO frontal sobre el mismo daemon no hay candado: los dos pueden
// etiquetar a la vez y SetLabels es un merge por máquina, así que el grafo
// puede acabar entero de uno, entero del otro o MEZCLADO. Por eso se relee
// después de etiquetar: si es entero del tenant, es suyo; si es entero de
// otro, se deja; si está mezclado no es de nadie, y se borra (lo hacen los
// dos, y el segundo recibe un 404 que no importa). Un grafo que no llega a
// despertar también se borra: nunca vuelve al fondo algo a medias.
func (s *Servidor) reclamarGrafo(ctx context.Context, t *Tenant, tpl string, sirve func(*hosts.Host) bool) (*hosts.Host, plantilla.Instancia, bool) {
	vistos := map[string]bool{}
	for {
		h, gid, ok := s.etiquetarGrafo(ctx, t, tpl, sirve, vistos)
		if !ok {
			return nil, plantilla.Instancia{}, false
		}
		if _, err := h.Cliente.GraphThaw(ctx, gid); err != nil {
			s.soltarGrafo(ctx, h, gid, "thaw failed: "+err.Error())
			continue
		}
		ahora, err := s.releerInstancia(ctx, h, gid)
		if err != nil || ahora == nil || !ahora.De(t.Nombre) || ahora.Rota() {
			continue
		}
		return h, *ahora, true
	}
}

// etiquetarGrafo es la parte de reclamarGrafo que va bajo s.mu: pone las
// etiquetas del tenant a todos los nodos de la primera instancia libre y
// comprueba que ha quedado entera suya. vistos guarda las ya intentadas (por
// host e id) para no volver a ellas en la misma reclamación.
func (s *Servidor) etiquetarGrafo(ctx context.Context, t *Tenant, tpl string, sirve func(*hosts.Host) bool, vistos map[string]bool) (*hosts.Host, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	etiquetas := map[string]string{
		LabelTenant:                 t.Nombre,
		plantilla.EtiquetaReclamado: strconv.FormatInt(time.Now().Unix(), 10),
	}
	for _, h := range s.reg.Candidatos(ctx, sirve) {
		ins, err := instancias(ctx, h)
		if err != nil {
			continue
		}
		for _, in := range ins {
			if in.Plantilla != tpl || !in.Libre() {
				continue
			}
			gid := in.Grafo.ID
			if vistos[h.Nombre+"/"+gid] {
				continue
			}
			vistos[h.Nombre+"/"+gid] = true
			fallo := false
			for _, nombre := range in.Grafo.SortedNodeNames() {
				if err := h.Cliente.SetLabels(ctx, in.Maquinas[nombre].ID, etiquetas); err != nil {
					s.log.Printf("frontal: claiming graph %s/%s: node %s: %v", h.Nombre, gid, nombre, err)
					fallo = true
					break
				}
			}
			ahora, err := s.releerInstancia(ctx, h, gid)
			switch {
			case err != nil:
				continue
			case ahora == nil:
				continue // la borró otro (o `template rm`) entre tanto
			case !fallo && ahora.De(t.Nombre) && !ahora.Rota():
				return h, gid, true
			case !ahora.Mezclada && ahora.Tenant != "" && ahora.Tenant != t.Nombre:
				continue // la ganó otro frontal
			case ahora.Libre():
				continue // no llegó a etiquetarse nada: sigue en el fondo
			default:
				s.soltarGrafo(ctx, h, gid, "claim left it mixed or half-labeled")
				continue
			}
		}
	}
	return nil, "", false
}

// releerInstancia vuelve a leer una instancia por id; (nil, nil) si ya no está.
func (s *Servidor) releerInstancia(ctx context.Context, h *hosts.Host, gid string) (*plantilla.Instancia, error) {
	ins, err := instancias(ctx, h)
	if err != nil {
		return nil, err
	}
	for i := range ins {
		if ins[i].Grafo.ID == gid {
			return &ins[i], nil
		}
	}
	return nil, nil
}

// soltarGrafo borra un grafo que no puede quedarse: a medias, mezclado o que
// no despertó.
func (s *Servidor) soltarGrafo(ctx context.Context, h *hosts.Host, gid, porque string) {
	if err := h.Cliente.GraphRemove(context.WithoutCancel(ctx), gid); err != nil && !api.IsNotFound(err) {
		s.log.Printf("frontal: removing graph %s/%s (%s): %v", h.Nombre, gid, porque, err)
		return
	}
	s.log.Printf("frontal: removed graph %s/%s: %s", h.Nombre, gid, porque)
}

// ---- limpieza

// limpiarGrafos es la parte de Limpiar que toca grafos:
//   - uno reclamado que lleva más de Abandono sin usarse (ni reclamado ni con
//     ninguna máquina tocada desde entonces) se borra entero;
//   - uno roto (a medias o mezclado) se borra si lo estaba ya en la vuelta
//     anterior y tiene más de margenGrafoRoto: así no se toca un `graph up` ni
//     una reclamación en curso, que son rotos de paso.
//
// Los libres no se tocan: son del fondo.
func (s *Servidor) limpiarGrafos(ctx context.Context) {
	porHost, errs := s.instanciasEnCadaHost(ctx)
	for _, err := range errs {
		s.log.Printf("frontal: sweep: %v", err)
	}
	s.rotosMu.Lock()
	previos := s.rotos
	s.rotosMu.Unlock()
	ahora := map[string]bool{}
	for h, ins := range porHost {
		for _, in := range ins {
			clave := h.Nombre + "/" + in.Grafo.ID
			if in.Rota() {
				if time.Since(in.Grafo.CreatedAt) < margenGrafoRoto {
					continue
				}
				if !previos[clave] {
					ahora[clave] = true
					continue
				}
				s.soltarGrafo(ctx, h, in.Grafo.ID, "broken in two sweeps in a row")
				continue
			}
			if in.Tenant == "" {
				continue
			}
			desde := time.Unix(in.ReclamadaEn(), 0)
			for _, mc := range in.Maquinas {
				if r := relojTTL(mc); r.After(desde) {
					desde = r
				}
			}
			if time.Since(desde) < s.abandono {
				continue
			}
			s.soltarGrafo(ctx, h, in.Grafo.ID, fmt.Sprintf("tenant %s: unused for %s", in.Tenant, time.Since(desde).Round(time.Minute)))
		}
	}
	s.rotosMu.Lock()
	s.rotos = ahora
	s.rotosMu.Unlock()
}
