// Package osb es un shim compatible con OpenSandbox (OSB) 1.2.1 sobre la API /v1
// del gateway de sandboxes: el plano de control `sandbox-lifecycle` bajo /v1 y,
// en el mismo listener, el execd de cada sandbox bajo
// /sandboxes/{id}/port/44772, que es lo que devuelve `endpoints/44772`.
//
// Tiene la misma forma que internal/e2b: habla con el gateway por HTTP y no
// importa frontal, así un test de frontal puede montarlo delante sin ciclo. Las
// specs que sigue están copiadas en testdata/ (ver testdata/NOTICE).
package osb

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
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/e2b"
	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
)

// VersionOSB es la versión de OpenSandbox que sigue el shim.
const VersionOSB = "1.2.1"

// Puertos con significado para el SDK de OSB.
const (
	PuertoExecd  = 44772
	PuertoEgress = 18080
)

// EntrypointDefecto es el entrypoint que manda el SDK si no se le da otro: no
// hace nada, así que el shim no lanza ningún proceso por él.
var EntrypointDefecto = []string{"tail", "-f", "/dev/null"}

// API es lo que el shim necesita del cliente HTTP del gateway: una petición ya
// autenticada con el token del inquilino. cuerpo y tipo pueden ser nulos.
type API interface {
	Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error)
}

// Ingreso es la entrada HTTP del gateway (`-ingress-domain`): los puertos que no
// son de execd ni de egress se reenvían ahí con el Host reescrito.
type Ingreso struct {
	// Dominio es el -ingress-domain del gateway.
	Dominio string
	// URL es la dirección del gateway a la que se conecta el shim.
	URL *url.URL
	// Token es el del inquilino: va en Authorization, el invitado no lo ve.
	Token string
	// Transport es el del cliente del gateway (nil = el de por defecto).
	Transport http.RoundTripper
}

// Servidor es el shim. Es un http.Handler.
type Servidor struct {
	API API
	// Clave es la OPEN-SANDBOX-API-KEY que acepta el shim; sin ella no se acepta
	// ninguna petición.
	Clave string
	// Imagenes traduce image.uri a una plantilla de kindling (`-image-map`). Se
	// prueba la uri exacta y luego sin la etiqueta (`python:3.12` → `python`).
	Imagenes map[string]string
	// PlantillaDefecto es la plantilla de las imágenes sin correspondencia
	// (`-default-template`); vacía, esas imágenes son un 422.
	PlantillaDefecto string
	// Publico es el host[:puerto] que se anuncia en endpoints/{port}; vacío, el
	// Host de la petición.
	Publico string
	// Ingreso, si no es nil, sirve los demás puertos por la entrada del gateway.
	Ingreso *Ingreso
	// Log recibe una línea por petición; nunca lleva la clave.
	Log func(string)

	once      sync.Once
	mux       *http.ServeMux
	execd     *http.ServeMux
	egress    *http.ServeMux
	proxy     *httputil.ReverseProxy
	mu        sync.Mutex
	entradas  map[string][]string // entrypoint pedido por sandbox, si no es el por defecto
	cmds      map[string]*comando // comandos en primer plano, por id
	cmdOrden  []string
	contextos map[string]string // contexto por defecto de /code: sandbox+"\x00"+lenguaje
}

// Declarada es una operación de una spec de OSB que el shim reconoce y contesta
// 501 con motivo. docs/opensandbox.md las repite (lo comprueba el contrato).
type Declarada struct {
	Spec, Metodo, Ruta, Motivo string
}

const (
	specLifecycle   = "sandbox-lifecycle"
	specExecd       = "execd"
	specDiagnostico = "diagnostic"
)

var (
	motivoVault     = "credential vaults are not supported by the kindling gateway: inject secrets with `kling sbx` sessions or env"
	motivoSnapshot  = "per-sandbox snapshots are not exposed by the kindling gateway: build a template with `kling sbx template`"
	motivoTemplates = "OSB templates are not supported: kindling templates are built with `kling sbx template` and selected with -image-map"
	motivoDiag      = "the diagnostics API is not supported by the kindling gateway"
	motivoIsolated  = "isolated sessions are not supported: every kindling sandbox is already its own microVM"
	motivoIdentidad = "file operations as another uid/gid are not supported by the kindling gateway"
	motivoWatch     = "streaming metrics are not supported: poll GET /metrics"
)

