package machine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/internal/esquema"
	"github.com/juan52878911/kindling/pkg/api"
)

// doradoEnDisco deja un dorado con sus tres ficheros y los digests que los
// cubren. firma decide cómo va firmado: "v2" (como lo firma hoy un commit),
// "v1" (como lo firmaba un kling anterior a B2, en un meta de schema 2) o ""
// (sin firma). Devuelve su directorio.
func doradoEnDisco(t *testing.T, m *Manager, name, firma string) string {
	t.Helper()
	dir := m.snapDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, contenido := range map[string]string{"overlay.ext4": "disco", "snap.file": "estado", "mem.file": "memoria"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(contenido), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &api.Snapshot{Name: name, Image: "toolchain", Egress: "none", VCPUs: 1, MemMiB: 256,
		Labels: map[string]string{api.LabelOwner: "ana", api.LabelPorts: "8080"}}
	var err error
	if s.RootfsSHA256, err = digest.File(filepath.Join(dir, "overlay.ext4")); err != nil {
		t.Fatal(err)
	}
	if s.SnapSHA256, err = digest.File(filepath.Join(dir, "snap.file")); err != nil {
		t.Fatal(err)
	}
	schema := metaSchema
	switch firma {
	case "v2":
		if s.MemDigest, err = digest.Tree(filepath.Join(dir, "mem.file")); err != nil {
			t.Fatal(err)
		}
		if err := m.firmar(s); err != nil {
			t.Fatal(err)
		}
	case "v1":
		sum, err := m.macDe(contenidoFirmado(s))
		if err != nil {
			t.Fatal(err)
		}
		s.Signature, schema = hex.EncodeToString(sum), 2
	}
	b, err := json.MarshalIndent(metaEnDisco{esquema.Cabecera{Schema: schema}, s}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(dir, b); err != nil {
		t.Fatal(err)
	}
	return dir
}

// editarMetaAMano cambia meta.json en disco como lo haría alguien con acceso
// al fichero, sin pasar por el daemon.
func editarMetaAMano(t *testing.T, m *Manager, name string, edit func(map[string]any)) {
	t.Helper()
	ruta := filepath.Join(m.snapDir(name), "meta.json")
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	edit(meta)
	if b, err = json.Marshal(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruta, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m.invalidateSnapCache(name)
}

// A2: un dorado firmado al que se le quitan la firma y los digests (para que
// verifyIntegrity lo diera por antiguo y nada lo comprobara) ya no se
// restaura: la firma se exige por defecto.
func TestFirmaVaciadaRunFromFalla(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := annotTestManager(t)
	doradoEnDisco(t, m, "svc", "v2")
	editarMetaAMano(t, m, "svc", func(meta map[string]any) {
		for _, k := range []string{"signature", "rootfs_sha256", "rootfs_digest", "snap_sha256", "mem_digest"} {
			delete(meta, k)
		}
		meta["allow_exec"] = true
	})
	_, err := m.runFrom(context.Background(), api.RunRequest{From: "svc"})
	if !errors.Is(err, errFirma) || !strings.Contains(err.Error(), "require_signed") {
		t.Fatalf("runFrom de un dorado con la firma quitada = %v; quería errFirma y cómo arreglarlo", err)
	}
	// Firmado pero sin ningún digest tampoco: eso no lo escribió kindling.
	s := &api.Snapshot{Name: "svc", Signature: firmaV2 + "00"}
	if err := m.verifyIntegrity(s, m.snapDir("svc")); !errors.Is(err, errCorrupto) {
		t.Fatalf("verifyIntegrity de uno firmado sin digests = %v", err)
	}
}

// Sin firma: se rechaza por defecto, diciendo cómo arreglarlo; con
// daemon.require_signed off (o KLING_REQUIRE_SIGNED=0) se acepta, y
// KLING_REQUIRE_SIGNED=1 manda sobre la configuración.
func TestSnapshotSinFirma(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	t.Cleanup(func() { FijarFirmaExigida(true) })
	m := annotTestManager(t)
	s := snapDePrueba()
	if err := m.comprobarFirma(s); !errors.Is(err, errFirma) || !strings.Contains(err.Error(), "kling save -replace") {
		t.Fatalf("sin firma, por defecto: %v", err)
	}
	FijarFirmaExigida(false)
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("sin firma y require_signed off: %v", err)
	}
	t.Setenv("KLING_REQUIRE_SIGNED", "1")
	if err := m.comprobarFirma(s); !errors.Is(err, errFirma) {
		t.Fatalf("KLING_REQUIRE_SIGNED=1 sobre require_signed off: %v", err)
	}
	FijarFirmaExigida(true)
	t.Setenv("KLING_REQUIRE_SIGNED", "0")
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("KLING_REQUIRE_SIGNED=0: %v", err)
	}
}

// Con la firma sin exigir, una v1 se sigue comprobando con lo que la v1
// cubría.
func TestFirmaV1SinExigirSigueValiendo(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "0")
	m := annotTestManager(t)
	s := snapDePrueba()
	s.Labels = map[string]string{api.LabelPorts: "8080"}
	sum, err := m.macDe(contenidoFirmado(s))
	if err != nil {
		t.Fatal(err)
	}
	s.Signature = hex.EncodeToString(sum) // como la escribía firmar() antes de B2
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("una firma v1 dejó de valer: %v", err)
	}
	c := *s
	c.AllowExec = true
	if err := m.comprobarFirma(&c); !errors.Is(err, errFirma) {
		t.Errorf("la v1 dejó de cubrir la política: %v", err)
	}
}

