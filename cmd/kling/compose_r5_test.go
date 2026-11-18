package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// F4: dos nombres de proyecto largos que empiezan igual ya no dan el mismo
// proyecto (ni los mismos volúmenes) al cortarse, y el corte se avisa.
func TestNombreLargoNoColisiona(t *testing.T) {
	a, b := nombreDNS("customer-portal-backend-staging"), nombreDNS("customer-portal-backend-production")
	if a == b || len(a) > maxNombreDNS || len(b) > maxNombreDNS {
		t.Fatalf("%q y %q", a, b)
	}
	if err := api.ValidateGraphName(a); err != nil {
		t.Fatal(err)
	}
	if nombreDNS("Web_App") != "web-app" || nombreDNS(strings.Repeat("x", 24)) != strings.Repeat("x", 24) {
		t.Fatal("un nombre corto cambió")
	}
	if nombreDNS("customer-portal-backend-staging") != a {
		t.Fatal("no es estable")
	}
	src := "services:\n  web:\n    image: nginx\n    volumes: [\"data:/d\"]\nvolumes:\n  data:\n"
	tr, err := traducirCompose([]byte(src), "customer-portal-backend-staging", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proyect != a || !slices.Equal(volumenesDe(tr), []string{a + "-data"}) {
		t.Fatalf("proyecto %q volúmenes %q", tr.Proyect, volumenesDe(tr))
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), "over 24 characters") {
		t.Fatalf("sin aviso del corte: %q", tr.Avisos)
	}
}

// F4: down -v solo borra los volúmenes con la etiqueta de su proyecto (y de
// su fichero). El de otro proyecto parado —sin máquinas que lo delaten— se
// llama igual y no se borra; ni nada más de esa orden.
func TestDownVSoloBorraLosSuyos(t *testing.T) {
	src := "services:\n  b-web:\n    image: nginx\n    volumes: [\"b-data:/d\", \"b-log:/l\"]\nvolumes:\n  b-data:\n  b-log:\n"
	tr, err := traducirCompose([]byte(src), "a", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	mio := map[string]string{labelComposeProject: "a", labelComposeFile: "/p/compose.yaml"}
	vol := func(n string, l map[string]string) *api.Volume { return &api.Volume{Name: n, Labels: l} }

	borrar, avisos, err := volumenesABorrar(tr, "/p/compose.yaml", []*api.Volume{vol("a-b-data", mio), vol("a-b-log", nil), vol("otro", nil)})
	if err != nil || !slices.Equal(borrar, []string{"a-b-data"}) || len(avisos) != 1 || !strings.Contains(avisos[0], "a-b-log") {
		t.Fatalf("borrar %q avisos %q err %v", borrar, avisos, err)
	}
	// El volumen data del proyecto a-b (parado) es a-b-data: ni ese ni el suyo.
	ajeno := map[string]string{labelComposeProject: "a-b"}
	borrar, _, err = volumenesABorrar(tr, "/p/compose.yaml", []*api.Volume{vol("a-b-data", ajeno), vol("a-b-log", mio)})
	if err == nil || borrar != nil || !strings.Contains(err.Error(), "project a-b") {
		t.Fatalf("borrar %q err %v", borrar, err)
	}
	// El mismo proyecto de otro compose.yaml, tampoco.
	_, _, err = volumenesABorrar(tr, "/otro/compose.yaml", []*api.Volume{vol("a-b-data", mio)})
	if err == nil || !strings.Contains(err.Error(), "another compose file") {
		t.Fatalf("otro fichero: %v", err)
	}
}

// F4: up crea los volúmenes con las etiquetas del proyecto y no adopta uno
// que lleva las de otro.
func TestUpEtiquetaSusVolumenes(t *testing.T) {
	src := "services:\n  web:\n    image: nginx\n    volumes: [\"data:/d\", \"log:/l\"]\nvolumes:\n  data:\n  log:\n"
	tr, err := traducirCompose([]byte(src), "p", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	tr.etiquetas = map[string]string{labelComposeProject: "p", labelComposeFile: "/p/compose.yaml"}
	var mu sync.Mutex
	hay := []*api.Volume{{Name: "p-log"}}
	var creados []api.CreateVolumeRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(hay)
	})
	mux.HandleFunc("POST /volumes", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateVolumeRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		creados = append(creados, req)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.Volume{Name: req.Name, Labels: req.Labels})
	})
	c := fakeDaemon(t, mux)
	if err := crearVolumenes(context.Background(), c, tr, volumenesDe(tr)); err != nil {
		t.Fatal(err)
	}
	if len(creados) != 1 || creados[0].Name != "p-data" || creados[0].Labels[labelComposeProject] != "p" || creados[0].Labels[labelComposeFile] != "/p/compose.yaml" {
		t.Fatalf("creados %+v", creados)
	}
	mu.Lock()
	hay = []*api.Volume{{Name: "p-data", Labels: map[string]string{labelComposeProject: "q"}}}
	creados = nil
	mu.Unlock()
	if err := crearVolumenes(context.Background(), c, tr, volumenesDe(tr)); err == nil || !strings.Contains(err.Error(), "volume p-data") || creados != nil {
		t.Fatalf("err %v creados %+v", err, creados)
	}
}

