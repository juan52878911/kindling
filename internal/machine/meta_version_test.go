package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// ponerFijacion copia testdata/esquema/<fichero> como meta.json del dorado name.
func ponerFijacion(t *testing.T, m *Manager, name, fichero string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "esquema", fichero))
	if err != nil {
		t.Fatal(err)
	}
	writeSnapMeta(t, m, name, string(b))
	return b
}

// Un meta de v0.17 (sin schema) se lee entero, leer no lo toca, y la primera
// escritura deja la copia .v0.bak con el original y el meta en v1 sin perder
// nada de lo que tenía.
func TestMetaV0SeMigraConCopia(t *testing.T) {
	m := annotTestManager(t)
	original := ponerFijacion(t, m, "svc", "meta.v0.json")
	ruta := filepath.Join(m.snapDir("svc"), "meta.json")

	antes, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatalf("un meta v0 se tiene que poder leer: %v", err)
	}
	if antes.Egress != "allowlist" || antes.KernelSHA256 != "cd" || !antes.GuestIPv6Off || len(antes.Annotations) != 1 {
		t.Fatalf("v0 leído a medias: %+v", antes)
	}
	if _, err := os.Stat(esquema.RutaRespaldo(ruta, 0)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("leer no debe dejar copia: no ha migrado nada")
	}

	if _, err := m.SetAnnotation("svc", "otra", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	copia, err := os.ReadFile(esquema.RutaRespaldo(ruta, 0))
	if err != nil || !bytes.Equal(copia, original) {
		t.Fatalf("la copia .v0.bak tiene que ser el meta original: %v\n%s", err, copia)
	}
	b, _ := os.ReadFile(ruta)
	if v, err := esquema.Version(b); err != nil || v != metaSchema {
		t.Fatalf("tras escribir, schema = %d, %v; quería %d", v, err, metaSchema)
	}
	despues, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	delete(despues.Annotations, "otra")
	if !reflect.DeepEqual(antes, despues) {
		t.Fatalf("la migración perdió campos:\nantes   %+v\ndespués %+v", antes, despues)
	}

	// Una segunda escritura ya es v1 a v1: la copia buena no se pisa.
	if _, err := m.RemoveAnnotation("svc", "otra"); err != nil {
		t.Fatal(err)
	}
	if copia2, _ := os.ReadFile(esquema.RutaRespaldo(ruta, 0)); !bytes.Equal(copia2, original) {
		t.Fatal("la copia .v0.bak se pisó")
	}
}

// Un meta de un kling más nuevo no se lee, no se anota y no se toca: ni el
// listado lo enseña a medias ni una anotación lo reescribe sin sus campos.
func TestMetaMasNuevoNoSeToca(t *testing.T) {
	m := annotTestManager(t)
	original := ponerFijacion(t, m, "svc", "meta.v2.json")
	ruta := filepath.Join(m.snapDir("svc"), "meta.json")

	if _, err := m.loadSnapshot("svc"); !esquema.EsMasNuevo(err) {
		t.Fatalf("loadSnapshot = %v; quería ErrMasNuevo", err)
	}
	if _, err := m.Snapshot("svc"); !esquema.EsMasNuevo(err) {
		t.Fatalf("Snapshot = %v; quería ErrMasNuevo", err)
	}
	if _, err := m.SetAnnotation("svc", "otra", json.RawMessage(`1`)); !esquema.EsMasNuevo(err) {
		t.Fatalf("SetAnnotation = %v; quería ErrMasNuevo", err)
	}
	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "svc"}); !esquema.EsMasNuevo(err) {
		t.Fatalf("runFrom = %v; quería ErrMasNuevo", err)
	}
	if err := m.RemoveSnapshot("svc"); err == nil {
		t.Fatal("borró un dorado que no entiende")
	}
	b, err := os.ReadFile(ruta)
	if err != nil || !bytes.Equal(b, original) {
		t.Fatalf("el meta del futuro cambió: %v\n%s", err, b)
	}
	if _, err := os.Stat(esquema.RutaRespaldo(ruta, 2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no hay nada que respaldar de un fichero que no se toca")
	}
}

