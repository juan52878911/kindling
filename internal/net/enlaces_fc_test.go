//go:build !darwin

package net

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func resolverNulo(context.Context) (string, string, error) { return "", "", nil }

// Lo que el manager pida se comprueba otra vez antes de tocar reglas: solo
// nombres de grafo, puertos válidos, uno por puerto, nada en 53/80/443.
func TestValidarLinks(t *testing.T) {
	bien := []LinkSpec{{Host: "api.graph", Port: 8080, Resolve: resolverNulo}, {Host: "db.graph", Port: 5432, Resolve: resolverNulo}}
	if err := validarLinks(bien); err != nil {
		t.Fatalf("links válidos rechazados: %v", err)
	}
	casos := map[string][]LinkSpec{
		"no es de grafo":  {{Host: "api.example.com", Port: 8080, Resolve: resolverNulo}},
		"puerto 53":       {{Host: "api.graph", Port: 53, Resolve: resolverNulo}},
		"puerto 443":      {{Host: "api.graph", Port: 443, Resolve: resolverNulo}},
		"puerto 0":        {{Host: "api.graph", Port: 0, Resolve: resolverNulo}},
		"repetido":        {{Host: "api.graph", Port: 8080, Resolve: resolverNulo}, {Host: "db.graph", Port: 8080, Resolve: resolverNulo}},
		"sin resolvedor":  {{Host: "api.graph", Port: 8080}},
		"host con barras": {{Host: "a/b.graph", Port: 8080, Resolve: resolverNulo}},
	}
	for nombre, ls := range casos {
		if err := validarLinks(ls); err == nil {
			t.Errorf("%s: aceptado", nombre)
		}
	}
	muchos := make([]LinkSpec, linkMaxPorts+1)
	for i := range muchos {
		muchos[i] = LinkSpec{Host: "a.graph", Port: 1000 + i, Resolve: resolverNulo}
	}
	if err := validarLinks(muchos); err == nil {
		t.Error("más enlaces que puertos de enlace: aceptado")
	}
}

// La comprobación de una regla es la misma regla con -C y sin posición.
func TestComprobacionDe(t *testing.T) {
	casos := map[string]string{
		"-t nat -I PREROUTING 1 -i tap0 -p tcp --dport 8080 -j DNAT": "-t nat -C PREROUTING -i tap0 -p tcp --dport 8080 -j DNAT",
		"-I FORWARD 1 -i tap0 -j ACCEPT":                             "-C FORWARD -i tap0 -j ACCEPT",
		"-t nat -A POSTROUTING -o vg-x -j MASQUERADE":                "-t nat -C POSTROUTING -o vg-x -j MASQUERADE",
	}
	for in, want := range casos {
		if got := strings.Join(comprobacionDe(strings.Fields(in)), " "); got != want {
			t.Errorf("%s\n  = %s\n want %s", in, got, want)
		}
	}
}

// Todas las reglas de un grafo se quedan en el veth del nodo: destino
// n.HostIP (el lado host de SU veth) y, si hay DNAT, a n.HostIP. Ninguna
// abre el FORWARD a otra cosa: el FORWARD entre netns sigue cerrado.
func TestReglasDeGrafoNoSalenDelVeth(t *testing.T) {
	n := Plan(5, "abcdef0123456789")
	for _, e := range []Egress{EgressNone, EgressInternet} {
		reglas := append(n.reglasGrafo(e, true), n.reglasEnlace(8080, 0)...)
		for _, r := range reglas {
			l := strings.Join(r, " ")
			if strings.Contains(l, "DNAT") && !strings.Contains(l, "--to-destination "+n.HostIP+":") {
				t.Errorf("%s: DNAT fuera del veth: %s", e, l)
			}
			if strings.Contains(l, "FORWARD") && (!strings.Contains(l, "-d "+n.HostIP) || !strings.Contains(l, "ACCEPT")) {
				t.Errorf("%s: FORWARD que no va a n.HostIP: %s", e, l)
			}
			if strings.Contains(l, "MASQUERADE") && !strings.Contains(l, "-d "+n.HostIP) {
				t.Errorf("%s: MASQUERADE general: %s", e, l)
			}
		}
	}
	enl := strings.Join(n.reglasEnlace(8080, 3)[0], " ")
	if !strings.Contains(enl, "-I PREROUTING 1") || !strings.Contains(enl, "--dport 8080") || !strings.HasSuffix(enl, n.HostIP+":5403") {
		t.Errorf("DNAT de enlace inesperado: %s", enl)
	}
}

// Un nodo con solo aristas link no recibe los DNAT ni los ACCEPT de los
// proxies de credenciales (el de Postgres se lleva todo el TCP a n.HostIP);
// uno con una arista credential, sí. El DNS va siempre.
func TestReglasGrafoCredencialesSoloConArista(t *testing.T) {
	n := Plan(5, "abcdef0123456789")
	proxies := func(reglas [][]string) (pg, http int) {
		for _, r := range reglas {
			l := strings.Join(r, " ")
			if strings.Contains(l, fmt.Sprintf("%s:%d", n.HostIP, pgPort)) || strings.Contains(l, "--dport "+pgPortStr) {
				pg++
			}
			if strings.Contains(l, fmt.Sprintf("%s:%d", n.HostIP, credPort)) || strings.Contains(l, "--dport "+credPortStr) {
				http++
			}
		}
		return
	}
	for _, e := range []Egress{EgressNone, EgressInternet} {
		sin := n.reglasGrafo(e, false)
		if pg, http := proxies(sin); pg != 0 || http != 0 {
			t.Errorf("%s sin arista credential: %d reglas de Postgres y %d del proxy HTTP", e, pg, http)
		}
		dns := 0
		for _, r := range sin {
			if strings.Contains(strings.Join(r, " "), "--dport 53 ") {
				dns++
			}
		}
		if dns != 2 {
			t.Errorf("%s: el DNS al resolver tiene que seguir (%d reglas)", e, dns)
		}
		if pg, http := proxies(n.reglasGrafo(e, true)); pg != 2 || http != 3 {
			t.Errorf("%s con arista credential: %d reglas de Postgres (quería DNAT y ACCEPT) y %d del HTTP", e, pg, http)
		}
	}
}
