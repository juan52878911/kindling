// Package consola sirve la consola web mínima del gateway: HTML, JS y CSS
// embebidos, sin dependencias ni CDN. Habla con la API /v1 desde el navegador
// con el token de inquilino que pega el usuario; no amplía ningún permiso.
package consola

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var web embed.FS

// Prefijo es la única ruta que sirve el paquete.
const Prefijo = "/console/"

// csp prohíbe todo lo que no venga del propio origen.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Recursos devuelve el sistema de ficheros embebido (para los tests).
func Recursos() fs.FS {
	sub, _ := fs.Sub(web, "web")
	return sub
}

// Handler sirve los recursos bajo Prefijo con las cabeceras de seguridad.
// Solo responde a GET y HEAD.
func Handler() http.Handler {
	files := http.StripPrefix(Prefijo, http.FileServer(http.FS(Recursos())))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	})
}
