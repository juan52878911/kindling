//go:build darwin

package machine

import (
	"context"
	"os/exec"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
)

// prioDarwinProcess y prioDarwinBG son PRIO_DARWIN_PROCESS y PRIO_DARWIN_BG
// de <sys/resource.h>: lo que hace taskpolicy -b. Vale sobre otro proceso del
// mismo usuario (también el auxiliar de Apple), y se deshace con 0.
const (
	prioDarwinProcess = 4
	prioDarwinBG      = 0x1000
)

// qosProceso pone (baja) o quita la QoS de segundo plano de pid: núcleos de
// eficiencia, E/S y red en segundo plano.
func qosProceso(pid int, baja bool) error {
	v := 0
	if baja {
		v = prioDarwinBG
	}
	return syscall.Setpriority(prioDarwinProcess, pid, v)
}

// enBateriaSO dice si el Mac va con batería, según pmset. Sin cgo no hay
// IOPSCopyPowerSourcesInfo; un proceso por minuto como mucho (fuenteCache) no
// se nota, y solo con daemon.battery_mode en auto.
func enBateriaSO() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "ps").Output()
	if err != nil {
		return false, err
	}
	return fuenteEsBateria(string(out)), nil
}

// cpuInvitado da la CPU que lleva gastada la máquina del socket sock (ms) y el
// pid de su auxiliar de Apple, preguntando a su kling-vz.
func cpuInvitado(ctx context.Context, sock string) (int64, int, error) {
	e, err := fc.New(sock).KlingCPUState(ctx)
	if err != nil {
		return -1, 0, err
	}
	return e.CPUMS, e.HelperPID, nil
}
