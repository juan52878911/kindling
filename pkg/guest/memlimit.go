package guest

// El tope blando de memoria del runtime de Go en el agente.
//
// Sin él, el agente corre con GOGC=100 y nada más: tras un pico (un /exec con
// mucha salida, una subida de /files) el montón se queda al doble de lo vivo
// hasta la siguiente GC, y el scavenger devuelve las páginas al sistema a su
// ritmo. En un invitado de 128-256 MiB son decenas de MiB que el globo no
// puede reclamar. Con un tope, la GC y el scavenger aprietan al acercarse a
// él. Es blando: si lo vivo lo pasa, Go no falla, solo recolecta más a menudo
// (con la CPU de la GC acotada a la mitad).

import (
	"log"
	"os"
	"runtime/debug"
)

const (
	agenteLimiteMin = 32 << 20
	agenteLimiteMax = 128 << 20
)

// limiteMemoriaAgente es el tope para un invitado de totalMiB: una cuarta
// parte, entre 32 y 128 MiB. Con GOMEMLIMIT en el entorno (env) el runtime ya
// lo aplicó y no se toca; sin total conocido, tampoco.
func limiteMemoriaAgente(totalMiB int, env string) (int64, bool) {
	if env != "" || totalMiB <= 0 {
		return 0, false
	}
	return min(max(int64(totalMiB)<<20/4, agenteLimiteMin), agenteLimiteMax), true
}

// ajustarLimiteMemoria pone el tope si este proceso es el agente de una
// microVM (PID 1): en los tests y en el host no se toca el runtime.
func ajustarLimiteMemoria() {
	if os.Getpid() != 1 {
		return
	}
	f, err := os.Open(meminfoPath)
	if err != nil {
		return
	}
	mi, _ := parseMeminfo(f)
	f.Close()
	if lim, ok := limiteMemoriaAgente(mi.TotalMiB, os.Getenv("GOMEMLIMIT")); ok {
		debug.SetMemoryLimit(lim)
		log.Printf("agent: Go memory limit %d MiB (guest has %d MiB)", lim>>20, mi.TotalMiB)
	}
}
