package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

const clave = "e2b_clave-secreta-0123456789"

// gateway imita lo que el shim usa de /v1 y guarda las peticiones recibidas.
type gateway struct {
	mu       sync.Mutex
	archivos map[string][]byte
	execs    []map[string]any
	renews   []int
	borrados []string
	bloquear chan struct{} // si no es nil, /exec queda abierto hasta cerrarlo
	eco      bool          // los errores repiten el Authorization recibido
	fondo    *fondoFalso   // si no es nil, el gateway tiene procesos de fondo
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok-inquilino" {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if g.eco {
		w.WriteHeader(502)
		fmt.Fprintf(w, `{"message":"upstream failed for %s"}`, clave)
		return
	}
	if g.fondo != nil && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sbx1/processes") {
		g.fondo.servir(w, r)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	j := func(st int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		json.NewEncoder(w).Encode(v)
	}
	sb := map[string]any{"id": "sbx1", "template": "base", "state": "running", "vcpus": 2, "mem_mib": 512,
		"created_at": "2026-01-02T03:04:05Z", "expires_at": "2026-01-02T03:09:05Z", "ttl_seconds": 300,
		"labels": map[string]string{"app": "t"}}
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/v1/sandboxes":
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		g.execs = append(g.execs, map[string]any{"_crear": req})
		j(201, sb)
	case r.Method == "GET" && p == "/v1/sandboxes":
		j(200, []any{sb})
	case p == "/v1/sandboxes/sbx1" && r.Method == "GET":
		j(200, sb)
	case p == "/v1/sandboxes/sbx1" && r.Method == "DELETE":
		g.borrados = append(g.borrados, "sbx1")
		w.WriteHeader(204)
	case p == "/v1/sandboxes/sbx1/renew":
		var req struct {
			TTL int `json:"ttl_seconds"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		g.renews = append(g.renews, req.TTL)
		j(200, sb)
	case p == "/v1/sandboxes/sbx1/exec":
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		g.execs = append(g.execs, req)
		cmd := req["cmd"].([]any)
		if r.URL.Query().Get("wait") == "1" {
			g.execWait(w, cmd)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		if g.bloquear != nil {
			fl.Flush()
			g.mu.Unlock()
			select {
			case <-g.bloquear:
			case <-r.Context().Done():
			}
			g.mu.Lock()
			return
		}
		b64 := base64.StdEncoding.EncodeToString
		fmt.Fprintf(w, `{"stream":"stdout","data":%q}`+"\n", b64([]byte("hola\n")))
		fl.Flush()
		fmt.Fprintf(w, `{"stream":"stderr","data":%q}`+"\n", b64([]byte("aviso\n")))
		fmt.Fprint(w, `{"exit":3,"duration_ms":12}`+"\n")
	case p == "/v1/sandboxes/sbx1/files":
		ruta := r.URL.Query().Get("path")
		switch r.Method {
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			if g.archivos == nil {
				g.archivos = map[string][]byte{}
			}
			g.archivos[ruta] = b
			j(200, map[string]any{"path": ruta, "size": len(b), "mode": "0644"})
		case "GET":
			b, ok := g.archivos[ruta]
			if ruta == "/home/user/dir" {
				j(200, map[string]any{"path": ruta, "is_dir": true, "mode": "0755", "mod_time": "2026-01-02T03:04:05Z"})
				return
			}
			if !ok {
				j(404, map[string]any{"message": "no such file"})
				return
			}
			if r.URL.Query().Get("stat") == "1" {
				j(200, map[string]any{"path": ruta, "size": len(b), "mode": "0644", "mod_time": "2026-01-02T03:04:05Z"})
				return
			}
			w.Write(b)
		}
	default:
		j(404, map[string]any{"message": "no route " + r.Method + " " + p})
	}
}

func (g *gateway) execWait(w http.ResponseWriter, cmd []any) {
	out := ""
	if cmd[0] == "find" {
		out = "f\t5\t644\t1767323045.0000000000\t/home/user/dir/a.txt\nd\t4096\t755\t1767323045.0\t/home/user/dir/sub\n"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": base64.StdEncoding.EncodeToString([]byte(out))})
}

type apiTest struct{ base string }

func (a apiTest) Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, metodo, a.base+ruta, cuerpo)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer tok-inquilino")
	if cuerpo != nil {
		req.Header.Set("Content-Type", tipo)
	}
	return http.DefaultClient.Do(req)
}

type banco struct {
	gw   *gateway
	shim *httptest.Server
	logs *bytes.Buffer
	mu   sync.Mutex
}

func nuevoBanco(t *testing.T) *banco {
	t.Helper()
	b := &banco{gw: &gateway{}, logs: &bytes.Buffer{}}
	gs := httptest.NewServer(b.gw)
	t.Cleanup(gs.Close)
	s := &Servidor{API: apiTest{gs.URL}, Clave: clave, Dominio: "e2b.test",
		Log: func(l string) { b.mu.Lock(); b.logs.WriteString(l + "\n"); b.mu.Unlock() }}
	b.shim = httptest.NewServer(s)
	t.Cleanup(b.shim.Close)
	return b
}

// peticion hace una petición al shim como el SDK: el host de envd es
// <puerto>-<sandbox>.<dominio>.
func (b *banco) peticion(t *testing.T, metodo, ruta, host string, cab map[string]string, cuerpo io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(metodo, b.shim.URL+ruta, cuerpo)
	if host != "" {
		req.Host = host
	}
	for k, v := range cab {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func leer(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func envelope(carga string) []byte {
	b := make([]byte, 5, 5+len(carga))
	binary.BigEndian.PutUint32(b[1:], uint32(len(carga)))
	return append(b, carga...)
}

func TestFlujoSDK(t *testing.T) {
	b := nuevoBanco(t)
	api := map[string]string{"X-API-Key": clave, "Content-Type": "application/json"}

	// 1. create: POST /sandboxes como el SDK.
	resp := b.peticion(t, "POST", "/sandboxes", "", api, strings.NewReader(
		`{"templateID":"base","timeout":300,"metadata":{"app":"t"},"secure":true,"allow_internet_access":false}`))
	cuerpo := leer(t, resp)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, cuerpo)
	}
	var sb struct {
		TemplateID, SandboxID, ClientID, EnvdVersion, EnvdAccessToken, Domain string
	}
	json.Unmarshal(cuerpo, &sb)
	if sb.SandboxID != "onrhqmi" || sb.TemplateID != "base" || sb.EnvdVersion == "" || sb.EnvdAccessToken == "" || sb.Domain != "e2b.test" || sb.ClientID == "" {
		t.Fatalf("respuesta de create: %s", cuerpo)
	}
	crear := b.gw.execs[0]["_crear"].(map[string]any)
	if crear["template"] != "base" || crear["ttl_seconds"] != float64(300) || crear["egress"] != "none" ||
		crear["labels"].(map[string]any)["app"] != "t" {
		t.Fatalf("petición al gateway: %v", crear)
	}

	envd := map[string]string{"X-Access-Token": sb.EnvdAccessToken, "E2b-Sandbox-Id": "onrhqmi", "E2b-Sandbox-Port": "49983"}
	host := "49983-onrhqmi.e2b.test"

	// 2. Process/Start("echo hola") en streaming: bytes exactos.
	peticionStart := `{"process":{"cmd":"/bin/bash","args":["-l","-c","echo hola"],"envs":{"A":"1"},"cwd":"/work"},"stdin":false}`
	cab := map[string]string{"X-Access-Token": sb.EnvdAccessToken, "Content-Type": "application/connect+json",
		"Connect-Protocol-Version": "1", "Connect-Timeout-Ms": "60000", "Authorization": "Basic dXNlcjo=",
		"Keepalive-Ping-Interval": "50"}
	resp = b.peticion(t, "POST", "/process.Process/Start", host, cab, bytes.NewReader(envelope(peticionStart)))
	got := leer(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/connect+json" {
		t.Fatalf("start: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var esperado []byte
	for _, f := range []string{
		`{"event":{"start":{"pid":1}}}`,
		`{"event":{"data":{"stdout":"aG9sYQo="}}}`,
		`{"event":{"data":{"stderr":"YXZpc28K"}}}`,
		`{"event":{"end":{"exitCode":3,"exited":true,"status":"exit status 3"}}}`,
	} {
		esperado = append(esperado, envelope(f)...)
	}
	fin := envelope(`{}`)
	fin[0] = 0x02
	esperado = append(esperado, fin...)
	if !bytes.Equal(got, esperado) {
		t.Fatalf("frames Connect distintos\n got: %q\nwant: %q", got, esperado)
	}
	// Lo que llegó al gateway.
	ex := b.gw.execs[1]
	cmd := fmt.Sprint(ex["cmd"])
	if cmd != "[/bin/bash -l -c echo hola]" || ex["dir"] != "/work" || ex["timeout_seconds"] != float64(60) ||
		fmt.Sprint(ex["env"]) != "[A=1]" {
		t.Fatalf("exec al gateway: %v", ex)
	}

	// 3. files write: multipart como el SDK (campo "file", nombre = ruta).
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="/home/user/a.txt"`)
	h.Set("Content-Type", "application/octet-stream")
	pw, _ := mw.CreatePart(h)
	pw.Write([]byte("contenido\x00bin"))
	mw.Close()
	resp = b.peticion(t, "POST", "/files?path=%2Fhome%2Fuser%2Fa.txt&username=user", host,
		map[string]string{"X-Access-Token": sb.EnvdAccessToken, "Content-Type": mw.FormDataContentType()}, &mp)
	cuerpo = leer(t, resp)
	if resp.StatusCode != 200 || string(cuerpo) != `[{"name":"a.txt","path":"/home/user/a.txt","type":"file"}]`+"\n" {
		t.Fatalf("files write: %d %s", resp.StatusCode, cuerpo)
	}
	if string(b.gw.archivos["/home/user/a.txt"]) != "contenido\x00bin" {
		t.Fatalf("el gateway recibió %q", b.gw.archivos["/home/user/a.txt"])
	}

	// 4. files read (ruta relativa a Home).
	resp = b.peticion(t, "GET", "/files?path=a.txt", host, map[string]string{"X-Access-Token": sb.EnvdAccessToken}, nil)
	if got := leer(t, resp); string(got) != "contenido\x00bin" || resp.StatusCode != 200 {
		t.Fatalf("files read: %d %q", resp.StatusCode, got)
	}
	resp = b.peticion(t, "GET", "/files?path=/nada", host, map[string]string{"X-Access-Token": sb.EnvdAccessToken}, nil)
	if got := leer(t, resp); resp.StatusCode != 404 || !strings.Contains(string(got), `"code":404`) {
		t.Fatalf("files read 404: %d %s", resp.StatusCode, got)
	}

	// 5. kill.
	resp = b.peticion(t, "DELETE", "/sandboxes/onrhqmi", "", api, nil)
	if leer(t, resp); resp.StatusCode != 204 || len(b.gw.borrados) != 1 {
		t.Fatalf("kill: %d", resp.StatusCode)
	}
	_ = envd
}

