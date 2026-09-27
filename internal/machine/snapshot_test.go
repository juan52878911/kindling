package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El error de TSC es el peor tipo de fallo: determinista, masivo (un reinicio
// del host invalida TODOS los snapshots a la vez) y con un mensaje de
// Firecracker que no apunta a ninguna salida. Traducirlo es la diferencia entre
// un 502 críptico y saber que hay que rehacer el snapshot.
func TestElErrorDeTSCSeTraduceAAlgoAccionable(t *testing.T) {
	// Tal y como llega de verdad, envuelto por el cliente de fc.
	crudo := fmt.Errorf("firecracker PUT /snapshot/load: 400 Bad Request: " +
		"Load snapshot error: Failed to restore from snapshot: " +
		"Could not set TSC scaling within the snapshot: Invalid argument (os error 22)")

	err := explainRestoreErr(crudo, `snapshot "github"`, "  kling mcp import github -force")
	if err == nil {
		t.Fatal("se tragó el error")
	}
	// Tiene que decir qué pasó (el reinicio del host) y qué hacer (rehacerlo).
	for _, q := range []string{"reboot", "kling mcp import github -force", `snapshot "github"`} {
		if !strings.Contains(err.Error(), q) {
			t.Errorf("la traducción no menciona %q:\n%v", q, err)
		}
	}
	// Y conserva el error original: quien depure necesita el texto de verdad.
	if !strings.Contains(err.Error(), "Could not set TSC scaling") {
		t.Error("la traducción perdió el error original de Firecracker")
	}

	// Lo que no se entiende pasa INTACTO: adornar un error sin conocer su causa
	// es peor que dejarlo crudo.
	otro := errors.New("firecracker PUT /snapshot/load: 400 Bad Request: missing mem file")
	if got := explainRestoreErr(otro, "snapshot \"x\"", "whatever"); got != otro {
		t.Errorf("alteró un error que no reconoce: %v", got)
	}
	if got := explainRestoreErr(nil, "snapshot \"x\"", "whatever"); got != nil {
		t.Errorf("inventó un error donde no lo había: %v", got)
	}
}

// loadSnapshotCached tiene que servir lo cacheado mientras meta.json no
// cambie de mtime, y no releerlo del disco (M-08): se comprueba corrompiendo
// el fichero por debajo y devolviéndole a mano el mismo mtime que tenía. Si
// releyera, el segundo Snapshot() fallaría al parsear el JSON roto.
func TestSnapshotCacheadaPorMtime(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", `{"name":"svc","image":"svc","vcpus":1}`)

	primero, err := m.Snapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if primero.VCPUs != 1 {
		t.Fatalf("VCPUs = %d, quería 1", primero.VCPUs)
	}

	metaPath := filepath.Join(m.snapDir("svc"), "meta.json")
	fi, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte("{esto no es json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metaPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	segundo, err := m.Snapshot("svc")
	if err != nil {
		t.Fatalf("debía servir la caché en vez de releer el meta corrupto: %v", err)
	}
	if segundo.VCPUs != 1 {
		t.Fatalf("VCPUs = %d tras la caché; quería el 1 cacheado", segundo.VCPUs)
	}
}

// La caché se invalida a mano en cuanto se escribe el meta.json (SetAnnotation
// pasa por editMeta -> writeMeta -> invalidateSnapCache): Snapshot() no puede
// seguir enseñando la versión de antes de anotar.
func TestSnapshotCacheSeInvalidaAlAnotar(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", `{"name":"svc","image":"svc"}`)

	if _, err := m.Snapshot("svc"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAnnotation("svc", "k", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	s, err := m.Snapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Annotations["k"]; !ok {
		t.Fatal("Snapshot() sigue sirviendo la versión cacheada de antes de anotar")
	}
}

// removeSnapshot también invalida: un `commit -replace` con el mismo nombre no
// puede heredar la caché del snapshot que reemplazó.
func TestSnapshotCacheSeInvalidaAlBorrar(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", `{"name":"svc","image":"svc","vcpus":1}`)
	if _, err := m.Snapshot("svc"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveSnapshot("svc"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	writeSnapMeta(t, m, "svc", `{"name":"svc","image":"svc","vcpus":7}`)
	s, err := m.Snapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if s.VCPUs != 7 {
		t.Fatalf("VCPUs = %d; quería el 7 del snapshot NUEVO, no el 1 cacheado del borrado", s.VCPUs)
	}
}

// Snapshot(name) cuenta solo las instancias del suyo, no las de los demás
// dorados: antes buscaba el nombre dentro del listado completo, así que un
// error de esa cuenta cruzada no se veía a simple vista.
func TestSnapshotCuentaSoloSusPropiasInstancias(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "a", `{"name":"a","image":"a"}`)
	writeSnapMeta(t, m, "b", `{"name":"b","image":"b"}`)
	m.byID["x"] = &api.Machine{ID: "x", From: "a", State: api.StateRunning}

	sa, err := m.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	if sa.Instances != 1 {
		t.Errorf("a.Instances = %d, quería 1", sa.Instances)
	}
	sb, err := m.Snapshot("b")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Instances != 0 {
		t.Errorf("b.Instances = %d, quería 0 (nadie corre desde b)", sb.Instances)
	}
}