// NoImplementadas son las operaciones declaradas como 501.
var NoImplementadas = func() []Declarada {
	var d []Declarada
	add := func(spec, motivo string, ops ...string) {
		for _, o := range ops {
			m, r, _ := strings.Cut(o, " ")
			d = append(d, Declarada{spec, m, r, motivo})
		}
	}
	add(specLifecycle, motivoVault,
		"POST /sandboxes/{sandboxId}/credential-vault", "GET /sandboxes/{sandboxId}/credential-vault",
		"PATCH /sandboxes/{sandboxId}/credential-vault", "DELETE /sandboxes/{sandboxId}/credential-vault",
		"GET /sandboxes/{sandboxId}/credential-vault/credentials",
		"GET /sandboxes/{sandboxId}/credential-vault/credentials/{credentialName}",
		"GET /sandboxes/{sandboxId}/credential-vault/bindings",
		"GET /sandboxes/{sandboxId}/credential-vault/bindings/{bindingName}")
	add(specLifecycle, motivoSnapshot, "GET /snapshots", "GET /snapshots/{snapshotId}",
		"DELETE /snapshots/{snapshotId}", "POST /sandboxes/{sandboxId}/snapshots")
	add(specLifecycle, motivoTemplates, "POST /templates", "GET /templates", "GET /templates/{templateId}",
		"DELETE /templates/{templateId}")
	add(specDiagnostico, motivoDiag, "GET /sandboxes/{sandboxId}/diagnostics/context",
		"GET /sandboxes/{sandboxId}/diagnostics/logs", "GET /sandboxes/{sandboxId}/diagnostics/events")
	add(specExecd, motivoWatch, "GET /metrics/watch")
	for _, o := range []string{"GET files/info", "DELETE files", "POST files/permissions", "POST files/mv",
		"GET files/search", "POST files/replace", "POST files/upload", "GET files/download",
		"GET directories/list", "POST directories", "DELETE directories"} {
		m, r, _ := strings.Cut(o, " ")
		add(specExecd, motivoIdentidad, m+" /v1/filesystem/{uid}/{gid}/"+r)
	}
	add(specExecd, motivoIsolated, "POST /v1/isolated/session", "GET /v1/isolated/sessions",
		"GET /v1/isolated/capabilities", "GET /v1/isolated/session/{sessionId}",
		"DELETE /v1/isolated/session/{sessionId}", "POST /v1/isolated/session/{sessionId}/run",
		"GET /v1/isolated/session/{sessionId}/runs/{runId}", "GET /v1/isolated/session/{sessionId}/runs/{runId}/logs",
		"GET /v1/isolated/session/{sessionId}/diff", "POST /v1/isolated/session/{sessionId}/commit",
		"GET /v1/isolated/session/{sessionId}/files/info", "GET /v1/isolated/session/{sessionId}/files/download",
		"POST /v1/isolated/session/{sessionId}/files/upload", "DELETE /v1/isolated/session/{sessionId}/files",
		"POST /v1/isolated/session/{sessionId}/files/mv", "POST /v1/isolated/session/{sessionId}/files/permissions",
		"POST /v1/isolated/session/{sessionId}/files/replace", "GET /v1/isolated/session/{sessionId}/files/search",
		"GET /v1/isolated/session/{sessionId}/directories/list", "POST /v1/isolated/session/{sessionId}/directories",
		"DELETE /v1/isolated/session/{sessionId}/directories")
	return d
}()

// ---- Utilidades HTTP ----

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
func (e *estadoW) Unwrap() http.ResponseWriter { return e.ResponseWriter }

func escribirJSON(w http.ResponseWriter, st int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	json.NewEncoder(w).Encode(v)
}

// errOSB es el ErrorResponse de las dos specs: {"code": "...", "message": "..."}.
func errOSB(w http.ResponseWriter, st int, code, msg string) {
	escribirJSON(w, st, map[string]string{"code": code, "message": msg})
}

// codigoRuta es el code de una ruta que no existe en ninguna spec: el test de
// contrato lo distingue de un 404 de "sandbox no encontrado".
const codigoRuta = "ROUTE_NOT_FOUND"

func (s *Servidor) logf(f string, a ...any) {
	if s.Log != nil {
		s.Log(s.redactar(fmt.Sprintf(f, a...)))
	}
}

// redactar tapa la clave en cualquier texto que salga del shim.
func (s *Servidor) redactar(t string) string {
	if s.Clave == "" {
		return t
	}
	return strings.ReplaceAll(t, s.Clave, "***")
}

func igual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (s *Servidor) claveValida(r *http.Request) bool {
	k := r.Header.Get("OPEN-SANDBOX-API-KEY")
	return s.Clave != "" && k != "" && igual(k, s.Clave)
}

