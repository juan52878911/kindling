// Package config guarda la configuración persistente del CLI.
//
// JSON y no YAML a propósito: kindling no tiene dependencias externas y añadir
// uno solo por el formato del fichero de configuración no compensa.
//
// El fichero vive en ~/.config/kling/config.json (o $KLING_CONFIG).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/durable"
)

// Config es el contenido del fichero.
type Config struct {
	// CurrentContext es el contexto activo. Vacío = socket local.
	CurrentContext string `json:"current_context,omitempty"`

	// Contexts son daemons con nombre, como los de docker.
	Contexts map[string]*Context `json:"contexts,omitempty"`

	// Defaults evita repetir las mismas opciones en cada `kling run`.
	Defaults Defaults `json:"defaults"`

	// Gateway configura `kling gateway` sin flags.
	Gateway Gateway `json:"gateway"`

	// Extensions guarda la configuración que declaran las extensiones de kling:
	// extensions.<extensión>.<clave> = valor. El núcleo no sabe qué significa;
	// solo lo convierte al tipo que declara el manifiesto al escribirlo.
	Extensions map[string]map[string]json.RawMessage `json:"extensions,omitempty"`

	path string
}

type Context struct {
	Host        string `json:"host"`
	Description string `json:"description,omitempty"`
}

type Defaults struct {
	Image  string `json:"image,omitempty"`
	Egress string `json:"egress,omitempty"`
	MemMiB int    `json:"mem_mib,omitempty"`
	VCPUs  int    `json:"vcpus,omitempty"`
	CPUPct int    `json:"cpu_pct,omitempty"`
	TTL    int    `json:"ttl_seconds,omitempty"`
}

type Gateway struct {
	Listen string `json:"listen,omitempty"`
	Idle   string `json:"idle,omitempty"`

	// URL es la dirección por la que los AGENTES alcanzan el gateway, que no
	// tiene por qué ser la de escucha: el gateway puede escuchar en 0.0.0.0 y
	// los clientes llegar por la IP de la LAN.
	URL string `json:"url,omitempty"`

	// Token es el secreto que exige el gateway en Authorization: Bearer.
	//
	// En el host del gateway lo genera él solo la primera vez. En la máquina
	// donde corre el CLI hay que ponerlo a mano, porque son dos ficheros de
	// configuración distintos: `connect` lo necesita para comprobar que el
	// endpoint responde y para escribirlo en la configuración del agente.
	Token string `json:"token,omitempty"`

	// Tokens son tokens con nombre y cuotas, ADEMÁS del Token único de arriba.
	//
	// Retrocompatible a propósito: si esta lista está vacía, el gateway se
	// comporta exactamente como siempre con el Token único como tenant "default"
	// y sin límites. En cuanto hay tokens con nombre, cada uno lleva sus cuotas.
	Tokens []TokenLimit `json:"tokens,omitempty"`
}

// TokenLimit es un token con nombre y sus cuotas, para repartir el gateway entre
// varios clientes (tenants).
//
// OJO: las cuotas NO son una frontera de seguridad. Todas las microVMs comparten
// el mismo daemon y el mismo bridge de red, así que un tenant puede alcanzar lo
// de otro por debajo del gateway. Existen para OTRA cosa: reparto justo y
// contención de accidentes (un cliente en bucle que abre mil conexiones no debe
// dejar sin memoria ni sin servicio a los demás). Quien necesite aislamiento
// fuerte necesita daemons separados, no cuotas.
type TokenLimit struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	// MaxInstances es el máximo de instancias despiertas atribuibles al tenant.
	// 0 = sin límite.
	MaxInstances int `json:"max_instances,omitempty"`
	// MaxInflight es el máximo de peticiones en vuelo simultáneas del tenant.
	// 0 = sin límite.
	MaxInflight int `json:"max_inflight,omitempty"`
}

// Path devuelve la ruta del fichero de configuración.
//
// Siempre ~/.config/kling, también en macOS. os.UserConfigDir() devolvería ahí
// "~/Library/Application Support", que es lo correcto para apps de escritorio
// pero sorprendente para una herramienta de terminal: nadie va a buscar la
// configuración de un CLI dentro de Library.
func Path() string {
	if p := os.Getenv("KLING_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".kling", "config.json")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "kling", "config.json")
}

// Load lee la configuración. Un fichero ausente no es un error: se devuelve una
// configuración vacía y utilizable.
func Load() (*Config, error) {
	p := Path()
	c := &Config{path: p, Contexts: map[string]*Context{}}

	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	c.path = p
	if c.Contexts == nil {
		c.Contexts = map[string]*Context{}
	}
	liftLegacyMemory(c, b)
	return c, nil
}

// legacyMemory es la sección "memory" de v0.5 y anteriores. Era de kindling-mcp
// (la memoria de uso del gateway): Load la eleva una vez a extensions.mcp, y como
// Config ya no la tiene, el siguiente Save no la vuelve a escribir.
type legacyMemory struct {
	Enabled bool   `json:"enabled"`
	Service string `json:"service,omitempty"`
}

func liftLegacyMemory(c *Config, raw []byte) {
	var old struct {
		Memory *legacyMemory `json:"memory"`
	}
	if json.Unmarshal(raw, &old) != nil || old.Memory == nil {
		return
	}
	if _, ok := c.Extensions["mcp"]["memory.enabled"]; ok {
		return // ya elevada: manda lo nuevo
	}
	if !old.Memory.Enabled && old.Memory.Service == "" {
		return
	}
	_ = c.SetExtension("mcp", "memory.enabled", "bool", strconv.FormatBool(old.Memory.Enabled))
	if old.Memory.Service != "" {
		_ = c.SetExtension("mcp", "memory.service", "string", old.Memory.Service)
	}
}

// Save escribe la configuración de forma atómica.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Durable: aqui vive el endpoint del daemon y el token del gateway, que los
	// pone una persona. Perderlos en un corte no se arregla solo — hay que
	// volver a configurarlos a mano, y el sintoma es un 401 sin explicacion.
	if err := durable.Escribir(c.path, b, 0o600); err != nil {
		return err
	}
	return nil
}

