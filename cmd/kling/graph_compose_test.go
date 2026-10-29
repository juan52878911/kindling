package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func sinEntorno(string) (string, bool) { return "", false }

// Los tres compose de testdata/compose se traducen a su grafo y sus avisos
// esperados (UPDATE_GOLDEN=1 los reescribe).
func TestComposeGolden(t *testing.T) {
	for _, nombre := range []string{"webapp", "health", "unsupported"} {
		t.Run(nombre, func(t *testing.T) {
			dir := filepath.Join("testdata", "compose")
			datos, err := os.ReadFile(filepath.Join(dir, nombre+".compose.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			tr, err := traducirCompose(datos, nombre, false, sinEntorno)
			if err != nil {
				t.Fatal(err)
			}
			sal := map[string]string{
				nombre + ".graph.yaml":   tr.Grafo,
				nombre + ".warnings.txt": strings.Join(tr.Avisos, "\n") + "\n",
			}
			for f, got := range sal {
				p := filepath.Join(dir, f)
				if os.Getenv("UPDATE_GOLDEN") != "" {
					if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Errorf("%s differs:\n--- got\n%s--- want\n%s", f, got, want)
				}
			}
		})
	}
}

// El grafo traducido de webapp se lee con el mismo camino que `graph up`.
func TestComposeGrafoSeLee(t *testing.T) {
	datos, _ := os.ReadFile(filepath.Join("testdata", "compose", "webapp.graph.yaml"))
	req, err := leerGrafo("webapp.graph.yaml", datos, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Graph.Name != "shop" || len(req.Graph.Nodes) != 3 || len(req.Graph.Edges) != 8 {
		t.Errorf("graph = %+v", req.Graph)
	}
}

func TestComposeInterpolar(t *testing.T) {
	env := map[string]string{"A": "uno", "VACIA": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	var avisos []string
	got, err := interpolarCompose("${A} ${B:-dos} ${VACIA:-tres} ${VACIA-cuatro} $$A ${FALTA} $A-x ${VACIA:?no falla con ?} pre$A", lookup,
		func(s string) { avisos = append(avisos, s) })
	if err == nil || !strings.Contains(err.Error(), "no falla con ?") {
		t.Fatalf("${VACIA:?msg} vacía: err = %v", err)
	}
	if want := "uno dos tres  $A  uno-x  preuno"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(avisos) != 1 || !strings.Contains(avisos[0], "FALTA") {
		t.Errorf("avisos = %v", avisos)
	}
	// ${X?msg} solo falla sin definir; ${X:?msg}, también vacía.
	if _, err := interpolarCompose("${VACIA?m}", lookup, func(string) {}); err != nil {
		t.Errorf("${VACIA?m} definida y vacía: %v", err)
	}
	if _, err := interpolarCompose("${FALTA?hace falta FALTA}", lookup, func(string) {}); err == nil || !strings.Contains(err.Error(), "hace falta FALTA") {
		t.Errorf("${FALTA?m}: %v", err)
	}
	// Sin msg, el de Docker.
	if _, err := interpolarCompose("${FALTA:?}", lookup, func(string) {}); err == nil || !strings.Contains(err.Error(), "missing a value") {
		t.Errorf("${FALTA:?}: %v", err)
	}
}

// El .env junto al compose da las variables que la shell no tiene; la shell
// manda.
func TestComposeDotEnv(t *testing.T) {
	dir := t.TempDir()
	contenido := "# comentario\nexport TAG=16-alpine\nPASS=\"se cr\\\"eto\"\nPIN='0123'\nUSER=desde-fichero # nota\n\nSHELL_GANA=fichero\n"
	if err := os.WriteFile(filepath.Join(dir, ficheroVariables), []byte(contenido), 0o600); err != nil {
		t.Fatal(err)
	}
	shell := map[string]string{"SHELL_GANA": "shell"}
	lookup, err := conDotEnv(dir, func(k string) (string, bool) { v, ok := shell[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"TAG": "16-alpine", "PASS": `se cr"eto`, "PIN": "0123", "USER": "desde-fichero", "SHELL_GANA": "shell"} {
		if v, ok := lookup(k); !ok || v != want {
			t.Errorf("%s = %q (%v), want %q", k, v, ok, want)
		}
	}
	c := "services:\n  db:\n    image: postgres:${TAG}\n    environment:\n      PIN: ${PIN}\n      CODIGO: 0042\n"
	tr, err := traducirCompose([]byte(c), "dev", false, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if tr.vivos[0].imagen != "postgres:16-alpine" {
		t.Errorf("imagen = %q", tr.vivos[0].imagen)
	}
	env := map[string]string{}
	for _, kv := range tr.vivos[0].env {
		env[kv[0]] = kv[1]
	}
	// El cero de delante no se pierde (ni del .env ni escrito en el YAML).
	if env["PIN"] != "0123" || env["CODIGO"] != "0042" {
		t.Errorf("env = %v", env)
	}
	// Una línea sin = es un error (no se ignora en silencio).
	os.WriteFile(filepath.Join(dir, ficheroVariables), []byte("ESTO NO\n"), 0o600)
	if _, err := conDotEnv(dir, os.LookupEnv); err == nil {
		t.Error("aceptó una línea sin =")
	}
}

// healthcheck: disable: true desactiva aunque haya test (en el orden que sea).
func TestComposeHealthcheckDisable(t *testing.T) {
	c := "services:\n  db:\n    image: postgres:16\n    healthcheck:\n      test: [\"CMD\", \"pg_isready\"]\n      disable: true\n  app:\n    image: node:22\n    depends_on:\n      db: {condition: service_healthy}\n"
	tr, err := traducirCompose([]byte(c), "dev", false, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if s := tr.por["db"]; len(s.salud) != 0 {
		t.Errorf("salud = %v con disable: true", s.salud)
	}
}

// ports: "80" (sin host) se publica en uno libre al azar; 0.0.0.0 avisa de
// que es 127.0.0.1; /udp se publica (solo Linux) y no es un puerto del nodo.
func TestComposePuertosComoDocker(t *testing.T) {
	c := "services:\n  web:\n    image: nginx\n    ports:\n      - \"80\"\n      - \"0.0.0.0:8443:443\"\n      - \"127.0.0.1::9000\"\n      - \"5353:53/udp\"\n"
	tr, err := traducirCompose([]byte(c), "dev", false, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	want := []api.PortPublish{{Host: 0, Guest: 80}, {Host: 8443, Guest: 443}, {Host: 0, Guest: 9000}, {Host: 5353, Guest: 53, Proto: "udp"}}
	if got := tr.Publica["web"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Publica = %+v", got)
	}
	if got := tr.por["web"].puertos; !reflect.DeepEqual(got, []int{80, 443, 9000}) {
		t.Errorf("puertos del nodo = %v", got)
	}
	avisos := strings.Join(tr.Avisos, "\n")
	if !strings.Contains(avisos, "published on 127.0.0.1 only, not on 0.0.0.0") {
		t.Errorf("sin aviso de 0.0.0.0: %s", avisos)
	}
}

func TestComposeMemoria(t *testing.T) {
	for in, want := range map[string]int{"512m": 512, "1g": 1024, "256MB": 256, "1073741824": 1024, "2048k": 2} {
		if got := memoriaMiB(in, "x", func(string, ...any) {}); got != want {
			t.Errorf("%s = %d, want %d", in, got, want)
		}
	}
}

func TestComposeErrores(t *testing.T) {
	for nombre, src := range map[string]string{
		"sin servicios": "name: x\n",
		"solo build":    "services:\n  a:\n    build: .\n",
		"ancla":         "services:\n  a: &x\n    image: y\n",
		"lista":         "- a\n- b\n",
	} {
		if _, err := traducirCompose([]byte(src), "x", false, sinEntorno); err == nil {
			t.Errorf("%s: want an error", nombre)
		}
	}
}

// Un servicio con guion bajo se renombra y se avisa; -name manda sobre name:.
func TestComposeNombres(t *testing.T) {
	src := "name: viejo\nservices:\n  mi_web:\n    image: x/y\n    ports: [\"8000\"]\n"
	tr, err := traducirCompose([]byte(src), "nuevo", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Grafo, "name: nuevo\n") || !strings.Contains(tr.Grafo, "  mi-web: ") {
		t.Errorf("grafo:\n%s", tr.Grafo)
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), `renamed to node "mi-web"`) {
		t.Errorf("avisos = %v", tr.Avisos)
	}
}

// platform: linux/amd64 llega al import como -arch amd64 (en un daemon arm64,
// emulado con qemu-user); una plataforma que no es linux se avisa.
func TestComposePlatform(t *testing.T) {
	src := "services:\n  a:\n    image: x/a\n    platform: linux/amd64\n  b:\n    image: x/b\n    platform: windows/amd64\n"
	tr, err := traducirCompose([]byte(src), "p", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	antes := strings.Join(tr.Antes, "\n")
	if !strings.Contains(antes, "-name p-a -replace -arch amd64") {
		t.Errorf("antes:\n%s", antes)
	}
	if strings.Contains(antes, "-name p-b -replace -arch") {
		t.Errorf("b no debe llevar -arch:\n%s", antes)
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), "platform: windows/amd64") {
		t.Errorf("avisos = %v", tr.Avisos)
	}
}
