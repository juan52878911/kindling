package mcpsbx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// apiFalsa guarda lo que recibe y contesta con lo guionizado por ruta.
type apiFalsa struct {
	mu    sync.Mutex
	pets  []petFalsa
	resp  map[string]respFalsa // "MÉTODO ruta" -> respuesta
	otros respFalsa
}

type petFalsa struct{ Metodo, Ruta, Cuerpo, Tipo string }

type respFalsa struct {
	Codigo int
	Cuerpo string
}

func (a *apiFalsa) Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error) {
	var b []byte
	if cuerpo != nil {
		b, _ = io.ReadAll(cuerpo)
	}
	a.mu.Lock()
	a.pets = append(a.pets, petFalsa{metodo, ruta, string(b), tipo})
	r, ok := a.resp[metodo+" "+ruta]
	a.mu.Unlock()
	if !ok {
		r = a.otros
	}
	if r.Codigo == 0 {
		r.Codigo = 200
	}
	return &http.Response{StatusCode: r.Codigo, Status: http.StatusText(r.Codigo), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(r.Cuerpo))}, nil
}

func (a *apiFalsa) ultima(t *testing.T) petFalsa {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pets) == 0 {
		t.Fatal("no hubo peticiones")
	}
	return a.pets[len(a.pets)-1]
}

// llama invoca una herramienta por el camino real (JSON-RPC), no por el mapa.
func llama(t *testing.T, a API, nombre string, args any) (string, bool) {
	t.Helper()
	ab, _ := json.Marshal(args)
	p, _ := json.Marshal(map[string]any{"name": nombre, "arguments": json.RawMessage(ab)})
	var out bytes.Buffer
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(p) + "}\n"
	s := &Servidor{API: a, Nombre: "t", Version: "0"}
	if err := s.Servir(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool `json:"isError"`
		}
		Error *errRPC
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("respuesta ilegible %q: %v", out.String(), err)
	}
	if r.Error != nil {
		t.Fatalf("error RPC: %+v", r.Error)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

func TestToolsListDevuelve16(t *testing.T) {
	var out bytes.Buffer
	s := &Servidor{API: &apiFalsa{}, Nombre: "t", Version: "0"}
	s.Servir(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out)
	var r struct{ Result struct{ Tools []herramienta } }
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Result.Tools) != 16 {
		t.Fatalf("tools/list devuelve %d herramientas, quiero 16", len(r.Result.Tools))
	}
	vistas := map[string]bool{}
	for _, h := range r.Result.Tools {
		vistas[h.Name] = true
		if _, ok := manejadores[h.Name]; !ok {
			t.Errorf("%s anunciada sin manejador", h.Name)
		}
	}
	for _, n := range []string{"sandbox_run_code", "sandbox_pause", "sandbox_resume", "sandbox_list_files", "sandbox_open_port", "sandbox_set_egress", "sandbox_fork"} {
		if !vistas[n] {
			t.Errorf("falta %s", n)
		}
	}
	if len(manejadores) != 16 {
		t.Errorf("hay %d manejadores", len(manejadores))
	}
}

func TestHerramientasNuevasLlamanASuRuta(t *testing.T) {
	casos := []struct {
		nombre, metodo, ruta string
		args                 map[string]any
		cuerpo               map[string]any // nil = sin comprobar / sin cuerpo
	}{
		{"sandbox_pause", "POST", "/v1/sandboxes/lab/m1/pause", map[string]any{"id": "lab/m1"}, nil},
		{"sandbox_resume", "POST", "/v1/sandboxes/lab/m1/resume", map[string]any{"id": "lab/m1"}, nil},
		{"sandbox_fork", "POST", "/v1/sandboxes/lab/m1/fork", map[string]any{"id": "lab/m1", "count": 2, "ttl_seconds": 90},
			map[string]any{"count": float64(2), "ttl_seconds": float64(90)}},
		{"sandbox_open_port", "POST", "/v1/sandboxes/lab/m1/endpoints", map[string]any{"id": "lab/m1", "port": 8080, "public": true},
			map[string]any{"port": float64(8080), "public": true}},
		{"sandbox_set_egress", "PATCH", "/v1/sandboxes/lab/m1/egress",
			map[string]any{"id": "lab/m1", "egress": "allowlist", "allow_domains": []string{"pypi.org"}},
			map[string]any{"egress": "allowlist", "allow_domains": []any{"pypi.org"}}},
		{"sandbox_list_files", "GET", "/v1/sandboxes/lab/m1/files?list=1&path=%2Fwork+dir", map[string]any{"id": "lab/m1", "path": "/work dir"}, nil},
		{"sandbox_run_code", "POST", "/v1/sandboxes/lab/m1/code", map[string]any{"id": "lab/m1", "code": "1+1", "language": "python", "timeout_seconds": 5},
			map[string]any{"code": "1+1", "language": "python", "timeout_seconds": float64(5)}},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			a := &apiFalsa{otros: respFalsa{Cuerpo: `{"entries":[],"state":"paused"}`}}
			txt, esErr := llama(t, a, c.nombre, c.args)
			if esErr {
				t.Fatalf("error inesperado: %s", txt)
			}
			p := a.ultima(t)
			if p.Metodo != c.metodo || p.Ruta != c.ruta {
				t.Fatalf("llamó %s %s, quiero %s %s", p.Metodo, p.Ruta, c.metodo, c.ruta)
			}
			if c.cuerpo != nil {
				var got map[string]any
				if err := json.Unmarshal([]byte(p.Cuerpo), &got); err != nil {
					t.Fatalf("cuerpo %q: %v", p.Cuerpo, err)
				}
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(c.cuerpo)
				if string(gb) != string(wb) {
					t.Errorf("cuerpo %s, quiero %s", gb, wb)
				}
			}
		})
	}
}

