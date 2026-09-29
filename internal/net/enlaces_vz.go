//go:build darwin

package net

// En macOS no hay nada que montar en el host para las aristas de un grafo
// (docs/grafos.md): la red vive dentro de cada kling-vz, que atiende las
// aristas en su pasarela y pide cada conexión al daemon por su broker
// (internal/machine/broker*.go, pkg/linkbroker). internal/machine le manda
// las aristas por su API (PUT /kling/graph). Estas funciones existen para que
// el manager no distinga sistemas.

import (
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

// SetGraph no monta nada en macOS: ver la cabecera.
func SetGraph(n *Net, spec GraphSpec) error { return nil }

// InvalidarEnlaces no tiene nada que cortar aquí: las sesiones de las
// aristas en macOS las lleva el broker del manager.
func InvalidarEnlaces(ids ...string) int { return 0 }
