package e2b

// envd por sandbox: procesos (Connect RPC en JSON), sistema de ficheros y
// `/files`. Todo se traduce a la API /v1 del gateway.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type procConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  *string           `json:"cwd,omitempty"`
}

type proceso struct {
	sandbox string
	pid     uint32
	tag     string
	cfg     procConfig
	cancel  context.CancelFunc
	senal   atomic.Int32
}

func (s *Servidor) olvidar(sandbox string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pid, p := range s.procs {
		if p.sandbox == sandbox {
			p.cancel()
			delete(s.procs, pid)
		}
	}
	for k := range s.etiquetas {
		if strings.HasPrefix(k, sandbox+"\x00") {
			delete(s.etiquetas, k)
		}
	}
}

// unario lee el cuerpo JSON de una llamada unaria; devuelve false (y responde)
// si el cliente no habla JSON o el cuerpo no es válido.
func unario(w http.ResponseWriter, r *http.Request, v any) bool {
	if tipoContenido(r) != tipoUnario {
		rechazarTipo(w, r, tipoUnario)
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxSobre))
	if err == nil && len(strings.TrimSpace(string(b))) > 0 {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		escribirErrorUnario(w, &errConnect{cInvalido, "invalid JSON request: " + err.Error()})
		return false
	}
	return true
}

func (s *Servidor) errConnectGW(e error) *errConnect {
	var g *errGW
	if errors.As(e, &g) {
		return &errConnect{connectDeHTTP(g.Status), g.Msg}
	}
	return &errConnect{cInterno, s.redactar(e.Error())}
}

// ---- Procesos ----

type selector struct {
	PID *uint32 `json:"pid"`
	Tag *string `json:"tag"`
}

func (s *Servidor) buscar(id string, sel selector) *proceso {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.procs {
		if p.sandbox != id {
			continue
		}
		if (sel.PID != nil && p.pid == *sel.PID) || (sel.Tag != nil && p.tag == *sel.Tag) {
			return p
		}
	}
	return nil
}

