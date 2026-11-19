package api

// Procesos en segundo plano y sesiones con estado, dentro de una microVM con
// AllowExec.
//
// A diferencia de POST /machines/{ref}/exec, un proceso de fondo NO está atado
// a la conexión que lo lanzó: sigue corriendo cuando quien lo pidió se va, y
// su salida se guarda en un anillo acotado dentro del invitado para leerla
// después (desde un desplazamiento) o seguirla en vivo. Vive en el agente del
// invitado, así que una máquina congelada y descongelada lo conserva; un
// reinicio, un stop/start o una copia de un snapshot, no.
//
// Una sesión es un cwd y un entorno con nombre que persisten entre execs:
// ExecRequest.Session la usa y, al acabar el comando, la actualiza con el
// directorio y el entorno en que terminó. También vive en el invitado.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Capacidades que anuncia el daemon (GET /version) para estas rutas.
const (
	CapabilityProcesses = "processes"
	CapabilitySessions  = "sessions"
)

// Rutas del agente del invitado (y del daemon, con el prefijo
// /machines/{ref}). Terminan en "/" las que cuelgan subrutas.
const (
	GuestProcessesPath = "/processes"
	GuestSessionsPath  = "/sessions"
)

// Límites. El anillo y las tablas están acotados dentro del invitado: un
// proceso que escupe gigas o un bucle que lanza miles no pueden con PID 1.
const (
	// ProcessMaxRunning son los procesos de fondo vivos a la vez por máquina.
	ProcessMaxRunning = 32
	// ProcessMaxEntries son los procesos que recuerda la tabla (vivos más
	// terminados); al llenarse se olvida el terminado más antiguo.
	ProcessMaxEntries = 64
	// Salida retenida por proceso (stdout y stderr juntos). Lo que se sale del
	// anillo se pierde, y ProcessEvent.Dropped lo dice.
	ProcessDefaultOutput = 1 << 20
	ProcessMaxOutput     = 16 << 20

	SessionMax        = 64
	SessionDefaultTTL = 3600
	SessionMaxTTL     = 24 * 3600
	// SessionMaxEnv acota el entorno guardado de una sesión.
	SessionMaxEnv = 256 << 10
)

// Estados de ProcessInfo.State.
const (
	ProcessRunning = "running"
	ProcessExited  = "exited"
)

