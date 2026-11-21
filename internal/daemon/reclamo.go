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

// revisarReclamo es el revisor de la ruta bajo política de autorización: las
// mismas reglas de etiquetas que PUT /machines/{ref}/labels (nadie fija
// kling.owner), y el dueño que sella el daemon es quien llama, de modo que la
// cuota se comprueba contra él al reclamar.
func revisarReclamo(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.ClaimRequest) error {
		if err := etiquetasDeInquilino(t, req.Labels); err != nil {
			return err
		}
		if req.Labels == nil {
			req.Labels = map[string]string{}
		}
		req.Labels[api.LabelOwner] = t
		return nil
	})
}

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
