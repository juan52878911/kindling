package mcp

import (
	"context"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// ToolsOf devuelve el catálogo capturado del snapshot y cuándo se capturó. Lee
// la anotación; si no está —un daemon v0.4, que solo conoce el campo— cae al
// campo antiguo.
func ToolsOf(s *api.Snapshot) ([]ToolSpec, *time.Time) {
	var t Tools
	if ok, err := s.Annotation(ToolsKey, &t); ok && err == nil {
		return t.Tools, t.CapturedAt
	}
	return s.Tools, s.ToolsAt
}

// HealthOf devuelve el veredicto del último sondeo del snapshot.
func HealthOf(s *api.Snapshot) Health {
	var h Health
	if ok, err := s.Annotation(HealthKey, &h); ok && err == nil {
		return h
	}
	return Health{Status: s.Health, At: s.HealthAt, Error: s.HealthErr}
}

// SetTools guarda el catálogo del servicio.
func SetTools(ctx context.Context, c *api.Client, name string, tools []ToolSpec) error {
	now := time.Now()
	_, err := c.SetAnnotation(ctx, name, ToolsKey, Tools{Tools: tools, CapturedAt: &now})
	if api.IsUnsupported(err) && !isMissingSnapshot(err) {
		_, err = c.SetCatalog(ctx, name, tools)
	}
	return err
}

// SetHealth guarda el veredicto de un sondeo. cause solo cuenta si no está sano.
func SetHealth(ctx context.Context, c *api.Client, name string, healthy bool, cause string) error {
	now := time.Now()
	h := Health{Status: Healthy, At: &now}
	if !healthy {
		h.Status, h.Error = Unhealthy, cause
	}
	_, err := c.SetAnnotation(ctx, name, HealthKey, h)
	if api.IsUnsupported(err) && !isMissingSnapshot(err) {
		_, err = c.SetHealth(ctx, name, healthy, cause)
	}
	return err
}

// isMissingSnapshot distingue el 404 de "este snapshot no existe" del 404 de
// "este daemon no conoce la ruta": el primero no se arregla con la ruta vieja.
func isMissingSnapshot(err error) bool {
	return err != nil && strings.Contains(err.Error(), "does not exist")
}
