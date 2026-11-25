// Package e2b es un shim compatible con E2B sobre la API /v1 del gateway de
// sandboxes: el plano de control REST de E2B (`/sandboxes`…) y, en el mismo
// listener, el agente envd de cada sandbox (Connect RPC en JSON y `/files`).
//
// La especificación pública que sigue vive en spec/. El shim no habla protobuf
// binario: un cliente que lo pida recibe un error Connect claro.
package e2b

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
)

// API es lo que el shim necesita del cliente HTTP del gateway: hacer una
// petición ya autenticada con el token del inquilino. cuerpo y tipo pueden ser
// nulos.
type API interface {
	Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error)
}

// EnvdVersion es la versión de envd que anuncia el shim: el SDK la usa para
// decidir qué funciones asume.
const EnvdVersion = "0.2.4"

// Servidor es el shim. Es un http.Handler.
type Servidor struct {
	API API
	// Clave es la API key de E2B que acepta el shim (cabecera X-API-Key). Es el
	// token del inquilino del gateway; sin ella no se acepta ninguna petición.
	Clave string
	// Dominio es el dominio base que anuncia a los clientes (E2B_DOMAIN).
	Dominio string
	// Home resuelve las rutas relativas de envd (por defecto /home/user).
	Home string
	// SandboxDebug es el id del gateway al que apunta el modo E2B_DEBUG (vacío:
	// sin modo debug). Solo aplica a las peticiones de HandlerDebug.
	SandboxDebug string
	// Log recibe una línea por petición; nunca lleva la clave.
	Log func(string)

	once  sync.Once
	mux   *http.ServeMux
	mu    sync.Mutex
	procs map[uint32]*proceso
	pid   uint32
	// etiquetas guarda la etiqueta (tag) de E2B de cada proceso de fondo, por
	// sandbox+"\x00"+tag: el núcleo no conoce etiquetas.
	etiquetas map[string]int
	// contextos es el contexto de código por defecto de cada sandbox y
	// lenguaje (sandbox+"\x00"+lenguaje → id del gateway).
	contextos map[string]string
}

