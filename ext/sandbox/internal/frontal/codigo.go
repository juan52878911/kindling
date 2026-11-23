package frontal

// Intérprete de código: /code y /code/contexts del execd de OpenSandbox con
// Python, bash y JavaScript (node), estado que persiste entre ejecuciones de un
// contexto, resultados por tipo MIME, interrupción y salida en flujo NDJSON.
//
// No toca el agente del invitado. Un contexto es un proceso de fondo
// (/processes) que corre el conductor (codigo_conductor.py) y escucha en un
// socket Unix de un directorio 0700 (en bash, una FIFO); ejecutar es un exec
// de un cliente de una línea que manda el código por stdin a ese socket. Cada
// lenguaje trae su conductor embebido (tabla lenguajes). El gateway no guarda
// estado: los contextos de un sandbox se reconocen por la línea de órdenes de
// sus procesos, así que sobreviven a un reinicio del gateway y el dueño es el
// del sandbox (buscarEjecutable ya contesta 404 por los ajenos).

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

//go:embed codigo_conductor.py
var conductorPy []byte

//go:embed codigo_conductor.sh
var conductorSh []byte

//go:embed codigo_conductor.js
var conductorJs []byte

// lenguaje es un lenguaje del intérprete: su intérprete en el invitado, el
// nombre del conductor y su fuente.
type lenguaje struct {
	nombre     string
	interprete string
	conductor  string
	fuente     []byte
}

// lenguajes: los admitidos. El primero es el de por defecto.
var lenguajes = []lenguaje{
	{nombre: "python", interprete: "python3", conductor: "conductor.py", fuente: conductorPy},
	{nombre: "bash", interprete: "bash", conductor: "conductor.sh", fuente: conductorSh},
	{nombre: "javascript", interprete: "node", conductor: "conductor.js", fuente: conductorJs},
}

// lenguajeDe resuelve el nombre ("" = Python).
func lenguajeDe(nombre string) (lenguaje, error) {
	if nombre == "" {
		return lenguajes[0], nil
	}
	var nombres []string
	for _, l := range lenguajes {
		if l.nombre == nombre {
			return l, nil
		}
		nombres = append(nombres, fmt.Sprintf("%q", l.nombre))
	}
	return lenguaje{}, fmt.Errorf("unsupported language %q: use %s", nombre, strings.Join(nombres, ", "))
}

// errSinInterprete es que la imagen del sandbox no trae el intérprete.
type errSinInterprete struct{ l lenguaje }

func (e *errSinInterprete) Error() string {
	return fmt.Sprintf("this sandbox image has no %s: %s code contexts need it (add %s to the image)",
		e.l.interprete, e.l.nombre, e.l.interprete)
}

const (
	// maxContextosPorSandbox acota los contextos vivos de un sandbox.
	maxContextosPorSandbox = 8
	// plazoCodigoPorDefecto y plazoCodigoMax acotan una ejecución (segundos).
	plazoCodigoPorDefecto = 60
	plazoCodigoMax        = 600
	// dirCodigo es el prefijo del directorio (0700) de cada contexto; el
	// resto es el id.
	dirCodigo = "/tmp/.kindling-code-"
	// salidaCodigoMax acota los bytes que el cliente de una ejecución puede
	// devolver (el exec lo trunca; el proceso del conductor sigue).
	salidaCodigoMax = 8 << 20
)

