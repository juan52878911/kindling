package osb

// El execd de OSB (puerto 44772) sobre las rutas del gateway: /command y sus
// procesos de fondo, /session, /code, /files, /directories y /metrics.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Topes del lado del shim.
const (
	maxComandos     = 256      // comandos en primer plano que se recuerdan
	maxSalidaComand = 1 << 20  // salida que se guarda de cada uno para /logs
	maxLeerFichero  = 64 << 20 // replace y la lectura por líneas
	maxBusqueda     = 1000     // resultados de /files/search
)

func (s *Servidor) rutasExecd(m *http.ServeMux) {
	m.HandleFunc("GET /ping", s.ping)
	m.HandleFunc("POST /command", s.comandoRun)
	m.HandleFunc("DELETE /command", s.comandoInterrumpir)
	// /command/status/{id} y /command/{id}/logs se solapan para ServeMux.
	m.HandleFunc("GET /command/{a}/{b}", s.comandoInfo)
	m.HandleFunc("POST /session", s.sesionCrear)
	m.HandleFunc("POST /session/{sid}/run", s.sesionRun)
	m.HandleFunc("DELETE /session/{sid}", s.sesionBorrar)
	m.HandleFunc("POST /code/context", s.contextoCrear)
	m.HandleFunc("GET /code/contexts", s.contextosListar)
	m.HandleFunc("DELETE /code/contexts", s.contextosBorrar)
	m.HandleFunc("GET /code/contexts/{cid}", s.contextoVer)
	m.HandleFunc("DELETE /code/contexts/{cid}", s.contextoBorrar)
	m.HandleFunc("POST /code", s.codigoRun)
	m.HandleFunc("DELETE /code", s.codigoInterrumpir)
	m.HandleFunc("GET /files/info", s.ficherosInfo)
	m.HandleFunc("DELETE /files", s.ficherosBorrar)
	m.HandleFunc("POST /files/permissions", s.ficherosPermisos)
	m.HandleFunc("POST /files/mv", s.ficherosMover)
	m.HandleFunc("GET /files/search", s.ficherosBuscar)
	m.HandleFunc("POST /files/replace", s.ficherosReemplazar)
	m.HandleFunc("POST /files/upload", s.ficherosSubir)
	m.HandleFunc("GET /files/download", s.ficherosBajar)
	m.HandleFunc("GET /directories/list", s.dirListar)
	m.HandleFunc("POST /directories", s.dirCrear)
	m.HandleFunc("DELETE /directories", s.dirBorrar)
	m.HandleFunc("GET /metrics", s.metricas)
}

// falloExecd contesta un error del gateway con los codes de execd; c404 es el
// code de un 404 (FILE_NOT_FOUND, CONTEXT_NOT_FOUND…).
func (s *Servidor) falloExecd(w http.ResponseWriter, err error, c404 string) {
	e, ok := err.(*errGW)
	if !ok {
		errOSB(w, http.StatusInternalServerError, "RUNTIME_ERROR", s.redactar(err.Error()))
		return
	}
	st := e.Status
	if st == http.StatusBadGateway && (e.Code == "not_found" || strings.Contains(strings.ToLower(e.Msg), "no such file")) {
		st = http.StatusNotFound
	}
	code := "RUNTIME_ERROR"
	switch {
	case st == 404 && c404 != "":
		code = c404
	case st == 400 || st == 413 || st == 422:
		code = "INVALID_REQUEST_BODY"
	case st == 501:
		code = "NOT_SUPPORTED"
	case st == 503:
		code = "SERVICE_UNAVAILABLE"
	}
	if st == http.StatusBadGateway && e.Code == "" {
		st = http.StatusInternalServerError
	}
	errOSB(w, st, code, e.Msg)
}

