package imagen

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/internal/ext4"
)

// Recorte por familia (kling image import -slim): las imágenes de Docker
// arrastran lo que el servicio no usa (LLVM para el JIT de Postgres, sus
// bitcodes, cabeceras, versiones viejas de extensiones). Las reglas son datos
// (slim.json), no código por familia: cada una dice cómo se reconoce la
// familia en el árbol aplanado y qué se quita. Es opt-in y se aplica antes de
// escribir el ext4; la receta lleva Slim, así que otra importación no la
// confunde con ésta.

//go:embed slim.json
var slimJSON []byte

// ReglasSlim es el contenido de slim.json.
type ReglasSlim struct {
	Familias []FamiliaSlim `json:"familias"`
}

// FamiliaSlim son las reglas de una familia de imágenes. Varias familias
// pueden casar con la misma imagen (timescaledb es postgres además).
type FamiliaSlim struct {
	Nombre string `json:"nombre"`
	// Detecta son globs de rutas (sin / inicial): la familia aplica si
	// existe alguna.
	Detecta []string `json:"detecta"`
	// Quita son globs de rutas que se borran (con lo de debajo).
	Quita []string `json:"quita,omitempty"`
	// Queda vacía un directorio salvo unas entradas.
	Queda []QuedaSlim `json:"queda,omitempty"`
	// UltimaVersion deja, de los ficheros de un directorio que casan, solo
	// la versión más alta de cada grupo.
	UltimaVersion []VersionSlim `json:"ultimaVersion,omitempty"`
	// AñadeArgs añade argumentos al comando del servicio si lo lanza
	// SiEmpieza (jit=off: sin llvmjit.so, Postgres con jit=on lo intentaría
	// en cada consulta cara).
	AñadeArgs *ArgsSlim `json:"añadeArgs,omitempty"`
}

// QuedaSlim borra las entradas de Dir menos las de Salvo y las que pida la
// variable de entorno SalvoEntorno de la imagen (LANG=en_US.utf8 conserva
// en_US.utf8, en_US.UTF-8 y en_US: el entrypoint de la imagen la usa).
type QuedaSlim struct {
	Dir          string   `json:"dir"`
	Salvo        []string `json:"salvo,omitempty"`
	SalvoEntorno string   `json:"salvoEntorno,omitempty"`
}

// VersionSlim: Patron tiene dos grupos, el nombre y la versión (1.2.3).
type VersionSlim struct {
	Dir    string `json:"dir"`
	Patron string `json:"patron"`
}

// ArgsSlim ver FamiliaSlim.AñadeArgs.
type ArgsSlim struct {
	SiEmpieza string   `json:"siEmpieza"`
	Args      []string `json:"args"`
}

// CargarReglasSlim lee las reglas embebidas.
func CargarReglasSlim() (ReglasSlim, error) { return ParseReglasSlim(slimJSON) }

// ParseReglasSlim valida y lee unas reglas.
func ParseReglasSlim(b []byte) (ReglasSlim, error) {
	var r ReglasSlim
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("slim rules: %w", err)
	}
	for _, f := range r.Familias {
		if f.Nombre == "" || len(f.Detecta) == 0 {
			return r, fmt.Errorf("slim rules: a family needs nombre and detecta")
		}
		for _, g := range slices.Concat(f.Detecta, f.Quita) {
			if _, err := casaRuta(g, "x"); err != nil {
				return r, fmt.Errorf("slim rules: %s: %w", f.Nombre, err)
			}
		}
		for _, v := range f.UltimaVersion {
			re, err := regexp.Compile(v.Patron)
			if err != nil || re.NumSubexp() != 2 {
				return r, fmt.Errorf("slim rules: %s: patron %q needs two groups (name, version)", f.Nombre, v.Patron)
			}
		}
		if f.AñadeArgs != nil && (f.AñadeArgs.SiEmpieza == "" || len(f.AñadeArgs.Args) == 0) {
			return r, fmt.Errorf("slim rules: %s: añadeArgs needs siEmpieza and args", f.Nombre)
		}
	}
	return r, nil
}

// casaRuta compara una ruta con un glob por segmentos (path.Match en cada
// uno); "**" casa cualquier número de segmentos.
func casaRuta(glob, p string) (bool, error) {
	return casaSegs(strings.Split(glob, "/"), strings.Split(p, "/"))
}

func casaSegs(g, p []string) (bool, error) {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if ok, err := casaSegs(g[1:], p[i:]); ok || err != nil {
					return ok, err
				}
			}
			return false, nil
		}
		if len(p) == 0 {
			return false, nil
		}
		ok, err := path.Match(g[0], p[0])
		if err != nil || !ok {
			return false, err
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0, nil
}

