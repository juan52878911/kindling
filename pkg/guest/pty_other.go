//go:build !linux

package guest

// Fuera de Linux no hay invitado: el agente corre como PID 1 dentro de una
// microVM. Estos stubs existen para que el paquete siga compilando en el Mac de
// desarrollo, donde se ejecutan los tests que no tocan el PTY.

import (
	"errors"
	"os"
)

var errNoPTY = errors.New("pseudo-terminals are only available inside a Linux guest")

func openPTY(rows, cols uint16) (*os.File, *os.File, error) { return nil, nil, errNoPTY }
func resizePTY(master *os.File, rows, cols uint16) error    { return errNoPTY }
func foregroundPgrp(master *os.File) (int, error)           { return 0, errNoPTY }
func ptySupported() error                                   { return errNoPTY }
