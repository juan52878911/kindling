//go:build darwin && arm64

package hvvm

/*
#include "khv.h"
*/
import "C"

// filtrarID pasa los registros de identificación por khv_filtrar_id (vm.c),
// lo que ve el invitado de ID_AA64PFR0/PFR1/ZFR0/SMFR0_EL1. Solo para las
// pruebas.
func filtrarID(pfr0, pfr1, zfr0, smfr0 uint64) (uint64, uint64, uint64, uint64) {
	a, b, c, d := C.uint64_t(pfr0), C.uint64_t(pfr1), C.uint64_t(zfr0), C.uint64_t(smfr0)
	C.khv_filtrar_id(&a, &b, &c, &d)
	return uint64(a), uint64(b), uint64(c), uint64(d)
}

// pruebaCPUOn es khv_prueba_cpu_on: 0 si el estado guardado de un CPU_ON
// pendiente cuadra.
func pruebaCPUOn() int { return int(C.khv_prueba_cpu_on()) }

// sysregISS es khv_sysreg_iss: el hv_sys_reg_t de un MSR/MRS atrapado.
func sysregISS(iss uint64) uint16 { return uint16(C.khv_sysreg_iss(C.uint64_t(iss))) }

// finMotivo es khv_fin_motivo: KHV_FIN_NADA mientras la VM no haya terminado.
func finMotivo() int { return int(C.khv_fin_motivo()) }
