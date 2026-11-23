package frontal

// Ciclo de vida de los sandboxes: crear, listar, ver, renovar, borrar y la
// limpieza de los abandonados.
//
// PROPIEDAD. Cada sandbox lleva la etiqueta tenant=<nombre> del token que lo
// creó, y toda ruta /v1/sandboxes/{id} pasa por buscar(), que solo devuelve la
// máquina si esa etiqueta coincide con el tenant que llama. Si no coincide —o
// no existe— la respuesta es un 404 idéntico: confirmar que un id existe pero
// es de otro (un 403) regalaría información sobre lo ajeno.
//
// El daemon resuelve referencias por id, por NOMBRE y por PREFIJO de 4 o más
// caracteres. Por eso el frontal nunca le pasa lo que escribió el cliente:
// busca la máquina en la lista del host, comprueba el id EXACTO y la etiqueta,
// y solo entonces habla con el daemon usando el id completo.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/ext/sandbox/internal/pool"
	"github.com/juan52878911/kindling/pkg/api"
)

// maxCuerpo acota el JSON de creación y renovación. Un sandbox se describe en
// unos cientos de bytes; 64 KiB deja sitio a etiquetas y dominios sin que
// nadie pueda hacernos leer sin fin.
const maxCuerpo = 64 << 10

// prefijoReservado es el espacio de etiquetas del sistema (api.LabelOwner,
// api.LabelDBPrefix y las que vengan): ningún cliente puede fijarlas.
const prefijoReservado = "kling."

// maxEtiquetas es cuántas etiquetas propias admite una petición. Van al
// meta.json de la máquina en el daemon.
const maxEtiquetas = 32

// Sandbox es lo que ve el cliente. El id es "host/máquina": lleva el host
// porque los ids del daemon solo son únicos dentro de él, y la barra hace que
// nadie pueda confundirlo con un id del daemon ni pasárselo a `kling` tal cual.
type Sandbox struct {
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	Template string    `json:"template,omitempty"`
	Image    string    `json:"image,omitempty"`
	State    api.State `json:"state"`

	VCPUs        int      `json:"vcpus"`
	MemMiB       int      `json:"mem_mib"`
	Egress       string   `json:"egress,omitempty"`
	AllowDomains []string `json:"allow_domains,omitempty"`
	OnTTL        string   `json:"on_ttl"`
	TTLSeconds   int      `json:"ttl_seconds"`

	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt es el reloj del TTL del daemon: con on_ttl=freeze cada exec lo
	// pone a ahora, así que es "la última vez que se usó"; con on_ttl=remove
	// solo lo mueve renew. ExpiresAt es ese reloj más el TTL.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`

	// Labels son las etiquetas del cliente, sin las reservadas.
	Labels map[string]string `json:"labels,omitempty"`
}

// CrearPeticion es el cuerpo de POST /v1/sandboxes.
type CrearPeticion struct {
	Template     string            `json:"template,omitempty"`
	Image        string            `json:"image,omitempty"`
	TTLSeconds   int               `json:"ttl_seconds,omitempty"`
	OnTTL        string            `json:"on_ttl,omitempty"`
	Egress       string            `json:"egress,omitempty"`
	AllowDomains []string          `json:"allow_domains,omitempty"`
	MemMiB       int               `json:"mem_mib,omitempty"`
	VCPUs        int               `json:"vcpus,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`

	// Count crea un lote de Count sandboxes iguales (ver lote.go). Ausente es
	// uno solo y la respuesta es el sandbox; presente, aunque sea 1, la
	// respuesta es LoteRespuesta.
	Count *int `json:"count,omitempty"`
	// Atomic hace el lote todo o nada: si uno falla, se borran los ya creados.
	Atomic bool `json:"atomic,omitempty"`
	// Volumes son volúmenes del tenant a montar (volumenes.go).
	Volumes []VolumenPeticion `json:"volumes,omitempty"`

	// volumenes es lo que resolverVolumenes decidió; no viaja en el JSON.
	volumenes *planVolumenes
	// desplazo es el índice del sandbox en su lote: reparte entre hosts.
	desplazo int
}

// RenovarPeticion es el cuerpo de POST /v1/sandboxes/{id}/renew.
type RenovarPeticion struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// idDe compone el id visible; partirID lo deshace. Un id sin barra no es
// nuestro, y se trata como inexistente.
func idDe(h *hosts.Host, mc *api.Machine) string { return h.Nombre + "/" + mc.ID }

func partirID(id string) (host, ref string, ok bool) {
	i := strings.IndexByte(id, '/')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// vista convierte la máquina del daemon en lo que se enseña.
func vista(h *hosts.Host, mc *api.Machine) Sandbox {
	sb := Sandbox{
		ID: idDe(h, mc), Host: h.Nombre,
		Template: mc.Labels[LabelTemplate], Image: mc.Image, State: mc.State,
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, Egress: mc.Egress, AllowDomains: mc.AllowDomains,
		OnTTL: mc.OnTTL, TTLSeconds: mc.TTLSeconds, CreatedAt: mc.CreatedAt,
	}
	if sb.OnTTL == "" {
		sb.OnTTL = api.OnTTLFreeze
	}
	if desde := relojTTL(mc); !desde.IsZero() {
		d := desde
		sb.LastUsedAt = &d
		if mc.TTLSeconds > 0 {
			e := desde.Add(time.Duration(mc.TTLSeconds) * time.Second)
			sb.ExpiresAt = &e
		}
	}
	for k, v := range mc.Labels {
		if k == api.LabelKind || k == LabelTenant || k == LabelTemplate || strings.HasPrefix(k, prefijoReservado) {
			continue
		}
		if sb.Labels == nil {
			sb.Labels = map[string]string{}
		}
		sb.Labels[k] = v
	}
	return sb
}

// relojTTL es desde cuándo cuenta el TTL, con los mismos respaldos que usa el
// daemon para máquinas anteriores a TTLAt.
func relojTTL(mc *api.Machine) time.Time {
	switch {
	case mc.TTLAt != nil:
		return *mc.TTLAt
	case mc.StartedAt != nil:
		return *mc.StartedAt
	}
	return mc.CreatedAt
}

// esSandboxDe dice si la máquina es un sandbox de este tenant.
func esSandboxDe(mc *api.Machine, t *Tenant) bool {
	return mc.Labels[api.LabelKind] == api.KindSandbox && mc.Labels[LabelTenant] == t.Nombre &&
		plantilla.DelFrontal(mc.Labels)
}

// ---- consultas a los hosts

// enCadaHost ejecuta f contra todos los hosts a la vez y devuelve lo que cada
// uno contestó, en el orden estable del registro. Los que fallan quedan a nil
// y se apuntan en errs; el que llama decide si un host caído es fatal.
func (s *Servidor) enCadaHost(ctx context.Context, f func(context.Context, *hosts.Host) ([]*api.Machine, error)) (map[*hosts.Host][]*api.Machine, []error) {
	todos := s.reg.Todos()
	out := make([]([]*api.Machine), len(todos))
	errs := make([]error, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out[i], errs[i] = f(ctx, h)
			if errs[i] != nil {
				errs[i] = deHost(h, errs[i])
			}
		}(i, h)
	}
	wg.Wait()
	res := make(map[*hosts.Host][]*api.Machine, len(todos))
	var fallos []error
	for i, h := range todos {
		if errs[i] != nil {
			fallos = append(fallos, errs[i])
			continue
		}
		res[h] = out[i]
	}
	return res, fallos
}

