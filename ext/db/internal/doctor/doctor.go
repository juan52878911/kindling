// Package doctor revisa la seguridad de un Postgres: el de una copia de
// kling db (por `kling exec` dentro de la máquina, como el superusuario local)
// o uno cualquiera alcanzable desde el host (-url, con pgmini).
//
// Cada hallazgo lleva una regla (DBnnn), una severidad, un mensaje en inglés y
// cómo arreglarlo. Run escribe el informe legible y devuelve cuántos problemas
// hay: todo hallazgo que no es informativo.
//
// Todo lo que viene de la base (nombres de roles, expresiones de políticas,
// verificadores) es dato no fiable: se escapa antes de imprimirlo y nunca se
// ejecuta ni se mete en una línea de shell.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
)

// Etiquetas de kling db sobre las máquinas (api.KeyPattern: sin '/').
const (
	LabelGolden = "kling.db.golden"
	LabelOwner  = "kling.db.owner"
	LabelState  = "kling.db.state"

	StatePreparing = "preparing"
	StateReady     = "ready"
)

// Target es lo que se revisa: una máquina (copia de kling db) o una URL.
// Exactamente uno de los dos.
type Target struct {
	Machine string
	URL     string
	// Solo con URL. CAFile añade raíces de confianza a las del sistema (gana
	// sobre sslrootcert de la URL); TLSServerName es el nombre que se
	// verifica si no es el host de la URL; Insecure permite hablar sin TLS
	// (o sin verificar al servidor) con un servidor que no es loopback.
	CAFile        string
	TLSServerName string
	Insecure      bool
}

// Severity ordena los hallazgos. Info no cuenta como problema.
type Severity int

const (
	Info Severity = iota
	Warn
	High
	Critical
)

func (s Severity) String() string {
	switch s {
	case Warn:
		return "WARN"
	case High:
		return "HIGH"
	case Critical:
		return "CRITICAL"
	}
	return "INFO"
}

// Finding es un hallazgo: regla, severidad, qué pasa y cómo arreglarlo.
type Finding struct {
	Rule string
	Sev  Severity
	Msg  string
	Fix  string
}

// querier lanza una consulta que devuelve un único valor de texto (las de la
// revisión devuelven JSON en una sola línea). name identifica la consulta en
// los logs y en los tests; db es la base (solo cuenta en las copias).
type querier interface {
	query(ctx context.Context, db, name, sql string) (string, error)
}

type report struct {
	target   string
	findings []Finding
}

func (r *report) add(rule string, sev Severity, fix, format string, args ...any) {
	r.findings = append(r.findings, Finding{Rule: rule, Sev: sev, Msg: fmt.Sprintf(format, args...), Fix: fix})
}

func (r *report) problems() int {
	n := 0
	for _, f := range r.findings {
		if f.Sev > Info {
			n++
		}
	}
	return n
}

func (r *report) write(w io.Writer) {
	fs := append([]Finding(nil), r.findings...)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Sev > fs[j].Sev })
	fmt.Fprintf(w, "kling db doctor: %s\n", r.target)
	var cuenta [Critical + 1]int
	for _, f := range fs {
		cuenta[f.Sev]++
		fmt.Fprintf(w, "  %-8s %s  %s\n", f.Sev, f.Rule, f.Msg)
		if f.Fix != "" {
			fmt.Fprintf(w, "  %-8s        fix: %s\n", "", f.Fix)
		}
	}
	if len(fs) == 0 {
		fmt.Fprintln(w, "  no findings")
	}
	fmt.Fprintf(w, "summary: %d critical, %d high, %d warn, %d info; %d problem(s)\n",
		cuenta[Critical], cuenta[High], cuenta[Warn], cuenta[Info], r.problems())
}

// Run revisa t, escribe el informe en w y devuelve cuántos problemas encontró.
// err es para lo que impide revisar (no se alcanza la máquina o la base); una
// comprobación suelta que no puede correr es un hallazgo, no un error.
func Run(ctx context.Context, k klingc.Kling, t Target, w io.Writer) (problems int, err error) {
	if (t.Machine == "") == (t.URL == "") {
		return 0, errors.New("doctor: give exactly one of a machine or a URL")
	}
	r := &report{}
	if t.Machine != "" {
		if k == nil {
			return 0, errors.New("doctor: no kling client")
		}
		err = runCopy(ctx, k, t.Machine, r)
	} else {
		err = runURL(ctx, t, r)
	}
	if err != nil {
		return 0, err
	}
	r.write(w)
	return r.problems(), nil
}

// safe deja un texto de la base listo para la terminal: sin caracteres de
// control (secuencias de escape) y acotado a n runas.
func safe(s string, n int) string {
	var b strings.Builder
	i := 0
	for _, c := range s {
		if i == n {
			b.WriteString("...")
			break
		}
		if c == '\n' || c == '\t' || c == '\r' {
			c = ' '
		} else if c == utf8.RuneError || unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			// Cf: también los controles bidi, que reordenan lo que se ve.
			c = '?'
		}
		b.WriteRune(c)
		i++
	}
	return b.String()
}

// q cita un identificador de la base para el mensaje.
func q(s string) string { return `"` + safe(s, 64) + `"` }
