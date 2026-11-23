package frontal

// POST /v1/sandboxes/{id}/fork: ramifica un sandbox vivo en count copias con
// la memoria, los procesos y el disco que tiene ahora. Cada copia es un
// sandbox más del mismo inquilino y gasta cuota como si se hubiera creado.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// ForkPeticion es el cuerpo de POST .../fork.
type ForkPeticion struct {
	// Count: cuántas copias (por defecto 1; hasta el tope de lote del
	// inquilino y api.ForkMax).
	Count int `json:"count,omitempty"`
	// TTLSeconds de cada copia, por defecto el del original.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// ForkRespuesta son las copias, ya listas.
type ForkRespuesta struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

func (s *Servidor) handleFork(w http.ResponseWriter, r *http.Request, id string) {
	var req ForkPeticion
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	t := tenantDe(r)
	n := max(req.Count, 1)
	tope := min(api.ForkMax, t.MaxLote)
	if req.Count < 0 || n > tope {
		fail(w, http.StatusBadRequest, fmt.Errorf("count must be between 1 and %d", tope))
		return
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > api.SandboxMaxTTL {
		fail(w, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL))
		return
	}
	h, mc, err := s.buscar(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	// Cuota: las n copias se reservan de golpe, antes de pausar el original.
	plantilla, mem := mc.Labels[LabelTemplate], memCuota(mc.MemMiB)
	if err := s.reservarN(r.Context(), t, plantilla, mem, n); err != nil {
		codigo := codigoReservar(err)
		if codigo == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", retryAfterCuota)
		}
		fail(w, codigo, err)
		return
	}
	defer s.liberarN(t, plantilla, mem, n)

	// Las copias nacen con lo que las hace del inquilino y NO con lo del
	// original que no se hereda: los puertos públicos y sus revocaciones son
	// del original.
	etiquetas := map[string]string{LabelTenant: t.Nombre}
	for k, v := range mc.Labels {
		switch {
		case k == LabelTemplate || k == LabelEnv:
			etiquetas[k] = v
		case etiquetaDeUsuario(k) && v != "" && api.ValidateForkLabels(map[string]string{k: v}) == nil:
			etiquetas[k] = v
		}
	}
	res, err := h.Cliente.ForkSandbox(r.Context(), mc.ID, api.ForkRequest{
		Count: n, TTLSeconds: req.TTLSeconds, Labels: etiquetas,
	})
	if err != nil {
		code := codigoExec(err)
		var se *api.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound && strings.Contains(se.Message, "route") {
			code = http.StatusNotImplemented
		}
		fail(w, code, deHost(h, err))
		return
	}
	// Garantías que no dependen de lo que el daemon copie: dueño, marca de
	// sandbox y sin puertos públicos heredados.
	out := make([]Sandbox, 0, len(res.Sandboxes))
	for _, c := range res.Sandboxes {
		fix := map[string]string{}
		if c.Labels[LabelTenant] != t.Nombre {
			fix[LabelTenant] = t.Nombre
		}
		if c.Labels[api.LabelKind] != api.KindSandbox {
			fix[api.LabelKind] = api.KindSandbox
		}
		for _, k := range []string{LabelPublico, LabelRevocados} {
			if c.Labels[k] != "" {
				fix[k] = ""
			}
		}
		if len(fix) > 0 {
			if err := h.Cliente.SetLabels(r.Context(), c.ID, fix); err != nil {
				s.deshacerFork(h, res.Sandboxes)
				fail(w, http.StatusBadGateway, deHost(h, err))
				return
			}
			c.Labels = api.MergeLabels(c.Labels, fix)
		}
		out = append(out, vista(h, c))
	}
	if len(out) > 0 {
		w.Header().Set("Location", "/v1/sandboxes/"+out[0].ID)
	}
	writeJSON(w, http.StatusCreated, ForkRespuesta{Sandboxes: out})
}

// deshacerFork borra las copias de un fork que no se pudo dejar bien puesto: no
// se entrega ninguna, así que no deben quedar vivas sin dueño.
func (s *Servidor) deshacerFork(h *hosts.Host, copias []*api.Machine) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range copias {
		if err := h.Cliente.Remove(ctx, c.ID); err != nil {
			s.log.Printf("frontal: removing fork copy %s/%s: %v", h.Nombre, c.ID, err)
		}
	}
}