var reIDContexto = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Contexto es un contexto de código de un sandbox.
type Contexto struct {
	ID        string    `json:"id"`
	Language  string    `json:"language"`
	PID       int       `json:"pid"`
	Cwd       string    `json:"cwd,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// ContextoPeticion es el cuerpo de POST .../code/contexts.
type ContextoPeticion struct {
	Language string `json:"language,omitempty"`
	// Cwd es el directorio de trabajo del contexto ("" = el del agente).
	Cwd string `json:"cwd,omitempty"`
}

// CodigoPeticion es el cuerpo de POST .../code.
type CodigoPeticion struct {
	Code string `json:"code"`
	// ContextID, vacío = un contexto efímero que se borra al acabar.
	ContextID string `json:"context_id,omitempty"`
	Language  string `json:"language,omitempty"`
	// TimeoutSeconds por ejecución (por defecto 60, máximo 600).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// CodigoEvento es una línea del flujo NDJSON de POST .../code. Type es
// stdout o stderr (con Text), result (Text = repr del último valor y Results
// = sus representaciones por tipo MIME), error (Ename, Evalue, Traceback) o
// done (OK, DurationMS); done cierra el flujo, siempre.
type CodigoEvento struct {
	Type      string   `json:"type"`
	Text      string   `json:"text,omitempty"`
	Ename     string   `json:"ename,omitempty"`
	Evalue    string   `json:"evalue,omitempty"`
	Traceback []string `json:"traceback,omitempty"`
	// Results: text/plain siempre; text/html e image/png (base64) si el valor
	// las tiene.
	Results    map[string]string `json:"results,omitempty"`
	OK         *bool             `json:"ok,omitempty"`
	DurationMS *int64            `json:"duration_ms,omitempty"`
	// ExecutionCount es el número de ejecuciones del contexto, contada con
	// esta (en result y done).
	ExecutionCount int `json:"execution_count,omitempty"`
}

func (c Contexto) dir() string { return dirCodigo + c.ID }

// despachaCodigo atiende /v1/sandboxes/{id}/code[/contexts[/{cid}]]. resto
// son los segmentos tras "code".
func (s *Servidor) despachaCodigo(w http.ResponseWriter, r *http.Request, id string, resto []string) {
	nomala := func() {
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	}
	switch {
	case len(resto) == 0:
		if r.Method != http.MethodPost {
			nomala()
			return
		}
		s.handleCodigo(w, r, id)
	case resto[0] != "contexts" || len(resto) > 3 || (len(resto) == 3 && resto[2] != "execution"):
		noRuta(w, r)
	case len(resto) == 3:
		if r.Method != http.MethodDelete {
			nomala()
			return
		}
		s.handleInterrumpir(w, r, id, resto[1])
	case len(resto) == 1:
		switch r.Method {
		case http.MethodPost:
			s.handleContextoNuevo(w, r, id)
		case http.MethodGet:
			s.handleContextos(w, r, id)
		default:
			nomala()
		}
	default:
		switch r.Method {
		case http.MethodGet:
			s.handleContexto(w, r, id, resto[1])
		case http.MethodDelete:
			s.handleContextoBorrar(w, r, id, resto[1])
		default:
			nomala()
		}
	}
}

// contextosDe lista los contextos vivos de un sandbox, por sus procesos.
func contextosDe(ctx context.Context, h *hosts.Host, ref string) ([]Contexto, error) {
	ps, err := h.Cliente.Processes(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []Contexto
	for _, p := range ps {
		if p.State != api.ProcessRunning || len(p.Cmd) != 4 || p.Cmd[2] != "serve" {
			continue
		}
		l, ok := lenguajeDeProceso(p.Cmd[0], path.Base(p.Cmd[1]))
		if !ok {
			continue
		}
		id, ok := strings.CutPrefix(path.Dir(p.Cmd[1]), dirCodigo)
		if !ok || !reIDContexto.MatchString(id) {
			continue
		}
		out = append(out, Contexto{ID: id, Language: l.nombre, PID: p.PID, Cwd: p.Dir, StartedAt: p.StartedAt})
	}
	return out, nil
}

func buscarContexto(ctx context.Context, h *hosts.Host, ref, cid string) (*Contexto, error) {
	if !reIDContexto.MatchString(cid) {
		return nil, nil
	}
	cs, err := contextosDe(ctx, h, ref)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		if cs[i].ID == cid {
			return &cs[i], nil
		}
	}
	return nil, nil
}

func noContexto(cid string) error { return fmt.Errorf("no code context %q in this sandbox", cid) }

// lenguajeDeProceso reconoce el conductor de un contexto por su línea de órdenes.
func lenguajeDeProceso(interprete, conductor string) (lenguaje, bool) {
	for _, l := range lenguajes {
		if l.interprete == interprete && l.conductor == conductor {
			return l, true
		}
	}
	return lenguaje{}, false
}

// crearContexto monta el directorio, sube el conductor y lo arranca como
// proceso de fondo. Devuelve *errSinInterprete si la imagen no tiene el intérprete.
func (s *Servidor) crearContexto(ctx context.Context, h *hosts.Host, ref, cwd, sesion string, l lenguaje) (*Contexto, error) {
	cs, err := contextosDe(ctx, h, ref)
	if err != nil {
		return nil, err
	}
	if len(cs) >= maxContextosPorSandbox {
		return nil, &errTope{fmt.Sprintf("this sandbox already has %d code contexts; the limit is %d", len(cs), maxContextosPorSandbox)}
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	c := &Contexto{ID: hex.EncodeToString(b), Language: l.nombre, Cwd: cwd}
	dir := c.dir()

	// El directorio nace 0700: umask 077 antes de mkdir.
	res, err := h.Cliente.Exec(ctx, ref, api.ExecRequest{
		Cmd:            []string{"sh", "-c", "command -v " + l.interprete + " >/dev/null 2>&1 || exit 127; umask 077; mkdir -p " + dir},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
	case 127:
		return nil, &errSinInterprete{l}
	default:
		return nil, fmt.Errorf("preparing the context directory failed (exit %d): %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	limpiar := func() { s.borrarDirContexto(h, ref, dir) }
	// 0644 dentro de un directorio 0700: el dueño del directorio lo lee y nadie más llega.
	if _, err := h.Cliente.WriteFile(ctx, ref, dir+"/"+l.conductor, 0o644, false, bytes.NewReader(l.fuente)); err != nil {
		limpiar()
		return nil, err
	}
	// El intérprete nace con el entorno de la sesión del env de la creación (si
	// la hay): es el que ven luego todas las ejecuciones del contexto.
	arranque := api.ProcessStartRequest{
		Cmd:     []string{l.interprete, dir + "/" + l.conductor, "serve", dir + "/s.sock"},
		Dir:     cwd,
		Session: sesion,
	}
	p, err := h.Cliente.StartProcess(ctx, ref, arranque)
	if err != nil && sesion != "" && esSesionPerdida(err) {
		arranque.Session = ""
		p, err = h.Cliente.StartProcess(ctx, ref, arranque)
	}
	if err != nil {
		limpiar()
		return nil, err
	}
	c.PID, c.StartedAt = p.PID, p.StartedAt
	return c, nil
}

// borrarDirContexto quita el directorio de un contexto, sin esperar a nadie.
func (s *Servidor) borrarDirContexto(h *hosts.Host, ref, dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := h.Cliente.Exec(ctx, ref, api.ExecRequest{Cmd: []string{"rm", "-rf", dir}, TimeoutSeconds: 10}, nil); err != nil {
		s.log.Printf("frontal: code: removing %s in %s/%s: %v", dir, h.Nombre, ref, err)
	}
}

// errTope es un tope superado (429).
type errTope struct{ msg string }

func (e *errTope) Error() string { return e.msg }

// errContexto traduce un error de crearContexto o de borrado a una respuesta.
func (s *Servidor) errContexto(w http.ResponseWriter, h *hosts.Host, err error) {
	var et *errTope
	var si *errSinInterprete
	switch {
	case errors.As(err, &si):
		fail(w, http.StatusUnprocessableEntity, err)
	case errors.As(err, &et):
		fail(w, http.StatusTooManyRequests, err)
	default:
		fail(w, codigoExec(err), deHost(h, err))
	}
}

func (s *Servidor) handleContextoNuevo(w http.ResponseWriter, r *http.Request, id string) {
	var req ContextoPeticion
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	l, err := lenguajeDe(req.Language)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	c, err := s.crearContexto(r.Context(), h, mc.ID, req.Cwd, sesionPorDefecto(mc), l)
	if err != nil {
		s.errContexto(w, h, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Servidor) handleContextos(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	cs, err := contextosDe(r.Context(), h, mc.ID)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if cs == nil {
		cs = []Contexto{}
	}
	writeJSON(w, http.StatusOK, cs)
}

func (s *Servidor) handleContexto(w http.ResponseWriter, r *http.Request, id, cid string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	c, err := buscarContexto(r.Context(), h, mc.ID, cid)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if c == nil {
		fail(w, http.StatusNotFound, noContexto(cid))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Servidor) handleContextoBorrar(w http.ResponseWriter, r *http.Request, id, cid string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	c, err := buscarContexto(r.Context(), h, mc.ID, cid)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if c == nil {
		fail(w, http.StatusNotFound, noContexto(cid))
		return
	}
	s.matarContexto(r.Context(), h, mc.ID, c, w)
}

// handleInterrumpir sirve DELETE .../code/contexts/{cid}/execution: SIGINT al
// conductor, que lo convierte en una interrupción de la ejecución en curso (y
// no hace nada si no hay ninguna). El contexto y su estado siguen.
func (s *Servidor) handleInterrumpir(w http.ResponseWriter, r *http.Request, id, cid string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	c, err := buscarContexto(r.Context(), h, mc.ID, cid)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if c == nil {
		fail(w, http.StatusNotFound, noContexto(cid))
		return
	}
	if _, err := h.Cliente.SignalProcess(r.Context(), mc.ID, c.PID, "INT"); err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// matarContexto mata el proceso del contexto (y a sus hijos: DELETE señala al
// grupo) y quita su directorio. Contesta 204 o el error.
func (s *Servidor) matarContexto(ctx context.Context, h *hosts.Host, ref string, c *Contexto, w http.ResponseWriter) {
	if err := h.Cliente.RemoveProcess(ctx, ref, c.PID); err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	s.borrarDirContexto(h, ref, c.dir())
	w.WriteHeader(http.StatusNoContent)
}

// handleCodigo sirve POST /v1/sandboxes/{id}/code: NDJSON de CodigoEvento.
func (s *Servidor) handleCodigo(w http.ResponseWriter, r *http.Request, id string) {
	const tope = 2*api.ExecMaxStdin + 64<<10
	if r.ContentLength > tope {
		fail(w, http.StatusRequestEntityTooLarge, errors.New("code body is too large"))
		return
	}
	b, err := api.LeerCuerpo(r.Body, tope)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var req CodigoPeticion
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	switch {
	case len(req.Code) > api.ExecMaxStdin:
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("code is %d bytes; the limit is %d", len(req.Code), api.ExecMaxStdin))
		return
	case strings.TrimSpace(req.Code) == "":
		fail(w, http.StatusBadRequest, errors.New("code is required"))
		return
	case req.TimeoutSeconds < 0 || req.TimeoutSeconds > plazoCodigoMax:
		fail(w, http.StatusBadRequest, fmt.Errorf("timeout_seconds must be between 0 and %d", plazoCodigoMax))
		return
	case req.ContextID != "" && !reIDContexto.MatchString(req.ContextID):
		fail(w, http.StatusNotFound, noContexto(req.ContextID))
		return
	}
	l, err := lenguajeDe(req.Language)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	plazo := req.TimeoutSeconds
	if plazo == 0 {
		plazo = plazoCodigoPorDefecto
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}

	var c *Contexto
	if req.ContextID != "" {
		if c, err = buscarContexto(r.Context(), h, mc.ID, req.ContextID); err != nil {
			fail(w, codigoExec(err), deHost(h, err))
			return
		}
		if c == nil {
			fail(w, http.StatusNotFound, noContexto(req.ContextID))
			return
		}
		// El contexto manda: un language que lo contradice es un error, no se
		// ejecuta en otro intérprete.
		if req.Language != "" && req.Language != c.Language {
			fail(w, http.StatusBadRequest, fmt.Errorf("context %s is a %s context, not %s", c.ID, c.Language, req.Language))
			return
		}
		if l, err = lenguajeDe(c.Language); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		if c, err = s.crearContexto(r.Context(), h, mc.ID, "", sesionPorDefecto(mc), l); err != nil {
			s.errContexto(w, h, err)
			return
		}
		// Efímero: se borra pase lo que pase, aunque el cliente se vaya.
		defer func() {
			bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
			defer cancel()
			if err := h.Cliente.RemoveProcess(bg, mc.ID, c.PID); err != nil {
				s.log.Printf("frontal: code: removing ephemeral context %s in %s/%s: %v", c.ID, h.Nombre, mc.ID, err)
			}
			s.borrarDirContexto(h, mc.ID, c.dir())
		}()
	}

	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	empezado := false
	emitir := func(ev CodigoEvento) error {
		if !empezado {
			empezado = true
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	terminado := false
	var pend []byte
	linea := func(l []byte) error {
		l = bytes.TrimSpace(l)
		if len(l) == 0 {
			return nil
		}
		var ev CodigoEvento
		if err := json.Unmarshal(l, &ev); err != nil || ev.Type == "" {
			return emitir(CodigoEvento{Type: "stderr", Text: string(l) + "\n"})
		}
		if ev.Type == "done" {
			terminado = true
		}
		return emitir(ev)
	}
	dir := c.dir()
	res, err := h.Cliente.Exec(r.Context(), mc.ID, api.ExecRequest{
		Cmd:            []string{l.interprete, dir + "/" + l.conductor, "run", dir + "/s.sock", strconv.Itoa(plazo)},
		Stdin:          []byte(req.Code),
		TimeoutSeconds: plazo + 15,
		MaxOutputBytes: salidaCodigoMax,
	}, func(stream string, data []byte) error {
		if stream != "stdout" {
			// Lo que el cliente dice por stderr es suyo (no del código): un fallo.
			return emitir(CodigoEvento{Type: "error", Ename: "ContextError", Evalue: strings.TrimSpace(string(data))})
		}
		pend = append(pend, data...)
		for {
			i := bytes.IndexByte(pend, '\n')
			if i < 0 {
				return nil
			}
			if err := linea(pend[:i]); err != nil {
				return err
			}
			pend = pend[i+1:]
		}
	})
	if err == nil && len(pend) > 0 {
		_ = linea(pend)
	}
	switch {
	case err != nil && !empezado:
		fail(w, codigoExec(err), deHost(h, err))
	case err != nil:
		// El flujo ya dijo 200: se explica el fallo (sin datos del host) y se
		// cierra con done, como todo flujo de /code.
		_ = emitir(CodigoEvento{Type: "error", Ename: "GatewayError", Evalue: errorDeFlujo(deHost(h, err))})
		if !terminado {
			ok := false
			_ = emitir(CodigoEvento{Type: "done", OK: &ok})
		}
	case !terminado:
		// El cliente murió o el contexto se perdió sin cerrar el flujo.
		msg := fmt.Sprintf("the execution ended without finishing (exit %d)", res.ExitCode)
		if res.TimedOut {
			msg = "the execution timed out"
		}
		ok := false
		_ = emitir(CodigoEvento{Type: "error", Ename: "ContextError", Evalue: msg})
		_ = emitir(CodigoEvento{Type: "done", OK: &ok})
	}
}
