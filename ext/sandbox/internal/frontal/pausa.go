package frontal

// Pausa, reanudación y política de salida en caliente de un sandbox. Son
// pasarelas finas al daemon del host: el control de inquilino es el mismo que
// el de renew (buscar solo devuelve sandboxes del tenant) y los errores del
// daemon (409 por estado, 400 por una política inválida) llegan tal cual.

import (
	"net/http"

	"github.com/juan52878911/kindling/pkg/api"
)

// handlePausar sirve POST /v1/sandboxes/{id}/pause: pausa un sandbox running
// sin volcar su memoria.
func (s *Servidor) handlePausar(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	out, err := h.Cliente.Pause(r.Context(), mc.ID)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, vista(h, out))
}

// handleReanudar sirve POST /v1/sandboxes/{id}/resume: despierta un sandbox
// pausado o congelado.
func (s *Servidor) handleReanudar(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	out, err := h.Cliente.Thaw(r.Context(), mc.ID)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, vista(h, out))
}

// handleEgress sirve PATCH /v1/sandboxes/{id}/egress: cambia el modo de salida
// (y la lista de dominios) de un sandbox vivo.
func (s *Servidor) handleEgress(w http.ResponseWriter, r *http.Request, id string) {
	var pet api.EgressRequest
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	out, err := h.Cliente.SetEgress(r.Context(), mc.ID, pet)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}
