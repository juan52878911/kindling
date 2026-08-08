// Package gateway enruta llamadas MCP a microVMs bajo demanda.
//
// Es un proceso APARTE del daemon, y a propósito: el daemon nunca escucha en la
// red porque controlarlo equivale a root en su host. El gateway sí escucha, pero
// su superficie es mucho más estrecha — solo sabe despertar instancias de
// snapshots ya existentes y hacer de proxy.
//
//	cliente MCP ──HTTP──> gateway ──socket unix──> daemon ──> microVM
//	                          └────────HTTP proxy───────────────┘
package gateway

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// GuestPort es donde se espera que escuche el servidor MCP dentro de la microVM.
const GuestPort = 8080

// readyTimeout acota la espera a que la herramienta abra su puerto. Generoso
// para el arranque en frío, irrelevante tras un thaw.
const readyTimeout = 20 * time.Second

// waitReady sondea el puerto del invitado hasta que acepta conexiones.
func waitReady(ctx context.Context, ip string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		time.Sleep(25 * time.Millisecond)
	}
	return last
}

// Gateway mantiene una instancia caliente por servicio y enruta hacia ella.
type Gateway struct {
	client   *api.Client
	idle     time.Duration
	mu       sync.Mutex
	services map[string]*entry // servicio -> instancia
}

type entry struct {
	machineID string
	ip        string
	lastUse   time.Time
	proxy     *httputil.ReverseProxy
}

func New(client *api.Client, idle time.Duration) *Gateway {
	return &Gateway{client: client, idle: idle, services: map[string]*entry{}}
}

// Handler expone las rutas del gateway.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/services", g.handleServices)
	mux.HandleFunc("/mcp/{service}/", g.handleProxy)
	mux.HandleFunc("/mcp/{service}", g.handleProxy)
	return logging(mux)
}

func logging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// handleServices lista qué snapshots hay disponibles como servicios MCP.
func (g *Gateway) handleServices(w http.ResponseWriter, r *http.Request) {
	snaps, err := g.client.Snapshots(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, s := range snaps {
		name := s.Name
		if svc := s.Service(); svc != "" {
			name = svc
		}
		status := "frío"
		if e, ok := g.services[name]; ok {
			status = fmt.Sprintf("caliente en %s desde hace %s",
				e.ip, time.Since(e.lastUse).Round(time.Second))
		}
		fmt.Fprintf(w, "%-24s snapshot=%-20s %s\n", name, s.Name, status)
	}
}

// handleProxy es el camino caliente: asegura instancia y hace de proxy.
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	if service == "" {
		http.Error(w, "falta el servicio en la ruta", http.StatusBadRequest)
		return
	}

	e, err := g.ensure(r.Context(), service)
	if err != nil {
		http.Error(w, fmt.Sprintf("no pude preparar %q: %v", service, err), http.StatusBadGateway)
		return
	}

	// La ruta que ve la herramienta no incluye el prefijo de enrutado.
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/mcp/"+service)
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	e.proxy.ServeHTTP(w, r)
}