func TestHerramientasNuevasValidan(t *testing.T) {
	casos := []struct {
		nombre string
		args   map[string]any
		quiere string
	}{
		{"sandbox_pause", map[string]any{"id": "../x"}, "invalid sandbox id"},
		{"sandbox_resume", map[string]any{}, "id is required"},
		{"sandbox_fork", map[string]any{"id": "lab/m1", "count": -1}, "count"},
		{"sandbox_open_port", map[string]any{"id": "lab/m1"}, "port"},
		{"sandbox_open_port", map[string]any{"id": "lab/m1", "port": 70000}, "port"},
		{"sandbox_set_egress", map[string]any{"id": "lab/m1"}, "egress is required"},
		{"sandbox_list_files", map[string]any{"id": "lab/m1"}, "path is required"},
		{"sandbox_run_code", map[string]any{"id": "lab/m1", "code": " "}, "code is required"},
	}
	for _, c := range casos {
		a := &apiFalsa{}
		txt, esErr := llama(t, a, c.nombre, c.args)
		if !esErr || !strings.Contains(txt, c.quiere) {
			t.Errorf("%s %v: err=%v %q, quiero error con %q", c.nombre, c.args, esErr, txt, c.quiere)
		}
		if len(a.pets) != 0 {
			t.Errorf("%s hizo %d peticiones con argumentos inválidos", c.nombre, len(a.pets))
		}
	}
}

func TestRunCodeFormateaElFlujo(t *testing.T) {
	flujo := strings.Join([]string{
		`{"type":"stdout","text":"hola\n"}`,
		`{"type":"stderr","text":"aviso\n"}`,
		`{"type":"result","text":"2","results":{"text/plain":"2"}}`,
		`{"type":"error","ename":"ZeroDivisionError","evalue":"division by zero","traceback":["t1"]}`,
		`{"type":"done","ok":false,"duration_ms":12}`,
	}, "\n") + "\n"
	a := &apiFalsa{otros: respFalsa{Cuerpo: flujo}}
	txt, esErr := llama(t, a, "sandbox_run_code", map[string]any{"id": "lab/m1", "code": "1/0"})
	if esErr {
		t.Fatalf("un error del código no es un error de la herramienta: %s", txt)
	}
	for _, q := range []string{"ok: false", "hola", "aviso", "result: 2", "ZeroDivisionError: division by zero"} {
		if !strings.Contains(txt, q) {
			t.Errorf("falta %q en:\n%s", q, txt)
		}
	}
}

func TestRunCodeAcotaLaSalida(t *testing.T) {
	grande := strings.Repeat("x", 3*MaxSalida)
	linea, _ := json.Marshal(map[string]any{"type": "stdout", "text": grande})
	a := &apiFalsa{otros: respFalsa{Cuerpo: string(linea) + "\n" + `{"type":"done","ok":true}` + "\n"}}
	txt, _ := llama(t, a, "sandbox_run_code", map[string]any{"id": "lab/m1", "code": "print(1)"})
	if !strings.Contains(txt, "[truncated: stdout is ") || len(txt) > 2*MaxSalida {
		t.Errorf("sin tope: len=%d", len(txt))
	}
}

func TestListFilesFormateaYAcota(t *testing.T) {
	a := &apiFalsa{otros: respFalsa{Cuerpo: `{"path":"/w","entries":[{"name":"a.txt","type":"file","size":3,"mode":"0644"},{"name":"sub","type":"dir","mode":"0755"}],"truncated":true}`}}
	txt, esErr := llama(t, a, "sandbox_list_files", map[string]any{"id": "lab/m1", "path": "/w"})
	if esErr {
		t.Fatal(txt)
	}
	for _, q := range []string{"a.txt", "sub/", "listing truncated"} {
		if !strings.Contains(txt, q) {
			t.Errorf("falta %q en:\n%s", q, txt)
		}
	}
	// Un listado enorme se recorta como el resto.
	var sb strings.Builder
	sb.WriteString(`{"entries":[`)
	for i := range 20000 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"name":"fichero-con-nombre-largo-`)
		sb.WriteString(strings.Repeat("a", 20))
		sb.WriteString(`","type":"file","size":1,"mode":"0644"}`)
	}
	sb.WriteString(`]}`)
	a = &apiFalsa{otros: respFalsa{Cuerpo: sb.String()}}
	txt, _ = llama(t, a, "sandbox_list_files", map[string]any{"id": "lab/m1", "path": "/w"})
	if len(txt) > 2*MaxSalida || !strings.Contains(txt, "[truncated:") {
		t.Errorf("listado sin tope: len=%d", len(txt))
	}
}

func TestErrorDelGatewayEnHerramientaNueva(t *testing.T) {
	a := &apiFalsa{otros: respFalsa{Codigo: 409, Cuerpo: `{"message":"already paused"}`}}
	txt, esErr := llama(t, a, "sandbox_pause", map[string]any{"id": "lab/m1"})
	if !esErr || !strings.Contains(txt, "already paused") || !strings.Contains(txt, "HTTP 409") {
		t.Errorf("err=%v %q", esErr, txt)
	}
}
