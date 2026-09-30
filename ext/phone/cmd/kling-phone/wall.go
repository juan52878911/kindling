package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El muro (`kling phone view`): todas las pantallas a la vez en una web, con
// toques, deslizar, teclas y la salud de cada teléfono. Es el wall.py del
// prototipo en Go, pero sin `kling exec`: habla con kling-phoned por el proxy
// del daemon con el token de cada clon.
//
// Escucha SOLO en loopback por defecto: tocar un teléfono es controlarlo. Y
// como un navegador visita otras webs a la vez, el muro además exige un Host
// de loopback (contra el rebinding de DNS) y una cabecera propia en los POST
// (una web ajena no puede ponerla sin que el navegador pregunte antes, y el
// muro no contesta a esa pregunta): ninguna página de fuera toca un teléfono.

//go:embed wall.html
var wallPage []byte

const wallHeader = "X-Kling-Wall"

type wall struct {
	a   *app
	sem chan struct{} // capturas a la vez

	mu     sync.Mutex
	byName map[string]*api.Machine
	listAt time.Time
	health map[string]wallHealth
	shots  map[string]*shotCache
}

type wallHealth struct {
	OK  bool   `json:"ok"`
	Why string `json:"why,omitempty"`
}

type shotCache struct {
	mu  sync.Mutex
	at  time.Time
	png []byte
}

func newWall(a *app, par int) *wall {
	return &wall{a: a, sem: make(chan struct{}, par), byName: map[string]*api.Machine{},
		health: map[string]wallHealth{}, shots: map[string]*shotCache{}}
}

// running devuelve los teléfonos que corren (lista cacheada 2 s).
func (w *wall) running(ctx context.Context) ([]*api.Machine, error) {
	w.mu.Lock()
	fresh := time.Since(w.listAt) < 2*time.Second
	w.mu.Unlock()
	if !fresh {
		ps, err := w.a.phones(ctx)
		if err != nil {
			return nil, err
		}
		m := map[string]*api.Machine{}
		for _, p := range ps {
			if p.State == api.StateRunning {
				m[p.Name] = p
			}
		}
		w.mu.Lock()
		w.byName, w.listAt = m, time.Now()
		w.mu.Unlock()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*api.Machine, 0, len(w.byName))
	for _, p := range w.byName {
		out = append(out, p)
	}
	sortNatural(out)
	return out, nil
}

func (w *wall) machine(ctx context.Context, name string) (*api.Machine, error) {
	if _, err := w.running(ctx); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.byName[name]
	if m == nil {
		return nil, fmt.Errorf("no running phone %s", name)
	}
	return m, nil
}