func malaPeticion(w http.ResponseWriter, msg string) {
	errOSB(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", msg)
}

func idAleatorio(prefijo string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefijo + hex.EncodeToString(b)
}

// ping contesta 200 si el sandbox existe y está en marcha (el SDK espera a
// esto tras crear o reanudar).
func (s *Servidor) ping(w http.ResponseWriter, r *http.Request) {
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "GET", rutaSB(idDe(r)), nil, &g); err != nil {
		s.falloExecd(w, err, "")
		return
	}
	if g.State != "running" && g.State != "frozen" {
		errOSB(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "sandbox is "+g.State)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Comandos ----

// comando es un comando en primer plano: su estado y su salida (acotada) para
// /command/status y /command/{id}/logs.
type comando struct {
	id, sandbox, linea string
	inicio, fin        time.Time
	exit               *int
	error              string
	corriendo          bool
	salida             []byte
	cancel             context.CancelFunc
}

func (s *Servidor) registrar(sandbox, linea string, cancel context.CancelFunc) *comando {
	c := &comando{id: idAleatorio("c"), sandbox: sandbox, linea: linea, inicio: time.Now(), corriendo: true, cancel: cancel}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds[c.id] = c
	s.cmdOrden = append(s.cmdOrden, c.id)
	for len(s.cmdOrden) > maxComandos {
		delete(s.cmds, s.cmdOrden[0])
		s.cmdOrden = s.cmdOrden[1:]
	}
	return c
}

func (s *Servidor) comandoDe(sandbox, id string) *comando {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.cmds[id]; c != nil && c.sandbox == sandbox {
		return c
	}
	return nil
}

type peticionComando struct {
	Command    string            `json:"command"`
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd"`
	Background bool              `json:"background"`
	Timeout    int64             `json:"timeout"`
	UID        *int              `json:"uid"`
	GID        *int              `json:"gid"`
	Envs       map[string]string `json:"envs"`
}

// segundos pasa un plazo en milisegundos de OSB a segundos enteros del gateway.
func segundos(ms int64) int {
	if ms <= 0 {
		return 0
	}
	return int((ms + 999) / 1000)
}

func (p peticionComando) aExec() (map[string]any, string, error) {
	var cmd []string
	linea := p.Command
	switch {
	case len(p.Argv) > 0:
		cmd = p.Argv
		linea = strings.Join(p.Argv, " ")
	case p.Command != "":
		cmd = []string{"sh", "-c", p.Command}
	default:
		return nil, "", fmt.Errorf("command or argv is required")
	}
	req := map[string]any{"cmd": cmd}
	if p.Cwd != "" {
		req["dir"] = p.Cwd
	}
	if len(p.Envs) > 0 {
		req["env"] = envLista(p.Envs)
	}
	if t := segundos(p.Timeout); t > 0 {
		req["timeout_seconds"] = t
	}
	switch {
	case p.GID != nil && p.UID == nil:
		return nil, "", fmt.Errorf("gid needs uid")
	case p.UID != nil && p.GID != nil:
		req["user"] = fmt.Sprintf("%d:%d", *p.UID, *p.GID)
	case p.UID != nil:
		req["user"] = strconv.Itoa(*p.UID)
	}
	return req, linea, nil
}

func (s *Servidor) comandoRun(w http.ResponseWriter, r *http.Request) {
	var p peticionComando
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		malaPeticion(w, "invalid request body")
		return
	}
	req, linea, err := p.aExec()
	if err != nil {
		malaPeticion(w, err.Error())
		return
	}
	id := idDe(r)
	if p.Background {
		var pr struct {
			PID int `json:"pid"`
		}
		if err := s.llamarGW(r.Context(), "POST", rutaSB(id)+"/processes", req, &pr); err != nil {
			s.falloExecd(w, err, "")
			return
		}
		f := nuevoSSE(w)
		defer f.cerrar()
		f.enviar(evento{Type: "init", Text: "p" + strconv.Itoa(pr.PID)})
		f.enviar(evento{Type: "execution_complete"})
		return
	}
	s.ejecutar(w, r, id, linea, req)
}

// ejecutar corre un exec en primer plano y lo convierte en SSE de execd.
func (s *Servidor) ejecutar(w http.ResponseWriter, r *http.Request, id, linea string, req map[string]any) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := s.registrar(id, linea, cancel)
	resp, err := s.gw(ctx, "POST", rutaSB(id)+"/exec", req)
	if err == nil && resp.StatusCode >= 300 {
		err = s.leerError(resp)
		resp.Body.Close()
	}
	if err != nil {
		s.terminar(c, nil, err.Error())
		s.falloExecd(w, err, "")
		return
	}
	defer resp.Body.Close()
	f := nuevoSSE(w)
	defer f.cerrar()
	f.enviar(evento{Type: "init", Text: c.id})
	code := traducirExec(ctx, f, resp.Body, func(b []byte) {
		s.mu.Lock()
		if n := maxSalidaComand - len(c.salida); n > 0 {
			c.salida = append(c.salida, b[:min(n, len(b))]...)
		}
		s.mu.Unlock()
	})
	msg := ""
	if code == nil {
		msg = "interrupted or cut"
	}
	s.terminar(c, code, msg)
}

func (s *Servidor) terminar(c *comando, code *int, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.corriendo, c.exit, c.error, c.fin = false, code, msg, time.Now()
}

// pidDe dice si un id de comando es de un proceso de fondo ("p<pid>").
func pidDe(id string) (int, bool) {
	if !strings.HasPrefix(id, "p") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	return n, err == nil && n > 0
}