// Un commit -replace a medias con un meta del futuro no se "recupera": el
// nuevo está entero aunque este binario no lo entienda, y ninguno se aparta.
func TestRecuperarReemplazosRespetaElMetaDelFuturo(t *testing.T) {
	m := annotTestManager(t)
	ponerFijacion(t, m, "svc", "meta.v2.json")
	anterior := filepath.Join(m.root, "snapshots", ".svc"+sufijoAnterior)
	if err := os.MkdirAll(anterior, 0o755); err != nil {
		t.Fatal(err)
	}
	m.recuperarReemplazos()
	if _, err := os.Stat(anterior); err != nil {
		t.Fatalf("se tocó el anterior: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(m.snapDir("svc"), "meta.json")); !bytes.Contains(b, []byte("campo_del_futuro")) {
		t.Fatalf("se apartó el nuevo: %s", b)
	}
}

// editMeta conserva las claves que este binario no conoce (las de un kling más
// nuevo con la misma versión de esquema), y no resucita las que sí conoce y
// quitó.
func TestEditMetaConservaLoQueNoConoce(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", `{"schema": 1, "name": "svc", "image": "eco",
		"annotations": {"vieja": 1},
		"campo_nuevo": {"a": [1, 2]}, "otro_nuevo": "x"}`)

	for _, paso := range []func() error{
		func() error { _, err := m.SetAnnotation("svc", "otra", json.RawMessage(`{"b": true}`)); return err },
		func() error { _, err := m.RemoveAnnotation("svc", "vieja"); return err },
		func() error { _, err := m.RemoveAnnotation("svc", "otra"); return err },
	} {
		if err := paso(); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(m.snapDir("svc"), "meta.json"))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("meta inválido: %v\n%s", err, b)
	}
	if string(got["otro_nuevo"]) != `"x"` || !json.Valid(got["campo_nuevo"]) || !strings.Contains(string(got["campo_nuevo"]), `"a"`) {
		t.Fatalf("se perdieron las claves desconocidas:\n%s", b)
	}
	if _, hay := got["annotations"]; hay {
		t.Fatalf("las anotaciones quitadas volvieron del meta anterior:\n%s", b)
	}
	if _, err := os.Stat(esquema.RutaRespaldo(filepath.Join(m.snapDir("svc"), "meta.json"), 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("de v1 a v1 no hay migración ni copia")
	}
}

// codificarMeta escribe la versión y el origen, y nunca el campo calculado
// Stale.
func TestCodificarMetaConOrigen(t *testing.T) {
	m := newTestManager(t)
	m.FijarOrigen("v0.18.0", "Firecracker v1.12.0\n\nSupported snapshot data format versions: 5.0.0\n")
	s := &api.Snapshot{Name: "svc", Stale: "no se guarda"}
	m.grabarOrigen(s)
	if s.VMM != "firecracker 1.12.0" || s.KlingVersion != "0.18.0" {
		t.Fatalf("origen = %q / %q", s.VMM, s.KlingVersion)
	}
	b, err := codificarMeta(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := esquema.Version(b); v != metaSchema || bytes.Contains(b, []byte("stale")) ||
		!bytes.Contains(b, []byte(`"vmm": "firecracker 1.12.0"`)) {
		t.Fatalf("meta:\n%s", b)
	}
	// Sin FijarOrigen (tests, un daemon sin VMM) no se graba nada.
	s2 := &api.Snapshot{}
	newTestManager(t).grabarOrigen(s2)
	if s2.VMM != "" || s2.KlingVersion != "" {
		t.Fatalf("sin origen: %+v", s2)
	}
}

func TestCausaObsoleto(t *testing.T) {
	casos := []struct {
		hecho, ahora string
		obsoleto     bool
	}{
		{"", "firecracker 1.17.0", false},                   // v0: no consta
		{"firecracker 1.12.0", "", false},                   // no se sabe qué corre
		{"firecracker 1.12.0", "firecracker 1.12.3", false}, // parche: mismo formato
		{"firecracker 1.12.0", "firecracker 1.13.0", true},
		{"firecracker 1.12.0", "firecracker 2.0.0", true},
		{"firecracker 1.12.0", "kling-vz 0.18.0", true}, // otro VMM
		{"kling-vz 0.17.0", "kling-vz 0.18.0", false},   // lo decide su kling_vz
		{"firecracker dev", "firecracker 1.13.0", false},
	}
	for _, c := range casos {
		if got := causaObsoleto(c.hecho, c.ahora); (got != "") != c.obsoleto {
			t.Errorf("causaObsoleto(%q, %q) = %q; obsoleto=%v", c.hecho, c.ahora, got, c.obsoleto)
		}
	}
	if got := normalizarVMM("kling-vz 0.18.0"); got != "kling-vz 0.18.0" {
		t.Errorf("normalizarVMM = %q", got)
	}
}

// Un dorado hecho con otra MAJOR.MINOR de Firecracker sale obsoleto en el
// listado y en inspect, y runFrom se niega con la orden para rehacerlo en vez
// de llegar al VMM.
func TestDoradoObsoletoSeMarcaYNoSeRestaura(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := annotTestManager(t)
	escribirSnapshot(t, m, "viejo", api.Snapshot{VMM: "firecracker 1.12.0"})
	escribirSnapshot(t, m, "parche", api.Snapshot{VMM: "firecracker 1.17.0"})
	m.FijarOrigen("0.18.0", "Firecracker v1.17.2")

	s, err := m.Snapshot("viejo")
	if err != nil || !strings.Contains(s.Stale, "firecracker 1.12.0") || !strings.Contains(s.Stale, "firecracker 1.17.2") {
		t.Fatalf("Snapshot: %v, stale %q", err, s.Stale)
	}
	obsoletos := map[string]string{}
	for _, s := range m.Snapshots() {
		obsoletos[s.Name] = s.Stale
	}
	if obsoletos["viejo"] == "" || obsoletos["parche"] != "" {
		t.Fatalf("listado: %v", obsoletos)
	}

	_, err = m.runFrom(context.Background(), api.RunRequest{From: "viejo"})
	if !errors.Is(err, ErrDoradoObsoleto) || !strings.Contains(err.Error(), "kling save -replace") {
		t.Fatalf("runFrom = %v; quería ErrDoradoObsoleto con la orden", err)
	}
	// Leer y marcar no escribe: el meta sigue sin "stale".
	if b, _ := os.ReadFile(filepath.Join(m.snapDir("viejo"), "meta.json")); bytes.Contains(b, []byte("stale")) {
		t.Fatalf("stale llegó al disco: %s", b)
	}
}

// Una máquina congelada con otro Firecracker no se intenta descongelar: se
// dice con cuál se congeló antes de lanzar nada.
func TestThawSeNiegaConOtroVMM(t *testing.T) {
	m := newTestManager(t)
	m.FijarOrigen("0.18.0", "Firecracker v1.17.0")
	id := "0bb0000000000001"
	mc := m.addForTest(id)
	m.mu.Lock()
	m.byID[id].State = api.StateWarm
	m.mu.Unlock()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("volcado "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "firecracker 1.12.0"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Thaw(context.Background(), mc.ID)
	if err == nil || !strings.Contains(err.Error(), "frozen with firecracker 1.12.0") {
		t.Fatalf("Thaw = %v; quería el error del VMM distinto", err)
	}
}

// El commit graba con qué se hizo el dorado y en v1.
func TestCommitGrabaElOrigen(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	m.FijarOrigen("0.18.0", "Firecracker v1.12.0")
	id := "c2aa170000000002"
	falso, _ := plantillaParaCommit(t, m, id)
	escribirKernel(t, m, "kernel de esta prueba")
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})
	if _, err := m.Commit(context.Background(), id, "dorado", false); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
	if v, _ := esquema.Version(b); v != metaSchema {
		t.Fatalf("schema %d:\n%s", v, b)
	}
	got, err := m.loadSnapshot("dorado")
	if err != nil || got.VMM != "firecracker 1.12.0" || got.KlingVersion != "0.18.0" {
		t.Fatalf("%v %+v", err, got)
	}
}
