package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/internal/esquema"
	"github.com/juan52878911/kindling/internal/events"
)

// M13: un dorado lleva, firmado, el digest del disco de solo lectura sobre el
// que se congeló (la capa y su base), y no se restaura si ese disco cambia.
func TestCommitFirmaElDiscoBase(t *testing.T) {
	m := newTestManager(t)
	id := "c3aa170000000013"
	falso, _ := plantillaParaCommit(t, m, id)
	img := "capa"
	m.mu.Lock()
	m.byID[id].Image = img
	m.mu.Unlock()
	escribir := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capa, base := m.layerPath(img), m.imagePath(defaultBaseImage)
	escribir(capa, "delta de la capa")
	escribir(base, "base comun")
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			volcadoFalso(m, id, dir)
		}
	})
	snap, err := m.Commit(context.Background(), id, "dorado", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	quieroCapa, _ := digest.Tree(capa)
	quieroBase, _ := digest.Tree(base)
	if snap.ImageDigest != quieroCapa || snap.BaseDigest != quieroBase {
		t.Fatalf("digests del disco = %q, %q; quería los de la capa y la base", snap.ImageDigest, snap.BaseDigest)
	}
	if err := m.verifyIntegrity(snap, dir); err != nil {
		t.Fatalf("recién hecho no pasa: %v", err)
	}

	// La firma los cubre: quitarlos del meta para saltarse la comprobación no
	// cuadra.
	enDisco, err := m.loadSnapshot("dorado")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(enDisco); err != nil {
		t.Fatalf("la firma del commit no cuadra: %v", err)
	}
	enDisco.ImageDigest, enDisco.BaseDigest = "", ""
	if err := m.comprobarFirma(enDisco); !errors.Is(err, errFirma) {
		t.Fatalf("sin los digests del disco la firma = %v; quería errFirma", err)
	}

	// La base cambia por debajo: no se restaura, y se dice que es la base.
	escribir(base, "otra base distinta")
	if err := m.verifyIntegrity(snap, dir); !errors.Is(err, errCorrupto) || !strings.Contains(err.Error(), "base image") {
		t.Fatalf("con la base cambiada = %v; quería errCorrupto nombrando la base", err)
	}
	// Vuelve a ser la de antes (otra fecha, mismo contenido): se rehashea y vale.
	escribir(base, "base comun")
	if err := m.verifyIntegrity(snap, dir); err != nil {
		t.Fatalf("con la base de vuelta: %v", err)
	}
	// Y la capa.
	escribir(capa, "delta cambiado")
	if err := m.verifyIntegrity(snap, dir); !errors.Is(err, errCorrupto) {
		t.Fatalf("con la capa cambiada = %v; quería errCorrupto", err)
	}
}

// Un meta.json v3 (v1.0: firma v2 sin los digests del disco) se sigue
// restaurando: su firma cuadra y, antes de que corra la migración, se
// re-firma en el momento en v3 con los digests de la imagen que hay (M13);
// sube a v5 con meta.json.v3.bak sin dejar de cuadrar. Lo firmó la clave de
// testdata/esquema/clave-de-prueba.key sobre los ficheros de doradoEnDisco.
func TestMetaV3SigueValiendoYSubeAV4(t *testing.T) {
	m := managerConClaveDePrueba(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	dir := m.snapDir("svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, contenido := range map[string]string{"overlay.ext4": "disco", "snap.file": "estado", "mem.file": "memoria"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(contenido), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := m.imagePath("toolchain")
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, []byte("imagen"), 0o644); err != nil {
		t.Fatal(err)
	}
	ruta := filepath.Join(dir, "meta.json")
	original := copiarFijacion(t, "meta.v3.json", ruta)
	if v, _ := esquema.Version(original); v != 3 {
		t.Fatalf("la fijación es v%d; tiene que ser v3", v)
	}
	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("la firma de un v3 ya no cuadra: %v", err)
	}
	if !strings.HasPrefix(s.Signature, firmaV3) || s.ImageDigest == "" {
		t.Fatalf("no se re-firmó en v3 con el digest de la imagen: firma %q, imagen %q", s.Signature, s.ImageDigest)
	}
	if err := m.verifyIntegrity(s, dir); err != nil {
		t.Fatalf("un v3 re-firmado no se restaura: %v", err)
	}
	if _, err := m.SetAnnotation("svc", "nota", []byte(`1`)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ruta)
	if v, _ := esquema.Version(b); v != metaSchema {
		t.Fatalf("tras reescribirlo, schema = %d; quería %d", v, metaSchema)
	}
	if bak, err := os.ReadFile(esquema.RutaRespaldo(ruta, 3)); err != nil || string(bak) != string(original) {
		t.Fatalf("sin meta.json.v3.bak con el original: %v", err)
	}
	m.invalidateSnapCache("svc")
	s, err = m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("subido a v5 la firma ya no cuadra: %v", err)
	}
}
