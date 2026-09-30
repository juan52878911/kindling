// Package klingc es cómo ext/db habla con kindling: ejecutando el binario
// `kling`, igual que haría una persona. Lo comparten el CLI (`kling db`), el
// doctor y la auditoría, y en los tests se sustituye por uno falso que apunta
// cada llamada.
//
// Por qué el CLI y no pkg/api: `kling exec -i` ya resuelve el transporte (socket
// local o ssh://), los plazos y el tope de stdin, y lo que se ejecuta es
// exactamente lo que alguien podría repetir a mano para depurar. La única
// excepción es reetiquetar una máquina, que el CLI del núcleo no ofrece: va por
// pkg/api (ver Labeler).
package klingc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// Kling ejecuta `kling <args...>` con stdin (puede ser nil) y devuelve su
// salida estándar. Un código de salida distinto de 0 es un error.
//
// Regla para quien lo llama: NADA secreto en args. Lo secreto viaja por stdin.
type Kling interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout []byte, err error)
}

// Labeler reetiqueta una máquina (fusiona: las claves dadas pisan las que
// hubiera). El CLI del núcleo no tiene un comando para esto.
type Labeler interface {
	SetLabels(ctx context.Context, ref string, labels map[string]string) error
}

// Credentialer entrega y retira credenciales del proxy de credenciales de una
// máquina (kling db attach/detach). Va por pkg/api y no por `kling machine
// credential` para que la contraseña viaje en el cuerpo de la petición al
// daemon, nunca en argv ni en un fichero, y porque el CLI del núcleo no expone
// upstream_machine.
type Credentialer interface {
	SetCredential(ctx context.Context, ref string, spec api.CredentialSpec) error
	RemoveCredential(ctx context.Context, ref, env, upstreamMachine string) error
}

// Machiner lista y descongela máquinas por el API del daemon, sin un proceso
// de kling por llamada. Lo usa el camino de cada checkout de kling db branch
// (el gancho de git): lanzar `kling` cuesta ~5 ms cada vez, y Thaw devuelve la
// máquina ya al día, sin el inspect de después. Es lo mismo que `kling ps
// -json` y `kling thaw`.
type Machiner interface {
	List(ctx context.Context) ([]*api.Machine, error)
	Thaw(ctx context.Context, ref string) (*api.Machine, error)
}

// CLI es la implementación de verdad.
type CLI struct {
	// Bin es el ejecutable de kling. Vacío: Resolve.
	Bin string
	// Host es el -H del usuario. Vacío: lo que diga el entorno (KLING_HOST,
	// contexto activo o socket local), con la misma precedencia que kling.
	Host string
	// Stderr, si no es nil, recibe la salida de error del hijo además de
	// guardarse para el mensaje de error.
	Stderr io.Writer
}

// New devuelve un CLI con el binario resuelto.
func New(host string) (*CLI, error) {
	bin, err := Resolve()
	if err != nil {
		return nil, err
	}
	return &CLI{Bin: bin, Host: host}, nil
}

// Resolve encuentra el binario de kling: $KLING (una ruta, sin banderas), luego
// $KLING_BIN (lo exporta kling al ejecutar una extensión) y luego el PATH.
func Resolve() (string, error) {
	for _, env := range []string{"KLING", "KLING_BIN"} {
		if v := os.Getenv(env); v != "" {
			if strings.ContainsAny(v, " \t") {
				if _, err := os.Stat(v); err != nil {
					return "", fmt.Errorf("$%s must be the path of the kling binary (flags go in -H or KLING_HOST): %q", env, v)
				}
			}
			return v, nil
		}
	}
	p, err := exec.LookPath("kling")
	if err != nil {
		return "", errors.New("kling not found: install it or set $KLING to its path")
	}
	return p, nil
}

// Error es un `kling` que salió mal. Stderr es la cola de su salida de error:
// quien sepa que puede citar algo sensible no la enseña (ver Detail).
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	cmd := "kling"
	if len(e.Args) > 0 {
		cmd += " " + e.Args[0]
	}
	if e.Stderr != "" {
		return fmt.Sprintf("%s: %v: %s", cmd, e.Err, e.Stderr)
	}
	return fmt.Sprintf("%s: %v", cmd, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// maxStderr es cuánto de la salida de error se guarda para el mensaje.
const maxStderr = 2048

// Run implementa Kling.
func (c *CLI) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdin = stdin
	cmd.Env = c.env()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&errb, c.Stderr)
	} else {
		cmd.Stderr = &errb
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > maxStderr {
			msg = "..." + msg[len(msg)-maxStderr:]
		}
		return out.Bytes(), &Error{Args: append([]string(nil), args...), Stderr: msg, Err: err}
	}
	return out.Bytes(), nil
}

// Command es `kling <args...>` sin ejecutar, con el mismo binario y el mismo
// daemon que Run, para quien necesite conectarle la terminal (kling shell).
// Nada secreto en args, como en Run.
func (c *CLI) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = c.env()
	return cmd
}

// env es el entorno del hijo. -H se traduce a KLING_HOST: kling lo lee con la
// misma precedencia que el flag salvo que otro -H lo pise, y así no hay que
// saber dónde va el flag en cada subcomando (`sandbox fork`, `exec -i`...).
func (c *CLI) env() []string {
	env := os.Environ()
	if c.Host == "" {
		return env
	}
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "KLING_HOST=") {
			out = append(out, kv)
		}
	}
	return append(out, "KLING_HOST="+c.Host)
}

// client es el cliente del API del daemon, resuelto igual que kling (-H >
// KLING_HOST > contexto > socket local).
func (c *CLI) client() *api.Client {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	return api.NewClient(cfg.Host(c.Host))
}

// List implementa Machiner.
func (c *CLI) List(ctx context.Context) ([]*api.Machine, error) {
	return c.client().List(ctx)
}

// Thaw implementa Machiner.
func (c *CLI) Thaw(ctx context.Context, ref string) (*api.Machine, error) {
	return c.client().Thaw(ctx, ref)
}

// SetLabels implementa Labeler contra el API del daemon.
func (c *CLI) SetLabels(ctx context.Context, ref string, labels map[string]string) error {
	return c.client().SetLabels(ctx, ref, labels)
}

// SetCredential implementa Credentialer: una credencial más para la máquina
// (se fusiona por variable con las que tenga).
func (c *CLI) SetCredential(ctx context.Context, ref string, spec api.CredentialSpec) error {
	_, err := c.client().SetCredentials(ctx, ref, api.CredentialsRequest{Credentials: []api.CredentialSpec{spec}})
	return err
}

// RemoveCredential implementa Credentialer.
func (c *CLI) RemoveCredential(ctx context.Context, ref, env, upstreamMachine string) error {
	_, err := c.client().RemoveCredential(ctx, ref, env, upstreamMachine)
	return err
}