func (s *Servidor) comandoInterrumpir(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("id")
	if cid == "" {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "id is required")
		return
	}
	id := idDe(r)
	if pid, ok := pidDe(cid); ok {
		if err := s.llamarGW(r.Context(), "POST", fmt.Sprintf("%s/processes/%d/signal", rutaSB(id), pid), map[string]string{"signal": "TERM"}, nil); err != nil {
			s.falloExecd(w, err, "")
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	c := s.comandoDe(id, cid)
	if c == nil {
		errOSB(w, http.StatusNotFound, "RUNTIME_ERROR", "no such command")
		return
	}
	c.cancel()
	w.WriteHeader(http.StatusOK)
}

type gwProceso struct {
	PID       int        `json:"pid"`
	Cmd       []string   `json:"cmd"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	ExitCode  *int       `json:"exit_code"`
	Signal    string     `json:"signal"`
}

func (s *Servidor) comandoInfo(w http.ResponseWriter, r *http.Request) {
	a, b := r.PathValue("a"), r.PathValue("b")
	switch {
	case a == "status":
		s.comandoEstado(w, r, b)
	case b == "logs":
		s.comandoLogs(w, r, a)
	default:
		errOSB(w, http.StatusNotFound, codigoRuta, "no such execd route: "+r.URL.Path)
	}
}

func (s *Servidor) comandoEstado(w http.ResponseWriter, r *http.Request, cid string) {
	id := idDe(r)
	if pid, ok := pidDe(cid); ok {
		var p gwProceso
		if err := s.llamarGW(r.Context(), "GET", fmt.Sprintf("%s/processes/%d", rutaSB(id), pid), nil, &p); err != nil {
			s.falloExecd(w, err, "RUNTIME_ERROR")
			return
		}
		out := map[string]any{"id": cid, "content": strings.Join(p.Cmd, " "), "running": p.State == "running",
			"exit_code": p.ExitCode, "started_at": p.StartedAt, "finished_at": p.EndedAt}
		if p.Signal != "" {
			out["error"] = "killed by signal " + p.Signal
		}
		escribirJSON(w, http.StatusOK, out)
		return
	}
	c := s.comandoDe(id, cid)
	if c == nil {
		errOSB(w, http.StatusNotFound, "RUNTIME_ERROR", "no such command")
		return
	}
	s.mu.Lock()
	out := map[string]any{"id": c.id, "content": c.linea, "running": c.corriendo, "exit_code": c.exit,
		"started_at": c.inicio, "finished_at": nil}
	if !c.fin.IsZero() {
		out["finished_at"] = c.fin
	}
	if c.error != "" {
		out["error"] = c.error
	}
	s.mu.Unlock()
	escribirJSON(w, http.StatusOK, out)
}

// comandoLogs da la salida desde la línea `cursor` y en
// EXECD-COMMANDS-TAIL-CURSOR la siguiente que pedir.
func (s *Servidor) comandoLogs(w http.ResponseWriter, r *http.Request, cid string) {
	cursor := 0
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			malaPeticion(w, "cursor must be a non-negative integer")
			return
		}
		cursor = n
	}
	id := idDe(r)
	var salida []byte
	if pid, ok := pidDe(cid); ok {
		resp, err := s.gw(r.Context(), "GET", fmt.Sprintf("%s/processes/%d/output?offset=0", rutaSB(id), pid), nil)
		if err == nil && resp.StatusCode >= 300 {
			err = s.leerError(resp)
			resp.Body.Close()
		}
		if err != nil {
			s.falloExecd(w, err, "RUNTIME_ERROR")
			return
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var ev eventoExec
			if json.Unmarshal(sc.Bytes(), &ev) == nil && len(salida) < maxSalidaComand {
				salida = append(salida, ev.Data...)
			}
		}
		resp.Body.Close()
	} else {
		c := s.comandoDe(id, cid)
		if c == nil {
			errOSB(w, http.StatusNotFound, "RUNTIME_ERROR", "no such command")
			return
		}
		s.mu.Lock()
		salida = append([]byte(nil), c.salida...)
		s.mu.Unlock()
	}
	lineas := strings.SplitAfter(string(salida), "\n")
	if n := len(lineas); n > 0 && lineas[n-1] == "" {
		lineas = lineas[:n-1]
	}
	if cursor > len(lineas) {
		cursor = len(lineas)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("EXECD-COMMANDS-TAIL-CURSOR", strconv.Itoa(len(lineas)))
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, strings.Join(lineas[cursor:], ""))
}

// ---- Sesiones ----

func (s *Servidor) sesionCrear(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p) // cuerpo opcional
	sid := idAleatorio("osb-")
	req := map[string]any{"id": sid}
	if p.Cwd != "" {
		req["dir"] = p.Cwd
	}
	if err := s.llamarGW(r.Context(), "POST", rutaSB(idDe(r))+"/sessions", req, nil); err != nil {
		s.falloExecd(w, err, "")
		return
	}
	escribirJSON(w, http.StatusOK, map[string]string{"session_id": sid})
}

func (s *Servidor) sesionRun(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int64  `json:"timeout"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil || p.Command == "" {
		malaPeticion(w, "command is required")
		return
	}
	req := map[string]any{"cmd": []string{"sh", "-c", p.Command}, "session": r.PathValue("sid")}
	if p.Cwd != "" {
		req["dir"] = p.Cwd
	}
	if t := segundos(p.Timeout); t > 0 {
		req["timeout_seconds"] = t
	}
	s.ejecutar(w, r, idDe(r), p.Command, req)
}

func (s *Servidor) sesionBorrar(w http.ResponseWriter, r *http.Request) {
	ruta := rutaSB(idDe(r)) + "/sessions/" + url.PathEscape(r.PathValue("sid"))
	if err := s.llamarGW(r.Context(), "DELETE", ruta, nil, nil); err != nil {
		s.falloExecd(w, err, "SESSION_NOT_FOUND")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Intérprete de código ----

type gwContexto struct {
	ID       string `json:"id"`
	Language string `json:"language"`
}

func lenguaje(l string) string {
	if l == "" {
		return "python"
	}
	return l
}

func (s *Servidor) contextoCrear(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Language string `json:"language"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
	var c gwContexto
	if err := s.llamarGW(r.Context(), "POST", rutaSB(idDe(r))+"/code/contexts", map[string]string{"language": lenguaje(p.Language)}, &c); err != nil {
		s.falloExecd(w, err, "")
		return
	}
	escribirJSON(w, http.StatusOK, gwContexto{ID: c.ID, Language: lenguaje(c.Language)})
}

func (s *Servidor) listarContextos(ctx context.Context, id, idioma string) ([]gwContexto, error) {
	var cs []gwContexto
	if err := s.llamarGW(ctx, "GET", rutaSB(id)+"/code/contexts", nil, &cs); err != nil {
		return nil, err
	}
	out := []gwContexto{}
	for _, c := range cs {
		if idioma == "" || c.Language == idioma {
			out = append(out, gwContexto{ID: c.ID, Language: c.Language})
		}
	}
	return out, nil
}

func (s *Servidor) contextosListar(w http.ResponseWriter, r *http.Request) {
	cs, err := s.listarContextos(r.Context(), idDe(r), r.URL.Query().Get("language"))
	if err != nil {
		s.falloExecd(w, err, "")
		return
	}
	escribirJSON(w, http.StatusOK, cs)
}

func (s *Servidor) contextosBorrar(w http.ResponseWriter, r *http.Request) {
	idioma := r.URL.Query().Get("language")
	if idioma == "" {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "language is required")
		return
	}
	id := idDe(r)
	cs, err := s.listarContextos(r.Context(), id, idioma)
	if err != nil {
		s.falloExecd(w, err, "")
		return
	}
	for _, c := range cs {
		if err := s.llamarGW(r.Context(), "DELETE", rutaSB(id)+"/code/contexts/"+url.PathEscape(c.ID), nil, nil); err != nil {
			s.falloExecd(w, err, "CONTEXT_NOT_FOUND")
			return
		}
	}
	s.mu.Lock()
	delete(s.contextos, id+"\x00"+idioma)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *Servidor) contextoVer(w http.ResponseWriter, r *http.Request) {
	var c gwContexto
	if err := s.llamarGW(r.Context(), "GET", rutaSB(idDe(r))+"/code/contexts/"+url.PathEscape(r.PathValue("cid")), nil, &c); err != nil {
		s.falloExecd(w, err, "CONTEXT_NOT_FOUND")
		return
	}
	escribirJSON(w, http.StatusOK, gwContexto{ID: c.ID, Language: c.Language})
}

func (s *Servidor) contextoBorrar(w http.ResponseWriter, r *http.Request) {
	if err := s.llamarGW(r.Context(), "DELETE", rutaSB(idDe(r))+"/code/contexts/"+url.PathEscape(r.PathValue("cid")), nil, nil); err != nil {
		s.falloExecd(w, err, "CONTEXT_NOT_FOUND")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// contextoDefecto es el contexto persistente que usa /code sin context.id, uno
// por sandbox y lenguaje, como el execd real.
func (s *Servidor) contextoDefecto(ctx context.Context, id, idioma string, nuevo bool) (string, error) {
	k := id + "\x00" + idioma
	s.mu.Lock()
	cid := s.contextos[k]
	s.mu.Unlock()
	if cid != "" && !nuevo {
		return cid, nil
	}
	var c gwContexto
	if err := s.llamarGW(ctx, "POST", rutaSB(id)+"/code/contexts", map[string]string{"language": idioma}, &c); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.contextos[k] = c.ID
	s.mu.Unlock()
	return c.ID, nil
}

// eventoCodigo es una línea del NDJSON de /code del gateway.
type eventoCodigo struct {
	Type           string            `json:"type"`
	Text           string            `json:"text"`
	Ename          string            `json:"ename"`
	Evalue         string            `json:"evalue"`
	Traceback      []string          `json:"traceback"`
	Results        map[string]string `json:"results"`
	OK             *bool             `json:"ok"`
	DurationMS     *int64            `json:"duration_ms"`
	ExecutionCount int               `json:"execution_count"`
}

func (s *Servidor) codigoRun(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Context *gwContexto `json:"context"`
		Code    string      `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&p); err != nil || p.Code == "" {
		malaPeticion(w, "code is required")
		return
	}
	id := idDe(r)
	idioma, cid := "python", ""
	if p.Context != nil {
		idioma, cid = lenguaje(p.Context.Language), p.Context.ID
	}
	porDefecto := cid == ""
	var resp *http.Response
	for intento := 0; intento < 2; intento++ {
		if porDefecto {
			var err error
			if cid, err = s.contextoDefecto(r.Context(), id, idioma, intento > 0); err != nil {
				s.falloExecd(w, err, "")
				return
			}
		}
		var err error
		resp, err = s.gw(r.Context(), "POST", rutaSB(id)+"/code", map[string]any{"code": p.Code, "context_id": cid, "language": idioma})
		if err == nil && resp.StatusCode >= 300 {
			err = s.leerError(resp)
			resp.Body.Close()
		}
		if e, ok := err.(*errGW); ok && e.Status == http.StatusNotFound && porDefecto && intento == 0 {
			continue // el contexto por defecto murió: se crea otro
		}
		if err != nil {
			s.falloExecd(w, err, "CONTEXT_NOT_FOUND")
			return
		}
		break
	}
	defer resp.Body.Close()
	f := nuevoSSE(w)
	defer f.cerrar()
	f.enviar(evento{Type: "init", Text: cid})
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var ev eventoCodigo
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "stdout", "stderr":
			if ev.Text != "" {
				f.enviar(evento{Type: ev.Type, Text: ev.Text})
			}
		case "result":
			if ev.ExecutionCount > 0 {
				f.enviar(evento{Type: "execution_count", ExecutionCount: ev.ExecutionCount})
			}
			res := map[string]any{}
			for k, v := range ev.Results {
				if k == "text/plain" {
					k = "text"
				}
				res[k] = v
			}
			if _, ok := res["text"]; !ok && ev.Text != "" {
				res["text"] = ev.Text
			}
			f.enviar(evento{Type: "result", Results: res})
		case "error":
			f.enviar(evento{Type: "error", Error: &errorEvento{EName: ev.Ename, EValue: ev.Evalue, Traceback: append([]string{}, ev.Traceback...)}})
		case "done":
			var ms int64
			if ev.DurationMS != nil {
				ms = *ev.DurationMS
			}
			f.enviar(evento{Type: "execution_complete", ExecutionTime: ms, ExecutionCount: ev.ExecutionCount})
			return
		}
	}
	f.fallo("RuntimeError", "the gateway closed the stream before the code finished")
}

