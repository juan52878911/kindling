package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/contrato"
	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// casoCodigo es una petición de la tabla de códigos y lo que contesta.
type casoCodigo struct {
	method, path, body string
}

// casosCodigos son las peticiones de la tabla de códigos de estado: al menos
// una por ruta, sobre el escenario de servidorCodigos (sin VMM: nada arranca
// de verdad). Solo entra lo que contesta igual en Linux y en macOS.
var casosCodigos = []casoCodigo{
	{"GET", "/info", ""},
	{"GET", "/machines", ""},
	{"POST", "/machines", `{"image":`},
	{"POST", "/machines", `{"image":"nope"}`},
	{"POST", "/machines", `{"image":"../x"}`},
	{"POST", "/machines", `{"image":"hay","on_ttl":"explode"}`},
	{"POST", "/machines", `{"image":"hay","disk_mib":1}`},
	{"POST", "/machines", `{"image":"hay","mem_mib":512,"mem_max_mib":256}`},
	{"POST", "/machines", `{"image":"hay","from":"nada"}`},
	{"POST", "/machines", `{"image":"hay","name":"parada"}`},
	{"POST", "/machines", `{"image":"hay"}`},
	{"GET", "/machines/nadie", ""},
	{"GET", "/machines/parada", ""},
	{"POST", "/machines/nadie/freeze", ""},
	{"POST", "/machines/parada/freeze", ""},
	{"POST", "/machines/nadie/thaw", ""},
	{"POST", "/machines/parada/thaw", ""},
	{"POST", "/machines/nadie/pause", ""},
	{"POST", "/machines/parada/pause", ""},
	{"POST", "/machines/dormida/pause", ""},
	{"POST", "/machines/nadie/squeeze", ""},
	{"POST", "/machines/dormida/squeeze", ""},
	{"POST", "/machines/nadie/resize", `{"mem_mib":512}`},
	{"POST", "/machines/nadie/resize", `{"mem_mib":`},
	{"POST", "/machines/nadie/mmds", `{}`},
	{"POST", "/machines/parada/mmds", `{}`},
	{"POST", "/machines/parada/credentials", `{"credentials":[]}`},
	{"DELETE", "/machines/parada/credentials/mal-env", ""},
	{"POST", "/machines/nadie/stop", ""},
	{"POST", "/machines/nadie/start", ""},
	{"POST", "/machines/dormida/start", ""},
	{"POST", "/machines/parada/start", ""},
	{"DELETE", "/machines/nadie", ""},
	{"PUT", "/machines/nadie/labels", `{}`},
	{"POST", "/machines/nadie/commit", `{"name":"x"}`},
	{"POST", "/images", `{`},
	{"GET", "/images", ""},
	{"DELETE", "/images/nada", ""},
	{"DELETE", "/images/usada", ""},
	{"GET", "/volumes", ""},
	{"POST", "/volumes", `{`},
	{"DELETE", "/volumes/nada", ""},
	{"POST", "/volumes/nada/populate", ""},
	{"POST", "/volumes/nada/clone", `{"to":"otro"}`},
	{"GET", "/volumes/nada/snapshots", ""},
	{"POST", "/volumes/nada/snapshots", `{}`},
	{"POST", "/volumes/nada/restore", `{}`},
	{"DELETE", "/volumes/nada/snapshots/s1", ""},
	{"GET", "/registries", ""},
	{"POST", "/registries", `{`},
	{"DELETE", "/registries/nada.example", ""},
	{"GET", "/images/nada/recipe", ""},
	{"GET", "/images/nada/files?path=/etc/hostname", ""},
	{"PUT", "/images/nada/files?path=/etc/hostname", "x"},
	{"GET", "/images/nada/blob", ""},
	{"PUT", "/images/nada/blob", "x"},
	{"GET", "/oci/blobs/malo", ""},
	{"PUT", "/oci/blobs/malo", "x"},
	{"GET", "/snapshots", ""},
	{"GET", "/snapshots/nada", ""},
	{"GET", "/snapshots/dorado", ""},
	{"PUT", "/snapshots/nada/annotations/k", `1`},
	{"PUT", "/snapshots/nada/credentials", `{"credentials":[]}`},
	{"GET", "/snapshots/nada/pool", ""},
	{"PUT", "/snapshots/nada/pool", `{"size":1}`},
	{"DELETE", "/snapshots/nada/annotations/k", ""},
	{"GET", "/store/ns", ""},
	{"GET", "/store/ns/nada", ""},
	{"PUT", "/store/ns/k", `1`},
	{"PUT", "/store/graph/k", `1`},
	{"DELETE", "/store/ns/nada", ""},
	{"DELETE", "/snapshots/nada", ""},
	{"GET", "/machines/nadie/logs", ""},
	{"GET", "/machines/nadie/credaudit", ""},
	{"GET", "/machines/nadie/egress", ""},
	{"POST", "/machines/nadie/guest", `{}`},
	{"POST", "/machines/nadie/renew", ""},
	{"GET", "/machines/nadie/ready", ""},
	{"POST", "/machines/nadie/hooks", `{}`},
	{"POST", "/machines/nadie/exec", `{"cmd":["true"]}`},
	{"POST", "/machines/nadie/exec", `{`},
	{"POST", "/machines/nadie/shell", ""},
	{"GET", "/machines/nadie/files?path=/x", ""},
	{"PUT", "/machines/nadie/files?path=/x", "x"},
	{"DELETE", "/machines/nadie/files?path=/x", ""},
	{"POST", "/shares/uploads", ""},
	{"POST", "/sandboxes", `{`},
	{"GET", "/sandboxes", ""},
	{"GET", "/sandboxes/nadie", ""},
	{"POST", "/sandboxes/nadie/renew", ""},
	{"POST", "/sandboxes/nadie/fork", `{}`},
	{"DELETE", "/sandboxes/nadie", ""},
	{"POST", "/graphs", `{`},
	{"GET", "/graphs", ""},
	{"GET", "/graphs/nada", ""},
	{"POST", "/graphs/nada/freeze", ""},
	{"POST", "/graphs/nada/thaw", ""},
	{"POST", "/graphs/nada/snapshot", `{}`},
	{"POST", "/graphs/nada/fork", `{}`},
	{"DELETE", "/graphs/nada", ""},
	{"GET", "/events", ""},
	{"GET", "/metrics", ""},
	{"GET", "/procstats", ""},
	{"POST", "/cow/store/grow", `{`},
	{"POST", "/machines/nadie/publish", `{}`},
	{"DELETE", "/machines/nadie/publish", ""},
	{"GET", "/publish", ""},
}

