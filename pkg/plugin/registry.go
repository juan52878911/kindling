package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestTimeout es lo que se espera a que una extensión imprima su manifiesto.
// Una extensión colgada no puede dejar a `kling help` esperando.
var ManifestTimeout = 2 * time.Second

// HookTimeout es lo que se espera a un gancho. Un gancho que falla o tarda
// produce una línea de aviso, nunca un error de `kling`.
var HookTimeout = 5 * time.Second

// waitDelay es lo que se espera a que se cierre la salida de una extensión ya
// muerta por plazo, antes de dejar de leerla.
const waitDelay = 200 * time.Millisecond

// notPlugins son ejecutables kling-* que acompañan a kindling y no son
// extensiones: nunca se les pide un manifiesto. Están aquí los que publica la
// release del núcleo y de sus extensiones; los de una extensión de terceros
// los declara su manifiesto en Companions y los lee companionsIn.
var notPlugins = map[string]bool{
	"kling-bridge":       true,
	"kling-bridge-local": true,
	"kling-guest":        true,
	"kling-vz":           true, // VMM nativo de macOS, junto a kling
	"kling-chispa":       true, // servidor de clasificadores dentro de la imagen
	"kling-daemon":       true,
	"kling-builder":      true,
}

// IsCompanion dice si el ejecutable name (kling-<algo>) es un compañero
// conocido y no una extensión.
func IsCompanion(name string) bool { return notPlugins[name] }

// Builtin es una extensión que vive dentro del binario de kling. Sirve para
// que lo que todavía no se ha mudado a su propio binario pase ya por el mismo
// camino —ayuda, completado, ganchos— que una extensión externa.
type Builtin struct {
	Manifest Manifest
	Commands map[string]func(args []string) error
	Hooks    map[string]func(args []string, w io.Writer) error
}

// Plugin es una extensión encontrada.
type Plugin struct {
	Name     string
	Path     string // vacío para las incorporadas
	Manifest *Manifest
	Builtin  *Builtin
	// Err es por qué no se puede usar (manifiesto roto, versión, ...). Una
	// extensión con Err se lista pero no se ejecuta.
	Err error
	// Shadowed son las palabras de primer nivel que reclama (su nombre, si
	// tiene comandos bajo él, y sus comandos promovidos) y que ya tiene el
	// núcleo u otra extensión anterior: no se le enrutan.
	Shadowed []string
	// Disabled es que la apagó el usuario (plugins.disabled). Va con Err, para
	// que todo lo que ya salta las extensiones con error la salte también.
	Disabled bool
}

// Registry es el conjunto de extensiones que ve este kling.
type Registry struct {
	Plugins []*Plugin
	// owner es la palabra de primer nivel -> extensión que la sirve. La
	// palabra es el nombre de la extensión (espacio de sus comandos) o un
	// comando promovido.
	owner map[string]*Plugin
}

// Options configura el descubrimiento.
type Options struct {
	// Core son los comandos del núcleo: ganan siempre.
	Core []string
	// Version es la del núcleo, para comprobar MinKling.
	Version string
	// Builtins son las extensiones incorporadas; van antes que las externas.
	Builtins []*Builtin
	// Path son los directorios donde buscar kling-*; nil = SearchPath().
	Path []string
	// Disabled son las extensiones apagadas por el usuario: se listan, pero
	// no reciben comandos ni ganchos. A una externa apagada ni siquiera se le
	// pide el manifiesto: apagarla es no ejecutar nada suyo.
	Disabled []string
}

// DisabledError es el Err de una extensión apagada.
type DisabledError struct{ Name string }

func (e *DisabledError) Error() string {
	return fmt.Sprintf("disabled (kling plugins enable %s)", e.Name)
}

// SearchPath es dónde se buscan extensiones, en orden: $KLING_PLUGIN_PATH, el
// directorio lib/kindling/plugins junto al binario de kling, el de datos del
// usuario y por último el PATH.
func SearchPath() []string {
	var dirs []string
	if v := os.Getenv("KLING_PLUGIN_PATH"); v != "" {
		dirs = append(dirs, filepath.SplitList(v)...)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "..", "lib", "kindling", "plugins"))
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		dirs = append(dirs, filepath.Join(d, "kling", "plugins"))
	} else if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(h, ".local", "share", "kling", "plugins"))
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	return dirs
}

