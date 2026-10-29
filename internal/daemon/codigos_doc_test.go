package daemon

import (
	"bufio"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// La sección "Códigos de estado por ruta" de docs/api.md la escribió a mano
// un agente antes de que los códigos se corrigieran, y se quedó atrás. Este
// test la ata a los goldens: nombra exactamente las rutas de rutas.txt, cada
// código de codigos.txt aparece en la fila de su ruta (literal o por uno de
// los mapeos que cita) y cada `code` del golden está en la tabla de codes.

// docCodigos es lo que se lee de la sección.
type docCodigos struct {
	rutas  map[string]map[int]bool // ruta → códigos que nombra su fila
	mapeos map[string]map[int]bool // mapeo → códigos que puede dar
	codes  map[int][]string        // HTTP → codes de la tabla de codes
}

var (
	reCodigoTilde = regexp.MustCompile("`([1-5][0-9][0-9])`")
	reCodigoSolo  = regexp.MustCompile(`\b([1-5][0-9][0-9])\b`)
	reTilde       = regexp.MustCompile("`([^`]+)`")
	reCode        = regexp.MustCompile("`([a-z][a-z_]+)`")
)

func celdas(fila string) []string {
	f := strings.Trim(strings.TrimSpace(fila), "|")
	out := strings.Split(f, " | ")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func codigosDe(re *regexp.Regexp, s string) map[int]bool {
	m := map[int]bool{}
	for _, x := range re.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(x[1])
		m[n] = true
	}
	return m
}

// rutasDeCelda expande la primera columna: "`POST /machines/{ref}/freeze` ·
// `thaw`" son dos rutas, y "`GET` · `PUT` · `DELETE /x`" tres.
func rutasDeCelda(c string) []string {
	var out, pendientes []string
	var metodo, ruta string
	for _, parte := range strings.Split(c, " · ") {
		m := reTilde.FindStringSubmatch(parte)
		if m == nil {
			continue
		}
		tok := m[1]
		switch {
		case strings.Contains(tok, " "):
			metodo, ruta, _ = strings.Cut(tok, " ")
			out = append(out, tok)
			for _, p := range pendientes {
				out = append(out, p+" "+ruta)
			}
			pendientes = nil
		case strings.ToUpper(tok) == tok:
			pendientes = append(pendientes, tok)
		default:
			out = append(out, metodo+" "+path.Dir(ruta)+"/"+tok)
		}
	}
	return out
}

func leerDocCodigos(t *testing.T) docCodigos {
	t.Helper()
	b, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "## Códigos de estado por ruta")
	if i < 0 {
		t.Fatal("docs/api.md has no «Códigos de estado por ruta» section")
	}
	doc = doc[i:]
	if j := strings.Index(doc[1:], "\n## "); j >= 0 {
		doc = doc[:j+1]
	}
	d := docCodigos{rutas: map[string]map[int]bool{}, mapeos: map[string]map[int]bool{}, codes: map[int][]string{}}
	var cabecera []string
	for _, l := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(l, "|") {
			cabecera = nil
			continue
		}
		c := celdas(l)
		if cabecera == nil {
			cabecera = c
			continue
		}
		if strings.HasPrefix(c[0], "---") {
			continue
		}
		switch cabecera[0] {
		case "HTTP":
			n, err := strconv.Atoi(strings.Trim(c[0], "`"))
			if err != nil {
				t.Fatalf("code table row %q: %v", l, err)
			}
			for _, x := range reCode.FindAllStringSubmatch(strings.Join(c[1:], " "), -1) {
				d.codes[n] = append(d.codes[n], x[1])
			}
		case "Mapeo":
			nombre := strings.Trim(c[0], "`")
			m := map[int]bool{}
			for k := 1; k < len(c) && k < len(cabecera); k++ {
				if n, err := strconv.Atoi(strings.Trim(cabecera[k], "`")); err == nil {
					if c[k] != "—" && c[k] != "" {
						m[n] = true
					}
					continue
				}
				for n := range codigosDe(reCodigoTilde, c[k]) {
					m[n] = true
				}
			}
			d.mapeos[nombre] = m
		case "Ruta":
			if len(c) != 3 {
				t.Fatalf("route row with %d columns: %q", len(c), l)
			}
			m := codigosDe(reCodigoSolo, c[1])
			for n := range codigosDe(reCodigoTilde, c[2]) {
				m[n] = true
			}
			// Los mapeos que cita la fila suman sus códigos (su tabla va antes).
			for _, x := range reTilde.FindAllStringSubmatch(c[2], -1) {
				for n := range d.mapeos[x[1]] {
					m[n] = true
				}
			}
			for _, r := range rutasDeCelda(c[0]) {
				if _, dup := d.rutas[r]; dup {
					t.Errorf("docs/api.md lists %s twice", r)
				}
				d.rutas[r] = m
			}
		}
	}
	return d
}

func leerGolden(t *testing.T, nombre string) []string {
	t.Helper()
	f, err := os.Open("testdata/contrato/" + nombre)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func TestCodigosMatchAPIDoc(t *testing.T) {
	d := leerDocCodigos(t)
	if len(d.mapeos) == 0 || len(d.codes) == 0 {
		t.Fatal("docs/api.md: the mapping or the code table is missing")
	}

	var rutas []string
	for _, l := range leerGolden(t, "rutas.txt") {
		f := strings.Fields(l)
		rutas = append(rutas, f[0]+" "+f[1])
	}
	for _, r := range rutas {
		if _, ok := d.rutas[r]; !ok {
			t.Errorf("route %s is not in the status code table of docs/api.md", r)
		}
	}
	for r := range d.rutas {
		if !slices.Contains(rutas, r) {
			t.Errorf("docs/api.md documents %s, which is not in rutas.txt", r)
		}
	}

	for status, codes := range d.codes {
		if g := api.CodeForStatus(status); g != "" && !slices.Contains(codes, string(g)) {
			t.Errorf("docs/api.md: %d should list code %q (api.CodeForStatus)", status, g)
		}
	}

	for _, l := range leerGolden(t, "codigos.txt") {
		ruta, resto, ok1 := strings.Cut(l, "  ←  ")
		_, res, ok2 := strings.Cut(resto, "  →  ")
		if !ok1 || !ok2 {
			t.Fatalf("codigos.txt: malformed line %q", l)
		}
		f := strings.Fields(res)
		status, err := strconv.Atoi(f[0])
		if err != nil {
			t.Fatalf("codigos.txt: %q: %v", l, err)
		}
		if m, ok := d.rutas[ruta]; ok && !m[status] {
			t.Errorf("codigos.txt says %s answers %d (%s), but its row in docs/api.md does not name it", ruta, status, strings.TrimSpace(resto))
		}
		if len(f) > 1 && f[1] != "-" && !slices.Contains(d.codes[status], f[1]) {
			t.Errorf("codigos.txt gives code %q with %d, missing from the code table of docs/api.md", f[1], status)
		}
	}
}