// servidorCodigos es el escenario de la tabla: una máquina parada, una
// congelada, una imagen suelta (hay), otra que usa un dorado (usada) y ese
// dorado.
func servidorCodigos(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	// En Linux, sin jailer ni usuario sin privilegios, Run se niega antes de
	// mirar la petición.
	t.Setenv("KLING_JAILER", "0")
	root := t.TempDir()
	b, _ := json.Marshal([]*api.Machine{
		{ID: "cccc000000000001", Name: "parada", State: api.StateStopped, Image: "hay"},
		{ID: "cccc000000000002", Name: "dormida", State: api.StateWarm, Image: "hay"},
	})
	escribir := func(p string, b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	escribir(filepath.Join(root, "state.json"), b)
	escribir(filepath.Join(root, "images", "hay.ext4"), []byte("x"))
	escribir(filepath.Join(root, "images", "usada.ext4"), []byte("x"))
	escribir(filepath.Join(root, "snapshots", "dorado", "meta.json"), []byte(`{"name":"dorado","image":"usada"}`))
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	s := &Server{mgr: mgr, root: root, bus: bus, store: &store{dir: filepath.Join(root, "store")}}
	return s, s.routes()
}

// Los códigos de estado son contrato (docs/compatibilidad.md): cambiaron en
// v0.18, v0.19 y v0.20 sin que nada lo detectara. La tabla
// testdata/contrato/codigos.txt dice, por ruta, qué contesta cada petición de
// casosCodigos (código HTTP y code del cuerpo). Cambiar un código es
// incompatible y exige subir api.APIVersion; cada ruta tiene que tener al
// menos una fila.
func TestContratoCodigos(t *testing.T) {
	s, h := servidorCodigos(t)
	mux := http.NewServeMux()
	for _, rt := range s.rutas() {
		mux.HandleFunc(rt.patron, func(http.ResponseWriter, *http.Request) {})
	}
	cubiertas := map[string]bool{}
	var lineas []string
	for _, c := range casosCodigos {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		_, patron := mux.Handler(req)
		if patron == "" {
			t.Errorf("%s %s no casa con ninguna ruta", c.method, c.path)
			continue
		}
		cubiertas[patron] = true
		if c.path == "/events" {
			// Un flujo: con el contexto ya cancelado contesta las cabeceras y
			// vuelve.
			ctx, cancel := context.WithCancel(req.Context())
			cancel()
			req = req.WithContext(ctx)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		code := "-"
		if rr.Code >= 400 {
			var e api.Error
			if json.Unmarshal(rr.Body.Bytes(), &e) == nil && e.Code != "" {
				code = string(e.Code)
			} else {
				code = "(no json)"
			}
		}
		cuerpo := ""
		if c.body != "" {
			cuerpo = " " + c.body
		}
		lineas = append(lineas, fmt.Sprintf("%s  ←  %s %s%s  →  %d %s", patron, c.method, c.path, cuerpo, rr.Code, code))
	}
	for _, rt := range s.rutas() {
		if !cubiertas[rt.patron] {
			t.Errorf("la ruta %s no tiene ninguna fila en casosCodigos", rt.patron)
		}
	}
	cab := []string{
		"Códigos de estado por ruta: <ruta>  ←  <petición>  →  <HTTP> <code del cuerpo>.",
		"Escenario: servidorCodigos (máquinas parada y dormida, imágenes hay y usada,",
		"dorado de usada), sin VMM. Lo genera TestContratoCodigos",
		"(internal/daemon/contrato_codigos_test.go):",
		"  go test ./internal/daemon -run TestContratoCodigos -update",
		"Cambiar un código exige subir api.APIVersion (internal/contrato).",
	}
	if err := contrato.Comparar("testdata/contrato/codigos.txt", api.APIVersion, cab, lineas, *actualizar); err != nil {
		t.Fatal(err)
	}
}
