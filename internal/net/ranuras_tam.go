package net

import (
	"log"
	"os"
	"strconv"
	"time"
)

// TAMAÑO ADAPTATIVO DEL FONDO DE RANURAS.
//
// El fondo fijo (PoolPorDefecto) se agota con una ráfaga: las altas que
// sobran pagan el montaje completo. El objetivo crece con las tomas de los
// últimos 60 s (6 cubos de 10 s), hasta KLING_NET_POOL_MAX, y en reposo baja
// 2 ranuras por vuelta de Recoger hasta el mínimo.

// PoolMaxPorDefecto es el tope del tamaño adaptativo (KLING_NET_POOL_MAX).
const PoolMaxPorDefecto = 64

// TamPoolMax lee KLING_NET_POOL_MAX: vacío o inválido, el defecto. Un valor
// menor que el mínimo (KLING_NET_POOL) desactiva la adaptación.
func TamPoolMax() int {
	v := os.Getenv("KLING_NET_POOL_MAX")
	if v == "" {
		return PoolMaxPorDefecto
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 256 {
		log.Printf("net: KLING_NET_POOL_MAX=%q is not valid (0-256); using %d", v, PoolMaxPorDefecto)
		return PoolMaxPorDefecto
	}
	return n
}

const (
	cubosTam      = 6
	anchoCuboTam  = 10 * time.Second
	bajaPorVuelta = 2
)

// tamAdaptativo calcula el objetivo del fondo. No es seguro para uso
// concurrente: el Fondo lo llama con su cerrojo.
type tamAdaptativo struct {
	min, max int
	cubos    [cubosTam]int
	ids      [cubosTam]int64 // cubo de tiempo al que pertenece cada posición
	actual   int
}

func (a *tamAdaptativo) activo() bool { return a.max > a.min }

func cuboDe(t time.Time) int64 { return t.UnixNano() / int64(anchoCuboTam) }

// anotar cuenta una toma (con ranura o sin ella: si solo contaran las vacías,
// al crecer el fondo desaparecerían y el tamaño oscilaría).
func (a *tamAdaptativo) anotar(ahora time.Time) {
	id := cuboDe(ahora)
	i := int(((id % cubosTam) + cubosTam) % cubosTam)
	if a.ids[i] != id {
		a.ids[i], a.cubos[i] = id, 0
	}
	a.cubos[i]++
}

// demanda son las tomas de la ventana, acotadas a [min, max].
func (a *tamAdaptativo) demanda(ahora time.Time) int {
	cur, n := cuboDe(ahora), 0
	for i := range a.cubos {
		if a.ids[i] > cur-cubosTam && a.ids[i] <= cur {
			n += a.cubos[i]
		}
	}
	return min(max(n, a.min), a.max)
}

// objetivo sube el tamaño si la demanda lo pide y lo devuelve; no baja.
func (a *tamAdaptativo) objetivo(ahora time.Time) int {
	if !a.activo() {
		return a.min
	}
	a.actual = max(a.actual, a.min, a.demanda(ahora))
	return a.actual
}

// encoger baja el tamaño como mucho bajaPorVuelta hacia la demanda y lo
// devuelve.
func (a *tamAdaptativo) encoger(ahora time.Time) int {
	if !a.activo() {
		return a.min
	}
	if d := a.demanda(ahora); a.actual > d {
		a.actual = max(d, a.actual-bajaPorVuelta)
	}
	a.actual = max(a.actual, a.min)
	return a.actual
}
