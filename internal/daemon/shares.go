package daemon

// POST /shares/uploads: el contenido de una carpeta en modo copy, como un tar.
// Ver docs/compartir.md.

import (
	"errors"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/internal/share"
)

func (s *Server) handleShareUpload(w http.ResponseWriter, r *http.Request) {
	// El plazo de lectura del servidor (30 s) es para peticiones JSON; subir un
	// repositorio por un túnel SSH lento tarda más.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Minute))
	up, err := s.mgr.StageShareUpload(r.Context(), r.Body)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, share.ErrTooLarge):
			code = http.StatusRequestEntityTooLarge
		case errors.Is(err, machine.ErrShareRequest):
			code = http.StatusBadRequest
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusCreated, up)
}