func TestControl(t *testing.T) {
	b := nuevoBanco(t)
	api := map[string]string{"X-API-Key": clave, "Content-Type": "application/json"}
	resp := b.peticion(t, "GET", "/sandboxes/onrhqmi", "", api, nil)
	var d map[string]any
	json.Unmarshal(leer(t, resp), &d)
	if d["state"] != "running" || d["cpuCount"] != float64(2) || d["memoryMB"] != float64(512) || d["endAt"] != "2026-01-02T03:09:05Z" {
		t.Fatalf("detalle: %v", d)
	}
	resp = b.peticion(t, "GET", "/sandboxes?metadata=app%3Dt", "", api, nil)
	var l []map[string]any
	json.Unmarshal(leer(t, resp), &l)
	if len(l) != 1 {
		t.Fatalf("lista: %v", l)
	}
	resp = b.peticion(t, "GET", "/v2/sandboxes?metadata=app%3Dotro", "", api, nil)
	if got := strings.TrimSpace(string(leer(t, resp))); got != "[]" {
		t.Fatalf("filtro: %s", got)
	}
	resp = b.peticion(t, "POST", "/sandboxes/onrhqmi/timeout", "", api, strings.NewReader(`{"timeout":120}`))
	leer(t, resp)
	if resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	resp = b.peticion(t, "POST", "/sandboxes/onrhqmi/refreshes", "", api, strings.NewReader(`{"duration":30}`))
	leer(t, resp)
	if resp.StatusCode != 204 || fmt.Sprint(b.gw.renews) != "[120 30]" {
		t.Fatalf("renovaciones: %v", b.gw.renews)
	}
	resp = b.peticion(t, "POST", "/sandboxes/onrhqmi/connect", "", api, strings.NewReader(`{"timeout":10}`))
	if leer(t, resp); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestNoImplementado(t *testing.T) {
	b := nuevoBanco(t)
	api := map[string]string{"X-API-Key": clave, "Content-Type": "application/json"}
	for _, c := range []struct{ m, p string }{
		{"POST", "/sandboxes/onrhqmi/pause"}, {"POST", "/sandboxes/onrhqmi/resume"}, {"GET", "/sandboxes/onrhqmi/metrics"},
		{"POST", "/sandboxes/onrhqmi/snapshots"}, {"GET", "/v2/sandboxes/onrhqmi/logs"}, {"POST", "/v3/templates"},
		{"PUT", "/sandboxes/onrhqmi/network"},
	} {
		resp := b.peticion(t, c.m, c.p, "", api, strings.NewReader(`{}`))
		got := leer(t, resp)
		if resp.StatusCode != 501 || !strings.Contains(string(got), `"code":501`) {
			t.Errorf("%s %s: %d %s", c.m, c.p, resp.StatusCode, got)
		}
	}
	// RPC de envd sin soporte: error Connect.
	cab := map[string]string{"X-Access-Token": (&Servidor{Clave: clave}).tokenEnvd("sbx1"), "Content-Type": "application/json"}
	for _, p := range []string{"/process.Process/SendInput", "/filesystem.Filesystem/WatchDir", "/filesystem.Filesystem/CreateWatcher"} {
		resp := b.peticion(t, "POST", p, "49983-onrhqmi.e2b.test", cab, strings.NewReader(`{}`))
		got := leer(t, resp)
		if resp.StatusCode != 501 || !strings.Contains(string(got), `"code":"unimplemented"`) {
			t.Errorf("%s: %d %s", p, resp.StatusCode, got)
		}
	}
}

func TestProtobufRechazado(t *testing.T) {
	b := nuevoBanco(t)
	cab := map[string]string{"X-Access-Token": (&Servidor{Clave: clave}).tokenEnvd("sbx1"), "Content-Type": "application/connect+proto"}
	resp := b.peticion(t, "POST", "/process.Process/Start", "49983-onrhqmi.e2b.test", cab, strings.NewReader("\x00\x00\x00\x00\x00"))
	got := leer(t, resp)
	if resp.StatusCode != 415 || !strings.Contains(string(got), "unimplemented") || !strings.Contains(string(got), "JSON") {
		t.Fatalf("%d %s", resp.StatusCode, got)
	}
	cab["Content-Type"] = "application/proto"
	resp = b.peticion(t, "POST", "/filesystem.Filesystem/Stat", "49983-onrhqmi.e2b.test", cab, strings.NewReader(""))
	if got := leer(t, resp); resp.StatusCode != 415 {
		t.Fatalf("%d %s", resp.StatusCode, got)
	}
}

func TestFilesystemRPC(t *testing.T) {
	b := nuevoBanco(t)
	b.gw.archivos = map[string][]byte{"/home/user/a.txt": []byte("hola")}
	cab := map[string]string{"X-Access-Token": (&Servidor{Clave: clave}).tokenEnvd("sbx1"), "Content-Type": "application/json", "Connect-Protocol-Version": "1"}
	host := "49983-onrhqmi.e2b.test"
	llamar := func(rpc, cuerpo string) (int, map[string]any) {
		resp := b.peticion(t, "POST", "/filesystem.Filesystem/"+rpc, host, cab, strings.NewReader(cuerpo))
		var m map[string]any
		json.Unmarshal(leer(t, resp), &m)
		return resp.StatusCode, m
	}
	st, m := llamar("Stat", `{"path":"a.txt"}`)
	e, _ := m["entry"].(map[string]any)
	if st != 200 || e["name"] != "a.txt" || e["type"] != "FILE_TYPE_FILE" || e["size"] != "4" || e["mode"] != float64(0o644) || e["permissions"] != "-rw-r--r--" {
		t.Fatalf("stat: %d %v", st, m)
	}
	if st, m = llamar("Stat", `{"path":"/nada"}`); st != 404 || m["code"] != "not_found" {
		t.Fatalf("stat 404: %d %v", st, m)
	}
	st, m = llamar("ListDir", `{"path":"/home/user/dir","depth":1}`)
	es, _ := m["entries"].([]any)
	if st != 200 || len(es) != 2 || es[1].(map[string]any)["type"] != "FILE_TYPE_DIRECTORY" || es[0].(map[string]any)["path"] != "/home/user/dir/a.txt" {
		t.Fatalf("listdir: %d %v", st, m)
	}
	if st, m = llamar("MakeDir", `{"path":"/home/user/dir"}`); st != 409 || m["code"] != "already_exists" {
		t.Fatalf("makedir existente: %d %v", st, m)
	}
	if st, m = llamar("Remove", `{"path":"a.txt"}`); st != 200 {
		t.Fatalf("remove: %d %v", st, m)
	}
	if st, m = llamar("Remove", `{"path":"/"}`); st != 400 {
		t.Fatalf("remove /: %d %v", st, m)
	}
	last := b.gw.execs[len(b.gw.execs)-1]["cmd"]
	if fmt.Sprint(last) != "[rm -rf -- /]" && fmt.Sprint(last) != "[rm -rf -- /home/user/a.txt]" {
		t.Fatalf("último exec: %v", last)
	}
}

func TestSendSignalCortaElProceso(t *testing.T) {
	b := nuevoBanco(t)
	b.gw.bloquear = make(chan struct{})
	defer close(b.gw.bloquear)
	tok := (&Servidor{Clave: clave}).tokenEnvd("sbx1")
	host := "49983-onrhqmi.e2b.test"
	cab := map[string]string{"X-Access-Token": tok, "Content-Type": "application/connect+json"}
	resp := b.peticion(t, "POST", "/process.Process/Start", host, cab,
		strings.NewReader(string(envelope(`{"process":{"cmd":"sleep","args":["99"]},"tag":"dormir"}`))))
	defer resp.Body.Close()
	var cab5 [5]byte
	if _, err := io.ReadFull(resp.Body, cab5[:]); err != nil {
		t.Fatal(err)
	}
	carga := make([]byte, binary.BigEndian.Uint32(cab5[1:]))
	io.ReadFull(resp.Body, carga)
	if string(carga) != `{"event":{"start":{"pid":1}}}` {
		t.Fatalf("start: %s", carga)
	}
	cu := map[string]string{"X-Access-Token": tok, "Content-Type": "application/json"}
	r := b.peticion(t, "POST", "/process.Process/List", host, cu, strings.NewReader(`{}`))
	if got := string(leer(t, r)); !strings.Contains(got, `"pid":1`) || !strings.Contains(got, `"tag":"dormir"`) {
		t.Fatalf("list: %s", got)
	}
	r = b.peticion(t, "POST", "/process.Process/SendSignal", host, cu, strings.NewReader(`{"process":{"pid":1},"signal":"SIGNAL_SIGKILL"}`))
	if got := leer(t, r); r.StatusCode != 200 {
		t.Fatalf("signal: %d %s", r.StatusCode, got)
	}
	resto, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(resto), `"exitCode":137`) || resto[len(resto)-len(`{}`)-5] != 0x02 {
		t.Fatalf("fin tras señal: %q", resto)
	}
	r = b.peticion(t, "POST", "/process.Process/SendSignal", host, cu, strings.NewReader(`{"process":{"pid":1},"signal":"SIGNAL_SIGKILL"}`))
	if got := leer(t, r); r.StatusCode != 404 {
		t.Fatalf("señal a un proceso muerto: %d %s", r.StatusCode, got)
	}
}

