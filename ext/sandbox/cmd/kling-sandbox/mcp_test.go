package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const tokenMCP = "tok-SECRETO-9f3a1c"

// gatewayFalso imita lo justo de /v1 y exige el Authorization. Si el token es
// otro, contesta 401 repitiendo la cabecera en el mensaje (peor caso para la
// fuga de secretos).
func gatewayFalso(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	ficheros := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if got := r.Header.Get("Authorization"); got != "Bearer "+tokenMCP {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"message": "bad credentials: " + got})
			return
		}
		p := r.URL.Path
		switch {
		case r.Method == "POST" && p == "/v1/sandboxes":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["template"] != "py" || b["ttl_seconds"] != float64(120) || b["labels"].(map[string]any)["k"] != "v" {
				w.WriteHeader(400)
				io.WriteString(w, `{"message":"bad create body"}`)
				return
			}
			io.WriteString(w, `{"id":"lab/m1","host":"lab","state":"running"}`)
		case r.Method == "GET" && p == "/v1/sandboxes":
			io.WriteString(w, `[{"id":"lab/m1","state":"running"}]`)
		case r.Method == "POST" && p == "/v1/sandboxes/lab/m1/exec":
			var b struct {
				Cmd []string `json:"cmd"`
				Dir string   `json:"dir"`
				Env []string `json:"env"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			if r.URL.Query().Get("wait") != "1" || b.Dir != "/work" || len(b.Env) != 1 || b.Env[0] != "A=1" {
				w.WriteHeader(400)
				io.WriteString(w, `{"message":"bad exec"}`)
				return
			}
			out := strings.Join(b.Cmd, " ")
			if b.Cmd[0] == "big" {
				out = strings.Repeat("x", 200<<10)
			}
			json.NewEncoder(w).Encode(map[string]any{"exit_code": 3, "stdout": []byte(out), "stderr": []byte("oops")})
		case r.Method == "PUT" && p == "/v1/sandboxes/lab/m1/files":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			ficheros[r.URL.Query().Get("path")] = string(b)
			mu.Unlock()
			w.WriteHeader(204)
		case r.Method == "GET" && p == "/v1/sandboxes/lab/m1/files":
			mu.Lock()
			c, ok := ficheros[r.URL.Query().Get("path")]
			mu.Unlock()
			if r.URL.Query().Get("path") == "/big" {
				c, ok = strings.Repeat("y", 300<<10), true
			}
			if !ok {
				w.WriteHeader(404)
				io.WriteString(w, `{"message":"no such file"}`)
				return
			}
			io.WriteString(w, c)
		case r.Method == "POST" && p == "/v1/sandboxes/lab/m1/renew":
			io.WriteString(w, `{"id":"lab/m1","ttl_seconds":600}`)
		case r.Method == "DELETE" && p == "/v1/sandboxes/lab/m1":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"no sandbox \"x\""}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type sesionMCP struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Reader
	id  int
}

func (s *sesionMCP) rpc(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	s.in.Write(append(b, '\n'))
	linea, err := s.out.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("%s: %v", method, err)
	}
	var r map[string]any
	if err := json.Unmarshal(linea, &r); err != nil {
		s.t.Fatalf("%s: %v: %s", method, err, linea)
	}
	if int(r["id"].(float64)) != s.id {
		s.t.Fatalf("id %v, quería %d", r["id"], s.id)
	}
	return r
}

// llamar devuelve (texto, isError) de una tools/call.
func (s *sesionMCP) llamar(name string, args any) (string, bool) {
	s.t.Helper()
	r := s.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if r["error"] != nil {
		s.t.Fatalf("%s: error JSON-RPC %v", name, r["error"])
	}
	res := r["result"].(map[string]any)
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	e, _ := res["isError"].(bool)
	return txt, e
}

// arrancarMCP lanza el servidor sobre pipes. Devuelve la sesión y una función
// que cierra stdin y devuelve todo lo que salió por stdout y stderr.
func arrancarMCP(t *testing.T, url, token string) (*sesionMCP, func() string) {
	t.Helper()
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_SANDBOX_URL", url)
	t.Setenv("KLING_SANDBOX_TOKEN", token)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errF, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	viejo := os.Stderr
	os.Stderr = errF
	t.Cleanup(func() { os.Stderr = viejo })
	fin := make(chan error, 1)
	go func() { e := servirMCP(nil, inR, outW); outW.Close(); fin <- e }()
	s := &sesionMCP{t: t, in: inW, out: bufio.NewReader(outR)}
	return s, func() string {
		inW.Close()
		if e := <-fin; e != nil {
			t.Fatal(e)
		}
		b, _ := os.ReadFile(errF.Name())
		return string(b)
	}
}

func TestMCPPuntaAPunta(t *testing.T) {
	srv := gatewayFalso(t)
	s, cerrar := arrancarMCP(t, srv.URL, tokenMCP)
	var todo strings.Builder

	r := s.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	if r["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %v", r)
	}
	s.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	s.rpc("ping", nil)

	r = s.rpc("tools/list", nil)
	nombres := map[string]bool{}
	for _, x := range r["result"].(map[string]any)["tools"].([]any) {
		nombres[x.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"sandbox_create", "sandbox_list", "sandbox_exec", "sandbox_read_file", "sandbox_write_file", "sandbox_delete", "sandbox_renew"} {
		if !nombres[n] {
			t.Errorf("falta la herramienta %s", n)
		}
	}

	ok := func(name string, args any, quiere string) string {
		t.Helper()
		txt, e := s.llamar(name, args)
		todo.WriteString(txt)
		if e || !strings.Contains(txt, quiere) {
			t.Errorf("%s: isError=%v, %q no contiene %q", name, e, txt, quiere)
		}
		return txt
	}
	ok("sandbox_create", map[string]any{"template": "py", "ttl_seconds": 120, "labels": map[string]string{"k": "v"}}, `"lab/m1"`)
	ok("sandbox_list", map[string]any{}, "lab/m1")
	txt := ok("sandbox_exec", map[string]any{"id": "lab/m1", "command": []string{"echo", "hola"}, "cwd": "/work", "env": map[string]string{"A": "1"}}, "exit: 3")
	if !strings.Contains(txt, "echo hola") || !strings.Contains(txt, "oops") {
		t.Errorf("exec: %q", txt)
	}
	ok("sandbox_write_file", map[string]any{"id": "lab/m1", "path": "/a b.txt", "content": "contenido"}, "wrote 9 bytes")
	ok("sandbox_read_file", map[string]any{"id": "lab/m1", "path": "/a b.txt"}, "contenido")
	ok("sandbox_renew", map[string]any{"id": "lab/m1", "ttl_seconds": 600}, "600")
	ok("sandbox_delete", map[string]any{"id": "lab/m1"}, "deleted")

	// Errores de la API: isError, no error JSON-RPC.
	txt, e := s.llamar("sandbox_read_file", map[string]any{"id": "lab/m1", "path": "/nada"})
	if !e || !strings.Contains(txt, "no such file") || !strings.Contains(txt, "404") {
		t.Errorf("404: %v %q", e, txt)
	}
	txt, e = s.llamar("sandbox_delete", map[string]any{"id": "lab/otro"})
	if !e || !strings.Contains(txt, "404") {
		t.Errorf("404 delete: %v %q", e, txt)
	}
	if txt, e = s.llamar("sandbox_delete", map[string]any{"id": "../x"}); !e || !strings.Contains(txt, "invalid sandbox id") {
		t.Errorf("id inválido: %v %q", e, txt)
	}
	if txt, e = s.llamar("sandbox_exec", map[string]any{"id": "lab/m1"}); !e {
		t.Errorf("exec sin command: %q", txt)
	}

	// Salidas enormes: truncadas con aviso.
	txt, e = s.llamar("sandbox_exec", map[string]any{"id": "lab/m1", "command": []string{"big"}, "cwd": "/work", "env": map[string]string{"A": "1"}})
	if e || !strings.Contains(txt, "[truncated: stdout is 204800 bytes") || len(txt) > 70<<10 {
		t.Errorf("exec enorme: err=%v len=%d", e, len(txt))
	}
	txt, e = s.llamar("sandbox_read_file", map[string]any{"id": "lab/m1", "path": "/big"})
	if e || !strings.Contains(txt, "[truncated: file is 307200 bytes") || len(txt) > 70<<10 {
		t.Errorf("fichero enorme: err=%v len=%d", e, len(txt))
	}

	// Método y herramienta desconocidos sí son errores de protocolo.
	if s.rpc("nope", nil)["error"] == nil || s.rpc("tools/call", map[string]any{"name": "zzz"})["error"] == nil {
		t.Error("método/herramienta desconocidos deberían dar error JSON-RPC")
	}
	stderr := cerrar()
	if strings.Contains(todo.String()+stderr, tokenMCP) {
		t.Error("el token aparece en la salida")
	}
}

// Un 401 que repite la cabecera Authorization en su mensaje no debe filtrar el
// token: ni en el resultado, ni en stderr, ni en el error de arranque.
func TestMCPTokenNoSale(t *testing.T) {
	srv := gatewayFalso(t)
	// El servidor falso espera tokenMCP; el cliente manda otro que sí es
	// "secreto" y que el 401 devolvería.
	malo := "tok-OTRO-77b2"
	s, cerrar := arrancarMCP(t, srv.URL, malo)
	txt, e := s.llamar("sandbox_list", map[string]any{})
	if !e || !strings.Contains(txt, "401") {
		t.Fatalf("401: %v %q", e, txt)
	}
	stderr := cerrar()
	for _, o := range []string{txt, stderr} {
		if strings.Contains(o, malo) {
			t.Errorf("el token sale en %q", o)
		}
	}

	// Gateway caído: el error de red tampoco lleva el token.
	srv.Close()
	s, cerrar = arrancarMCP(t, srv.URL, tokenMCP)
	txt, e = s.llamar("sandbox_list", map[string]any{})
	if !e || strings.Contains(txt, tokenMCP) {
		t.Errorf("gateway caído: %v %q", e, txt)
	}
	if strings.Contains(cerrar(), tokenMCP) {
		t.Error("token en stderr")
	}

	// Sin token: el error de arranque no lo menciona (no hay) y no entra en pánico.
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_SANDBOX_TOKEN", "")
	if err := servirMCP(nil, strings.NewReader(""), io.Discard); err == nil {
		t.Error("sin token debería fallar")
	}
}
