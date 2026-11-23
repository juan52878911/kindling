package frontal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// BenchmarkExecConNSandboxes mide un exec ?wait=1 por el gateway con N
// sandboxes más en el host (D-01): resolver el id no debería costar más con
// 1000 que con 10. Da el p50 por operación y los listados del host por
// operación.
func BenchmarkExecConNSandboxes(b *testing.B) {
	for _, n := range []int{10, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			f := nuevoFalso(b)
			s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(map[string]string{"a": f.endpoint}),
				Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}},
				Log:     log.New(io.Discard, "", 0)})
			if err != nil {
				b.Fatal(err)
			}
			srv := httptest.NewServer(s)
			defer srv.Close()
			id := crearBench(b, srv)
			// El resto son de otro inquilino, con etiquetas como las de verdad.
			f.mu.Lock()
			var labels map[string]string
			for _, mc := range f.maquinas {
				labels = mc.Labels
			}
			f.mu.Unlock()
			for i := 1; i < n; i++ {
				l := api.MergeLabels(nil, labels)
				l[LabelTenant] = "bob"
				f.sembrar(&api.Machine{Image: "base", Labels: l})
			}
			cuerpo, _ := json.Marshal(api.ExecRequest{Cmd: []string{"true"}})
			cli := &http.Client{}
			f.mu.Lock()
			antes := f.sandboxesLlamadas
			f.mu.Unlock()
			ds := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ini := time.Now()
				req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes/"+id+"/exec?wait=1", bytes.NewReader(cuerpo))
				req.Header.Set("Authorization", "Bearer "+tokenAlice)
				resp, err := cli.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					b.Fatalf("status %d", resp.StatusCode)
				}
				ds = append(ds, time.Since(ini))
			}
			b.StopTimer()
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			b.ReportMetric(float64(ds[len(ds)/2].Microseconds()), "p50-µs")
			f.mu.Lock()
			b.ReportMetric(float64(f.sandboxesLlamadas-antes)/float64(b.N), "listados/op")
			f.mu.Unlock()
		})
	}
}

func crearBench(b *testing.B, srv *httptest.Server) string {
	b.Helper()
	cuerpo, _ := json.Marshal(CrearPeticion{Image: "base"})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", bytes.NewReader(cuerpo))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		bs, _ := io.ReadAll(resp.Body)
		b.Fatalf("create: %d %s", resp.StatusCode, bs)
	}
	var sb Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
		b.Fatal(err)
	}
	return sb.ID
}

// D-01: resolver un id pide esa máquina al host y no lista el host; y lo que
// vuelve se sigue comprobando: una máquina que no es sandbox, aunque lleve la
// etiqueta del inquilino, da el mismo 404 que una que no existe.
func TestBuscarPorIDSinListarElHost(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	exec := func(tok, id string) *http.Response {
		return pide(t, srv, tok, "POST", "/v1/sandboxes/"+id+"/exec?wait=1", api.ExecRequest{Cmd: []string{"true"}})
	}
	f.mu.Lock()
	antes := f.sandboxesLlamadas
	f.mu.Unlock()
	resp := exec(tokenAlice, sb.ID)
	esperaCodigo(t, resp, 200)
	resp.Body.Close()
	f.mu.Lock()
	listados := f.sandboxesLlamadas - antes
	f.mu.Unlock()
	if listados != 0 {
		t.Fatalf("exec listed the host %d times", listados)
	}

	esperaCodigo(t, exec(tokenBob, sb.ID), 404)
	f.mu.Lock()
	ref := sb.ID[len("a/"):]
	otra := *f.maquinas[ref]
	otra.ID = "0123456789abcdef"
	otra.Labels = api.MergeLabels(nil, f.maquinas[ref].Labels)
	otra.Labels[api.LabelKind] = "service"
	f.maquinas[otra.ID] = &otra
	f.mu.Unlock()
	esperaCodigo(t, exec(tokenAlice, "a/"+otra.ID), 404)
	esperaCodigo(t, exec(tokenAlice, "a/fedcba9876543210"), 404)
}
