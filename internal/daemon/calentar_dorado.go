package daemon

import (
	"errors"
	"net/http"
)

// SEMILLA de la ronda 9 (analisis/ronda9-carga/plan-r9.md, C9-10). Dueño: Z1.
//
// handleWarmSnapshot es POST /snapshots/{name}/warm: trae el mem.file del
// dorado a la caché de página (lectores en paralelo, en segundo plano salvo
// api.WarmSnapshotRequest.Wait) y, con Pin, lo ancla o lo suelta. Contrato que
// Z1 tiene que cumplir:
//
//   - cuerpo api.WarmSnapshotRequest (vacío vale), respuesta
//     api.WarmSnapshotResult;
//   - 404 si el dorado no existe; 501 donde no se pueda (anclar en macOS);
//   - anclar exige además ser administrador (es RAM no recuperable del host)
//     y pasa por la admisión de memoria: sin sitio, 409 o 507 como un run;
//   - idempotente: repetirlo con el dorado ya caliente no lee nada.
//
// Hasta que Z1 lo implemente responde 501 (404 si el dorado no existe, que es
// ya el código definitivo), que para los clientes es "daemon sin la capacidad"
// (api.WarmUnavailable): siguen como hoy.
func (s *Server) handleWarmSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, err := s.mgr.Snapshot(r.PathValue("name")); err != nil {
		fail(w, snapshotStatus(err), err)
		return
	}
	fail(w, http.StatusNotImplemented, errors.New("snapshot warm: not implemented"))
}
