//go:build unix

package main

import (
	"os"
	"syscall"
)

// detached pone el hijo en su propia sesión: sin terminal de control, las
// señales de la del usuario (Ctrl-C, cerrar la ventana) no le llegan.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func openNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
}
