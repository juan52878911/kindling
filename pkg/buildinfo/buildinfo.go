// Package buildinfo resuelve la versión que enseña un programa cuando nadie la
// inyectó al compilar.
//
// `make` pasa -X main.Version=$(git describe ...) y eso es lo que se enseña.
// Pero un `go build ./examples/domotica` o un `go install ...@v0.14.0` a secas
// dejan "dev", que no dice nada. Go graba en el binario el módulo principal y
// la revisión de git con la que se compiló (runtime/debug.ReadBuildInfo), y de
// ahí se saca algo útil: la versión del módulo si vino de `go install
// ...@vX.Y.Z`, o la revisión corta (con -dirty si el árbol tenía cambios),
// como haría `git describe --always --dirty` sin etiquetas.
package buildinfo

import "runtime/debug"

// Version devuelve injected si trae una versión de verdad; si no, lo que Go
// grabó en el binario, y "dev" si tampoco hay nada.
func Version(injected string) string {
	if injected != "" && injected != "dev" {
		return injected
	}
	return fromBuildInfo(debug.ReadBuildInfo())
}

func fromBuildInfo(bi *debug.BuildInfo, ok bool) string {
	if !ok || bi == nil {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		rev += "-dirty"
	}
	return rev
}
