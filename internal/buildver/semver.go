package buildver

import "strings"

// El único sitio donde se comparan versiones de kling: pkg/plugin
// (min_kling, actualizaciones de extensiones) e internal/upgrade (no volver
// atrás sin querer) usan esto.

// version es una versión leída: sus tres números, el pre-release ("rc.2") y
// si lleva algo que la hace de desarrollo (un `git describe` con commits
// encima, -dirty) o metadatos de build (+…).
type version struct {
	num  [3]int
	pre  []string
	dev  bool
	meta bool
}

const alfanum = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-"

// parse lee "vX.Y[.Z][-pre][+meta]". Los sufijos de desarrollo
// ("-5-gabc123", "-dirty") se apartan y se anotan en dev: lo que queda es la
// versión de la etiqueta de la que salió.
func parse(v string) (version, bool) {
	var out version
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v, out.meta = v[:i], true
	}
	if s, ok := strings.CutSuffix(v, "-dirty"); ok {
		v, out.dev = s, true
	}
	if s, ok := sinDescribe(v); ok {
		v, out.dev = s, true
	}
	core, pre, hayPre := strings.Cut(v, "-")
	partes := strings.Split(core, ".")
	if core == "" || len(partes) > 3 {
		return out, false
	}
	for i, p := range partes {
		n, ok := numero(p)
		if !ok {
			return out, false
		}
		out.num[i] = n
	}
	if hayPre {
		for _, id := range strings.Split(pre, ".") {
			if id == "" || strings.Trim(id, alfanum) != "" {
				return out, false
			}
			out.pre = append(out.pre, id)
		}
	}
	return out, true
}

// sinDescribe quita el "-N-gSHA" final que deja `git describe` fuera de una
// etiqueta exacta.
func sinDescribe(v string) (string, bool) {
	i := strings.LastIndexByte(v, '-')
	if i < 0 || len(v) < i+3 || v[i+1] != 'g' || !hex(v[i+2:]) {
		return v, false
	}
	j := strings.LastIndexByte(v[:i], '-')
	if j < 0 {
		return v, false
	}
	if _, ok := numero(v[j+1 : i]); !ok {
		return v, false
	}
	return v[:j], true
}

func numero(s string) (int, bool) {
	if s == "" || len(s) > 9 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n, true
}

func hex(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef") == ""
}

// Compare ordena dos versiones como semver: -1, 0 o 1, y false si alguna no
// se puede leer ("dev", "dev+…"). Un pre-release va antes que su versión
// (1.0.0-rc.2 < 1.0.0) y se ordena por sus campos (rc.2 < rc.10). Los
// sufijos de desarrollo y los metadatos (+…) no cuentan: 0.17.0-12-gabc es
// igual que 0.17.0.
func Compare(a, b string) (int, bool) {
	x, ok := parse(a)
	if !ok {
		return 0, false
	}
	y, ok := parse(b)
	if !ok {
		return 0, false
	}
	for i := range x.num {
		if x.num[i] != y.num[i] {
			return signo(x.num[i] - y.num[i]), true
		}
	}
	switch {
	case len(x.pre) == 0 && len(y.pre) == 0:
		return 0, true
	case len(x.pre) == 0:
		return 1, true
	case len(y.pre) == 0:
		return -1, true
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		if c := compararCampo(x.pre[i], y.pre[i]); c != 0 {
			return c, true
		}
	}
	return signo(len(x.pre) - len(y.pre)), true
}

// compararCampo sigue semver: los numéricos por su valor y antes que los
// alfanuméricos; estos, por orden ASCII.
func compararCampo(a, b string) int {
	na, aok := numero(a)
	nb, bok := numero(b)
	switch {
	case aok && bok:
		return signo(na - nb)
	case aok:
		return -1
	case bok:
		return 1
	}
	return strings.Compare(a, b)
}

func signo(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// IsRelease dice si v sirve como etiqueta de release: "1.0.0" o
// "1.0.0-rc.2" sí; "dev", un `git describe` con commits encima
// ("0.12.0-5-gabc"), "-dirty" o con metadatos (+…), no.
func IsRelease(v string) bool {
	p, ok := parse(v)
	return ok && !p.dev && !p.meta
}

// pseudo dice si mv es una pseudoversión de Go
// (…-20261109120000-0123456789ab): la de un commit sin etiqueta.
func pseudo(mv string) bool {
	i := strings.LastIndexByte(mv, '-')
	if i < 15 || len(mv)-i-1 != 12 || !hex(mv[i+1:]) {
		return false
	}
	sep := mv[i-15]
	return strings.Trim(mv[i-14:i], "0123456789") == "" && (sep == '-' || sep == '.')
}