// F4: sin grafo, un fallo de List no para la orden, pero queda dicho para
// que down -v no borre a ciegas.
func TestVerProyectoGuardaElFalloDeLaLista(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"roto"}`, http.StatusInternalServerError)
	})
	c := fakeDaemon(t, mux)
	v, err := verProyecto(context.Background(), c, "p", false)
	if err != nil || v.g != nil || v.errLista == nil {
		t.Fatalf("v %+v err %v", v, err)
	}
}

// S14: cerrar la terminal (SIGHUP) deshace el up como Ctrl-C, en vez de
// matarlo sin borrar sus temporales.
func TestUpAtiendeSIGHUP(t *testing.T) {
	if !slices.Contains(senalesUp, os.Signal(syscall.SIGHUP)) {
		t.Fatalf("senalesUp = %v", senalesUp)
	}
}

// F4, compatibilidad: un proyecto de más de 24 caracteres que levantó un
// kling anterior (nombre cortado sin hash) se sigue encontrando en down y en
// el up que lo reemplaza, si sus máquinas llevan la etiqueta de ese
// proyecto; el de otro proyecto con ese nombre no. Sus volúmenes viejos no
// se dejan atrás en silencio.
func TestProyectoLargoDeUnKlingAnterior(t *testing.T) {
	src := "services:\n  web:\n    image: nginx\n    volumes: [\"data:/d\"]\nvolumes:\n  data:\n"
	tr, err := traducirCompose([]byte(src), "customer-portal-backend-staging", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	viejo := "customer-portal-backend"
	if tr.proyectoViejo != viejo || nombreDNSViejo("customer-portal-backend-staging") != viejo {
		t.Fatalf("viejo = %q", tr.proyectoViejo)
	}
	var mu sync.Mutex
	var borrados []string
	etiqueta := viejo
	vieja := []*api.Volume{{Name: viejo + "-data"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != viejo {
			http.Error(w, `{"error":"no"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(api.Graph{ID: "g1", Name: viejo})
	})
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode([]*api.Machine{{ID: "m1", Name: viejo + "-web",
			Labels: map[string]string{api.LabelGraph: "g1", labelComposeProject: etiqueta}}})
	})
	mux.HandleFunc("DELETE /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		borrados = append(borrados, r.PathValue("ref"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(vieja) })
	c := fakeDaemon(t, mux)
	ctx := context.Background()

	v, err := verProyecto(ctx, c, tr.Proyect, false)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := v.adoptarViejo(ctx, c, tr.proyectoViejo, ""); !ok || err != nil || v.nombre != viejo || v.g == nil {
		t.Fatalf("adoptado %v %v: %+v", ok, err, v)
	}
	// El up que lo reemplaza quita el grafo viejo.
	if err := sustituirProyecto(ctx, c, tr, v, &preparacionCompose{t: tr, c: c}, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !slices.Equal(borrados, []string{viejo}) {
		t.Fatalf("borrados %q", borrados)
	}
	// Con la etiqueta de otro proyecto, no es suyo.
	etiqueta = "otro"
	mu.Unlock()
	v, _ = verProyecto(ctx, c, tr.Proyect, false)
	if ok, _ := v.adoptarViejo(ctx, c, tr.proyectoViejo, ""); ok || v.g != nil {
		t.Fatalf("adoptó el de otro: %+v", v)
	}

	// Su volumen viejo: up no crea uno nuevo vacío en su lugar, y down -v lo
	// avisa.
	if err := crearVolumenes(ctx, c, tr, volumenesDe(tr)); err == nil || !strings.Contains(err.Error(), viejo+"-data") {
		t.Fatalf("up: %v", err)
	}
	if _, avisos, _ := volumenesABorrar(tr, "", vieja); len(avisos) != 1 || !strings.Contains(avisos[0], viejo+"-data") {
		t.Fatalf("down -v: %q", avisos)
	}
}