type execEvento struct {
	Stream     string `json:"stream"`
	Data       string `json:"data"`
	Exit       *int   `json:"exit"`
	DurationMS int    `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Error      string `json:"error"`
}

func (s *Servidor) procStart(w http.ResponseWriter, r *http.Request, id string) {
	if tipoContenido(r) != tipoStreaming {
		rechazarTipo(w, r, tipoStreaming)
		return
	}
	fl := nuevoFlujo(w)
	_, carga, err := leerSobre(r.Body)
	if err != nil {
		fl.fin(&errConnect{cInvalido, err.Error()})
		return
	}
	var req struct {
		Process procConfig `json:"process"`
		PTY     *struct{}  `json:"pty"`
		Tag     string     `json:"tag"`
	}
	if err := json.Unmarshal(carga, &req); err != nil {
		fl.fin(&errConnect{cInvalido, "invalid JSON request: " + err.Error()})
		return
	}
	if req.PTY != nil {
		fl.fin(&errConnect{cNoImplementa, "PTY is not supported by the kindling gateway: run without pty"})
		return
	}
	if req.Process.Cmd == "" {
		fl.fin(&errConnect{cInvalido, "process.cmd is required"})
		return
	}
	cuerpo := map[string]any{"cmd": envolverBash(req.Process.Cmd, req.Process.Args)}
	if req.Process.Cwd != nil && *req.Process.Cwd != "" {
		cuerpo["dir"] = *req.Process.Cwd
	}
	if len(req.Process.Envs) > 0 {
		var env []string
		for k, v := range req.Process.Envs {
			env = append(env, k+"="+v)
		}
		cuerpo["env"] = env
	}
	ms, _ := strconv.Atoi(r.Header.Get("Connect-Timeout-Ms"))
	// Con procesos de fondo en el gateway, el comando es del sandbox y no de
	// esta conexión (fondo.go); sin ellos se cae al exec atado a la petición.
	if s.arrancarFondo(r, fl, id, req.Process, req.Tag, cuerpo, ms) {
		return
	}
	if ms > 0 {
		cuerpo["timeout_seconds"] = (ms + 999) / 1000
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resp, err := s.gw(ctx, "POST", rutaSB(id)+"/exec", cuerpo)
	if err != nil {
		fl.fin(&errConnect{cNoDisponible, err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fl.fin(s.errConnectGW(s.leerError(resp)))
		return
	}

	s.mu.Lock()
	s.pid++
	p := &proceso{sandbox: id, pid: s.pid, tag: req.Tag, cfg: req.Process, cancel: cancel}
	s.procs[p.pid] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.procs, p.pid)
		s.mu.Unlock()
	}()

	evento := func(k string, v any) error { return fl.mensaje(map[string]any{"event": map[string]any{k: v}}) }
	if evento("start", map[string]any{"pid": p.pid}) != nil {
		return
	}
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		linea, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(linea))) > 0 {
			var ev execEvento
			if json.Unmarshal(linea, &ev) != nil {
				fl.fin(&errConnect{cInterno, "invalid event from the gateway"})
				return
			}
			switch {
			case ev.Exit != nil:
				fin := map[string]any{"exitCode": *ev.Exit, "exited": true, "status": "exit status " + strconv.Itoa(*ev.Exit)}
				if ev.TimedOut {
					fin["error"] = "command timed out"
				}
				evento("end", fin)
				fl.fin(nil)
				return
			case ev.Error != "":
				fl.fin(&errConnect{cNoDisponible, s.redactar(ev.Error)})
				return
			case ev.Stream == "stdout" || ev.Stream == "stderr":
				if evento("data", map[string]any{ev.Stream: ev.Data}) != nil {
					return
				}
			}
		}
		if err != nil {
			if sg := p.senal.Load(); sg != 0 {
				evento("end", map[string]any{"exitCode": 128 + int(sg), "exited": true, "status": "signal: " + sigNombre(sg)})
				fl.fin(nil)
				return
			}
			if r.Context().Err() == nil {
				fl.fin(&errConnect{cNoDisponible, "the exec stream ended without an exit event"})
			}
			return
		}
	}
}

func sigNombre(n int32) string {
	if n == 9 {
		return "killed"
	}
	return "terminated"
}

func (s *Servidor) procList(w http.ResponseWriter, r *http.Request, id string) {
	if !unario(w, r, &struct{}{}) {
		return
	}
	s.mu.Lock()
	lista := []map[string]any{}
	for _, p := range s.procs {
		if p.sandbox != id {
			continue
		}
		e := map[string]any{"config": p.cfg, "pid": p.pid}
		if p.tag != "" {
			e["tag"] = p.tag
		}
		lista = append(lista, e)
	}
	s.mu.Unlock()
	lista = append(lista, s.listarFondo(r.Context(), id)...)
	escribirJSON(w, 200, map[string]any{"processes": lista})
}

// procSignal corta el flujo de exec: el gateway mata el comando al cerrarse la
// petición. No hay señales distintas de TERM y KILL.
func (s *Servidor) procSignal(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Process selector        `json:"process"`
		Signal  json.RawMessage `json:"signal"`
	}
	if !unario(w, r, &req) {
		return
	}
	sg := int32(15)
	switch strings.Trim(string(req.Signal), `"`) {
	case "SIGNAL_SIGKILL", "9":
		sg = 9
	case "SIGNAL_SIGTERM", "15", "SIGNAL_UNSPECIFIED", "0", "", "null":
	default:
		escribirErrorUnario(w, &errConnect{cInvalido, "unknown signal " + string(req.Signal)})
		return
	}
	p := s.buscar(id, req.Process)
	if p == nil {
		s.senalFondo(w, r, id, req.Process, sg)
		return
	}
	p.senal.Store(sg)
	p.cancel()
	escribirJSON(w, 200, map[string]any{})
}

// procCloseStdin: los comandos arrancan con stdin vacío y cerrado, así que no
// hay nada que cerrar.
func (s *Servidor) procCloseStdin(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Process selector `json:"process"`
	}
	if !unario(w, r, &req) {
		return
	}
	if s.buscar(id, req.Process) == nil {
		if _, err := s.pidFondo(r.Context(), id, req.Process); err != nil {
			escribirErrorUnario(w, s.errConnectGW(err))
			return
		}
	}
	escribirJSON(w, 200, map[string]any{})
}

// ---- Ficheros ----

func (s *Servidor) resolver(p string) string {
	if !path.IsAbs(p) {
		p = path.Join(s.Home, p)
	}
	return path.Clean(p)
}

