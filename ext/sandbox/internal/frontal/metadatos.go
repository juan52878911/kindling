package frontal

// Metadatos y métricas de un sandbox:
//
//	PATCH /v1/sandboxes/{id}/metadata   añade, cambia y quita etiquetas propias
//	GET   /v1/sandboxes/{id}/metrics    memoria (y CPU, si el daemon la da) del sandbox

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// MetadatosPeticion es el cuerpo de PATCH .../metadata. Labels se añaden o
// cambian (el valor no puede ir vacío) y Remove se quitan; una clave no puede
// estar en las dos.
type MetadatosPeticion struct {
	Labels map[string]string `json:"labels,omitempty"`
	Remove []string          `json:"remove,omitempty"`
}

// maxValorEtiqueta acota el valor de una etiqueta de usuario.
const maxValorEtiqueta = 256

// etiquetaDeUsuario dice si k es una etiqueta que el cliente puede tocar: las
// del frontal (kind, tenant, template) y todo el espacio kling. son suyas.
func etiquetaDeUsuario(k string) bool {
	return k != api.LabelKind && k != LabelTenant && k != LabelTemplate && !strings.HasPrefix(k, prefijoReservado)
}

func (s *Servidor) handleMetadatos(w http.ResponseWriter, r *http.Request, id string) {
	var req MetadatosPeticion
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Labels) == 0 && len(req.Remove) == 0 {
		fail(w, http.StatusBadRequest, errors.New("nothing to change: pass labels and/or remove"))
		return
	}
	cambio := map[string]string{}
	for k, v := range req.Labels {
		switch {
		case !etiquetaDeUsuario(k):
			fail(w, http.StatusBadRequest, fmt.Errorf("label %q is reserved", k))
			return
		case !api.KeyPattern.MatchString(k):
			fail(w, http.StatusBadRequest, fmt.Errorf("label %q is not valid (lowercase letters, digits, '.', '_', '-')", k))
			return
		case v == "":
			fail(w, http.StatusBadRequest, fmt.Errorf("label %q has an empty value: use remove to delete it", k))
			return
		case len(v) > maxValorEtiqueta:
			fail(w, http.StatusBadRequest, fmt.Errorf("label %q: value longer than %d bytes", k, maxValorEtiqueta))
			return
		}
		cambio[k] = v
	}
	for _, k := range req.Remove {
		if !etiquetaDeUsuario(k) {
			fail(w, http.StatusBadRequest, fmt.Errorf("label %q is reserved", k))
			return
		}
		if _, dos := cambio[k]; dos {
			fail(w, http.StatusBadRequest, fmt.Errorf("label %q is both set and removed", k))
			return
		}
		// El daemon solo fusiona etiquetas: quitar una es dejarla vacía, y
		// vista y filtros ignoran las vacías.
		cambio[k] = ""
	}
	h, _, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	// Leer, contar y escribir bajo el cerrojo del sandbox: dos PATCH a la vez
	// no se saltan juntos el tope de etiquetas.
	c := s.cerrojoDe(id + "\x00metadata")
	c.Lock()
	defer c.Unlock()
	_, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	nuevas := api.MergeLabels(mc.Labels, cambio)
	n := 0
	for k, v := range nuevas {
		if v != "" && etiquetaDeUsuario(k) {
			n++
		}
	}
	if n > maxEtiquetas {
		fail(w, http.StatusBadRequest, fmt.Errorf("too many labels (%d); the limit is %d", n, maxEtiquetas))
		return
	}
	if err := api.ValidateLabels(nuevas); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := h.Cliente.SetLabels(r.Context(), mc.ID, cambio); err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	c2 := *mc
	c2.Labels = nuevas
	writeJSON(w, http.StatusOK, vista(h, &c2))
}

// Metricas es la respuesta de GET .../metrics.
type Metricas struct {
	ID    string    `json:"id"`
	State api.State `json:"state"`
	// MemoryMiB es la memoria que el daemon mide del sandbox; ausente si no
	// la tiene (el sandbox está parado). MemoryKind dice qué mide: "pss"
	// (Linux) o "footprint" (macOS).
	MemoryMiB  int64  `json:"memory_mib,omitempty"`
	MemoryKind string `json:"memory_kind,omitempty"`
	// CPUSeconds es la CPU gastada desde que arrancó; ausente si el daemon no
	// la da.
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	// MemLimitMiB es la memoria con la que se creó.
	MemLimitMiB int `json:"mem_limit_mib,omitempty"`
}

func (s *Servidor) handleMetricas(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	ps, err := h.Cliente.ProcStats(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	out := Metricas{ID: idDe(h, mc), State: mc.State, MemLimitMiB: mc.MemMiB}
	for _, m := range ps.Machines {
		// Solo la fila de este sandbox: el resto del host es de otros.
		if m.ID != mc.ID {
			continue
		}
		out.State = m.State
		out.MemoryMiB, out.MemoryKind = m.PSSMiB, ps.MemoryKind
		if m.FootprintMiB > 0 {
			out.MemoryMiB = m.FootprintMiB
		}
		if out.MemoryKind == "" {
			out.MemoryKind = "pss"
		}
		out.CPUSeconds = m.CPUSeconds
		break
	}
	writeJSON(w, http.StatusOK, out)
}
