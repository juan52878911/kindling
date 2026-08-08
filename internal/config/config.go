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
	return c, nil
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
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
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
		return fmt.Errorf("clave inválida %q: usa sección.campo (p. ej. defaults.image)", key)
	}
	atoi := func() (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("%s espera un número, no %q", key, value)
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
				return fmt.Errorf("defaults.egress solo admite none o internet")
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
			return fmt.Errorf("campo desconocido defaults.%s", field)
		}
	case "gateway":
		switch field {
		case "listen":
			c.Gateway.Listen = value
		case "idle":
			c.Gateway.Idle = value
		case "url":
			c.Gateway.URL = value
		default:
			return fmt.Errorf("campo desconocido gateway.%s", field)
		}
	default:
		return fmt.Errorf("sección desconocida %q: usa defaults o gateway", section)
	}
	return nil
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
	}
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