type gwStat struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// modoNumero entiende el modo del gateway en octal ("0644") o como el de ls
// ("-rw-r--r--").
func modoNumero(m string) uint32 {
	if n, err := strconv.ParseUint(m, 8, 32); err == nil {
		return uint32(n)
	}
	if len(m) >= 10 {
		var n uint32
		for _, c := range m[len(m)-9:] {
			n <<= 1
			if c != '-' {
				n |= 1
			}
		}
		return n
	}
	return 0
}

func permisos(modo uint32, dir bool) string {
	b := []byte("----------")
	if dir {
		b[0] = 'd'
	}
	for i, c := range "rwxrwxrwx" {
		if modo&(1<<uint(8-i)) != 0 {
			b[i+1] = byte(c)
		}
	}
	return string(b)
}

// entrada arma el EntryInfo de filesystem.proto en su forma JSON (int64 como
// cadena, enum por nombre, Timestamp RFC 3339).
func entrada(ruta string, dir bool, size int64, modo uint32, mod time.Time) map[string]any {
	tipo := "FILE_TYPE_FILE"
	if dir {
		tipo = "FILE_TYPE_DIRECTORY"
	}
	return map[string]any{"name": path.Base(ruta), "type": tipo, "path": ruta,
		"size": strconv.FormatInt(size, 10), "mode": modo, "permissions": permisos(modo, dir),
		"modifiedTime": mod.UTC().Format(time.RFC3339Nano)}
}

func (s *Servidor) stat(ctx context.Context, id, ruta string) (map[string]any, error) {
	var st gwStat
	if err := s.llamarGW(ctx, "GET", rutaSB(id)+"/files?stat=1&path="+url.QueryEscape(ruta), nil, &st); err != nil {
		return nil, err
	}
	return entrada(ruta, st.IsDir, st.Size, modoNumero(st.Mode), st.ModTime), nil
}

// scriptBash es el envoltorio de /bin/bash: el SDK de E2B lanza cada comando
// como `/bin/bash -l -c CMD` y el envd real siempre trae bash, pero las
// imágenes de kindling no. Se usa bash si existe y, si no, /bin/sh con los
// mismos argumentos salvo `-l` (busybox no siempre lo acepta).
const scriptBash = `if [ -x /bin/bash ]; then exec /bin/bash "$@"; fi; if [ "$1" = -l ]; then shift; fi; exec /bin/sh "$@"`

// envolverBash arma el argv que se manda al gateway. Solo /bin/bash y bash se
// envuelven; cualquier otro cmd va tal cual.
func envolverBash(cmd string, args []string) []string {
	if cmd == "/bin/bash" || cmd == "bash" {
		return append([]string{"/bin/sh", "-c", scriptBash, "sh"}, args...)
	}
	return append([]string{cmd}, args...)
}

// desenvolverBash deshace envolverBash para que List devuelva lo que pidió el
// cliente.
func desenvolverBash(cmd []string) (string, []string) {
	if len(cmd) >= 4 && cmd[0] == "/bin/sh" && cmd[1] == "-c" && cmd[2] == scriptBash && cmd[3] == "sh" {
		return "/bin/bash", cmd[4:]
	}
	return cmd[0], cmd[1:]
}

type execResultado struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ejecutar corre un comando corto y espera su resultado.
func (s *Servidor) ejecutar(ctx context.Context, id string, cmd ...string) (stdout, stderr string, code int, err error) {
	var res execResultado
	if err = s.llamarGW(ctx, "POST", rutaSB(id)+"/exec?wait=1", map[string]any{"cmd": cmd, "timeout_seconds": 30}, &res); err != nil {
		return
	}
	o, _ := base64.StdEncoding.DecodeString(res.Stdout)
	e, _ := base64.StdEncoding.DecodeString(res.Stderr)
	return string(o), strings.TrimSpace(string(e)), res.ExitCode, nil
}

func (s *Servidor) fsStat(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Path string `json:"path"`
	}
	if !unario(w, r, &req) {
		return
	}
	if req.Path == "" {
		escribirErrorUnario(w, &errConnect{cInvalido, "path is required"})
		return
	}
	e, err := s.stat(r.Context(), id, s.resolver(req.Path))
	if err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	escribirJSON(w, 200, map[string]any{"entry": e})
}

