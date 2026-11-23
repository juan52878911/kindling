package frontal

// API de inquilinos y claves (solo admins): /v1/tenants.
//
// Los inquilinos de KLING_SANDBOX_TENANTS salen en las lecturas como de solo
// lectura (source=env); los del almacén se crean, cambian y borran aquí. La
// clave en claro solo viaja en la respuesta que la crea o la rota.

import (
	"errors"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/inquilinos"
)

type vistaClave struct {
	ID        string     `json:"id"`
	Prefijo   string     `json:"prefix"`
	Creada    time.Time  `json:"created"`
	Caduca    *time.Time `json:"expires,omitempty"`
	Revocada  *time.Time `json:"revoked,omitempty"`
	UltimoUso *time.Time `json:"last_used,omitempty"`
}

type vistaTenant struct {
	ID       string           `json:"id"`
	Nombre   string           `json:"name"`
	Origen   string           `json:"source"` // "store" o "env"
	SoloLeer bool             `json:"read_only"`
	Admin    bool             `json:"admin"`
	Topes    inquilinos.Topes `json:"limits"`
	Creado   *time.Time       `json:"created,omitempty"`
	Claves   []vistaClave     `json:"keys"`
}

func vistaDeClave(c inquilinos.Clave) vistaClave {
	return vistaClave{ID: c.ID, Prefijo: c.Prefijo, Creada: c.Creada, Caduca: c.Caduca, Revocada: c.Revocada, UltimoUso: c.UltimoUso}
}

func vistaDeAlmacen(q inquilinos.Inquilino) vistaTenant {
	v := vistaTenant{ID: q.Nombre, Nombre: q.Nombre, Origen: "store", Admin: q.Admin, Topes: q.Topes, Creado: &q.Creado, Claves: []vistaClave{}}
	for _, c := range q.Claves {
		v.Claves = append(v.Claves, vistaDeClave(c))
	}
	return v
}

func vistaDeEntorno(t Tenant) vistaTenant {
	return vistaTenant{ID: t.Nombre, Nombre: t.Nombre, Origen: "env", SoloLeer: true, Admin: t.Admin, Claves: []vistaClave{},
		Topes: inquilinos.Topes{
			MaxSandboxes: t.MaxSandboxes, MaxPorPlantilla: t.MaxPorPlantilla, MaxShells: t.MaxShells,
			MaxProcesos: t.MaxProcesos, MaxSesiones: t.MaxSesiones, MaxSeguir: t.MaxSeguir,
			MaxMemMiB: t.MaxMemMiB, MaxVCPUs: t.MaxVCPUs, MaxMemTotalMiB: t.MaxMemTotalMiB,
			MaxLote: t.MaxLote, MaxVolumenes: t.MaxVolumenes,
		}}
}

// entorno devuelve el tenant del entorno con ese nombre.
func (s *Servidor) entorno(n string) *Tenant {
	for i := range s.tenants {
		if s.tenants[i].Nombre == n {
			return &s.tenants[i]
		}
	}
	return nil
}

// soloAdmin envuelve un handler para que lo conteste solo un admin.
func (s *Servidor) soloAdmin(h http.HandlerFunc) http.Handler {
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tenantDe(r).Admin {
			fail(w, http.StatusForbidden, errors.New("managing tenants and keys needs an admin token"))
			return
		}
		h(w, r)
	}))
}

func (s *Servidor) rutasTenants(m *http.ServeMux) {
	m.Handle("GET /v1/tenants", s.soloAdmin(s.handleListarTenants))
	m.Handle("POST /v1/tenants", s.soloAdmin(s.handleCrearTenant))
	m.Handle("GET /v1/tenants/{id}", s.soloAdmin(s.handleVerTenant))
	m.Handle("PATCH /v1/tenants/{id}", s.soloAdmin(s.handleCambiarTenant))
	m.Handle("DELETE /v1/tenants/{id}", s.soloAdmin(s.handleBorrarTenant))
	m.Handle("POST /v1/tenants/{id}/keys", s.soloAdmin(s.handleCrearClave))
	m.Handle("POST /v1/tenants/{id}/keys/{kid}/rotate", s.soloAdmin(s.handleRotarClave))
	m.Handle("DELETE /v1/tenants/{id}/keys/{kid}", s.soloAdmin(s.handleRevocarClave))
}

// errAlmacen traduce un error del almacén a su respuesta.
func errAlmacen(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inquilinos.ErrNoExiste), errors.Is(err, inquilinos.ErrClaveNoExiste):
		fail(w, http.StatusNotFound, err)
	case errors.Is(err, inquilinos.ErrExiste):
		fail(w, http.StatusConflict, err)
	case errors.Is(err, inquilinos.ErrNombre), errors.Is(err, inquilinos.ErrTope), errors.Is(err, inquilinos.ErrGracia), errors.Is(err, inquilinos.ErrPasado):
		fail(w, http.StatusBadRequest, err)
	default:
		// fail oculta el detalle en los 5xx: un error de disco nombra rutas.
		fail(w, http.StatusInternalServerError, err)
	}
}

// escritura comprueba que se pueda modificar el tenant id: hace falta almacén
// y que no sea del entorno. Devuelve false si ya contestó.
func (s *Servidor) escritura(w http.ResponseWriter, id string) bool {
	if s.almacen == nil {
		fail(w, http.StatusNotImplemented, errors.New("the tenant store is not enabled (start the gateway with -state-dir)"))
		return false
	}
	if s.entorno(id) != nil {
		fail(w, http.StatusConflict, errors.New("tenant "+id+" comes from KLING_SANDBOX_TENANTS and is read-only"))
		return false
	}
	return true
}