// ensure devuelve una instancia viva del servicio, despertándola si hace falta.
//
// Tres casos, de más barato a más caro:
//  1. ya hay una caliente          -> 0 ms
//  2. hay una congelada            -> ~30 ms de thaw
//  3. no hay ninguna               -> se instancia del snapshot dorado
func (g *Gateway) ensure(ctx context.Context, service string) (*entry, error) {
	g.mu.Lock()
	if e, ok := g.services[service]; ok {
		e.lastUse = time.Now()
		g.mu.Unlock()
		if g.alive(ctx, e.machineID) {
			return e, nil
		}
		// Murió por debajo: se descarta y se reconstruye.
		g.mu.Lock()
		delete(g.services, service)
	}
	g.mu.Unlock()

	mc, err := g.acquire(ctx, service)
	if err != nil {
		return nil, err
	}

	// Tener la microVM en marcha no significa que la herramienta escuche ya. Tras
	// un thaw el proceso está listo casi al instante, pero en frío el invitado aún
	// arranca. Sin esperar aquí, la primera petición se comería un "connection
	// refused" que el cliente MCP interpretaría como que la herramienta no existe.
	if err := waitReady(ctx, mc.IP, GuestPort, readyTimeout); err != nil {
		return nil, fmt.Errorf("la herramienta no empezó a escuchar: %w", err)
	}

	target, _ := url.Parse("http://" + net.JoinHostPort(mc.IP, strconv.Itoa(GuestPort)))
	e := &entry{
		machineID: mc.ID,
		ip:        mc.IP,
		lastUse:   time.Now(),
		proxy:     httputil.NewSingleHostReverseProxy(target),
	}
	// Timeouts generosos: MCP usa streaming HTTP y las respuestas pueden ser largas.
	e.proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	e.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Una herramienta puede reiniciarse entre peticiones. Se le da una
		// oportunidad de volver antes de declarar el fallo.
		if waitReady(r.Context(), e.ip, GuestPort, 3*time.Second) == nil {
			log.Printf("proxy %s: %v (reintentando)", service, err)
			e.proxy.ServeHTTP(w, r)
			return
		}
		log.Printf("proxy %s: %v", service, err)
		http.Error(w, fmt.Sprintf("la herramienta %q no respondió: %v", service, err), http.StatusBadGateway)
	}

	g.mu.Lock()
	g.services[service] = e
	g.mu.Unlock()
	return e, nil
}

// acquire busca una máquina utilizable para el servicio, por orden de coste.
func (g *Gateway) acquire(ctx context.Context, service string) (*api.Machine, error) {
	machines, err := g.client.List(ctx)
	if err != nil {
		return nil, err
	}

	match := func(m *api.Machine) bool {
		return m.Service() == service || m.From == service
	}

	// 1) alguna ya en marcha
	for _, m := range machines {
		if match(m) && m.State == api.StateRunning && m.IP != "" {
			return m, nil
		}
	}
	// 2) alguna congelada: ~30 ms
	for _, m := range machines {
		if match(m) && m.State == api.StateWarm {
			log.Printf("%s: descongelando %s", service, m.Name)
			return g.client.Thaw(ctx, m.ID)
		}
	}
	// 3) instanciar del snapshot dorado
	snap, err := g.snapshotFor(ctx, service)
	if err != nil {
		return nil, err
	}
	log.Printf("%s: instanciando desde el snapshot %s", service, snap.Name)
	return g.client.Run(ctx, api.RunRequest{
		From:       snap.Name,
		Labels:     map[string]string{api.LabelService: service},
		TTLSeconds: int(g.idle.Seconds()) * 2, // red de seguridad si el gateway muere
	})
}

func (g *Gateway) snapshotFor(ctx context.Context, service string) (*api.Snapshot, error) {
	snaps, err := g.client.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if s.Service() == service || s.Name == service {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no hay snapshot para el servicio %q", service)
}

func (g *Gateway) alive(ctx context.Context, id string) bool {
	machines, err := g.client.List(ctx)
	if err != nil {
		return false
	}
	for _, m := range machines {
		if m.ID == id {
			return m.State == api.StateRunning
		}
	}
	return false
}

// Reap congela las instancias que llevan demasiado tiempo sin recibir peticiones.
//
// Congelar y no eliminar es lo que hace que la siguiente petición cueste 30 ms en
// vez de 2,6 segundos: la herramienta sigue ahí, solo que dormida.
func (g *Gateway) Reap(ctx context.Context) {
	t := time.NewTicker(g.idle / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.reapOnce(ctx)
		}
	}
}

func (g *Gateway) reapOnce(ctx context.Context) {
	type victim struct{ service, id string }
	var victims []victim

	g.mu.Lock()
	for svc, e := range g.services {
		if time.Since(e.lastUse) > g.idle {
			victims = append(victims, victim{svc, e.machineID})
			delete(g.services, svc)
		}
	}
	g.mu.Unlock()

	for _, v := range victims {
		if _, err := g.client.Freeze(ctx, v.id); err != nil {
			log.Printf("reap %s: %v", v.service, err)
			continue
		}
		log.Printf("%s: congelada por inactividad", v.service)
	}
}
