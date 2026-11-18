package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/esquema"
	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// M13: un dorado con la firma v2 (meta.json v4, sin los digests de su
// imagen) pasa a la v3 al migrar, con los digests calculados sobre la imagen
// que hay. Desde entonces la firma cubre sin_agente, y volver a poner el meta
// v2 (su copia .v4.bak) ya no cuela.
func TestMigrarFirmasV2AV3(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := annotTestManager(t)
	dir := doradoEnDisco(t, m, "svc", "v2")

	m.MigrarFirmas(context.Background())
	if !m.firmasV3Migradas() || !m.firmasMigradas() {
		t.Fatal("sin las marcas de migrados")
	}
	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	img, _, _ := m.digestsDeImagen("toolchain")
	if !strings.HasPrefix(s.Signature, firmaV3) || s.ImageDigest != img {
		t.Fatalf("no se re-firmó en v3 con el digest de la imagen: firma %q, imagen %q", s.Signature, s.ImageDigest)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("re-firmado no pasa: %v", err)
	}

	editarMetaAMano(t, m, "svc", func(meta map[string]any) { meta["sin_agente"] = true })
	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "svc"}); !errors.Is(err, errFirma) {
		t.Fatalf("sin_agente puesto a mano tras migrar = %v; quería errFirma", err)
	}

	bak, err := os.ReadFile(esquema.RutaRespaldo(filepath.Join(dir, "meta.json"), 4))
	if err != nil {
		t.Fatalf("sin copia del meta v4 de antes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), bak, 0o644); err != nil {
		t.Fatal(err)
	}
	m.invalidateSnapCache("svc")
	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "svc"}); !errors.Is(err, errFirma) || !strings.Contains(err.Error(), "v2") {
		t.Fatalf("meta v2 puesto de vuelta tras migrar = %v; quería errFirma por la v2", err)
	}
}

// Un meta.json v4 (v1.0.x: firma v2, con image_digest y con sin_agente
// fuera de la firma) se sigue restaurando: su firma cuadra y se re-firma en
// v3, que ya cubre sin_agente, y sube a v5 con meta.json.v4.bak. Lo firmó la
// clave de testdata/esquema/clave-de-prueba.key sobre los ficheros de
// doradoEnDisco y una imagen "imagen".
func TestMetaV4SeRefirmaEnV3(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
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
	original := copiarFijacion(t, "meta.v4.json", ruta)
	if v, _ := esquema.Version(original); v != 4 {
		t.Fatalf("la fijación es v%d; tiene que ser v4", v)
	}
	m.MigrarFirmas(context.Background())
	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Signature, firmaV3) || !s.SinAgente {
		t.Fatalf("no se re-firmó en v3 conservando sin_agente: firma %q, sin_agente %v", s.Signature, s.SinAgente)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("re-firmado no pasa: %v", err)
	}
	if err := m.verifyIntegrity(s, dir); err != nil {
		t.Fatalf("re-firmado no se restaura: %v", err)
	}
	if bak, err := os.ReadFile(esquema.RutaRespaldo(ruta, 4)); err != nil || string(bak) != string(original) {
		t.Fatalf("sin meta.json.v4.bak con el original: %v", err)
	}
	c := *s
	c.SinAgente = false
	if err := m.comprobarFirma(&c); !errors.Is(err, errFirma) {
		t.Fatalf("sin_agente cambiado tras migrar: %v", err)
	}
}

// Un dorado v2 cuya imagen ya no está no se puede re-firmar (no hay sobre qué
// calcular su digest), pero tampoco se restauraría: no deja la migración
// pendiente para siempre.
func TestMigrarFirmasV2SinImagenNoQuedaPendiente(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := annotTestManager(t)
	doradoEnDisco(t, m, "svc", "v2")
	if err := os.Remove(m.imagePath("toolchain")); err != nil {
		t.Fatal(err)
	}
	capturarLog(t)
	m.MigrarFirmas(context.Background())
	if !m.firmasV3Migradas() {
		t.Fatal("un dorado sin imagen dejó la migración pendiente")
	}
	if s, _ := m.loadSnapshot("svc"); strings.HasPrefix(s.Signature, firmaV3) {
		t.Fatal("se re-firmó un dorado sin poder calcular el digest de su imagen")
	}
}