func (s *Servidor) handleListarTenants(w http.ResponseWriter, r *http.Request) {
	out := []vistaTenant{}
	for _, t := range s.tenants {
		out = append(out, vistaDeEntorno(t))
	}
	if s.almacen != nil {
		for _, q := range s.almacen.Listar() {
			out = append(out, vistaDeAlmacen(q))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": out})
}

func (s *Servidor) handleVerTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if t := s.entorno(id); t != nil {
		writeJSON(w, http.StatusOK, vistaDeEntorno(*t))
		return
	}
	if s.almacen != nil {
		if q, err := s.almacen.Obtener(id); err == nil {
			writeJSON(w, http.StatusOK, vistaDeAlmacen(q))
			return
		}
	}
	errAlmacen(w, inquilinos.ErrNoExiste)
}

type pedirTenant struct {
	Nombre string           `json:"name"`
	Admin  bool             `json:"admin"`
	Topes  inquilinos.Topes `json:"limits"`
}

func (s *Servidor) handleCrearTenant(w http.ResponseWriter, r *http.Request) {
	var p pedirTenant
	if err := leerJSON(r, &p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if !s.escritura(w, p.Nombre) {
		return
	}
	q, err := s.almacen.Crear(p.Nombre, p.Admin, p.Topes)
	if err != nil {
		errAlmacen(w, err)
		return
	}
	s.avisarCuotasAbiertas(s.deAlmacen(q))
	writeJSON(w, http.StatusCreated, vistaDeAlmacen(q))
}

type cambiarTenant struct {
	Admin *bool             `json:"admin"`
	Topes *inquilinos.Topes `json:"limits"`
}

func (s *Servidor) handleCambiarTenant(w http.ResponseWriter, r *http.Request) {
	var p cambiarTenant
	if err := leerJSON(r, &p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	if !s.escritura(w, id) {
		return
	}
	// Un admin no puede quitarse el rol a sí mismo: si fuera el último del
	// almacén, la API quedaría sin nadie que la use.
	if p.Admin != nil && !*p.Admin && tenantDe(r).Nombre == id {
		fail(w, http.StatusConflict, errors.New("an admin can't remove its own admin role"))
		return
	}
	q, err := s.almacen.Actualizar(id, p.Admin, p.Topes)
	if err != nil {
		errAlmacen(w, err)
		return
	}
	s.avisarCuotasAbiertas(s.deAlmacen(q))
	writeJSON(w, http.StatusOK, vistaDeAlmacen(q))
}

func (s *Servidor) handleBorrarTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.escritura(w, id) {
		return
	}
	if tenantDe(r).Nombre == id {
		fail(w, http.StatusConflict, errors.New("an admin can't delete itself"))
		return
	}
	if err := s.almacen.Borrar(id); err != nil {
		errAlmacen(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// claveCreada es la única respuesta que lleva una clave en claro.
type claveCreada struct {
	ID           string     `json:"id"`
	Prefijo      string     `json:"prefix"`
	Creada       time.Time  `json:"created"`
	Caduca       *time.Time `json:"expires,omitempty"`
	Clave        string     `json:"key"`
	ViejaCaduca  *time.Time `json:"old_key_expires,omitempty"`
	ViejaClaveID string     `json:"old_key_id,omitempty"`
}

func nuevaClaveCreada(c inquilinos.Clave, texto string) claveCreada {
	return claveCreada{ID: c.ID, Prefijo: c.Prefijo, Creada: c.Creada, Caduca: c.Caduca, Clave: texto}
}

type pedirClave struct {
	Caduca *time.Time `json:"expires_at"`
}

func (s *Servidor) handleCrearClave(w http.ResponseWriter, r *http.Request) {
	var p pedirClave
	if err := leerJSON(r, &p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	if !s.escritura(w, id) {
		return
	}
	c, texto, err := s.almacen.NuevaClave(id, p.Caduca)
	if err != nil {
		errAlmacen(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, nuevaClaveCreada(c, texto))
}

type pedirRotar struct {
	// GraciaMin es cuántos minutos sigue valiendo la clave vieja (por defecto
	// 10; 0 = deja de valer ya).
	GraciaMin *int `json:"grace_minutes"`
}

func (s *Servidor) handleRotarClave(w http.ResponseWriter, r *http.Request) {
	var p pedirRotar
	if err := leerJSON(r, &p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	if !s.escritura(w, id) {
		return
	}
	gracia := 10
	if p.GraciaMin != nil {
		gracia = *p.GraciaMin
	}
	if gracia < 0 || gracia > int(inquilinos.MaxGracia/time.Minute) {
		errAlmacen(w, inquilinos.ErrGracia)
		return
	}
	nueva, texto, vieja, err := s.almacen.Rotar(id, r.PathValue("kid"), time.Duration(gracia)*time.Minute)
	if err != nil {
		errAlmacen(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, func() claveCreada {
		r := nuevaClaveCreada(nueva, texto)
		r.ViejaClaveID, r.ViejaCaduca = vieja.ID, vieja.Caduca
		return r
	}())
}

func (s *Servidor) handleRevocarClave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.escritura(w, id) {
		return
	}
	if err := s.almacen.Revocar(id, r.PathValue("kid")); err != nil {
		errAlmacen(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