func TestAutenticacion(t *testing.T) {
	b := nuevoBanco(t)
	for _, k := range []string{"", "otra", clave + "x"} {
		cab := map[string]string{"Content-Type": "application/json"}
		if k != "" {
			cab["X-API-Key"] = k
		}
		resp := b.peticion(t, "GET", "/sandboxes", "", cab, nil)
		if leer(t, resp); resp.StatusCode != 401 {
			t.Errorf("clave %q: %d", k, resp.StatusCode)
		}
	}
	// Token de envd de otro sandbox, o ninguno.
	host := "49983-onrhqmi.e2b.test"
	for _, tok := range []string{"", "x", (&Servidor{Clave: clave}).tokenEnvd("otro")} {
		resp := b.peticion(t, "GET", "/files?path=/a", host, map[string]string{"X-Access-Token": tok}, nil)
		if leer(t, resp); resp.StatusCode != 401 {
			t.Errorf("token %q: %d", tok, resp.StatusCode)
		}
	}
	// Sin sandbox identificable.
	resp := b.peticion(t, "GET", "/files?path=/a", "", map[string]string{"X-API-Key": clave}, nil)
	if leer(t, resp); resp.StatusCode != 404 {
		t.Errorf("sin sandbox: %d", resp.StatusCode)
	}
	// Sin clave configurada no entra nadie.
	vacio := httptest.NewServer(&Servidor{API: apiTest{}})
	defer vacio.Close()
	req, _ := http.NewRequest("GET", vacio.URL+"/sandboxes", nil)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Errorf("sin clave configurada: %d", r.StatusCode)
	}
}

