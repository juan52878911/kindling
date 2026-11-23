package osb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	clave   = "osb-clave-secreta-0123456789"
	idGW    = "a/m1"
	tokenGW = "tok-inquilino"
)

var idOSB = IDaOSB(idGW)

// gwFalso imita lo que el shim usa de /v1 y anota lo que recibe.
type gwFalso struct {
	t        *testing.T
	mu       sync.Mutex
	sbs      map[string]map[string]any // por id del gateway
	orden    []string
	ficheros map[string][]byte
	modos    map[string]string
	pets     []string         // "METODO ruta?query"
	cuerpos  map[string][]any // cuerpo JSON por "METODO ruta"
	salida   map[int]string   // salida de los procesos de fondo
	bloquear chan struct{}    // si no es nil, un exec con "bloquea" espera a cerrarlo
	sinMeta  bool             // PATCH metadata responde como un gateway anterior
	n        int
}

func nuevoGW(t *testing.T) *gwFalso {
	g := &gwFalso{t: t, sbs: map[string]map[string]any{}, ficheros: map[string][]byte{}, modos: map[string]string{},
		cuerpos: map[string][]any{}, salida: map[int]string{}}
	g.sembrar(idGW, "running", map[string]string{"app": "t"})
	return g
}

func (g *gwFalso) sembrar(id, estado string, labels map[string]string) map[string]any {
	sb := map[string]any{"id": id, "template": "py", "state": estado, "vcpus": 2, "mem_mib": 512,
		"created_at": "2026-01-02T03:04:05Z", "expires_at": "2026-01-02T03:09:05Z", "ttl_seconds": 300,
		"labels": labels, "egress": "allowlist", "allow_domains": []string{"pypi.org"}}
	g.sbs[id] = sb
	g.orden = append(g.orden, id)
	return sb
}

func (g *gwFalso) anotar(r *http.Request, cuerpo []byte) {
	clave := r.Method + " " + r.URL.Path
	g.pets = append(g.pets, clave+"?"+r.URL.RawQuery)
	var v any
	if json.Unmarshal(cuerpo, &v) == nil {
		g.cuerpos[clave] = append(g.cuerpos[clave], v)
	}
}

func (g *gwFalso) cuerpo(clave string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	cs := g.cuerpos[clave]
	if len(cs) == 0 {
		g.t.Fatalf("el gateway no recibió %s; recibió %v", clave, g.pets)
	}
	m, _ := cs[len(cs)-1].(map[string]any)
	return m
}

func (g *gwFalso) recibio(prefijo string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.pets {
		if strings.HasPrefix(p, prefijo) {
			return true
		}
	}
	return false
}

func j(w http.ResponseWriter, st int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	json.NewEncoder(w).Encode(v)
}

func (g *gwFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+tokenGW {
		j(w, 401, map[string]string{"message": "unauthorized", "code": "unauthorized"})
		return
	}
	var cuerpo []byte
	if r.Method != "PUT" {
		cuerpo, _ = io.ReadAll(r.Body)
	}
	g.mu.Lock()
	g.anotar(r, cuerpo)
	p, q := r.URL.Path, r.URL.Query()
	if p == "/v1/templates" {
		g.mu.Unlock()
		j(w, 200, []map[string]any{{"name": "py", "hosts": []any{}}, {"name": "node", "hosts": []any{}}, {"name": "g", "kind": "graph"}})
		return
	}
	if p == "/v1/sandboxes" && r.Method == "POST" {
		g.n++
		id := fmt.Sprintf("a/n%d", g.n)
		var req map[string]any
		json.Unmarshal(cuerpo, &req)
		sb := g.sembrar(id, "running", nil)
		if l, ok := req["labels"].(map[string]any); ok {
			sb["labels"] = l
		}
		g.mu.Unlock()
		j(w, 201, sb)
		return
	}
	if p == "/v1/sandboxes" && r.Method == "GET" {
		var lista []map[string]any
		limite, _ := strconv.Atoi(q.Get("limit"))
		cursor := q.Get("cursor")
		ids := append([]string(nil), g.orden...)
		sort.Strings(ids)
		sig := ""
		for _, id := range ids {
			if cursor != "" && id <= cursor {
				continue
			}
			if limite > 0 && len(lista) == limite {
				sig = lista[len(lista)-1]["id"].(string)
				break
			}
			lista = append(lista, g.sbs[id])
		}
		g.mu.Unlock()
		if sig != "" {
			w.Header().Set("X-Kindling-Next-Cursor", sig)
		}
		j(w, 200, lista)
		return
	}
	resto, ok := strings.CutPrefix(p, "/v1/sandboxes/")
	if !ok {
		g.mu.Unlock()
		j(w, 404, map[string]string{"message": "no route " + r.Method + " " + p, "code": "not_found"})
		return
	}
	partes := strings.SplitN(resto, "/", 4)
	id := partes[0] + "/" + partes[1]
	sb := g.sbs[id]
	if sb == nil {
		g.mu.Unlock()
		j(w, 404, map[string]string{"message": "sandbox not found", "code": "not_found"})
		return
	}
	accion := ""
	if len(partes) > 2 {
		accion = partes[2]
	}
	sub := ""
	if len(partes) > 3 {
		sub = partes[3]
	}
	defer func() {
		if g.bloquear == nil || accion != "exec" {
			return
		}
	}()
	switch {
	case accion == "" && r.Method == "GET":
		g.mu.Unlock()
		j(w, 200, sb)
	case accion == "" && r.Method == "DELETE":
		delete(g.sbs, id)
		g.mu.Unlock()
		w.WriteHeader(204)
	case accion == "pause":
		sb["state"] = "paused"
		g.mu.Unlock()
		j(w, 200, sb)
	case accion == "resume":
		sb["state"] = "running"
		g.mu.Unlock()
		j(w, 200, sb)
	case accion == "renew":
		g.mu.Unlock()
		j(w, 200, sb)
	case accion == "egress":
		var req map[string]any
		json.Unmarshal(cuerpo, &req)
		sb["egress"] = req["egress"]
		sb["allow_domains"] = req["allow_domains"]
		g.mu.Unlock()
		j(w, 200, map[string]any{"applied": "live"})
	case accion == "metadata" && r.Method == "PATCH":
		if g.sinMeta {
			g.mu.Unlock()
			j(w, 404, map[string]string{"message": "no route PATCH " + p, "code": "not_found"})
			return
		}
		var req map[string]any
		json.Unmarshal(cuerpo, &req)
		l, _ := sb["labels"].(map[string]string)
		nuevo := map[string]string{}
		for k, v := range l {
			nuevo[k] = v
		}
		for k, v := range req {
			if v == nil {
				delete(nuevo, k)
			} else {
				nuevo[k] = v.(string)
			}
		}
		sb["labels"] = nuevo
		g.mu.Unlock()
		j(w, 200, sb)
	case accion == "exec":
		g.mu.Unlock()
		g.exec(w, r, cuerpo)
	case accion == "processes":
		g.procesos(w, r, sub, cuerpo)
	case accion == "sessions":
		g.mu.Unlock()
		if r.Method == "DELETE" && sub == "nada" {
			j(w, 404, map[string]string{"message": "session not found", "code": "not_found"})
			return
		}
		j(w, 201, map[string]any{"id": "x"})
	case accion == "code":
		g.mu.Unlock()
		g.codigo(w, r, sub, cuerpo)
	case accion == "files":
		g.ficherosGW(w, r, sub, cuerpo)
	default:
		g.mu.Unlock()
		j(w, 404, map[string]string{"message": "no route " + r.Method + " " + p, "code": "not_found"})
	}
}

