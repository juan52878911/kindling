package aigw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// RESPUESTAS DE UN BACKEND HACIA EL CLIENTE.
//
// Lo que contesta una réplica (o un upstream) no es de fiar: decide el código,
// el cuerpo y las cabeceras. El gateway copia el código y el cuerpo, acotado
// en bytes y en tiempo, y de las cabeceras solo un Content-Type de la lista
// blanca: con -no-auth en loopback, un invitado que sirviera HTML lo serviría
// bajo el origen del gateway.

// tiposPermitidos son los Content-Type que pasan del backend al cliente; el
// resto sale como application/octet-stream, que un navegador no interpreta.
var tiposPermitidos = map[string]bool{
	"application/json":     true,
	"text/event-stream":    true,
	"application/x-ndjson": true,
}

// tipoContenido devuelve el Content-Type que se le da al cliente: el tipo del
// backend sin parámetros si está en la lista blanca (JSON y SSE son UTF-8 por
// definición), application/json si no dijo nada y octet-stream si dijo otro.
func tipoContenido(ct string) string {
	if ct == "" {
		return "application/json"
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || !tiposPermitidos[mt] {
		return "application/octet-stream"
	}
	return mt
}

// esFlujo dice si una respuesta se manda a trozos según llegan: SSE y NDJSON.
func esFlujo(mt string) bool {
	return mt == "text/event-stream" || mt == "application/x-ndjson"
}

// claseCodigo agrupa un código HTTP por clase para las métricas: el código lo
// elige el backend y no puede abrir una serie nueva por cada valor.
func claseCodigo(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// devolver copia resp al cliente. name es el modelo (para métricas y log);
// stream fuerza el vaciado tras cada lectura aunque el tipo no sea de flujo
// (el proxy OpenAI lo sabe por "stream": true del cuerpo).
//
// Pasado MaxProxyBytes la respuesta se corta. Las cabeceras ya salieron, así
// que no cabe un error limpio: se aborta la conexión (abortar) para que el
// cliente vea un cuerpo incompleto como error y no un 200 con JSON a medias.
func (g *Gateway) devolver(w http.ResponseWriter, resp *http.Response, name string, stream bool) {
	code := resp.StatusCode
	if code < 200 || code > 599 {
		code = http.StatusBadGateway
	}
	g.met.inc(g.met.proxy, name, claseCodigo(code))
	mt := tipoContenido(resp.Header.Get("Content-Type"))
	stream = stream || esFlujo(mt)
	h := w.Header()
	h.Set("Content-Type", mt)
	h.Set("X-Content-Type-Options", "nosniff")
	if stream {
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(code)

	// Copia acotada. Con streaming se vacía el búfer tras cada lectura para
	// que cada token llegue cuando sale; sin él, al final.
	rc := http.NewResponseController(w)
	// Sin WriteTimeout en el servidor, el plazo se queda en la conexión:
	// quitarlo al acabar para la siguiente petición de la misma conexión.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if n+int64(k) > g.opts.MaxProxyBytes {
				log.Printf("proxy %s: answer over %d bytes, cut", name, g.opts.MaxProxyBytes)
				g.met.vonErr(name, "too_large")
				abortar(rc)
				return
			}
			n += int64(k)
			_ = rc.SetWriteDeadline(time.Now().Add(plazoEscritura))
			if _, werr := w.Write(buf[:k]); werr != nil {
				return // el cliente se fue; el contexto cancela la petición al backend
			}
			if stream {
				if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
					return
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				g.met.vonErr(name, "stream")
				abortar(rc) // también cortada: que no parezca completa
			}
			return
		}
	}
}