func (s *Servidor) codigoInterrumpir(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("id")
	if cid == "" {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "id is required")
		return
	}
	if err := s.llamarGW(r.Context(), "DELETE", rutaSB(idDe(r))+"/code/contexts/"+url.PathEscape(cid)+"/execution", nil, nil); err != nil {
		s.falloExecd(w, err, "CONTEXT_NOT_FOUND")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Ficheros ----

type gwStat struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// modoOSB pasa el modo octal del gateway ("0644") al entero de OSB (644).
func modoOSB(m string) int {
	m = strings.TrimLeft(m, "0")
	if len(m) > 4 {
		m = m[len(m)-4:]
	}
	n, _ := strconv.Atoi(m)
	return n
}

// modoGW pasa el modo de OSB (644, dígitos octales) al octal del gateway.
func modoGW(m int) (string, error) {
	t := strconv.Itoa(m)
	if m < 0 || m > 7777 || strings.ContainsAny(t, "89") {
		return "", fmt.Errorf("invalid mode %d: octal digits like 644", m)
	}
	return fmt.Sprintf("0%03s", t), nil
}

func infoDe(p string, st gwStat) map[string]any {
	tipo := "file"
	if st.IsDir {
		tipo = "directory"
	}
	if st.Path != "" {
		p = st.Path
	}
	return map[string]any{"path": p, "type": tipo, "size": st.Size, "modified_at": st.ModTime,
		"created_at": st.ModTime, "owner": "", "group": "", "mode": modoOSB(st.Mode)}
}

func rutaFich(id, p string, extra url.Values) string {
	v := url.Values{"path": {p}}
	for k, x := range extra {
		v[k] = x
	}
	return rutaSB(id) + "/files?" + v.Encode()
}

func (s *Servidor) stat(ctx context.Context, id, p string) (gwStat, error) {
	var st gwStat
	err := s.llamarGW(ctx, "GET", rutaFich(id, p, url.Values{"stat": {"1"}}), nil, &st)
	return st, err
}

func rutas(r *http.Request) ([]string, bool) {
	ps := r.URL.Query()["path"]
	return ps, len(ps) > 0
}

func (s *Servidor) ficherosInfo(w http.ResponseWriter, r *http.Request) {
	ps, ok := rutas(r)
	if !ok {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "path is required")
		return
	}
	out := map[string]any{}
	for _, p := range ps {
		st, err := s.stat(r.Context(), idDe(r), p)
		if err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
		out[p] = infoDe(p, st)
	}
	escribirJSON(w, http.StatusOK, out)
}

