package daemon

// POST /sandboxes/claim (api.CapabilityClaim): reclamo atómico de una máquina
// del pool del gateway. La lógica vive en machine.Manager.Reclamar.

import (
	"errors"
	"net/http"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func init() { Capabilities = append(Capabilities, api.CapabilityClaim) }

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req api.ClaimRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	mc, err := s.mgr.Reclamar(req)
	if err != nil {
		if errors.Is(err, machine.ErrNoReclamable) {
			writeJSON(w, http.StatusNotFound, api.Error{Message: err.Error(), Code: api.CodeNoClaimable})
			return
		}
		status := cicloStatus(err)
		if errors.Is(err, machine.ErrNameTaken) {
			status = http.StatusConflict
		}
		fail(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}
