package frontal

// Test de contrato: ext/sandbox/api/openapi.yaml y las rutas /v1 reales del
// frontal tienen que decir lo mismo. Sin dependencias de YAML (el módulo no
// debe ganar ninguna): la spec se lee con un escáner de líneas que solo
// entiende la disposición regular que tiene el fichero (paths a 2 espacios,
// métodos a 4; schemas a 4, properties a 6, propiedades a 8).

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/inquilinos"
	"github.com/juan52878911/kindling/pkg/api"
)

const rutaSpec = "../../api/openapi.yaml"

var reMetodoSpec = regexp.MustCompile(`^    (get|post|put|delete|patch|head|options):\s*$`)

type specLeida struct {
	rutas   map[string]bool     // "GET /v1/sandboxes/{id}"
	schemas map[string][]string // schema -> propiedades
}

func leerSpec(t *testing.T) specLeida {
	t.Helper()
	f, err := os.Open(rutaSpec)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sp := specLeida{rutas: map[string]bool{}, schemas: map[string][]string{}}
	seccion, enSchemas, enProps := "", false, false
	ruta, schema := "", ""
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		l := sc.Text()
		if strings.Contains(l, "\t") {
			t.Fatalf("openapi.yaml:%d: tabulador (YAML no los admite)", n)
		}
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		sangria := len(l) - len(strings.TrimLeft(l, " "))
		if sangria == 0 {
			seccion = strings.TrimSuffix(strings.TrimSpace(l), ":")
			enSchemas, enProps = false, false
			continue
		}
		switch seccion {
		case "paths":
			if sangria == 2 && strings.HasPrefix(l, "  /") && strings.HasSuffix(l, ":") {
				ruta = strings.TrimSuffix(strings.TrimSpace(l), ":")
			} else if m := reMetodoSpec.FindStringSubmatch(l); m != nil {
				if ruta == "" {
					t.Fatalf("openapi.yaml:%d: método sin ruta", n)
				}
				sp.rutas[strings.ToUpper(m[1])+" "+ruta] = true
			}
		case "components":
			if sangria == 2 {
				enSchemas = strings.TrimSpace(l) == "schemas:"
				continue
			}
			if !enSchemas {
				continue
			}
			switch {
			case sangria == 4:
				schema = strings.TrimSuffix(strings.TrimSpace(l), ":")
				sp.schemas[schema] = nil
				enProps = false
			case sangria == 6:
				enProps = strings.TrimSpace(l) == "properties:"
			case sangria == 8 && enProps:
				sp.schemas[schema] = append(sp.schemas[schema], strings.TrimSuffix(strings.TrimSpace(l), ":"))
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(sp.rutas) == 0 || len(sp.schemas) == 0 {
		t.Fatal("la spec no tiene paths o schemas: ¿cambió la disposición del fichero?")
	}
	return sp
}

func leerFuente(t *testing.T, nombre string) string {
	t.Helper()
	b, err := os.ReadFile(nombre)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// concreta sustituye {id} por un id con la forma real host/máquina.
func concreta(ruta string) string {
	if strings.HasPrefix(ruta, "/v1/tenants") { // aquí el id no lleva barra
		return strings.NewReplacer("{id}", "t1", "{kid}", "k1").Replace(ruta)
	}
	return strings.NewReplacer("{id}", "h1/m1", "{pid}", "1", "{sid}", "s1").Replace(ruta)
}

// TestContratoRutas compara la spec con lo que el frontal sirve de verdad.
// Sondea cada método sobre cada ruta candidata y considera "ruta existente"
// la que no contesta 405 ni el 404 de texto del ServeMux/NotFound: un 404 JSON
// ("no such sandbox") es de la ruta, no de su ausencia. Las candidatas salen de
// la spec Y de las acciones y patrones que aparecen en el código fuente, así
// que una ruta añadida al despacho sin documentar se ve igual.
func TestContratoRutas(t *testing.T) {
	sp := leerSpec(t)
	srv, _ := montar(t, map[string]*falso{})

	candidatas := map[string]bool{}
	for r := range sp.rutas {
		candidatas[strings.SplitN(r, " ", 2)[1]] = true
	}
	// Acciones por sandbox que el despacho conoce (sandboxes.go).
	for _, m := range regexp.MustCompile(`accion == "(\w+)"`).FindAllStringSubmatch(leerFuente(t, "sandboxes.go"), -1) {
		candidatas["/v1/sandboxes/{id}/"+m[1]] = true
	}
	// Patrones registrados en el mux (frontal.go).
	reMux := regexp.MustCompile(`m\.Handle(?:Func)?\("(?:([A-Z]+) )?(/v1/[^"]*)"`)
	for _, m := range reMux.FindAllStringSubmatch(leerFuente(t, "frontal.go")+leerFuente(t, "tenants.go"), -1) {
		p := m[2]
		if strings.HasSuffix(p, "/") { // subárbol despachado a mano: {id}
			candidatas[p+"{id}"] = true
			continue
		}
		candidatas[p] = true
		if m[1] != "" && !sp.rutas[m[1]+" "+p] {
			t.Errorf("el mux registra %s %s y la spec no lo documenta", m[1], p)
		}
	}

	servidas := map[string]bool{}
	// Sin seguir redirecciones: el mux manda "/v1/sandboxes" con un método sin
	// ruta propia a "/v1/sandboxes/", y eso no es una ruta servida.
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for p := range candidatas {
		for _, metodo := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
			req, _ := http.NewRequest(metodo, srv.URL+concreta(p), strings.NewReader("{"))
			req.Header.Set("Authorization", "Bearer "+tokenAlice)
			resp, err := cli.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", metodo, p, err)
			}
			cuerpo, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode/100 == 3 {
				continue // 405 (del mux o del despacho) o redirección: no hay ruta
			}
			if resp.StatusCode == http.StatusNotFound && strings.Contains(string(cuerpo), "page not found") {
				continue
			}
			servidas[metodo+" "+p] = true
		}
	}

	for r := range servidas {
		if !sp.rutas[r] {
			t.Errorf("el frontal sirve %s y la spec no lo documenta", r)
		}
	}
	for r := range sp.rutas {
		if !servidas[r] {
			t.Errorf("la spec documenta %s y el frontal no lo sirve", r)
		}
	}
}

// TestContratoEsquemas: cada propiedad de los esquemas principales de la spec
// es un json tag de su struct, y a la inversa (un campo nuevo sin documentar
// también falla).
func TestContratoEsquemas(t *testing.T) {
	sp := leerSpec(t)
	structs := map[string]any{
		"Sandbox": Sandbox{}, "CrearPeticion": CrearPeticion{}, "VolumenPeticion": VolumenPeticion{},
		"Volumen": Volumen{}, "CrearVolumenPeticion": CrearVolumenPeticion{}, "LoteRespuesta": LoteRespuesta{}, "LoteError": LoteError{}, "RenovarPeticion": RenovarPeticion{},
		"Grafo": Grafo{}, "NodoGrafo": NodoGrafo{}, "CrearGrafoPeticion": CrearGrafoPeticion{},
		"TemplateInfo": TemplateInfo{}, "TemplateHost": TemplateHost{}, "HostEstado": hosts.Estado{},
		"Error": api.Error{}, "ExecRequest": api.ExecRequest{}, "ExecEvent": api.ExecEvent{},
		"Tenant": vistaTenant{}, "ClaveTenant": vistaClave{}, "TopesTenant": inquilinos.Topes{},
		"CrearTenantPeticion": pedirTenant{}, "CambiarTenantPeticion": cambiarTenant{},
		"CrearClavePeticion": pedirClave{}, "RotarClavePeticion": pedirRotar{}, "ClaveCreada": claveCreada{},
		"ExecResult": api.ExecResult{}, "ProcessStartRequest": api.ProcessStartRequest{}, "ProcessInfo": api.ProcessInfo{},
		"ProcessEvent": api.ProcessEvent{}, "ProcessSignalRequest": api.ProcessSignalRequest{}, "SessionRequest": api.SessionRequest{}, "Session": api.Session{}, "ShellRequest": api.ShellRequest{}, "FileStat": api.FileStat{},
	}
	for nombre, v := range structs {
		declaradas, ok := sp.schemas[nombre]
		if !ok {
			t.Errorf("la spec no declara el esquema %s", nombre)
			continue
		}
		var tags []string
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if !f.IsExported() || tag == "-" {
				continue
			}
			if tag == "" {
				tag = f.Name
			}
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		sort.Strings(declaradas)
		if !reflect.DeepEqual(tags, declaradas) {
			t.Errorf("esquema %s: la spec dice %v y el struct tiene %v", nombre, declaradas, tags)
		}
	}
}

// TestContratoDeclaraLoPrometido: lo que el encargo exige que diga la spec.
func TestContratoDeclaraLoPrometido(t *testing.T) {
	b := leerFuente(t, rutaSpec)
	if !strings.HasPrefix(b, "openapi: 3.1") {
		t.Error("la spec debe ser OpenAPI 3.1")
	}
	for _, s := range []string{
		"scheme: bearer", "application/x-ndjson", "name: wait", "Upgrade: kling-shell/1",
		"name: path", "name: stat", "name: mode", "name: mkdir", "67108864", "WWW-Authenticate",
	} {
		if !strings.Contains(b, s) {
			t.Errorf("la spec no contiene %q", s)
		}
	}
	if api.FileMaxUpload != 67108864 {
		t.Errorf("FileMaxUpload cambió a %d: actualiza la spec", api.FileMaxUpload)
	}
}
