//go:build darwin

package machine

import "syscall"

// nivelPresionSO lee kern.memorystatus_vm_pressure_level: el nivel con el que
// macOS avisa a las aplicaciones (DISPATCH_SOURCE_TYPE_MEMORYPRESSURE) antes de
// que jetsam empiece a matar. Un sysctl sin cgo; la notificación de dispatch
// necesitaría cgo, y una vuelta del vigilante cada 10 s llega de sobra.
func nivelPresionSO() (int, error) {
	v, err := syscall.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return 0, err
	}
	return int(v), nil
}
