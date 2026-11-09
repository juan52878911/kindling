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

// imagesMux es un daemon falso con estas imágenes en GET /images.
func imagesMux(nombres ...string) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		imgs := []api.Image{}
		for _, n := range nombres {
			imgs = append(imgs, api.Image{Name: n})
		}
		_ = json.NewEncoder(w).Encode(imgs)
	})
	return m
}

func TestTryImagenPorDefectoAusente(t *testing.T) {
	err := imagenTryDisponible(context.Background(), fakeDaemon(t, imagesMux("min", "otra")))
	if err == nil || !strings.Contains(err.Error(), `"toolchain"`) {
		t.Fatalf("sin toolchain ha de fallar nombrándola: %v", err)
	}
	h := hintFor(err)
	if !strings.Contains(h, "kling image toolchain") || !strings.Contains(h, "-image min") {
		t.Fatalf("la pista ha de dar el comando que la construye y -image min: %q", h)
	}

	err = imagenTryDisponible(context.Background(), fakeDaemon(t, imagesMux("otra")))
	if err == nil || strings.Contains(hintFor(err), "-image min") {
		t.Fatalf("sin min no se sugiere -image min: %v / %q", err, hintFor(err))
	}
}

func TestTryImagenPorDefectoPresente(t *testing.T) {
	if err := imagenTryDisponible(context.Background(), fakeDaemon(t, imagesMux("min", "toolchain"))); err != nil {
		t.Fatal(err)
	}
	// Un daemon que no sabe listar no bloquea: el daemon dirá lo suyo.
	if err := imagenTryDisponible(context.Background(), fakeDaemon(t, http.NewServeMux())); err != nil {
		t.Fatalf("sin GET /images se sigue: %v", err)
	}
}

// Un comando que falla con conexiones cortadas por el egress lo dice, antes
// de borrar el sandbox (después ya no hay a quién preguntar).
func TestTryAvisaDelEgressAlFallar(t *testing.T) {
	for _, exit := range []int{0, 1} {
		d := &tryDaemon{exit: exit}
		m := d.mux()
		m.HandleFunc("GET /machines/{ref}/egress", func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			borrado := len(d.removed) > 0
			d.mu.Unlock()
			if borrado {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(api.EgressBlocked{Egress: "none", Total: 3,
				Destinations: []api.BlockedDest{{Dest: "registry.npmjs.org", Kind: "dns", Count: 3}}})
		})
		var out, errOut bytes.Buffer
		o := tryOptions{cmd: []string{"npx", "x"}, req: api.SandboxRequest{Image: "i"}}
		if _, err := runTry(context.Background(), fakeDaemon(t, m), o, &out, &errOut); err != nil {
			t.Fatal(err)
		}
		avisa := strings.Contains(errOut.String(), "registry.npmjs.org")
		if avisa != (exit != 0) {
			t.Errorf("exit %d: aviso de egress %v, stderr %q", exit, avisa, errOut.String())
		}
	}
}

// try -json: un solo objeto con la salida del comando separada de lo que
// cuenta kling, el código del comando, y el sandbox borrado igual.
func TestTryJSON(t *testing.T) {
	d := &tryDaemon{exit: 3}
	c := fakeDaemon(t, d.mux())
	var out bytes.Buffer
	o := tryOptions{cmd: []string{"echo", "hi"}, req: api.SandboxRequest{Image: "alpine-3-20"}}
	code, err := runTryJSON(context.Background(), c, o, &out)
	if err != nil || code != 3 {
		t.Fatalf("%d %v", code, err)
	}
	var j tryJSON
	if err := json.Unmarshal(out.Bytes(), &j); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if j.ExitCode != 3 || j.Stdout != "hello\n" || j.Stderr != "warn\n" || j.Sandbox != "0123456789ab" || j.Image != "alpine-3-20" {
		t.Fatalf("json %+v", j)
	}
	if !strings.Contains(j.Messages, "ready in") || strings.Contains(j.Stderr, "ready in") {
		t.Fatalf("kling's lines must not mix with the command's stderr: %+v", j)
	}
	if len(d.removed) != 1 {
		t.Fatalf("the sandbox was not removed: %v", d.removed)
	}
}
