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

// Modos de aislamiento de un servicio con instancia persistente (anotación
// mcp.isolation).
//
//   - service: una instancia (y sus réplicas) para todas las sesiones. Cada
//     sesión tiene su proceso, pero comparten el disco de la instancia: lo que
//     una escribe en /tmp o en el directorio de datos lo lee la siguiente. Es
//     lo que un servicio stateful como `memory` quiere —su grafo es de todos— y
//     el comportamiento de siempre, por eso es el de por defecto.
//   - session: una microVM por sesión, restaurada del dorado con su propio
//     overlay, que se congela con la sesión dentro y se destruye al cerrarla o
//     caducar. Ninguna sesión ve lo que escribió otra. Los volúmenes siguen
//     siendo compartidos: son justo lo que se quiere conservar.
const (
	IsolationService = "service"
	IsolationSession = "session"
)

// IsolationKey es la anotación con el modo de aislamiento. Es una anotación y
// no una etiqueta para poder cambiarla sin reimportar (`kling mcp isolation`).
const IsolationKey = "mcp.isolation"

// Isolation devuelve el modo de aislamiento del snapshot: IsolationService si
// no lo declara o si el valor no se entiende.
func Isolation(s *api.Snapshot) string {
	var m string
	if s == nil {
		return IsolationService
	}
	if ok, err := s.Annotation(IsolationKey, &m); ok && err == nil && m == IsolationSession {
		return IsolationSession
	}
	return IsolationService
}

// ValidIsolation dice si m es un modo de aislamiento conocido.
func ValidIsolation(m string) bool { return m == IsolationService || m == IsolationSession }

// SetIsolation guarda el modo de aislamiento del servicio. service borra la
// anotación en vez de escribirla: es el valor por defecto, y así un snapshot
// que nunca la tuvo y uno que volvió a service son indistinguibles.
func SetIsolation(ctx context.Context, c *api.Client, name, mode string) error {
	if !ValidIsolation(mode) {
		return fmt.Errorf("unknown isolation %q (use %s or %s)", mode, IsolationService, IsolationSession)
	}
	if mode == IsolationService {
		return tooOld(c.RemoveAnnotation(ctx, name, IsolationKey))
	}
	_, err := c.SetAnnotation(ctx, name, IsolationKey, mode)
	return tooOld(err)
}

// SessionIsolationConflict explica por qué un servicio con estos volúmenes no
// puede aislar cada sesión, o devuelve nil si puede.
//
// Un volumen de escritura tiene un solo escritor (internal/machine/volume.go), y
// en modo session cada sesión es otra máquina que lo monta —congelada también
// cuenta—. La segunda sesión no arrancaría mientras la primera siguiera viva:
// el servicio atendería a una sola sesión a la vez hasta que la anterior se
// cerrase o caducase. Mejor decirlo al activarlo que en cada initialize.
func SessionIsolationConflict(vols []api.VolumeAttachment) error {
	for _, v := range vols {
		if !v.ReadOnly {
			return fmt.Errorf("volume %q is mounted read-write, and a volume has a single writer: "+
				"with one microVM per session, a second session could not start while the first one "+
				"exists (frozen included). Mount it read-only (%s:ro), or keep isolation service",
				v.Name, v.Name)
		}
	}
	return nil
}
