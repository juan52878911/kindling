//go:build darwin && arm64

package hvvm

/*
#include "khv.h"

int khv_prueba_globo(uint64_t out[4]); // prueba_globo.c
int khv_prueba_globo_cuota(int n, uint64_t out[2]);
*/
import "C"

// pruebaGlobo es khv_prueba_globo (prueba_globo.c): los tramos que pide
// soltar el globo al inflar, al recibir un informe de páginas libres y al
// desinflar, y los búferes del informe devueltos. ok=false sin RAM de
// mentira. Solo para las pruebas.
func pruebaGlobo() (inflar, informe, devueltos, desinflar uint64, ok bool) {
	var out [4]C.uint64_t
	if C.khv_prueba_globo(&out[0]) != 0 {
		return 0, 0, 0, 0, false
	}
	return uint64(out[0]), uint64(out[1]), uint64(out[2]), uint64(out[3]), true
}

// pruebaGloboCuota es khv_prueba_globo_cuota: n informes seguidos del mismo
// bloque. Devuelve los tramos que se pidió soltar y los búferes devueltos.
func pruebaGloboCuota(n int) (tramos, devueltos uint64, ok bool) {
	var out [2]C.uint64_t
	if C.khv_prueba_globo_cuota(C.int(n), &out[0]) != 0 {
		return 0, 0, false
	}
	return uint64(out[0]), uint64(out[1]), true
}
