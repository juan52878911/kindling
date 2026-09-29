package daemon

// API de los grafos de microVMs (docs/grafos.md, capacidad "graphs"). La
// lógica vive en internal/machine/grafo*.go; aquí solo se traduce HTTP.

import (
	"errors"
	"net/http"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// grafoMaxCuerpo: un grafo de 32 nodos y 64 aristas con sus claves cabe de
// sobra.
const grafoMaxCuerpo = 1 << 20

// graphStatus es el código de un error de grafo sin código propio.
func graphStatus(err error) int {
	if errors.Is(err, machine.ErrNoGraph) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func (s *Server) handleGraphUp(w http.ResponseWriter, r *http.Request) {
	var req api.GraphRequest
	if err := decodeJSONCon(w, r, &req, grafoMaxCuerpo); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	g, err := s.mgr.GraphUp(r.Context(), req.Graph, req.Secrets)
	if err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleGraphs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, filtrarGrafos(r, s.mgr.Graphs()))
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	g, err := s.mgr.Graph(r.PathValue("ref"))
	if err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleGraphFreeze(w http.ResponseWriter, r *http.Request) {
	g, err := s.mgr.GraphFreeze(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleGraphThaw(w http.ResponseWriter, r *http.Request) {
	g, err := s.mgr.GraphThaw(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleGraphSnapshot(w http.ResponseWriter, r *http.Request) {
	var req api.GraphSnapshotRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			fail(w, jsonBodyStatus(err), err)
			return
		}
	}
	snap, err := s.mgr.GraphSnapshot(r.Context(), r.PathValue("ref"), req.Name)
	if err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleGraphFork(w http.ResponseWriter, r *http.Request) {
	var req api.GraphForkRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			fail(w, jsonBodyStatus(err), err)
			return
		}
	}
	gs, err := s.mgr.GraphFork(r.Context(), r.PathValue("ref"), req.Count)
	if err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, api.GraphForkResult{Graphs: gs})
}

func (s *Server) handleGraphRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.GraphRemove(r.Context(), r.PathValue("ref")); err != nil {
		fail(w, graphStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
