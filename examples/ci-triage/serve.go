package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

//go:embed web
var webFS embed.FS

// Topes de la página: un log pegado no puede pasar de 8 MiB, se analiza uno a
// la vez (cada uno son miles de peticiones al gateway y quizá despertar a
// VON) y se recuerdan los últimos 64 análisis para poder confirmarlos.
const (
	maxLogBody     = 8 << 20
	maxJSONBody    = 16 << 10
	keepAnalyses   = 64
	maxShownLines  = 4000
	contextAround  = 150
	maxLineShownIn = 400
)

// analysis es lo que el servidor recuerda de un análisis: la confirmación se
// arma con esto, no con lo que mande el navegador (que podría ser cualquier
// cosa).
type analysis struct {
	id   string
	res  *triage.Result
	hash string
}

type server struct {
	gw   *triage.Gateway
	opts triage.Options
	fb   *triage.FeedbackLog

	busy chan struct{}

	mu    sync.Mutex
	order *list.List
	byID  map[string]*list.Element
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	gw := gatewayFlags(fs)
	listen := fs.String("listen", "127.0.0.1:8089", "address of the page (loopback only: it has no login)")
	fbPath := fs.String("feedback", "ci-triage-feedback.jsonl", "JSONL file the confirmations are appended to")
	workers := fs.Int("workers", 8, "classify requests in flight")
	_ = fs.Parse(args)
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		// La página no tiene usuarios ni token: en otra dirección cualquiera de
		// la red podría despertar a VON o escribir en el fichero de
		// confirmaciones.
		return fmt.Errorf("-listen %s: the page has no login, use a loopback address", *listen)
	}
	g, err := gw.dial(*workers)
	if err != nil {
		return err
	}
	fl, err := triage.NewFeedbackLog(*fbPath, maxFeedbackBytes)
	if err != nil {
		return err
	}
	o := gw.options()
	o.Workers = *workers
	s := &server{gw: g, opts: o, fb: fl, busy: make(chan struct{}, 1), order: list.New(), byID: map[string]*list.Element{}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	tasks, err := g.Tasks(tctx)
	cancel()
	if err != nil {
		return fmt.Errorf("the gateway at %s does not answer (is `kling ai serve` running with examples/ci-triage/ai.json?): %w", gw.addr, err)
	}
	have := map[string]bool{}
	for _, t := range tasks {
		have[t.Name] = true
	}
	for _, t := range []string{o.LinesTask, o.CategoryTask} {
		if !have[t] {
			return fmt.Errorf("the gateway has no task %q (see examples/ci-triage/ai.json)", t)
		}
	}
	if o.SummaryTask != "" && !have[o.SummaryTask] {
		log.Printf("the gateway has no task %q: no VON layer", o.SummaryTask)
		s.opts.SummaryTask = ""
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	fmt.Printf("CI triage on http://%s/ (confirmations go to %s)\n", ln.Addr(), *fbPath)
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) handler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("POST /api/analyze", s.handleAnalyze)
	mux.HandleFunc("POST /api/feedback", s.handleFeedback)
	mux.HandleFunc("GET /api/categories", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"categories": triage.HumanCategories})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// En loopback, un Host que no es de loopback es DNS rebinding: otra web
		// intentando hablar con esta página desde el navegador.
		h := r.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		if ip := net.ParseIP(h); h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method == http.MethodPost {
			// Solo JSON o texto plano con cabecera propia: un formulario de otra
			// web no puede mandar ninguno sin pasar por un preflight.
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if ct != "application/json" && r.Header.Get("X-CI-Triage") != "1" {
				http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// shownLine es una línea de la vista: su número (desde 1), su texto
// recortado y su puntuación. Gap marca un salto de líneas omitidas antes.
type shownLine struct {
	N     int     `json:"n"`
	Text  string  `json:"t"`
	Score float64 `json:"s,omitempty"`
	Gap   bool    `json:"gap,omitempty"`
}

func (s *server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	select {
	case s.busy <- struct{}{}:
		defer func() { <-s.busy }()
	default:
		writeErr(w, http.StatusTooManyRequests, "another log is being analyzed; try again in a moment")
		return
	}
	lg, err := triage.Read(http.MaxBytesReader(w, r.Body, maxLogBody), triage.DefaultLimits)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("reading the log: %v (at most %d MiB)", err, maxLogBody>>20))
		return
	}
	if len(lg.Lines) == 0 {
		writeErr(w, http.StatusBadRequest, "the log is empty")
		return
	}
	res, err := triage.Analyze(r.Context(), s.gw, lg, s.opts)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	id := newID()
	s.remember(&analysis{id: id, res: res, hash: triage.HashLog(lg)})
	writeJSON(w, map[string]any{"id": id, "result": res, "lines": shownLines(lg, res), "categories": triage.HumanCategories})
}

// shownLines: el log entero si es corto; si no, las líneas alrededor de los
// tramos elegidos y la cola, con saltos marcados.
func shownLines(lg *triage.Log, res *triage.Result) []shownLine {
	n := len(lg.Lines)
	keep := make([]bool, n)
	if n <= maxShownLines {
		for i := range keep {
			keep[i] = true
		}
	} else {
		for _, c := range res.Chunks {
			for j := max(0, c.From-1-contextAround); j < min(n, c.To+contextAround); j++ {
				keep[j] = true
			}
		}
		for j := max(0, n-contextAround); j < n; j++ {
			keep[j] = true
		}
	}
	var out []shownLine
	gap := false
	for i, k := range keep {
		if !k {
			gap = true
			continue
		}
		sl := shownLine{N: i + 1, Text: triage.Clip(lg.Lines[i].Text, maxLineShownIn), Gap: gap}
		if i < len(res.Scores) && res.Scores[i] >= 0.01 {
			sl.Score = float64(int(res.Scores[i]*100)) / 100
		}
		out = append(out, sl)
		gap = false
	}
	return out
}

func (s *server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Category string `json:"category"`
		Note     string `json:"note"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	a := s.lookup(req.ID)
	if a == nil {
		writeErr(w, http.StatusNotFound, "unknown or expired analysis; analyze the log again")
		return
	}
	fb, err := triage.NewFeedback(a.res, a.res.Fields, a.hash, req.Category, req.Note)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.fb.Append(fb); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, triage.ErrFeedbackFull) {
			code = http.StatusInsufficientStorage
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, map[string]any{"recorded": true, "agreed": fb.Agreed, "label": fb.Label})
}

func (s *server) remember(a *analysis) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[a.id] = s.order.PushFront(a)
	for s.order.Len() > keepAnalyses {
		old := s.order.Back()
		s.order.Remove(old)
		delete(s.byID, old.Value.(*analysis).id)
	}
}

func (s *server) lookup(id string) *analysis {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		return e.Value.(*analysis)
	}
	return nil
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": strings.TrimSpace(msg)})
}