// Host resuelve a qué daemon hablar, por orden de precedencia:
//
//	-H  >  $KLING_HOST  >  contexto activo  >  socket local
//
// El flag gana siempre para que una invocación puntual no obligue a cambiar de
// contexto ni a tocar el fichero.
func (c *Config) Host(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("KLING_HOST"); v != "" {
		return v
	}
	if ctx, ok := c.Contexts[c.CurrentContext]; ok && ctx.Host != "" {
		return ctx.Host
	}
	return ""
}

// ContextNames devuelve los contextos ordenados.
func (c *Config) ContextNames() []string {
	names := make([]string, 0, len(c.Contexts))
	for n := range c.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Set asigna un valor por su ruta con puntos: "defaults.image", "gateway.listen".
func (c *Config) Set(key, value string) error {
	section, field, ok := strings.Cut(key, ".")
	if !ok {
		return fmt.Errorf("invalid key %q: use section.field (e.g. defaults.image)", key)
	}
	atoi := func() (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("%s expects a number, not %q", key, value)
		}
		return n, nil
	}

	switch section {
	case "defaults":
		switch field {
		case "image":
			c.Defaults.Image = value
		case "egress":
			if value != "none" && value != "internet" {
				return fmt.Errorf("defaults.egress only accepts none or internet")
			}
			c.Defaults.Egress = value
		case "mem_mib":
			n, err := atoi()
			if err != nil {
				return err
			}
			c.Defaults.MemMiB = n
		case "vcpus":
			n, err := atoi()
			if err != nil {
				return err
			}
			c.Defaults.VCPUs = n
		case "cpu_pct":
			n, err := atoi()
			if err != nil {
				return err
			}
			c.Defaults.CPUPct = n
		case "ttl_seconds":
			n, err := atoi()
			if err != nil {
				return err
			}
			c.Defaults.TTL = n
		default:
			return fmt.Errorf("unknown field defaults.%s", field)
		}
	case "gateway":
		switch field {
		case "listen":
			c.Gateway.Listen = value
		case "idle":
			c.Gateway.Idle = value
		case "url":
			c.Gateway.URL = value
		case "token":
			c.Gateway.Token = value
		default:
			return fmt.Errorf("unknown field gateway.%s", field)
		}
	default:
		return fmt.Errorf("unknown section %q: use defaults or gateway, or <extension>.<key> for what an extension declares (kling plugins)", section)
	}
	return nil
}

// Extension decodifica en out la clave key de la extensión name. false si no
// está puesta.
func (c *Config) Extension(name, key string, out any) (bool, error) {
	raw, ok := c.Extensions[name][key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, out)
}

// SetExtension guarda value en la clave key de la extensión name, convertido al
// tipo que la extensión declaró: string, secret, bool o int.
func (c *Config) SetExtension(name, key, typ, value string) error {
	var v any
	switch typ {
	case "string", "secret":
		v = value
	case "bool":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "si", "sí", "on":
			v = true
		case "false", "0", "no", "off":
			v = false
		default:
			return fmt.Errorf("%s.%s expects true or false, not %q", name, key, value)
		}
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s.%s expects a number, not %q", name, key, value)
		}
		v = n
	default:
		return fmt.Errorf("%s.%s has an unknown type %q", name, key, typ)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if c.Extensions == nil {
		c.Extensions = map[string]map[string]json.RawMessage{}
	}
	if c.Extensions[name] == nil {
		c.Extensions[name] = map[string]json.RawMessage{}
	}
	c.Extensions[name][key] = b
	return nil
}

// ExtensionValue devuelve el valor de una clave de extensión para mostrarlo;
// los de tipo secret, enmascarados.
func (c *Config) ExtensionValue(name, key, typ string) string {
	raw, ok := c.Extensions[name][key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if typ == "secret" {
			return mask(s)
		}
		return s
	}
	return string(raw)
}

// Keys lista las claves configurables con su valor actual.
func (c *Config) Keys() [][2]string {
	return [][2]string{
		{"defaults.image", c.Defaults.Image},
		{"defaults.egress", c.Defaults.Egress},
		{"defaults.vcpus", itoa(c.Defaults.VCPUs)},
		{"defaults.mem_mib", itoa(c.Defaults.MemMiB)},
		{"defaults.cpu_pct", itoa(c.Defaults.CPUPct)},
		{"defaults.ttl_seconds", itoa(c.Defaults.TTL)},
		{"gateway.listen", c.Gateway.Listen},
		{"gateway.idle", c.Gateway.Idle},
		{"gateway.url", c.Gateway.URL},
		{"gateway.token", mask(c.Gateway.Token)},
	}
}

// mask oculta un secreto dejando lo justo para reconocerlo.
//
// `config show` se teclea en terminales que se comparten por pantalla, así que
// el valor entero no puede salir por ahí. Para copiarlo está la salida de
// `kling gateway` al generarlo, y en último caso el propio fichero, que es del
// usuario y tiene modo 0600.
func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "********"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// Or devuelve el primer valor no vacío. Sirve para encadenar
// flag > configuración > valor incorporado.
func Or[T comparable](vals ...T) T {
	var zero T
	for _, v := range vals {
		if v != zero {
			return v
		}
	}
	return zero
}
