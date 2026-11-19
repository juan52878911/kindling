package auditoria

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/telemetria"
)

// Servir atiende GET /v1/audit?since=…&limit=…&tenant=…: un admin ve todo (y
// puede filtrar por tenant); un inquilino, solo lo suyo. since es RFC 3339 o
// una duración ("15m": desde hace 15 minutos). Va en telemetria.Config.Propios.
func Servir(r *Registro) func(http.ResponseWriter, *http.Request, telemetria.Identidad, bool) bool {
	return func(w http.ResponseWriter, req *http.Request, id telemetria.Identidad, ok bool) bool {
		if req.URL.Path != "/v1/audit" {
			return false
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kindling-sandbox"`)
			responder(w, http.StatusUnauthorized, map[string]string{"error": "missing Authorization header: Bearer <token>, or the token is invalid"})
			return true
		}
		if req.Method != http.MethodGet {
			responder(w, http.StatusMethodNotAllowed, map[string]string{"error": req.Method + " is not allowed on this route"})
			return true
		}
		q := req.URL.Query()
		var desde time.Time
		if s := q.Get("since"); s != "" {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				desde = t
			} else if d, err := time.ParseDuration(s); err == nil && d >= 0 {
				desde = time.Now().Add(-d)
			} else {
				responder(w, http.StatusBadRequest, map[string]string{"error": "since: RFC 3339 time or a duration like 15m"})
				return true
			}
		}
		limite := 1000
		if s := q.Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 10000 {
				responder(w, http.StatusBadRequest, map[string]string{"error": "limit: 1 to 10000"})
				return true
			}
			limite = n
		}
		tenant := id.Nombre
		if id.Admin {
			tenant = q.Get("tenant") // vacío = todos
		}
		ls, mas, err := r.Leer(desde, tenant, limite)
		if err != nil {
			responder(w, http.StatusInternalServerError, map[string]string{"error": "reading the audit log failed"})
			return true
		}
		if ls == nil {
			ls = []Linea{}
		}
		responder(w, http.StatusOK, map[string]any{"events": ls, "truncated": mas})
		return true
	}
}

func responder(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
