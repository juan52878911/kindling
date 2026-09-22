package api

// Ejecución y ficheros dentro de una microVM, y sandboxes: lo que un agente de
// código necesita para correr lo que escribe sin tocar el host.
//
// Todo esto exige que la máquina se creara con AllowExec. Es una decisión que se
// toma al ARRANCAR, no después: la puerta es un parámetro de la línea de comandos
// del kernel que solo escribe el host, y se congela con la memoria. Una microVM de
// servicio no tiene las rutas registradas siquiera, y un snapshot sin AllowExec no
// puede dar máquinas con ella.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Límites de la ejecución. El daemon rechaza lo que se pase de los máximos y
// aplica los valores por defecto cuando la petición no dice nada.
const (
	ExecDefaultTimeout = 300 * time.Second
	ExecMaxTimeout     = 3600 * time.Second
	// Salida por flujo (stdout y stderr por separado). Lo que pase del tope se
	// descarta y el evento de salida lo marca con Truncated: el proceso sigue
	// corriendo, no se le corta la tubería.
	ExecDefaultOutput = 8 << 20
	ExecMaxOutput     = 64 << 20
	ExecMaxStdin      = 1 << 20

	FileMaxUpload   = 64 << 20
	FileMaxDownload = 256 << 20
)

// Valores de RunRequest.OnTTL.
const (
	OnTTLFreeze = "freeze" // por defecto: la máquina se congela y vuelve en ms
	OnTTLRemove = "remove" // la máquina se destruye: lo que quiere un sandbox
)

// LabelKind distingue para qué se creó una máquina. Los sandboxes llevan
// kind=sandbox; el resto no lo lleva.
const (
	LabelKind   = "kind"
	KindSandbox = "sandbox"
)

// ExecRequest es un comando para ejecutar dentro de la microVM. Cmd es argv, sin
// shell: quien quiera tuberías pasa ["sh", "-c", "..."].
type ExecRequest struct {
	Cmd []string `json:"cmd"`
	Dir string   `json:"dir,omitempty"`
	// Env se AÑADE al entorno del invitado (KEY=value).
	Env []string `json:"env,omitempty"`
	// Stdin viaja entero en la petición (en base64 dentro del JSON), hasta
	// ExecMaxStdin. Sin él, el comando lee de /dev/null.
	Stdin          []byte `json:"stdin,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty"`
}

