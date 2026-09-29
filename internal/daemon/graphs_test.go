package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// La API de grafos sin arrancar nada: capacidad, 404, validación, aristas
// en los dos sistemas y las etiquetas y el espacio del almacén reservados.
func TestGraphsAPI(t *testing.T) {
	_, h := testServer(t)

	var info api.Info
	if err := json.Unmarshal(call(t, h, "GET", "/info", "").Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Capabilities, "graphs") {
		t.Fatalf("sin la capacidad graphs: %v", info.Capabilities)
	}
	if rr := call(t, h, "GET", "/graphs", ""); rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("GET /graphs = %d %s", rr.Code, rr.Body)
	}
	for _, r := range [][2]string{{"GET", "/graphs/nada"}, {"POST", "/graphs/nada/freeze"}, {"POST", "/graphs/nada/thaw"},
		{"POST", "/graphs/nada/snapshot"}, {"POST", "/graphs/nada/fork"}, {"DELETE", "/graphs/nada"}} {
		if rr := call(t, h, r[0], r[1], ""); rr.Code != 404 {
			t.Errorf("%s %s = %d, quería 404: %s", r[0], r[1], rr.Code, rr.Body)
		}
	}
	// Validación: 400 con el motivo.
	rr := call(t, h, "POST", "/graphs", `{"graph":{"name":"Mal","nodes":{"a":{"image":"min"}}}}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "graph name") {
		t.Fatalf("nombre inválido = %d %s", rr.Code, rr.Body)
	}
	rr = call(t, h, "POST", "/graphs", `{"graph":{"name":"x","nodes":{"a":{}}}}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "needs from") {
		t.Fatalf("nodo sin from ni image = %d %s", rr.Code, rr.Body)
	}
	// Una arista entre máquinas ya no es un 501 en ningún sistema (en macOS
	// la sirve el broker de enlaces): aquí falla, si falla, por arrancar.
	con := `{"graph":{"name":"y","nodes":{"a":{"image":"min"},"b":{"image":"min","ports":[8081]}},` +
		`"edges":[{"from":"a","to":"b","kind":"link","port":8081}]}}`
	rr = call(t, h, "POST", "/graphs", con)
	if rr.Code == 501 || strings.Contains(rr.Body.String(), "Linux-only") {
		t.Fatalf("aristas = %d %s: no deben ser solo de Linux", rr.Code, rr.Body)
	}

	// Nadie pone etiquetas de grafo por la API de máquinas.
	rr = call(t, h, "POST", "/machines", `{"image":"min","labels":{"kling.graph":"0123456789abcdef"}}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "reserved") {
		t.Fatalf("run con kling.graph = %d %s", rr.Code, rr.Body)
	}
	rr = call(t, h, "POST", "/sandboxes", `{"image":"min","labels":{"kling.graph.node":"db"}}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "reserved") {
		t.Fatalf("sandbox con kling.graph.node = %d %s", rr.Code, rr.Body)
	}
	// Ni escribe el espacio del almacén donde viven los grafos.
	if rr := call(t, h, "PUT", "/store/graph/0123456789abcdef", `{"id":"0123456789abcdef"}`); rr.Code != 403 {
		t.Fatalf("PUT /store/graph = %d, quería 403", rr.Code)
	}
	if rr := call(t, h, "DELETE", "/store/graph/0123456789abcdef", ""); rr.Code != 403 {
		t.Fatalf("DELETE /store/graph = %d, quería 403", rr.Code)
	}
}
