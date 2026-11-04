package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// ExitError pide un código de salida concreto. Una extensión en Go lo devuelve
// desde su comando (p. ej. el 3 de `mcp heal`, que systemd interpreta) y Main lo
// convierte en os.Exit.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}
func (e *ExitError) Unwrap() error { return e.Err }

// Códigos de salida que una extensión comparte con el núcleo (cmd/kling,
// codigoDeSalida; docs/guide.md, "Exit codes"). Son contrato: un script que
// llama a `kling mcp …` los distingue igual que con `kling run`. Una extensión
// que quiera un código propio usa uno que no choque (6-125).
const (
	ExitFailure     = 1   // cualquier otro error
	ExitUsage       = 2   // comando, flag o argumentos que no valen
	ExitDaemonOld   = 3   // el daemon habla un API más viejo (api.ErrDaemonTooOld)
	ExitStaleGolden = 4   // la plantilla es de otro VMM o versión: rehacerla
	ExitInterrupted = 130 // Ctrl-C o SIGTERM (128 + SIGINT, como una shell)
)

// ExitCode es el código de salida que corresponde a err: 0 sin error, el de un
// ExitError, y si no el del contrato del núcleo (daemon viejo, dorado
// obsoleto, uso) o 1. Así una extensión externa sale con el mismo código que
// tendría incorporada, donde lo decide el núcleo.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	var viejo *api.ErrDaemonTooOld
	switch {
	case errors.As(err, &viejo):
		return ExitDaemonOld
	case api.IsStaleGolden(err):
		return ExitStaleGolden
	case strings.HasPrefix(err.Error(), "usage: "):
		return ExitUsage
	}
	return ExitFailure
}

// Exec pasa el control a la extensión para ejecutar cmd: una incorporada corre
// en este proceso; una externa REEMPLAZA este proceso (exec), así el código de
// salida, las señales y la terminal son los suyos sin nada en medio. Solo vuelve
// si algo falla antes de ejecutarla.
func Exec(p *Plugin, cmd string, args []string, configPath string) error {
	if p.Err != nil {
		return fmt.Errorf("extension %s is not usable: %v", p.Name, p.Err)
	}
	if p.Builtin != nil {
		f := p.Builtin.Commands[cmd]
		if f == nil {
			return fmt.Errorf("extension %s declares %q but does not implement it", p.Name, cmd)
		}
		return f(args)
	}
	argv := append([]string{p.Path, cmd}, args...)
	err := syscall.Exec(p.Path, argv, Env(configPath))
	// exec solo vuelve si falló. Se intenta como hijo, que es lo que hacen las
	// plataformas sin exec.
	code, rerr := RunChild(p, cmd, args, configPath)
	if rerr != nil {
		return fmt.Errorf("could not run %s: %v (exec: %v)", p.Path, rerr, err)
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// RunChild ejecuta el comando de una extensión externa como proceso hijo, con
// la terminal de este, y devuelve su código de salida.
func RunChild(p *Plugin, cmd string, args []string, configPath string) (int, error) {
	c := exec.Command(p.Path, append([]string{cmd}, args...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = Env(configPath)
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// Muerta por una señal, ExitCode da -1 y os.Exit(-1) saldría con 255:
		// se devuelve 128+señal, lo que diría una shell (y lo que se vería si
		// el exec hubiera funcionado).
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// RunHook invoca el gancho h de la extensión y devuelve lo que escribió. Tiene
// plazo: un gancho que tarda o falla devuelve error, y quien llama lo muestra
// como aviso sin romper su propia salida.
func RunHook(ctx context.Context, p *Plugin, h string, args []string, configPath string) ([]byte, error) {
	if p.Builtin != nil {
		f := p.Builtin.Hooks[h]
		if f == nil {
			return nil, fmt.Errorf("declares hook %q but does not implement it", h)
		}
		var buf bytes.Buffer
		err := f(args, &buf)
		return buf.Bytes(), err
	}
	ctx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()
	// Con tope, como --kling-manifest: lo que un gancho escribe se guarda en
	// memoria. De stderr solo se muestran 200 bytes.
	out := limitedBuffer{max: maxHookBytes}
	errb := limitedBuffer{max: 4 << 10}
	c := exec.CommandContext(ctx, p.Path, append([]string{"--kling-hook", h}, args...)...)
	c.Stdout, c.Stderr = &out, &errb
	c.Env = Env(configPath)
	c.WaitDelay = waitDelay // ver loadManifest
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return out.Bytes(), fmt.Errorf("hook %s took more than %s", h, HookTimeout)
		}
		msg := bytes.TrimSpace(errb.Bytes())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return out.Bytes(), fmt.Errorf("hook %s failed: %v %s", h, err, msg)
	}
	if out.over {
		return out.Bytes(), fmt.Errorf("hook %s printed more than %d bytes", h, maxHookBytes)
	}
	return out.Bytes(), nil
}

// maxHookBytes es el tope de la salida de un gancho.
const maxHookBytes = 1 << 20