// tokenExecd es el X-EXECD-ACCESS-TOKEN de un sandbox: HMAC de su id con la
// clave, sin estado, y el de un sandbox no sirve en otro.
func (s *Servidor) tokenExecd(id string) string {
	h := hmac.New(sha256.New, []byte(s.Clave))
	h.Write([]byte("osb-execd:" + id))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// IDaOSB es el id que ve OSB para un id del gateway (`host/máquina`): el mismo
// base32 que la entrada HTTP y el shim de E2B.
func IDaOSB(gw string) string { return hosts.IDaDNS(gw) }

// ---- Arranque y rutas ----

func (s *Servidor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.once.Do(s.iniciar)
	rw := &estadoW{ResponseWriter: w, st: 200}
	s.mux.ServeHTTP(rw, r)
	s.logf("%s %s -> %d", r.Method, r.URL.Path, rw.st)
}

func (s *Servidor) iniciar() {
	s.entradas = map[string][]string{}
	s.cmds = map[string]*comando{}
	s.contextos = map[string]string{}
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/sandboxes", s.control(s.crear))
	m.HandleFunc("GET /v1/sandboxes", s.control(s.listar))
	m.HandleFunc("GET /v1/sandboxes/{id}", s.control(s.ver))
	m.HandleFunc("DELETE /v1/sandboxes/{id}", s.control(s.borrar))
	m.HandleFunc("PATCH /v1/sandboxes/{id}/metadata", s.control(s.metadatos))
	m.HandleFunc("POST /v1/sandboxes/{id}/pause", s.control(s.pausar))
	m.HandleFunc("POST /v1/sandboxes/{id}/resume", s.control(s.reanudar))
	m.HandleFunc("POST /v1/sandboxes/{id}/renew-expiration", s.control(s.renovar))
	m.HandleFunc("GET /v1/sandboxes/{id}/endpoints/{port}", s.control(s.endpoint))
	m.HandleFunc("GET /v1/sandboxes/{id}/networkpolicy", s.control(s.politicaVer))
	m.HandleFunc("PUT /v1/sandboxes/{id}/networkpolicy", s.control(s.politicaPoner))
	m.HandleFunc("PATCH /v1/sandboxes/{id}/networkpolicy", s.control(s.politicaParchear))
	m.HandleFunc("DELETE /v1/sandboxes/{id}/networkpolicy", s.control(s.politicaQuitar))
	// Métricas de cliente del SDK (tiempo de creación…): se aceptan y se tiran.
	m.HandleFunc("POST /v1/metrics/events", s.control(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.WriteHeader(http.StatusNoContent)
	}))
	m.HandleFunc("/sandboxes/{id}/port/{port}", s.puerto)
	m.HandleFunc("/sandboxes/{id}/port/{port}/{resto...}", s.puerto)

	s.execd = http.NewServeMux()
	s.rutasExecd(s.execd)
	s.egress = http.NewServeMux()
	s.egress.HandleFunc("GET /policy", s.puertoEgress(s.politicaVer))
	s.egress.HandleFunc("PATCH /policy", s.puertoEgress(s.politicaParchear))
	s.egress.HandleFunc("DELETE /policy", s.puertoEgress(s.politicaQuitar))
	s.egress.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		errOSB(w, http.StatusNotImplemented, "NOT_SUPPORTED", "only GET, PATCH and DELETE /policy of the egress sidecar are served by kindling")
	})

	for _, d := range NoImplementadas {
		d := d
		h := func(w http.ResponseWriter, r *http.Request) {
			errOSB(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", d.Motivo)
		}
		if d.Spec == specExecd {
			s.execd.HandleFunc(d.Metodo+" "+d.Ruta, h)
		} else {
			m.HandleFunc(d.Metodo+" /v1"+d.Ruta, s.control(h))
		}
	}
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		errOSB(w, http.StatusNotFound, codigoRuta, "no such OpenSandbox route: "+r.Method+" "+r.URL.Path)
	})
	s.execd.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		errOSB(w, http.StatusNotFound, codigoRuta, "no such execd route: "+r.Method+" "+r.URL.Path)
	})
	if s.Ingreso != nil {
		s.proxy = s.proxyIngreso()
	}
	s.mux = m
}

// control envuelve una ruta del plano de control: exige la clave y traduce el
// {id} de OSB al id del gateway.
func (s *Servidor) control(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.claveValida(r) {
			errOSB(w, http.StatusUnauthorized, "SANDBOX::UNAUTHORIZED", "invalid or missing API key (OPEN-SANDBOX-API-KEY)")
			return
		}
		if id := r.PathValue("id"); id != "" {
			gw, ok := e2b.DecodificarID(id)
			if !ok {
				errOSB(w, http.StatusNotFound, "SANDBOX::NOT_FOUND", "sandbox not found")
				return
			}
			r.SetPathValue("id", gw)
		}
		h(w, r)
	}
}

type ctxID struct{}

func idDe(r *http.Request) string { v, _ := r.Context().Value(ctxID{}).(string); return v }

// puerto sirve /sandboxes/{id}/port/{port}/...: el execd (44772), el sidecar de
// egress (18080) o, con Ingreso, cualquier otro puerto del sandbox.
func (s *Servidor) puerto(w http.ResponseWriter, r *http.Request) {
	id, ok := e2b.DecodificarID(r.PathValue("id"))
	if !ok {
		errOSB(w, http.StatusNotFound, "SANDBOX::NOT_FOUND", "sandbox not found")
		return
	}
	p, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || p < 1 || p > 65535 {
		errOSB(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "invalid port")
		return
	}
	if !s.claveValida(r) && !(s.Clave != "" && igual(r.Header.Get("X-EXECD-ACCESS-TOKEN"), s.tokenExecd(id))) {
		errOSB(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing X-EXECD-ACCESS-TOKEN (use the headers returned by endpoints/{port})")
		return
	}
	resto := "/" + r.PathValue("resto")
	r2 := r.Clone(context.WithValue(r.Context(), ctxID{}, id))
	r2.URL.Path, r2.URL.RawPath = resto, ""
	switch p {
	case PuertoExecd:
		s.execd.ServeHTTP(w, r2)
	case PuertoEgress:
		s.egress.ServeHTTP(w, r2)
	default:
		if s.proxy == nil {
			errOSB(w, http.StatusNotImplemented, "NOT_IMPLEMENTED",
				fmt.Sprintf("port %d: this shim has no gateway ingress (start it with -ingress-domain, the gateway's)", p))
			return
		}
		r2.Header.Del("X-EXECD-ACCESS-TOKEN")
		r2.Header.Del("OPEN-SANDBOX-API-KEY")
		r2 = r2.WithContext(context.WithValue(r2.Context(), ctxPuerto{}, p))
		s.proxy.ServeHTTP(w, r2)
	}
}

type ctxPuerto struct{}

func (s *Servidor) proxyIngreso() *httputil.ReverseProxy {
	ing := s.Ingreso
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			p, _ := pr.In.Context().Value(ctxPuerto{}).(int)
			pr.SetURL(ing.URL)
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = fmt.Sprintf("%d-%s.%s", p, hosts.IDaDNS(idDe(pr.In)), ing.Dominio)
			pr.Out.Header.Set("Authorization", "Bearer "+ing.Token)
		},
		Transport: ing.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			errOSB(w, http.StatusBadGateway, "GATEWAY_ERROR", s.redactar("gateway ingress: "+err.Error()))
		},
	}
}

