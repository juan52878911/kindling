package machine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// ErrNoDebugfs dice que no hay debugfs en este host. Va aparte porque debugfs
// solo sirve para INSPECCIONAR imágenes (¿lleva agente?, ¿entiende capas?,
// images cat): en un Mac sin e2fsprogs de Homebrew arrancar, congelar,
// descongelar y exec funcionan igual, y quien pregunta por diagnóstico puede
// seguir sin la respuesta en vez de negarse a arrancar.
var ErrNoDebugfs = errors.New("cannot find debugfs (comes with e2fsprogs)")

// debugfsBin localiza debugfs, que en Debian vive en /sbin y no siempre está en
// el PATH de un servicio de systemd, y en macOS en el prefijo keg-only de
// Homebrew.
func debugfsBin() string {
	return buscarE2fs("debugfs")
}

// maxSalidaDebugfs es lo que se acepta de la salida de una orden de debugfs
// que se analiza como texto (stat, stats): sobra con mucho, y sin tope un
// cat sobre la imagen equivocada cargaba el fichero entero en el daemon.
const maxSalidaDebugfs = 1 << 20

// errSalidaGrande dice que la salida de debugfs pasó del tope pedido.
var errSalidaGrande = errors.New("debugfs output exceeds the limit")

// salidaAcotada es c.Output() leyendo como mucho max bytes: si hay más, mata
// el proceso y devuelve errSalidaGrande, sin haber cargado el resto.
func salidaAcotada(c *exec.Cmd, max int64) ([]byte, error) {
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	if c.Stderr == nil {
		c.Stderr = &stderr
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(out, max+1))
	if int64(len(b)) > max {
		_ = c.Process.Kill()
		_ = c.Wait()
		return nil, fmt.Errorf("%w (%d bytes)", errSalidaGrande, max)
	}
	if err := c.Wait(); err != nil {
		if s := bytes.TrimSpace(stderr.Bytes()); len(s) > 0 {
			return nil, fmt.Errorf("%w: %s", err, s)
		}
		return nil, err
	}
	return b, rerr
}
