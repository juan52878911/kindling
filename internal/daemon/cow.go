package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// handleGrowCoWStore amplía el almacén de copias de disco (kling cow grow,
// docs/cow.md). Solo admin: es espacio del host.
//
// Sin el contexto de la petición: cortar a medias un xfs_growfs porque el
// cliente se fue no gana nada (los dos crecen de forma transaccional, pero el
// fichero ya estaría reservado). Con un tope, eso sí.
func (s *Server) handleGrowCoWStore(w http.ResponseWriter, r *http.Request) {
	var req api.GrowCoWStoreRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	st, err := s.mgr.GrowCoWStore(ctx, req.SizeMiB, req.AddMiB)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
