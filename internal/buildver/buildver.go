// Package buildver da la versión de un binario cuando el enlazador no la fijó
// (-ldflags "-X main.Version=…"): la del módulo si se instaló con
// `go install …@vX.Y.Z`, o la revisión de git de un `go build` a mano.
package buildver

import (
	"runtime/debug"
	"strings"
)

// Version es v si la fijó el enlazador; si es "dev" o vacía, lo que diga
// debug.ReadBuildInfo: "X.Y.Z" de una etiqueta limpia, o "dev+<8 de la
// revisión>[.dirty]". Sin nada de eso, v tal cual.
func Version(v string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	return resolver(v, bi)
}

// IsDev dice si v es de un binario de desarrollo: no se compara con otras
// versiones (doctor, image ls). "dev+…" lo es: lleva el commit, no la release.
func IsDev(v string) bool {
	v = strings.TrimPrefix(v, "v")
	return v == "" || v == "dev" || strings.HasPrefix(v, "dev+")
}

func resolver(v string, bi *debug.BuildInfo) string {
	if (v != "" && v != "dev") || bi == nil {
		return v
	}
	// Una etiqueta limpia (go install …@v1.0.0) es una release. Una
	// pseudoversión o con +dirty (go build desde Go 1.24) no: su revisión.
	if mv := bi.Main.Version; strings.HasPrefix(mv, "v") && !strings.ContainsAny(mv, "-+") && strings.Count(mv, ".") == 2 {
		return strings.TrimPrefix(mv, "v")
	}
	var rev, sucio string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				sucio = ".dirty"
			}
		}
	}
	if rev == "" {
		return v
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	return "dev+" + rev + sucio
}
