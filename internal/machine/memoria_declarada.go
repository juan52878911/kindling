package machine

// Admisión por memoria DECLARADA (macOS). checkHostMemory mira la memoria que
// el host tiene libre AHORA; en macOS una VM ocupa solo lo que su invitado
// toca, así que con muchas máquinas ociosas siempre parece que cabe una más, y
// cuando los invitados empiezan a tocar su memoria (o un VMM tomado por el
// suyo reserva sin freno: Darwin no deja ponerle techo por proceso, ver
// SECURITY.md) ya están todas dentro y quien decide es jetsam. Esto acota lo
// prometido: la suma de mem_mib (o mem_max_mib) de las máquinas con VMM vivo
// más las que están arrancando no pasa de KLING_MEM_OVERCOMMIT veces la RAM
// física. En Linux no aplica: cada VM tiene su techo en el cgroup y
// memoriaTotalPlataforma da 0.

import (
	"fmt"
	"os"
	"strconv"

	"github.com/juan52878911/kindling/pkg/api"
)

// overcommitPorDefecto: cuatro veces la RAM. Deja la densidad medida (127
// VMs de 256 MiB en un Mac de 16 GiB son ~32 GiB declarados) y para antes de
// prometer decenas de GiB que el Mac no podría dar si los invitados los usaran.
const overcommitPorDefecto = 4.0

// overcommitMemoria es cuántas veces la RAM física puede sumar lo declarado:
// KLING_MEM_OVERCOMMIT (0 = sin este techo), o overcommitPorDefecto.
func overcommitMemoria() float64 {
	if v := os.Getenv("KLING_MEM_OVERCOMMIT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return overcommitPorDefecto
}

// memoriaFisicaDeclarada es la RAM física en MiB (0 en Linux); una variable
// para las pruebas.
var memoriaFisicaDeclarada = memoriaTotalPlataforma

// limiteDeclaradaMiB es el techo de lo declarado, 0 si no hay.
func limiteDeclaradaMiB() int64 {
	f := overcommitMemoria()
	total := memoriaFisicaDeclarada()
	if f <= 0 || total <= 0 {
		return 0
	}
	return int64(f * float64(total))
}

// declaradaVivaLocked suma lo declarado por las máquinas cuyo VMM ocupa
// memoria: corriendo o en pausa (las congeladas y paradas no tienen proceso).
// Con m.mu tomado.
func (m *Manager) declaradaVivaLocked() int64 {
	var n int64
	for _, mc := range m.byID {
		if mc.State == api.StateRunning || mc.State == api.StatePaused {
			n += int64(memCuota(mc.MemMiB, mc.MemMaxMiB))
		}
	}
	return n
}

// checkMemoriaDeclaradaLocked rechaza un arranque de wantMiB que dejaría lo
// declarado por encima del techo. Con 507 (memoria insuficiente), que
// reserveMemoryMakingRoom entiende: congela ociosas y reintenta. Con m.mu
// tomado.
func (m *Manager) checkMemoriaDeclaradaLocked(wantMiB int) error {
	lim := limiteDeclaradaMiB()
	if lim <= 0 || wantMiB <= 0 {
		return nil
	}
	vivas := m.declaradaVivaLocked() + int64(m.pendingDeclMiB)
	if vivas+int64(wantMiB) <= lim {
		return nil
	}
	return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
		"doesn't fit: the running microVMs already declare %d MiB of memory and this one asks for %d MiB; "+
			"the daemon admits up to %d MiB declared (%g times the %d MiB of RAM).\n"+
			"Freeze or remove an instance (`kling ps -a`), give it less memory with -mem, "+
			"or raise KLING_MEM_OVERCOMMIT in the daemon's environment (0 turns this limit off)",
		vivas, wantMiB, lim, overcommitMemoria(), memoriaFisicaDeclarada())}
}
