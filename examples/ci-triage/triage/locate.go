package triage

import (
	"regexp"
	"sort"
	"strings"
)

// Chunk es un tramo contiguo de líneas [Start, End] (índices de Log.Lines).
type Chunk struct {
	Start, End int
	Score      float64 // la puntuación de su mejor línea
}

// ChunkOptions acota lo que sale del localizador: lo que ve la capa siguiente
// tiene que caber en ~300-500 tokens, no en 10 000 líneas.
type ChunkOptions struct {
	MaxChunks int     // tramos como mucho (2: el error y, si lo hay, su resumen)
	MaxLines  int     // líneas por tramo
	MaxBytes  int     // bytes de texto entre todos los tramos
	Rel       float64 // una vecina entra si puntúa ≥ Rel × la mejor del tramo
	Second    float64 // un segundo tramo solo si su ancla puntúa ≥ Second × la primera
	Gap       int     // líneas flojas seguidas que se toleran dentro de un tramo
}

// MaxChunkBytes es el texto que ven la categoría y VON: ~500 tokens.
const MaxChunkBytes = 2000

// DefaultChunkOptions se eligieron sobre la validación de LogChunks.
var DefaultChunkOptions = ChunkOptions{MaxChunks: 2, MaxLines: 30, MaxBytes: MaxChunkBytes, Rel: 0.35, Second: 0.8, Gap: 3}

// Locate elige los tramos a partir de la puntuación de cada línea (0 las que
// no se puntuaron). Ancla en la mejor línea, crece hacia los lados mientras
// las vecinas puntúen cerca de ella (tolerando Gap líneas flojas, que en una
// pila o un diff son normales) y repite para un segundo tramo si hay otra
// ancla casi tan buena lejos de la primera.
func Locate(lg *Log, score []float64, o ChunkOptions) []Chunk {
	n := len(score)
	if n == 0 {
		return nil
	}
	order := make([]int, 0, n)
	for i, s := range score {
		if s > 0 {
			order = append(order, i)
		}
	}
	// A igual puntuación, la línea más tardía: el fallo suele estar al final.
	sort.SliceStable(order, func(a, b int) bool {
		if score[order[a]] != score[order[b]] {
			return score[order[a]] > score[order[b]]
		}
		return order[a] > order[b]
	})
	var out []Chunk
	used := make([]bool, n)
	bytes := 0
	for _, a := range order {
		if len(out) == o.MaxChunks || bytes >= o.MaxBytes {
			break
		}
		if used[a] {
			continue
		}
		if len(out) > 0 && score[a] < o.Second*out[0].Score {
			break
		}
		cut := o.Rel * score[a]
		// Primero hacia arriba (el mensaje de error suele ir antes que su pila) y
		// luego hacia abajo, con el mismo tope de líneas para los dos.
		lo, hi := a, a
		for j, gap := a-1, 0; j >= 0 && hi-lo+1 < o.MaxLines && !used[j]; j-- {
			if score[j] >= cut {
				lo, gap = j, 0
			} else if gap++; gap > o.Gap {
				break
			}
		}
		for j, gap := a+1, 0; j < n && hi-lo+1 < o.MaxLines && !used[j]; j++ {
			if score[j] >= cut {
				hi, gap = j, 0
			} else if gap++; gap > o.Gap {
				break
			}
		}
		c := Chunk{Start: lo, End: hi, Score: score[a]}
		for j := lo; j <= hi; j++ {
			used[j] = true
			if lg != nil {
				bytes += len(lg.Lines[j].Text) + 1
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// ChunkText es el texto de los tramos, acotado a maxBytes, con «…» entre
// tramos: es lo único que ven la categoría y VON.
func ChunkText(lg *Log, cs []Chunk, maxBytes int) string {
	var b strings.Builder
	for k, c := range cs {
		if k > 0 {
			b.WriteString("…\n")
		}
		for j := c.Start; j <= c.End && j < len(lg.Lines); j++ {
			t := lg.Lines[j].Text
			if b.Len()+len(t)+1 > maxBytes {
				b.WriteString(truncUTF8(t, max(0, maxBytes-b.Len())))
				return b.String()
			}
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// commandRe saca el comando que falló de la línea con la que Travis cierra un
// paso: The command "npm test" exited with 1.
var commandRe = regexp.MustCompile(`The command "(.{1,200}?)" (?:exited with|failed)`)

// CategoryFields son los campos de la entrada de la categoría: la sección del
// primer tramo y las palabras del comando o paso que falló («npm run lint»,
// «bundle exec rspec», «Backend tests»). Dicen tanto como el propio trozo.
func CategoryFields(lg *Log, cs []Chunk) map[string]any {
	f := map[string]any{}
	if len(cs) == 0 {
		return f
	}
	first := lg.Lines[cs[0].Start]
	if s := sectionKey(first.Section); s != "" {
		f["sec"] = s
	}
	cmd := ""
	// El comando de Travis va en una línea poco después del tramo.
	last := cs[len(cs)-1].End
	for j := last; j < len(lg.Lines) && j < last+400; j++ {
		if m := commandRe.FindStringSubmatch(lg.Lines[j].Text); m != nil {
			cmd = m[1]
			break
		}
	}
	if cmd == "" {
		cmd = first.Section // GitHub Actions: el nombre del paso o del grupo
	}
	if w := cmdWords(cmd); len(w) > 0 {
		f["cmd"] = w
	}
	return f
}

// cmdWords: las palabras de un comando, sin rutas ni opciones con valor, como
// mucho 12 (Chispa toma hasta 16 elementos de una lista).
func cmdWords(cmd string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(cmd), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':' || r > 127)
	}) {
		w = strings.Trim(w, "-:")
		if len(w) < 2 || len(w) > 30 || allDigits(w) {
			continue
		}
		out = append(out, w)
		if len(out) == 12 {
			break
		}
	}
	return out
}
