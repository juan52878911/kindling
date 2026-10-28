//go:build !linux && !darwin

package share

import "os"

// Sin kqueue ni inotify: OpWatch contesta ENOSYS y la carpeta se queda con
// las cachés de 1 s.
func newWatchBackend(*os.Root, func(rawEvent)) (watchBackend, error) { return nil, errNoWatch }
