package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crea un volumen de mentira con el reparto que se le pida.
func volumenCon(t *testing.T, dirs ...string) string {
	t.Helper()
	raiz := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(raiz, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return raiz
}

func valor(env []string, key string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Un volumen con node_modules entra en NODE_PATH; uno sin él, no.
//
// La comprobación de existencia no es cosmética: una entrada inventada hace que
// node recorra rutas inexistentes en cada require, y una en PYTHONPATH aparece
// en el rastro de pila de cualquier import fallido, mandando a quien depura al
// sitio equivocado.
func TestSoloEntranLosVolumenesQueTraenPaquetes(t *testing.T) {
	conNode := volumenCon(t, "node_modules/lodash")
	vacio := volumenCon(t, "cosas/mias")

	env := libraryEnv(nil, []volumeSpec{
		{mount: conNode, readOnly: true},
		{mount: vacio},
	})

	np, ok := valor(env, "NODE_PATH")
	if !ok {
		t.Fatal("no puso NODE_PATH habiendo node_modules")
	}
	if !strings.Contains(np, filepath.Join(conNode, "node_modules")) {
		t.Errorf("NODE_PATH no apunta a la biblioteca: %q", np)
	}
	if strings.Contains(np, vacio) {
		t.Errorf("metió un volumen sin paquetes: %q", np)
	}
	if _, ok := valor(env, "PYTHONPATH"); ok {
		t.Error("puso PYTHONPATH sin que hubiera paquetes de python")
	}
}

// Sin volúmenes con paquetes no se exporta NADA.
//
// Exportar la variable vacía no es lo mismo que no exportarla: para python, un
// PYTHONPATH="" es una entrada de ruta vacía, y eso significa "el directorio
// actual" — se colaría el cwd en el camino de búsqueda de imports.
func TestSinPaquetesNoSeExportaNada(t *testing.T) {
	for _, vols := range [][]volumeSpec{nil, {{mount: volumenCon(t, "datos")}}} {
		env := libraryEnv([]string{"PATH=/bin"}, vols)
		if v, ok := valor(env, "NODE_PATH"); ok {
			t.Errorf("exportó NODE_PATH=%q sin motivo", v)
		}
		if v, ok := valor(env, "PYTHONPATH"); ok {
			t.Errorf("exportó PYTHONPATH=%q sin motivo", v)
		}
		if len(env) != 1 {
			t.Errorf("tocó el entorno sin necesidad: %v", env)
		}
	}
}

// Lo que la imagen ya traiga MANDA sobre la biblioteca compartida.
//
// Al revés, actualizar el volumen cambiaría en silencio la versión que usa un
// servicio que ya funcionaba, y el cambio no aparecería en ningún sitio.
func TestLoDeLaImagenVaPrimero(t *testing.T) {
	vol := volumenCon(t, "node_modules")
	env := libraryEnv([]string{"NODE_PATH=/usr/local/lib/node_modules"}, []volumeSpec{{mount: vol}})

	np, _ := valor(env, "NODE_PATH")
	propio := strings.Index(np, "/usr/local/lib/node_modules")
	compartido := strings.Index(np, vol)
	if propio < 0 || compartido < 0 {
		t.Fatalf("perdió una de las dos rutas: %q", np)
	}
	if propio > compartido {
		t.Errorf("la biblioteca compartida se puso por delante de la de la imagen: %q", np)
	}
	// Y no debe duplicar la variable.
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "NODE_PATH=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("hay %d NODE_PATH en el entorno", n)
	}
}

// Las dos formas de pip, que son las dos comunes:
//
//	pip install --target <vol>   -> los paquetes cuelgan del volumen
//	pip install --prefix <vol>   -> <vol>/lib/pythonX.Y/site-packages
func TestLasDosFormasDePip(t *testing.T) {
	target := volumenCon(t, "requests", "requests-2.31.0.dist-info")
	prefix := volumenCon(t, "lib/python3.12/site-packages/click")

	env := libraryEnv(nil, []volumeSpec{{mount: target}, {mount: prefix}})
	pp, ok := valor(env, "PYTHONPATH")
	if !ok {
		t.Fatal("no puso PYTHONPATH")
	}
	if !strings.Contains(pp, target) {
		t.Errorf("no reconoció el reparto de --target: %q", pp)
	}
	if !strings.Contains(pp, filepath.Join(prefix, "lib", "python3.12", "site-packages")) {
		t.Errorf("no reconoció el reparto de --prefix: %q", pp)
	}
}

// Un volumen de DATOS no debe entrar en PYTHONPATH aunque tenga carpetas.
//
// Es el caso peligroso: si entrase, un fichero llamado json.py dentro de los
// datos del usuario taparía el módulo json de la biblioteca estándar, y el fallo
// aparecería lejísimos de su causa. Por eso se exige la marca de metadatos de
// pip y no basta con que haya directorios.
func TestUnVolumenDeDatosNoSeCuelaEnPythonpath(t *testing.T) {
	datos := volumenCon(t, "notas", "proyectos/uno")
	if err := os.WriteFile(filepath.Join(datos, "json.py"), []byte("# trampa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := libraryEnv(nil, []volumeSpec{{mount: datos}})
	if v, ok := valor(env, "PYTHONPATH"); ok {
		t.Errorf("metió un volumen de datos en PYTHONPATH: %q", v)
	}
}
