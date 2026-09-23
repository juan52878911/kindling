package main

import "testing"

// parseHosts es lo único de -hosts/mcp.hosts que se puede probar sin un
// daemon: el resto (levantar un Gateway por host, construir el Router) lo
// ejercitan los tests de internal/gateway y scripts/90-e2e.sh.
func TestParseHostsVacioEsUnSoloHostDeSiempre(t *testing.T) {
	hosts, err := parseHosts("")
	if err != nil {
		t.Fatalf("spec vacío no debería fallar: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("spec vacío = %v, quería ningún host (comportamiento de antes de -hosts)", hosts)
	}
}

func TestParseHostsVariosPares(t *testing.T) {
	hosts, err := parseHosts("mac=unix:///tmp/kling.sock, lab = ssh://juan@192.168.2.60 ")
	if err != nil {
		t.Fatalf("no debería fallar: %v", err)
	}
	want := []hostSpec{
		{Name: "mac", Endpoint: "unix:///tmp/kling.sock"},
		{Name: "lab", Endpoint: "ssh://juan@192.168.2.60"},
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts=%v, quería %v", hosts, want)
	}
	for i, w := range want {
		if hosts[i] != w {
			t.Errorf("host %d = %+v, quería %+v", i, hosts[i], w)
		}
	}
}

func TestParseHostsEntradaInvalida(t *testing.T) {
	casos := []string{
		"sin-igual",
		"=sin-nombre",
		"nombre=",
	}
	for _, c := range casos {
		if _, err := parseHosts(c); err == nil {
			t.Errorf("parseHosts(%q) debería fallar (formato inválido)", c)
		}
	}
}

func TestParseHostsNombreDuplicado(t *testing.T) {
	if _, err := parseHosts("a=x,a=y"); err == nil {
		t.Error("un nombre de host repetido debería fallar: ¿a cuál de los dos se refiere?")
	}
}
