package daemon

import (
	"errors"
	"io"
	"net/http"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// handleWarmSnapshot es POST /snapshots/{name}/warm (C9-10): trae el
// mem.file del dorado a la caché de página (varios lectores, en segundo plano
// salvo api.WarmSnapshotRequest.Wait) y, con Pin, lo ancla o lo suelta
// (machine.Manager.CalentarDorado).
//
//   - cuerpo api.WarmSnapshotRequest (vacío vale), respuesta
//     api.WarmSnapshotResult;
//   - 404 si el dorado no existe; 501 si no se puede anclar (macOS);
//   - anclar o soltar exige ser administrador (es RAM no recuperable del
//     host): un inquilino recibe 403 aunque el dorado sea suyo. Calentar sin
//     anclar no, que es caché recuperable;
//   - anclar pasa por la admisión de memoria: sin sitio, 507 como un run;
//   - idempotente: repetirlo con el dorado ya caliente no lee nada.
func (s *Server) handleWarmSnapshot(w http.ResponseWriter, r *http.Request) {
	var req api.WarmSnapshotRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	if req.Pin != nil {
		if _, filtra := inquilinoDe(r); filtra {
			fail(w, http.StatusForbidden, errAuthz(http.StatusForbidden,
				"pinning a snapshot in RAM is admin-only under the daemon's authz policy: it is host memory that can't be reclaimed"))
			return
		}
	}
	res, err := s.mgr.CalentarDorado(r.Context(), r.PathValue("name"), req)
	if err != nil {
		// Un 507 de la admisión llega con su código (fail mira el StatusError).
		if errors.Is(err, machine.ErrAncladoNoSoportado) {
			fail(w, http.StatusNotImplemented, err)
		} else {
			fail(w, snapshotStatus(err), err)
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}
