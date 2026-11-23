package frontal

import "net/http"

// BusquedaFicheros es la respuesta de GET .../files/search.
type BusquedaFicheros struct {
	Path      string   `json:"path"`
	Glob      string   `json:"glob"`
	Matches   []string `json:"matches"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (s *Servidor) despachaFicheros(w http.ResponseWriter, r *http.Request, id string, resto []string) {
	if len(resto) == 0 {
		s.handleFiles(w, r, id)
		return
	}
	noRuta(w, r)
}
