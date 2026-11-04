//go:build !darwin && !linux

package machine

// vigilarSalida: aquí no se sabe esperar a un proceso ajeno; queda el
// vigilante (sweep).
func vigilarSalida(pid int, fn func()) bool { return false }