// reID es el id que el shim expone a E2B: base32 en minúsculas, seguro en una
// ruta y en una etiqueta DNS. reGW es lo que se acepta como id del gateway
// (`host/ref`, por ejemplo).
var (
	reID = regexp.MustCompile(`^[a-z2-7]{1,208}$`)
	reGW = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/@=-]{0,127}$`)
)

// IDDebug es el id que el SDK de E2B usa con E2B_DEBUG=true.
const IDDebug = "debug_sandbox_id"

// DecodificarID deshace hosts.IDaDNS, la única codificación de ids del gateway
// a DNS (la misma que usa la entrada HTTP): el id que ve E2B es el de gateway
// en base32 minúsculas sin relleno, determinista y sin estado. Cabe en una
// etiqueta (`<puerto>-<id>` ≤ 63) si el id del gateway mide ≤ 35 bytes; con ids
// más largos solo funcionan la ruta y la cabecera E2b-Sandbox-Id, no el host.
// Falla si no es un id codificado canónico o no sale un id de gateway válido.
func DecodificarID(e string) (string, bool) {
	if !reID.MatchString(e) {
		return "", false
	}
	gw, err := hosts.IDdeDNS(e)
	if err != nil || hosts.IDaDNS(gw) != e || !reGW.MatchString(gw) {
		return "", false
	}
	return gw, true
}

type claveCtx struct{}

// HandlerDebug sirve el shim en el listener de depuración (E2B_DEBUG): ahí las
// peticiones de envd sin id o con debug_sandbox_id van al sandbox fijado en
// Servidor.SandboxDebug y no exigen X-Access-Token. Solo debe escuchar en
// loopback.
func (s *Servidor) HandlerDebug() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claveCtx{}, true)))
	})
}

func (s *Servidor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.once.Do(s.iniciar)
	rw := &estadoW{ResponseWriter: w, st: 200}
	s.mux.ServeHTTP(rw, r)
	s.logf("%s %s -> %d", r.Method, r.URL.Path, rw.st)
}

type estadoW struct {
	http.ResponseWriter
	st int
}

func (e *estadoW) WriteHeader(c int) { e.st = c; e.ResponseWriter.WriteHeader(c) }
func (e *estadoW) Flush() {
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Servidor) logf(f string, a ...any) {
	if s.Log != nil {
		s.Log(s.redactar(fmt.Sprintf(f, a...)))
	}
}

// redactar tapa la clave en cualquier texto que salga del shim (logs, errores).
func (s *Servidor) redactar(t string) string {
	if s.Clave == "" {
		return t
	}
	return strings.ReplaceAll(t, s.Clave, "***")
}

func (s *Servidor) iniciar() {
	if s.Home == "" {
		s.Home = "/home/user"
	}
	s.procs = map[uint32]*proceso{}
	s.etiquetas = map[string]int{}
	s.contextos = map[string]string{}
	m := http.NewServeMux()
	// Plano de control.
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.idEnvd(r); ok { // envd contesta 204; el plano de control, 200
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	m.HandleFunc("POST /sandboxes", s.control(s.crear))
	m.HandleFunc("POST /v2/sandboxes", s.control(s.crear))
	m.HandleFunc("GET /sandboxes", s.control(s.listar))
	m.HandleFunc("GET /v2/sandboxes", s.control(s.listar))
	m.HandleFunc("GET /sandboxes/{id}", s.control(s.ver))
	m.HandleFunc("DELETE /sandboxes/{id}", s.control(s.matar))
	m.HandleFunc("POST /sandboxes/{id}/timeout", s.control(s.timeout))
	m.HandleFunc("POST /sandboxes/{id}/refreshes", s.control(s.refrescar))
	m.HandleFunc("POST /sandboxes/{id}/connect", s.control(s.conectar))
	m.HandleFunc("POST /v2/sandboxes/{id}/connect", s.control(s.conectar))
	m.HandleFunc("POST /sandboxes/{id}/pause", s.control(s.pausar))
	m.HandleFunc("POST /sandboxes/{id}/resume", s.control(s.reanudar))
	m.HandleFunc("PUT /sandboxes/{id}/network", s.control(s.red))
	// envd: Connect RPC y ficheros.
	m.HandleFunc("POST /process.Process/Start", s.envd(s.procStart))
	m.HandleFunc("POST /process.Process/List", s.envd(s.procList))
	m.HandleFunc("POST /process.Process/Connect", s.envd(s.procConnect))
	m.HandleFunc("POST /process.Process/SendSignal", s.envd(s.procSignal))
	m.HandleFunc("POST /process.Process/CloseStdin", s.envd(s.procCloseStdin))
	m.HandleFunc("POST /filesystem.Filesystem/Stat", s.envd(s.fsStat))
	m.HandleFunc("POST /filesystem.Filesystem/ListDir", s.envd(s.fsListDir))
	m.HandleFunc("POST /filesystem.Filesystem/MakeDir", s.envd(s.fsMakeDir))
	m.HandleFunc("POST /filesystem.Filesystem/Remove", s.envd(s.fsRemove))
	m.HandleFunc("POST /filesystem.Filesystem/Move", s.envd(s.fsMove))
	m.HandleFunc("GET /files", s.envd(s.fileLeer))
	m.HandleFunc("POST /files", s.envd(s.fileEscribir))
	m.HandleFunc("GET /envs", s.envd(func(w http.ResponseWriter, r *http.Request, id string) { escribirJSON(w, 200, map[string]string{}) }))
	m.HandleFunc("POST /init", s.envd(func(w http.ResponseWriter, r *http.Request, id string) { w.WriteHeader(http.StatusNoContent) }))
	// Intérprete de código (e2b-code-interpreter, puerto 49999).
	m.HandleFunc("POST /execute", s.codigo(s.codigoEjecutar))
	m.HandleFunc("POST /contexts", s.codigo(s.ctxCrear))
	m.HandleFunc("GET /contexts", s.codigo(s.ctxListar))
	m.HandleFunc("DELETE /contexts/{cid}", s.codigo(s.ctxBorrar))
	m.HandleFunc("POST /contexts/{cid}/restart", s.codigo(func(w http.ResponseWriter, r *http.Request, id string) {
		errE2B(w, http.StatusNotImplemented, "restarting a code context is not supported by the kindling gateway: remove it and create another")
	}))
	m.HandleFunc("/", s.resto)
	s.mux = m
}

// noImplementadas son las rutas de E2B que el shim reconoce y contesta 501.
var noImplementadas = []struct{ prefijo, motivo string }{
	{"/process.Process/", "this process RPC is not supported by the kindling gateway (no interactive stdin or PTY)"},
	{"/filesystem.Filesystem/", "filesystem watchers are not supported by the kindling gateway"},
	{"/files/compose", "file composition is not supported by the kindling gateway"},
	{"/sandboxes/", "this E2B operation has no equivalent in the kindling gateway (fork, snapshots, metrics, logs)"},
	{"/v2/sandboxes/", "this E2B operation has no equivalent in the kindling gateway (logs)"},
	{"/sandboxes", "this E2B operation has no equivalent in the kindling gateway"},
	{"/templates", "E2B template builds are not supported: use `kling sbx template`"},
	{"/v2/templates", "E2B template builds are not supported: use `kling sbx template`"},
	{"/v3/templates", "E2B template builds are not supported: use `kling sbx template`"},
	{"/snapshots", "snapshots are not supported by the kindling gateway"},
	{"/teams/", "team metrics are not supported by the kindling gateway"},
	{"/nodes", "cluster administration is not supported by the kindling gateway"},
}

func (s *Servidor) resto(w http.ResponseWriter, r *http.Request) {
	for _, n := range noImplementadas {
		if strings.HasPrefix(r.URL.Path, n.prefijo) {
			if strings.HasPrefix(r.URL.Path, "/process.Process/") || strings.HasPrefix(r.URL.Path, "/filesystem.Filesystem/") {
				if _, ok := s.idEnvd(r); ok {
					escribirErrorUnario(w, &errConnect{cNoImplementa, n.motivo})
					return
				}
			}
			errE2B(w, http.StatusNotImplemented, n.motivo)
			return
		}
	}
	errE2B(w, http.StatusNotFound, "not found")
}

func escribirJSON(w http.ResponseWriter, st int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	json.NewEncoder(w).Encode(v)
}

// errE2B responde el Error de E2B: {"code": n, "message": "..."}.
func errE2B(w http.ResponseWriter, st int, msg string) {
	escribirJSON(w, st, map[string]any{"code": st, "message": msg})
}

func igual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (s *Servidor) claveValida(r *http.Request) bool {
	k := r.Header.Get("X-API-Key")
	return s.Clave != "" && k != "" && igual(k, s.Clave)
}

// tokenEnvd es el X-Access-Token de envd de un sandbox: derivado de la clave,
// así no hay estado y un token de un sandbox no sirve en otro.
func (s *Servidor) tokenEnvd(id string) string {
	h := hmac.New(sha256.New, []byte(s.Clave))
	h.Write([]byte("envd:" + id))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (s *Servidor) control(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.claveValida(r) {
			errE2B(w, http.StatusUnauthorized, "invalid or missing API key (X-API-Key)")
			return
		}
		if id := r.PathValue("id"); id != "" {
			gw, ok := DecodificarID(id)
			if !ok {
				errE2B(w, http.StatusNotFound, "sandbox not found")
				return
			}
			r.SetPathValue("id", gw) // los manejadores trabajan con el id del gateway
		}
		h(w, r)
	}
}

// idEnvd saca el id de gateway del sandbox al que va una petición de envd: el
// SDK usa el host `<puerto>-<id>.<dominio>` y manda además E2b-Sandbox-Id; con
// E2B_SANDBOX_URL solo llega la cabecera.
func (s *Servidor) idEnvd(r *http.Request) (string, bool) {
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, '-'); i > 0 && allDigits(host[:i]) {
		if id, ok := DecodificarID(host[i+1:]); ok {
			return id, true
		}
	}
	if id, ok := DecodificarID(r.Header.Get("E2b-Sandbox-Id")); ok {
		return id, true
	}
	return "", false
}

// esDebug dice si la petición llegó al listener de depuración y a qué sandbox
// del gateway apunta (id ausente o debug_sandbox_id).
func (s *Servidor) esDebug(r *http.Request) (string, bool) {
	if s.SandboxDebug == "" || r.Context().Value(claveCtx{}) == nil {
		return "", false
	}
	if _, ok := s.idEnvd(r); ok {
		return "", false
	}
	h := r.Header.Get("E2b-Sandbox-Id")
	host := strings.SplitN(strings.SplitN(r.Host, ":", 2)[0], ".", 2)[0]
	if h == "" || h == IDDebug || strings.HasSuffix(host, "-"+IDDebug) {
		return s.SandboxDebug, true
	}
	return "", false
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// envd envuelve un manejador de envd: resuelve el sandbox y exige el token de
// acceso que el shim entregó al crearlo (o la propia clave de la API).
func (s *Servidor) envd(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rpc := strings.HasPrefix(r.URL.Path, "/process.") || strings.HasPrefix(r.URL.Path, "/filesystem.")
		falla := func(st int, c, msg string) {
			if rpc {
				escribirErrorUnario(w, &errConnect{c, msg})
			} else {
				errE2B(w, st, msg)
			}
		}
		if id, ok := s.esDebug(r); ok {
			h(w, r, id)
			return
		}
		id, ok := s.idEnvd(r)
		if !ok {
			falla(404, cNoEncontrado, "sandbox not identified: use the <port>-<sandboxID>.<domain> host or the E2b-Sandbox-Id header with the sandboxID returned by create")
			return
		}
		if !s.claveValida(r) && !(s.Clave != "" && igual(r.Header.Get("X-Access-Token"), s.tokenEnvd(id))) {
			falla(401, cNoAutenticado, "invalid or missing X-Access-Token")
			return
		}
		h(w, r, id)
	}
}

// ---- Gateway ----

// gw hace una petición al gateway con cuerpo JSON opcional.
func (s *Servidor) gw(ctx context.Context, metodo, ruta string, cuerpo any) (*http.Response, error) {
	var r io.Reader
	tipo := ""
	if cuerpo != nil {
		b, err := json.Marshal(cuerpo)
		if err != nil {
			return nil, err
		}
		r, tipo = bytes.NewReader(b), "application/json"
	}
	resp, err := s.API.Do(ctx, metodo, ruta, r, tipo)
	if err != nil {
		return nil, fmt.Errorf("gateway unreachable: %s", s.redactar(err.Error()))
	}
	return resp, nil
}

type errGW struct {
	Status int
	Msg    string
}

func (e *errGW) Error() string { return e.Msg }

// leerError lee el cuerpo de error del gateway ({"message": ...}).
func (s *Servidor) leerError(resp *http.Response) *errGW {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Message string `json:"message"`
	}
	m := ""
	if json.Unmarshal(b, &e) == nil {
		m = e.Message
	}
	if m == "" {
		m = strings.TrimSpace(string(b))
	}
	if m == "" {
		m = resp.Status
	}
	return &errGW{Status: resp.StatusCode, Msg: s.redactar(m)}
}

// llamarGW hace una petición JSON y decodifica la respuesta. Un estado >= 300 es
// *errGW.
func (s *Servidor) llamarGW(ctx context.Context, metodo, ruta string, cuerpo, out any) error {
	resp, err := s.gw(ctx, metodo, ruta, cuerpo)
	if err != nil {
		return &errGW{Status: http.StatusBadGateway, Msg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return s.leerError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return &errGW{Status: http.StatusBadGateway, Msg: "invalid gateway response"}
	}
	return nil
}

func rutaSB(id string) string { return "/v1/sandboxes/" + url.PathEscape(id) }

func (s *Servidor) falloE2B(w http.ResponseWriter, err error) {
	if e, ok := err.(*errGW); ok {
		errE2B(w, e.Status, e.Msg)
		return
	}
	errE2B(w, http.StatusInternalServerError, s.redactar(err.Error()))
}

// ---- Plano de control ----

type gwSandbox struct {
	ID         string            `json:"id"`
	Template   string            `json:"template"`
	State      string            `json:"state"`
	VCPUs      int               `json:"vcpus"`
	MemMiB     int               `json:"mem_mib"`
	CreatedAt  time.Time         `json:"created_at"`
	ExpiresAt  time.Time         `json:"expires_at"`
	TTLSeconds int               `json:"ttl_seconds"`
	Labels     map[string]string `json:"labels"`
}

func estadoE2B(st string) string {
	switch st {
	case "paused", "suspended", "hibernated":
		return "paused"
	}
	return "running"
}

func (g gwSandbox) detalle() map[string]any {
	fin := g.ExpiresAt
	if fin.IsZero() {
		fin = g.CreatedAt.Add(time.Duration(g.TTLSeconds) * time.Second)
	}
	md := g.Labels
	if md == nil {
		md = map[string]string{}
	}
	return map[string]any{
		"templateID": g.Template, "sandboxID": hosts.IDaDNS(g.ID), "clientID": "kindling",
		"startedAt": g.CreatedAt, "endAt": fin,
		"cpuCount": max(g.VCPUs, 1), "memoryMB": max(g.MemMiB, 1), "diskSizeMB": 0,
		"metadata": md, "state": estadoE2B(g.State), "envdVersion": EnvdVersion,
	}
}

func (s *Servidor) crear(w http.ResponseWriter, r *http.Request) {
	var p struct {
		TemplateID string            `json:"templateID"`
		Timeout    int               `json:"timeout"`
		Metadata   map[string]string `json:"metadata"`
		EnvVars    map[string]string `json:"envVars"`
		Internet   *bool             `json:"allow_internet_access"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		errE2B(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if p.TemplateID == "" {
		errE2B(w, http.StatusBadRequest, "templateID is required")
		return
	}
	req := map[string]any{"template": p.TemplateID}
	if p.Timeout > 0 {
		req["ttl_seconds"] = p.Timeout
	}
	if len(p.Metadata) > 0 {
		req["labels"] = p.Metadata
	}
	// Las envs de Sandbox.create van al env del gateway: la sesión "default",
	// que ven los comandos y procesos de fondo sin sesión, como en E2B.
	if len(p.EnvVars) > 0 {
		req["env"] = p.EnvVars
	}
	if p.Internet != nil && !*p.Internet {
		req["egress"] = "none"
	}
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "POST", "/v1/sandboxes", req, &g); err != nil {
		s.falloE2B(w, err)
		return
	}
	escribirJSON(w, http.StatusCreated, s.sandboxCreado(g))
}

