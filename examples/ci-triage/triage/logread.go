// Package triage es el núcleo del ejemplo de triaje de fallos de CI: lee un
// log (acotado), saca de cada línea lo que Chispa necesita para decidir si
// explica el fallo, elige el trozo que lo explica y habla con el gateway de IA
// de kindling (`kling ai serve`) para las tres capas.
//
// No importa nada de kindling: todo lo que sabe de Chispa y de VON lo pregunta
// por HTTP al gateway, como haría cualquier otro programa.
package triage

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Limits acota la lectura de un log. Un log de CI puede pesar cientos de megas
// (tests que imprimen sin parar, un bucle que no acaba): nada de lo de aquí
// puede depender de que sea pequeño.
type Limits struct {
	// MaxBytes es lo que se guarda del log. Si es más largo se queda la COLA:
	// lo que explica un fallo casi siempre está al final, y el principio
	// (información del worker, instalación) es lo más prescindible.
	MaxBytes int64
	// MaxLines es el tope de líneas (también de la cola).
	MaxLines int
	// MaxLineBytes recorta una línea larguísima (un JSON minificado, una barra
	// de progreso sin saltos) para que no domine ni la memoria ni las
	// características.
	MaxLineBytes int
	// MaxStream es lo que se está dispuesto a leer de un flujo sin fin (stdin)
	// antes de rendirse: aunque solo se guarde la cola, leer tiene un coste.
	MaxStream int64
}

// DefaultLimits: 16 MiB y 100 000 líneas bastan para cualquier log de CI
// razonable (el mayor de LogChunks tiene 32 738 líneas y 2,4 MB).
var DefaultLimits = Limits{MaxBytes: 16 << 20, MaxLines: 100_000, MaxLineBytes: 2048, MaxStream: 1 << 30}

// Line es una línea del log tal como se ve en pantalla, sin secuencias ANSI.
type Line struct {
	Text    string // lo visible, sin ANSI ni marcas de tiempo del runner
	Section string // paso o bloque plegado donde está (travis_fold, ##[group], paso de GitHub)
	Job     string // trabajo de GitHub Actions, si el log lo dice
	Red     bool   // el runner la pintó en rojo: así marcan muchos errores
	Yellow  bool
	Marked  bool // GitHub Actions la marcó como ##[error]
}

// Log es un log leído.
type Log struct {
	Lines     []Line
	Format    string // travis | github | plain
	Truncated bool   // se quedó solo la cola
	Skipped   int64  // bytes que se saltaron del principio
}

// ReadFile lee un fichero de log. Si pesa más de MaxBytes salta directamente
// a la cola (sin leer el principio).
func ReadFile(path string, lim Limits) (*Log, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var skipped int64
	if st.Mode().IsRegular() && st.Size() > lim.MaxBytes {
		skipped = st.Size() - lim.MaxBytes
		if _, err := f.Seek(skipped, io.SeekStart); err != nil {
			return nil, err
		}
	}
	lg, err := read(f, lim, skipped > 0)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		lg.Truncated, lg.Skipped = true, lg.Skipped+skipped
	}
	return lg, nil
}

// Read lee un log de un flujo guardando como mucho MaxBytes de la cola.
func Read(r io.Reader, lim Limits) (*Log, error) { return read(r, lim, false) }

func read(r io.Reader, lim Limits, midLine bool) (*Log, error) {
	if lim.MaxBytes <= 0 || lim.MaxLines <= 0 || lim.MaxLineBytes <= 0 {
		return nil, errors.New("triage: limits must be positive")
	}
	if lim.MaxStream <= 0 {
		lim.MaxStream = lim.MaxBytes
	}
	data, skipped, err := readTail(io.LimitReader(r, lim.MaxStream), lim.MaxBytes)
	if err != nil {
		return nil, err
	}
	if skipped > 0 || midLine {
		// La primera línea de la cola está cortada por la mitad: fuera.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			skipped += int64(i + 1)
			data = data[i+1:]
		}
	}
	lg := parse(data, lim)
	lg.Skipped = skipped
	lg.Truncated = skipped > 0 || lg.Truncated
	return lg, nil
}

