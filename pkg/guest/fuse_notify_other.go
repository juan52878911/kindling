//go:build !linux

package guest

// Fuera de Linux no hay montaje: el protocolo se prueba con un /dev/fuse falso
// y sin operaciones reales sobre el montaje.
func lockThread()    {}
func gettid() uint32 { return 0 }

var touchInGuest func(op synthOp, full string)
