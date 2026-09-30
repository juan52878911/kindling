package askllm

// Proveedor "opencode": habla con MiniMax (u otro modelo que opencode tenga
// configurado) a través del CLI de opencode, para quien ya lo tiene con su
// cuenta y no quiere una clave de Anthropic.
//
// Cómo se encierra el CLI:
//   - --pure: sin plugins externos.
//   - directorio temporal NUEVO y vacío (0700) como cwd, borrado al acabar:
//     opencode no ve ni el proyecto ni nada del usuario por ahí.
//   - NUNCA --auto: si opencode pidiera un permiso, nadie lo concede.
//   - el prompt (esquema y pregunta) va SIEMPRE por stdin: `opencode run` lee
//     de ahí el mensaje cuando no es un terminal. Por argumento lo vería con ps
//     cualquier usuario del equipo, y un fichero adjunto hace que el modelo a
//     veces intente la herramienta de lectura, lo que aborta.
//   - solo se aceptan eventos text y step_start/step_finish; cualquier otro
//     (uso de herramientas, permisos, errores) aborta y mata al proceso.
//   - plazo por contexto, matando el GRUPO de procesos entero.
//
// El entorno del hijo es el del usuario (opencode necesita su config y sus
// credenciales de MiniMax): no se filtra a propósito. opencode guarda además
// la sesión (el prompt y la respuesta) en su propio almacén local, como con
// cualquier uso suyo; este paquete no lo puede evitar.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultOpenCodeModel es el modelo de opencode si no se pide otro con -model.
	DefaultOpenCodeModel = "minimax-coding-plan/MiniMax-M2.7"
	// DefaultLLMTimeout es el plazo de una petición al modelo.
	DefaultLLMTimeout = 90 * time.Second

	maxOpenCodeOut  = 4 << 20
	maxOpenCodeLine = 1 << 20
)

var errNoFinish = errors.New("opencode's output ended without a finished step")

// OpenCode usa el CLI de opencode.
type OpenCode struct {
	Bin     string
	Model   string
	Timeout time.Duration
}

// FindOpenCode busca el binario: en PATH y, si no, en ~/.opencode/bin.
func FindOpenCode() (string, bool) {
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".opencode", "bin", "opencode")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// NewOpenCode con el binario dado. model vacío: DefaultOpenCodeModel; timeout <= 0: 90 s.
func NewOpenCode(bin, model string, timeout time.Duration) *OpenCode {
	if model == "" {
		model = DefaultOpenCodeModel
	}
	if timeout <= 0 {
		timeout = DefaultLLMTimeout
	}
	return &OpenCode{Bin: bin, Model: model, Timeout: timeout}
}

// Name implementa Provider.
func (o *OpenCode) Name() string { return "opencode (" + o.Model + ")" }

// openEvent es lo poco que se lee de cada línea JSON.
type openEvent struct {
	Type string `json:"type"`
	Part struct {
		Text   string `json:"text"`
		Reason string `json:"reason"`
	} `json:"part"`
	Error json.RawMessage `json:"error"`
}

// providerError es un fallo del proveedor que opencode informa con un evento
// "error" (caída o límite de la API, plazo del proveedor...). Es lo único que
// se reintenta: una herramienta o un permiso abortan sin más.
type providerError struct{ msg string }

func (e *providerError) Error() string {
	return "the model provider failed (reported by opencode): " + e.msg
}

// Complete implementa Provider.
func (o *OpenCode) Complete(ctx context.Context, system, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	text, err := o.attempt(ctx, system, prompt)
	var pe *providerError
	if errors.As(err, &pe) && ctx.Err() == nil {
		// Fallos del proveedor intermitentes (visto con MiniMax): un reintento,
		// dentro del mismo plazo total.
		text, err = o.attempt(ctx, system, prompt)
	}
	return text, err
}

// attempt es una ejecución de opencode.
func (o *OpenCode) attempt(ctx context.Context, system, prompt string) (string, error) {
	dir, err := os.MkdirTemp("", "kling-db-ask-")
	if err != nil {
		return "", fmt.Errorf("creating the scratch directory: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	full := system + "\n\n" + prompt
	// Sin mensaje en los argumentos: opencode lo lee de stdin (ver arriba).
	cmd := exec.CommandContext(ctx, o.Bin, "run", "--pure", "-m", o.Model, "--format", "json")
	cmd.Stdin = strings.NewReader(full)
	cmd.Dir = dir
	setProcessGroup(cmd)
	var stderr limitBuf
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting opencode: %w", err)
	}
	text, perr := parseOpenCode(stdout)
	if perr != nil {
		killGroup(cmd) // no seguir gastando ni dejar huérfanos
	}
	// Vaciar lo que quede para que Wait no se cuelgue con la tubería llena.
	_, _ = io.Copy(io.Discard, io.LimitReader(stdout, maxOpenCodeOut))
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return "", fmt.Errorf("opencode did not answer within %s (killed)", o.Timeout)
	}
	if perr != nil {
		if errors.Is(perr, errNoFinish) && werr != nil {
			return "", fmt.Errorf("opencode failed: %v%s", werr, stderr.hint())
		}
		return "", perr
	}
	if werr != nil {
		return "", fmt.Errorf("opencode failed: %v%s", werr, stderr.hint())
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("opencode returned no text")
	}
	return text, nil
}

// parseOpenCode lee el JSON por líneas de --format json y devuelve el texto.
func parseOpenCode(r io.Reader) (string, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxOpenCodeOut))
	sc.Buffer(make([]byte, 0, 64<<10), maxOpenCodeLine)
	var sb strings.Builder
	finished := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev openEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return "", errors.New("opencode's output is not the expected JSON events")
		}
		switch ev.Type {
		case "text":
			sb.WriteString(ev.Part.Text)
		case "step_start":
		case "step_finish":
			finished = true
			if r := ev.Part.Reason; r != "" && r != "stop" {
				return "", fmt.Errorf("opencode stopped with reason %q instead of an answer", clip(r, 40))
			}
		case "error":
			return "", &providerError{msg: clip(strings.Join(strings.Fields(errorText(ev.Error)), " "), 300)}
		default:
			// tool_use, permisos...: el modelo no debe usar nada.
			return "", fmt.Errorf("opencode sent an unexpected %q event (tools and permissions are not allowed): aborted", clip(ev.Type, 40))
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading opencode's output: %w", err)
	}
	if !finished {
		return "", errNoFinish
	}
	return sb.String(), nil
}

// limitBuf guarda solo el principio de stderr, para los mensajes de error.
type limitBuf struct{ b bytes.Buffer }

func (l *limitBuf) Write(p []byte) (int, error) {
	if room := 2048 - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (l *limitBuf) hint() string {
	s := strings.TrimSpace(l.b.String())
	if s == "" {
		return ""
	}
	return ": " + clip(strings.Join(strings.Fields(s), " "), 300)
}

// errorText saca un mensaje legible del campo "error" de un evento de opencode
// (un texto, o un objeto con data.message/message/name), sin caracteres de control.
func errorText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "no details"
	}
	var str string
	if json.Unmarshal(raw, &str) == nil && str != "" {
		return sinControl(str)
	}
	var obj struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		for _, m := range []string{obj.Data.Message, obj.Message, obj.Name} {
			if m != "" {
				return sinControl(m)
			}
		}
	}
	return "no details"
}

func sinControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
