package machine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// managerConClaveDePrueba es un manager con la clave de host con la que se
// sellaron las fijaciones de testdata/esquema (las escribió el daemon de
// v0.17, antes de la cabecera).
func managerConClaveDePrueba(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t)
	clave, err := os.ReadFile(filepath.Join("testdata", "esquema", "clave-de-prueba.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(m.root, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.root, "secrets", "snapshot.key"), clave, 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}

func copiarFijacion(t *testing.T, fichero, ruta string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "esquema", fichero))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(ruta), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruta, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

// Un almacén de v0.17 (sin cabecera) se lee, leer no lo toca, y el siguiente
// sellado lo deja en v1 con la copia del original al lado.
func TestAlmacenV0SeLeeYSeMigraConCopia(t *testing.T) {
	m := managerConClaveDePrueba(t)
	ruta := m.credPath("m1")
	original := copiarFijacion(t, "credentials.v0.enc", ruta)

	creds, err := m.cargarCredenciales("m1")
	if err != nil || len(creds) != 1 || creds[0].Secret != "sk_test_FIJACION" || creds[0].Domain != "api.example.com" {
		t.Fatalf("almacén v0: %+v, %v", creds, err)
	}
	if b, _ := os.ReadFile(ruta); !bytes.Equal(b, original) {
		t.Fatal("leer reescribió el almacén")
	}

	creds = append(creds, credproxy.Credential{Env: "OTRA", Domain: "otra.example.com", Placeholder: "kling-cred-bb", Secret: "sk_test_OTRA"})
	if err := m.guardarCredenciales("m1", creds); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ruta)
	if versionSellada(b) != versionAlmacen || !bytes.HasPrefix(b, []byte("KLCS\x01")) {
		t.Fatalf("tras sellar no quedó en v1: %q", b[:min(8, len(b))])
	}
	copia, err := os.ReadFile(esquema.RutaRespaldo(ruta, 0))
	if err != nil || !bytes.Equal(copia, original) {
		t.Fatalf("la copia .v0.bak tiene que ser el almacén original: %v", err)
	}
	if fi, _ := os.Stat(esquema.RutaRespaldo(ruta, 0)); fi.Mode().Perm() != 0o600 {
		t.Errorf("la copia no es 0600: %v", fi.Mode().Perm())
	}
	back, err := m.cargarCredenciales("m1")
	if err != nil || len(back) != 2 || back[1].Secret != "sk_test_OTRA" {
		t.Fatalf("v1: %+v, %v", back, err)
	}

	// Quitar todas las credenciales borra también la copia: no deben seguir
	// en disco, ni cifradas.
	if err := m.guardarCredenciales("m1", nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ruta, esquema.RutaRespaldo(ruta, 0)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s sigue ahí tras quitar las credenciales", filepath.Base(p))
		}
	}
}

// El almacén de plantilla tiene el mismo formato y la misma migración.
func TestAlmacenDePlantillaV0(t *testing.T) {
	m := managerConClaveDePrueba(t)
	ruta := m.credSnapPath("svc")
	original := copiarFijacion(t, "plantilla.v0.enc", ruta)
	specs, err := m.cargarCredencialesPlantilla("svc")
	if err != nil || len(specs) != 1 || specs[0].Secret != "sk_test_FIJACION" {
		t.Fatalf("plantilla v0: %+v, %v", specs, err)
	}
	sellado, err := m.sellar(specs, "snapshot:svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := escribirSellado(ruta, sellado); err != nil {
		t.Fatal(err)
	}
	if copia, _ := os.ReadFile(esquema.RutaRespaldo(ruta, 0)); !bytes.Equal(copia, original) {
		t.Fatal("falta la copia .v0.bak de la plantilla")
	}
	if specs, err := m.cargarCredencialesPlantilla("svc"); err != nil || len(specs) != 1 {
		t.Fatalf("plantilla v1: %+v, %v", specs, err)
	}
}

// Un almacén de un kling más nuevo no se descifra, no se pisa y no se borra.
func TestAlmacenMasNuevoNoSeToca(t *testing.T) {
	m := managerConClaveDePrueba(t)
	ruta := m.credPath("m1")
	futuro := append([]byte("KLCS\x02"), bytes.Repeat([]byte{0xaa}, 64)...)
	if err := os.MkdirAll(filepath.Dir(ruta), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruta, futuro, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.cargarCredenciales("m1"); !esquema.EsMasNuevo(err) {
		t.Fatalf("cargar = %v; quería ErrMasNuevo", err)
	}
	creds := []credproxy.Credential{{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-aa", Secret: "x"}}
	if err := m.guardarCredenciales("m1", creds); !esquema.EsMasNuevo(err) {
		t.Fatalf("guardar = %v; quería ErrMasNuevo", err)
	}
	if err := m.guardarCredenciales("m1", nil); !esquema.EsMasNuevo(err) {
		t.Fatalf("borrar = %v; quería ErrMasNuevo", err)
	}
	if b, _ := os.ReadFile(ruta); !bytes.Equal(b, futuro) {
		t.Fatal("el almacén del futuro cambió")
	}
	if _, err := os.Stat(esquema.RutaRespaldo(ruta, 2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no hay nada que respaldar de un fichero que no se toca")
	}
}

// La cabecera va dentro del dato autenticado: quitársela a un v1 no lo
// convierte en un v0 legible.
func TestCabeceraDelAlmacenAutenticada(t *testing.T) {
	m := managerConClaveDePrueba(t)
	sellado, err := m.sellar([]string{"a"}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if err := m.abrir("x", sellado, "m1", &out); err != nil || len(out) != 1 {
		t.Fatalf("v1: %v %v", out, err)
	}
	if err := m.abrir("x", sellado[len(magiaAlmacen)+1:], "m1", &out); err == nil {
		t.Fatal("un v1 sin su cabecera se descifró como v0")
	}
}