func lineaExec(w io.Writer, stream, s string) {
	fmt.Fprintf(w, `{"stream":%q,"data":%q}`+"\n", stream, base64.StdEncoding.EncodeToString([]byte(s)))
}

func (g *gwFalso) exec(w http.ResponseWriter, r *http.Request, cuerpo []byte) {
	var req struct {
		Cmd []string `json:"cmd"`
	}
	json.Unmarshal(cuerpo, &req)
	if r.URL.Query().Get("wait") == "1" {
		out := ""
		if len(req.Cmd) == 3 && req.Cmd[2] == guionMetricas {
			out = "2\nMemTotal: 1048576 kB\nMemAvailable: 524288 kB\ncpu 100 0 100 700 100 0 0 0\ncpu 150 0 150 750 150 0 0 0\n"
		}
		j(w, 200, map[string]any{"exit_code": 0, "stdout": []byte(out), "duration_ms": 1})
		return
	}
	linea := strings.Join(req.Cmd, " ")
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	switch {
	case strings.Contains(linea, "bloquea"):
		lineaExec(w, "stdout", "primero\n")
		fl.Flush()
		select {
		case <-g.bloquear:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"exit":0,"duration_ms":5}`+"\n")
	case strings.Contains(linea, "falla"):
		lineaExec(w, "stderr", "mal\n")
		fmt.Fprint(w, `{"exit":3,"duration_ms":7}`+"\n")
	default:
		lineaExec(w, "stdout", "hola\n")
		// Una runa partida entre dos trozos no debe salir rota.
		lineaExec(w, "stdout", "ñ"[:1])
		lineaExec(w, "stdout", "ñ"[1:]+"\n")
		fmt.Fprint(w, `{"exit":0,"duration_ms":12}`+"\n")
	}
}

func (g *gwFalso) procesos(w http.ResponseWriter, r *http.Request, sub string, cuerpo []byte) {
	defer g.mu.Unlock()
	switch {
	case sub == "" && r.Method == "POST":
		g.salida[77] = "l1\nl2\nl3\n"
		j(w, 201, map[string]any{"pid": 77, "state": "running"})
	case sub == "77" && r.Method == "GET":
		code := 0
		j(w, 200, map[string]any{"pid": 77, "cmd": []string{"sh", "-c", "sleep 1"}, "state": "exited",
			"started_at": "2026-01-02T03:04:05Z", "ended_at": "2026-01-02T03:04:06Z", "exit_code": code})
	case sub == "77/output":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		lineaExec(w, "stdout", g.salida[77])
	case sub == "77/signal":
		w.WriteHeader(204)
	default:
		j(w, 404, map[string]string{"message": "process not found", "code": "not_found"})
	}
}

func (g *gwFalso) codigo(w http.ResponseWriter, r *http.Request, sub string, cuerpo []byte) {
	switch {
	case sub == "contexts" && r.Method == "POST":
		var req map[string]string
		json.Unmarshal(cuerpo, &req)
		g.mu.Lock()
		g.n++
		n := g.n
		g.mu.Unlock()
		j(w, 201, map[string]any{"id": fmt.Sprintf("ctx%d", n), "language": req["language"], "pid": 9})
	case sub == "contexts" && r.Method == "GET":
		j(w, 200, []map[string]any{{"id": "ctxA", "language": "python"}, {"id": "ctxB", "language": "bash"}})
	case strings.HasPrefix(sub, "contexts/"):
		if strings.Contains(sub, "nada") {
			j(w, 404, map[string]string{"message": "context not found", "code": "not_found"})
			return
		}
		if r.Method == "GET" {
			j(w, 200, map[string]any{"id": strings.TrimPrefix(sub, "contexts/"), "language": "python"})
			return
		}
		w.WriteHeader(204)
	case sub == "" && r.Method == "POST":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"type":"stdout","text":"hola\n"}`+"\n")
		fmt.Fprint(w, `{"type":"result","text":"2","results":{"text/plain":"2","text/html":"<b>2</b>"},"execution_count":1}`+"\n")
		fmt.Fprint(w, `{"type":"error","ename":"ValueError","evalue":"x","traceback":["tb"]}`+"\n")
		fmt.Fprint(w, `{"type":"done","ok":false,"duration_ms":4,"execution_count":1}`+"\n")
	default:
		j(w, 404, map[string]string{"message": "no route", "code": "not_found"})
	}
}

func (g *gwFalso) ficherosGW(w http.ResponseWriter, r *http.Request, sub string, cuerpo []byte) {
	q := r.URL.Query()
	p := q.Get("path")
	switch {
	case sub == "move":
		var req map[string]any
		json.Unmarshal(cuerpo, &req)
		de, a := req["from"].(string), req["to"].(string)
		g.ficheros[a], g.modos[a] = g.ficheros[de], g.modos[de]
		delete(g.ficheros, de)
		g.mu.Unlock()
		j(w, 200, map[string]any{"path": a})
	case sub == "mkdir":
		g.mu.Unlock()
		j(w, 201, map[string]any{"path": "x", "is_dir": true})
	case sub == "search":
		var m []string
		for k := range g.ficheros {
			if strings.HasPrefix(k, p) && strings.HasSuffix(k, strings.TrimPrefix(q.Get("glob"), "*")) {
				m = append(m, k)
			}
		}
		sort.Strings(m)
		g.mu.Unlock()
		j(w, 200, map[string]any{"path": p, "glob": q.Get("glob"), "matches": m})
	case r.Method == "PUT":
		g.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.ficheros[p], g.modos[p] = b, q.Get("mode")
		g.mu.Unlock()
		j(w, 200, map[string]any{"path": p, "size": len(b)})
	case r.Method == "DELETE":
		delete(g.ficheros, p)
		g.mu.Unlock()
		w.WriteHeader(204)
	case q.Get("list") == "1":
		g.mu.Unlock()
		if p == "/d" {
			j(w, 200, map[string]any{"path": p, "entries": []map[string]any{{"name": "sub", "type": "dir", "mode": "0755"}, {"name": "f.txt", "type": "file", "size": 3, "mode": "0644"}}})
		} else {
			j(w, 200, map[string]any{"path": p, "entries": []map[string]any{{"name": "g.txt", "type": "file", "size": 1, "mode": "0600"}}})
		}
	default:
		b, ok := g.ficheros[p]
		modo := g.modos[p]
		g.mu.Unlock()
		if !ok {
			j(w, 502, map[string]string{"message": "stat " + p + ": no such file or directory", "code": "not_found"})
			return
		}
		if q.Get("stat") == "1" {
			j(w, 200, map[string]any{"path": p, "size": len(b), "mode": modo, "mod_time": "2026-01-02T03:04:05Z"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		w.Write(b)
	}
}

type apiTest struct{ base string }

func (a apiTest) Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, metodo, a.base+ruta, cuerpo)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tokenGW)
	if cuerpo != nil {
		req.Header.Set("Content-Type", tipo)
	}
	return http.DefaultClient.Do(req)
}

