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
	for ref, want := range map[string]string{"old": "no /service", "none": "declares no service"} {
		if _, err := (serviceLogSource{c: c, ref: ref}).fetch(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