// La migración re-firma en v2 los dorados v1 del host (con el digest del
// mem.file, que la v1 no tenía) y deja la marca. Desde entonces la firma
// cubre las etiquetas: cambiar kling.owner en meta.json se ve. Y volver a
// poner el meta v1 (su copia .bak) ya no cuela: la v1 no se re-firma más.
func TestMigrarFirmasV1(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := annotTestManager(t)
	dir := doradoEnDisco(t, m, "svc", "v1")
	doradoEnDisco(t, m, "nuevo", "v2")
	doradoEnDisco(t, m, "suelto", "")
	doradoEnDisco(t, m, "ajeno", "v1")
	editarMetaAMano(t, m, "ajeno", func(meta map[string]any) { meta["allow_exec"] = true })

	m.MigrarFirmas(context.Background())
	if !m.firmasMigradas() {
		t.Fatal("sin la marca de migrados: lo que no cuadra o no tiene firma no es algo pasajero")
	}
	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Signature, firmaV2) || s.MemDigest == "" {
		t.Fatalf("no se re-firmó en v2 con el digest de la memoria: firma %q, mem %q", s.Signature, s.MemDigest)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("re-firmado no pasa: %v", err)
	}
	if s.Labels[api.LabelOwner] != "ana" {
		t.Fatalf("la migración cambió las etiquetas: %v", s.Labels)
	}
	if _, err := os.Stat(esquema.RutaRespaldo(filepath.Join(dir, "meta.json"), 2)); err != nil {
		t.Fatalf("sin copia del meta v2 de antes: %v", err)
	}
	if a, _ := m.loadSnapshot("ajeno"); strings.HasPrefix(a.Signature, firmaV2) {
		t.Fatal("se re-firmó un dorado cuya v1 no cuadra")
	}

	editarMetaAMano(t, m, "svc", func(meta map[string]any) {
		meta["labels"].(map[string]any)[api.LabelOwner] = "eva"
	})
	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "svc"}); !errors.Is(err, errFirma) {
		t.Fatalf("kling.owner cambiado tras migrar = %v; quería errFirma", err)
	}

	bak, err := os.ReadFile(esquema.RutaRespaldo(filepath.Join(dir, "meta.json"), 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), bak, 0o644); err != nil {
		t.Fatal(err)
	}
	m.invalidateSnapCache("svc")
	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "svc"}); !errors.Is(err, errFirma) || !strings.Contains(err.Error(), "v1") {
		t.Fatalf("meta v1 puesto de vuelta tras migrar = %v; quería errFirma por la v1", err)
	}
}

// Antes de que corra la migración (un runFrom que llega primero, o un
// manager sin vigilantes), una v1 que cuadra se re-firma en el momento.
func TestFirmaV1SeMigraAlRestaurar(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := annotTestManager(t)
	doradoEnDisco(t, m, "svc", "v1")
	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("v1 de este host sin migrar todavía: %v", err)
	}
	if !strings.HasPrefix(s.Signature, firmaV2) {
		t.Fatalf("no quedó re-firmada: %q", s.Signature)
	}
	if d, _ := m.loadSnapshot("svc"); d.Signature != s.Signature {
		t.Fatalf("la firma nueva no llegó al disco: %q", d.Signature)
	}
	// Una v1 que no cuadra no se re-firma.
	doradoEnDisco(t, m, "otro", "v1")
	editarMetaAMano(t, m, "otro", func(meta map[string]any) { meta["egress"] = "all" })
	o, _ := m.loadSnapshot("otro")
	if err := m.comprobarFirma(o); !errors.Is(err, errFirma) {
		t.Fatalf("v1 alterada: %v", err)
	}
	// Un mem.file cambiado (sin digest en la v1) no se puede saber; uno con
	// el overlay cambiado sí, y no se re-firma.
	doradoEnDisco(t, m, "roto", "v1")
	if err := os.WriteFile(filepath.Join(m.snapDir("roto"), "overlay.ext4"), []byte("otro disco"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := m.loadSnapshot("roto")
	if err := m.comprobarFirma(r); !errors.Is(err, errCorrupto) {
		t.Fatalf("v1 con el overlay cambiado: %v", err)
	}
}