func (s *Servidor) puertoEgress(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", idDe(r))
		h(w, r)
	}
}

// ---- Gateway ----

type errGW struct {
	Status int
	Code   string
	Msg    string
}

func (e *errGW) Error() string { return e.Msg }

// sinRuta dice si el gateway no conoce la ruta (uno anterior a la función).
func (e *errGW) sinRuta() bool {
	return e.Status == http.StatusMethodNotAllowed || (e.Status == http.StatusNotFound && strings.HasPrefix(e.Msg, "no route "))
}

func (s *Servidor) gw(ctx context.Context, metodo, ruta string, cuerpo any) (*http.Response, error) {
	var rd io.Reader
	tipo := ""
	if cuerpo != nil {
		b, err := json.Marshal(cuerpo)
		if err != nil {
			return nil, err
		}
		rd, tipo = bytes.NewReader(b), "application/json"
	}
	resp, err := s.API.Do(ctx, metodo, ruta, rd, tipo)
	if err != nil {
		return nil, &errGW{Status: http.StatusBadGateway, Code: "bad_gateway", Msg: "gateway unreachable: " + s.redactar(err.Error())}
	}
	return resp, nil
}

func (s *Servidor) leerError(resp *http.Response) *errGW {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	json.Unmarshal(b, &e)
	m := e.Message
	if m == "" {
		m = strings.TrimSpace(string(b))
	}
	if m == "" {
		m = resp.Status
	}
	return &errGW{Status: resp.StatusCode, Code: e.Code, Msg: s.redactar(m)}
}

// llamarGW hace una petición JSON y decodifica la respuesta; >= 300 es *errGW.
func (s *Servidor) llamarGW(ctx context.Context, metodo, ruta string, cuerpo, out any) error {
	resp, err := s.gw(ctx, metodo, ruta, cuerpo)
	if err != nil {
		return err
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
		return &errGW{Status: http.StatusBadGateway, Code: "bad_gateway", Msg: "invalid gateway response"}
	}
	return nil
}

func rutaSB(id string) string { return "/v1/sandboxes/" + url.PathEscape(id) }

// codigoOSB es el code de OSB para un estado HTTP del plano de control.
func codigoOSB(st int) string {
	switch st {
	case 400:
		return "SANDBOX::INVALID_PARAMETER"
	case 401:
		return "SANDBOX::UNAUTHORIZED"
	case 403:
		return "SANDBOX::FORBIDDEN"
	case 404:
		return "SANDBOX::NOT_FOUND"
	case 409:
		return "SANDBOX::CONFLICT"
	case 413:
		return "SANDBOX::PAYLOAD_TOO_LARGE"
	case 422:
		return "SANDBOX::UNPROCESSABLE"
	case 429:
		return "SANDBOX::RATE_LIMITED"
	case 501:
		return "SANDBOX::NOT_IMPLEMENTED"
	}
	return "SANDBOX::INTERNAL_ERROR"
}

// fallo contesta un error del gateway en el plano de control. El code del
// gateway va en el mensaje, para quien depure.
func (s *Servidor) fallo(w http.ResponseWriter, err error) {
	e, ok := err.(*errGW)
	if !ok {
		errOSB(w, http.StatusInternalServerError, codigoOSB(500), s.redactar(err.Error()))
		return
	}
	msg := e.Msg
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	errOSB(w, e.Status, codigoOSB(e.Status), msg)
}

func noImplementado(w http.ResponseWriter, motivo string) {
	errOSB(w, http.StatusNotImplemented, codigoOSB(501), motivo)
}

// ---- Plano de control ----

