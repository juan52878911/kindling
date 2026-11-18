package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestNewLines(t *testing.T) {
	l := func(s string) []string { return splitLines(s) }
	casos := []struct {
		printed, cur, want string
	}{
		{"", "a\nb", "a\nb"},
		{"a\nb", "a\nb", ""},
		{"a\nb", "a\nb\nc\nd", "c\nd"},
		// La ventana se desliza: lo viejo desaparece por arriba.
		{"a\nb\nc", "b\nc\nd", "d"},
		// La última línea estaba a medias: se reescribe entera.
		{"a\nb\npar", "a\nb\npartial\nnext", "partial\nnext"},
		// Más de una ventana entera de golpe: se escribe toda.
		{"a\nb", "x\ny", "x\ny"},
	}
	for _, c := range casos {
		got := strings.Join(newLines(l(c.printed), l(c.cur)), "\n")
		if got != c.want {
			t.Errorf("newLines(%q, %q) = %q, want %q", c.printed, c.cur, got, c.want)
		}
	}
}

// fakeLogs es una consola que crece en cada consulta y se para al final.
type fakeLogs struct {
	mu     sync.Mutex
	pages  []string
	i      int
	states []bool
}

func (f *fakeLogs) fetch(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pages[min(f.i, len(f.pages)-1)]
	f.i++
	return p, nil
}

func (f *fakeLogs) running(context.Context) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	alive := f.states[min(f.i, len(f.states)-1)]
	if alive {
		return true, "running", nil
	}
	return false, "stopped", nil
}

func TestFollowLogsHastaQueSePara(t *testing.T) {
	src := &fakeLogs{
		pages:  []string{"boot\nstep 1", "boot\nstep 1\nstep 2", "boot\nstep 1\nstep 2\nbye"},
		states: []bool{true, true, false},
	}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := followLogs(ctx, &out, src, []string{"boot", "step 1"}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if out.String() != "step 2\nbye\n" {
		t.Fatalf("solo lo nuevo, y lo último de una máquina que se para: %q", out.String())
	}
}

func TestFollowLogsSeCancela(t *testing.T) {
	src := &fakeLogs{pages: []string{"a"}, states: []bool{true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := followLogs(ctx, &bytes.Buffer{}, src, nil, time.Millisecond); err != context.Canceled {
		t.Fatalf("%v", err)
	}
}

func TestServiceLogSource(t *testing.T) {
	body := `{"declared":true,"argv":["postgres"],"running":true,"starts":1,"log":"cortada\nuno\ndos\n"}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
		var req api.GuestRequest
		json.NewDecoder(r.Body).Decode(&req)
		switch {
		case r.PathValue("ref") == "old":
			json.NewEncoder(w).Encode(api.GuestResponse{Status: 404, Body: "404 page not found"})
		case r.PathValue("ref") == "none":
			json.NewEncoder(w).Encode(api.GuestResponse{Status: 200, Body: `{"declared":false}`})
		case req.Path == fmt.Sprintf("/service?tail=%d", serviceLogWindow) && req.Method == "GET":
			json.NewEncoder(w).Encode(api.GuestResponse{Status: 200, Body: body})
		case req.Path == fmt.Sprintf("/service?tail=%d&timestamps=1", serviceLogWindow):
			json.NewEncoder(w).Encode(api.GuestResponse{Status: 200,
				Body: `{"declared":true,"timestamps":true,"log":"2026-10-28T11:00:00.000001Z uno\n"}`})
		default:
			http.Error(w, "unexpected "+req.Path, 400)
		}
	})
	c := fakeDaemon(t, mux)
	out, err := serviceLogSource{c: c, ref: "pg"}.fetch(context.Background())
	if err != nil || out != "cortada\nuno\ndos\n" {
		t.Fatalf("%q %v", out, err)
	}
	if got := lastLines(out, 2); got != "uno\ndos\n" {
		t.Fatalf("lastLines %q", got)
	}
	si := true
	out, err = serviceLogSource{c: c, ref: "pg", tiempos: &si, avisado: new(bool)}.fetch(context.Background())
	if err != nil || out != "2026-10-28T11:00:00.000001Z uno\n" {
		t.Fatalf("-t: %q %v", out, err)
	}
	for ref, want := range map[string]string{"old": "no /service", "none": "declares no service"} {
		if _, err := (serviceLogSource{c: c, ref: ref}).fetch(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

// consolaConCursor es un daemon que sirve la consola con cursor: cada consulta
// del estado añade la página siguiente y, al acabarse, la máquina se para.
func consolaConCursor(t *testing.T, pages []string) *api.Client {
	t.Helper()
	var mu sync.Mutex
	var buf string
	i := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		state := api.StateRunning
		if i < len(pages) {
			buf += pages[i]
			i++
		}
		if i == len(pages) {
			state = api.StateStopped
		}
		json.NewEncoder(w).Encode(api.Machine{Name: "m", State: state})
	})
	mux.HandleFunc("GET /machines/{ref}/logs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := buf
		if v := r.URL.Query().Get("offset"); v != "" {
			var n int
			fmt.Sscan(v, &n)
			out = buf[n:]
		}
		w.Header().Set(api.LogOffsetHeader, fmt.Sprint(len(buf)))
		w.Header().Set(api.LogRotationHeader, "0")
		fmt.Fprint(w, out)
	})
	return fakeDaemon(t, mux)
}

// F10: con salida periódica (líneas idénticas) el ancla de newLines casaba al
// final de la ventana y lo nuevo se descartaba. Con cursor sale todo, y una
// línea a medias sale una vez y entera.
func TestFollowLogsConCursorNoPierdeRepetidas(t *testing.T) {
	c := consolaConCursor(t, []string{"tick\ntick\n", "tick\n", "tick\npar", "cial\n", "fin"})
	src := &logSource{c: c, ref: "m"}
	_, cur, err := c.LogsTail(context.Background(), "m", 0)
	if err != nil || cur == nil {
		t.Fatalf("cursor inicial: %v %v", cur, err)
	}
	src.cur = cur
	var w bytes.Buffer
	if err := followLogs(context.Background(), &w, src, nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if want := "tick\ntick\ntick\ntick\nparcial\nfin\n"; w.String() != want {
		t.Fatalf("salida %q, quería %q", w.String(), want)
	}
}
