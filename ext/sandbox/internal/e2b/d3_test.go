package e2b

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
)

// gwBarra es un gateway falso cuyos ids llevan barra (`host/ref`), como el real.
type gwBarra struct {
	mu       sync.Mutex
	id       string
	ficheros map[string][]byte
	borrados []string
}

func (g *gwBarra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	j := func(st int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		json.NewEncoder(w).Encode(v)
	}
	base := "/v1/sandboxes/" + strings.ReplaceAll(g.id, "/", "%2F")
	ep := r.URL.EscapedPath()
	sb := map[string]any{"id": g.id, "template": "base", "state": "running"}
	switch {
	case r.Method == "POST" && ep == "/v1/sandboxes":
		j(201, sb)
	case r.Method == "DELETE" && ep == base:
		g.borrados = append(g.borrados, g.id)
		w.WriteHeader(204)
	case ep == base+"/exec":
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"stream":"stdout","data":"aG9sYQo="}`+"\n"+`{"exit":0,"duration_ms":1}`+"\n")
	case ep == base+"/files" && r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		if g.ficheros == nil {
			g.ficheros = map[string][]byte{}
		}
		g.ficheros[r.URL.Query().Get("path")] = b
		j(200, map[string]any{"path": r.URL.Query().Get("path"), "size": len(b), "mode": "0644"})
	case ep == base+"/files" && r.Method == "GET":
		b, ok := g.ficheros[r.URL.Query().Get("path")]
		if !ok {
			j(404, map[string]any{"message": "no such file"})
			return
		}
		w.Write(b)
	default:
		j(404, map[string]any{"message": "no route " + r.Method + " " + ep})
	}
}

func bancoBarra(t *testing.T, id string, debug bool) (*gwBarra, *httptest.Server, *httptest.Server) {
	t.Helper()
	g := &gwBarra{id: id}
	gs := httptest.NewServer(g)
	t.Cleanup(gs.Close)
	s := &Servidor{API: apiTest{gs.URL}, Clave: clave, Dominio: "e2b.test"}
	if debug {
		s.SandboxDebug = id
	}
	shim := httptest.NewServer(s)
	t.Cleanup(shim.Close)
	dbg := httptest.NewServer(s.HandlerDebug())
	t.Cleanup(dbg.Close)
	return g, shim, dbg
}

func llamar(t *testing.T, metodo, url string, cab map[string]string, cuerpo []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(metodo, url, bytes.NewReader(cuerpo))
	for k, v := range cab {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestIDCodificado(t *testing.T) {
	for _, gw := range []string{"h1/m-1", "sbx1", "host.local:8443/ref_larga-" + strings.Repeat("x", 60)} {
		e := hosts.IDaDNS(gw)
		if !reID.MatchString(e) || strings.ContainsAny(e, "/_-=") || e != strings.ToLower(e) {
			t.Fatalf("%q -> %q no es seguro en ruta y DNS", gw, e)
		}
		if got, ok := DecodificarID(e); !ok || got != gw {
			t.Fatalf("%q -> %q -> %q,%v", gw, e, got, ok)
		}
	}
	if len("49983-"+hosts.IDaDNS("h1/m-1")) > 63 {
		t.Fatal("el id típico no cabe en una etiqueta DNS")
	}
	for _, mal := range []string{"", "h1/m-1", "ONRHQMI", "onrhqm", "onrhqmi=", "a", strings.Repeat("a", 300)} {
		if _, ok := DecodificarID(mal); ok {
			t.Fatalf("%q no debería decodificar", mal)
		}
	}
}

// Flujo del SDK real con E2B_API_URL y E2B_SANDBOX_URL: todo al listener
// principal, sin host <puerto>-<id>, solo con cabeceras.
func TestFlujoSoloCabeceras(t *testing.T) {
	for _, id := range []string{"h1/m-1", "h1/" + strings.Repeat("r", 90)} {
		g, shim, _ := bancoBarra(t, id, false)
		api := map[string]string{"X-API-Key": clave, "Content-Type": "application/json"}
		st, cuerpo := llamar(t, "POST", shim.URL+"/v2/sandboxes", api, []byte(`{"templateID":"base","timeout":300,"secure":true}`))
		if st != 201 {
			t.Fatalf("create: %d %s", st, cuerpo)
		}
		var sb struct{ SandboxID, EnvdAccessToken string }
		json.Unmarshal(cuerpo, &sb)
		if sb.SandboxID != hosts.IDaDNS(id) || strings.Contains(sb.SandboxID, "/") {
			t.Fatalf("sandboxID %q", sb.SandboxID)
		}
		envd := map[string]string{"E2b-Sandbox-Id": sb.SandboxID, "E2b-Sandbox-Port": "49983",
			"X-Access-Token": sb.EnvdAccessToken, "Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"}

		st, cuerpo = llamar(t, "POST", shim.URL+"/process.Process/Start", envd,
			envelope(`{"process":{"cmd":"/bin/bash","args":["-l","-c","echo hola"]},"stdin":false}`))
		if st != 200 || !bytes.Contains(cuerpo, []byte(`"stdout":"aG9sYQo="`)) {
			t.Fatalf("start: %d %q", st, cuerpo)
		}
		f := map[string]string{"E2b-Sandbox-Id": sb.SandboxID, "X-Access-Token": sb.EnvdAccessToken}
		boundary := "XB"
		mp := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"/home/user/a.txt\"\r\n" +
			"Content-Type: application/octet-stream\r\n\r\ncontenido\r\n--" + boundary + "--\r\n"
		f["Content-Type"] = "multipart/form-data; boundary=" + boundary
		st, cuerpo = llamar(t, "POST", shim.URL+"/files?path=%2Fhome%2Fuser%2Fa.txt&username=user", f, []byte(mp))
		if st != 200 {
			t.Fatalf("write: %d %s", st, cuerpo)
		}
		delete(f, "Content-Type")
		st, cuerpo = llamar(t, "GET", shim.URL+"/files?path=%2Fhome%2Fuser%2Fa.txt&username=user", f, nil)
		if st != 200 || string(cuerpo) != "contenido" {
			t.Fatalf("read: %d %q", st, cuerpo)
		}
		// Token de otro sandbox o ausente: 401.
		f["X-Access-Token"] = (&Servidor{Clave: clave}).tokenEnvd("otro/x")
		if st, _ = llamar(t, "GET", shim.URL+"/files?path=%2Fhome%2Fuser%2Fa.txt", f, nil); st != 401 {
			t.Fatalf("token ajeno: %d", st)
		}
		// kill como el SDK: DELETE /sandboxes/<id codificado>.
		st, cuerpo = llamar(t, "DELETE", shim.URL+"/sandboxes/"+sb.SandboxID, api, nil)
		if st != 204 || len(g.borrados) != 1 || g.borrados[0] != id {
			t.Fatalf("kill: %d %s %v", st, cuerpo, g.borrados)
		}
		// El id crudo con barra ya no es un id válido.
		if st, _ = llamar(t, "DELETE", shim.URL+"/sandboxes/"+id, api, nil); st != 404 && st != 501 {
			t.Fatalf("id crudo: %d", st)
		}
	}
}

func TestDebug(t *testing.T) {
	_, shim, dbg := bancoBarra(t, "h1/m-1", true)
	sinToken := map[string]string{"E2b-Sandbox-Id": IDDebug, "Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"}
	cuerpo := envelope(`{"process":{"cmd":"/bin/bash","args":["-c","echo hola"]},"stdin":false}`)
	st, b := llamar(t, "POST", dbg.URL+"/process.Process/Start", sinToken, cuerpo)
	if st != 200 || !bytes.Contains(b, []byte("aG9sYQo=")) {
		t.Fatalf("debug: %d %q", st, b)
	}
	// Con host <puerto>-debug_sandbox_id.
	req, _ := http.NewRequest("POST", dbg.URL+"/process.Process/Start", bytes.NewReader(cuerpo))
	req.Host = "49983-" + IDDebug + ".e2b.test"
	for k, v := range sinToken {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("debug por host: %v %v", err, resp)
	}
	resp.Body.Close()
	// El listener principal sigue exigiendo token; el de debug, con otro id real, también.
	if st, _ = llamar(t, "POST", shim.URL+"/process.Process/Start", sinToken, cuerpo); st == 200 {
		t.Fatal("el listener principal no debe aceptar debug")
	}
	otro := map[string]string{"E2b-Sandbox-Id": hosts.IDaDNS("h2/x-9"), "Content-Type": "application/connect+json"}
	if st, _ = llamar(t, "POST", dbg.URL+"/process.Process/Start", otro, cuerpo); st == 200 {
		t.Fatal("un sandbox real sin token no debe pasar por debug")
	}
}
