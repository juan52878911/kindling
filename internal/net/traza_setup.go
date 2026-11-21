//go:build !darwin

package net

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// trazaSetup cronometra las sub-fases de Setup y, con KLING_NET_TRACE=1, las
// escribe al log al terminar. Sin la variable no hace nada (ni mira el reloj).
type trazaSetup struct {
	ns      string
	inicio  time.Time
	ultimo  time.Time
	fases   []string
	apagada bool
}

func nuevaTrazaSetup(ns string) *trazaSetup {
	if os.Getenv("KLING_NET_TRACE") != "1" {
		return &trazaSetup{apagada: true}
	}
	ahora := time.Now()
	return &trazaSetup{ns: ns, inicio: ahora, ultimo: ahora}
}

// fase anota lo que tardó desde la anterior.
func (t *trazaSetup) fase(nombre string) {
	if t.apagada {
		return
	}
	ahora := time.Now()
	t.fases = append(t.fases, fmt.Sprintf("%s=%.1fms", nombre, float64(ahora.Sub(t.ultimo).Microseconds())/1000))
	t.ultimo = ahora
}

func (t *trazaSetup) fin(err error) {
	if t.apagada {
		return
	}
	estado := "ok"
	if err != nil {
		estado = "error"
	}
	log.Printf("net: setup %s %s total=%.1fms %s", t.ns, estado,
		float64(time.Since(t.inicio).Microseconds())/1000, strings.Join(t.fases, " "))
}
