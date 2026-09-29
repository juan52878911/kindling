package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// AISLAMIENTO POR SESIÓN (docs/aislamiento-por-sesion.md).
//
// Con la anotación mcp.isolation=session, cada sesión MCP del servicio tiene su
// propia microVM (ver pkg/scheduler/aislada.go): la sesión nueva nace en una
// máquina restaurada del dorado, las peticiones de esa sesión van siempre a
// ella —congelada o no—, y el DELETE o la caducidad la destruyen. Aquí solo
// vive la parte MCP: saber qué servicios lo piden, abrir la sesión con su
// initialize y traducir los ids como en el camino compartido.

// isolationTTL es cuánto se fía el gateway de lo que leyó de mcp.isolation.
// Corto a propósito: `kling mcp isolation` cambia el modo sin reimportar, y
// quien lo cambia espera que la siguiente sesión ya lo respete.
const isolationTTL = 15 * time.Second

// isolationCache recuerda el modo de aislamiento de cada servicio.
type isolationCache struct {
	mu   sync.Mutex
	at   time.Time
	modo map[string]string
}

// aislado dice si las sesiones del servicio llevan microVM propia. Si no se
// puede leer el catálogo de snapshots, lo último que se supo; sin nada sabido,
// el modo compartido de siempre (el error saldrá igual al despertar).
func (g *Gateway) aislado(ctx context.Context, service string) bool {
	c := &g.aisl
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.modo == nil || time.Since(c.at) > isolationTTL {
		if snaps, err := g.Client().Snapshots(ctx); err == nil {
			modo := map[string]string{}
			for _, s := range snaps {
				m := mcp.Isolation(s)
				modo[s.Name] = m
				if svc := s.Service(); svc != "" {
					modo[svc] = m
				}
			}
			c.modo, c.at = modo, time.Now()
		}
	}
	return c.modo[service] == mcp.IsolationSession
}

// serveIsolatedSession abre una sesión en una microVM propia: la crea, le pasa
// el initialize y, si el invitado abre sesión, la fija a esa máquina con un id
// acuñado aquí. Si no la abre, la máquina no es de nadie y se destruye.
func (g *Gateway) serveIsolatedSession(w http.ResponseWriter, r *http.Request, service string, tnt *scheduler.Tenant) {
	// Solo un POST (el initialize) puede abrir sesión. Un DELETE o un GET sin
	// sesión caían en la primaria compartida, que un servicio aislado no debe
	// tener: se contesta sin despertar nada.
	if r.Method != http.MethodPost || r.GetBody == nil {
		http.Error(w, "this service isolates each session: start one with initialize (POST)",
			http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	key := scheduler.NewSessionKey()
	e, err := g.IsolatedSession(ctx, service, key, tnt, true)
	if err != nil {
		g.newSessionError(w, r, service, err)
		return
	}
	rec := &grabadorAcotado{ResponseRecorder: httptest.NewRecorder(), max: maxProxyBody}
	g.Begin(e)
	e.Proxy().ServeHTTP(rec, r)
	g.End(e)
	if rec.excedido {
		g.ReleaseIsolated(ctx, key)
		http.Error(w, fmt.Sprintf("the %q guest answered initialize with more than %d bytes", service, maxProxyBody),
			http.StatusBadGateway)
		return
	}

	guestSID := rec.Header().Get(SessionHeader)
	ext := ""
	if guestSID != "" && rec.Code < 400 {
		if err := g.BindGuest(key, guestSID, service, e); err != nil {
			g.ReleaseIsolated(ctx, key)
			http.Error(w, fmt.Sprintf("could not register the session for %q: %v", service, err),
				http.StatusInternalServerError)
			return
		}
		ext = key
	} else {
		// Sin sesión abierta nadie volverá a esta máquina: fuera ya, no a los
		// SessionTTL de la caducidad.
		g.ReleaseIsolated(ctx, key)
	}
	if rec.Code < 500 {
		g.anotarExito(service)
	}
	for k, vs := range rec.Header() {
		if k == SessionHeader {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if ext != "" {
		w.Header().Set(SessionHeader, ext)
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// recuperarAislada devuelve despierta la máquina de la sesión aislada ext,
// que es la ÚNICA que puede servirla. Si ya no existe, la sesión se da por
// perdida y el cliente tiene que rehacer el initialize: darle otra máquina
// sería darle un disco vacío como si fuera el suyo.
func (g *Gateway) recuperarAislada(w http.ResponseWriter, r *http.Request, service, ext string) *scheduler.Instance {
	e, err := g.IsolatedSession(r.Context(), service, ext, scheduler.TenantFrom(r.Context()), false)
	if err == nil {
		g.Rebind(ext, e)
		return e
	}
	if errors.Is(err, scheduler.ErrSessionLost) || errors.Is(err, scheduler.ErrNoSuchSession) {
		g.Forget(ext)
		http.Error(w, "the isolated MCP session was lost with its machine; start a new one with initialize",
			http.StatusNotFound)
		return nil
	}
	if errors.Is(err, scheduler.ErrTenantInstances) {
		http.Error(w, fmt.Sprintf("could not wake the session of %q: %v", service, err), http.StatusTooManyRequests)
		return nil
	}
	http.Error(w, fmt.Sprintf("could not wake the session of %q: %v", service, err), http.StatusBadGateway)
	return nil
}