// InformeSlim dice qué quitó (o quitaría) el recorte.
type InformeSlim struct {
	Familias []string
	Ficheros int
	Bytes    int64
	// Args son los argumentos que hay que añadir al comando del servicio
	// (ArgsSlimSobre).
	Args []ArgsSlim
	// Rutas son las raíces de lo quitado, sin / inicial.
	Rutas []string
}

// Aplica quita del árbol lo que dicen las reglas de las familias que casan.
// env es el entorno de la imagen (KEY=VALUE). Con dry no toca el árbol: solo
// cuenta, para `kling save -audit` y los tests.
func (r ReglasSlim) Aplica(tree *ext4.Node, env []string, dry bool) (InformeSlim, error) {
	var inf InformeSlim
	for _, f := range r.Familias {
		if !detecta(tree, f.Detecta) {
			continue
		}
		inf.Familias = append(inf.Familias, f.Nombre)
		borrar := map[string]bool{}
		if len(f.Quita) > 0 {
			_ = tree.Walk(func(p string, n *ext4.Node) error {
				p = strings.TrimPrefix(p, "/")
				if p == "" {
					return nil
				}
				for _, g := range f.Quita {
					if ok, _ := casaRuta(g, p); ok {
						borrar[p] = true
						break
					}
				}
				return nil
			})
		}
		for _, q := range f.Queda {
			d := tree.Lookup(q.Dir)
			if d == nil || !d.IsDir() {
				continue
			}
			salvo := slices.Clone(q.Salvo)
			if q.SalvoEntorno != "" {
				salvo = append(salvo, localesDe(valorEnv(env, q.SalvoEntorno))...)
			}
			for _, name := range d.Children() {
				if !slices.Contains(salvo, name) {
					borrar[path.Join(q.Dir, name)] = true
				}
			}
		}
		for _, v := range f.UltimaVersion {
			re := regexp.MustCompile(v.Patron)
			d := tree.Lookup(v.Dir)
			if d == nil || !d.IsDir() {
				continue
			}
			mejor := map[string]string{}
			for _, name := range d.Children() {
				if m := re.FindStringSubmatch(name); m != nil {
					if cur, ok := mejor[m[1]]; !ok || cmpVersion(m[2], re.FindStringSubmatch(cur)[2]) > 0 {
						mejor[m[1]] = name
					}
				}
			}
			for _, name := range d.Children() {
				if m := re.FindStringSubmatch(name); m != nil && mejor[m[1]] != name {
					borrar[path.Join(v.Dir, name)] = true
				}
			}
		}
		if f.AñadeArgs != nil {
			inf.Args = append(inf.Args, *f.AñadeArgs)
		}
		for _, p := range slices.Sorted(maps.Keys(borrar)) {
			n := tree.Lookup(p)
			if n == nil {
				continue // ya se fue con un padre
			}
			_ = n.Walk(func(_ string, c *ext4.Node) error {
				if c.IsReg() {
					inf.Ficheros++
					inf.Bytes += c.Size
				}
				return nil
			})
			inf.Rutas = append(inf.Rutas, p)
			if !dry {
				tree.Remove(p)
			}
		}
	}
	return inf, nil
}

func detecta(tree *ext4.Node, globs []string) bool {
	found := false
	_ = tree.Walk(func(p string, n *ext4.Node) error {
		if found {
			return nil
		}
		for _, g := range globs {
			if ok, _ := casaRuta(g, strings.TrimPrefix(p, "/")); ok {
				found = true
			}
		}
		return nil
	})
	return found
}

func valorEnv(env []string, k string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v
		}
	}
	return ""
}

// localesDe da los nombres que un LANG necesita en /usr/lib/locale:
// en_US.utf8 → en_US.utf8, en_US, en_US.UTF-8.
func localesDe(lang string) []string {
	if lang == "" {
		return nil
	}
	base, cod, _ := strings.Cut(lang, ".")
	out := []string{lang, base}
	if cod != "" {
		out = append(out, base+"."+strings.ToLower(strings.ReplaceAll(cod, "-", "")), base+".UTF-8")
	}
	return out
}

// cmpVersion compara "2.9.1" con "2.10.0" numéricamente.
func cmpVersion(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// ArgsSlimSobre añade a argv, a continuación del primer argumento que sea
// SiEmpieza (por nombre base), los argumentos de las reglas: `postgres` pasa a
// `postgres -c jit=off`. Si ya están, no repite.
func ArgsSlimSobre(argv []string, reglas []ArgsSlim) []string {
	for _, r := range reglas {
		for i, a := range argv {
			if path.Base(a) != r.SiEmpieza {
				continue
			}
			if slices.Contains(argv, r.Args[len(r.Args)-1]) {
				break
			}
			out := slices.Clone(argv[:i+1])
			out = append(out, r.Args...)
			argv = append(out, argv[i+1:]...)
			break
		}
	}
	return argv
}