func (s *Servidor) fsListDir(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Path  string `json:"path"`
		Depth uint32 `json:"depth"`
	}
	if !unario(w, r, &req) {
		return
	}
	if req.Path == "" {
		escribirErrorUnario(w, &errConnect{cInvalido, "path is required"})
		return
	}
	ruta := s.resolver(req.Path)
	st, err := s.stat(r.Context(), id, ruta)
	if err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	if st["type"] != "FILE_TYPE_DIRECTORY" {
		escribirErrorUnario(w, &errConnect{cInvalido, fmt.Sprintf("path %q is not a directory", ruta)})
		return
	}
	depth := max(req.Depth, 1)
	if entradas, err := s.listarGW(r.Context(), id, ruta, int(depth)); err == nil {
		escribirJSON(w, 200, map[string]any{"entries": entradas})
		return
	}
	// Respaldo: gateway sin files?list=1 (o con un listado truncado).
	out, errOut, code, err := s.ejecutar(r.Context(), id, "find", ruta, "-mindepth", "1",
		"-maxdepth", strconv.FormatUint(uint64(depth), 10), "-printf", `%y\t%s\t%m\t%T@\t%p\n`)
	if err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	if code != 0 {
		escribirErrorUnario(w, &errConnect{cInterno, s.redactar("listing failed (needs GNU find in the sandbox): " + errOut)})
		return
	}
	entradas := []map[string]any{}
	for _, l := range strings.Split(out, "\n") {
		c := strings.SplitN(l, "\t", 5)
		if len(c) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(c[1], 10, 64)
		modo, _ := strconv.ParseUint(c[2], 8, 32)
		seg, _ := strconv.ParseFloat(c[3], 64)
		entradas = append(entradas, entrada(c[4], c[0] == "d", size, uint32(modo), time.Unix(int64(seg), 0)))
	}
	escribirJSON(w, 200, map[string]any{"entries": entradas})
}