// La clave no sale nunca: ni en los logs ni en errores, aunque el gateway la
// repita o falle el transporte.
func TestClaveNoSeFiltra(t *testing.T) {
	b := nuevoBanco(t)
	b.gw.eco = true
	api := map[string]string{"X-API-Key": clave, "Content-Type": "application/json"}
	var todo bytes.Buffer
	for _, c := range []struct{ m, p, cuerpo string }{
		{"GET", "/sandboxes/onrhqmi", ""}, {"POST", "/sandboxes", `{"templateID":"base"}`},
		{"POST", "/sandboxes/onrhqmi/pause", "{}"}, {"GET", "/sandboxes/onrhqmi/../x?k=" + clave, ""},
	} {
		resp := b.peticion(t, c.m, c.p, "", api, strings.NewReader(c.cuerpo))
		todo.Write(leer(t, resp))
	}
	envd := map[string]string{"X-API-Key": clave, "Content-Type": "application/json", "E2b-Sandbox-Id": "onrhqmi"}
	resp := b.peticion(t, "POST", "/filesystem.Filesystem/Stat", "", envd, strings.NewReader(`{"path":"/a"}`))
	todo.Write(leer(t, resp))
	resp = b.peticion(t, "GET", "/files?path=/a", "", envd, nil)
	todo.Write(leer(t, resp))
	// Transporte roto: el error del cliente lleva la clave.
	s := &Servidor{API: roto{}, Clave: clave, Log: func(l string) { todo.WriteString(l) }}
	srv := httptest.NewServer(s)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/sandboxes/onrhqmi", nil)
	req.Header.Set("X-API-Key", clave)
	r, _ := http.DefaultClient.Do(req)
	todo.Write(leer(t, r))
	b.mu.Lock()
	todo.Write(b.logs.Bytes())
	b.mu.Unlock()
	if strings.Contains(todo.String(), clave) {
		t.Fatalf("la clave aparece en la salida:\n%s", todo.String())
	}
	if !strings.Contains(todo.String(), "***") {
		t.Fatalf("esperaba la clave tapada:\n%s", todo.String())
	}
}