// Discover encuentra las extensiones y resuelve qué comando sirve cada una.
func Discover(ctx context.Context, o Options) *Registry {
	r := &Registry{owner: map[string]*Plugin{}}
	core := map[string]bool{}
	for _, c := range o.Core {
		core[c] = true
	}
	claim := func(p *Plugin) {
		if p.Err != nil {
			return
		}
		var words []string
		if len(p.Manifest.Namespaced()) > 0 {
			words = append(words, p.Manifest.Name)
		}
		for _, c := range p.Manifest.Promoted() {
			words = append(words, c.Name)
		}
		for _, w := range words {
			if core[w] || r.owner[w] != nil {
				p.Shadowed = append(p.Shadowed, w)
				continue
			}
			r.owner[w] = p
		}
	}

	off := map[string]bool{}
	for _, d := range o.Disabled {
		off[d] = true
	}

	seen := map[string]bool{}
	for _, b := range o.Builtins {
		m := b.Manifest
		p := &Plugin{Name: m.Name, Manifest: &m, Builtin: b}
		p.Err = m.Validate()
		if off[m.Name] {
			p.Err, p.Disabled = &DisabledError{m.Name}, true
		}
		seen[m.Name] = true
		r.Plugins = append(r.Plugins, p)
		claim(p)
	}

	path := o.Path
	if path == nil {
		path = SearchPath()
	}
	for _, dir := range path {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		companions := companionsIn(dir)
		for _, e := range entries {
			name, ok := strings.CutPrefix(e.Name(), "kling-")
			if !ok || notPlugins[e.Name()] || companions[e.Name()] || strings.ContainsAny(name, ".") || seen[name] {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if !isExecutable(full) {
				continue
			}
			seen[name] = true
			p := &Plugin{Name: name, Path: full}
			if off[name] {
				p.Err, p.Disabled = &DisabledError{name}, true
				r.Plugins = append(r.Plugins, p)
				continue
			}
			p.Manifest, p.Err = loadManifest(ctx, full)
			if p.Err == nil && p.Manifest.Name != name {
				p.Err = fmt.Errorf("its manifest says it is %q, but the binary is kling-%s", p.Manifest.Name, name)
			}
			if p.Err == nil && !VersionAtLeast(o.Version, p.Manifest.MinKling) {
				p.Err = fmt.Errorf("needs kling %s or newer (this is %s)", p.Manifest.MinKling, o.Version)
			}
			r.Plugins = append(r.Plugins, p)
			claim(p)
		}
	}
	return r
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

func loadManifest(ctx context.Context, path string) (*Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, ManifestTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "--kling-manifest")
	cmd.Stdout = &out
	cmd.Env = Env("")
	// Sin WaitDelay, matar la extensión al vencer el plazo no basta: si dejó un
	// nieto con la salida abierta (un script que llama a sleep), Run espera a que
	// ese nieto la cierre y el plazo no sirve de nada.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("--kling-manifest took more than %s", ManifestTimeout)
		}
		return nil, fmt.Errorf("--kling-manifest failed: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("--kling-manifest did not print a valid manifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Lookup devuelve la extensión que sirve la palabra de primer nivel word (el
// nombre de una extensión con comandos, o un comando promovido), o nil.
func (r *Registry) Lookup(word string) *Plugin {
	return r.owner[word]
}

// Resolve traduce lo que se tecleó tras `kling` a lo que recibe la extensión.
// Para `kling mcp add x` devuelve la extensión mcp y ["add", "x"]; para
// `kling connect -all`, la extensión mcp y ["connect", "-all"]. Sin extensión
// que sirva word devuelve nil.
func (r *Registry) Resolve(word string, rest []string) (*Plugin, []string) {
	p := r.owner[word]
	if p == nil {
		return nil, nil
	}
	// Un comando promovido que se llama como la extensión (el "hello" de
	// kling-hello) gana sobre el espacio de nombres: es lo que siempre fue.
	if p.Manifest.Name == word && p.Manifest.Command(word) == nil {
		return p, rest
	}
	return p, append([]string{word}, rest...)
}

// IsNamespace dice si word es el nombre de una extensión utilizable con
// comandos bajo él.
func (r *Registry) IsNamespace(word string) bool {
	p := r.owner[word]
	return p != nil && p.Manifest.Name == word && p.Manifest.Command(word) == nil && len(p.Manifest.Namespaced()) > 0
}

// DisabledFor devuelve la extensión apagada que serviría cmd, o nil, para que
// el núcleo diga "está desactivada" en vez de "comando desconocido". De una
// externa apagada no se conoce el manifiesto: se supone que su comando es su
// nombre, que es lo habitual.
func (r *Registry) DisabledFor(cmd string) *Plugin {
	for _, p := range r.Plugins {
		if !p.Disabled {
			continue
		}
		if p.Name == cmd || (p.Manifest != nil && p.Manifest.Command(cmd) != nil) {
			return p
		}
	}
	return nil
}

// companionsIn son los ejecutables que las extensiones instaladas en dir
// declararon como compañeros (leídos de sus kling-<n>.json): no son
// extensiones y no se les pide manifiesto.
func companionsIn(dir string) map[string]bool {
	out := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(dir, "kling-*.json"))
	for _, m := range matches {
		if s, err := readSidecar(m); err == nil {
			for _, c := range s.Companions {
				out[c] = true
			}
		}
	}
	return out
}

// CompanionsIn son, ordenados, los compañeros que declaran las extensiones
// instaladas en dir.
func CompanionsIn(dir string) []string {
	var out []string
	for c := range companionsIn(dir) {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Commands son los comandos de primer nivel que aportan las extensiones
// utilizables (los promovidos, y uno por cada espacio de nombres, que resume la
// extensión), en orden de grupo y nombre. Es lo que sale en `kling help` y en
// el primer nivel del completado.
func (r *Registry) Commands() []Command {
	var out []Command
	for _, p := range r.Namespaces() {
		m := p.Manifest
		c := Command{Name: m.Name, Group: m.Group(), Summary: m.Summary}
		for _, sc := range m.Namespaced() {
			if !sc.Hidden {
				c.Subcommands = append(c.Subcommands, sc.Name)
			}
			for _, ma := range sc.MachineArgs {
				if ma == "" {
					c.MachineArgs = append(c.MachineArgs, sc.Name)
				}
			}
		}
		out = append(out, c)
	}
	for _, p := range r.Plugins {
		if p.Err != nil {
			continue
		}
		for _, c := range p.Manifest.Promoted() {
			if r.owner[c.Name] == p && !c.Hidden {
				out = append(out, c)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// Namespaces son las extensiones utilizables que tienen comandos bajo su
// nombre, en orden de descubrimiento.
func (r *Registry) Namespaces() []*Plugin {
	var out []*Plugin
	for _, p := range r.Plugins {
		if p.Err == nil && r.owner[p.Manifest.Name] == p && len(p.Manifest.Namespaced()) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// WithHook son las extensiones utilizables que declaran el gancho h.
func (r *Registry) WithHook(h string) []*Plugin {
	var out []*Plugin
	for _, p := range r.Plugins {
		if p.Err == nil && p.Manifest.HasHook(h) {
			out = append(out, p)
		}
	}
	return out
}

// Env es el entorno para una extensión: el del proceso más lo que el núcleo le
// cuenta de sí mismo. configPath vacío no añade KLING_CONFIG.
func Env(configPath string) []string {
	env := os.Environ()
	set := func(k, v string) {
		for i, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				env[i] = k + "=" + v
				return
			}
		}
		env = append(env, k+"="+v)
	}
	set("KLING_PLUGIN_API", APIVersion)
	if exe, err := os.Executable(); err == nil {
		set("KLING_BIN", exe)
	}
	if configPath != "" {
		set("KLING_CONFIG", configPath)
	}
	if v := coreVersion; v != "" {
		set("KLING_VERSION", v)
	}
	return env
}

// coreVersion la fija el núcleo con SetCoreVersion, para KLING_VERSION.
var coreVersion string

// SetCoreVersion registra la versión del núcleo que se pasa a las extensiones.
func SetCoreVersion(v string) { coreVersion = v }