// healthLoop mira cada 15 s que Android vive: una pantalla puede verse normal
// con system_server caído (visto en vz tras horas: la captura congelada).
func (w *wall) healthLoop(ctx context.Context) {
	for {
		ps, err := w.running(ctx)
		if err == nil {
			var wg sync.WaitGroup
			res := map[string]wallHealth{}
			var mu sync.Mutex
			for _, m := range ps {
				wg.Add(1)
				go func(m *api.Machine) {
					defer wg.Done()
					c, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					h, err := w.a.health(c, m)
					hv := wallHealth{OK: err == nil && h != nil && h.OK}
					switch {
					case h == nil:
						hv.Why = "phone API does not answer"
					case !h.SystemServer:
						hv.Why = "system_server is not running"
					case !h.BootCompleted:
						hv.Why = "boot not completed"
					case !hv.OK:
						hv.Why = or(h.Detail, "unhealthy")
					}
					mu.Lock()
					res[m.Name] = hv
					mu.Unlock()
				}(m)
			}
			wg.Wait()
			w.mu.Lock()
			w.health = res
			w.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// shot: una captura por teléfono a la vez; si varias pestañas piden la misma,
// esperan a la que está en curso.
func (w *wall) shot(ctx context.Context, name string) ([]byte, error) {
	w.mu.Lock()
	sc := w.shots[name]
	if sc == nil {
		sc = &shotCache{}
		w.shots[name] = sc
	}
	w.mu.Unlock()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.png != nil && time.Since(sc.at) < 250*time.Millisecond {
		return sc.png, nil
	}
	m, err := w.machine(ctx, name)
	if err != nil {
		return nil, err
	}
	w.sem <- struct{}{}
	defer func() { <-w.sem }()
	r, err := w.a.callPhone(ctx, m, "GET", "/v1/screen", nil, false)
	if err != nil {
		return nil, err
	}
	sc.png, sc.at = r.Body, time.Now()
	return r.Body, nil
}

func (w *wall) forget(name string) {
	w.mu.Lock()
	if sc := w.shots[name]; sc != nil {
		sc.at = time.Time{}
	}
	w.mu.Unlock()
}

var wallKeys = map[string]string{"HOME": "HOME", "BACK": "BACK", "RECENTS": "APP_SWITCH", "POWER": "WAKEUP"}

var reWallPath = regexp.MustCompile(`^/(shot|tap|swipe|key)/([A-Za-z0-9][A-Za-z0-9._-]{0,63})$`)

// loopbackHost: el Host de la petición es de loopback (127.0.0.1, ::1 o
// localhost), con cualquier puerto.
func loopbackHost(h string) bool {
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (w *wall) handler(checkHost bool) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if checkHost && !loopbackHost(r.Host) {
			http.Error(rw, "the wall only answers on a loopback host name", http.StatusForbidden)
			return
		}
		rw.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			rw.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
			_, _ = rw.Write(wallPage)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/phones" {
			ps, err := w.running(ctx)
			if err != nil {
				writeJSONErr(rw, http.StatusBadGateway, err)
				return
			}
			type row struct {
				Name   string      `json:"name"`
				Health *wallHealth `json:"health,omitempty"`
			}
			out := []row{}
			w.mu.Lock()
			for _, m := range ps {
				e := row{Name: m.Name}
				if h, ok := w.health[m.Name]; ok {
					e.Health = &h
				}
				out = append(out, e)
			}
			w.mu.Unlock()
			writeJSON(rw, http.StatusOK, out)
			return
		}
		mm := reWallPath.FindStringSubmatch(r.URL.Path)
		if mm == nil {
			writeJSONErr(rw, http.StatusNotFound, errors.New("not found"))
			return
		}
		act, name := mm[1], mm[2]
		if act == "shot" {
			if r.Method != http.MethodGet {
				writeJSONErr(rw, http.StatusMethodNotAllowed, errors.New("GET"))
				return
			}
			t0 := time.Now()
			png, err := w.shot(ctx, name)
			if err != nil {
				writeJSONErr(rw, http.StatusBadGateway, err)
				return
			}
			rw.Header().Set("Content-Type", "image/png")
			rw.Header().Set("X-Capture-Ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
			_, _ = rw.Write(png)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get(wallHeader) != "1" {
			writeJSONErr(rw, http.StatusForbidden, errors.New("POST with the "+wallHeader+" header only"))
			return
		}
		q := r.URL.Query()
		num := func(k string, def int) (int, error) {
			v := q.Get(k)
			if v == "" && def >= 0 {
				return def, nil
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || f > 16384 {
				return 0, fmt.Errorf("bad %s", k)
			}
			return int(f), nil
		}
		var path string
		var body any
		switch act {
		case "tap":
			x, e1 := num("x", -1)
			y, e2 := num("y", -1)
			if e1 != nil || e2 != nil {
				writeJSONErr(rw, http.StatusBadRequest, errors.New("bad parameters"))
				return
			}
			path, body = "/v1/tap", map[string]int{"x": x, "y": y}
		case "swipe":
			var v [5]int
			for i, k := range []string{"x1", "y1", "x2", "y2", "ms"} {
				def := -1
				if k == "ms" {
					def = 250
				}
				n, err := num(k, def)
				if err != nil {
					writeJSONErr(rw, http.StatusBadRequest, errors.New("bad parameters"))
					return
				}
				v[i] = n
			}
			path, body = "/v1/swipe", map[string]int{"x1": v[0], "y1": v[1], "x2": v[2], "y2": v[3], "ms": max(1, min(v[4], 10000))}
		case "key":
			k := wallKeys[q.Get("k")]
			if k == "" {
				writeJSONErr(rw, http.StatusBadRequest, errors.New("unknown key"))
				return
			}
			path, body = "/v1/key", map[string]string{"key": k}
		}
		m, err := w.machine(ctx, name)
		if err != nil {
			writeJSONErr(rw, http.StatusNotFound, err)
			return
		}
		b, _ := json.Marshal(body)
		_, err = w.a.callPhone(ctx, m, "POST", path, b, false)
		w.forget(name)
		if err != nil {
			writeJSONErr(rw, http.StatusBadGateway, err)
			return
		}
		writeJSON(rw, http.StatusOK, map[string]bool{"ok": true})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func cmdView(args []string) error {
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	host := hostFlag(fs)
	listen := fs.String("listen", "127.0.0.1:8765", "where the wall listens (keep it on loopback: touching a phone controls it)")
	open := fs.Bool("open", false, "open it in the browser")
	par := fs.Int("par", 6, "screenshots at a time")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		fmt.Fprintf(a.errw, "warning: the wall listens on %s, not loopback: anyone who reaches it controls the phones\n", ln.Addr())
	}
	w := newWall(a, max(1, *par))
	go w.healthLoop(ctx)
	srv := &http.Server{Handler: w.handler(true), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	u := "http://" + ln.Addr().String() + "/"
	fmt.Fprintf(a.out, "wall: %s (Ctrl-C to stop)\n", u)
	if *open {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		_ = exec.Command(opener, u).Start()
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
