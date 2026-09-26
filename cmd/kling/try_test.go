package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// tryDaemon es un daemon falso que crea sandboxes, ejecuta con una salida fija
// y apunta qué se borró.
type tryDaemon struct {
	mu      sync.Mutex
	created []api.SandboxRequest
	execs   [][]string
	removed []string
	exit    int
}

func (d *tryDaemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req api.SandboxRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.created = append(d.created, req)
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.Machine{ID: "0123456789abcdef", Name: "sb-1", Image: req.Image, From: req.From})
	})
	m.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req api.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.execs = append(d.execs, req.Cmd)
		exit := d.exit
		d.mu.Unlock()
		enc := json.NewEncoder(w)
		_ = enc.Encode(api.ExecEvent{Stream: "stdout", Data: []byte("hello\n")})
		_ = enc.Encode(api.ExecEvent{Stream: "stderr", Data: []byte("warn\n")})
		_ = enc.Encode(api.ExecEvent{Exit: &exit})
	})
	m.HandleFunc("DELETE /sandboxes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.removed = append(d.removed, r.PathValue("ref"))
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return m
}

func TestTryCreaEjecutaYBorra(t *testing.T) {
	d := &tryDaemon{exit: 3}
	c := fakeDaemon(t, d.mux())
	var out, errOut bytes.Buffer
	o := tryOptions{cmd: []string{"echo", "hi"}, req: api.SandboxRequest{Image: tryDefaultImage, MemMiB: 512}}
	code, err := runTry(context.Background(), c, o, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Fatalf("el código ha de ser el del comando: %d", code)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout solo lleva la salida del comando: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "warn") || !strings.Contains(errOut.String(), "ready in") {
		t.Fatalf("stderr: %q", errOut.String())
	}
	if len(d.created) != 1 || d.created[0].Image != "toolchain" || d.created[0].MemMiB != 512 {
		t.Fatalf("petición: %+v", d.created)
	}
	if len(d.execs) != 1 || strings.Join(d.execs[0], " ") != "echo hi" {
		t.Fatalf("exec: %v", d.execs)
	}
	if len(d.removed) != 1 || d.removed[0] != "0123456789abcdef" {
		t.Fatalf("el sandbox no se borró: %v", d.removed)
	}
}

func TestTryKeepNoBorra(t *testing.T) {
	d := &tryDaemon{}
	c := fakeDaemon(t, d.mux())
	var out, errOut bytes.Buffer
	o := tryOptions{keep: true, cmd: []string{"true"}, req: api.SandboxRequest{From: "tpl"}}
	if code, err := runTry(context.Background(), c, o, &out, &errOut); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if len(d.removed) != 0 {
		t.Fatalf("con -keep no se borra: %v", d.removed)
	}
	if !strings.Contains(errOut.String(), "kept sandbox 0123456789ab") {
		t.Fatalf("ha de decir qué dejó: %q", errOut.String())
	}
}

func TestTryBorraAunqueFalleElExec(t *testing.T) {
	d := &tryDaemon{}
	m := d.mux()
	m2 := http.NewServeMux()
	m2.Handle("/", m)
	m2.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(api.Error{Message: "guest agent not answering"})
	})
	c := fakeDaemon(t, m2)
	var out, errOut bytes.Buffer
	_, err := runTry(context.Background(), c, tryOptions{cmd: []string{"x"}, req: api.SandboxRequest{Image: "i"}}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "guest agent") {
		t.Fatalf("error esperado: %v", err)
	}
	if len(d.removed) != 1 {
		t.Fatalf("un exec fallido no ha de dejar el sandbox vivo: %v", d.removed)
	}
}