// readTail lee todo r y devuelve sus últimos max bytes, con memoria acotada a
// 2×max: un búfer que se compacta cuando se llena.
func readTail(r io.Reader, max int64) ([]byte, int64, error) {
	buf := make([]byte, 0, min(max, 1<<20))
	var skipped int64
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if int64(len(buf)) > 2*max {
			drop := int64(len(buf)) - max
			skipped += drop
			buf = append(buf[:0], buf[drop:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if int64(len(buf)) > max {
		drop := int64(len(buf)) - max
		skipped += drop
		buf = buf[drop:]
	}
	return buf, skipped, nil
}

// maxRawLine: una línea en bruto más larga se parte en trozos (bufio fallaría
// con ErrTooLong). Con la cola ya acotada no hace falta más.
const maxRawLine = 1 << 20

func parse(data []byte, lim Limits) *Log {
	lg := &Log{Format: "plain"}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxRawLine)
	sc.Split(splitLines)
	var p parser
	for sc.Scan() {
		p.line(sc.Bytes(), lim, lg)
	}
	if len(lg.Lines) > lim.MaxLines {
		lg.Lines = lg.Lines[len(lg.Lines)-lim.MaxLines:]
		lg.Truncated = true
	}
	lg.Format = p.format()
	return lg
}

// splitLines es bufio.ScanLines, pero corta en seco las líneas que no caben
// en el búfer en vez de fallar.
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		if len(data) == 0 {
			return 0, nil, nil
		}
		return len(data), data, nil
	}
	if len(data) >= maxRawLine {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parser guarda el estado entre líneas: la sección en la que se está.
type parser struct {
	travisSection string
	ghStep        string // paso que da `gh run view --log-failed` (a veces "UNKNOWN STEP")
	ghGroup       string // el último ##[group] dentro del paso
	nTravis, nGH  int
}

func (p *parser) format() string {
	switch {
	case p.nGH > 0 && p.nGH >= p.nTravis:
		return "github"
	case p.nTravis > 0:
		return "travis"
	}
	return "plain"
}

// line interpreta una línea en bruto: marcas de Travis y de GitHub Actions,
// retornos de carro de las barras de progreso y colores ANSI.
func (p *parser) line(raw []byte, lim Limits, lg *Log) {
	s := string(raw)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	s = strings.TrimRight(s, "\r")
	job := ""
	gh := false

	// `gh run view --log-failed`: "trabajo\tpaso\tmarca-de-tiempo contenido".
	if a, rest, ok := strings.Cut(s, "\t"); ok {
		if b, rest2, ok := strings.Cut(rest, "\t"); ok {
			if c, ok := cutTimestamp(strings.TrimPrefix(rest2, "\ufeff")); ok {
				gh, job, s = true, a, c
				if b != p.ghStep {
					p.ghStep, p.ghGroup = b, ""
				}
			}
		}
	}
	// El log de un paso descargado tal cual: "marca-de-tiempo contenido".
	if !gh {
		if c, ok := cutTimestamp(strings.TrimPrefix(s, "\ufeff")); ok {
			gh, s = true, c
		}
	}
	if gh {
		p.nGH++
	}

	marked := false
	switch {
	case strings.HasPrefix(s, "##[group]"):
		s = strings.TrimPrefix(s, "##[group]")
		p.ghGroup, _, _ = stripANSI(s)
	case strings.HasPrefix(s, "##[endgroup]"):
		return
	case strings.HasPrefix(s, "##[error]"):
		s, marked = strings.TrimPrefix(s, "##[error]"), true
	case strings.HasPrefix(s, "##[warning]"), strings.HasPrefix(s, "##[notice]"), strings.HasPrefix(s, "##[debug]"):
		s = s[strings.IndexByte(s, ']')+1:]
	}

	// Travis: un \r seguido de ESC[0K borra la línea; lo que se ve es el último
	// tramo con algo visible. Los tramos anteriores pueden traer marcas de
	// plegado que cambian la sección.
	var visible string
	var red, yellow bool
	endFold := false
	for seg := range strings.SplitSeq(s, "\r") {
		txt, r, y := stripANSI(seg)
		if name, ok := strings.CutPrefix(txt, "travis_fold:start:"); ok {
			p.nTravis++
			p.travisSection = foldName(name)
			continue
		}
		if strings.HasPrefix(txt, "travis_fold:end:") {
			p.nTravis++
			endFold = true
			continue
		}
		if strings.HasPrefix(txt, "travis_time:") {
			p.nTravis++
			continue
		}
		if strings.TrimSpace(txt) != "" {
			visible, red, yellow = txt, r, y
		}
	}
	if len(visible) > lim.MaxLineBytes {
		visible = truncUTF8(visible, lim.MaxLineBytes)
	}
	sec := p.travisSection
	if p.nGH > 0 {
		sec = p.ghStep
		if sec == "" || sec == "UNKNOWN STEP" {
			sec = p.ghGroup
		}
	}
	lg.Lines = append(lg.Lines, Line{Text: visible, Section: sec, Job: job, Red: red, Yellow: yellow, Marked: marked})
	if endFold {
		p.travisSection = ""
	}
	// Tope de memoria mientras se lee: si pasa del doble, se queda la cola.
	if len(lg.Lines) > 2*lim.MaxLines {
		lg.Lines = append(lg.Lines[:0], lg.Lines[len(lg.Lines)-lim.MaxLines:]...)
		lg.Truncated = true
	}
}

// foldName: "install.3" → "install", "git.checkout" → "git.checkout".
func foldName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '.'); i > 0 && allDigits(s[i+1:]) {
		s = s[:i]
	}
	return s
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// cutTimestamp quita una marca RFC 3339 de GitHub Actions
// ("2026-09-01T18:52:03.4695845Z ") del principio.
func cutTimestamp(s string) (string, bool) {
	if len(s) < 21 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return s, false
	}
	if !allDigits(s[:4]) || !allDigits(s[5:7]) || !allDigits(s[8:10]) {
		return s, false
	}
	i := strings.IndexByte(s, ' ')
	if i < 0 || i > 40 || s[i-1] != 'Z' {
		return s, false
	}
	return s[i+1:], true
}

