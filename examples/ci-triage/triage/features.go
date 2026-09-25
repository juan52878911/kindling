package triage

import (
	"regexp"
	"strings"
)

// Lo que ve Chispa de cada línea. Un modelo lineal no ve el contexto por sí
// solo, y en un log el contexto lo es todo: la línea «at Foo.bar(Foo.java:12)»
// explica el fallo si va detrás de «AssertionError» y es ruido si va detrás de
// un aviso de deprecación. Por eso cada línea lleva, además de su texto, las
// marcas de sus vecinas, su distancia al error más cercano y al final del log,
// y si el runner la pintó en rojo. Todo son campos baratos (sin expresiones
// regulares salvo dos), para que extraer no cueste más que decidir.

// MaxLineText es lo que se manda de cada línea a Chispa: más allá de ~300
// caracteres una línea es un volcado (JSON, una lista de ficheros) y sus
// palabras solo añaden ruido.
const MaxLineText = 320

// marker es una familia de palabras que delata el papel de una línea.
type marker struct {
	name string
	subs []string
}

// markers se buscan en minúsculas con strings.Contains: ~1 µs por línea.
var markers = []marker{
	{"error", []string{"error", "err!", "erro:"}},
	{"fail", []string{"fail", "✗", "✖", "❌", "[-]"}},
	{"exception", []string{"exception", "traceback", "panic", "fatal", "crash", "segmentation", "abort"}},
	{"assert", []string{"assert", "expected", "actual", "but was", "but got", "to equal", "should"}},
	{"timeout", []string{"timeout", "timed out", "time limit", "deadline"}},
	{"resource", []string{"killed", "out of memory", "no space left", "oom"}},
	{"missing", []string{"not found", "no such file", "cannot find", "could not find", "unable to", "denied", "not recognized", "undefined"}},
	{"exit", []string{"exited with", "exit code", "exit status", "returned non-zero", "process completed with"}},
	{"warn", []string{"warn", "deprecat", "notice"}},
	{"ok", []string{" ok", "pass", "success", "✓", "✔", "done", "finished", "installed", "up to date", "complete"}},
	{"progress", []string{"downloading", "fetching", "installing", "resolving", "receiving", "extracting", "unpacking", "setting up", "collecting", "building wheel", "compiling"}},
	{"summary", []string{"tests:", "failures", "errors:", "passed", "failed", "examples,", "specs,", "tests run"}},
}

var (
	// frameRe reconoce una línea de pila de casi cualquier lenguaje.
	frameRe = regexp.MustCompile(`^\s*(at |File "|# \./|from |\d+: 0x|#\d+ 0x|in \S+\.\w+:\d+|\S+\.\w{1,6}:\d+(:\d+)?:?\s*(in|$))`)
	// exitRe: la línea con la que el runner da por fallado un paso.
	exitRe = regexp.MustCompile(`(?i)the command ".*" (exited|failed)|process completed with exit code [1-9]|exited with (code )?[1-9]|error: process completed|##\[error\]`)
)

// lineInfo es lo que se calcula una vez por línea antes de montar las
// características (las de las vecinas se reutilizan).
type lineInfo struct {
	lower   string
	marks   uint32
	blank   bool
	exit    bool
	strong  bool // error, fallo o excepción: el ancla de la distancia
	indent  bool
	isFrame bool
}

func analyze(l Line) lineInfo {
	t := l.Text
	inf := lineInfo{lower: strings.ToLower(t)}
	if strings.TrimSpace(t) == "" {
		inf.blank = true
		return inf
	}
	for k, m := range markers {
		for _, s := range m.subs {
			if strings.Contains(inf.lower, s) {
				inf.marks |= 1 << k
				break
			}
		}
	}
	inf.exit = l.Marked || exitRe.MatchString(t)
	inf.strong = l.Marked || inf.marks&(1<<0|1<<1|1<<2) != 0
	inf.indent = t[0] == ' ' || t[0] == '\t'
	inf.isFrame = frameRe.MatchString(t)
	return inf
}

func markNames(m uint32, prefix string, dst []string) []string {
	for k := range markers {
		if m&(1<<k) != 0 {
			dst = append(dst, prefix+markers[k].name)
		}
	}
	return dst
}