func (s *Servidor) ficherosBorrar(w http.ResponseWriter, r *http.Request) {
	ps, ok := rutas(r)
	if !ok {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "path is required")
		return
	}
	for _, p := range ps {
		if err := s.llamarGW(r.Context(), "DELETE", rutaFich(idDe(r), p, nil), nil, nil); err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// argv corre un comando sin shell en el sandbox y falla si no sale 0.
func (s *Servidor) argv(ctx context.Context, id string, cmd ...string) error {
	var res struct {
		ExitCode int    `json:"exit_code"`
		Stderr   []byte `json:"stderr"`
	}
	if err := s.llamarGW(ctx, "POST", rutaSB(id)+"/exec?wait=1", map[string]any{"cmd": cmd, "timeout_seconds": 60}, &res); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &errGW{Status: http.StatusBadRequest, Msg: fmt.Sprintf("%s: %s", cmd[0], strings.TrimSpace(string(res.Stderr)))}
	}
	return nil
}

type permiso struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  *int   `json:"mode"`
}

// permisos aplica modo y dueño con chmod/chown (sin shell).
func (s *Servidor) permisos(ctx context.Context, id, p string, pm permiso) error {
	if pm.Mode != nil {
		m, err := modoGW(*pm.Mode)
		if err != nil {
			return &errGW{Status: http.StatusBadRequest, Msg: err.Error()}
		}
		if err := s.argv(ctx, id, "chmod", m, "--", p); err != nil {
			return err
		}
	}
	if pm.Owner != "" || pm.Group != "" {
		dueño := pm.Owner
		if pm.Group != "" {
			dueño += ":" + pm.Group
		}
		if err := s.argv(ctx, id, "chown", dueño, "--", p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Servidor) ficherosPermisos(w http.ResponseWriter, r *http.Request) {
	var p map[string]permiso
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		malaPeticion(w, "body must map paths to {owner, group, mode}")
		return
	}
	for _, k := range ordenadas(p) {
		if err := s.permisos(r.Context(), idDe(r), k, p[k]); err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func ordenadas[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Servidor) ficherosMover(w http.ResponseWriter, r *http.Request) {
	var p []struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		malaPeticion(w, "body must be an array of {src, dest}")
		return
	}
	for _, m := range p {
		if m.Src == "" || m.Dest == "" {
			malaPeticion(w, "src and dest are required")
			return
		}
		if err := s.llamarGW(r.Context(), "POST", rutaSB(idDe(r))+"/files/move", map[string]any{"from": m.Src, "to": m.Dest, "overwrite": true}, nil); err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ficherosBuscar usa files/search del gateway y completa cada resultado con su
// stat (hasta maxBusqueda, de ocho en ocho).
func (s *Servidor) ficherosBuscar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, patron := q.Get("path"), q.Get("pattern")
	if dir == "" {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "path is required")
		return
	}
	if patron == "" || patron == "**" {
		patron = "*"
	}
	id := idDe(r)
	var b struct {
		Matches []string `json:"matches"`
	}
	ruta := rutaSB(id) + "/files/search?" + url.Values{"path": {dir}, "glob": {path.Base(patron)}}.Encode()
	if err := s.llamarGW(r.Context(), "GET", ruta, nil, &b); err != nil {
		s.falloExecd(w, err, "FILE_NOT_FOUND")
		return
	}
	if len(b.Matches) > maxBusqueda {
		b.Matches = b.Matches[:maxBusqueda]
	}
	out := make([]map[string]any, len(b.Matches))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, m := range b.Matches {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := s.stat(r.Context(), id, m)
			if err != nil {
				st = gwStat{Path: m}
			}
			out[i] = infoDe(m, st)
		}(i, m)
	}
	wg.Wait()
	escribirJSON(w, http.StatusOK, out)
}

// leer baja un fichero entero (hasta maxLeerFichero).
func (s *Servidor) leer(ctx context.Context, id, p string) ([]byte, error) {
	resp, err := s.gw(ctx, "GET", rutaFich(id, p, nil), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, s.leerError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxLeerFichero+1))
	if err != nil {
		return nil, &errGW{Status: http.StatusBadGateway, Msg: "reading the file: " + err.Error()}
	}
	if len(b) > maxLeerFichero {
		return nil, &errGW{Status: http.StatusRequestEntityTooLarge, Msg: "file is larger than 64 MiB"}
	}
	return b, nil
}