// ExecEvent es una línea del flujo NDJSON de POST /machines/{ref}/exec.
//
// Llegan trozos de salida (Stream "stdout" o "stderr", con Data) mientras el
// comando corre, y al final exactamente uno de estos dos: un evento con Exit
// (terminó, con su código) o uno con Error (no se pudo ejecutar o se cortó).
type ExecEvent struct {
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`

	Exit       *int   `json:"exit,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ExecResult es el resultado agregado (?wait=1, o lo que devuelve Exec al
// terminar el flujo).
type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout,omitempty"`
	Stderr     []byte `json:"stderr,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}

// FileStat describe un fichero dentro de una microVM.
type FileStat struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"` // octal, p. ej. "0644"
	IsDir   bool      `json:"is_dir,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

// SandboxRequest crea un sandbox: una microVM de usar y tirar con ejecución
// encendida, sin red por defecto y que se destruye sola al vencer su TTL.
type SandboxRequest struct {
	Name string `json:"name,omitempty"`
	// Image o From (un snapshot creado con AllowExec). Si no se da ninguno se
	// usa la imagen por defecto del daemon.
	Image  string `json:"image,omitempty"`
	From   string `json:"from,omitempty"`
	VCPUs  int    `json:"vcpus,omitempty"`
	MemMiB int    `json:"mem_mib,omitempty"`
	// TTLSeconds: por defecto SandboxDefaultTTL. Vence y se destruye; renovarlo
	// es POST /sandboxes/{id}/renew.
	TTLSeconds   int                `json:"ttl_seconds,omitempty"`
	Egress       string             `json:"egress,omitempty"`
	AllowDomains []string           `json:"allow_domains,omitempty"`
	CPUPct       int                `json:"cpu_pct,omitempty"`
	Volumes      []VolumeAttachment `json:"volumes,omitempty"`
	Labels       map[string]string  `json:"labels,omitempty"`
}

// SandboxDefaultTTL y SandboxMaxTTL acotan la vida de un sandbox.
const (
	SandboxDefaultTTL = 600
	SandboxMaxTTL     = 24 * 3600
)

// RenewRequest alarga la vida de un sandbox: vence TTLSeconds a partir de ahora.
type RenewRequest struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// ExecError es el error de un flujo de exec que se cortó sin código de salida.
type ExecError struct{ Message string }

func (e *ExecError) Error() string { return e.Message }

// Exec ejecuta un comando dentro de la máquina y va pasando los trozos de salida
// a onOutput según llegan (stream es "stdout" o "stderr"). Devuelve el resultado
// final, sin la salida si hubo onOutput. Si onOutput es nil, la salida se
// acumula en el resultado.
//
// Un código de salida distinto de cero NO es un error: lo es no poder ejecutar,
// o que el flujo se corte.
func (c *Client) Exec(ctx context.Context, ref string, r ExecRequest, onOutput func(stream string, data []byte) error) (*ExecResult, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://kling/machines/"+url.PathEscape(ref)+"/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Cliente largo: la salida puede tardar minutos en llegar y no es un daemon
	// colgado.
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, statusErrorFrom(resp)
	}

	res := &ExecResult{}
	var stdout, stderr bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	// Un trozo son hasta 32 KiB de salida, que en base64 y con el sobre JSON
	// ronda los 44 KiB: el búfer por defecto del Scanner (64 KiB) ya da, pero se
	// deja margen para no depender de eso.
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev ExecEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("unreadable exec event: %w", err)
		}
		switch {
		case ev.Error != "":
			return nil, &ExecError{Message: ev.Error}
		case ev.Exit != nil:
			res.ExitCode = *ev.Exit
			res.DurationMS = ev.DurationMS
			res.Truncated = ev.Truncated
			res.TimedOut = ev.TimedOut
			if onOutput == nil {
				res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()
			}
			return res, nil
		case ev.Stream != "":
			if onOutput != nil {
				if err := onOutput(ev.Stream, ev.Data); err != nil {
					return nil, err
				}
			} else if ev.Stream == "stderr" {
				stderr.Write(ev.Data)
			} else {
				stdout.Write(ev.Data)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, &ExecError{Message: "the exec stream ended without an exit code (the machine died or the connection broke)"}
}

// ReadFile abre un fichero de dentro de la máquina. Quien llama cierra el
// lector.
func (c *Client) ReadFile(ctx context.Context, ref, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, machineFileURL(ref, path, nil), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, statusErrorFrom(resp)
	}
	return resp.Body, nil
}

// StatFile describe un fichero de dentro de la máquina.
func (c *Client) StatFile(ctx context.Context, ref, path string) (*FileStat, error) {
	var st FileStat
	u := machineFileURL(ref, path, url.Values{"stat": {"1"}})
	return &st, c.do(ctx, http.MethodGet, u[len("http://kling"):], nil, &st)
}

// WriteFile escribe r en path dentro de la máquina, con permisos mode (0 =
// 0644). mkdir crea los directorios que falten.
func (c *Client) WriteFile(ctx context.Context, ref, path string, mode uint32, mkdir bool, r io.Reader) (*FileStat, error) {
	q := url.Values{}
	if mode != 0 {
		q.Set("mode", "0"+strconv.FormatUint(uint64(mode), 8))
	}
	if mkdir {
		q.Set("mkdir", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, machineFileURL(ref, path, q), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, statusErrorFrom(resp)
	}
	var st FileStat
	return &st, json.NewDecoder(resp.Body).Decode(&st)
}

// RemoveFile borra un fichero (o un directorio vacío) de dentro de la máquina.
func (c *Client) RemoveFile(ctx context.Context, ref, path string) error {
	u := machineFileURL(ref, path, nil)
	return c.do(ctx, http.MethodDelete, u[len("http://kling"):], nil, nil)
}

// CreateSandbox crea un sandbox y devuelve su máquina, ya corriendo.
func (c *Client) CreateSandbox(ctx context.Context, r SandboxRequest) (*Machine, error) {
	var m Machine
	return &m, c.doWith(c.long, ctx, http.MethodPost, "/sandboxes", r, &m)
}

// Sandboxes lista los sandboxes vivos.
func (c *Client) Sandboxes(ctx context.Context) ([]*Machine, error) {
	var out []*Machine
	return out, c.do(ctx, http.MethodGet, "/sandboxes", nil, &out)
}

// RenewSandbox alarga la vida de un sandbox.
func (c *Client) RenewSandbox(ctx context.Context, ref string, ttlSeconds int) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/renew", RenewRequest{TTLSeconds: ttlSeconds}, &m)
}

// RemoveSandbox destruye un sandbox.
func (c *Client) RemoveSandbox(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(ref), nil, nil)
}

func machineFileURL(ref, path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("path", path)
	return "http://kling/machines/" + url.PathEscape(ref) + "/files?" + q.Encode()
}

func statusErrorFrom(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e Error
	if json.Unmarshal(b, &e) != nil || e.Message == "" {
		e.Message = resp.Status
	}
	return &StatusError{Code: resp.StatusCode, Message: e.Message}
}

// Limits valida la petición y devuelve el plazo y el tope de salida efectivos,
// con los valores por defecto aplicados. La usan el daemon y el invitado: el
// invitado no se fía de que quien llama sea el daemon.
func (r ExecRequest) Limits() (time.Duration, int, error) {
	if len(r.Cmd) == 0 || r.Cmd[0] == "" {
		return 0, 0, fmt.Errorf("missing cmd")
	}
	if r.TimeoutSeconds < 0 || r.MaxOutputBytes < 0 {
		return 0, 0, fmt.Errorf("timeout and max_output_bytes can't be negative")
	}
	timeout := ExecDefaultTimeout
	if r.TimeoutSeconds > 0 {
		timeout = time.Duration(r.TimeoutSeconds) * time.Second
	}
	if timeout > ExecMaxTimeout {
		return 0, 0, fmt.Errorf("timeout %s is over the %s maximum", timeout, ExecMaxTimeout)
	}
	maxOut := ExecDefaultOutput
	if r.MaxOutputBytes > 0 {
		maxOut = r.MaxOutputBytes
	}
	if maxOut > ExecMaxOutput {
		return 0, 0, fmt.Errorf("max_output_bytes %d is over the %d maximum", maxOut, ExecMaxOutput)
	}
	if len(r.Stdin) > ExecMaxStdin {
		return 0, 0, fmt.Errorf("stdin is %d bytes; the maximum is %d", len(r.Stdin), ExecMaxStdin)
	}
	return timeout, maxOut, nil
}
