package machine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

// ultimaLineaConsola es la última línea no vacía de la consola de la máquina
// id, saneada y acotada, o "". Cuando el VMM muere de una señal no deja su
// línea de fin, y lo último que escribió suele ser el porqué: kling-hv
// muerto por HVF dejaba 'FIXME: "Handle no active virtual interrupt, EOIR1
// 0x1b"', y la consola se borraba con la copia de un fork deshecho.
func (m *Manager) ultimaLineaConsola(id string) string {
	cola, err := leerCola(filepath.Join(m.dir(id), "firecracker.log"), 4<<10)
	if err != nil {
		return ""
	}
	cola = bytes.TrimRight(cola, "\r\n\t ")
	if i := bytes.LastIndexByte(cola, '\n'); i >= 0 {
		cola = cola[i+1:]
	}
	return sanearMotivo(string(cola))
}

// plazoVMMCaido es lo que se espera a que desaparezca el proceso del VMM
// tras cortarse su API: esperarVMM lo recoge en cuanto sale.
const plazoVMMCaido = 500 * time.Millisecond

// conexionCortada dice si err es la API del VMM cerrándose a media petición.
func conexionCortada(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE)
}

// vmmCaido explica un error de restauración que es solo la API del VMM
// cortada ("Put http://localhost/kling/forwards: EOF"): si el proceso pid ya
// no existe, lo dice, con lo último de su consola. Si sigue vivo, o el error
// es otro, lo devuelve tal cual.
func (m *Manager) vmmCaido(err error, id string, pid int) error {
	if err == nil || pid <= 0 || !conexionCortada(err) {
		return err
	}
	for fin := time.Now().Add(plazoVMMCaido); ; {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			break
		}
		if time.Now().After(fin) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	msg := "the VMM process exited during the restore (it crashed, or the system killed it for memory; the daemon log has its exit status)"
	if l := m.ultimaLineaConsola(id); l != "" {
		msg += "; last line of its console: " + l
	}
	return fmt.Errorf("%w: %s", err, msg)
}
