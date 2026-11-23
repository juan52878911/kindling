package frontal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// Volúmenes por sandbox.
//
// Un volumen del daemon es un ext4 con nombre, global al host. El gateway los
// hace del tenant de dos maneras a la vez: el nombre real lleva un prefijo
// derivado del tenant (dos tenants pueden llamar "datos" al suyo sin
// chocar, y uno no puede ni nombrar el del otro) y la etiqueta LabelTenant va
// en el propio volumen. Un clon NO lleva etiquetas (el núcleo no las copia ni
// CloneVolumeRequest las admite), así que pertenece al tenant el volumen con
// su prefijo y la etiqueta o ninguna; con la de otro tenant, no. El prefijo
// solo lo pone este gateway, que no deja crear con otro.
// Lo ajeno es indistinguible de lo que no existe: 404.
//
// Un volumen vive en UN host y un sandbox solo monta volúmenes del host donde
// corre, así que pedir volúmenes existentes fija el host (o los hosts) donde
// se puede crear el sandbox.

// reVolumen es el nombre que el cliente da a un volumen. Más corto que el del
// daemon (64) para dejar sitio al prefijo.
var reVolumen = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

const (
	// maxVolumenesPorSandbox acota cuántos discos extra monta un sandbox.
	maxVolumenesPorSandbox = 4
	// MaxVolumenMiBPorDefecto es el tamaño máximo de un volumen nuevo.
	MaxVolumenMiBPorDefecto = 16384
)