// sandboxesDe lista los sandboxes del tenant en todos los hosts.
func (s *Servidor) sandboxesDe(ctx context.Context, t *Tenant) ([]Sandbox, []error) {
	porHost, errs := s.enCadaHost(ctx, func(ctx context.Context, h *hosts.Host) ([]*api.Machine, error) {
		return h.Cliente.Sandboxes(ctx)
	})
	var out []Sandbox
	for h, ms := range porHost {
		for _, mc := range ms {
			if esSandboxDe(mc, t) {
				out = append(out, vista(h, mc))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}

// errNoSandbox es la negativa única para "no existe", "es de otro", "el host
// no existe" y "el id no tiene nuestra forma": distinguirlas es lo que no se
// quiere.
type errNoSandbox struct{ id string }

func (e *errNoSandbox) Error() string { return fmt.Sprintf("no sandbox %q", e.id) }

func noSandbox(id string) error { return &errNoSandbox{id} }

// buscar resuelve un id a su host y su máquina, SOLO si es del tenant.
func (s *Servidor) buscar(ctx context.Context, t *Tenant, id string) (*hosts.Host, *api.Machine, error) {
	nombre, ref, ok := partirID(id)
	if !ok {
		return nil, nil, noSandbox(id)
	}
	h, ok := s.reg.Host(nombre)
	if !ok {
		return nil, nil, noSandbox(id)
	}
	ms, err := h.Cliente.Sandboxes(ctx)
	if err != nil {
		return nil, nil, deHost(h, err)
	}
	for _, mc := range ms {
		if mc.ID == ref && esSandboxDe(mc, t) {
			return h, mc, nil
		}
	}
	return nil, nil, noSandbox(id)
}

// ---- rutas

func (s *Servidor) handleListar(w http.ResponseWriter, r *http.Request) {
	t := tenantDe(r)
	out, errs := s.sandboxesDe(r.Context(), t)
	if out == nil {
		out = []Sandbox{}
	}
	// Un host caído no esconde los demás: se devuelve lo que hay y se avisa.
	w.Header().Set("Content-Type", "application/json")
	if len(errs) > 0 {
		w.Header().Set("X-Kindling-Partial", strings.Join(mensajes(errs), "; "))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSandbox despacha /v1/sandboxes/{host}/{ref}[/acción].
func (s *Servidor) handleSandbox(w http.ResponseWriter, r *http.Request) {
	resto := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
	if resto == "" || r.URL.Path == "/v1/sandboxes" {
		// La colección solo admite GET y POST, que el mux ya despachó.
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
		return
	}
	partes := strings.Split(resto, "/")
	if len(partes) < 2 || partes[0] == "" || partes[1] == "" {
		fail(w, http.StatusNotFound, noSandbox(resto))
		return
	}
	id := partes[0] + "/" + partes[1]
	accion := ""
	if len(partes) >= 3 {
		accion = partes[2]
	}
	if len(partes) > 3 && accion != "processes" && accion != "sessions" && accion != "code" {
		noRuta(w, r)
		return
	}
	switch {
	case accion == "" && r.Method == http.MethodGet:
		s.handleVer(w, r, id)
	case accion == "" && r.Method == http.MethodDelete:
		s.handleBorrar(w, r, id)
	case accion == "renew" && r.Method == http.MethodPost:
		s.handleRenovar(w, r, id)
	case accion == "pause" && r.Method == http.MethodPost:
		s.handlePausar(w, r, id)
	case accion == "resume" && r.Method == http.MethodPost:
		s.handleReanudar(w, r, id)
	case accion == "egress" && r.Method == http.MethodPatch:
		s.handleEgress(w, r, id)
	case accion == "exec" && r.Method == http.MethodPost:
		s.handleExec(w, r, id)
	case accion == "shell" && r.Method == http.MethodPost:
		s.handleShell(w, r, id)
	case accion == "endpoints" && r.Method == http.MethodPost:
		s.handleEndpoint(w, r, id)
	case accion == "files" && (r.Method == http.MethodGet || r.Method == http.MethodPut || r.Method == http.MethodDelete):
		s.handleFiles(w, r, id)
	case accion == "processes":
		s.despachaProcesos(w, r, id, partes[3:])
	case accion == "sessions":
		s.despachaSesiones(w, r, id, partes[3:])
	case accion == "code":
		s.despachaCodigo(w, r, id, partes[3:])
	case accion == "" || accion == "renew" || accion == "pause" || accion == "resume" || accion == "egress" || accion == "exec" || accion == "shell" || accion == "files" || accion == "endpoints":
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	default:
		noRuta(w, r)
	}
}

func (s *Servidor) handleVer(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	writeJSON(w, http.StatusOK, vista(h, mc))
}

func (s *Servidor) handleBorrar(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	if err := h.Cliente.RemoveSandbox(r.Context(), mc.ID); err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Servidor) handleRenovar(w http.ResponseWriter, r *http.Request, id string) {
	var pet RenovarPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ttl := pet.TTLSeconds
	if ttl == 0 {
		ttl = s.ttl
	}
	if ttl < 1 || ttl > api.SandboxMaxTTL {
		fail(w, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL))
		return
	}
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	out, err := h.Cliente.RenewSandbox(r.Context(), mc.ID, ttl)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, vista(h, out))
}

// codigoBuscar: un host que no contesta es un 502, no un 404, porque el
// sandbox puede existir y el cliente no debe borrarlo de su lista por eso.
func codigoBuscar(err error) int {
	var ns *errNoSandbox
	if errors.As(err, &ns) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// leerJSON decodifica el cuerpo con tope. Un cuerpo vacío vale (es "todo por
// defecto"); uno que no sea JSON o pase del tope, no.
func leerJSON(r *http.Request, v any) error {
	if r.ContentLength > maxCuerpo {
		return fmt.Errorf("body is %d bytes; the limit is %d", r.ContentLength, maxCuerpo)
	}
	b, err := api.LeerCuerpo(r.Body, maxCuerpo)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if dec.More() {
		return errors.New("invalid body: trailing data")
	}
	return nil
}

// ---- creación

func (s *Servidor) handleCrear(w http.ResponseWriter, r *http.Request) {
	entrada := time.Now()
	t := tenantDe(r)
	var pet CrearPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.normalizar(&pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	n := 1
	if pet.Count != nil {
		n = *pet.Count
		if n < 1 || n > t.MaxLote {
			fail(w, http.StatusBadRequest, fmt.Errorf("count must be between 1 and %d", t.MaxLote))
			return
		}
	}
	// Con plantilla, lo que la petición no dice (mem_mib o vcpus a 0) lo pone
	// el snapshot: se averigua aquí para acotar el tamaño EFECTIVO, que es el
	// que ocupa RAM en el host, y no solo el pedido.
	var snaps map[string]*api.Snapshot
	var durSnap time.Duration
	if pet.Template != "" {
		tramo := time.Now()
		snaps = s.snapshotsDe(r.Context(), plantilla.SnapshotDe(pet.Template))
		durSnap = time.Since(tramo)
		s.metricas.observar(tramoSnapshot, durSnap)
		if len(snaps) == 0 {
			err := &errSinPlantilla{pet.Template}
			s.metricas.registrar(resultadoDe(err), 0)
			fail(w, codigoCrear(err), err)
			return
		}
	}
	efectiva := tamanoEfectivo(pet, snaps)
	if err := dentroDeLimites(t, efectiva); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	plan, err := s.resolverVolumenes(r.Context(), t, pet.Volumes)
	if err != nil {
		fail(w, codigoVolumen(err), err)
		return
	}
	pet.volumenes = plan

	// Cuota. Se cuenta lo vivo en los daemons más lo que este proceso tiene en
	// camino, y se reserva el hueco ANTES de crear para que dos peticiones a la
	// vez no lo compartan. Un lote reserva los n de golpe: o caben todos o no
	// se crea ninguno.
	if err := s.reservarN(r.Context(), t, pet.Template, memCuota(efectiva.MemMiB), n); err != nil {
		codigo := codigoReservar(err)
		if codigo == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", retryAfterCuota)
			s.metricas.registrar(resultadoCuota, 0)
		} else {
			s.metricas.registrar(resultadoError, 0)
		}
		fail(w, codigo, err)
		return
	}
	defer s.liberarN(t, pet.Template, memCuota(efectiva.MemMiB), n)

	// Los clones se hacen con la cuota ya reservada: un lote rechazado no deja
	// volúmenes detrás.
	hVol, clonados, err := s.clonar(r.Context(), plan)
	if err != nil {
		s.metricas.registrar(resultadoDe(err), 0)
		fail(w, codigoVolumen(err), err)
		return
	}

	// prepasos: todo lo anterior a hablar con el daemon para crear, menos el
	// snapshot, que tiene su tramo.
	s.metricas.observar(tramoPrepasos, time.Since(entrada)-durSnap)

	if pet.Count != nil {
		s.respuestaLote(w, r, t, pet, snaps, n, hVol, clonados)
		s.metricas.observar(tramoTotal, time.Since(entrada))
		return
	}

	inicio := time.Now()
	defer func() { s.metricas.observar(tramoTotal, time.Since(entrada)) }()
	h, mc, err := s.crear(r.Context(), t, pet, snaps)
	if err != nil {
		s.metricas.registrar(resultadoDe(err), time.Since(inicio))
		if hVol != nil {
			s.borrarVolumenes(hVol, clonados)
		}
		s.falloCrear(w, pet, err)
		return
	}
	s.metricas.registrar(resultadoOK, time.Since(inicio))
	sb := vista(h, mc)
	w.Header().Set("Location", "/v1/sandboxes/"+sb.ID)
	writeJSON(w, http.StatusCreated, sb)
}

// falloCrear contesta al fallo de crear.
func (s *Servidor) falloCrear(w http.ResponseWriter, pet CrearPeticion, err error) {
	// Un reinicio del host invalida sus dorados: la reconstrucción ya se
	// lanzó en segundo plano (dentro de crear, en cuanto se vio el fallo) y
	// lo único que falta es decírselo a quien pidió el sandbox, con un
	// Retry-After en vez del 502/503 genérico que daría un fallo cualquiera.
	if pet.Template != "" && api.EsFalloTSC(err) {
		w.Header().Set("Retry-After", retryAfterReconstruccion)
		// errPublico: el mensaje (con lo que contestó el daemon) sale tal
		// cual y fail() no busca un *api.StatusError más abajo que nos pise
		// el 503 con el código que trajera el daemon.
		fail(w, http.StatusServiceUnavailable, &errPublico{err: err, msg: fmt.Sprintf(
			"template %q can't be restored right now: a host restart invalidated its golden snapshot; "+
				"rebuilding it in the background, try again shortly (%v)", pet.Template, err)})
		return
	}
	fail(w, codigoCrear(err), err)
}

// normalizar aplica los valores por defecto y rechaza lo que no vale. Por
// defecto sin red y durmiendo al vencer: lo que corre dentro lo escribió un
// agente, y dormir en vez de destruir es lo que permite retomar el trabajo en
// milisegundos sin pagar RAM entre tanto.
func (s *Servidor) normalizar(p *CrearPeticion) error {
	if p.Template != "" && p.Image != "" {
		return errors.New("pass template or image, not both")
	}
	if p.Template != "" && !nombreValido.MatchString(p.Template) {
		return fmt.Errorf("template name %q is not valid", p.Template)
	}
	if p.Template == "" && p.Image == "" {
		if s.imagen == "" {
			return errors.New("missing template (or image)")
		}
		p.Image = s.imagen
	}
	if p.TTLSeconds == 0 {
		p.TTLSeconds = s.ttl
	}
	if p.TTLSeconds < 1 || p.TTLSeconds > api.SandboxMaxTTL {
		return fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL)
	}
	switch p.OnTTL {
	case "":
		p.OnTTL = api.OnTTLFreeze
	case api.OnTTLFreeze, api.OnTTLRemove:
	default:
		return fmt.Errorf("invalid on_ttl %q: use %q or %q", p.OnTTL, api.OnTTLFreeze, api.OnTTLRemove)
	}
	switch p.Egress {
	case "":
		p.Egress = "none"
	case "none", "internet", "allowlist":
	default:
		return fmt.Errorf("invalid egress %q: use none, internet or allowlist", p.Egress)
	}
	if p.Egress != "allowlist" && len(p.AllowDomains) > 0 {
		return errors.New("allow_domains only makes sense with egress \"allowlist\"")
	}
	if p.Egress == "allowlist" && len(p.AllowDomains) == 0 {
		return errors.New("egress \"allowlist\" needs allow_domains")
	}
	if p.MemMiB < 0 || p.VCPUs < 0 {
		return errors.New("mem_mib and vcpus can't be negative")
	}
	if p.Count != nil && *p.Count < 1 {
		return errors.New("count must be at least 1")
	}
	if p.Atomic && p.Count == nil {
		return errors.New("atomic only makes sense with count")
	}
	n := 1
	if p.Count != nil {
		n = *p.Count
	}
	if err := validarVolumenes(p.Volumes, n); err != nil {
		return err
	}
	if len(p.Labels) > maxEtiquetas {
		return fmt.Errorf("too many labels (%d); the limit is %d", len(p.Labels), maxEtiquetas)
	}
	for k := range p.Labels {
		// Las reservadas las pone el frontal: dejar que el cliente las mande
		// sería dejarle firmar como otro tenant.
		// Todo lo que empieza por kling. es del sistema: kling.db.* es de
		// ext/db (falsificar de quién es una base) y kling.owner saca la
		// máquina del frontal, que dejaría de contarla en la cuota, listarla,
		// borrarla y limpiarla: creación ilimitada e invisible.
		if k == api.LabelKind || k == LabelTenant || k == LabelTemplate || strings.HasPrefix(k, prefijoReservado) {
			return fmt.Errorf("label %q is reserved", k)
		}
		if !api.KeyPattern.MatchString(k) {
			return fmt.Errorf("label %q is not valid (lowercase letters, digits, '.', '_', '-')", k)
		}
	}
	return nil
}

// tamanoEfectivo es p con mem_mib y vcpus a lo que de verdad tendrá el
// sandbox: lo pedido o, si es más, el mayor de los snapshots de la plantilla en
// los hosts que la tienen (cualquiera puede acabar sirviéndola). Se cuenta
// el mayor de los dos, que es el lado seguro. Con imagen y 0 manda el
// valor por defecto del daemon, que fija el operador del host.
func tamanoEfectivo(p CrearPeticion, snaps map[string]*api.Snapshot) CrearPeticion {
	for _, sn := range snaps {
		p.MemMiB = max(p.MemMiB, sn.MemMiB)
		p.VCPUs = max(p.VCPUs, sn.VCPUs)
	}
	return p
}

// dentroDeLimites rechaza un sandbox más grande de lo que el tenant puede pedir.
// p es el tamaño efectivo (tamanoEfectivo); un 0 que queda es "lo de la imagen",
// que fija el operador del daemon, así que no se acota aquí.
func dentroDeLimites(t *Tenant, p CrearPeticion) error {
	if t.MaxMemMiB > 0 && p.MemMiB > t.MaxMemMiB {
		return fmt.Errorf("mem_mib %d is over the limit of tenant %s (%d)", p.MemMiB, t.Nombre, t.MaxMemMiB)
	}
	if t.MaxVCPUs > 0 && p.VCPUs > t.MaxVCPUs {
		return fmt.Errorf("vcpus %d is over the limit of tenant %s (%d)", p.VCPUs, t.Nombre, t.MaxVCPUs)
	}
	return nil
}

// clave de contabilidad por plantilla.
func clavePlantilla(t *Tenant, plantilla string) string { return t.Nombre + "\x00" + plantilla }

// claveMemoria es la clave de enVuelo con los MiB que el tenant tiene en camino.
func claveMemoria(t *Tenant) string { return t.Nombre + "\x00mem" }

// memPorDefectoMiB es la RAM que el daemon da a una máquina pedida con
// mem_mib 0 (internal/machine: req.MemMiB <= 0 pasa a 256). La cuota cuenta
// eso y no el 0: con 0, cualquier número de sandboxes de imagen cabía en el
// tope total.
const memPorDefectoMiB = 256

// memCuota son los MiB que cuenta la cuota por una máquina de mem MiB.
func memCuota(mem int) int { return max(mem, memPorDefectoMiB) }

// ocupaRAM dice si una máquina en ese estado puede ocupar RAM: todo salvo lo
// parado o roto. Una dormida (frozen) cuenta, porque un exec la despierta sin
// pedir permiso a nadie.
func ocupaRAM(estado api.State) bool {
	return estado != api.StateStopped && estado != api.StateFailed
}

// memoriaViva es lo que unos sandboxes y grafos pueden ocupar en RAM.
func memoriaViva(sbs []Sandbox, grafos []Grafo) int {
	total := 0
	for _, sb := range sbs {
		if ocupaRAM(sb.State) {
			total += memCuota(sb.MemMiB)
		}
	}
	for _, g := range grafos {
		total += g.memMiB
	}
	return total
}

// reservar comprueba la cuota y apunta la creación en vuelo. mem son los MiB
// efectivos del sandbox que se va a crear (0 si no se saben).
func (s *Servidor) reservar(ctx context.Context, t *Tenant, plantilla string, mem int) error {
	return s.reservarN(ctx, t, plantilla, mem, 1)
}

// reservarN es reservar para un lote de n sandboxes de mem MiB cada uno: se
// cuenta el lote ENTERO antes de empezar, y o caben todos o no se reserva
// nada.
func (s *Servidor) reservarN(ctx context.Context, t *Tenant, plantilla string, mem, n int) error {
	if t.MaxSandboxes == 0 && (t.MaxPorPlantilla == 0 || plantilla == "") && t.MaxMemTotalMiB == 0 {
		s.mu.Lock()
		s.enVuelo[t.Nombre] += n
		s.enVuelo[clavePlantilla(t, plantilla)] += n
		if mem > 0 && t.MaxMemTotalMiB == 0 {
			s.enVuelo[claveMemoria(t)] += mem * n
		}
		s.mu.Unlock()
		return nil
	}
	// Contar, comprobar y apuntar van bajo el cerrojo del tenant, que también
	// toma liberar. Sin él, una creación que acababa (y se desapuntaba de
	// enVuelo) mientras otra contaba no salía en ninguna de las dos cuentas: la
	// lista se había leído antes de que existiera, y el contador ya no la
	// tenía. N peticiones a la vez superaban MaxSandboxes en tanto como N.
	// Contar de más es posible (la recién creada en la lista y aún en
	// enVuelo), que es el lado seguro.
	ct := s.cerrojoDe(t.Nombre)
	ct.Lock()
	defer ct.Unlock()
	// Falla CERRADO si algún host no contesta. Contar solo los que sí
	// responden dejaba crear de más: con un host caído sus sandboxes no
	// contaban, y al volver el tenant tenía más de los que su cuota permite.
	//
	// La suma de memoria sola solo perdona al host que el registro da por
	// caído: lo que vive en él no ocupa la RAM de los que contestan, y negarse
	// dejaba sin crear a todos los tenants en cuanto un host se caía. Un host
	// sano cuya lista falló ahora (un plazo vencido) sí cuenta: tomarlo por
	// caído subestimaba lo usado.
	cuenta := t.MaxSandboxes > 0 || (t.MaxPorPlantilla > 0 && plantilla != "")
	vivos, errs := s.sandboxesDe(ctx, t)
	var grafos []Grafo
	if cuenta || t.MaxMemTotalMiB > 0 {
		var errsG []error
		grafos, errsG = s.grafosDe(ctx, t)
		errs = append(errs, errsG...)
	}
	if len(errs) > 0 && (cuenta || !s.todosCaidos(ctx, errs)) {
		return &errCuotaSinContar{errors.Join(errs...)}
	}
	if s.trasContar != nil {
		s.trasContar()
	}
	total, dePlantilla := len(vivos)+len(grafos), 0
	for _, sb := range vivos {
		if plantilla != "" && sb.Template == plantilla {
			dePlantilla++
		}
	}
	// Un grafo cuenta como un sandbox, y por su plantilla con la clave
	// claveGrafo (plantilla trae ya esa forma cuando se pide un grafo).
	for _, g := range grafos {
		if plantilla != "" && claveGrafo(g.Template) == plantilla {
			dePlantilla++
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.MaxSandboxes > 0 && total+s.enVuelo[t.Nombre]+n > t.MaxSandboxes {
		return fmt.Errorf("tenant %s already has %d sandboxes and asks for %d more; the limit is %d", t.Nombre, total, n, t.MaxSandboxes)
	}
	if plantilla != "" && t.MaxPorPlantilla > 0 && dePlantilla+s.enVuelo[clavePlantilla(t, plantilla)]+n > t.MaxPorPlantilla {
		return fmt.Errorf("tenant %s already has %d sandboxes of template %s; the limit is %d", t.Nombre, dePlantilla, plantilla, t.MaxPorPlantilla)
	}
	// La suma de memoria: lo vivo, lo que está en camino y lo que se pide.
	if t.MaxMemTotalMiB > 0 {
		usada := memoriaViva(vivos, grafos) + s.enVuelo[claveMemoria(t)]
		if usada+mem*n > t.MaxMemTotalMiB {
			return fmt.Errorf("tenant %s already uses %d MiB and asks for %d more; the limit is %d MiB in total",
				t.Nombre, usada, mem*n, t.MaxMemTotalMiB)
		}
	}
	s.enVuelo[t.Nombre] += n
	s.enVuelo[clavePlantilla(t, plantilla)] += n
	if mem > 0 {
		s.enVuelo[claveMemoria(t)] += mem * n
	}
	return nil
}

// todosCaidos dice si cada error de errs es de un host caído: uno que tampoco
// contesta ahora a su sonda de estado (sin caché: la de hace un momento podía
// darlo por vivo o por muerto). Un error sin host, o de uno que contesta, es
// que no se sabe lo que hay ahí.
func (s *Servidor) todosCaidos(ctx context.Context, errs []error) bool {
	vivo := map[string]bool{}
	for _, e := range s.reg.Estados(ctx) {
		vivo[e.Nombre] = e.Vivo
	}
	for _, err := range errs {
		hs := hostsDe(err)
		if len(hs) == 0 {
			return false
		}
		for _, nombre := range hs {
			if vivo[nombre] {
				return false
			}
		}
	}
	return true
}

// cerrojoDe es el cerrojo de cuota del tenant nombre.
func (s *Servidor) cerrojoDe(nombre string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cerrojos == nil {
		s.cerrojos = map[string]*sync.Mutex{}
	}
	c := s.cerrojos[nombre]
	if c == nil {
		c = &sync.Mutex{}
		s.cerrojos[nombre] = c
	}
	return c
}

// errCuotaSinContar es que reservar no pudo contar lo que el tenant ya tiene
// porque algún host no contestó. No es "cuota superada" (429): es que no se
// sabe, y se niega hasta saberlo (503).
type errCuotaSinContar struct{ err error }

func (e *errCuotaSinContar) Error() string {
	return "can't check the tenant's quota, a host did not answer: " + e.err.Error()
}
func (e *errCuotaSinContar) Unwrap() error { return e.err }

// publico es lo que ve el tenant: qué hosts no contestaron, sin sus endpoints.
func (e *errCuotaSinContar) publico() string {
	if hs := hostsDe(e.err); len(hs) > 0 {
		return "can't check the tenant's quota, host " + unir(hs) + " did not answer; try again later"
	}
	return "can't check the tenant's quota, a host did not answer; try again later"
}

// codigoReservar es el estado HTTP de un fallo de reservar.
func codigoReservar(err error) int {
	var sc *errCuotaSinContar
	if errors.As(err, &sc) {
		return http.StatusServiceUnavailable
	}
	return http.StatusTooManyRequests
}

func (s *Servidor) liberar(t *Tenant, plantilla string, mem int) {
	s.liberarN(t, plantilla, mem, 1)
}

// liberarN deshace reservarN.
func (s *Servidor) liberarN(t *Tenant, plantilla string, mem, n int) {
	ct := s.cerrojoDe(t.Nombre)
	ct.Lock()
	defer ct.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.bajarEnVueloLocked(t.Nombre)
		s.bajarEnVueloLocked(clavePlantilla(t, plantilla))
	}
	if k := claveMemoria(t); mem > 0 {
		if s.enVuelo[k] <= mem*n {
			delete(s.enVuelo, k)
		} else {
			s.enVuelo[k] -= mem * n
		}
	}
}

// bajarEnVueloLocked descuenta una creación en vuelo y borra la clave al
// llegar a cero: un contador a 0 cuenta lo mismo que uno ausente, y dejarlo
// hacía crecer el mapa con cada par tenant/plantilla que pasó por el frontal.
func (s *Servidor) bajarEnVueloLocked(k string) {
	if s.enVuelo[k] <= 1 {
		delete(s.enVuelo, k)
		return
	}
	s.enVuelo[k]--
}

// errSinPlantilla es que la plantilla no está construida en ningún host.
type errSinPlantilla struct{ nombre string }

func (e *errSinPlantilla) Error() string {
	return fmt.Sprintf("template %q is not built on any reachable host", e.nombre)
}

func codigoCrear(err error) int {
	var sp *errSinPlantilla
	if errors.As(err, &sp) {
		return http.StatusNotFound
	}
	return codigoDeHost(err)
}

// crear decide dónde y cómo nace el sandbox.
//
// Con plantilla, el sandbox nace del snapshot de la plantilla y solo en hosts
// que lo tengan (los snapshots no viajan entre hosts). Si la petición no pide
// nada distinto de lo que una precalentada ya es, se reclama una; si no, se
// restaura una nueva del snapshot. Todo pasa por hosts.Intentar para que "no
// cabe" en un host se reintente en el siguiente.
func (s *Servidor) crear(ctx context.Context, t *Tenant, p CrearPeticion, snaps map[string]*api.Snapshot) (*hosts.Host, *api.Machine, error) {
	etiquetas := map[string]string{LabelTenant: t.Nombre}
	for k, v := range p.Labels {
		etiquetas[k] = v
	}
	req := api.SandboxRequest{
		VCPUs: p.VCPUs, MemMiB: p.MemMiB, TTLSeconds: p.TTLSeconds, OnTTL: p.OnTTL,
		Egress: p.Egress, AllowDomains: p.AllowDomains, Labels: etiquetas,
	}
	if p.volumenes != nil {
		req.Volumes = p.volumenes.atts
	}

	var sirve func(*hosts.Host) bool
	if p.Template == "" {
		req.Image = p.Image
		tramoImg := time.Now()
		tienen := s.hostsCon(ctx, func(ctx context.Context, h *hosts.Host) (bool, error) {
			imgs, err := h.Cliente.Images(ctx)
			if err != nil {
				return false, err
			}
			for _, im := range imgs {
				if im.Name == p.Image {
					return true, nil
				}
			}
			return false, nil
		})
		s.metricas.observar(tramoImagenes, time.Since(tramoImg))
		if len(tienen) == 0 {
			return nil, nil, &api.StatusError{Code: http.StatusNotFound,
				Message: fmt.Sprintf("image %q is not on any reachable host", p.Image)}
		}
		sirve = func(h *hosts.Host) bool { return tienen[h.Nombre] }
	} else {
		req.From = plantilla.SnapshotDe(p.Template)
		etiquetas[LabelTemplate] = p.Template
		if len(snaps) == 0 {
			return nil, nil, &errSinPlantilla{p.Template}
		}
		sirve = func(h *hosts.Host) bool { return snaps[h.Nombre] != nil }

		// Sin pool en ningún snapshot de la plantilla no hay nada que
		// reclamar: se ahorra el listado de cada host.
		if puedeReclamar(p) && algunoPideAlPool(snaps) {
			tramo := time.Now()
			h, mc := s.reclamarPrecalentada(ctx, t, p, etiquetas, snaps)
			s.metricas.observar(tramoReclamo, time.Since(tramo))
			if mc != nil {
				return h, mc, nil
			}
		}
	}

	if p.volumenes != nil && p.volumenes.solo != nil {
		antes, solo := sirve, p.volumenes.solo
		sirve = func(h *hosts.Host) bool { return solo[h.Nombre] && antes(h) }
	}

	tramoCreate := time.Now()
	defer func() { s.metricas.observar(tramoDaemon, time.Since(tramoCreate)) }()
	mc, h, err := hosts.IntentarRotado(ctx, s.reg, sirve, p.desplazo, func(ctx context.Context, h *hosts.Host) (*api.Machine, error) {
		mc, err := h.Cliente.CreateSandbox(ctx, req)
		// El fallo de TSC es DE ESE HOST: su dorado quedó invalidado por un
		// reinicio y solo una reconstrucción con la receta que lleva colgada lo
		// arregla (ver reconstruir.go). Se lanza aquí, en cuanto se ve el fallo,
		// para que empiece a repararse aunque hosts.Intentar acabe sirviendo la
		// petición desde otro host sano: si no, la próxima que caiga en este
		// mismo host tropezaría con lo mismo.
		if err != nil && p.Template != "" && api.EsFalloTSC(err) {
			s.repararEnSegundoPlano(h, p.Template)
		}
		return mc, err
	})
	if err != nil {
		return nil, nil, err
	}
	return h, mc, nil
}

// algunoPideAlPool dice si algún snapshot de la plantilla pide precalentadas
// (pool > 0). Si ninguno, reclamarPrecalentada solo gastaría un listado por
// host para no encontrar nada.
func algunoPideAlPool(snaps map[string]*api.Snapshot) bool {
	for _, sn := range snaps {
		if _, quiere := pool.DeseoDe(sn); quiere > 0 {
			return true
		}
	}
	return false
}

// snapshotsDe devuelve, por host, el snapshot llamado nombre en los hosts que
// lo tienen. Pide solo ese snapshot (GET /snapshots/{name}); sin caché, porque
// la versión del snapshot decide qué precalentada vale y un rebuild la dejaría
// vieja. Un daemon sin esa ruta (404/405 que no es "no existe") cae a la lista.
func (s *Servidor) snapshotsDe(ctx context.Context, nombre string) map[string]*api.Snapshot {
	var mu sync.Mutex
	out := map[string]*api.Snapshot{}
	s.hostsCon(ctx, func(ctx context.Context, h *hosts.Host) (bool, error) {
		sn, err := h.Cliente.Snapshot(ctx, nombre)
		if err == nil {
			mu.Lock()
			out[h.Nombre] = sn
			mu.Unlock()
			return true, nil
		}
		if !api.IsUnsupported(err) {
			return false, err
		}
		// 404: o el host no tiene el snapshot, o es un daemon viejo sin la
		// ruta. La lista lo distingue.
		snaps, err := h.Cliente.Snapshots(ctx)
		if err != nil {
			return false, err
		}
		for _, sn := range snaps {
			if sn.Name == nombre {
				mu.Lock()
				out[h.Nombre] = sn
				mu.Unlock()
				return true, nil
			}
		}
		return false, nil
	})
	return out
}

// hostsCon devuelve los nombres de los hosts para los que f dice que sí.
func (s *Servidor) hostsCon(ctx context.Context, f func(context.Context, *hosts.Host) (bool, error)) map[string]bool {
	todos := s.reg.Todos()
	res := make([]bool, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			ok, err := f(ctx, h)
			if err != nil {
				s.log.Printf("frontal: %s: %v", h.Nombre, err)
				return
			}
			res[i] = ok
		}(i, h)
	}
	wg.Wait()
	out := map[string]bool{}
	for i, h := range todos {
		if res[i] {
			out[h.Nombre] = true
		}
	}
	return out
}

// puedeReclamar dice si la petición se conforma con lo que una precalentada
// ya es. Red, on_ttl, memoria y vCPUs se fijan al ARRANCAR y una máquina viva
// no los cambia; lo único que se puede ajustar después es el TTL (renew) y
// las etiquetas.
func puedeReclamar(p CrearPeticion) bool {
	return p.Egress == "none" && p.OnTTL == api.OnTTLFreeze && p.MemMiB == 0 && p.VCPUs == 0 && len(p.Volumes) == 0
}

// plazoDaemon acota cada llamada al daemon que hace una reclamación: sin él,
// un host colgado dejaba la petición esperando lo que tardara el cliente en
// rendirse.
const plazoDaemon = 10 * time.Second

// reclamo apunta una precalentada que este proceso está reclamando (fin cero)
// o reclamó (fin, cuándo acabó). Una lista del host tomada ANTES de ese fin
// puede enseñarla aún sin dueño, y no sirve para reclamarla.
type reclamo struct{ fin time.Time }

// vidaReclamo es cuánto se recuerda un reclamo acabado, y vidaLista cuánto se
// fía reclamarPrecalentada de una lista: con vidaLista < vidaReclamo, ninguna
// lista en uso es anterior a un reclamo ya olvidado.
const (
	vidaReclamo = 5 * time.Minute
	vidaLista   = time.Minute
)

// reclamarPrecalentada busca, en los hosts que sirven y por orden de hueco,
// una instancia de la plantilla sin dueño y la hace del tenant.
//
// La reclamación es etiquetar (tenant=...) y renovar (el TTL pedido, contando
// desde ahora). Entre listar y etiquetar otra petición de este mismo proceso
// podría elegir la misma máquina; por eso cada una se marca en s.reclamos
// bajo s.mu antes de tocarla. Las llamadas al daemon van FUERA del candado y
// con plazo propio: un host colgado no para las demás creaciones (todas pasan
// por s.mu para apuntarse en vuelo). Dos FRONTALES distintos sobre el mismo
// daemon sí podrían chocar: SetLabels es un merge y el último gana; por eso
// reclamarUna va en dos fases con un nonce y solo uno pone etiquetas y TTL.
//
// snaps es el snapshot de la plantilla en cada host que la tiene: solo vale una
// precalentada que salió de ESE snapshot (plantilla.LabelVersion), no de uno
// anterior con el mismo nombre que un rebuild ya reemplazó.
//
// Con un daemon que anuncia sandbox-claim (api.CapabilityClaim) el reclamo es
// UNA llamada atómica (reclamarAtomica): sin listar, sin nonce y sin pausa. El
// camino de dos fases queda solo para un daemon anterior.
func (s *Servidor) reclamarPrecalentada(ctx context.Context, t *Tenant, p CrearPeticion, etiquetas map[string]string, snaps map[string]*api.Snapshot) (*hosts.Host, *api.Machine) {
	sirve := func(h *hosts.Host) bool { return snaps[h.Nombre] != nil }
	for _, h := range s.reg.Candidatos(ctx, sirve) {
		version := plantilla.VersionDe(snaps[h.Nombre])
		if s.hostReclama(ctx, h) {
			out, dosFases := s.reclamarAtomica(ctx, h, t, p, etiquetas, version)
			if out != nil {
				return h, out
			}
			if !dosFases {
				continue
			}
		}
		listada := time.Now()
		lctx, cancel := context.WithTimeout(ctx, plazoDaemon)
		ms, err := h.Cliente.Sandboxes(lctx)
		cancel()
		if err != nil {
			continue
		}
		for _, mc := range ms {
			if !esPrecalentadaDe(mc, p.Template, version) {
				continue
			}
			if time.Since(listada) > vidaLista {
				break // la lista es vieja: el siguiente host trae la suya
			}
			clave := h.Nombre + "/" + mc.ID
			if !s.marcarReclamo(clave, listada) {
				continue
			}
			out, ok := s.reclamarUna(ctx, h, mc, t, p, etiquetas, version)
			s.acabarReclamo(clave)
			if ok {
				return h, out
			}
		}
	}
	return nil, nil
}

// capHost es lo que se sabe de si un host sirve el reclamo atómico.
type capHost struct {
	reclama bool
	hasta   time.Time // cero: para siempre
}

// vidaSinReclamo es cuánto se recuerda que un daemon NO anuncia sandbox-claim:
// poco, para que un daemon actualizado sin reiniciar el gateway la pase a usar.
const vidaSinReclamo = time.Minute

// hostReclama dice si el daemon del host anuncia sandbox-claim. Se consulta una
// vez (GET /info) y se cachea; si la consulta falla no se cachea y esa petición
// usa las dos fases, que funcionan con cualquier daemon.
func (s *Servidor) hostReclama(ctx context.Context, h *hosts.Host) bool {
	s.mCap.Lock()
	c, ok := s.capReclamo[h.Nombre]
	s.mCap.Unlock()
	if ok && (c.hasta.IsZero() || time.Now().Before(c.hasta)) {
		return c.reclama
	}
	ictx, cancel := context.WithTimeout(ctx, plazoDaemon)
	defer cancel()
	info, err := h.Cliente.Info(ictx)
	if err != nil {
		return false
	}
	c = capHost{reclama: info.Has(api.CapabilityClaim)}
	if !c.reclama {
		c.hasta = time.Now().Add(vidaSinReclamo)
	}
	s.mCap.Lock()
	s.capReclamo[h.Nombre] = c
	s.mCap.Unlock()
	return c.reclama
}

// pedirRelleno avisa al fondo de que en ese host falta de esa plantilla.
func (s *Servidor) pedirRelleno(h *hosts.Host, plantilla string) {
	if s.rellena != nil {
		s.rellena.Pedir(h.Nombre, plantilla)
	}
}

// reclamarAtomica reclama una precalentada con una sola llamada al daemon
// (POST /sandboxes/claim): él elige, etiqueta y renueva bajo su cerrojo, así
// que no hace falta listar ni nonce ni pausa. Devuelve nil si no hay copia o
// el reclamo falla; en ambos casos y en el de éxito avisa al fondo.
//
// Además de las etiquetas del tenant pone LabelReclamo: un frontal anterior
// que comparta el daemon la ve como reclamada en su relectura.
//
// El segundo valor pide seguir por las dos fases: el daemon contestó con un
// 404/405 sin código, es decir, ya no tiene la ruta (se bajó de versión): se
// invalida la capacidad cacheada y esta misma petición no se pierde.
func (s *Servidor) reclamarAtomica(ctx context.Context, h *hosts.Host, t *Tenant, p CrearPeticion, etiquetas map[string]string, version string) (*api.Machine, bool) {
	defer s.pedirRelleno(h, p.Template)
	labels := make(map[string]string, len(etiquetas)+1)
	for k, v := range etiquetas {
		labels[k] = v
	}
	// Nonce único: si el reclamo se corta tras etiquetar, es lo que permite
	// encontrar y borrar lo que quedó a medias.
	nonce, err := nuevoNonce()
	if err != nil {
		s.log.Printf("frontal: claiming from %s: %v", h.Nombre, err)
		return nil, false
	}
	labels[LabelReclamo] = nonce
	match := map[string]string{api.LabelKind: api.KindSandbox, LabelTemplate: p.Template}
	absent := []string{LabelTenant, LabelReclamo}
	if version != "" {
		match[plantilla.LabelVersion] = version
	} else {
		// Sin versión no se casa con cualquiera: solo vale una sin etiqueta.
		absent = append(absent, plantilla.LabelVersion)
	}
	ctx, cancel := context.WithTimeout(ctx, plazoDaemon)
	defer cancel()
	out, err := h.Cliente.ClaimSandbox(ctx, api.ClaimRequest{
		Match: match, Absent: absent,
		Labels: labels, TTLSeconds: p.TTLSeconds,
	})
	if err != nil {
		if api.IsNoClaimable(err) {
			return nil, false
		}
		var se *api.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusMethodNotAllowed) && se.ErrorCode == "" {
			s.log.Printf("frontal: %s no longer serves sandbox-claim (%v); using two phases", h.Nombre, err)
			s.invalidarReclamo(h)
			return nil, true
		}
		s.log.Printf("frontal: claiming from %s: %v", h.Nombre, err)
		// El daemon pudo etiquetar antes del corte o del plazo: lo que lleve
		// el nonce cuenta en la cuota del inquilino. Se borra sin la
		// cancelación de la petición.
		s.soltarPorNonce(ctx, h, nonce)
		return nil, false
	}
	if !esSandboxDe(out, t) {
		s.log.Printf("frontal: claim on %s/%s came back without the tenant label; removing it", h.Nombre, out.ID)
		s.soltarMediaReclamada(ctx, h, out.ID, "claimed for another tenant")
		return nil, false
	}
	if (out.Egress != "" && out.Egress != "none") || out.OnTTL == api.OnTTLRemove {
		// Contrato de esPrecalentadaDe, que el camino atómico no comprueba:
		// solo un admin fabrica una candidata así, pero no se entrega.
		s.soltarMediaReclamada(ctx, h, out.ID, "not a prewarmed one (egress or on_ttl)")
		return nil, false
	}
	return out, false
}

// invalidarReclamo olvida que el host sirve sandbox-claim (por un minuto).
func (s *Servidor) invalidarReclamo(h *hosts.Host) {
	s.mCap.Lock()
	s.capReclamo[h.Nombre] = capHost{hasta: time.Now().Add(vidaSinReclamo)}
	s.mCap.Unlock()
}

// soltarPorNonce busca en el host las máquinas que llevan el nonce de un
// reclamo que falló y las borra.
func (s *Servidor) soltarPorNonce(ctx context.Context, h *hosts.Host, nonce string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plazoDaemon)
	defer cancel()
	ms, err := h.Cliente.Sandboxes(rctx)
	if err != nil {
		s.log.Printf("frontal: looking for half-claimed machines on %s: %v", h.Nombre, err)
		return
	}
	for _, mc := range ms {
		if mc.Labels[LabelReclamo] == nonce {
			s.soltarMediaReclamada(rctx, h, mc.ID, "claim failed after labelling")
		}
	}
}

// marcarReclamo aparta la máquina para esta petición si nadie de este proceso
// la está reclamando ni la reclamó después de listada.
func (s *Servidor) marcarReclamo(clave string, listada time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reclamos == nil {
		s.reclamos = map[string]reclamo{}
	}
	for k, r := range s.reclamos {
		if !r.fin.IsZero() && time.Since(r.fin) > vidaReclamo {
			delete(s.reclamos, k)
		}
	}
	if r, ok := s.reclamos[clave]; ok && (r.fin.IsZero() || !listada.After(r.fin)) {
		return false
	}
	s.reclamos[clave] = reclamo{}
	return true
}

func (s *Servidor) acabarReclamo(clave string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reclamos[clave] = reclamo{fin: time.Now()}
}

// esperaReclamo es la pausa entre las dos fases de reclamarUna. Tiene que ser
// la misma en todos los frontales de un daemon: cada uno comprueba que su
// propia segunda fase tardó menos que la pausa de los demás.
const esperaReclamo = 50 * time.Millisecond

// reclamarUna reclama una precalentada ya apartada en este proceso. El daemon
// no tiene compare-and-swap de etiquetas (PUT labels es un merge y el último
// gana), así que contra OTROS frontales sobre el mismo daemon va en dos fases,
// al estilo del algoritmo de Fischer:
//
//  1. Se escribe solo LabelReclamo=<nonce> y se espera esperaReclamo.
//  2. Se relee: si el nonce ya no es el nuestro, o la máquina ya tiene dueño,
//     otro frontal ganó y nos vamos sin haber tocado ni sus etiquetas ni su
//     TTL. Si sigue siendo nuestro, se ponen las etiquetas del tenant y se
//     renueva.
//
// Quien llega tarde a la fase 1 relee DESPUÉS de su pausa, y para entonces la
// fase 2 del ganador ya ha caído (se comprueba que tardó menos que la pausa;
// si no, la máquina se destruye: no se sabe si otro también pasó). Así solo
// uno pone etiquetas y TTL, y nadie mezcla los suyos con los de otro.
func (s *Servidor) reclamarUna(ctx context.Context, h *hosts.Host, mc *api.Machine, t *Tenant, p CrearPeticion, etiquetas map[string]string, version string) (*api.Machine, bool) {
	espera := s.esperaReclamo
	if espera <= 0 {
		espera = esperaReclamo
	}
	ctx, cancel := context.WithTimeout(ctx, plazoDaemon+espera)
	defer cancel()
	nonce, err := nuevoNonce()
	if err != nil {
		s.log.Printf("frontal: claiming %s/%s: %v", h.Nombre, mc.ID, err)
		return nil, false
	}
	if err := h.Cliente.SetLabels(ctx, mc.ID, map[string]string{LabelReclamo: nonce}); err != nil {
		s.log.Printf("frontal: claiming %s/%s: %v", h.Nombre, mc.ID, err)
		return nil, false
	}
	select {
	case <-time.After(espera):
	case <-ctx.Done():
		return nil, false
	}
	leida := time.Now()
	cur, err := h.Cliente.Get(ctx, mc.ID)
	if err != nil {
		s.log.Printf("frontal: claiming %s/%s: %v", h.Nombre, mc.ID, err)
		return nil, false
	}
	if cur.Labels[LabelReclamo] != nonce || !esPrecalentadaDe(cur, p.Template, version) {
		// Otro frontal la está reclamando o ya es suya: no se toca.
		return nil, false
	}

	fase2 := make(map[string]string, len(etiquetas)+1)
	for k, v := range etiquetas {
		fase2[k] = v
	}
	fase2[LabelReclamo] = nonce
	if err := h.Cliente.SetLabels(ctx, mc.ID, fase2); err != nil {
		s.soltarMediaReclamada(ctx, h, mc.ID, fmt.Sprintf("labels: %v", err))
		return nil, false
	}
	if d := time.Since(leida); d >= esperaReclamo || d >= espera {
		// Demasiado lenta: otro frontal pudo releer antes de que cayera y
		// pasar también. No se entrega.
		s.soltarMediaReclamada(ctx, h, mc.ID, fmt.Sprintf("second phase took %s", d.Round(time.Millisecond)))
		return nil, false
	}
	out, err := h.Cliente.RenewSandbox(ctx, mc.ID, p.TTLSeconds)
	if err != nil {
		// Ya lleva la etiqueta del tenant pero no se le va a entregar:
		// dejarla así la sacaba del pool y le contaba al tenant en su
		// cuota (y en su lista) un sandbox que nunca pidió, hasta que la
		// limpieza por abandono se la llevara. Se destruye; el pool la
		// repone. No se le quitan las etiquetas: llevaría las del cliente
		// a la siguiente que la reclamara.
		s.soltarMediaReclamada(ctx, h, mc.ID, fmt.Sprintf("renew: %v", err))
		return nil, false
	}
	if !esSandboxDe(out, t) {
		// Un frontal anterior a las dos fases la etiquetó después (el
		// último gana): es suya.
		return nil, false
	}
	for k, v := range etiquetas {
		if out.Labels[k] != v {
			// Es nuestra pero alguien mezcló etiquetas: no se entrega.
			s.soltarMediaReclamada(ctx, h, mc.ID, "labels changed under the claim")
			return nil, false
		}
	}
	return out, true
}

// soltarMediaReclamada destruye una precalentada que se empezó a etiquetar y
// no se va a entregar. Sin la cancelación de la petición: si el fallo es que
// el cliente se fue, borrar con su contexto fallaba también y la máquina se
// quedaba a medio reclamar (como soltarGrafo).
func (s *Servidor) soltarMediaReclamada(ctx context.Context, h *hosts.Host, id, motivo string) {
	s.log.Printf("frontal: claiming %s/%s: %s; removing it", h.Nombre, id, motivo)
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), plazoDaemon)
	defer rcancel()
	if rerr := h.Cliente.RemoveSandbox(rctx, id); rerr != nil {
		s.log.Printf("frontal: removing half-claimed %s/%s: %v", h.Nombre, id, rerr)
	}
}