type banco struct {
	gw   *gwFalso
	s    *Servidor
	shim *httptest.Server
	mu   sync.Mutex
	logs bytes.Buffer
}

func nuevoBanco(t *testing.T, ajustar ...func(*Servidor)) *banco {
	t.Helper()
	b := &banco{gw: nuevoGW(t)}
	gs := httptest.NewServer(b.gw)
	t.Cleanup(gs.Close)
	b.s = &Servidor{API: apiTest{gs.URL}, Clave: clave, Imagenes: map[string]string{"python:3.12": "py", "node": "node"},
		Log: func(l string) { b.mu.Lock(); b.logs.WriteString(l + "\n"); b.mu.Unlock() }}
	for _, f := range ajustar {
		f(b.s)
	}
	b.shim = httptest.NewServer(b.s)
	t.Cleanup(b.shim.Close)
	return b
}

// pedir hace una petición al plano de control con la clave.
func (b *banco) pedir(t *testing.T, metodo, ruta, cuerpo string) (int, map[string]any, http.Header) {
	t.Helper()
	return b.pedirCon(t, metodo, ruta, map[string]string{"OPEN-SANDBOX-API-KEY": clave}, cuerpo)
}

func (b *banco) pedirCon(t *testing.T, metodo, ruta string, cab map[string]string, cuerpo string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if cuerpo != "" {
		rd = strings.NewReader(cuerpo)
	}
	req, _ := http.NewRequest(metodo, b.shim.URL+ruta, rd)
	for k, v := range cab {
		req.Header.Set(k, v)
	}
	if cuerpo != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil && len(raw) > 0 {
		m = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, m, resp.Header
}

// execd hace una petición al execd del sandbox con su token.
func (b *banco) execd(t *testing.T, metodo, ruta, cuerpo string) (int, []byte, http.Header) {
	t.Helper()
	return b.execdCon(t, metodo, ruta, nil, strings.NewReader(cuerpo), "application/json")
}

func (b *banco) execdCon(t *testing.T, metodo, ruta string, cab map[string]string, cuerpo io.Reader, tipo string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(metodo, b.shim.URL+"/sandboxes/"+idOSB+"/port/44772"+ruta, cuerpo)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", b.s.tokenExecd(idGW))
	req.Header.Set("Content-Type", tipo)
	for k, v := range cab {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

// eventos parte un cuerpo SSE en sus eventos (sin los ping).
func eventos(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, trama := range strings.Split(string(raw), "\n\n") {
		d, ok := strings.CutPrefix(strings.TrimSpace(trama), "data: ")
		if !ok {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(d), &m); err != nil {
			t.Fatalf("evento no JSON: %q", d)
		}
		if m["type"] != "ping" {
			out = append(out, m)
		}
	}
	return out
}

func tipos(evs []map[string]any) string {
	var t []string
	for _, e := range evs {
		t = append(t, e["type"].(string))
	}
	return strings.Join(t, ",")
}

func TestCrearTraduce(t *testing.T) {
	b := nuevoBanco(t)
	st, m, h := b.pedir(t, "POST", "/v1/sandboxes", `{"image":{"uri":"python:3.12"},"timeout":120,
		"resourceLimits":{"cpu":"500m","memory":"1Gi"},"metadata":{"suite":"x"},"env":{"A":"1"},
		"entrypoint":["tail","-f","/dev/null"],
		"networkPolicy":{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org"}]}}`)
	if st != 202 {
		t.Fatalf("crear: %d %v", st, m)
	}
	req := b.gw.cuerpo("POST /v1/sandboxes")
	want := map[string]any{"template": "py", "ttl_seconds": float64(120), "vcpus": float64(1), "mem_mib": float64(1024),
		"egress": "allowlist"}
	for k, v := range want {
		if req[k] != v {
			t.Errorf("%s = %v, quería %v (%v)", k, req[k], v, req)
		}
	}
	if fmt.Sprint(req["labels"], req["env"], req["allow_domains"]) != "map[suite:x] map[A:1] [pypi.org]" {
		t.Errorf("etiquetas/env/dominios: %v", req)
	}
	id, _ := m["id"].(string)
	if id != IDaOSB("a/n1") || strings.Contains(id, "/") {
		t.Fatalf("id: %v", m)
	}
	if st, _ := m["status"].(map[string]any); st["state"] != "Running" || m["createdAt"] == nil || m["expiresAt"] == nil {
		t.Fatalf("respuesta: %v", m)
	}
	if h.Get("Location") != "/v1/sandboxes/"+id {
		t.Fatalf("Location: %q", h.Get("Location"))
	}
	if b.gw.recibio("POST /v1/sandboxes/a/n1/processes") {
		t.Fatal("el entrypoint por defecto no lanza nada")
	}
}

func TestCrearPlantillas(t *testing.T) {
	b := nuevoBanco(t)
	st, m, _ := b.pedir(t, "POST", "/v1/sandboxes", `{"image":{"uri":"ubuntu:24.04"}}`)
	msg, _ := m["message"].(string)
	if st != 422 || m["code"] != "SANDBOX::UNPROCESSABLE" || !strings.Contains(msg, "node, py") || strings.Contains(msg, " g,") ||
		!strings.Contains(msg, "python:3.12=py") {
		t.Fatalf("sin plantilla: %d %v", st, m)
	}
	// Sin la etiqueta, y por templateId.
	if st, m, _ := b.pedir(t, "POST", "/v1/sandboxes", `{"image":{"uri":"node:22"}}`); st != 202 || b.gw.cuerpo("POST /v1/sandboxes")["template"] != "node" {
		t.Fatalf("node:22: %d %v", st, m)
	}
	if st, _, _ := b.pedir(t, "POST", "/v1/sandboxes", `{"templateId":"otra"}`); st != 202 || b.gw.cuerpo("POST /v1/sandboxes")["template"] != "otra" {
		t.Fatalf("templateId: %d", st)
	}
	b2 := nuevoBanco(t, func(s *Servidor) { s.PlantillaDefecto = "base" })
	if st, _, _ := b2.pedir(t, "POST", "/v1/sandboxes", `{"image":{"uri":"ubuntu:24.04"},"timeout":null}`); st != 202 {
		t.Fatalf("plantilla por defecto: %d", st)
	}
	if req := b2.gw.cuerpo("POST /v1/sandboxes"); req["template"] != "base" || req["ttl_seconds"] != nil {
		t.Fatalf("por defecto: %v", req)
	}
	if st, m, _ := b.pedir(t, "POST", "/v1/sandboxes", `{"image":{"uri":"node"},"timeout":30}`); st != 400 || m["code"] != "SANDBOX::INVALID_PARAMETER" {
		t.Fatalf("timeout < 60: %d %v", st, m)
	}
}

func TestCrear501(t *testing.T) {
	b := nuevoBanco(t)
	for _, c := range []struct{ cuerpo, motivo string }{
		{`{"snapshotId":"s1"}`, "snapshotId"},
		{`{"image":{"uri":"node"},"extensions":{"poolRef":"p"}}`, "poolRef"},
		{`{"image":{"uri":"node"},"platform":{"os":"windows","arch":"amd64"}}`, "windows"},
		{`{"image":{"uri":"node"},"resourceLimits":{"nvidia.com/gpu":"1"}}`, "GPU"},
		{`{"image":{"uri":"node"},"volumes":[{"name":"v","host":{"path":"/x"},"mountPath":"/m"}]}`, "volumes"},
		{`{"image":{"uri":"node"},"networkPolicy":{"egress":[{"action":"deny","target":"x.com"}]}}`, "allow rules"},
		{`{"image":{"uri":"node"},"networkPolicy":{"egress":[{"action":"allow","target":"10.0.0.0/8"}]}}`, "domain targets"},
	} {
		st, m, _ := b.pedir(t, "POST", "/v1/sandboxes", c.cuerpo)
		if msg, _ := m["message"].(string); st != 501 || m["code"] != "SANDBOX::NOT_IMPLEMENTED" || !strings.Contains(msg, c.motivo) {
			t.Errorf("%s: %d %v", c.cuerpo, st, m)
		}
	}
	if b.gw.recibio("POST /v1/sandboxes?") {
		t.Fatal("un 501 no debe crear nada")
	}
}

func TestCrearEntrypoint(t *testing.T) {
	b := nuevoBanco(t)
	st, m, _ := b.pedir(t, "POST", "/v1/sandboxes", `{"image":{"uri":"node"},"entrypoint":["python","app.py"],"env":{"K":"v"}}`)
	if st != 202 || fmt.Sprint(m["entrypoint"]) != "[python app.py]" {
		t.Fatalf("crear: %d %v", st, m)
	}
	req := b.gw.cuerpo("POST /v1/sandboxes/a/n1/processes")
	if fmt.Sprint(req["cmd"], req["env"]) != "[python app.py] [K=v]" {
		t.Fatalf("proceso del entrypoint: %v", req)
	}
	if _, m, _ := b.pedir(t, "GET", "/v1/sandboxes/"+IDaOSB("a/n1"), ""); fmt.Sprint(m["entrypoint"]) != "[python app.py]" {
		t.Fatalf("ver: %v", m)
	}
}

func TestAutenticacion(t *testing.T) {
	b := nuevoBanco(t)
	for _, k := range []string{"", "otra"} {
		st, m, _ := b.pedirCon(t, "GET", "/v1/sandboxes/"+idOSB, map[string]string{"OPEN-SANDBOX-API-KEY": k}, "")
		if st != 401 || m["code"] != "SANDBOX::UNAUTHORIZED" || m["message"] == "" {
			t.Fatalf("clave %q: %d %v", k, st, m)
		}
	}
	ruta := "/sandboxes/" + idOSB + "/port/44772/ping"
	for _, tok := range []string{"", "x", b.s.tokenExecd("a/otro")} {
		if st, _, _ := b.pedirCon(t, "GET", ruta, map[string]string{"X-EXECD-ACCESS-TOKEN": tok}, ""); st != 401 {
			t.Fatalf("token %q: %d", tok, st)
		}
	}
	if st, _, _ := b.execd(t, "GET", "/ping", ""); st != 200 {
		t.Fatalf("ping con token: %d", st)
	}
	if st, _, _ := b.pedirCon(t, "GET", ruta, map[string]string{"OPEN-SANDBOX-API-KEY": clave}, ""); st != 200 {
		t.Fatalf("ping con la clave: %d", st)
	}
	// Un id que no es base32 de un id del gateway es 404, no un 500.
	if st, m, _ := b.pedir(t, "GET", "/v1/sandboxes/no-es-valido", ""); st != 404 || m["code"] != "SANDBOX::NOT_FOUND" {
		t.Fatalf("id malo: %d %v", st, m)
	}
	if st, m, _ := b.pedir(t, "GET", "/v1/nada", ""); st != 404 || m["code"] != codigoRuta {
		t.Fatalf("ruta desconocida: %d %v", st, m)
	}
}

func TestListarPaginado(t *testing.T) {
	b := nuevoBanco(t)
	for i := 2; i <= 5; i++ {
		b.gw.sembrar(fmt.Sprintf("a/m%d", i), map[bool]string{true: "paused", false: "running"}[i%2 == 0], map[string]string{"app": "t"})
	}
	b.gw.sembrar("a/m6", "frozen", map[string]string{"app": "otra"})
	st, m, _ := b.pedir(t, "GET", "/v1/sandboxes?page=2&pageSize=2&metadata=app%3Dt", "")
	pg, _ := m["pagination"].(map[string]any)
	items, _ := m["items"].([]any)
	if st != 200 || len(items) != 2 || pg["totalItems"] != float64(5) || pg["totalPages"] != float64(3) || pg["hasNextPage"] != true || pg["page"] != float64(2) {
		t.Fatalf("página 2: %d %v", st, m)
	}
	if id := items[0].(map[string]any)["id"]; id != IDaOSB("a/m3") {
		t.Fatalf("orden: %v", id)
	}
	if !b.gw.recibio("GET /v1/sandboxes?label=app%3Dt&limit=1000") {
		t.Fatalf("el filtro de metadata va al gateway: %v", b.gw.pets)
	}
	_, m, _ = b.pedir(t, "GET", "/v1/sandboxes?state=Paused", "")
	if pg := m["pagination"].(map[string]any); pg["totalItems"] != float64(3) || pg["hasNextPage"] != false {
		t.Fatalf("estado Paused (paused y frozen): %v", m)
	}
	// El listado del gateway por páginas de 1000: el falso corta y el shim sigue.
	for i := 7; i <= 1003; i++ {
		b.gw.sembrar(fmt.Sprintf("b/m%04d", i), "running", nil)
	}
	_, m, _ = b.pedir(t, "GET", "/v1/sandboxes?pageSize=1000&page=2", "")
	if pg := m["pagination"].(map[string]any); pg["totalItems"] != float64(1003) || len(m["items"].([]any)) != 3 {
		t.Fatalf("con cursor: %v", pg)
	}
	if st, _, _ := b.pedir(t, "GET", "/v1/sandboxes?page=0", ""); st != 400 {
		t.Fatalf("page=0: %d", st)
	}
}

func TestEstadoOSB(t *testing.T) {
	for gw, osb := range map[string]string{"running": "Running", "paused": "Paused", "frozen": "Paused",
		"created": "Pending", "stopped": "Terminated", "failed": "Failed"} {
		if got := EstadoOSB(gw); got != osb {
			t.Errorf("%s -> %s, quería %s", gw, got, osb)
		}
	}
}

func TestPausaReanudaRenueva(t *testing.T) {
	b := nuevoBanco(t)
	if st, _, _ := b.pedir(t, "POST", "/v1/sandboxes/"+idOSB+"/pause", ""); st != 202 {
		t.Fatalf("pause: %d", st)
	}
	_, m, _ := b.pedir(t, "GET", "/v1/sandboxes/"+idOSB, "")
	if m["status"].(map[string]any)["state"] != "Paused" {
		t.Fatalf("tras pausar: %v", m)
	}
	if st, _, _ := b.execd(t, "GET", "/ping", ""); st != 503 {
		t.Fatalf("ping de un pausado: %d", st)
	}
	if st, _, _ := b.pedir(t, "POST", "/v1/sandboxes/"+idOSB+"/resume", ""); st != 202 {
		t.Fatalf("resume: %d", st)
	}
	fin := time.Now().Add(300 * time.Second).UTC().Format(time.RFC3339)
	st, m, _ := b.pedir(t, "POST", "/v1/sandboxes/"+idOSB+"/renew-expiration", `{"expiresAt":"`+fin+`"}`)
	ttl := b.gw.cuerpo("POST /v1/sandboxes/a/m1/renew")["ttl_seconds"].(float64)
	if st != 200 || m["expiresAt"] == nil || ttl < 295 || ttl > 301 {
		t.Fatalf("renew: %d %v ttl=%v", st, m, ttl)
	}
	if st, _, _ := b.pedir(t, "POST", "/v1/sandboxes/"+idOSB+"/renew-expiration", `{"expiresAt":"2001-01-01T00:00:00Z"}`); st != 400 {
		t.Fatalf("renew al pasado: %d", st)
	}
	if st, _, _ := b.pedir(t, "DELETE", "/v1/sandboxes/"+idOSB, ""); st != 204 {
		t.Fatalf("delete: %d", st)
	}
	if st, m, _ := b.pedir(t, "GET", "/v1/sandboxes/"+idOSB, ""); st != 404 || m["code"] != "SANDBOX::NOT_FOUND" {
		t.Fatalf("tras borrar: %d %v", st, m)
	}
}

func TestMetadatos(t *testing.T) {
	b := nuevoBanco(t)
	st, m, _ := b.pedir(t, "PATCH", "/v1/sandboxes/"+idOSB+"/metadata", `{"nuevo":"1","app":null}`)
	if st != 200 || fmt.Sprint(m["metadata"]) != "map[nuevo:1]" {
		t.Fatalf("patch: %d %v", st, m)
	}
	b.gw.sinMeta = true
	if st, m, _ := b.pedir(t, "PATCH", "/v1/sandboxes/"+idOSB+"/metadata", `{"x":"1"}`); st != 501 {
		t.Fatalf("gateway sin la ruta: %d %v", st, m)
	}
}

func TestEndpoint(t *testing.T) {
	b := nuevoBanco(t)
	st, m, _ := b.pedir(t, "GET", "/v1/sandboxes/"+idOSB+"/endpoints/44772?use_server_proxy=false", "")
	host := strings.TrimPrefix(b.shim.URL, "http://")
	if st != 200 || m["endpoint"] != host+"/sandboxes/"+idOSB+"/port/44772" {
		t.Fatalf("endpoint: %d %v", st, m)
	}
	if h := m["headers"].(map[string]any); h["X-EXECD-ACCESS-TOKEN"] != b.s.tokenExecd(idGW) {
		t.Fatalf("cabeceras: %v", h)
	}
	b2 := nuevoBanco(t, func(s *Servidor) { s.Publico = "osb.example.com" })
	if _, m, _ := b2.pedir(t, "GET", "/v1/sandboxes/"+idOSB+"/endpoints/8080", ""); m["endpoint"] != "osb.example.com/sandboxes/"+idOSB+"/port/8080" {
		t.Fatalf("público: %v", m)
	}
	if st, _, _ := b.pedir(t, "GET", "/v1/sandboxes/"+idOSB+"/endpoints/0", ""); st != 400 {
		t.Fatalf("puerto 0: %d", st)
	}
	if st, _, _ := b.pedir(t, "GET", "/v1/sandboxes/"+IDaOSB("a/no")+"/endpoints/80", ""); st != 404 {
		t.Fatalf("sandbox ajeno: %d", st)
	}
}

func TestPolitica(t *testing.T) {
	b := nuevoBanco(t)
	ruta := "/v1/sandboxes/" + idOSB + "/networkpolicy"
	_, m, _ := b.pedir(t, "GET", ruta, "")
	if m["mode"] != "allowlist" || fmt.Sprint(m["policy"]) != "map[defaultAction:deny egress:[map[action:allow target:pypi.org]]]" {
		t.Fatalf("get: %v", m)
	}
	st, m, _ := b.pedir(t, "PATCH", ruta, `[{"action":"allow","target":"github.com"}]`)
	if req := b.gw.cuerpo("PATCH /v1/sandboxes/a/m1/egress"); st != 200 || strings.TrimSpace(fmt.Sprintln(req["egress"], req["allow_domains"])) != "allowlist [pypi.org github.com]" {
		t.Fatalf("patch: %d %v %v", st, m, req)
	}
	if st, m, _ := b.pedir(t, "PATCH", ruta, `[{"action":"deny","target":"github.com"}]`); st != 501 || m["code"] != "SANDBOX::NOT_IMPLEMENTED" {
		t.Fatalf("deny: %d %v", st, m)
	}
	if st, _, _ := b.pedir(t, "PATCH", ruta, `[{"action":"allow","target":"1.2.3.4"}]`); st != 501 {
		t.Fatalf("IP: %d", st)
	}
	if st, _, _ := b.pedir(t, "PATCH", ruta, `[{"action":"allow","target":"x.com","ports":[443]}]`); st != 501 {
		t.Fatalf("puertos: %d", st)
	}
	if st, m, _ := b.pedir(t, "DELETE", ruta, `["pypi.org","github.com"]`); st != 200 || m["mode"] != "deny_all" {
		t.Fatalf("delete: %d %v", st, m)
	}
	if req := b.gw.cuerpo("PATCH /v1/sandboxes/a/m1/egress"); req["egress"] != "none" {
		t.Fatalf("sin reglas: %v", req)
	}
	if st, m, _ := b.pedir(t, "PUT", ruta, `{"defaultAction":"allow"}`); st != 200 || m["mode"] != "allow_all" || b.gw.cuerpo("PATCH /v1/sandboxes/a/m1/egress")["egress"] != "internet" {
		t.Fatalf("put allow: %d %v", st, m)
	}
	// El sidecar de egress del SDK (puerto 18080) habla lo mismo.
	req, _ := http.NewRequest("GET", b.shim.URL+"/sandboxes/"+idOSB+"/port/18080/policy", nil)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", b.s.tokenExecd(idGW))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	json.NewDecoder(resp.Body).Decode(&p)
	resp.Body.Close()
	if resp.StatusCode != 200 || p["mode"] != "allow_all" {
		t.Fatalf("sidecar: %d %v", resp.StatusCode, p)
	}
}

func TestComandoSSE(t *testing.T) {
	b := nuevoBanco(t)
	st, raw, h := b.execd(t, "POST", "/command", `{"command":"echo hola","cwd":"/w","envs":{"A":"1"},"timeout":1500,"uid":1000,"gid":100}`)
	evs := eventos(t, raw)
	if st != 200 || h.Get("Content-Type") != "text/event-stream" || tipos(evs) != "init,stdout,stdout,execution_complete" {
		t.Fatalf("command: %d %s %s", st, h.Get("Content-Type"), raw)
	}
	if evs[1]["text"] != "hola\n" || evs[2]["text"] != "ñ\n" || evs[3]["exit_code"] != float64(0) || evs[3]["execution_time"] != float64(12) {
		t.Fatalf("eventos: %v", evs)
	}
	req := b.gw.cuerpo("POST /v1/sandboxes/a/m1/exec")
	if strings.TrimSpace(fmt.Sprintln(req["cmd"], req["dir"], req["env"], req["timeout_seconds"], req["user"])) != "[sh -c echo hola] /w [A=1] 2 1000:100" {
		t.Fatalf("exec: %v", req)
	}
	// argv va tal cual y un código distinto de 0 es un error con exit_code.
	_, raw, _ = b.execd(t, "POST", "/command", `{"argv":["falla","x"]}`)
	evs = eventos(t, raw)
	if tipos(evs) != "init,stderr,error" {
		t.Fatalf("falla: %s", raw)
	}
	e := evs[2]["error"].(map[string]any)
	if e["ename"] != "CommandExecError" || e["evalue"] != "3" || e["exit_code"] != float64(3) {
		t.Fatalf("error: %v", e)
	}
	if fmt.Sprint(b.gw.cuerpo("POST /v1/sandboxes/a/m1/exec")["cmd"]) != "[falla x]" {
		t.Fatal("argv")
	}
	// Estado y logs del comando en primer plano.
	cid := evs[0]["text"].(string)
	st, raw, _ = b.execd(t, "GET", "/command/status/"+cid, "")
	var est map[string]any
	json.Unmarshal(raw, &est)
	if st != 200 || est["running"] != false || est["exit_code"] != float64(3) || est["content"] != "falla x" {
		t.Fatalf("status: %d %s", st, raw)
	}
	if st, raw, _ := b.execd(t, "GET", "/command/"+cid+"/logs", ""); st != 200 || string(raw) != "mal\n" {
		t.Fatalf("logs: %d %q", st, raw)
	}
	if st, _, _ := b.execd(t, "POST", "/command", `{}`); st != 400 {
		t.Fatalf("sin command: %d", st)
	}
}

// TestComandoEventoAEvento comprueba que cada evento sale cuando llega, no al
// final: el gateway retiene el exec tras el primer trozo.
func TestComandoEventoAEvento(t *testing.T) {
	b := nuevoBanco(t)
	b.gw.bloquear = make(chan struct{})
	req, _ := http.NewRequest("POST", b.shim.URL+"/sandboxes/"+idOSB+"/port/44772/command", strings.NewReader(`{"command":"bloquea"}`))
	req.Header.Set("X-EXECD-ACCESS-TOKEN", b.s.tokenExecd(idGW))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	leido := make(chan string, 1)
	go func() {
		var acc []byte
		buf := make([]byte, 4096)
		for !bytes.Contains(acc, []byte("primero")) {
			n, err := resp.Body.Read(buf)
			acc = append(acc, buf[:n]...)
			if err != nil {
				break
			}
		}
		leido <- string(acc)
	}()
	select {
	case s := <-leido:
		if !strings.Contains(s, `"type":"init"`) || !strings.Contains(s, "primero") {
			t.Fatalf("faltan eventos: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("el stdout no llegó antes del final del comando")
	}
	close(b.gw.bloquear)
	resto, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(resto), "execution_complete") {
		t.Fatalf("final: %q", resto)
	}
}

func TestComandoFondo(t *testing.T) {
	b := nuevoBanco(t)
	_, raw, _ := b.execd(t, "POST", "/command", `{"command":"sleep 1","background":true}`)
	evs := eventos(t, raw)
	if tipos(evs) != "init,execution_complete" || evs[0]["text"] != "p77" {
		t.Fatalf("fondo: %s", raw)
	}
	st, raw, _ := b.execd(t, "GET", "/command/status/p77", "")
	var est map[string]any
	json.Unmarshal(raw, &est)
	if st != 200 || est["running"] != false || est["exit_code"] != float64(0) || est["content"] != "sh -c sleep 1" || est["finished_at"] == nil {
		t.Fatalf("status: %d %s", st, raw)
	}
	st, raw, h := b.execd(t, "GET", "/command/p77/logs?cursor=1", "")
	if st != 200 || string(raw) != "l2\nl3\n" || h.Get("EXECD-COMMANDS-TAIL-CURSOR") != "3" {
		t.Fatalf("logs: %d %q %v", st, raw, h)
	}
	if st, _, _ := b.execd(t, "DELETE", "/command?id=p77", ""); st != 200 || !b.gw.recibio("POST /v1/sandboxes/a/m1/processes/77/signal") {
		t.Fatalf("interrumpir: %d", st)
	}
	if st, _, _ := b.execd(t, "GET", "/command/status/p78", ""); st != 404 {
		t.Fatalf("proceso que no existe: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/command", ""); st != 400 {
		t.Fatalf("sin id: %d", st)
	}
}

func subir(t *testing.T, b *banco, ficheros map[string]string) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range ordenadas(ficheros) {
		mp, _ := mw.CreateFormFile("metadata", "metadata")
		fmt.Fprintf(mp, `{"path":%q,"mode":644}`, p)
		fp, _ := mw.CreateFormFile("file", p)
		io.WriteString(fp, ficheros[p])
	}
	mw.Close()
	st, raw, _ := b.execdCon(t, "POST", "/files/upload", nil, &buf, mw.FormDataContentType())
	if st != 200 {
		t.Logf("upload: %s", raw)
	}
	return st
}

func TestFicheros(t *testing.T) {
	b := nuevoBanco(t)
	if st := subir(t, b, map[string]string{"/w/a.txt": "uno\ndos\ntres\n", "/w/b.log": "0123456789"}); st != 200 {
		t.Fatalf("upload: %d", st)
	}
	if b.gw.modos["/w/a.txt"] != "0644" || !b.gw.recibio("PUT /v1/sandboxes/a/m1/files?mkdir=1&mode=0644&path=%2Fw%2Fa.txt") {
		t.Fatalf("subida: %v %v", b.gw.modos, b.gw.pets)
	}
	// Descarga entera, con Range (206), fuera de rango (416) y por líneas.
	if st, raw, _ := b.execd(t, "GET", "/files/download?path=/w/b.log", ""); st != 200 || string(raw) != "0123456789" {
		t.Fatalf("download: %d %q", st, raw)
	}
	st, raw, h := b.execdCon(t, "GET", "/files/download?path=/w/b.log", map[string]string{"Range": "bytes=2-4"}, nil, "")
	if st != 206 || string(raw) != "234" || h.Get("Content-Range") != "bytes 2-4/10" {
		t.Fatalf("range: %d %q %v", st, raw, h)
	}
	if st, raw, _ := b.execdCon(t, "GET", "/files/download?path=/w/b.log", map[string]string{"Range": "bytes=-3"}, nil, ""); st != 206 || string(raw) != "789" {
		t.Fatalf("sufijo: %d %q", st, raw)
	}
	if st, _, h := b.execdCon(t, "GET", "/files/download?path=/w/b.log", map[string]string{"Range": "bytes=20-"}, nil, ""); st != 416 || h.Get("Content-Range") != "bytes */10" {
		t.Fatalf("416: %d", st)
	}
	if st, raw, _ := b.execd(t, "GET", "/files/download?path=/w/a.txt&offset=2&limit=1", ""); st != 200 || string(raw) != "dos\n" {
		t.Fatalf("líneas: %d %q", st, raw)
	}
	if st, raw, _ := b.execd(t, "GET", "/files/download?path=/no", ""); st != 404 || !strings.Contains(string(raw), "FILE_NOT_FOUND") {
		t.Fatalf("no existe: %d %s", st, raw)
	}
	// info, replace, mv, search, permisos y borrado.
	st, raw, _ = b.execd(t, "GET", "/files/info?path=/w/a.txt&path=/w/b.log", "")
	var info map[string]map[string]any
	json.Unmarshal(raw, &info)
	if st != 200 || info["/w/a.txt"]["mode"] != float64(644) || info["/w/b.log"]["size"] != float64(10) || info["/w/a.txt"]["type"] != "file" {
		t.Fatalf("info: %d %s", st, raw)
	}
	st, raw, _ = b.execd(t, "POST", "/files/replace", `{"/w/a.txt":{"old":"o","new":"0"}}`)
	if st != 200 || !strings.Contains(string(raw), `"replacedCount":2`) || string(b.gw.ficheros["/w/a.txt"]) != "un0\nd0s\ntres\n" || b.gw.modos["/w/a.txt"] != "0644" {
		t.Fatalf("replace: %d %s %q", st, raw, b.gw.ficheros["/w/a.txt"])
	}
	if st, _, _ := b.execd(t, "POST", "/files/mv", `[{"src":"/w/b.log","dest":"/w/c.log"}]`); st != 200 || b.gw.ficheros["/w/c.log"] == nil {
		t.Fatalf("mv: %d", st)
	}
	st, raw, _ = b.execd(t, "GET", "/files/search?path=/w&pattern=*.log", "")
	if st != 200 || !strings.Contains(string(raw), `"path":"/w/c.log"`) || strings.Contains(string(raw), "a.txt") {
		t.Fatalf("search: %d %s", st, raw)
	}
	if st, _, _ := b.execd(t, "POST", "/files/permissions", `{"/w/a.txt":{"mode":600,"owner":"u","group":"g"}}`); st != 200 {
		t.Fatalf("permissions: %d", st)
	}
	if fmt.Sprint(b.gw.cuerpo("POST /v1/sandboxes/a/m1/exec")["cmd"]) != "[chown u:g -- /w/a.txt]" {
		t.Fatalf("chown: %v", b.gw.cuerpos["POST /v1/sandboxes/a/m1/exec"])
	}
	if st, _, _ := b.execd(t, "POST", "/files/permissions", `{"/w/a.txt":{"mode":968}}`); st != 400 {
		t.Fatalf("modo no octal: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/files?path=/w/a.txt", ""); st != 200 || b.gw.ficheros["/w/a.txt"] != nil {
		t.Fatalf("delete: %d", st)
	}
	// Directorios.
	st, raw, _ = b.execd(t, "GET", "/directories/list?path=/d&depth=2", "")
	if st != 200 || !strings.Contains(string(raw), `"path":"/d/sub/g.txt"`) || !strings.Contains(string(raw), `"type":"directory"`) {
		t.Fatalf("list: %d %s", st, raw)
	}
	if st, _, _ := b.execd(t, "POST", "/directories", `{"/w/nuevo":{"mode":755}}`); st != 200 || !b.gw.recibio("POST /v1/sandboxes/a/m1/files/mkdir") {
		t.Fatalf("mkdir: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/directories?path=/w/nuevo", ""); st != 200 || fmt.Sprint(b.gw.cuerpo("POST /v1/sandboxes/a/m1/exec")["cmd"]) != "[rm -rf -- /w/nuevo]" {
		t.Fatalf("rmdir: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/directories?path=/", ""); st != 400 {
		t.Fatalf("rm /: %d", st)
	}
}

func TestCodigo(t *testing.T) {
	b := nuevoBanco(t)
	st, raw, _ := b.execd(t, "POST", "/code/context", `{"language":"python"}`)
	if st != 200 || !strings.Contains(string(raw), `"language":"python"`) {
		t.Fatalf("context: %d %s", st, raw)
	}
	_, raw, _ = b.execd(t, "POST", "/code", `{"code":"print(1)","context":{"id":"ctxA","language":"python"}}`)
	evs := eventos(t, raw)
	if tipos(evs) != "init,stdout,execution_count,result,error,execution_complete" || evs[0]["text"] != "ctxA" {
		t.Fatalf("code: %s", raw)
	}
	if r := evs[3]["results"].(map[string]any); r["text"] != "2" || r["text/html"] != "<b>2</b>" {
		t.Fatalf("result: %v", r)
	}
	if e := evs[4]["error"].(map[string]any); e["ename"] != "ValueError" || evs[5]["execution_time"] != float64(4) {
		t.Fatalf("error/complete: %v", evs)
	}
	// Sin contexto: uno por defecto por lenguaje, que se reutiliza.
	b.execd(t, "POST", "/code", `{"code":"x=1"}`)
	b.execd(t, "POST", "/code", `{"code":"x"}`)
	n := 0
	for _, c := range b.gw.cuerpos["POST /v1/sandboxes/a/m1/code/contexts"] {
		if c.(map[string]any)["language"] == "python" {
			n++
		}
	}
	if n != 2 { // el de /code/context y el por defecto
		t.Fatalf("contextos creados: %d", n)
	}
	if st, raw, _ := b.execd(t, "GET", "/code/contexts?language=bash", ""); st != 200 || string(bytes.TrimSpace(raw)) != `[{"id":"ctxB","language":"bash"}]` {
		t.Fatalf("contexts: %d %s", st, raw)
	}
	if st, _, _ := b.execd(t, "GET", "/code/contexts/nada", ""); st != 404 {
		t.Fatalf("contexto que no existe: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/code/contexts?language=python", ""); st != 200 || !b.gw.recibio("DELETE /v1/sandboxes/a/m1/code/contexts/ctxA") {
		t.Fatalf("borrar por lenguaje: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/code?id=ctxA", ""); st != 200 || !b.gw.recibio("DELETE /v1/sandboxes/a/m1/code/contexts/ctxA/execution") {
		t.Fatalf("interrumpir: %d", st)
	}
}

func TestSesion(t *testing.T) {
	b := nuevoBanco(t)
	st, raw, _ := b.execd(t, "POST", "/session", `{"cwd":"/w"}`)
	var s map[string]string
	json.Unmarshal(raw, &s)
	if st != 200 || !strings.HasPrefix(s["session_id"], "osb-") {
		t.Fatalf("session: %d %s", st, raw)
	}
	if req := b.gw.cuerpo("POST /v1/sandboxes/a/m1/sessions"); req["id"] != s["session_id"] || req["dir"] != "/w" {
		t.Fatalf("sesión en el gateway: %v", req)
	}
	_, raw, _ = b.execd(t, "POST", "/session/"+s["session_id"]+"/run", `{"command":"echo hola","timeout":100}`)
	if tipos(eventos(t, raw)) != "init,stdout,stdout,execution_complete" {
		t.Fatalf("run: %s", raw)
	}
	if req := b.gw.cuerpo("POST /v1/sandboxes/a/m1/exec"); req["session"] != s["session_id"] || req["timeout_seconds"] != float64(1) {
		t.Fatalf("exec en sesión: %v", req)
	}
	if st, _, _ := b.execd(t, "DELETE", "/session/"+s["session_id"], ""); st != 200 {
		t.Fatalf("delete: %d", st)
	}
	if st, _, _ := b.execd(t, "DELETE", "/session/nada", ""); st != 404 {
		t.Fatalf("delete de una que no existe: %d", st)
	}
}

func TestMetricas(t *testing.T) {
	b := nuevoBanco(t)
	st, raw, _ := b.execd(t, "GET", "/metrics", "")
	var m map[string]float64
	json.Unmarshal(raw, &m)
	if st != 200 || m["cpu_count"] != 2 || m["mem_total_mib"] != 1024 || m["mem_used_mib"] != 512 || m["cpu_used_pct"] != 50 || m["timestamp"] == 0 {
		t.Fatalf("metrics: %d %s", st, raw)
	}
}

func TestOtroPuerto(t *testing.T) {
	b := nuevoBanco(t)
	if st, raw, _ := b.execdCon(t, "GET", "/", nil, nil, ""); st != 404 {
		t.Fatalf("raíz de execd: %d %s", st, raw)
	}
	req, _ := http.NewRequest("GET", b.shim.URL+"/sandboxes/"+idOSB+"/port/3000/app?x=1", nil)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", b.s.tokenExecd(idGW))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Fatalf("sin ingress: %d", resp.StatusCode)
	}
	// Con la entrada del gateway: Host reescrito y el token del inquilino.
	var visto *http.Request
	ing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visto = r.Clone(context.Background())
		io.WriteString(w, "app")
	}))
	defer ing.Close()
	u, _ := url.Parse(ing.URL)
	b2 := nuevoBanco(t, func(s *Servidor) { s.Ingreso = &Ingreso{Dominio: "sbx.test", URL: u, Token: tokenGW} })
	req, _ = http.NewRequest("GET", b2.shim.URL+"/sandboxes/"+idOSB+"/port/3000/app?x=1", nil)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", b2.s.tokenExecd(idGW))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cuerpo, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(cuerpo) != "app" || visto == nil {
		t.Fatalf("proxy: %d %s", resp.StatusCode, cuerpo)
	}
	if visto.Host != "3000-"+idOSB+".sbx.test" || visto.URL.Path != "/app" || visto.URL.RawQuery != "x=1" ||
		visto.Header.Get("Authorization") != "Bearer "+tokenGW || visto.Header.Get("X-EXECD-ACCESS-TOKEN") != "" {
		t.Fatalf("petición al ingress: host=%s path=%s q=%s cab=%v", visto.Host, visto.URL.Path, visto.URL.RawQuery, visto.Header)
	}
}

func TestClaveNoSeFiltra(t *testing.T) {
	b := nuevoBanco(t)
	b.pedirCon(t, "GET", "/v1/sandboxes?k="+clave, map[string]string{"OPEN-SANDBOX-API-KEY": clave}, "")
	b.mu.Lock()
	defer b.mu.Unlock()
	if strings.Contains(b.logs.String(), clave) || !strings.Contains(b.logs.String(), "GET /v1/sandboxes") {
		t.Fatalf("logs: %s", b.logs.String())
	}
}

func TestMemoriaYCPU(t *testing.T) {
	for q, want := range map[string]int{"512Mi": 512, "2Gi": 2048, "1G": 954, "1048576": 1, "1.5Gi": 1536} {
		if got, err := memMiBDe(q); err != nil || got != want {
			t.Errorf("memoria %s = %d %v, quería %d", q, got, err, want)
		}
	}
	for q, want := range map[string]int{"500m": 1, "1": 1, "2": 2, "2500m": 3} {
		if got, err := vcpusDe(q); err != nil || got != want {
			t.Errorf("cpu %s = %d %v, quería %d", q, got, err, want)
		}
	}
	if _, err := memMiBDe("mucho"); err == nil {
		t.Error("memoria inválida")
	}
}
