package main

// El trabajo de segundo plano de kling db branch -switch: otro kling-db,
// desligado de la terminal y del gancho de git, que escribe en un registro.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// backgroundLog es el registro del trabajo de segundo plano, en el estado de
// kling-db (0700). Pasado maxBackgroundLog se empieza de cero.
const (
	backgroundLog    = "branch-background.log"
	maxBackgroundLog = 1 << 20
)

// spawnBackground lanza este mismo binario con args y no lo espera. Sus
// salidas van al registro y no a las del padre: si heredara el stderr del
// gancho, quien lee ese stderr hasta EOF (un IDE, un git con tubería) esperaría
// al proceso de fondo, que es justo lo que se quiere evitar. Tampoco hereda la
// sesión: un Ctrl-C en la terminal no lo corta a medias.
func spawnBackground(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root, err := dbstate.Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	logf, err := openBackgroundLog(filepath.Join(root, backgroundLog))
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	c := exec.Command(exe, args...)
	c.Stdin, c.Stdout, c.Stderr = devnull, logf, logf
	c.SysProcAttr = detached()
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// openBackgroundLog abre el registro para añadir (0600, sin seguir un enlace
// que alguien hubiera puesto en su lugar), vaciándolo si creció demasiado.
func openBackgroundLog(p string) (*os.File, error) {
	f, err := openNoFollow(p)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New(p + " is not a regular file")
	}
	if st.Size() > maxBackgroundLog {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	fmt.Fprintf(f, "--- %s\n", procStart.UTC().Format("2006-01-02T15:04:05Z"))
	return f, nil
}