func (s *Servidor) escribir(ctx context.Context, id, p, modo string, mkdir bool, cuerpo io.Reader) error {
	extra := url.Values{}
	if modo != "" {
		extra.Set("mode", modo)
	}
	if mkdir {
		extra.Set("mkdir", "1")
	}
	resp, err := s.API.Do(ctx, "PUT", rutaFich(id, p, extra), cuerpo, "application/octet-stream")
	if err != nil {
		return &errGW{Status: http.StatusBadGateway, Msg: "gateway unreachable: " + s.redactar(err.Error())}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return s.leerError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *Servidor) ficherosReemplazar(w http.ResponseWriter, r *http.Request) {
	var p map[string]struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&p); err != nil {
		malaPeticion(w, "body must map paths to {old, new}")
		return
	}
	id := idDe(r)
	out := map[string]any{}
	for _, k := range ordenadas(p) {
		c := p[k]
		if c.Old == "" {
			malaPeticion(w, "old must not be empty")
			return
		}
		st, err := s.stat(r.Context(), id, k)
		if err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
		b, err := s.leer(r.Context(), id, k)
		if err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
		n := bytes.Count(b, []byte(c.Old))
		if n > 0 {
			nb := bytes.ReplaceAll(b, []byte(c.Old), []byte(c.New))
			if err := s.escribir(r.Context(), id, k, st.Mode, false, bytes.NewReader(nb)); err != nil {
				s.falloExecd(w, err, "FILE_NOT_FOUND")
				return
			}
		}
		out[k] = map[string]int{"replacedCount": n}
	}
	escribirJSON(w, http.StatusOK, out)
}