// stripANSI quita las secuencias de escape (CSI y OSC) y los caracteres de
// control, y dice si algo del texto visible iba en rojo o en amarillo.
func stripANSI(s string) (string, bool, bool) {
	if strings.IndexByte(s, 0x1b) < 0 && !hasControl(s) {
		return s, false, false
	}
	var b strings.Builder
	b.Grow(len(s))
	red, yellow, curRed, curYellow := false, false, false, false
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[': // CSI: ESC [ parámetros letra-final
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) && s[j] == 'm' {
					curRed, curYellow = sgr(s[i+2:j], curRed, curYellow)
				}
				i = min(j+1, len(s))
				continue
			case ']': // OSC: hasta BEL o ESC \
				j := i + 2
				for j < len(s) && s[j] != 0x07 && (s[j] != 0x1b || j+1 >= len(s) || s[j+1] != '\\') {
					j++
				}
				if j < len(s) && s[j] == 0x1b {
					j++
				}
				i = min(j+1, len(s))
				continue
			}
			i += 2
			continue
		}
		if c < 0x20 && c != '\t' || c == 0x7f {
			i++
			continue
		}
		if c != ' ' && c != '\t' {
			red = red || curRed
			yellow = yellow || curYellow
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), red, yellow
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return true
		}
	}
	return false
}

// sgr aplica los parámetros de un ESC[…m al estado de color.
func sgr(params string, red, yellow bool) (bool, bool) {
	if params == "" {
		return false, false
	}
	for p := range strings.SplitSeq(params, ";") {
		switch p {
		case "0", "00", "39":
			red, yellow = false, false
		case "31", "91":
			red, yellow = true, false
		case "33", "93":
			red, yellow = false, true
		case "30", "32", "34", "35", "36", "37", "90", "92", "94", "95", "96", "97":
			red, yellow = false, false
		}
	}
	return red, yellow
}

func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Clip recorta s a n bytes sin partir una runa (para mostrar líneas).
func Clip(s string, n int) string { return truncUTF8(s, n) }