func (s *Servidor) sandboxCreado(g gwSandbox) map[string]any {
	m := map[string]any{
		"templateID": g.Template, "sandboxID": hosts.IDaDNS(g.ID), "clientID": "kindling",
		"envdVersion": EnvdVersion, "envdAccessToken": s.tokenEnvd(g.ID),
		"trafficAccessToken": s.tokenTrafico(g.ID), "domain": nil,
	}
	if s.Dominio != "" {
		m["domain"] = s.Dominio
	}
	return m
}

func (s *Servidor) listar(w http.ResponseWriter, r *http.Request) {
	filtro, err := url.ParseQuery(r.URL.Query().Get("metadata"))
	if err != nil {
		errE2B(w, http.StatusBadRequest, "invalid metadata filter")
		return
	}
	var gs []gwSandbox
	if err := s.llamarGW(r.Context(), "GET", "/v1/sandboxes", nil, &gs); err != nil {
		s.falloE2B(w, err)
		return
	}
	out := []map[string]any{}
	for _, g := range gs {
		ok := true
		for k := range filtro {
			if g.Labels[k] != filtro.Get(k) {
				ok = false
			}
		}
		if ok {
			out = append(out, g.detalle())
		}
	}
	escribirJSON(w, 200, out)
}

func (s *Servidor) ver(w http.ResponseWriter, r *http.Request) {
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "GET", rutaSB(r.PathValue("id")), nil, &g); err != nil {
		s.falloE2B(w, err)
		return
	}
	escribirJSON(w, 200, g.detalle())
}