// nuevoNonce son 128 bits de crypto/rand en hexadecimal.
func nuevoNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// esPrecalentadaDe es el contrato con el pool del paquete plantilla: una
// máquina kind=sandbox, de esta plantilla, sin tenant, viva o dormida, sin red
// y que duerme al vencer. Y del frontal (sin kling.owner): las etiquetas de
// arriba las puede poner cualquier inquilino del daemon, que conservaría exec y
// ficheros de lo que se le diera a un cliente. Y de la versión del snapshot
// que el host tiene ahora (plantilla.LabelVersion): una de un dorado anterior
// llevaría el contenido viejo.
func esPrecalentadaDe(mc *api.Machine, template, version string) bool {
	if mc.Labels[api.LabelKind] != api.KindSandbox || mc.Labels[LabelTemplate] != template || mc.Labels[LabelTenant] != "" ||
		!plantilla.DelFrontal(mc.Labels) || mc.Labels[plantilla.LabelVersion] != version {
		return false
	}
	if mc.State != api.StateRunning && mc.State != api.StateWarm {
		return false
	}
	if mc.Egress != "" && mc.Egress != "none" {
		return false
	}
	return mc.OnTTL != api.OnTTLRemove
}

// ---- limpieza

// Limpiar hace una vuelta: borra los sandboxes con dueño que llevan más de
// Abandono sin usarse. Es el segundo plazo: el TTL del daemon los duerme, y
// dormidos no cuestan RAM pero sí disco y una entrada en el tope de máquinas;
// esto es lo que evita que se acumulen para siempre.
//
// Solo toca sandboxes con tenant: las precalentadas son del pool de
// plantilla, y las máquinas sin kind=sandbox ni se listan. Los grafos tienen
// su propia vuelta (limpiarGrafos).
func (s *Servidor) Limpiar(ctx context.Context) {
	defer s.limpiarGrafos(ctx)
	porHost, errs := s.enCadaHost(ctx, func(ctx context.Context, h *hosts.Host) ([]*api.Machine, error) {
		return h.Cliente.Sandboxes(ctx)
	})
	for _, err := range errs {
		s.log.Printf("frontal: sweep: %v", err)
	}
	for h, ms := range porHost {
		for _, mc := range ms {
			if mc.Labels[api.LabelKind] != api.KindSandbox || mc.Labels[LabelTenant] == "" ||
				!plantilla.DelFrontal(mc.Labels) {
				continue
			}
			desde := relojTTL(mc)
			if desde.IsZero() || time.Since(desde) < s.abandono {
				continue
			}
			if err := h.Cliente.RemoveSandbox(ctx, mc.ID); err != nil {
				s.log.Printf("frontal: sweep: removing %s/%s: %v", h.Nombre, mc.ID, err)
				continue
			}
			s.log.Printf("frontal: removed %s/%s (tenant %s): unused for %s", h.Nombre, mc.ID, mc.Labels[LabelTenant], time.Since(desde).Round(time.Minute))
		}
	}
}

// Vigilar repite Limpiar cada CadaLimpieza hasta que el contexto se cancele.
func (s *Servidor) Vigilar(ctx context.Context) {
	go func() {
		tick := time.NewTicker(s.cada)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.Limpiar(ctx)
			}
		}
	}()
}