type roto struct{}

func (roto) Do(context.Context, string, string, io.Reader, string) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: bad header X-API-Key: %s", clave)
}

func TestFrames(t *testing.T) {
	if got := sobre(0, []byte("ab")); !bytes.Equal(got, []byte{0, 0, 0, 0, 2, 'a', 'b'}) {
		t.Fatalf("%v", got)
	}
	if _, _, err := leerSobre(bytes.NewReader([]byte{1, 0, 0, 0, 0})); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("compresión: %v", err)
	}
	if _, _, err := leerSobre(bytes.NewReader([]byte{0, 0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("un sobre enorme debe rechazarse")
	}
	if _, _, err := leerSobre(bytes.NewReader([]byte{0, 0, 0, 0, 5, 'a'})); err == nil {
		t.Fatal("un sobre cortado debe rechazarse")
	}
	_ = time.Second
}

func TestStartErrores(t *testing.T) {
	b := nuevoBanco(t)
	tok := (&Servidor{Clave: clave}).tokenEnvd("sbx1")
	cab := map[string]string{"X-Access-Token": tok, "Content-Type": "application/connect+json"}
	for nombre, cuerpo := range map[string]string{
		"sin cmd":   `{"process":{}}`,
		"con pty":   `{"process":{"cmd":"sh"},"pty":{"size":{"cols":80,"rows":24}}}`,
		"json roto": `{`,
	} {
		resp := b.peticion(t, "POST", "/process.Process/Start", "49983-onrhqmi.e2b.test", cab, strings.NewReader(string(envelope(cuerpo))))
		got := leer(t, resp)
		// Un stream que falla antes de empezar termina con un solo sobre de fin con error.
		if resp.StatusCode != 200 || len(got) < 5 || got[0] != 0x02 || !strings.Contains(string(got[5:]), `"error"`) {
			t.Errorf("%s: %d %q", nombre, resp.StatusCode, got)
		}
	}
}