// VolumenPeticion es un elemento de "volumes" en POST /v1/sandboxes.
type VolumenPeticion struct {
	// Name es el volumen del tenant a montar. Tiene que existir, o crearse al
	// vuelo con CloneFrom.
	Name     string `json:"name"`
	Mount    string `json:"mount"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// CloneFrom, si Name no existe, lo crea como clon de este volumen del
	// tenant (en su mismo host). Con Name ya existente es un conflicto.
	CloneFrom string `json:"clone_from,omitempty"`
}

// Volumen es lo que ve el cliente.
type Volumen struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	SizeMiB   int64  `json:"size_mib"`
	UsedBytes int64  `json:"used_bytes"`
	InUse     bool   `json:"in_use"`
}

// CrearVolumenPeticion es el cuerpo de POST /v1/volumes.
type CrearVolumenPeticion struct {
	Name    string `json:"name"`
	SizeMiB int    `json:"size_mib,omitempty"`
	// CloneFrom crea el volumen como clon de otro del tenant, en su host.
	CloneFrom string `json:"clone_from,omitempty"`
	// Host fija dónde se crea. Sin él, el primero que lo acepte.
	Host string `json:"host,omitempty"`
}

// prefijoVolumen es el de los volúmenes nuevos: 8 bytes del hash del nombre.
func prefijoVolumen(t *Tenant) string {
	h := sha256.Sum256([]byte(t.Nombre))
	return "v" + hex.EncodeToString(h[:8]) + "-"
}

// prefijoCorto es el de antes (4 bytes): se sigue reconociendo al leer, no se
// escribe. 32 bits chocan con poco más de 65 000 tenants, y un clon no lleva
// etiqueta que deshaga el empate.
func prefijoCorto(t *Tenant) string {
	h := sha256.Sum256([]byte(t.Nombre))
	return "v" + hex.EncodeToString(h[:4]) + "-"
}

func nombreReal(t *Tenant, n string) string { return prefijoVolumen(t) + n }

func etiquetasVolumen(t *Tenant) map[string]string { return map[string]string{LabelTenant: t.Nombre} }

// esVolumenDe dice si v es del tenant (ver arriba). Con el prefijo largo basta
// él (y la etiqueta, si la hay). Con el corto, de antes, un volumen sin
// etiqueta (un clon) solo se atribuye si ningún otro tenant conocido comparte
// ese prefijo: si lo comparte no se sabe de quién es y no es de nadie.
func (s *Servidor) esVolumenDe(v *api.Volume, t *Tenant) bool {
	dueno, etiquetado := v.Labels[LabelTenant]
	if etiquetado && dueno != t.Nombre {
		return false
	}
	if strings.HasPrefix(v.Name, prefijoVolumen(t)) {
		return true
	}
	if !strings.HasPrefix(v.Name, prefijoCorto(t)) {
		return false
	}
	if etiquetado {
		return true
	}
	for _, o := range s.tenantsTodos() {
		if o.Nombre != t.Nombre && prefijoCorto(&o) == prefijoCorto(t) {
			return false
		}
	}
	return true
}

// buscaReal devuelve el nombre real con que existe el volumen nombre del
// tenant (con el prefijo largo o con el de antes) y en qué hosts; si no
// existe, el nombre que tendría uno nuevo.
func buscaReal(en map[string]map[string]bool, t *Tenant, nombre string) (string, map[string]bool, bool) {
	for _, real := range []string{nombreReal(t, nombre), prefijoCorto(t) + nombre} {
		if hs, ok := en[real]; ok {
			return real, hs, true
		}
	}
	return nombreReal(t, nombre), nil, false
}

func quitarPrefijo(t *Tenant, real string) string {
	if strings.HasPrefix(real, prefijoVolumen(t)) {
		return strings.TrimPrefix(real, prefijoVolumen(t))
	}
	return strings.TrimPrefix(real, prefijoCorto(t))
}

func vistaVolumen(t *Tenant, h *hosts.Host, v *api.Volume) Volumen {
	return Volumen{
		Name: quitarPrefijo(t, v.Name), Host: h.Nombre,
		SizeMiB: v.SizeBytes >> 20, UsedBytes: v.UsedBytes, InUse: len(v.UsedBy) > 0,
	}
}

type volEnHost struct {
	h *hosts.Host
	v *api.Volume
}

// volumenesDe lista los volúmenes del tenant en todos los hosts, en el orden
// del registro y luego por nombre.
func (s *Servidor) volumenesDe(ctx context.Context, t *Tenant) ([]volEnHost, []error) {
	todos := s.reg.Todos()
	out := make([][]*api.Volume, len(todos))
	errs := make([]error, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out[i], errs[i] = h.Cliente.Volumes(ctx)
			if errs[i] != nil {
				errs[i] = deHost(h, errs[i])
			}
		}()
	}
	wg.Wait()
	var res []volEnHost
	var fallos []error
	for i, h := range todos {
		if errs[i] != nil {
			fallos = append(fallos, errs[i])
			continue
		}
		vs := out[i]
		sort.Slice(vs, func(a, b int) bool { return vs[a].Name < vs[b].Name })
		for _, v := range vs {
			if s.esVolumenDe(v, t) {
				res = append(res, volEnHost{h, v})
			}
		}
	}
	return res, fallos
}

// validarVolumenes es la parte de normalizar que mira "volumes".
func validarVolumenes(vs []VolumenPeticion, count int) error {
	if len(vs) > maxVolumenesPorSandbox {
		return fmt.Errorf("too many volumes (%d); the limit is %d", len(vs), maxVolumenesPorSandbox)
	}
	nombres, montajes := map[string]bool{}, map[string]bool{}
	for i := range vs {
		v := &vs[i]
		if !reVolumen.MatchString(v.Name) {
			return fmt.Errorf("volume name %q is not valid (lowercase letters, digits, '_' and '-', up to 41)", v.Name)
		}
		if v.CloneFrom != "" {
			if !reVolumen.MatchString(v.CloneFrom) {
				return fmt.Errorf("clone_from %q is not a valid volume name", v.CloneFrom)
			}
			if v.CloneFrom == v.Name {
				return fmt.Errorf("volume %q can't be cloned from itself", v.Name)
			}
			if count > 1 {
				return errors.New("clone_from needs a single sandbox: every sandbox of a batch would need its own volume")
			}
		}
		if !path.IsAbs(v.Mount) || path.Clean(v.Mount) != v.Mount || v.Mount == "/" || strings.ContainsAny(v.Mount, "\x00\n") {
			return fmt.Errorf("volume %q: mount must be a clean absolute path other than \"/\"", v.Name)
		}
		if nombres[v.Name] || montajes[v.Mount] {
			return fmt.Errorf("volume %q: name or mount repeated", v.Name)
		}
		nombres[v.Name], montajes[v.Mount] = true, true
		if count > 1 && !v.ReadOnly {
			return fmt.Errorf("volume %q: a batch of %d sandboxes can only mount it read_only (an ext4 volume has one writer)", v.Name, count)
		}
	}
	return nil
}

// planVolumenes es lo que resolverVolumenes decide.
type planVolumenes struct {
	// atts son los montajes con el nombre real, listos para el daemon.
	atts []api.VolumeAttachment
	// solo son los hosts donde pueden nacer los sandboxes.
	solo map[string]bool
	// clones son los volúmenes que hay que crear antes, en solo[host].
	clones []clonPlan
	// propios son los volúmenes que el tenant tenía al resolver.
	propios int
}

type clonPlan struct{ origen, destino string }

type errVolumen struct {
	code int
	msg  string
}

func (e *errVolumen) Error() string { return e.msg }

func noVolumen(n string) error {
	return &errVolumen{http.StatusNotFound, fmt.Sprintf("no volume %q", n)}
}

// resolverVolumenes comprueba que cada volumen pedido existe y es del tenant
// (o se puede clonar de uno que lo es) y calcula en qué hosts caben todos.
func (s *Servidor) resolverVolumenes(ctx context.Context, t *Tenant, vs []VolumenPeticion) (*planVolumenes, error) {
	if len(vs) == 0 {
		return nil, nil
	}
	propios, errs := s.volumenesDe(ctx, t)
	en := map[string]map[string]bool{} // nombre real -> hosts
	for _, p := range propios {
		if en[p.v.Name] == nil {
			en[p.v.Name] = map[string]bool{}
		}
		en[p.v.Name][p.h.Nombre] = true
	}
	// Un host que no contestó podría tener el volumen: sin él no se puede
	// decir 404 con verdad.
	ausente := func(n string) error {
		if len(errs) > 0 {
			return fmt.Errorf("can't look up volume %q, a host did not answer: %w", n, errors.Join(errs...))
		}
		return noVolumen(n)
	}
	plan := &planVolumenes{propios: len(propios)}
	var comunes map[string]bool
	var fijado string // el volumen que fijó los hosts, para el mensaje
	cruzar := func(n string, hs map[string]bool) error {
		if comunes == nil {
			comunes, fijado = map[string]bool{}, n
			for h := range hs {
				comunes[h] = true
			}
		} else {
			for h := range comunes {
				if !hs[h] {
					delete(comunes, h)
				}
			}
		}
		if len(comunes) == 0 {
			return &errVolumen{http.StatusConflict, fmt.Sprintf(
				"volumes %q and %q live on different hosts: a sandbox can only mount volumes of the host it runs on", fijado, n)}
		}
		return nil
	}
	for _, v := range vs {
		real, hs, existe := buscaReal(en, t, v.Name)
		switch {
		case existe && v.CloneFrom != "":
			return nil, &errVolumen{http.StatusConflict, fmt.Sprintf("volume %q already exists: drop clone_from to mount it", v.Name)}
		case existe:
			if err := cruzar(v.Name, hs); err != nil {
				return nil, err
			}
		case v.CloneFrom != "":
			realOrig, orig, ok := buscaReal(en, t, v.CloneFrom)
			if !ok {
				return nil, ausente(v.CloneFrom)
			}
			if err := cruzar(v.CloneFrom, orig); err != nil {
				return nil, err
			}
			plan.clones = append(plan.clones, clonPlan{realOrig, real})
		default:
			return nil, ausente(v.Name)
		}
		plan.atts = append(plan.atts, api.VolumeAttachment{Name: real, Mount: v.Mount, ReadOnly: v.ReadOnly})
	}
	plan.solo = comunes
	// El tope de volúmenes cuenta también los clones de aquí (el cerrojo lo
	// toma handleCrear). Con un host sin contestar no se sabe cuántos hay: se
	// niega en vez de dejar pasar de más.
	if t.MaxVolumenes > 0 && len(plan.clones) > 0 {
		if len(errs) > 0 {
			return nil, &errVolumen{http.StatusServiceUnavailable, fmt.Sprintf("can't count the volumes of tenant %s: %v", t.Nombre, errors.Join(errs...))}
		}
		if len(propios)+len(plan.clones) > t.MaxVolumenes {
			return nil, &errVolumen{http.StatusTooManyRequests, fmt.Sprintf(
				"tenant %s already has %d volumes and asks for %d more; the limit is %d", t.Nombre, len(propios), len(plan.clones), t.MaxVolumenes)}
		}
	}
	return plan, nil
}

// clonar crea los volúmenes del plan en el primer host (orden del registro)
// donde caben, y deja el plan fijado a él. Devuelve lo que creó, para poder
// deshacerlo.
func (s *Servidor) clonar(ctx context.Context, plan *planVolumenes) (*hosts.Host, []string, error) {
	if plan == nil || len(plan.clones) == 0 {
		return nil, nil, nil
	}
	var h *hosts.Host
	for _, c := range s.reg.Todos() {
		if plan.solo[c.Nombre] {
			h = c
			break
		}
	}
	if h == nil {
		return nil, nil, hosts.ErrSinSitio
	}
	var hechos []string
	for _, c := range plan.clones {
		if _, err := h.Cliente.CloneVolume(ctx, c.origen, api.CloneVolumeRequest{To: c.destino}); err != nil {
			s.borrarVolumenes(h, hechos)
			return nil, nil, deHost(h, err)
		}
		hechos = append(hechos, c.destino)
	}
	plan.solo = map[string]bool{h.Nombre: true}
	return h, hechos, nil
}

// borrarVolumenes deshace clones propios tras un fallo; con contexto propio
// porque suele llamarse con el de la petición ya cancelado.
func (s *Servidor) borrarVolumenes(h *hosts.Host, reales []string) {
	for _, n := range reales {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := h.Cliente.RemoveVolume(ctx, n); err != nil {
			s.log.Printf("frontal: removing volume %s on %s: %v", n, h.Nombre, err)
		}
		cancel()
	}
}

// ---- rutas /v1/volumes

func (s *Servidor) handleVolumenes(w http.ResponseWriter, r *http.Request) {
	t := tenantDe(r)
	nombre := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/volumes"), "/")
	switch {
	case nombre == "" && r.Method == http.MethodGet:
		s.handleListarVolumenes(w, r, t)
	case nombre == "" && r.Method == http.MethodPost:
		s.handleCrearVolumen(w, r, t)
	case nombre == "":
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	case r.Method != http.MethodGet && r.Method != http.MethodDelete:
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	case strings.Contains(nombre, "/"):
		fail(w, http.StatusNotFound, noVolumen(nombre))
	case r.Method == http.MethodGet:
		s.handleVerVolumen(w, r, t, nombre)
	default:
		s.handleBorrarVolumen(w, r, t, nombre)
	}
}

func (s *Servidor) handleListarVolumenes(w http.ResponseWriter, r *http.Request, t *Tenant) {
	vs, errs := s.volumenesDe(r.Context(), t)
	if len(errs) > 0 {
		fail(w, http.StatusBadGateway, errors.Join(errs...))
		return
	}
	out := make([]Volumen, 0, len(vs))
	for _, p := range vs {
		out = append(out, vistaVolumen(t, p.h, p.v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Servidor) buscarVolumen(ctx context.Context, t *Tenant, nombre string) (volEnHost, error) {
	if !reVolumen.MatchString(nombre) {
		return volEnHost{}, noVolumen(nombre)
	}
	vs, errs := s.volumenesDe(ctx, t)
	for _, p := range vs {
		if p.v.Name == nombreReal(t, nombre) || p.v.Name == prefijoCorto(t)+nombre {
			return p, nil
		}
	}
	if len(errs) > 0 {
		return volEnHost{}, errors.Join(errs...)
	}
	return volEnHost{}, noVolumen(nombre)
}

func codigoVolumen(err error) int {
	var ev *errVolumen
	if errors.As(err, &ev) {
		return ev.code
	}
	return codigoDeHost(err)
}

func (s *Servidor) handleVerVolumen(w http.ResponseWriter, r *http.Request, t *Tenant, nombre string) {
	p, err := s.buscarVolumen(r.Context(), t, nombre)
	if err != nil {
		fail(w, codigoVolumen(err), err)
		return
	}
	writeJSON(w, http.StatusOK, vistaVolumen(t, p.h, p.v))
}

func (s *Servidor) handleBorrarVolumen(w http.ResponseWriter, r *http.Request, t *Tenant, nombre string) {
	p, err := s.buscarVolumen(r.Context(), t, nombre)
	if err != nil {
		fail(w, codigoVolumen(err), err)
		return
	}
	// Un volumen en uso lo rechaza el daemon con 409.
	if err := p.h.Cliente.RemoveVolume(r.Context(), p.v.Name); err != nil {
		fail(w, codigoDeHost(err), deHost(p.h, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Servidor) handleCrearVolumen(w http.ResponseWriter, r *http.Request, t *Tenant) {
	var pet CrearVolumenPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if !reVolumen.MatchString(pet.Name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("volume name %q is not valid (lowercase letters, digits, '_' and '-', up to 41)", pet.Name))
		return
	}
	if pet.SizeMiB < 0 || pet.SizeMiB > s.maxVolumenMiB {
		fail(w, http.StatusBadRequest, fmt.Errorf("size_mib must be between 0 and %d", s.maxVolumenMiB))
		return
	}
	if pet.CloneFrom != "" && (pet.SizeMiB != 0 || !reVolumen.MatchString(pet.CloneFrom) || pet.CloneFrom == pet.Name) {
		fail(w, http.StatusBadRequest, errors.New("clone_from needs a different, valid volume name and no size_mib"))
		return
	}
	if pet.Host != "" && !nombreValido.MatchString(pet.Host) {
		fail(w, http.StatusBadRequest, fmt.Errorf("host %q is not valid", pet.Host))
		return
	}
	// Contar y crear bajo el cerrojo del tenant: dos POST a la vez no pasan
	// juntos el tope.
	defer s.bloquear(t.Nombre + "\x00vol")()
	propios, errs := s.volumenesDe(r.Context(), t)
	if len(errs) > 0 {
		fail(w, http.StatusServiceUnavailable, errors.Join(errs...))
		return
	}
	real := nombreReal(t, pet.Name)
	var origen map[string]bool
	realOrigen := nombreReal(t, pet.CloneFrom)
	for _, p := range propios {
		if p.v.Name == real || p.v.Name == prefijoCorto(t)+pet.Name {
			fail(w, http.StatusConflict, fmt.Errorf("volume %q already exists", pet.Name))
			return
		}
		if pet.CloneFrom != "" && (p.v.Name == realOrigen || p.v.Name == prefijoCorto(t)+pet.CloneFrom) {
			realOrigen = p.v.Name
			if origen == nil {
				origen = map[string]bool{}
			}
			origen[p.h.Nombre] = true
		}
	}
	if t.MaxVolumenes > 0 && len(propios) >= t.MaxVolumenes {
		fail(w, http.StatusTooManyRequests, fmt.Errorf("tenant %s already has %d volumes; the limit is %d", t.Nombre, len(propios), t.MaxVolumenes))
		return
	}
	if pet.CloneFrom != "" && origen == nil {
		fail(w, http.StatusNotFound, noVolumen(pet.CloneFrom))
		return
	}
	sirve := func(h *hosts.Host) bool {
		return (pet.Host == "" || h.Nombre == pet.Host) && (origen == nil || origen[h.Nombre])
	}
	if pet.Host != "" {
		if _, ok := s.reg.Host(pet.Host); !ok {
			fail(w, http.StatusNotFound, fmt.Errorf("no host %q", pet.Host))
			return
		}
	}
	var v *api.Volume
	var h *hosts.Host
	var err error
	if pet.CloneFrom != "" {
		var res *api.CloneVolumeResult
		res, h, err = hosts.Intentar(r.Context(), s.reg, sirve, func(ctx context.Context, h *hosts.Host) (*api.CloneVolumeResult, error) {
			return h.Cliente.CloneVolume(ctx, realOrigen, api.CloneVolumeRequest{To: real})
		})
		if err == nil {
			v = res.Volume
		}
	} else {
		v, h, err = hosts.Intentar(r.Context(), s.reg, sirve, func(ctx context.Context, h *hosts.Host) (*api.Volume, error) {
			return h.Cliente.CreateVolume(ctx, api.CreateVolumeRequest{Name: real, SizeMiB: pet.SizeMiB, Labels: etiquetasVolumen(t)})
		})
	}
	if err != nil {
		fail(w, codigoDeHost(err), err)
		return
	}
	if v == nil {
		v = &api.Volume{Name: real}
	}
	// El clon hereda las etiquetas del origen, que ya son las del tenant.
	w.Header().Set("Location", "/v1/volumes/"+pet.Name)
	writeJSON(w, http.StatusCreated, vistaVolumen(t, h, v))
}
