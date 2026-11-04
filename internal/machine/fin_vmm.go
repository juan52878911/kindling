package machine

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode"
)

// POR QUÉ TERMINÓ EL VMM.
//
// kling-hv y kling-vz dejan en su consola (firecracker.log), al acabar, una
// línea con el motivo: "kling-hv: the guest stopped: the guest rebooted
// (SYSTEM_RESET)", "kling-vz: the VM stopped with an error: ...". El
// proceso sale con 0 en los dos casos, y el daemon solo decía "exited (exit
// status 0): failed": había que ir a mirar la consola para saber que el
// invitado se había reiniciado (panic=1) o apagado.

// colaFinVMM es cuánto se lee del final de la consola buscando la línea: el
// VMM la escribe lo último, tras lo que el invitado dejara.
const colaFinVMM = 16 << 10

// maxMotivoFinVMM acota el motivo que va al evento y al log.
const maxMotivoFinVMM = 200

// marcasFinVMM son lo que escribe el VMM (vz/internal/server, watch) tras
// su prefijo "kling-hv: " o "kling-vz: ".
var marcasFinVMM = []string{"the guest stopped", "the VM stopped with an error"}

// motivoFinVMM es el motivo que dejó el VMM de la máquina id en su consola al
// terminar, o "" si no dejó ninguno (Firecracker, un VMM anterior, un kill).
func (m *Manager) motivoFinVMM(id string) string {
	cola, err := leerCola(filepath.Join(m.dir(id), "firecracker.log"), colaFinVMM)
	if err != nil {
		return ""
	}
	return motivoEnCola(cola)
}

// motivoEnCola busca en cola la ÚLTIMA línea de fin del VMM. La consola es
// también la del invitado: una línea igual escrita por él va antes que la
// del VMM, que es lo último; aun así, lo que se devuelve es solo texto para
// el evento, saneado y acotado, y nunca decide nada.
func motivoEnCola(cola []byte) string {
	for len(cola) > 0 {
		i := bytes.LastIndexByte(bytes.TrimRight(cola, "\r\n"), '\n')
		linea := string(bytes.TrimRight(cola[i+1:], "\r\n"))
		cola = cola[:max(i, 0)]
		for _, pre := range []string{"kling-hv: ", "kling-vz: "} {
			resto, ok := strings.CutPrefix(linea, pre)
			if !ok {
				continue
			}
			for _, marca := range marcasFinVMM {
				if strings.HasPrefix(resto, marca) {
					return sanearMotivo(resto)
				}
			}
		}
		if i < 0 {
			break
		}
	}
	return ""
}

// sanearMotivo deja solo caracteres imprimibles y lo acota.
func sanearMotivo(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if r := []rune(s); len(r) > maxMotivoFinVMM {
		s = string(r[:maxMotivoFinVMM]) + "…"
	}
	return strings.TrimSpace(s)
}