// abortar cierra la conexión del cliente sin terminar la respuesta: en
// HTTP/1.1 falta el último trozo (o sobran bytes del Content-Length) y el
// cliente lo lee como error. Si el escritor no deja secuestrar la conexión
// (un ResponseRecorder, HTTP/2), la respuesta acaba ahí sin más, como antes.
func abortar(rc *http.ResponseController) {
	conn, _, err := rc.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// ---- catálogo de backends

// catalogo indexa los backends HTTP de un *Config: por nombre, por dorado y en
// orden. Se calcula una vez por *Config (un Reload o una versión nueva de una
// tarea dan otro) y se comparte entre peticiones: es de solo lectura.
type catalogo struct {
	lista  []*Backend
	byName map[string]*Backend
	bySnap map[string]*Backend
}

// catalogo devuelve el catálogo del registro efectivo.
func (g *Gateway) catalogo() *catalogo {
	cfg := g.config()
	p := g.guests
	p.catMu.Lock()
	defer p.catMu.Unlock()
	if p.cat != nil && p.catCfg == cfg {
		return p.cat
	}
	var bs []*Backend
	if p.backendsDe != nil {
		bs = p.backendsDe(cfg)
	} else {
		bs = cfg.Backends()
	}
	c := &catalogo{byName: map[string]*Backend{}, bySnap: map[string]*Backend{}}
	for _, b := range bs {
		c.byName[b.Name] = b
	}
	for _, n := range sortedKeys(c.byName) {
		b := c.byName[n]
		c.lista = append(c.lista, b)
		if b.Snapshot != "" && c.bySnap[b.Snapshot] == nil {
			c.bySnap[b.Snapshot] = b
		}
	}
	p.catCfg, p.cat = cfg, c
	return c
}

// porModelo busca el backend de contrato contract que nombra model (por su
// nombre o por su dorado). Sin model y con un único backend de ese contrato,
// es ese.
func (c *catalogo) porModelo(model, contract string) *Backend {
	if b := c.byName[model]; b != nil && b.Contract == contract {
		return b
	}
	if b := c.bySnap[model]; model != "" && b != nil && b.Contract == contract {
		return b
	}
	if model != "" {
		return nil
	}
	var unico *Backend
	for _, b := range c.lista {
		if b.Contract == contract {
			if unico != nil {
				return nil
			}
			unico = b
		}
	}
	return unico
}

// puertoDe es el puerto de las réplicas del dorado snap.
func (c *catalogo) puertoDe(snap string) int {
	if b := c.bySnap[snap]; b != nil && b.Port > 0 {
		return b.Port
	}
	return DefaultBackendPort
}

// permite dice si b declara method y ruta.
func (b *Backend) permite(method, ruta string) bool {
	for _, r := range b.Routes {
		if r.Method == method && r.Path == ruta {
			return true
		}
	}
	return false
}

// permitido dice si el inquilino puede usar b: el token principal siempre;
// los demás, si b no tiene lista o están en ella.
func permitido(b *Backend, tenant string) bool {
	if tenant == "default" || len(b.Tenants) == 0 {
		return true
	}
	for _, t := range b.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

// ---- petición hacia el backend

// salida es una petición a un backend, que se puede construir más de una vez
// (reintentos). El cuerpo es o bytes (siempre reintentable) o el del cliente
// tal cual (reintentable solo si aún no se leyó nada).
type salida struct {
	method string
	path   string // con la consulta, si la hay
	hdr    http.Header
	bytes  []byte
	cuerpo *cuerpoUnaVez
	largo  int64 // del cuerpo del cliente; -1 = no se sabe
}

// request construye la petición contra base ("http://ip:puerto" o la URL del
// upstream sin barra final).
func (s *salida) request(ctx context.Context, base string) (*http.Request, error) {
	var rd io.Reader = http.NoBody
	n := int64(0)
	switch {
	case s.bytes != nil:
		rd, n = bytes.NewReader(s.bytes), int64(len(s.bytes))
	case s.cuerpo != nil:
		rd, n = s.cuerpo.vista(), s.largo
	}
	req, err := http.NewRequestWithContext(ctx, s.method, base+s.path, rd)
	if err != nil {
		return nil, err
	}
	req.ContentLength = n
	if n == 0 {
		req.Body = http.NoBody
	}
	req.Header = s.hdr.Clone()
	return req, nil
}

// reintentable dice si repetir la petición no duplica nada que el backend
// haya podido ver: con bytes siempre; con el cuerpo del cliente, si aún no se
// leyó ni un byte (y entonces la petición anterior ya no puede leerlo).
func (s *salida) reintentable() bool { return s.cuerpo == nil || s.cuerpo.soltarSiIntacto() }

// excedido dice si el cuerpo del cliente pasó de MaxBody.
func (s *salida) excedido() bool { return s.cuerpo != nil && s.cuerpo.excedido.Load() }

// errCuerpoSoltado lo recibe una petición vieja que intenta leer el cuerpo
// del cliente cuando ya es de su reintento.
var errCuerpoSoltado = errors.New("client body handed to a retry")

// cuerpoUnaVez es el cuerpo del cliente camino del backend sin pasar por
// memoria. Cuenta lo leído (para saber si un reintento es seguro) y recuerda
// si MaxBytesReader lo cortó. Cada petición lo lee por su propia vista: el
// transporte puede seguir leyendo la de una petición fallida desde su
// goroutine de escritura, y al reintentar esa vista deja de leer. Ninguna
// vista cierra el cuerpo: es del servidor.
type cuerpoUnaVez struct {
	r        io.Reader
	leidos   atomic.Int64
	leyendo  atomic.Int32
	excedido atomic.Bool
	actual   atomic.Pointer[vistaCuerpo]
}

type vistaCuerpo struct{ c *cuerpoUnaVez }

// vista da una vista nueva, que pasa a ser la única que lee.
func (c *cuerpoUnaVez) vista() io.ReadCloser {
	v := &vistaCuerpo{c}
	c.actual.Store(v)
	return v
}

// soltarSiIntacto deja sin vista al cuerpo y dice si no se leyó nada (ni se
// está leyendo): si es así, otra petición puede empezar de cero.
func (c *cuerpoUnaVez) soltarSiIntacto() bool {
	c.actual.Store(nil)
	return c.leyendo.Load() == 0 && c.leidos.Load() == 0
}

func (v *vistaCuerpo) Read(p []byte) (int, error) {
	c := v.c
	c.leyendo.Add(1)
	defer c.leyendo.Add(-1)
	if c.actual.Load() != v {
		return 0, errCuerpoSoltado
	}
	n, err := c.r.Read(p)
	c.leidos.Add(int64(n))
	var mbe *http.MaxBytesError
	if err != nil && errors.As(err, &mbe) {
		c.excedido.Store(true)
	}
	return n, err
}

func (v *vistaCuerpo) Close() error { return nil }

// idPeticion devuelve el X-Request-Id del cliente si es razonable (hasta 128
// caracteres de [A-Za-z0-9._:-]) o uno nuevo: va al backend y vuelve.
func idPeticion(r *http.Request) string {
	id := r.Header.Get("X-Request-Id")
	if len(id) > 0 && len(id) <= 128 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:-") == "" {
		return id
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// cabecerasSalida son las únicas que el backend recibe del cliente: el tipo
// del cuerpo, lo que acepta y el id de la petición. Ni Authorization ni
// cookies: la credencial del backend la pone el gateway (ponerAuth).
func cabecerasSalida(r *http.Request, id string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if ct := r.Header.Get("Content-Type"); ct != "" {
		h.Set("Content-Type", ct)
	}
	h.Set("Accept", "application/json, text/event-stream")
	if a := r.Header.Get("Accept"); a != "" && len(a) <= 256 {
		h.Set("Accept", a)
	}
	h.Set("X-Request-Id", id)
	return h
}

// maxAuthBytes acota el fichero de una credencial.
const maxAuthBytes = 16 << 10

// ponerAuth añade a h la credencial de b, leída del fichero en cada petición
// (rotarla no pide recargar). Sin Auth no toca nada.
func ponerAuth(b *Backend, h http.Header) error {
	if b.Auth == nil {
		return nil
	}
	f, err := os.Open(b.Auth.File)
	if err != nil {
		return fmt.Errorf("model %q: credential: %w", b.Name, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAuthBytes+1))
	if err != nil {
		return fmt.Errorf("model %q: credential: %w", b.Name, err)
	}
	v := strings.TrimRight(string(raw), " \t\r\n")
	if v == "" || len(raw) > maxAuthBytes || strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("model %q: credential file is empty, too large or has several lines", b.Name)
	}
	if b.Auth.Scheme != "" {
		v = b.Auth.Scheme + " " + v
	}
	h.Set(b.Auth.Header, v)
	return nil
}

// reenviar manda s al backend b: a una réplica de su dorado o a su upstream.
// Devuelve la respuesta y con qué soltar lo reservado cuando se termine de
// leer.
func (g *Gateway) reenviar(ctx context.Context, b *Backend, s *salida) (*http.Response, func(), error) {
	if err := ponerAuth(b, s.hdr); err != nil {
		return nil, nil, &wakeError{err}
	}
	if b.Upstream != "" {
		u := g.upstreamDe(b)
		if u == nil {
			return nil, nil, &wakeError{fmt.Errorf("model %q: bad upstream", b.Name)}
		}
		return u.enviar(ctx, s)
	}
	resp, rep, err := g.aReplica(ctx, b.Name, b.Snapshot, b.Port, s)
	if err != nil {
		return nil, nil, err
	}
	return resp, rep.Release, nil
}

// ---- /v1/m/{modelo}/{ruta…}

// handleModelo reenvía una ruta declarada de un backend sin analizar el
// cuerpo: llega al backend byte a byte, con streaming en los dos sentidos, y
// sus errores vuelven tal cual. Los del gateway (403, 404, 503) llevan la
// forma de OpenAI. Los contratos openai* no se publican aquí: por su ruta
// llevan topes, semilla y nombre del modelo, y aquí se los saltarían.
func (g *Gateway) handleModelo(w http.ResponseWriter, r *http.Request) {
	name, ruta := r.PathValue("modelo"), "/"+r.PathValue("ruta")
	b := g.catalogo().byName[name]
	if b == nil || (b.Contract != ContractChispaDecide && b.Contract != ContractRaw) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("model %q has no routes under /v1/m (see GET /v1/models)", name))
		return
	}
	if !permitido(b, scheduler.TenantFrom(r.Context()).Name()) {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("this token cannot use model %q", name))
		return
	}
	if !b.permite(r.Method, ruta) || guest.IsControlPath(path.Clean(ruta)) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("model %q does not serve %s %s", name, r.Method, ruta))
		return
	}
	if r.ContentLength > g.opts.MaxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
		return
	}
	id := idPeticion(r)
	w.Header().Set("X-Request-Id", id)
	s := &salida{method: r.Method, path: ruta, hdr: cabecerasSalida(r, id), largo: r.ContentLength}
	if r.URL.RawQuery != "" {
		s.path += "?" + r.URL.RawQuery
	}
	if r.ContentLength != 0 {
		s.cuerpo = &cuerpoUnaVez{r: r.Body}
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.opts.ProxyTimeout)
	defer cancel()
	resp, soltar, err := g.reenviar(ctx, b, s)
	if err != nil {
		if s.excedido() {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
			return
		}
		g.errorProxy(w, name, err)
		return
	}
	defer soltar()
	defer resp.Body.Close()
	if ra := resp.Header.Get("Retry-After"); ra != "" && len(ra) <= 8 && strings.Trim(ra, "0123456789") == "" {
		w.Header().Set("Retry-After", ra)
	}
	g.devolver(w, resp, name, false)
}