func (s *Servidor) matar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.llamarGW(r.Context(), "DELETE", rutaSB(id), nil, nil); err != nil {
		s.falloE2B(w, err)
		return
	}
	s.olvidar(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Servidor) renovar(ctx context.Context, id string, seg int) error {
	return s.llamarGW(ctx, "POST", rutaSB(id)+"/renew", map[string]any{"ttl_seconds": seg}, nil)
}

func (s *Servidor) timeout(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Timeout int `json:"timeout"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p) != nil || p.Timeout < 1 {
		errE2B(w, http.StatusBadRequest, "timeout must be a positive number of seconds")
		return
	}
	if err := s.renovar(r.Context(), r.PathValue("id"), p.Timeout); err != nil {
		s.falloE2B(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Servidor) refrescar(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Duration int `json:"duration"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p) // cuerpo opcional
	if p.Duration < 1 {
		p.Duration = 60
	}
	if err := s.renovar(r.Context(), r.PathValue("id"), p.Duration); err != nil {
		s.falloE2B(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// conectar devuelve un sandbox existente (Sandbox.connect del SDK) y, si trae
// timeout, lo renueva. Uno pausado se reanuda antes y contesta 201, como E2B.
func (s *Servidor) conectar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p struct {
		Timeout int `json:"timeout"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "GET", rutaSB(id), nil, &g); err != nil {
		s.falloE2B(w, err)
		return
	}
	st := http.StatusOK
	if estadoE2B(g.State) == "paused" {
		if err := s.llamarGW(r.Context(), "POST", rutaSB(id)+"/resume", nil, &g); err != nil {
			s.falloE2B(w, err)
			return
		}
		st = http.StatusCreated
	}
	if p.Timeout > 0 {
		if err := s.renovar(r.Context(), id, p.Timeout); err != nil {
			s.falloE2B(w, err)
			return
		}
	}
	escribirJSON(w, st, s.sandboxCreado(g))
}

// pausar usa POST …/pause del gateway (la memoria se queda en el host).
func (s *Servidor) pausar(w http.ResponseWriter, r *http.Request) {
	if err := s.llamarGW(r.Context(), "POST", rutaSB(r.PathValue("id"))+"/pause", nil, nil); err != nil {
		s.falloE2B(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reanudar es el resume de E2B (obsoleto en su spec, pero el SDK viejo lo usa):
// POST …/resume del gateway, con timeout opcional.
func (s *Servidor) reanudar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p struct {
		Timeout int `json:"timeout"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "POST", rutaSB(id)+"/resume", nil, &g); err != nil {
		s.falloE2B(w, err)
		return
	}
	if p.Timeout > 0 {
		if err := s.renovar(r.Context(), id, p.Timeout); err != nil {
			s.falloE2B(w, err)
			return
		}
	}
	escribirJSON(w, http.StatusCreated, s.sandboxCreado(g))
}

// red traduce PUT /sandboxes/{id}/network a PATCH …/egress del gateway. Lo que
// el gateway expresa: dominios en allowOut (allowlist), sin internet
// (allow_internet_access false o denyOut 0.0.0.0/0) o abierto. IPs, CIDRs,
// denyOut parciales, reglas de transformación y proxy de salida son 501.
func (s *Servidor) red(w http.ResponseWriter, r *http.Request) {
	var p struct {
		AllowOut    []string        `json:"allowOut"`
		DenyOut     []string        `json:"denyOut"`
		DNSServers  json.RawMessage `json:"dnsServers"`
		EgressProxy json.RawMessage `json:"egressProxy"`
		Rules       json.RawMessage `json:"rules"`
		Internet    *bool           `json:"allow_internet_access"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		errE2B(w, http.StatusBadRequest, "invalid network config")
		return
	}
	vacio := func(m json.RawMessage) bool {
		t := strings.TrimSpace(string(m))
		return t == "" || t == "null" || t == "{}" || t == "[]"
	}
	if !vacio(p.EgressProxy) || !vacio(p.Rules) {
		errE2B(w, http.StatusNotImplemented, "egress proxies and transform rules are not supported by the kindling gateway")
		return
	}
	cerrado := p.Internet != nil && !*p.Internet
	for _, d := range p.DenyOut {
		if d != "0.0.0.0/0" {
			errE2B(w, http.StatusNotImplemented, "denyOut only supports 0.0.0.0/0 (no internet): the kindling gateway has no deny lists")
			return
		}
		cerrado = true
	}
	for _, a := range p.AllowOut {
		if !esDominio(a) {
			errE2B(w, http.StatusNotImplemented, fmt.Sprintf("allowOut %q: only domain names are supported by the kindling gateway, not IPs or CIDRs", a))
			return
		}
	}
	pet := map[string]any{"egress": "internet"}
	switch {
	case len(p.AllowOut) > 0:
		pet = map[string]any{"egress": "allowlist", "allow_domains": p.AllowOut}
	case cerrado:
		pet["egress"] = "none"
	}
	if err := s.llamarGW(r.Context(), "PATCH", rutaSB(r.PathValue("id"))+"/egress", pet, nil); err != nil {
		s.falloE2B(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func esDominio(t string) bool {
	if t == "" || net.ParseIP(t) != nil {
		return false
	}
	if _, _, err := net.ParseCIDR(t); err == nil {
		return false
	}
	return strings.ContainsAny(t, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}