// LineInput es lo que se le pasa a Chispa por una línea.
type LineInput struct {
	Index  int
	Text   string
	Fields map[string]any
}

// Features devuelve la entrada de Chispa de cada línea no vacía del log (las
// vacías no se clasifican: nunca explican nada y son el 10-20 % de un log).
func Features(lg *Log) []LineInput {
	n := len(lg.Lines)
	infos := make([]lineInfo, n)
	counts := make(map[string]int, n)
	for i, l := range lg.Lines {
		infos[i] = analyze(l)
		if !infos[i].blank {
			counts[l.Text]++
		}
	}
	// Distancia al error fuerte más cercano (hacia atrás y hacia delante) y a
	// la siguiente línea de «paso fallado»: dos barridos lineales.
	prevStrong, nextStrong, nextExit := make([]int, n), make([]int, n), make([]int, n)
	last := -1 << 30
	for i := range n {
		if infos[i].strong {
			last = i
		}
		prevStrong[i] = i - last
	}
	nxt, nx := 1<<30, 1<<30
	for i := n - 1; i >= 0; i-- {
		if infos[i].strong {
			nxt = i
		}
		if infos[i].exit {
			nx = i
		}
		nextStrong[i] = nxt - i
		nextExit[i] = nx - i
	}

	out := make([]LineInput, 0, n)
	for i, l := range lg.Lines {
		inf := infos[i]
		if inf.blank {
			continue
		}
		f := map[string]any{
			"end": n - 1 - i,
			"pos": decile(i, n),
			"red": l.Red,
		}
		if l.Yellow {
			f["yellow"] = true
		}
		if l.Marked {
			f["marked"] = true
		}
		if inf.indent {
			f["indent"] = true
		}
		if inf.isFrame {
			f["frame"] = true
		}
		if inf.exit {
			f["exit"] = true
		}
		if s := sectionKey(l.Section); s != "" {
			f["sec"] = s
		}
		if d := min(prevStrong[i], nextStrong[i]); d < 1<<20 {
			f["dist"] = d
		}
		if d := nextExit[i]; d < 1<<20 {
			f["toexit"] = d
		}
		if c := counts[l.Text]; c > 1 {
			f["dup"] = c
		}
		f["len"] = len(l.Text)
		// Una línea sin palabras («=====», «^~~~», «})») no explica nada por sí
		// misma: sin estos dos campos el modelo solo ve sus vecinas y la
		// puntúa como ellas.
		w, sym := wordStats(l.Text)
		f["words"] = w
		if sym {
			f["sym"] = true
		}
		var mk []string
		mk = markNames(inf.marks, "", mk)
		var prev, next uint32
		for j := max(0, i-3); j < i; j++ {
			prev |= infos[j].marks
		}
		for j := i + 1; j <= min(n-1, i+3); j++ {
			next |= infos[j].marks
		}
		if len(mk) > 0 {
			f["mk"] = mk
		}
		if prev != 0 {
			f["prev"] = markNames(prev, "", nil)
		}
		if next != 0 {
			f["next"] = markNames(next, "", nil)
		}
		out = append(out, LineInput{Index: i, Text: truncUTF8(l.Text, MaxLineText), Fields: f})
	}
	return out
}

// wordStats cuenta las palabras (dos letras o más seguidas, como mucho 64) y
// dice si la línea es sobre todo símbolos.
func wordStats(s string) (int, bool) {
	words, run, alnum, other := 0, 0, 0, 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r > 127:
			alnum++
			run++
			continue
		case r >= '0' && r <= '9':
			alnum++
		case r != ' ' && r != '\t':
			other++
		}
		if run >= 2 {
			words++
		}
		run = 0
	}
	if run >= 2 {
		words++
	}
	return min(words, 64), other > 2*alnum
}

// decile: posición relativa en décimas ("p0".."p9").
func decile(i, n int) string {
	d := 0
	if n > 0 {
		d = i * 10 / n
	}
	return "p" + string(rune('0'+min(d, 9)))
}

// sectionKey reduce el nombre de una sección a algo que se repita entre
// repos: minúsculas, sin números ni rutas, como mucho 40 bytes.
func sectionKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "run ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == ' ', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r > 127:
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
