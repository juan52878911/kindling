// Package plugin es el protocolo con el que se amplía `kling`.
//
// `kling` es el único comando que teclea quien usa kindling. Lo que no es del
// núcleo —alojar servidores MCP, sandboxes de código— lo aporta una extensión:
// un ejecutable `kling-<nombre>` que declara qué subcomandos añade. `kling` lo
// descubre, lo muestra en su ayuda y en el completado, y cuando alguien teclea
// uno de esos subcomandos le pasa el control con el mismo proceso (exec), así
// que códigos de salida, señales y terminal llegan intactos.
//
// El contrato, visto desde la extensión:
//
//	kling-mcp --kling-manifest            imprime su Manifest en JSON y sale con 0
//	kling-mcp <comando> [args...]         ejecuta uno de sus comandos
//	kling-mcp --kling-hook <gancho> [...] participa en `kling status` o `kling up`
//
// Recibe en el entorno KLING_CONFIG (ruta de la configuración), KLING_VERSION
// (versión del núcleo), KLING_BIN (ruta de kling) y KLING_PLUGIN_API. Lo demás
// —KLING_HOST, KLING_SOCKET, el flag -H— se lo resuelve la extensión con
// pkg/config, con la misma precedencia que el núcleo.
//
// Una extensión escrita en Go usa Main (serve.go) y no tiene que implementar
// nada de esto a mano.
package plugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ManifestVersion es la versión del formato del manifiesto que entiende este
// núcleo.
const ManifestVersion = 1

// APIVersion es la versión del protocolo (entorno y argumentos) que el núcleo
// ofrece a las extensiones. Viaja en KLING_PLUGIN_API.
const APIVersion = "1"

// Ganchos que el núcleo sabe invocar.
const (
	HookStatus = "status" // añade líneas a `kling status` (o un objeto con -json)
	HookUp     = "up"     // comprueba o pone en marcha lo suyo en `kling up`
)

// Manifest es lo que una extensión declara de sí misma.
type Manifest struct {
	ManifestVersion int    `json:"manifest_version"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	// MinKling es la versión mínima del núcleo que necesita. Vacía = cualquiera.
	MinKling string `json:"min_kling,omitempty"`
	Summary  string `json:"summary,omitempty"`

	Commands []Command   `json:"commands"`
	Config   []ConfigKey `json:"config,omitempty"`
	Hooks    []string    `json:"hooks,omitempty"`

	// Units son unidades de systemd que la extensión instala en el host del
	// daemon (p. ej. "kling-gateway.service"). `kling up` las arranca junto al
	// daemon si están instaladas.
	Units []string `json:"units,omitempty"`
}

// Command es un subcomando de primer nivel que la extensión añade a `kling`.
type Command struct {
	Name string `json:"name"`
	// Group es la sección de `kling help` donde aparece (p. ej. "MCP SERVICES").
	Group   string `json:"group,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Usage es el bloque que se imprime en `kling help`, ya con su formato de
	// columnas. Vacío = una línea con Name y Summary.
	Usage string `json:"usage,omitempty"`
	// Subcommands alimentan el completado de la shell.
	Subcommands []string `json:"subcommands,omitempty"`
	// MachineArgs pide que el completado ofrezca ids de máquinas tras estos
	// subcomandos ("" = tras el comando mismo).
	MachineArgs []string `json:"machine_args,omitempty"`
}

// ConfigKey es una clave de configuración que la extensión declara. Se guarda en
// config.json bajo extensions.<nombre>.<clave> y se toca con
// `kling config set <nombre>.<clave> <valor>`.
type ConfigKey struct {
	Key  string `json:"key"`
	Type string `json:"type"` // string | bool | int | secret
	Help string `json:"help,omitempty"`
}

var (
	reName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reCommand = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reKey     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	reUnit    = regexp.MustCompile(`^[a-zA-Z0-9@_.-]+\.(service|timer|socket|path)$`)
)

// Validate comprueba que el manifiesto se puede usar. Un manifiesto inválido no
// rompe `kling`: la extensión aparece en `kling plugins` con su error y ya.
func (m *Manifest) Validate() error {
	if m.ManifestVersion != ManifestVersion {
		return fmt.Errorf("manifest_version %d; this kling understands %d", m.ManifestVersion, ManifestVersion)
	}
	if !reName.MatchString(m.Name) {
		return fmt.Errorf("invalid extension name %q", m.Name)
	}
	seen := map[string]bool{}
	for _, c := range m.Commands {
		if !reCommand.MatchString(c.Name) {
			return fmt.Errorf("invalid command name %q", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("command %q declared twice", c.Name)
		}
		seen[c.Name] = true
	}
	for _, h := range m.Hooks {
		if h != HookStatus && h != HookUp {
			return fmt.Errorf("unknown hook %q", h)
		}
	}
	for _, u := range m.Units {
		if !reUnit.MatchString(u) {
			return fmt.Errorf("invalid systemd unit %q", u)
		}
	}
	for _, k := range m.Config {
		if !reKey.MatchString(k.Key) {
			return fmt.Errorf("invalid config key %q", k.Key)
		}
		switch k.Type {
		case "string", "bool", "int", "secret":
		default:
			return fmt.Errorf("config key %q: unknown type %q", k.Key, k.Type)
		}
	}
	return nil
}

// HasHook dice si la extensión declara el gancho h.
func (m *Manifest) HasHook(h string) bool {
	for _, x := range m.Hooks {
		if x == h {
			return true
		}
	}
	return false
}

// Command devuelve el comando de nombre name, o nil.
func (m *Manifest) Command(name string) *Command {
	for i := range m.Commands {
		if m.Commands[i].Name == name {
			return &m.Commands[i]
		}
	}
	return nil
}

// ConfigKey devuelve la clave de configuración declarada, o nil.
func (m *Manifest) ConfigKey(key string) *ConfigKey {
	for i := range m.Config {
		if m.Config[i].Key == key {
			return &m.Config[i]
		}
	}
	return nil
}

// VersionAtLeast compara versiones "vX.Y.Z[-algo]" por sus tres números. Una
// versión que no se puede leer ("dev", la de un binario compilado a mano) se
// considera suficiente: quien compila a mano sabe lo que hace, y bloquearle por
// no tener etiqueta solo estorbaría.
func VersionAtLeast(have, want string) bool {
	if want == "" {
		return true
	}
	h, ok := parseVersion(have)
	if !ok {
		return true
	}
	w, ok := parseVersion(want)
	if !ok {
		return true
	}
	for i := 0; i < 3; i++ {
		if h[i] != w[i] {
			return h[i] > w[i]
		}
	}
	return true
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
