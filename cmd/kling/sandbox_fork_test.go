package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestParseSandboxFork(t *testing.T) {
	o, err := parseSandboxFork([]string{"caja"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if o.ref != "caja" || !reflect.DeepEqual(o.req, api.ForkRequest{Count: 1}) || o.quiet || o.asJSON {
		t.Fatalf("por defecto: %+v (quería una copia y el ttl del original)", o)
	}

	// Los flags pueden ir detrás del sandbox, como en el resto de kling.
	o, err = parseSandboxFork([]string{"caja", "-n", "4", "-ttl", "30m", "-on-ttl", "freeze", "-q"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	want := api.ForkRequest{Count: 4, TTLSeconds: 1800, OnTTL: api.OnTTLFreeze}
	if o.ref != "caja" || !reflect.DeepEqual(o.req, want) || !o.quiet {
		t.Fatalf("con flags: %+v, quería %+v", o, want)
	}
	if o, err = parseSandboxFork([]string{"-json", "-ttl", "90", "caja"}, flag.ContinueOnError); err != nil ||
		!o.asJSON || o.req.TTLSeconds != 90 {
		t.Fatalf("-ttl en segundos y -json: %+v %v", o, err)
	}

	// -label repetido, validado con las reglas de la API.
	o, err = parseSandboxFork([]string{"caja", "-label", "kling.db.state=preparing", "-label", "a=b"}, flag.ContinueOnError)
	if err != nil || !reflect.DeepEqual(o.req.Labels, map[string]string{"kling.db.state": "preparing", "a": "b"}) {
		t.Fatalf("-label: %+v %v", o.req.Labels, err)
	}

	malos := map[string][]string{
		"label sin =":      {"caja", "-label", "x"},
		"label mayúsculas": {"caja", "-label", "Bad=1"},
		"label reservada":  {"caja", "-label", "kling.fork-of=x"},
		"sin sandbox":      {},
		"dos sandbox":      {"a", "b"},
		"cero copias":      {"caja", "-n", "0"},
		"demasiadas":       {"caja", "-n", "65"},
		"on-ttl":           {"caja", "-on-ttl", "explode"},
		"ttl inválido":     {"caja", "-ttl", "mucho"},
		"flag extraño":     {"caja", "-x"},
	}
	for nombre, args := range malos {
		if _, err := parseSandboxFork(args, flag.ContinueOnError); err == nil {
			t.Errorf("%s (%v): se aceptó", nombre, args)
		}
	}
}

// forkDaemon es un daemon falso con la ruta del fork; apunta lo que le piden.
type forkDaemon struct {
	mu   sync.Mutex
	refs []string
	reqs []api.ForkRequest
}

func (d *forkDaemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /sandboxes/{ref}/fork", func(w http.ResponseWriter, r *http.Request) {
		var req api.ForkRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.refs = append(d.refs, r.PathValue("ref"))
		d.reqs = append(d.reqs, req)
		d.mu.Unlock()
		if r.PathValue("ref") != "caja" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Message: `no sandbox "` + r.PathValue("ref") + `"`})
			return
		}
		res := api.ForkResult{Snapshot: "fork-0123456789ab-cafe0001"}
		for i := 0; i < req.Count; i++ {
			res.Sandboxes = append(res.Sandboxes, &api.Machine{
				ID: strings.Repeat(string(rune('a'+i)), 16), Name: "caja-" + strings.Repeat(string(rune('a'+i)), 6)})
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(res)
	})
	return m
}

func TestRunSandboxFork(t *testing.T) {
	d := &forkDaemon{}
	c := fakeDaemon(t, d.mux())
	var out bytes.Buffer
	o := forkOptions{ref: "caja", req: api.ForkRequest{Count: 2, TTLSeconds: 60}}
	if err := runSandboxFork(context.Background(), c, o, &out); err != nil {
		t.Fatal(err)
	}
	if len(d.reqs) != 1 || d.refs[0] != "caja" || !reflect.DeepEqual(d.reqs[0], o.req) {
		t.Fatalf("petición: %v %+v", d.refs, d.reqs)
	}
	s := out.String()
	for _, trozo := range []string{"caja branched into 2", "fork-0123456789ab-cafe0001", "aaaaaaaaaaaa  caja-aaaaaa", "bbbbbbbbbbbb  caja-bbbbbb"} {
		if !strings.Contains(s, trozo) {
			t.Errorf("salida sin %q:\n%s", trozo, s)
		}
	}

	out.Reset()
	o.quiet = true
	if err := runSandboxFork(context.Background(), c, o, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "aaaaaaaaaaaa\nbbbbbbbbbbbb\n" {
		t.Errorf("-q ha de dar solo los ids: %q", out.String())
	}

	out.Reset()
	o.quiet, o.asJSON = false, true
	if err := runSandboxFork(context.Background(), c, o, &out); err != nil {
		t.Fatal(err)
	}
	var res api.ForkResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res.Sandboxes) != 2 || res.Snapshot == "" {
		t.Errorf("-json: %v %s", err, out.String())
	}
}

// Un 404 del sandbox pasa tal cual; un 404 de la RUTA (daemon anterior al
// fork) se explica.
func TestRunSandboxForkErrores(t *testing.T) {
	c := fakeDaemon(t, (&forkDaemon{}).mux())
	var out bytes.Buffer
	err := runSandboxFork(context.Background(), c, forkOptions{ref: "otra", req: api.ForkRequest{Count: 1}}, &out)
	if err == nil || !strings.Contains(err.Error(), `no sandbox "otra"`) {
		t.Fatalf("sandbox inexistente: %v", err)
	}

	viejo := fakeDaemon(t, http.NewServeMux())
	err = runSandboxFork(context.Background(), viejo, forkOptions{ref: "caja", req: api.ForkRequest{Count: 1}}, &out)
	if err == nil || !strings.Contains(err.Error(), "can't fork") {
		t.Fatalf("daemon sin la ruta: %v", err)
	}
	if eh, ok := err.(*errWithHint); !ok || !strings.Contains(eh.hint, "upgrade") {
		t.Errorf("sin pista de actualizar: %#v", err)
	}
}
