package main

// Versiones de macOS: qué mínimo pide cada VMM y cómo se comparan. `kling up
// -check` y daemon.vmm auto miran la versión entera (15.1 no es 15.2).

import (
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/internal/machine"
)

// Versiones mínimas de macOS de cada VMM, comparadas enteras (15.1 < 15.2).
//
//   - vz: congelar y descongelar usa el guardado de estado de
//     Virtualization.framework, de macOS 14.
//   - hv: el GIC de Hypervisor.framework (hv_gic_create y compañía) es de
//     macOS 15.0 según el SDK, pero kling-hv (vz/internal/hvvm) se compila con
//     -mmacosx-version-min=15.2 y solo se ha probado en 15.2 o después; el
//     mínimo es el del binario, no el de la API (docs/mac.md).
const (
	macOSMinVZ = "14.0"
	macOSMinHV = "15.2"
)

// macOSMinDe es la versión mínima de macOS del backend.
func macOSMinDe(backend string) string {
	if backend == machine.BackendHV {
		return macOSMinHV
	}
	return macOSMinVZ
}

// partesVersion lee "15.2.1" como [15 2 1]; ok false si no es una versión.
func partesVersion(v string) (p [3]int, ok bool) {
	campos := strings.Split(strings.TrimSpace(v), ".")
	if len(campos) == 0 || len(campos) > 3 || campos[0] == "" {
		return p, false
	}
	for i, c := range campos {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 {
			return p, false
		}
		p[i] = n
	}
	return p, true
}

// versionAlMenos dice si v >= min. Una v que no se lee no llega a nada.
func versionAlMenos(v, min string) bool {
	a, ok := partesVersion(v)
	b, okMin := partesVersion(min)
	if !ok || !okMin {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}
