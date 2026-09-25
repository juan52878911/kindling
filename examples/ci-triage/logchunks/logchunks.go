// Package logchunks lee el conjunto LogChunks (Brandt, Panichella y Beller,
// MSR 2020; CC BY 4.0, Zenodo 10.5281/zenodo.3632351): 797 logs de Travis CI
// de 80 repositorios, cada uno con el trozo que explica el fallo anotado a
// mano. De aquí salen las etiquetas por línea del localizador.
package logchunks

import (
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

// Build es un log anotado.
type Build struct {
	Lang     string // lenguaje principal del repo (la carpeta de LogChunks)
	Repo     string // dueño@nombre
	ID       string // id del build de Travis
	LogPath  string // ruta absoluta del .log
	Chunk    string // el trozo anotado, tal cual viene en el XML
	Keywords []string
}

type xmlExamples struct {
	Examples []struct {
		Log      string `xml:"Log"`
		Keywords string `xml:"Keywords"`
		Category int    `xml:"Category"`
		Chunk    string `xml:"Chunk"`
	} `xml:"Example"`
}

// maxXML: el mayor XML de anotaciones pesa unos 100 KB.
const maxXML = 16 << 20

// Load lee las anotaciones de root (la carpeta LogChunks descomprimida).
func Load(root string) ([]Build, error) {
	files, err := filepath.Glob(filepath.Join(root, "build-failure-reason", "*", "*.xml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no build-failure-reason/*/*.xml (is it the unzipped LogChunks folder?)", root)
	}
	sort.Strings(files)
	var out []Build
	for _, f := range files {
		bs, err := loadXML(root, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, bs...)
	}
	return out, nil
}

func loadXML(root, path string) ([]Build, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var x xmlExamples
	if err := xml.NewDecoder(io.LimitReader(f, maxXML)).Decode(&x); err != nil {
		return nil, err
	}
	lang := filepath.Base(filepath.Dir(path))
	var out []Build
	for _, e := range x.Examples {
		rel := filepath.FromSlash(strings.TrimSpace(e.Log))
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 4 {
			return nil, fmt.Errorf("unexpected log path %q", e.Log)
		}
		var kws []string
		for k := range strings.SplitSeq(e.Keywords, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kws = append(kws, k)
			}
		}
		out = append(out, Build{
			Lang: lang, Repo: parts[1], ID: strings.TrimSuffix(parts[3], ".log"),
			LogPath: filepath.Join(root, "logs", rel), Chunk: e.Chunk, Keywords: kws,
		})
	}
	return out, nil
}

// pseudoANSI: el XML no puede llevar ESC, así que los colores del trozo
// anotado quedan como "[31m" sueltos.
var pseudoANSI = regexp.MustCompile(`\[[0-9;]*[mK]`)

// Norm normaliza una línea para comparar el log con el trozo anotado: sin
// colores, sin '<' ni '>' (el XML de LogChunks perdió los '<' del texto), y
// con los espacios colapsados.
func Norm(s string) string {
	s = pseudoANSI.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == 0x1b {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// Gold marca las líneas del log que forman el trozo anotado. Busca cada
// aparición del trozo: sus líneas no vacías, en orden, cada una contenida en
// una línea del log (el anotador a veces recortó sangrías, prefijos o el
// final). Tolera que el log intercale alguna línea que el trozo no tiene y que
// alguna línea del trozo no aparezca (secuencias de terminal que el XML
// estropeó), siempre que aparezca la primera y al menos el 70 % del resto.
// Devuelve cuántas apariciones encontró; con 0 el log no tiene oro por línea.
func Gold(lines []triage.Line, chunk string) ([]bool, int) {
	var want []string
	for l := range strings.SplitSeq(chunk, "\n") {
		if n := Norm(l); n != "" {
			want = append(want, n)
		}
	}
	gold := make([]bool, len(lines))
	if len(want) == 0 {
		return gold, 0
	}
	norm := make([]string, len(lines))
	for i, l := range lines {
		norm[i] = Norm(l.Text)
	}
	found := 0
	for i := 0; i < len(lines); i++ {
		if !strings.Contains(norm[i], want[0]) {
			continue
		}
		hit, end := align(norm, i, want)
		if hit == nil {
			continue
		}
		found++
		// Las líneas vacías (o no reconocidas) en medio del trozo también son
		// del trozo.
		for h := hit[0]; h <= hit[len(hit)-1]; h++ {
			gold[h] = true
		}
		i = end - 1
	}
	return gold, found
}

// align intenta casar want a partir de la línea i del log. Devuelve las
// líneas casadas y dónde acabó, o nil si no llega al 70 %.
func align(norm []string, i int, want []string) ([]int, int) {
	const lookahead = 3
	hit := []int{i}
	j, miss := i+1, 0
	for k := 1; k < len(want); k++ {
		found := -1
		for t, seen := j, 0; t < len(norm) && seen <= lookahead; t++ {
			if norm[t] == "" {
				continue
			}
			if strings.Contains(norm[t], want[k]) {
				found = t
				break
			}
			seen++
		}
		if found < 0 {
			miss++
			continue
		}
		hit = append(hit, found)
		j = found + 1
	}
	if len(want) > 1 && float64(miss) > 0.3*float64(len(want)-1) {
		return nil, 0
	}
	return hit, j
}

// Split reparte por repositorio (nunca un repo en dos conjuntos: sus logs se
// parecen demasiado entre sí y el localizador parecería mejor de lo que es).
// El orden de los repos sale de un hash con semilla, estable entre máquinas.
func Split(bs []Build, seed string, validPct, testPct int) (train, valid, test []Build) {
	repos := map[string]bool{}
	for _, b := range bs {
		repos[b.Repo] = true
	}
	names := make([]string, 0, len(repos))
	for r := range repos {
		names = append(names, r)
	}
	key := func(r string) uint64 {
		h := fnv.New64a()
		h.Write([]byte(seed + "\x00" + r))
		return h.Sum64()
	}
	sort.Slice(names, func(i, j int) bool { return key(names[i]) < key(names[j]) })
	nTest := (len(names)*testPct + 50) / 100
	nValid := (len(names)*validPct + 50) / 100
	where := map[string]int{}
	for i, r := range names {
		switch {
		case i < nTest:
			where[r] = 2
		case i < nTest+nValid:
			where[r] = 1
		}
	}
	for _, b := range bs {
		switch where[b.Repo] {
		case 0:
			train = append(train, b)
		case 1:
			valid = append(valid, b)
		default:
			test = append(test, b)
		}
	}
	return train, valid, test
}
