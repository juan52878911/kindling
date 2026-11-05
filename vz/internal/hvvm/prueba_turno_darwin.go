//go:build darwin && arm64

package hvvm

/*
#include "khv.h"

int khv_prueba_turno_vcpus(void);
*/
import "C"

// pruebaTurnoVCPUs es khv_prueba_turno_vcpus (vm.c): 0 si los hilos de vCPU
// pasan a hv_vcpu_create por orden de índice aunque lleguen al revés. Solo
// para las pruebas.
func pruebaTurnoVCPUs() int { return int(C.khv_prueba_turno_vcpus()) }
