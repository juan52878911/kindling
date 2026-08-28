package assets

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// La regla que protege el trabajo de su dueño: Materialize SOLO rellena huecos.
//
// Una imagen base construida a mano —con node, con python, con lo que necesite
// su servidor MCP— vale infinitamente más que la de fábrica, y `kling up` se
// ejecuta más de una vez. Pisarla no daría error: daría una microVM sin lo que
// hacía falta dentro, y el fallo aparecería mucho después.
// deFabrica es un origen con TODOS los artefactos que Materialize espera.
func deFabrica(contenido string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, n := range Artifacts {
		m[n] = &fstest.MapFile{Data: []byte(contenido)}
	}
	return m
}

func TestMaterializeNoPisaLoQueYaEsta(t *testing.T) {
	dir := t.TempDir()
	// Un artefacto que "ya construyó su dueño".
	mio := filepath.Join(dir, Artifacts[0])
	if err := os.WriteFile(mio, []byte("LA IMAGEN DE SU DUEÑO"), 0o644); err != nil {
		t.Fatal(err)
	}

	escritos, err := copiarDesde(deFabrica("la de fábrica"), dir)
	if err != nil {
		t.Fatalf("copiarDesde: %v", err)
	}

	b, _ := os.ReadFile(mio)
	if string(b) != "LA IMAGEN DE SU DUEÑO" {
		t.Errorf("se pisó lo que ya estaba: %q", b)
	}
	for _, e := range escritos {
		if e == mio {
			t.Error("Materialize dice haber escrito algo que no tocó")
		}
	}
	// El otro artefacto, que no estaba, sí se rellena.
	if len(escritos) != len(Artifacts)-1 {
		t.Errorf("escribió %d, esperaba %d (todos menos el que ya estaba)", len(escritos), len(Artifacts)-1)
	}
}

// Y sí rellena lo que falta, de forma atómica: un Ctrl-C a mitad de copiar 20 MB
// no puede dejar un fichero con el nombre bueno y medio contenido — eso no da un
// error, da un kernel que no arranca.
func TestMaterializeRellenaHuecosSinDejarTemporales(t *testing.T) {
	dir := t.TempDir()
	escritos, err := copiarDesde(deFabrica("contenido de fábrica"), dir)
	if err != nil {
		t.Fatalf("copiarDesde: %v", err)
	}
	if len(escritos) != len(Artifacts) {
		t.Fatalf("escribió %d artefactos, esperaba %d: %v", len(escritos), len(Artifacts), escritos)
	}
	b, err := os.ReadFile(filepath.Join(dir, Artifacts[0]))
	if err != nil || string(b) != "contenido de fábrica" {
		t.Errorf("contenido = %q, err = %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, Artifacts[0]+".tmp")); !os.IsNotExist(err) {
		t.Error("quedó un .tmp junto al artefacto")
	}
}

// Un binario compilado sin la etiqueta `embed_assets` no lleva nada dentro, y
// eso NO es un fallo: es el CLI que se instala en un Mac y no arranca microVMs
// jamás. Devolver un error ahí haría fallar `kling up` en la máquina de trabajo.
func TestSinArtefactosDentroNoEsUnError(t *testing.T) {
	if Embedded() {
		t.Skip("este binario se compiló con embed_assets")
	}
	escritos, err := Materialize(t.TempDir())
	if err != nil {
		t.Errorf("Materialize sin artefactos devolvió error: %v", err)
	}
	if len(escritos) != 0 {
		t.Errorf("dice haber escrito %v", escritos)
	}
}
