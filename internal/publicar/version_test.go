package publicar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// El publish.json de v0.20 (un array sin versión) se lee, y la primera
// escritura lo migra al formato 1 dejando antes publish.json.v0.bak, que es
// lo que -rollback devuelve a su sitio.
func TestPublishJSONMigraDesdeV0(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "publish.json")
	v0 := `[{"machine":"a","ports":[{"host":8080,"guest":80}]}]`
	if err := os.WriteFile(ruta, []byte(v0), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(&maquinasFalsas{ms: map[string]*api.Machine{}}, ruta, t.Logf)
	l := p.leer()
	if len(l) != 1 || l[0].Machine != "a" || len(l[0].Ports) != 1 || l[0].Ports[0].Host != 8080 {
		t.Fatalf("leer v0 = %+v", l)
	}
	p.guardar()
	if b, err := os.ReadFile(esquema.RutaRespaldo(ruta, 0)); err != nil || string(b) != v0 {
		t.Fatalf("copia v0 = %q, %v", b, err)
	}
	b, _ := os.ReadFile(ruta)
	if v, err := esquema.Version(b); err != nil || v != versionPublicar {
		t.Fatalf("versión escrita = %d, %v (%s)", v, err, b)
	}
	var f ficheroPublicar
	if err := json.Unmarshal(b, &f); err != nil || f.Publications == nil {
		t.Fatalf("formato 1 = %s, %v", b, err)
	}
	// Y el formato 1 se vuelve a leer.
	if err := os.WriteFile(ruta, []byte(`{"schema":1,"publications":[{"machine":"b","ports":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := p.leer(); len(l) != 1 || l[0].Machine != "b" {
		t.Fatalf("leer v1 = %+v", l)
	}
}

// Un publish.json de un kling más nuevo no se lee ni se pisa.
func TestPublishJSONDelFuturoNoSePisa(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "publish.json")
	futuro := `{"schema":99,"publications":[],"nuevo":true}`
	if err := os.WriteFile(ruta, []byte(futuro), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(&maquinasFalsas{ms: map[string]*api.Machine{}}, ruta, t.Logf)
	if l := p.leer(); l != nil {
		t.Fatalf("leyó uno del futuro: %+v", l)
	}
	p.guardar()
	if b, _ := os.ReadFile(ruta); string(b) != futuro {
		t.Fatalf("lo pisó: %s", b)
	}
	// Aunque no se haya leído antes: PrepararEscritura lo mira al escribir.
	p2 := New(&maquinasFalsas{ms: map[string]*api.Machine{}}, ruta, t.Logf)
	p2.guardar()
	if b, _ := os.ReadFile(ruta); string(b) != futuro {
		t.Fatalf("lo pisó sin leerlo: %s", b)
	}
}

// La fijación de v0.20 (internal/machine/testdata/esquema/v0.20, ver
// fijaciones_test.go allí) se lee entera y su copia queda tal cual.
func TestPublishJSONFijacionV020(t *testing.T) {
	original, err := os.ReadFile(filepath.Join("..", "machine", "testdata", "esquema", "v0.20", "publish.json"))
	if err != nil {
		t.Fatal(err)
	}
	ruta := filepath.Join(t.TempDir(), "publish.json")
	if err := os.WriteFile(ruta, original, 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(&maquinasFalsas{ms: map[string]*api.Machine{}}, ruta, t.Logf)
	l := p.leer()
	var quiero []api.Publication
	if err := json.Unmarshal(original, &quiero); err != nil {
		t.Fatal(err)
	}
	if len(l) != 1 || l[0].Machine != quiero[0].Machine || l[0].Name != quiero[0].Name || len(l[0].Ports) != 1 || l[0].Ports[0] != quiero[0].Ports[0] {
		t.Fatalf("leer = %+v; quería %+v", l, quiero)
	}
	p.guardar()
	if b, err := os.ReadFile(esquema.RutaRespaldo(ruta, 0)); err != nil || string(b) != string(original) {
		t.Fatalf("copia v0: %v", err)
	}
}
