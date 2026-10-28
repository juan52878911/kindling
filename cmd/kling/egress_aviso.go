package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// egressAvisoTimeout acota la consulta del aviso: nunca debe retrasar ni
// romper el comando que lo pide.
const egressAvisoTimeout = 3 * time.Second

// egressBlocker es lo que hace falta del daemon para el aviso; *api.Client lo
// cumple. Es opcional en los dobles de test (ver avisoEgress).
type egressBlocker interface {
	EgressBlocked(ctx context.Context, ref string) (*api.EgressBlocked, error)
}

// avisoEgress devuelve el aviso de "conexiones bloqueadas por egress" de la
// máquina ref, o "" si no se cortó nada, si el daemon no lo sabe (uno más
// viejo, sin la ruta) o si falla: es un consejo, no un resultado, y un fallo
// aquí no puede tapar el error de verdad del comando.
func avisoEgress(ctx context.Context, c any, ref string) string {
	b, ok := c.(egressBlocker)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), egressAvisoTimeout)
	defer cancel()
	e, err := b.EgressBlocked(ctx, ref)
	if err != nil || e == nil {
		return ""
	}
	return e.Warning()
}

// avisarEgress imprime por stderr el aviso de avisoEgress, si lo hay.
func avisarEgress(ctx context.Context, c any, ref string) {
	if a := avisoEgress(ctx, c, ref); a != "" {
		fmt.Fprintln(os.Stderr, "kling: "+a)
	}
}
