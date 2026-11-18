package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// cierreQueFalla es un fichero cuyo Close falla, como el de un disco lleno o
// una carpeta de red al vaciar lo pendiente.
type cierreQueFalla struct {
	bytes.Buffer
	cerrado bool
}

func (c *cierreQueFalla) Close() error {
	c.cerrado = true
	return errors.New("no space left on device")
}

// F18: `kling cp` de la máquina a un fichero comprueba el Close.
func TestVolcarYCerrarCuentaElClose(t *testing.T) {
	w := &cierreQueFalla{}
	n, err := volcarYCerrar(w, strings.NewReader("hola"))
	if err == nil || !w.cerrado || n != 4 {
		t.Fatalf("n=%d err=%v cerrado=%v", n, err, w.cerrado)
	}
}
