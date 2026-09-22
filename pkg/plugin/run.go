package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
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

// ExitCode es el código de salida que corresponde a err: 0 sin error, el de un
// ExitError, o 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return 1
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
	var out, errb bytes.Buffer
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
	return out.Bytes(), nil
}