type gwSandbox struct {
	ID           string            `json:"id"`
	Template     string            `json:"template"`
	Image        string            `json:"image"`
	State        string            `json:"state"`
	VCPUs        int               `json:"vcpus"`
	MemMiB       int               `json:"mem_mib"`
	Egress       string            `json:"egress"`
	AllowDomains []string          `json:"allow_domains"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    *time.Time        `json:"expires_at"`
	TTLSeconds   int               `json:"ttl_seconds"`
	Labels       map[string]string `json:"labels"`
}

// EstadoOSB traduce el estado del gateway al de OSB.
func EstadoOSB(st string) string {
	switch st {
	case "running":
		return "Running"
	case "paused", "frozen":
		return "Paused"
	case "created":
		return "Pending"
	case "stopped":
		return "Terminated"
	case "failed":
		return "Failed"
	}
	return "Pending"
}

func (s *Servidor) vista(g gwSandbox) map[string]any {
	md := g.Labels
	if md == nil {
		md = map[string]string{}
	}
	ep := EntrypointDefecto
	s.mu.Lock()
	if e, ok := s.entradas[g.ID]; ok {
		ep = e
	}
	s.mu.Unlock()
	v := map[string]any{
		"id": IDaOSB(g.ID), "status": map[string]any{"state": EstadoOSB(g.State)},
		"metadata": md, "entrypoint": ep, "createdAt": g.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if u := s.uriDe(g); u != "" {
		v["image"] = map[string]string{"uri": u}
	}
	if g.ExpiresAt != nil {
		v["expiresAt"] = g.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return v
}

// uriDe es la image.uri que se enseña: la del mapa que lleva a la plantilla, o
// la plantilla tal cual.
func (s *Servidor) uriDe(g gwSandbox) string {
	var cands []string
	for u, t := range s.Imagenes {
		if t == g.Template && g.Template != "" {
			cands = append(cands, u)
		}
	}
	if len(cands) > 0 {
		sort.Strings(cands)
		return cands[0]
	}
	if g.Template != "" {
		return g.Template
	}
	return g.Image
}

// plantillaDe resuelve image.uri a una plantilla.
func (s *Servidor) plantillaDe(uri string) string {
	if t, ok := s.Imagenes[uri]; ok {
		return t
	}
	sinEtiqueta := uri
	if i := strings.LastIndexByte(uri, ':'); i > strings.LastIndexByte(uri, '/') {
		sinEtiqueta = uri[:i]
	}
	if t, ok := s.Imagenes[sinEtiqueta]; ok {
		return t
	}
	return s.PlantillaDefecto
}

type reglaRed struct {
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Ports  []int  `json:"ports,omitempty"`
}

type politica struct {
	DefaultAction string     `json:"defaultAction,omitempty"`
	Egress        []reglaRed `json:"egress"`
}

type peticionCrear struct {
	Image *struct {
		URI string `json:"uri"`
	} `json:"image"`
	SnapshotID string `json:"snapshotId"`
	TemplateID string `json:"templateId"`
	Platform   *struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	} `json:"platform"`
	Timeout          *int              `json:"timeout"`
	ResourceLimits   map[string]string `json:"resourceLimits"`
	ResourceRequests map[string]string `json:"resourceRequests"`
	Env              map[string]string `json:"env"`
	Metadata         map[string]string `json:"metadata"`
	Lifecycle        json.RawMessage   `json:"lifecycle"`
	Entrypoint       []string          `json:"entrypoint"`
	NetworkPolicy    *politica         `json:"networkPolicy"`
	CredentialProxy  json.RawMessage   `json:"credentialProxy"`
	Volumes          json.RawMessage   `json:"volumes"`
	Extensions       map[string]string `json:"extensions"`
}

func nulo(m json.RawMessage) bool {
	t := strings.TrimSpace(string(m))
	return t == "" || t == "null" || t == "[]" || t == "{}"
}

// vcpusDe convierte una cantidad de CPU de Kubernetes ("500m", "2") a vCPUs
// enteras, redondeando hacia arriba.
func vcpusDe(q string) (int, error) {
	q = strings.TrimSpace(q)
	if strings.HasSuffix(q, "m") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(q, "m"), 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid cpu %q", q)
		}
		return int(math.Ceil(n / 1000)), nil
	}
	n, err := strconv.ParseFloat(q, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid cpu %q", q)
	}
	return int(math.Ceil(n)), nil
}

// memMiBDe convierte una cantidad de memoria de Kubernetes ("512Mi", "2Gi",
// "1G", bytes) a MiB, redondeando hacia arriba.
func memMiBDe(q string) (int, error) {
	q = strings.TrimSpace(q)
	mult := map[string]float64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12}
	f := 1.0
	for _, suf := range []string{"Ki", "Mi", "Gi", "Ti", "k", "K", "M", "G", "T"} {
		if strings.HasSuffix(q, suf) {
			f, q = mult[suf], strings.TrimSuffix(q, suf)
			break
		}
	}
	n, err := strconv.ParseFloat(q, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid memory %q", q)
	}
	return int(math.Ceil(n * f / (1 << 20))), nil
}

func igualesArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Servidor) crear(w http.ResponseWriter, r *http.Request) {
	var p peticionCrear
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "invalid request body: "+err.Error())
		return
	}
	switch {
	case p.SnapshotID != "":
		noImplementado(w, "snapshotId: creating from an OSB snapshot is not supported; use a kindling template")
		return
	case p.Extensions["poolRef"] != "":
		noImplementado(w, "extensions.poolRef: OSB pools are not supported; kindling keeps its own warm pool per template")
		return
	case p.Platform != nil && p.Platform.OS != "" && p.Platform.OS != "linux":
		noImplementado(w, "platform.os "+p.Platform.OS+": kindling sandboxes are Linux microVMs")
		return
	case !nulo(p.Volumes):
		noImplementado(w, "volumes: OSB host/PVC/OSSFS volumes are not supported; use kindling volumes through the gateway API")
		return
	case !nulo(p.CredentialProxy):
		noImplementado(w, "credentialProxy is not supported by the kindling gateway")
		return
	case !nulo(p.Lifecycle):
		noImplementado(w, "lifecycle hooks and renewal policies are not supported; renew with renew-expiration")
		return
	}
	for k := range p.ResourceLimits {
		if strings.Contains(strings.ToLower(k), "gpu") {
			noImplementado(w, "resourceLimits."+k+": GPUs are not supported by kindling microVMs")
			return
		}
	}
	plantilla := p.TemplateID
	uri := ""
	if plantilla == "" {
		if p.Image == nil || p.Image.URI == "" {
			errOSB(w, http.StatusBadRequest, codigoOSB(400), "image.uri or templateId is required")
			return
		}
		uri = p.Image.URI
		plantilla = s.plantillaDe(uri)
		if plantilla == "" {
			s.sinPlantilla(w, r.Context(), uri)
			return
		}
	}
	req := map[string]any{"template": plantilla}
	if p.Timeout != nil {
		if *p.Timeout < 60 {
			errOSB(w, http.StatusBadRequest, codigoOSB(400), "timeout must be at least 60 seconds")
			return
		}
		req["ttl_seconds"] = *p.Timeout
	}
	if c, ok := p.ResourceLimits["cpu"]; ok {
		n, err := vcpusDe(c)
		if err != nil {
			errOSB(w, http.StatusBadRequest, codigoOSB(400), "resourceLimits: "+err.Error())
			return
		}
		req["vcpus"] = n
	}
	if m, ok := p.ResourceLimits["memory"]; ok {
		n, err := memMiBDe(m)
		if err != nil {
			errOSB(w, http.StatusBadRequest, codigoOSB(400), "resourceLimits: "+err.Error())
			return
		}
		req["mem_mib"] = n
	}
	if len(p.Metadata) > 0 {
		req["labels"] = p.Metadata
	}
	if len(p.Env) > 0 {
		req["env"] = p.Env
	}
	if p.NetworkPolicy != nil {
		eg, doms, motivo := aEgress(*p.NetworkPolicy)
		if motivo != "" {
			noImplementado(w, "networkPolicy: "+motivo)
			return
		}
		req["egress"] = eg
		if len(doms) > 0 {
			req["allow_domains"] = doms
		}
	}
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "POST", "/v1/sandboxes", req, &g); err != nil {
		s.fallo(w, err)
		return
	}
	ep := p.Entrypoint
	if len(ep) == 0 {
		ep = EntrypointDefecto
	}
	if !igualesArgv(ep, EntrypointDefecto) {
		pet := map[string]any{"cmd": ep}
		if len(p.Env) > 0 {
			pet["env"] = envLista(p.Env)
		}
		if err := s.llamarGW(r.Context(), "POST", rutaSB(g.ID)+"/processes", pet, nil); err != nil {
			// Sin su entrypoint el sandbox no es el que se pidió: se borra.
			s.llamarGW(context.WithoutCancel(r.Context()), "DELETE", rutaSB(g.ID), nil, nil)
			s.fallo(w, err)
			return
		}
	}
	if !igualesArgv(ep, EntrypointDefecto) { // solo se guarda el que no es el de siempre
		s.mu.Lock()
		s.entradas[g.ID] = ep
		s.mu.Unlock()
	}
	v := s.vista(g)
	delete(v, "image")
	w.Header().Set("Location", "/v1/sandboxes/"+IDaOSB(g.ID))
	escribirJSON(w, http.StatusAccepted, v)
}

func envLista(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// sinPlantilla es el 422 de una image.uri sin correspondencia, con lo que hay.
func (s *Servidor) sinPlantilla(w http.ResponseWriter, ctx context.Context, uri string) {
	var ts []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	var nombres []string
	if s.llamarGW(ctx, "GET", "/v1/templates", nil, &ts) == nil {
		for _, t := range ts {
			if t.Kind == "" {
				nombres = append(nombres, t.Name)
			}
		}
	}
	var mapa []string
	for u, t := range s.Imagenes {
		mapa = append(mapa, u+"="+t)
	}
	sort.Strings(nombres)
	sort.Strings(mapa)
	msg := fmt.Sprintf("image %q has no kindling template: map it with -image-map uri=template or set -default-template (templates: %s; image map: %s)",
		uri, lista(nombres), lista(mapa))
	errOSB(w, http.StatusUnprocessableEntity, codigoOSB(422), msg)
}

func lista(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ", ")
}

func (s *Servidor) ver(w http.ResponseWriter, r *http.Request) {
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "GET", rutaSB(r.PathValue("id")), nil, &g); err != nil {
		s.fallo(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, s.vista(g))
}

func (s *Servidor) borrar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.llamarGW(r.Context(), "DELETE", rutaSB(id), nil, nil); err != nil {
		s.fallo(w, err)
		return
	}
	s.olvidar(id)
	w.WriteHeader(http.StatusNoContent)
}

// olvidar quita el estado local de un sandbox borrado.
func (s *Servidor) olvidar(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entradas, id)
	for k := range s.contextos {
		if strings.HasPrefix(k, id+"\x00") {
			delete(s.contextos, k)
		}
	}
}

// listar pagina por page/pageSize sobre el listado por limit/cursor del gateway:
// recorre todas sus páginas para dar totalItems exacto.
func (s *Servidor) listar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pagina, tam := 1, 20
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errOSB(w, http.StatusBadRequest, codigoOSB(400), "page must be >= 1")
			return
		}
		pagina = n
	}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			errOSB(w, http.StatusBadRequest, codigoOSB(400), "pageSize must be between 1 and 1000")
			return
		}
		tam = n
	}
	filtro, err := url.ParseQuery(q.Get("metadata"))
	if err != nil {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "invalid metadata filter")
		return
	}
	estados := map[string]bool{}
	for _, st := range q["state"] {
		for _, x := range strings.Split(st, ",") {
			if x = strings.TrimSpace(x); x != "" {
				estados[strings.ToLower(x)] = true
			}
		}
	}
	base := url.Values{"limit": {"1000"}}
	claves := make([]string, 0, len(filtro))
	for k := range filtro {
		claves = append(claves, k)
	}
	sort.Strings(claves)
	for _, k := range claves {
		base.Add("label", k+"="+filtro.Get(k))
	}
	var todos []gwSandbox
	cursor := ""
	for vuelta := 0; vuelta < 1000; vuelta++ {
		v := url.Values{}
		for k, x := range base {
			v[k] = x
		}
		if cursor != "" {
			v.Set("cursor", cursor)
		}
		resp, err := s.gw(r.Context(), "GET", "/v1/sandboxes?"+v.Encode(), nil)
		if err != nil {
			s.fallo(w, err)
			return
		}
		if resp.StatusCode >= 300 {
			e := s.leerError(resp)
			resp.Body.Close()
			s.fallo(w, e)
			return
		}
		var trozo []gwSandbox
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&trozo)
		resp.Body.Close()
		if err != nil {
			s.fallo(w, &errGW{Status: http.StatusBadGateway, Msg: "invalid gateway response"})
			return
		}
		todos = append(todos, trozo...)
		cursor = resp.Header.Get("X-Kindling-Next-Cursor")
		if cursor == "" {
			break
		}
	}
	var filtrados []gwSandbox
	for _, g := range todos {
		ok := len(estados) == 0 || estados[strings.ToLower(EstadoOSB(g.State))]
		for k := range filtro {
			if g.Labels[k] != filtro.Get(k) {
				ok = false
			}
		}
		if ok {
			filtrados = append(filtrados, g)
		}
	}
	total := len(filtrados)
	paginas := (total + tam - 1) / tam
	items := []map[string]any{}
	for i := (pagina - 1) * tam; i < total && i < pagina*tam; i++ {
		items = append(items, s.vista(filtrados[i]))
	}
	escribirJSON(w, http.StatusOK, map[string]any{"items": items, "pagination": map[string]any{
		"page": pagina, "pageSize": tam, "totalItems": total, "totalPages": paginas, "hasNextPage": pagina < paginas,
	}})
}

// metadatos usa PATCH …/metadata del gateway: una clave con null se quita.
func (s *Servidor) metadatos(w http.ResponseWriter, r *http.Request) {
	var p map[string]*string
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "body must be an object of string or null values")
		return
	}
	id := r.PathValue("id")
	var g gwSandbox
	err := s.llamarGW(r.Context(), "PATCH", rutaSB(id)+"/metadata", p, &g)
	if e, ok := err.(*errGW); ok && e.sinRuta() {
		noImplementado(w, "this gateway has no PATCH /v1/sandboxes/{id}/metadata yet: update kindling-sandbox")
		return
	}
	if err != nil {
		s.fallo(w, err)
		return
	}
	if g.ID == "" { // un gateway que no devuelve el sandbox: se relee
		if err := s.llamarGW(r.Context(), "GET", rutaSB(id), nil, &g); err != nil {
			s.fallo(w, err)
			return
		}
	}
	escribirJSON(w, http.StatusOK, s.vista(g))
}

func (s *Servidor) pausar(w http.ResponseWriter, r *http.Request) {
	if err := s.llamarGW(r.Context(), "POST", rutaSB(r.PathValue("id"))+"/pause", nil, nil); err != nil {
		s.fallo(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Servidor) reanudar(w http.ResponseWriter, r *http.Request) {
	if err := s.llamarGW(r.Context(), "POST", rutaSB(r.PathValue("id"))+"/resume", nil, nil); err != nil {
		s.fallo(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// renovar traduce la fecha absoluta de OSB a un ttl_seconds desde ahora.
func (s *Servidor) renovar(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil || p.ExpiresAt.IsZero() {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "expiresAt (RFC 3339) is required")
		return
	}
	seg := int(math.Ceil(time.Until(p.ExpiresAt).Seconds()))
	if seg < 1 {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "expiresAt must be in the future")
		return
	}
	var g gwSandbox
	if err := s.llamarGW(r.Context(), "POST", rutaSB(r.PathValue("id"))+"/renew", map[string]any{"ttl_seconds": seg}, &g); err != nil {
		s.fallo(w, err)
		return
	}
	fin := p.ExpiresAt
	if g.ExpiresAt != nil {
		fin = *g.ExpiresAt
	}
	escribirJSON(w, http.StatusOK, map[string]string{"expiresAt": fin.UTC().Format(time.RFC3339Nano)})
}

// endpoint devuelve la URL (sin esquema, como la del servidor de OSB) del puerto
// a través del propio shim, con la cabecera que lo autoriza.
func (s *Servidor) endpoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || p < 1 || p > 65535 {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "port must be between 1 and 65535")
		return
	}
	if r.URL.Query().Get("expires") != "" {
		noImplementado(w, "signed endpoints (expires) are not supported: use the returned headers")
		return
	}
	var g gwSandbox // el sandbox tiene que existir y ser del inquilino
	if err := s.llamarGW(r.Context(), "GET", rutaSB(id), nil, &g); err != nil {
		s.fallo(w, err)
		return
	}
	host := s.Publico
	if host == "" {
		host = r.Host
	}
	escribirJSON(w, http.StatusOK, map[string]any{
		"endpoint": fmt.Sprintf("%s/sandboxes/%s/port/%d", strings.TrimSuffix(host, "/"), IDaOSB(id), p),
		"headers":  map[string]string{"X-EXECD-ACCESS-TOKEN": s.tokenExecd(id)},
	})
}

// ---- Política de red ----

// esDominio dice si un target es un dominio (no una IP ni un CIDR).
func esDominio(t string) bool {
	if t == "" || net.ParseIP(t) != nil {
		return false
	}
	if _, _, err := net.ParseCIDR(t); err == nil {
		return false
	}
	return strings.ContainsAny(t, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

// aEgress traduce una NetworkPolicy de OSB a la salida del gateway. Solo valen
// reglas allow por dominio y sin puertos; lo demás devuelve el motivo del 501.
func aEgress(p politica) (egress string, dominios []string, motivo string) {
	def := p.DefaultAction
	if def == "" {
		def = "deny"
	}
	if def != "deny" && def != "allow" {
		return "", nil, "defaultAction must be allow or deny"
	}
	for _, r := range p.Egress {
		switch {
		case r.Action != "allow":
			return "", nil, "only allow rules are supported (the gateway has no deny lists)"
		case !esDominio(r.Target):
			return "", nil, fmt.Sprintf("target %q: only domain targets are supported, not IPs or CIDRs", r.Target)
		case len(r.Ports) > 0:
			return "", nil, "per-port rules are not supported"
		}
		dominios = append(dominios, r.Target)
	}
	if def == "allow" {
		return "internet", nil, ""
	}
	if len(dominios) == 0 {
		return "none", nil, ""
	}
	return "allowlist", dominios, ""
}

func deEgress(g gwSandbox) politica {
	switch g.Egress {
	case "internet":
		return politica{DefaultAction: "allow", Egress: []reglaRed{}}
	case "allowlist":
		p := politica{DefaultAction: "deny", Egress: []reglaRed{}}
		for _, d := range g.AllowDomains {
			p.Egress = append(p.Egress, reglaRed{Action: "allow", Target: d})
		}
		return p
	}
	return politica{DefaultAction: "deny", Egress: []reglaRed{}}
}

func estadoPolitica(p politica) map[string]any {
	modo := "deny_all"
	switch {
	case p.DefaultAction == "allow":
		modo = "allow_all"
	case len(p.Egress) > 0:
		modo = "allowlist"
	}
	return map[string]any{"status": "ok", "mode": modo, "policy": p}
}

func (s *Servidor) politicaActual(ctx context.Context, id string) (politica, error) {
	var g gwSandbox
	if err := s.llamarGW(ctx, "GET", rutaSB(id), nil, &g); err != nil {
		return politica{}, err
	}
	return deEgress(g), nil
}

func (s *Servidor) aplicar(w http.ResponseWriter, r *http.Request, p politica) {
	eg, doms, motivo := aEgress(p)
	if motivo != "" {
		noImplementado(w, motivo)
		return
	}
	pet := map[string]any{"egress": eg}
	if len(doms) > 0 {
		pet["allow_domains"] = doms
	}
	if err := s.llamarGW(r.Context(), "PATCH", rutaSB(r.PathValue("id"))+"/egress", pet, nil); err != nil {
		s.fallo(w, err)
		return
	}
	if p.Egress == nil {
		p.Egress = []reglaRed{}
	}
	if p.DefaultAction == "" {
		p.DefaultAction = "deny"
	}
	escribirJSON(w, http.StatusOK, estadoPolitica(p))
}

func (s *Servidor) politicaVer(w http.ResponseWriter, r *http.Request) {
	p, err := s.politicaActual(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fallo(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, estadoPolitica(p))
}

func (s *Servidor) politicaPoner(w http.ResponseWriter, r *http.Request) {
	var p politica
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "invalid NetworkPolicy")
		return
	}
	s.aplicar(w, r, p)
}

// politicaParchear añade reglas o sustituye las del mismo target.
func (s *Servidor) politicaParchear(w http.ResponseWriter, r *http.Request) {
	var reglas []reglaRed
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&reglas); err != nil {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "body must be an array of NetworkRule")
		return
	}
	p, err := s.politicaActual(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fallo(w, err)
		return
	}
	for _, n := range reglas {
		hecho := false
		for i := range p.Egress {
			if p.Egress[i].Target == n.Target {
				p.Egress[i], hecho = n, true
			}
		}
		if !hecho {
			p.Egress = append(p.Egress, n)
		}
	}
	s.aplicar(w, r, p)
}

// politicaQuitar borra las reglas de esos targets; los desconocidos se ignoran.
func (s *Servidor) politicaQuitar(w http.ResponseWriter, r *http.Request) {
	var targets []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&targets); err != nil {
		errOSB(w, http.StatusBadRequest, codigoOSB(400), "body must be an array of targets")
		return
	}
	p, err := s.politicaActual(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fallo(w, err)
		return
	}
	quitar := map[string]bool{}
	for _, t := range targets {
		quitar[t] = true
	}
	quedan := []reglaRed{}
	for _, x := range p.Egress {
		if !quitar[x.Target] {
			quedan = append(quedan, x)
		}
	}
	p.Egress = quedan
	s.aplicar(w, r, p)
}
