//go:build darwin

package net

// En macOS no hay aristas entre máquinas en esta versión (docs/grafos.md): la
// red vive dentro de cada kling-vz y el daemon no tiene dónde resolver bajo
// su candado en cada conexión. El manager lo rechaza antes con un 501; estas
// funciones existen para que compile igual.

import (
	"errors"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// LinkSpec: ver enlaces_fc.go.
type LinkSpec struct {
	Host    string
	Port    int
	Target  string
	Resolve credproxy.ResolveLinkFunc
}

// GraphSpec: ver enlaces_fc.go.
type GraphSpec struct {
	Egress      Egress
	Domains     []string
	Links       []LinkSpec
	Credentials bool
	AuditPath   string
}

// SetGraph no monta nada en macOS.
func SetGraph(n *Net, spec GraphSpec) error {
	return errors.New("graph edges between machines are Linux-only in this version")
}

// InvalidarEnlaces no tiene nada que cortar.
func InvalidarEnlaces(ids ...string) int { return 0 }
