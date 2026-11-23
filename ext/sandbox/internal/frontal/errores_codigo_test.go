package frontal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// C3: toda respuesta de error lleva code.
func TestErrorLlevaCode(t *testing.T) {
	srv, _ := montar(t, map[string]*falso{"a": nuevoFalso(t)})
	casos := []struct {
		metodo, ruta, token string
		estado              int
		code                api.ErrorCode
	}{
		{"GET", "/v1/sandboxes/a/inexistente", tokenAlice, 404, api.CodeNotFound},
		{"GET", "/v1/sandboxes", "", 401, api.CodeUnauthorized},
		{"POST", "/v1/sandboxes", tokenAlice, 400, api.CodeBadRequest},
	}
	for _, c := range casos {
		var cuerpo any
		if c.estado == 400 {
			cuerpo = `{"image":"base","on_ttl":"explode"}`
		}
		resp := pide(t, srv, c.token, c.metodo, c.ruta, cuerpo)
		esperaCodigo(t, resp, c.estado)
		e := decodifica[api.Error](t, resp)
		if e.Code != c.code || e.Message == "" {
			t.Errorf("%s %s: %+v, want code %q", c.metodo, c.ruta, e, c.code)
		}
	}
}

func TestFailConservaElCodeDelDaemon(t *testing.T) {
	err := &api.StatusError{Code: 404, Message: "no claimable machine", ErrorCode: api.CodeNoClaimable}
	rec := &respuestaMemoria{h: http.Header{}}
	fail(rec, 502, deHost(&hosts.Host{Nombre: "a"}, err))
	var e api.Error
	if rec.estado != 404 || json.Unmarshal(rec.cuerpo.Bytes(), &e) != nil || e.Code != api.CodeNoClaimable {
		t.Fatalf("%d %s", rec.estado, rec.cuerpo.String())
	}
}

// C17: rutas desconocidas y métodos no admitidos contestan JSON.
func TestRutasDesconocidasContestanJSON(t *testing.T) {
	srv, _ := montar(t, map[string]*falso{"a": nuevoFalso(t)})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	casos := []struct {
		metodo, ruta string
		estado       int
		code         api.ErrorCode
	}{
		{"GET", "/v1/nada", 404, api.CodeNotFound},
		{"DELETE", "/v1/sandboxes", 405, "method_not_allowed"},
		{"GET", "/v1/sandboxes/" + sb.ID + "/bogus", 404, api.CodeNotFound},
		{"GET", "/v1/sandboxes/" + sb.ID + "/code/otra", 404, api.CodeNotFound},
		{"GET", "/v1/sandboxes/" + sb.ID + "/a/b/c/d", 404, api.CodeNotFound},
	}
	for _, c := range casos {
		resp := pide(t, srv, tokenAlice, c.metodo, c.ruta, nil)
		esperaCodigo(t, resp, c.estado)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s: content type %q", c.metodo, c.ruta, ct)
		}
		e := decodifica[api.Error](t, resp)
		if e.Code != c.code || e.Message == "" {
			t.Errorf("%s %s: %+v", c.metodo, c.ruta, e)
		}
	}
}

// C4: lo que sale por un flujo ya empezado no lleva el endpoint del host.
func TestErrorDeFlujoNoEnsenaElEndpoint(t *testing.T) {
	h := &hosts.Host{Nombre: "a"}
	crudo := deHost(h, fmt.Errorf("read unix /var/run/kling/kling.sock->@: connection reset"))
	if msg := errorDeFlujo(crudo); strings.Contains(msg, "kling.sock") || strings.Contains(msg, "/var/run") || msg == "" {
		t.Fatalf("errorDeFlujo leaks: %q", msg)
	}
	// Lo que el daemon dijo con código propio sí pasa.
	se := &api.StatusError{Code: 409, Message: "machine is paused"}
	if msg := errorDeFlujo(deHost(h, se)); !strings.Contains(msg, "paused") {
		t.Fatalf("errorDeFlujo lost the daemon's message: %q", msg)
	}
}

func TestExecCortadoAMitadNoEnsenaElEndpoint(t *testing.T) {
	f := nuevoFalso(t)
	f.execCorta = true
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/exec", api.ExecRequest{Cmd: []string{"x"}})
	esperaCodigo(t, resp, 200)
	texto := leerTexto(t, resp)
	if !strings.Contains(texto, `"error"`) {
		t.Fatalf("no error event in %s", texto)
	}
	if strings.Contains(texto, f.endpoint) || strings.Contains(texto, ".sock") {
		t.Fatalf("the stream leaks the host's endpoint: %s", texto)
	}
}

// C5: /code acaba en done también tras un fallo del gateway a mitad de flujo.
func TestCodigoCortadoAMitadAcabaEnDone(t *testing.T) {
	b := montarCodigo(t)
	b.d.mu.Lock()
	b.d.corta = true
	b.d.mu.Unlock()
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": "print(1)"})
	esperaCodigo(t, resp, 200)
	defer resp.Body.Close()
	var evs []evCodigo
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var e evCodigo
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
		if e.Type == "error" && (strings.Contains(e.Evalue, ".sock") || strings.Contains(e.Evalue, "/tmp/")) {
			t.Errorf("error leaks a path: %q", e.Evalue)
		}
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != "done" || evs[len(evs)-1].OK == nil || *evs[len(evs)-1].OK {
		t.Fatalf("stream does not end in done{ok:false}: %+v", evs)
	}
}

// C18: exec rechaza campos desconocidos.
func TestExecRechazaCamposDesconocidos(t *testing.T) {
	srv, _ := montar(t, map[string]*falso{"a": nuevoFalso(t)})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/exec", `{"cmd":["true"],"timeout_secs":5}`)
	esperaCodigo(t, resp, 400)
	if e := decodifica[api.Error](t, resp); !strings.Contains(e.Message, "timeout_secs") {
		t.Fatalf("%+v", e)
	}
}

type respuestaMemoria struct {
	h      http.Header
	estado int
	cuerpo bytes.Buffer
}

func (r *respuestaMemoria) Header() http.Header { return r.h }
func (r *respuestaMemoria) WriteHeader(c int)   { r.estado = c }
func (r *respuestaMemoria) Write(b []byte) (int, error) {
	return r.cuerpo.Write(b)
}
