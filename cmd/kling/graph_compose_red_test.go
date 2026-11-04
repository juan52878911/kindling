package main

import (
	"fmt"
	"strings"
	"testing"
)

// La red del proyecto: sin depends_on, cada servicio alcanza a los de sus
// redes; networks: separa; un puerto repetido lo gana el de depends_on.
func TestComposeRedProyecto(t *testing.T) {
	src := `services:
  app:
    image: x/app
    ports: ["3000"]
    environment: {REDIS: "redis://cache:6379", OTRO: "http://aislado:9000"}
    depends_on: [api2]
  cache: {image: redis:7-alpine}
  api1: {image: x/a, ports: ["8081"]}
  api2: {image: x/b, ports: ["8081"]}
  aislado: {image: x/c, ports: ["9000"], networks: [solo]}
networks:
  solo: {}
`
	tr, err := traducirCompose([]byte(src), "p", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"{from: app, to: cache, kind: link, port: 6379}",
		"{from: cache, to: app, kind: link, port: 3000}",
		"{from: app, to: api2, kind: link, port: 8081}",
		"{from: cache, to: api1, kind: link, port: 8081}",
	} {
		if !strings.Contains(tr.Grafo, want) {
			t.Errorf("missing %s in\n%s", want, tr.Grafo)
		}
	}
	for _, no := range []string{"to: aislado", "from: aislado", "{from: app, to: api1"} {
		if strings.Contains(tr.Grafo, no) {
			t.Errorf("unexpected %s in\n%s", no, tr.Grafo)
		}
	}
	// Un valor suelto igual a un servicio solo es un host en una variable de
	// host: POSTGRES_USER=app es un usuario.
	src2 := "services:\n  app: {image: x/a, ports: [\"3000\"]}\n  db:\n    image: postgres:16\n    environment: {POSTGRES_USER: app, PEER_HOST: app}\n"
	tr2, err := traducirCompose([]byte(src2), "p", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range tr2.por["db"].env {
		if kv[0] == "POSTGRES_USER" && kv[1] != "app" || kv[0] == "PEER_HOST" && kv[1] != "app.graph" {
			t.Errorf("%s = %s", kv[0], kv[1])
		}
	}
	// El host de un servicio de otra red no se reescribe.
	for _, kv := range tr.por["app"].env {
		if kv[0] == "OTRO" && kv[1] != "http://aislado:9000" {
			t.Errorf("OTRO = %s", kv[1])
		}
		if kv[0] == "REDIS" && kv[1] != "redis://cache.graph:6379" {
			t.Errorf("REDIS = %s", kv[1])
		}
	}
}

// network_mode: none no alcanza ni es alcanzado.
func TestComposeRedNone(t *testing.T) {
	src := "services:\n  a: {image: x/a, ports: [\"1000\"]}\n  b: {image: x/b, ports: [\"2000\"], network_mode: none}\n"
	tr, err := traducirCompose([]byte(src), "p", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tr.Grafo, "kind: link") {
		t.Errorf("links with network_mode none:\n%s", tr.Grafo)
	}
}

// La red de un proyecto de 11 servicios es una malla completa (110 links):
// el tope de 64 es por nodo, no del grafo.
func TestComposeRedMalla(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "  s%d: {image: x/s, ports: [\"%d\"]}\n", i, 9000+i)
	}
	b.WriteString("  app: {image: x/app, ports: [\"3000\"], depends_on: [s1]}\n")
	tr, err := traducirCompose([]byte(b.String()), "p", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(tr.Grafo, "kind: link"); n != 110 {
		t.Errorf("%d links, want 110", n)
	}
	if strings.Contains(strings.Join(tr.Avisos, "\n"), "over the limit") {
		t.Errorf("avisos = %v", tr.Avisos)
	}
}

// Un servicio que alcanzaría más de 64 puertos se queda con los que usa; los
// demás conservan la malla.
func TestComposeRedTopePorNodo(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n")
	for i := 0; i < 5; i++ {
		var ps []string
		for j := 0; j < 14; j++ {
			ps = append(ps, fmt.Sprintf("\"%d\"", 9000+i*100+j))
		}
		fmt.Fprintf(&b, "  s%d: {image: x/s, ports: [%s]}\n", i, strings.Join(ps, ", "))
	}
	b.WriteString("  app: {image: x/app, depends_on: [s1]}\n")
	tr, err := traducirCompose([]byte(b.String()), "p", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(tr.Grafo, "{from: app, to: s1, kind: link"); n != 14 {
		t.Errorf("app -> s1: %d links, want 14:\n%s", n, tr.Grafo)
	}
	if strings.Contains(tr.Grafo, "{from: app, to: s0,") {
		t.Errorf("app still reaches s0:\n%s", tr.Grafo)
	}
	if n := strings.Count(tr.Grafo, "{from: s0, to: s4, kind: link"); n != 14 {
		t.Errorf("s0 -> s4: %d links, want 14", n)
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), "service app: reaching every service of its networks needs 70 links, over the limit of 64 per node") {
		t.Errorf("avisos = %v", tr.Avisos)
	}
}

// Más aristas que el tope del grafo: quedan solo las que se usan.
func TestComposeRedTope(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "  s%d: {image: x/s, ports: [\"%d\", \"%d\"]}\n", i, 9000+i, 10000+i)
	}
	b.WriteString("  app: {image: x/app, ports: [\"3000\"], depends_on: [s1]}\n")
	tr, err := traducirCompose([]byte(b.String()), "p", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(tr.Grafo, "kind: link"); n != 2 {
		t.Errorf("%d links, want 2:\n%s", n, tr.Grafo)
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), "over the limit of 1024") {
		t.Errorf("avisos = %v", tr.Avisos)
	}
}
