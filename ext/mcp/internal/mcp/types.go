// Package mcp reúne el vocabulario de MCP que hasta v0.4 vivía repartido por el
// núcleo: el catálogo de herramientas, la salud de un servicio, los servidores
// externos enlazados, la marca de "con estado" y el cable JSON-RPC sobre
// Streamable HTTP.
//
// Guarda su estado en el daemon con las piezas genéricas del núcleo —anotaciones
// de snapshot y el store— y no con campos propios. Es la mitad de kindling que
// se muda a kindling-mcp; mientras dure la transición (v0.5) habla con daemons
// anteriores cayendo a las rutas antiguas cuando las nuevas no existen.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// ToolSpec describe una herramienta tal y como la declaró su servidor MCP.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Link es un servidor MCP EXTERNO registrado en el agregador.
//
// No corre en una microVM: vive donde su dueño lo tenga —en el Mac, en otro
// host, en un servicio remoto— y kindling solo lo enruta. Sirve para traer
// capacidades que no tiene sentido meter en una máquina efímera, en particular
// las de memoria: un servicio al que todas las herramientas puedan escribir y
// del que puedan leer, sin que kindling tenga que implementar almacenamiento.
type Link struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Tools       []ToolSpec        `json:"tools,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Service devuelve el nombre de servicio del enlace.
func (l *Link) Service() string {
	if l.Labels != nil {
		if s := l.Labels[api.LabelService]; s != "" {
			return s
		}
	}
	return l.Name
}

// Capabilities son las capacidades que una imagen declara sobre lo que su
// servidor MCP necesita, detectadas de su árbol de dependencias al construirla.
// El import las usa para configurar el egress solo y avisar de módulos nativos.
type Capabilities struct {
	Browser bool     `json:"browser"`          // usa un navegador (Chromium)
	Egress  string   `json:"egress,omitempty"` // "none" | "internet" | "allowlist"
	Native  []string `json:"native,omitempty"` // módulos nativos npm detectados

	// NativeMissing son los módulos nativos que quedaron SIN su binario: el
	// empaquetado con --ignore-scripts no compila, y ni traían prebuild en el
	// tarball ni un paquete de plataforma que lo aportara. El servidor arranca e
	// introspecciona igual, pero la PRIMERA herramienta que los use peta en
	// caliente. Por eso el import lo trata como ERROR y no como aviso: es mejor
	// fallar al construir el catálogo que entregar un servicio que revienta luego.
	NativeMissing []string `json:"native_missing,omitempty"`

	// System son binarios del SISTEMA (no-npm) que el servidor invoca —git,
	// ffmpeg, ripgrep, pandoc, python…— y que el build horneó en la imagen con
	// apk. Sin ellos, la herramienta que los llame fallaría con "command not
	// found", un síntoma que no se parece a su causa. Es informativo: ya están
	// dentro.
	System []string `json:"system,omitempty"`

	// AllowDomains es la SEMILLA de dominios que el build extrajo de los literales
	// de URL del árbol npm. Es una pista editable, no una lista exhaustiva ni
	// autoritativa: el import la usa solo si se elige el modo "allowlist".
	AllowDomains []string `json:"allow_domains,omitempty"`
}

// Claves de las anotaciones que este paquete cuelga de cada snapshot. Sus formas
// JSON las comparte internal/machine/legacy_mcp.go para espejar los campos de
// v0.4; TestFormasCompatiblesConElNucleo lo vigila.
const (
	ToolsKey  = "mcp.tools"
	HealthKey = "mcp.health"
)

// Tools es la anotación mcp.tools: el catálogo que declaró el servidor al
// importarlo, capturado una vez para no despertar máquinas al listarlo.
type Tools struct {
	Tools      []ToolSpec `json:"tools"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
}

// Estados de salud.
const (
	Healthy   = "healthy"
	Unhealthy = "unhealthy"
)

// Health es la anotación mcp.health: el veredicto del último sondeo. Status
// vacío significa "nunca se ha sondeado".
type Health struct {
	Status string     `json:"status"`
	At     *time.Time `json:"at,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// LabelStateful marca los servicios que ACUMULAN estado entre llamadas.
//
// El modo efímero destruye la máquina tras cada acción, y con ella todo lo que
// el servidor guardara en memoria o en su disco. Para un servidor de ficheros
// da igual —el estado está fuera—, pero un grafo de conocimiento o un
// razonamiento por pasos perderían su contenido en cada invocación.
//
// Los servicios marcados así usan una instancia persistente, que se congela al
// quedar ociosa y vuelve en milisegundos conservando lo que tenía.
const LabelStateful = "stateful"

// Stateful indica si el servicio del snapshot debe conservar estado entre
// llamadas.
func Stateful(s *api.Snapshot) bool {
	return s != nil && s.Labels != nil && s.Labels[LabelStateful] == "true"
}

// CapabilitiesPath es donde 80-mcp-image.sh deja las capacidades detectadas de
// una imagen.
const CapabilitiesPath = "/etc/kling/capabilities.json"

// ImageCapabilities lee las capacidades que declara la imagen. nil, sin error,
// si la imagen no las declara (una construida antes de la detección, o que no es
// de servicio).
func ImageCapabilities(ctx context.Context, c *api.Client, image string) (*Capabilities, error) {
	b, err := c.ImageFile(ctx, image, CapabilitiesPath)
	if api.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var caps Capabilities
	if err := json.Unmarshal(b, &caps); err != nil {
		return nil, fmt.Errorf("%s in %s: %w", CapabilitiesPath, image, err)
	}
	return &caps, nil
}
