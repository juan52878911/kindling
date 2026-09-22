package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// ToolsOf devuelve el catálogo capturado del snapshot y cuándo se capturó.
func ToolsOf(s *api.Snapshot) ([]ToolSpec, *time.Time) {
	var t Tools
	if ok, err := s.Annotation(ToolsKey, &t); ok && err == nil {
		return t.Tools, t.CapturedAt
	}
	return nil, nil
}

// WithTools cuelga del snapshot la anotación mcp.tools con este catálogo y lo
// devuelve. Sirve para construir snapshots en tests y para quien tenga el
// catálogo en la mano sin haberlo leído del daemon.
func WithTools(s *api.Snapshot, tools []ToolSpec) *api.Snapshot {
	b, _ := json.Marshal(Tools{Tools: tools})
	if s.Annotations == nil {
		s.Annotations = map[string]json.RawMessage{}
	}
	s.Annotations[ToolsKey] = b
	return s
}

// HealthOf devuelve el veredicto del último sondeo del snapshot.
func HealthOf(s *api.Snapshot) Health {
	var h Health
	if ok, err := s.Annotation(HealthKey, &h); ok && err == nil {
		return h
	}
	return Health{}
}

// SetTools guarda el catálogo del servicio.
func SetTools(ctx context.Context, c *api.Client, name string, tools []ToolSpec) error {
	now := time.Now()
	_, err := c.SetAnnotation(ctx, name, ToolsKey, Tools{Tools: tools, CapturedAt: &now})
	return tooOld(err)
}

// SetHealth guarda el veredicto de un sondeo. cause solo cuenta si no está sano.
func SetHealth(ctx context.Context, c *api.Client, name string, healthy bool, cause string) error {
	now := time.Now()
	h := Health{Status: Healthy, At: &now}
	if !healthy {
		h.Status, h.Error = Unhealthy, cause
	}
	_, err := c.SetAnnotation(ctx, name, HealthKey, h)
	return tooOld(err)
}

// tooOld traduce el 404 de un daemon que no conoce la ruta en algo accionable.
// El 404 de "ese snapshot no existe" se deja tal cual.
func tooOld(err error) error {
	if err != nil && api.IsUnsupported(err) && !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("the kindling daemon is too old for kindling-mcp (it needs snapshot "+
			"annotations and the store, kindling v0.5 or newer): %w", err)
	}
	return err
}
