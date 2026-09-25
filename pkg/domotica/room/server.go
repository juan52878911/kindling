package room

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/domotica"
)

// La página es un solo documento con su CSS y su JS al lado, sin CDN ni nada
// de fuera: la demo tiene que funcionar sin red y sin permisos raros.
//
//go:embed web
var webFS embed.FS

// LayerInfo describe una capa para la cabecera de la página.
type LayerInfo struct {
	Name   string `json:"name"`   // template | jev | encoder | von
	Status string `json:"status"` // on | unavailable | off | forced
	Detail string `json:"detail,omitempty"`
}

// Preset es una orden de ejemplo de la página.
type Preset struct {
	Lang  string `json:"lang"`
	Text  string `json:"text"`
	Group string `json:"group"` // direct | paraphrase | indirect | oos
}

// Options monta el servidor.
type Options struct {
	// Decide pasa una orden por la cascada.
	Decide func(ctx context.Context, text, lang string) domotica.Trace
	Layers []LayerInfo
	// ExtraStats, si no es nil, añade números de fuera (el gateway de VON):
	// se llama cada pocos segundos y su resultado va tal cual en /api/stats.
	ExtraStats func(ctx context.Context) any
	Presets    []Preset
	// MaxInflight: órdenes a la vez (una escalada a VON tarda segundos). 2.
	MaxInflight int
	// LoopbackOnly rechaza peticiones cuyo Host no sea de loopback: sin eso,
	// una web cualquiera podría usar la demo con DNS rebinding.
	LoopbackOnly bool
}

// Server es la demo.
type Server struct {
	o     Options
	room  *Room
	sem   chan struct{}
	hubMu sync.Mutex
	subs  map[chan []byte]struct{}

	stMu    sync.Mutex
	byLayer map[string]int
	lat     map[string][]float64 // µs de las últimas órdenes por capa que decidió
	all     []float64
	total   int
	extra   any
}

// Topes.
const (
	maxBody     = 4 << 10
	maxText     = 300 // runas
	maxSubs     = 32
	latWindow   = 256
	statsPeriod = 3 * time.Second
)

// NewServer crea la demo con la habitación en su estado inicial.
func NewServer(o Options) *Server {
	if o.MaxInflight <= 0 {
		o.MaxInflight = 2
	}
	return &Server{o: o, room: New(), sem: make(chan struct{}, o.MaxInflight), subs: map[chan []byte]struct{}{},
		byLayer: map[string]int{}, lat: map[string][]float64{}}
}

// Room da acceso a la habitación (tests).
func (s *Server) Room() *Room { return s.room }

// Run mueve el termostato y refresca los números de fuera hasta que ctx acabe.
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if s.room.Tick() {
				s.broadcast("state", s.room.Snapshot())
			}
			if s.o.ExtraStats != nil && now.Sub(last) >= statsPeriod {
				last = now
				ectx, cancel := context.WithTimeout(ctx, 2*time.Second)
				x := s.o.ExtraStats(ectx)
				cancel()
				s.stMu.Lock()
				s.extra = x
				s.stMu.Unlock()
				s.broadcast("stats", s.stats())
			}
		}
	}
}

// Handler devuelve la página y la API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux.Handle("GET /", static)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.stats()) })
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/command", s.handleCommand)
	mux.HandleFunc("POST /api/reset", s.handleReset)
	return s.guard(mux)
}

func init() {
	// Algunos sistemas no traen estos tipos: la página no debe depender de
	// /etc/mime.types.
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
}

// guard: cabeceras de seguridad, Host de loopback y, en los POST, JSON (un
// formulario de otra web no puede mandar application/json sin preflight).
func (s *Server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if s.o.LoopbackOnly && !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if ct != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type stateResp struct {
	State   State       `json:"state"`
	Layers  []LayerInfo `json:"layers"`
	Presets []Preset    `json:"presets"`
	Stats   Stats       `json:"stats"`
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, stateResp{State: s.room.Snapshot(), Layers: s.o.Layers, Presets: s.o.Presets, Stats: s.stats()})
}

type commandReq struct {
	Text string `json:"text"`
	Lang string `json:"lang"`
}

// CommandResp es la respuesta de /api/command (y el evento "decision").
type CommandResp struct {
	Trace   domotica.Trace `json:"trace"`
	Effects []Effect       `json:"effects"`
	State   State          `json:"state"`
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req commandReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		writeErr(w, http.StatusBadRequest, "empty command")
		return
	case !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxText:
		writeErr(w, http.StatusBadRequest, "command too long or not UTF-8")
		return
	}
	lang := req.Lang
	if lang != "es" && lang != "en" {
		lang = "auto"
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		writeErr(w, http.StatusTooManyRequests, "busy: another command is still being decided")
		return
	}
	tr := s.o.Decide(r.Context(), text, lang)
	effects, st := s.room.Apply(tr.Actions)
	if effects == nil {
		effects = []Effect{}
	}
	s.record(tr)
	resp := CommandResp{Trace: tr, Effects: effects, State: st}
	s.broadcast("decision", resp)
	s.broadcast("stats", s.stats())
	writeJSON(w, resp)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	st := s.room.Reset()
	s.broadcast("state", st)
	writeJSON(w, map[string]any{"state": st})
}

// handleEvents es el canal SSE: estado, decisiones y números.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := make(chan []byte, 16)
	s.hubMu.Lock()
	if len(s.subs) >= maxSubs {
		s.hubMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "too many viewers")
		return
	}
	s.subs[ch] = struct{}{}
	s.hubMu.Unlock()
	defer func() {
		s.hubMu.Lock()
		delete(s.subs, ch)
		s.hubMu.Unlock()
	}()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 2000\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// broadcast manda un evento a todos sin esperar a nadie: un visor lento pierde
// eventos (el siguiente "state" lo pone al día), no frena la demo.
func (s *Server) broadcast(event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Stats son los contadores de la página.
type Stats struct {
	Total     int                `json:"total"`
	ByLayer   map[string]int     `json:"by_layer"`
	P50US     float64            `json:"p50_us"`
	P50ByUS   map[string]float64 `json:"p50_by_layer_us"`
	GoHeapMiB float64            `json:"go_heap_mib"`
	GoSysMiB  float64            `json:"go_sys_mib"`
	Extra     any                `json:"extra,omitempty"`
}

func (s *Server) record(tr domotica.Trace) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	s.total++
	s.byLayer[tr.Decided]++
	push := func(xs []float64, v float64) []float64 {
		xs = append(xs, v)
		if len(xs) > latWindow {
			xs = xs[len(xs)-latWindow:]
		}
		return xs
	}
	s.lat[tr.Decided] = push(s.lat[tr.Decided], tr.TotalUS)
	s.all = push(s.all, tr.TotalUS)
}

func p50(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	return c[len(c)/2]
}

func (s *Server) stats() Stats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.stMu.Lock()
	defer s.stMu.Unlock()
	st := Stats{Total: s.total, ByLayer: map[string]int{}, P50US: p50(s.all), P50ByUS: map[string]float64{},
		GoHeapMiB: float64(ms.HeapAlloc) / (1 << 20), GoSysMiB: float64(ms.Sys) / (1 << 20), Extra: s.extra}
	for k, v := range s.byLayer {
		st.ByLayer[k] = v
	}
	for k, v := range s.lat {
		st.P50ByUS[k] = p50(v)
	}
	return st
}
