//go:build darwin && arm64

package hvvm

/*
#include "khv.h"

int khv_prueba_vtimer_pendiente(void);
*/
import "C"

// pruebaVtimerPendiente es khv_prueba_vtimer_pendiente (vm.c): 0 si el
// snapshot lleva sumado el ajuste del contador virtual que un vCPU aún no
// aplicó. Solo para las pruebas.
func pruebaVtimerPendiente() int { return int(C.khv_prueba_vtimer_pendiente()) }