// ficherosSubir lee el multipart del SDK (pares metadata + file) y sube cada
// fichero sin cargarlo en memoria.
func (s *Servidor) ficherosSubir(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		malaPeticion(w, "multipart/form-data body expected")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	id := idDe(r)
	type meta struct {
		Path  string `json:"path"`
		Owner string `json:"owner"`
		Group string `json:"group"`
		Mode  *int   `json:"mode"`
	}
	var pendiente *meta
	subidos := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			malaPeticion(w, "invalid multipart body: "+err.Error())
			return
		}
		switch part.FormName() {
		case "metadata":
			var m meta
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&m); err != nil || m.Path == "" {
				errOSB(w, http.StatusBadRequest, "INVALID_FILE_METADATA", "metadata must be JSON with a path")
				return
			}
			pendiente = &m
		case "file":
			if pendiente == nil {
				errOSB(w, http.StatusBadRequest, "INVALID_FILE_METADATA", "each file part must follow its metadata part")
				return
			}
			modo := ""
			if pendiente.Mode != nil {
				if modo, err = modoGW(*pendiente.Mode); err != nil {
					errOSB(w, http.StatusBadRequest, "INVALID_FILE_METADATA", err.Error())
					return
				}
			}
			if err := s.escribir(r.Context(), id, pendiente.Path, modo, true, part); err != nil {
				s.falloExecd(w, err, "FILE_NOT_FOUND")
				return
			}
			if pendiente.Owner != "" || pendiente.Group != "" {
				if err := s.permisos(r.Context(), id, pendiente.Path, permiso{Owner: pendiente.Owner, Group: pendiente.Group}); err != nil {
					s.falloExecd(w, err, "FILE_NOT_FOUND")
					return
				}
			}
			pendiente = nil
			subidos++
		}
		part.Close()
	}
	if subidos == 0 {
		errOSB(w, http.StatusBadRequest, "INVALID_FILE", "no file part in the body")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// rango interpreta un único rango de bytes (a-b, a-, -n) sobre un tamaño.
func rango(h string, tam int64) (ini, fin int64, ok bool) {
	esp, found := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !found || strings.Contains(esp, ",") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(esp, "-")
	if !found {
		return 0, 0, false
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		ini, fin = max(tam-n, 0), tam-1
	default:
		x, err := strconv.ParseInt(a, 10, 64)
		if err != nil || x < 0 {
			return 0, 0, false
		}
		ini, fin = x, tam-1
		if b != "" {
			y, err := strconv.ParseInt(b, 10, 64)
			if err != nil || y < x {
				return 0, 0, false
			}
			fin = min(y, tam-1)
		}
	}
	return ini, fin, ini < tam
}

// ficherosBajar sirve la descarga con Range (206/416) o por líneas
// (offset/limit), como execd.
func (s *Servidor) ficherosBajar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := q.Get("path")
	if p == "" {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "path is required")
		return
	}
	id := idDe(r)
	h := r.Header.Get("Range")
	if q.Get("offset") != "" || q.Get("limit") != "" {
		if h != "" {
			malaPeticion(w, "offset/limit and Range are mutually exclusive")
			return
		}
		s.bajarLineas(w, r, id, p)
		return
	}
	st, err := s.stat(r.Context(), id, p)
	if err != nil {
		s.falloExecd(w, err, "FILE_NOT_FOUND")
		return
	}
	ini, fin := int64(0), st.Size-1
	parcial := false
	if h != "" {
		var ok bool
		if ini, fin, ok = rango(h, st.Size); !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", st.Size))
			errOSB(w, http.StatusRequestedRangeNotSatisfiable, "INVALID_REQUEST_BODY", "range not satisfiable")
			return
		}
		parcial = true
	}
	resp, err := s.gw(r.Context(), "GET", rutaFich(id, p, nil), nil)
	if err == nil && resp.StatusCode >= 300 {
		err = s.leerError(resp)
		resp.Body.Close()
	}
	if err != nil {
		s.falloExecd(w, err, "FILE_NOT_FOUND")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	n := fin - ini + 1
	if st.Size == 0 {
		n = 0
	}
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	if parcial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", ini, fin, st.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	io.CopyN(io.Discard, resp.Body, ini)
	io.CopyN(w, resp.Body, n)
}

func (s *Servidor) bajarLineas(w http.ResponseWriter, r *http.Request, id, p string) {
	q := r.URL.Query()
	desde, cuantas := 1, -1
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			malaPeticion(w, "offset is a 1-based line number")
			return
		}
		desde = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			malaPeticion(w, "limit must be >= 0")
			return
		}
		cuantas = n
	}
	b, err := s.leer(r.Context(), id, p)
	if err != nil {
		s.falloExecd(w, err, "FILE_NOT_FOUND")
		return
	}
	lineas := strings.SplitAfter(string(b), "\n")
	if n := len(lineas); n > 0 && lineas[n-1] == "" {
		lineas = lineas[:n-1]
	}
	ini := min(desde-1, len(lineas))
	fin := len(lineas)
	if cuantas >= 0 {
		fin = min(ini+cuantas, len(lineas))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, strings.Join(lineas[ini:fin], ""))
}

type gwLista struct {
	Path    string `json:"path"`
	Entries []struct {
		Name    string    `json:"name"`
		Type    string    `json:"type"`
		Size    int64     `json:"size"`
		Mode    string    `json:"mode"`
		ModTime time.Time `json:"mod_time"`
	} `json:"entries"`
}

