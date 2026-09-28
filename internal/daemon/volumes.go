package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/juan52878911/kindling/pkg/api"
)

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Volumes())
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req api.CreateVolumeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	// Formatear un ext4 tarda; se acota por el contexto de la petición porque
	// aquí no queda nada a medias que pueda envenenar al host: CreateVolume
	// construye en .tmp y solo renombra al terminar.
	v, err := s.mgr.CreateVolume(r.Context(), req.Name, req.SizeMiB)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleRemoveVolume(w http.ResponseWriter, r *http.Request) {
	conSnaps, err := boolQuery(r, "snapshots")
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.mgr.RemoveVolume(r.PathValue("name"), conSnaps); err != nil {
		// 409 y no 400: "lo está usando alguien" es un conflicto de estado, no
		// una petición mal formada, y quien llama puede reintentar más tarde.
		fail(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePopulateVolume(w http.ResponseWriter, r *http.Request) {
	var req api.PopulateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	req.Volume = r.PathValue("name")

	// context.WithoutCancel a propósito, y no es un descuido.
	//
	// Si el cliente corta —se le acaba la paciencia, se cae la red—, cancelar
	// esto mataría la microVM a media instalación y dejaría el volumen a medio
	// poblar: con paquetes a medias, que es peor que vacío porque parece
	// completo. Se deja terminar y se destruye la máquina limpiamente.
	//
	// Ya pasó con la construcción de imágenes, y el síntoma fue una imagen
	// corrupta que hacía entrar en pánico al invitado siguiente.
	res, err := s.mgr.PopulateVolume(context.WithoutCancel(r.Context()), req)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// boolQuery lee un parámetro booleano; ausente es falso. Un valor que no se
// entiende es un 400, no un falso: ?snapshots=yes no puede acabar en "no".
func boolQuery(r *http.Request, k string) (bool, error) {
	v := r.URL.Query().Get(k)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid %s=%q: use 1 or 0", k, v)
	}
	return b, nil
}

// Los snapshots de volumen (ver machine/volume_snapshot.go). Los errores ya
// traen su código (400, 404, 409, 507); lo que no lo trae es un fallo del host.

func (s *Server) handleVolumeSnapshots(w http.ResponseWriter, r *http.Request) {
	l, err := s.mgr.VolumeSnapshots(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleSnapshotVolume(w http.ResponseWriter, r *http.Request) {
	var req api.SnapshotVolumeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	// Con el contexto de la petición: si el cliente corta, la copia se
	// cancela y se borra su .tmp; no queda nada publicado a medias.
	snap, err := s.mgr.SnapshotVolume(r.Context(), r.PathValue("name"), req.Name)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleRestoreVolume(w http.ResponseWriter, r *http.Request) {
	var req api.RestoreVolumeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	// Igual que el snapshot: cancelar antes de publicar no deja nada a medias,
	// y el volumen solo se sustituye con un rename al final.
	res, err := s.mgr.RestoreVolume(r.Context(), r.PathValue("name"), req.Snapshot)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRemoveVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RemoveVolumeSnapshot(r.PathValue("name"), r.PathValue("snap")); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