// ProcessStartRequest lanza un comando en segundo plano (POST
// /machines/{ref}/processes). Cmd es argv, sin shell.
type ProcessStartRequest struct {
	Cmd []string `json:"cmd"`
	Dir string   `json:"dir,omitempty"`
	// Env se AÑADE al entorno del invitado (KEY=value).
	Env []string `json:"env,omitempty"`
	// User como en ExecRequest.
	User string `json:"user,omitempty"`
	// Session aplica el cwd y el entorno de la sesión (y Dir/Env encima). Un
	// proceso de fondo NO la modifica: nunca "termina" mientras se le espera.
	Session string `json:"session,omitempty"`
	// TimeoutSeconds, si es >0, mata el proceso pasado ese tiempo. Por
	// defecto no hay plazo: un servidor de fondo vive hasta que se le señale.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// MaxOutputBytes es el anillo de salida (por defecto ProcessDefaultOutput).
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

// ProcessInfo es el estado de un proceso de fondo.
type ProcessInfo struct {
	// PID es el del invitado, y la clave de las demás rutas.
	PID       int       `json:"pid"`
	Cmd       []string  `json:"cmd"`
	Dir       string    `json:"dir,omitempty"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	// EndedAt, ExitCode y Signal solo con State "exited". Signal es la señal
	// que lo mató ("KILL", "TERM"…), y ExitCode entonces es 128+n como en una
	// shell.
	EndedAt  *time.Time `json:"ended_at,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Signal   string     `json:"signal,omitempty"`
	TimedOut bool       `json:"timed_out,omitempty"`
	// OutputBytes es la salida total producida hasta ahora; OutputStart el
	// desplazamiento del primer byte que aún se conserva (>0 si el anillo
	// descartó lo más viejo).
	OutputBytes int64 `json:"output_bytes"`
	OutputStart int64 `json:"output_start,omitempty"`
}

// ProcessEvent es una línea del flujo NDJSON de GET .../processes/{pid}/output.
//
// Trozos de salida (Stream "stdout"/"stderr", con Data); Offset es el
// desplazamiento SIGUIENTE al trozo, el que se pasa como ?offset= para
// continuar. Dropped, en el primero, son los bytes que pedía el cliente y el
// anillo ya había descartado. Con ?follow=1 el flujo acaba con un evento Exit
// cuando el proceso termina; los eventos vacíos con solo Offset son latidos.
type ProcessEvent struct {
	Stream  string `json:"stream,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Offset  int64  `json:"offset"`
	Dropped int64  `json:"dropped,omitempty"`

	Exit   *int   `json:"exit,omitempty"`
	Signal string `json:"signal,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ProcessSignalRequest es el cuerpo de POST .../processes/{pid}/signal.
// Signal es un nombre ("TERM", "SIGKILL", "INT", "HUP", "USR1"…) o un número;
// vacío = TERM. La señal va al GRUPO del proceso, con sus hijos.
type ProcessSignalRequest struct {
	Signal string `json:"signal,omitempty"`
}

// SessionRequest crea una sesión (POST /machines/{ref}/sessions).
type SessionRequest struct {
	// ID, si se da (letras, cifras, ._-, hasta 64); si no, el agente lo genera.
	ID  string   `json:"id,omitempty"`
	Dir string   `json:"dir,omitempty"`
	Env []string `json:"env,omitempty"`
	// TTLSeconds es la inactividad tras la que caduca (por defecto
	// SessionDefaultTTL, tope SessionMaxTTL). Cada exec lo renueva.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// Session es el estado de una sesión.
type Session struct {
	ID string `json:"id"`
	// Dir es el cwd con el que acabó el último exec ("" = el del agente).
	Dir string `json:"dir,omitempty"`
	// Env son las variables que la sesión AÑADE o cambia respecto al entorno
	// base del invitado (KEY=value); Unset, las que quitó.
	Env       []string  `json:"env,omitempty"`
	Unset     []string  `json:"unset,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Execs     int       `json:"execs"`
}

func procPath(ref string, pid int, sub string) string {
	p := "/machines/" + url.PathEscape(ref) + "/processes"
	if pid > 0 {
		p += "/" + strconv.Itoa(pid)
	}
	return p + sub
}

// StartProcess lanza un comando en segundo plano y devuelve su pid.
func (c *Client) StartProcess(ctx context.Context, ref string, r ProcessStartRequest) (*ProcessInfo, error) {
	var p ProcessInfo
	return &p, c.do(ctx, http.MethodPost, procPath(ref, 0, ""), r, &p)
}

// Processes lista los procesos de fondo de la máquina.
func (c *Client) Processes(ctx context.Context, ref string) ([]ProcessInfo, error) {
	var out []ProcessInfo
	return out, c.do(ctx, http.MethodGet, procPath(ref, 0, ""), nil, &out)
}

// Process da el estado de un proceso. Con wait>0 espera hasta ese tiempo a que
// termine (si no termina, devuelve el estado "running").
func (c *Client) Process(ctx context.Context, ref string, pid int, wait time.Duration) (*ProcessInfo, error) {
	path := procPath(ref, pid, "")
	var p ProcessInfo
	if wait > 0 {
		path += "?wait=" + strconv.Itoa(int((wait+time.Second-1)/time.Second))
		return &p, c.doWith(c.long, ctx, http.MethodGet, path, nil, &p)
	}
	return &p, c.do(ctx, http.MethodGet, path, nil, &p)
}

// SignalProcess manda una señal al grupo del proceso (vacía = TERM).
func (c *Client) SignalProcess(ctx context.Context, ref string, pid int, signal string) (*ProcessInfo, error) {
	var p ProcessInfo
	return &p, c.do(ctx, http.MethodPost, procPath(ref, pid, "/signal"), ProcessSignalRequest{Signal: signal}, &p)
}

// RemoveProcess mata el proceso si sigue vivo y lo olvida, con su salida.
func (c *Client) RemoveProcess(ctx context.Context, ref string, pid int) error {
	return c.do(ctx, http.MethodDelete, procPath(ref, pid, ""), nil, nil)
}

// ProcessOutput lee la salida del proceso desde offset y se la pasa a fn. Con
// follow sigue hasta que el proceso termina (o ctx se cancela) y devuelve el
// código de salida; sin follow, vuelca lo que hay y devuelve nil en exit si
// sigue corriendo. next es el desplazamiento desde el que continuar.
func (c *Client) ProcessOutput(ctx context.Context, ref string, pid int, offset int64, follow bool, fn func(ProcessEvent) error) (exit *int, next int64, err error) {
	q := url.Values{"offset": {strconv.FormatInt(offset, 10)}}
	if follow {
		q.Set("follow", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://kling"+procPath(ref, pid, "/output")+"?"+q.Encode(), nil)
	if err != nil {
		return nil, offset, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, offset, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, offset, statusErrorFrom(resp)
	}
	next = offset
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev ProcessEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, next, fmt.Errorf("unreadable process event: %w", err)
		}
		if ev.Error != "" {
			return nil, next, &ExecError{Message: ev.Error}
		}
		if ev.Offset > next {
			next = ev.Offset
		}
		if fn != nil && (len(ev.Data) > 0 || ev.Dropped > 0) {
			if err := fn(ev); err != nil {
				return nil, next, err
			}
		}
		if ev.Exit != nil {
			code := *ev.Exit
			exit = &code
		}
	}
	return exit, next, sc.Err()
}

// CreateSession crea una sesión.
func (c *Client) CreateSession(ctx context.Context, ref string, r SessionRequest) (*Session, error) {
	var s Session
	return &s, c.do(ctx, http.MethodPost, "/machines/"+url.PathEscape(ref)+"/sessions", r, &s)
}

// Sessions lista las sesiones de la máquina.
func (c *Client) Sessions(ctx context.Context, ref string) ([]Session, error) {
	var out []Session
	return out, c.do(ctx, http.MethodGet, "/machines/"+url.PathEscape(ref)+"/sessions", nil, &out)
}

// GetSession da el estado de una sesión.
func (c *Client) GetSession(ctx context.Context, ref, id string) (*Session, error) {
	var s Session
	return &s, c.do(ctx, http.MethodGet, "/machines/"+url.PathEscape(ref)+"/sessions/"+url.PathEscape(id), nil, &s)
}

// RemoveSession borra una sesión.
func (c *Client) RemoveSession(ctx context.Context, ref, id string) error {
	return c.do(ctx, http.MethodDelete, "/machines/"+url.PathEscape(ref)+"/sessions/"+url.PathEscape(id), nil, nil)
}