// gwListado es la respuesta de files?list=1 (api.FileList del gateway).
type gwListado struct {
	Entries []struct {
		Name    string    `json:"name"`
		Type    string    `json:"type"`
		Size    int64     `json:"size"`
		Mode    string    `json:"mode"`
		ModTime time.Time `json:"mod_time"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

var errListado = errors.New("gateway listing unavailable")

// listarGW lista ruta con GET files?list=1 del gateway, bajando por los
// subdirectorios hasta depth. Falla con errListado si el gateway no lo
// soporta o el listado viene truncado, para que el llamador use find.
func (s *Servidor) listarGW(ctx context.Context, id, ruta string, depth int) ([]map[string]any, error) {
	var ls gwListado
	if err := s.llamarGW(ctx, "GET", rutaSB(id)+"/files?list=1&path="+url.QueryEscape(ruta), nil, &ls); err != nil {
		return nil, errListado
	}
	if ls.Truncated {
		return nil, errListado
	}
	out := []map[string]any{}
	for _, e := range ls.Entries {
		p := path.Join(ruta, e.Name)
		modo, _ := strconv.ParseUint(e.Mode, 8, 32)
		dir := e.Type == "dir"
		out = append(out, entrada(p, dir, e.Size, uint32(modo), e.ModTime))
		if dir && depth > 1 {
			sub, err := s.listarGW(ctx, id, p, depth-1)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
	}
	return out, nil
}

func (s *Servidor) fsMakeDir(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Path string `json:"path"`
	}
	if !unario(w, r, &req) {
		return
	}
	if req.Path == "" {
		escribirErrorUnario(w, &errConnect{cInvalido, "path is required"})
		return
	}
	ruta := s.resolver(req.Path)
	if e, err := s.stat(r.Context(), id, ruta); err == nil {
		msg := fmt.Sprintf("path %q already exists", ruta)
		if e["type"] == "FILE_TYPE_DIRECTORY" {
			msg = fmt.Sprintf("directory already exists: %q", ruta)
		}
		escribirErrorUnario(w, &errConnect{cYaExiste, msg})
		return
	}
	if _, errOut, code, err := s.ejecutar(r.Context(), id, "mkdir", "-p", "--", ruta); err != nil || code != 0 {
		if err == nil {
			err = errors.New(s.redactar(errOut))
		}
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	e, err := s.stat(r.Context(), id, ruta)
	if err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	escribirJSON(w, 200, map[string]any{"entry": e})
}

func (s *Servidor) fsRemove(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Path string `json:"path"`
	}
	if !unario(w, r, &req) {
		return
	}
	if req.Path == "" {
		escribirErrorUnario(w, &errConnect{cInvalido, "path is required"})
		return
	}
	ruta := s.resolver(req.Path)
	if ruta == "/" {
		escribirErrorUnario(w, &errConnect{cInvalido, "refusing to remove /"})
		return
	}
	if _, err := s.stat(r.Context(), id, ruta); err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	if _, errOut, code, err := s.ejecutar(r.Context(), id, "rm", "-rf", "--", ruta); err != nil || code != 0 {
		if err == nil {
			err = errors.New(s.redactar(errOut))
		}
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	escribirJSON(w, 200, map[string]any{})
}

func (s *Servidor) fsMove(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if !unario(w, r, &req) {
		return
	}
	if req.Source == "" || req.Destination == "" {
		escribirErrorUnario(w, &errConnect{cInvalido, "source and destination are required"})
		return
	}
	src, dst := s.resolver(req.Source), s.resolver(req.Destination)
	if _, err := s.stat(r.Context(), id, src); err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	if _, errOut, code, err := s.ejecutar(r.Context(), id, "mv", "--", src, dst); err != nil || code != 0 {
		if err == nil {
			err = errors.New(s.redactar(errOut))
		}
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	e, err := s.stat(r.Context(), id, dst)
	if err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	escribirJSON(w, 200, map[string]any{"entry": e})
}

// ---- /files ----

func (s *Servidor) fileLeer(w http.ResponseWriter, r *http.Request, id string) {
	p := r.URL.Query().Get("path")
	if p == "" {
		errE2B(w, http.StatusBadRequest, "path is required")
		return
	}
	ruta := s.resolver(p)
	resp, err := s.gw(r.Context(), "GET", rutaSB(id)+"/files?path="+url.QueryEscape(ruta), nil)
	if err != nil {
		errE2B(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		e := s.leerError(resp)
		if e.Status == 404 {
			e.Msg = fmt.Sprintf("path '%s' does not exist", ruta)
		}
		errE2B(w, e.Status, e.Msg)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if l := resp.Header.Get("Content-Length"); l != "" {
		w.Header().Set("Content-Length", l)
	}
	io.Copy(w, resp.Body)
}

func (s *Servidor) subir(ctx context.Context, id, ruta string, cuerpo io.Reader) (map[string]any, *errGW) {
	resp, err := s.API.Do(ctx, "PUT", rutaSB(id)+"/files?mkdir=1&path="+url.QueryEscape(ruta), cuerpo, "application/octet-stream")
	if err != nil {
		return nil, &errGW{http.StatusBadGateway, "gateway unreachable: " + s.redactar(err.Error())}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, s.leerError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return map[string]any{"path": ruta, "name": path.Base(ruta), "type": "file"}, nil
}

// nombreParte es el filename de una parte multipart TAL CUAL: write_files del
// SDK manda varias partes sin ?path= y con la ruta absoluta como filename.
// multipart.Part.FileName no sirve: se queda con el último elemento de la ruta
// (filepath.Base), y el fichero acabaría colgando de Home.
func nombreParte(p *multipart.Part) string {
	_, ps, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return ps["filename"]
}

func (s *Servidor) fileEscribir(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20+1<<20)
	q := r.URL.Query().Get("path")
	var out []map[string]any
	fallo := func(e *errGW) { errE2B(w, e.Status, e.Msg) }
	if tipoContenido(r) == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			errE2B(w, http.StatusBadRequest, "invalid multipart body")
			return
		}
		for {
			var parte *multipart.Part
			parte, err = mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				errE2B(w, http.StatusBadRequest, "invalid multipart body")
				return
			}
			if parte.FormName() != "file" {
				continue
			}
			ruta := q
			if ruta == "" {
				ruta = nombreParte(parte)
			} else if len(out) > 0 {
				errE2B(w, http.StatusBadRequest, "several files with a single path")
				return
			}
			if ruta == "" {
				errE2B(w, http.StatusBadRequest, "path is required")
				return
			}
			e, eg := s.subir(r.Context(), id, s.resolver(ruta), parte)
			if eg != nil {
				fallo(eg)
				return
			}
			out = append(out, e)
		}
		if len(out) == 0 {
			errE2B(w, http.StatusBadRequest, "no file in the multipart body")
			return
		}
	} else {
		if q == "" {
			errE2B(w, http.StatusBadRequest, "path is required")
			return
		}
		e, eg := s.subir(r.Context(), id, s.resolver(q), r.Body)
		if eg != nil {
			fallo(eg)
			return
		}
		out = append(out, e)
	}
	escribirJSON(w, 200, out)
}
