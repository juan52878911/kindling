package frontal

// Ganchos para quien envuelve el frontal desde fuera (telemetría y auditoría
// del gateway). Viven aparte para no tocar frontal.go.
//
// Desde fuera no se puede saber QUIÉN es el dueño de un token (el tenant se
// resuelve dentro de auth, después de que el envoltorio ya ha visto la
// petición) ni sacar las métricas sin un token de admin, así que se exponen
// estas dos cosas, y nada más: lo demás (ruta, código, cuerpo) se ve desde fuera.

import (
	"bytes"
	"context"
	"net/http"
)

// Identidad es lo que un envoltorio externo puede saber de un token: el nombre
// del inquilino y si es admin. Nunca el token.
type Identidad struct {
	Nombre string
	Admin  bool
}

// Identificar da el inquilino al que abre el token del encabezado Authorization
// ("Bearer <token>"). ok es false si no abre nada.
func (s *Servidor) Identificar(authorization string) (Identidad, bool) {
	tok, ok := bearer(authorization)
	if !ok {
		return Identidad{}, false
	}
	t := s.resolver(tok)
	if t == nil {
		return Identidad{}, false
	}
	return Identidad{Nombre: t.Nombre, Admin: t.Admin}, true
}

// MetricasTexto devuelve lo mismo que un admin vería en GET /v1/metrics
// (formato de texto de Prometheus), para exportarlo sin necesitar un token.
func (s *Servidor) MetricasTexto(ctx context.Context) (string, error) {
	adm := &Tenant{Nombre: "otlp", Admin: true}
	r, err := http.NewRequestWithContext(context.WithValue(ctx, claveTenant{}, adm), http.MethodGet, "/v1/metrics", nil)
	if err != nil {
		return "", err
	}
	w := &bufferRespuesta{h: http.Header{}}
	s.handleMetrics(w, r)
	return w.b.String(), nil
}

type bufferRespuesta struct {
	h http.Header
	b bytes.Buffer
}

func (w *bufferRespuesta) Header() http.Header         { return w.h }
func (w *bufferRespuesta) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w *bufferRespuesta) WriteHeader(int)             {}