func (s *Servidor) dirListar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir := q.Get("path")
	if dir == "" {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "path is required")
		return
	}
	prof := 1
	if v := q.Get("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			malaPeticion(w, "depth must be >= 1")
			return
		}
		prof = min(n, 16)
	}
	id := idDe(r)
	out := []map[string]any{}
	var recorrer func(d string, nivel int) error
	recorrer = func(d string, nivel int) error {
		var l gwLista
		if err := s.llamarGW(r.Context(), "GET", rutaFich(id, d, url.Values{"list": {"1"}}), nil, &l); err != nil {
			return err
		}
		for _, e := range l.Entries {
			if len(out) >= 10000 {
				return nil
			}
			p := path.Join(d, e.Name)
			tipo := map[string]string{"dir": "directory", "file": "file", "symlink": "symlink"}[e.Type]
			if tipo == "" {
				tipo = "other"
			}
			out = append(out, map[string]any{"path": p, "type": tipo, "size": e.Size, "modified_at": e.ModTime,
				"created_at": e.ModTime, "owner": "", "group": "", "mode": modoOSB(e.Mode)})
			if e.Type == "dir" && nivel < prof {
				if err := recorrer(p, nivel+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := recorrer(dir, 1); err != nil {
		s.falloExecd(w, err, "FILE_NOT_FOUND")
		return
	}
	escribirJSON(w, http.StatusOK, out)
}

func (s *Servidor) dirCrear(w http.ResponseWriter, r *http.Request) {
	var p map[string]permiso
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		malaPeticion(w, "body must map paths to {owner, group, mode}")
		return
	}
	id := idDe(r)
	for _, k := range ordenadas(p) {
		if err := s.llamarGW(r.Context(), "POST", rutaSB(id)+"/files/mkdir", map[string]string{"path": k}, nil); err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
		if err := s.permisos(r.Context(), id, k, p[k]); err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// dirBorrar borra directorios con su contenido (rm -rf, sin shell).
func (s *Servidor) dirBorrar(w http.ResponseWriter, r *http.Request) {
	ps, ok := rutas(r)
	if !ok {
		errOSB(w, http.StatusBadRequest, "MISSING_QUERY", "path is required")
		return
	}
	for _, p := range ps {
		if p == "" || path.Clean(p) == "/" {
			malaPeticion(w, "refusing to remove /")
			return
		}
		if err := s.argv(r.Context(), idDe(r), "rm", "-rf", "--", p); err != nil {
			s.falloExecd(w, err, "FILE_NOT_FOUND")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Métricas ----

// guionMetricas lee CPU y memoria del invitado: dos muestras de /proc/stat
// separadas 200 ms.
const guionMetricas = `nproc; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; head -n1 /proc/stat; sleep 0.2; head -n1 /proc/stat`

func (s *Servidor) metricas(w http.ResponseWriter, r *http.Request) {
	var res struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout"`
	}
	id := idDe(r)
	if err := s.llamarGW(r.Context(), "POST", rutaSB(id)+"/exec?wait=1", map[string]any{"cmd": []string{"sh", "-c", guionMetricas}, "timeout_seconds": 10}, &res); err != nil {
		s.falloExecd(w, err, "")
		return
	}
	if res.ExitCode != 0 {
		errOSB(w, http.StatusInternalServerError, "RUNTIME_ERROR", "could not read /proc in the sandbox")
		return
	}
	cpus, total, libre, pct := analizarMetricas(string(res.Stdout))
	escribirJSON(w, http.StatusOK, map[string]any{"cpu_count": cpus, "cpu_used_pct": pct,
		"mem_total_mib": total / 1024, "mem_used_mib": (total - libre) / 1024, "timestamp": ahoraMS()})
}

// analizarMetricas lee la salida de guionMetricas: vCPUs, memoria total y libre
// en KiB y el % de CPU usada entre las dos muestras.
func analizarMetricas(out string) (cpus, totalKiB, libreKiB, pct float64) {
	var muestras [][]float64
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 1:
			cpus, _ = strconv.ParseFloat(f[0], 64)
		case len(f) >= 2 && f[0] == "MemTotal:":
			totalKiB, _ = strconv.ParseFloat(f[1], 64)
		case len(f) >= 2 && f[0] == "MemAvailable:":
			libreKiB, _ = strconv.ParseFloat(f[1], 64)
		case len(f) > 4 && f[0] == "cpu":
			var v []float64
			for _, x := range f[1:] {
				n, _ := strconv.ParseFloat(x, 64)
				v = append(v, n)
			}
			muestras = append(muestras, v)
		}
	}
	if len(muestras) == 2 {
		suma := func(v []float64) (t, ocio float64) {
			for i, x := range v {
				t += x
				if i == 3 || i == 4 { // idle, iowait
					ocio += x
				}
			}
			return
		}
		t0, o0 := suma(muestras[0])
		t1, o1 := suma(muestras[1])
		if dt := t1 - t0; dt > 0 {
			pct = 100 * (dt - (o1 - o0)) / dt
		}
	}
	return
}
