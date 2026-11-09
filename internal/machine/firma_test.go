package machine

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func snapDePrueba() *api.Snapshot {
	return &api.Snapshot{Name: "svc", Image: "toolchain", RootfsSHA256: "aa", SnapSHA256: "bb",
		Egress: "none", VCPUs: 1, MemMiB: 256}
}

func TestFirmaDeSnapshots(t *testing.T) {
	m := newTestManager(t)
	s := snapDePrueba()
	if err := m.firmar(s); err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("recién firmado: %v", err)
	}

	// La clave es solo de root.
	fi, err := os.Stat(filepath.Join(m.root, "secrets", "snapshot.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("clave: %v, permisos %v", err, fi.Mode().Perm())
	}

	// Cambiar lo que decide cómo nace una instancia rompe la firma: los hashes,
	// y sobre todo la política (encender exec o dar red).
	for nombre, tocar := range map[string]func(*api.Snapshot){
		"hash del overlay": func(s *api.Snapshot) { s.RootfsSHA256 = "cc" },
		"hash en árbol":    func(s *api.Snapshot) { s.RootfsDigest = "sha256-tree-4m:cc" },
		"encender exec":    func(s *api.Snapshot) { s.AllowExec = true },
		"dar internet":     func(s *api.Snapshot) { s.Egress = "internet" },
		"añadir volumen":   func(s *api.Snapshot) { s.Volumes = []api.VolumeAttachment{{Name: "x", Mount: "/x"}} },
	} {
		c := *s
		tocar(&c)
		if err := m.comprobarFirma(&c); !errors.Is(err, errFirma) {
			t.Errorf("%s: la firma siguió valiendo (%v)", nombre, err)
		}
	}

	// Las anotaciones no: cambian después del commit por diseño.
	c := *s
	c.Annotations = map[string]json.RawMessage{"mcp.tools": json.RawMessage("[]")}
	if err := m.comprobarFirma(&c); err != nil {
		t.Errorf("una anotación invalidó la firma: %v", err)
	}

	// KernelSHA256 (K2) tampoco: es una comprobación de compatibilidad con el
	// vmlinux instalado, no una decisión de arranque, y cubrirla habría roto la
	// firma de todo snapshot anterior a este campo.
	k := *s
	k.KernelSHA256 = strings.Repeat("f", 64)
	if err := m.comprobarFirma(&k); err != nil {
		t.Errorf("KernelSHA256 invalidó la firma: %v", err)
	}

	// GuestIPv6Off (F2) tampoco: es diagnóstico, y cubrirlo invalidaría la
	// firma de todo dorado congelado antes de este campo.
	i := *s
	i.GuestIPv6Off = true
	if err := m.comprobarFirma(&i); err != nil {
		t.Errorf("GuestIPv6Off invalidó la firma: %v", err)
	}

	// Otro host (otra clave) no la reconoce.
	otro := newTestManager(t)
	if err := otro.comprobarFirma(s); !errors.Is(err, errFirma) {
		t.Errorf("un snapshot de otro host pasó la firma: %v", err)
	}
}

// Los snapshots anteriores a las firmas siguen restaurándose, salvo que se exija.
// Sin RootfsDigest (los dorados de antes de meta.json v2) se firma byte a
// byte lo mismo que antes de existir el campo: sus firmas siguen valiendo.
func TestContenidoFirmadoSinDigestEnArbol(t *testing.T) {
	want := `{"Name":"svc","Image":"toolchain","Rootfs":"aa","Snap":"bb","Egress":"none",` +
		`"Allow":"","Vols":"","AllowExec":false,"VCPUs":1,"MemMiB":256}`
	if got := string(contenidoFirmado(snapDePrueba())); got != want {
		t.Fatalf("signed content changed for old goldens:\n got %s\nwant %s", got, want)
	}
}

func TestSnapshotSinFirma(t *testing.T) {
	m := newTestManager(t)
	s := snapDePrueba()
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("sin firma y sin exigirla: %v", err)
	}
	t.Setenv("KLING_REQUIRE_SIGNED", "1")
	if err := m.comprobarFirma(s); !errors.Is(err, errFirma) {
		t.Fatalf("exigiendo firma: %v", err)
	}
}

// B2: la firma v2 cubre lo que la v1 dejaba fuera y sí decide cómo nace una
// instancia. Cambiar cualquiera de esos campos en meta.json la rompe.
func TestFirmaV2CubreEtiquetasTopesEntornoYMemoria(t *testing.T) {
	m := newTestManager(t)
	s := snapDePrueba()
	s.Labels = map[string]string{api.LabelPorts: "8080", api.LabelOwner: "ana"}
	s.MemMaxMiB, s.CPUPct, s.EnvKeys = 512, 50, []string{"TOKEN"}
	s.MemDigest = "sha256-tree-4m:aa"
	if err := m.firmar(s); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Signature, firmaV2) {
		t.Fatalf("una firma nueva debería ser v2: %q", s.Signature)
	}
	if err := m.comprobarFirma(s); err != nil {
		t.Fatalf("recién firmada: %v", err)
	}
	for nombre, tocar := range map[string]func(*api.Snapshot){
		"abrir un puerto": func(s *api.Snapshot) {
			s.Labels = map[string]string{api.LabelPorts: "8080,22", api.LabelOwner: "ana"}
		},
		"cambiar de dueño": func(s *api.Snapshot) {
			s.Labels = map[string]string{api.LabelPorts: "8080", api.LabelOwner: "eva"}
		},
		"subir el techo":   func(s *api.Snapshot) { s.MemMaxMiB = 8192 },
		"quitar el tope":   func(s *api.Snapshot) { s.CPUPct = 0 },
		"fijar el tope":    func(s *api.Snapshot) { s.CPUPctFixed = true },
		"cambiar el env":   func(s *api.Snapshot) { s.EnvKeys = nil },
		"cambiar la RAM":   func(s *api.Snapshot) { s.MemDigest = "sha256-tree-4m:bb" },
		"bajar a firma v1": func(s *api.Snapshot) { s.Signature = strings.TrimPrefix(s.Signature, firmaV2) },
		"y lo de siempre":  func(s *api.Snapshot) { s.AllowExec = true },
		"otra versión":     func(s *api.Snapshot) { s.Signature = "v3:" + strings.TrimPrefix(s.Signature, firmaV2) },
	} {
		c := *s
		tocar(&c)
		if err := m.comprobarFirma(&c); !errors.Is(err, errFirma) {
			t.Errorf("%s: la firma siguió valiendo (%v)", nombre, err)
		}
	}

	// Las credenciales no van en meta.json (se leen del almacén sellado) y se
	// ponen después del commit: no la invalidan.
	c := *s
	c.CredentialDomains = []string{"api.example.com"}
	if err := m.comprobarFirma(&c); err != nil {
		t.Errorf("las credenciales invalidaron la firma: %v", err)
	}
}

// Los dorados firmados en v1 siguen valiendo, con lo que la v1 cubría: no hay
// que rehacer los dorados de nadie para actualizar.
func TestFirmaV1SigueValiendo(t *testing.T) {
	m := newTestManager(t)
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

// Lo que pide el informe, de punta a punta por meta.json: un dorado con
// kling.ports cambiado a mano en el fichero no pasa la firma.
func TestMetaConPuertosAlteradosNoPasaLaFirma(t *testing.T) {
	m := newTestManager(t)
	s := snapDePrueba()
	s.Labels = map[string]string{api.LabelPorts: "8080"}
	if err := m.firmar(s); err != nil {
		t.Fatal(err)
	}
	b, err := codificarMeta(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if leido, _, err := decodificarMeta("meta.json", b); err != nil || m.comprobarFirma(leido) != nil {
		t.Fatalf("el meta intacto no pasa: %v", err)
	}
	alterado := strings.Replace(string(b), `"kling.ports": "8080"`, `"kling.ports": "8080,22"`, 1)
	if alterado == string(b) {
		t.Fatalf("el meta no tiene la etiqueta como se esperaba:\n%s", b)
	}
	leido, _, err := decodificarMeta("meta.json", []byte(alterado))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(leido); !errors.Is(err, errFirma) {
		t.Fatalf("kling.ports alterado en meta.json pasó la firma: %v", err)
	}
}

// Etiquetas y entorno vacíos (no nil) al firmar vuelven como nil de meta.json
// (omitempty): la firma tiene que seguir valiendo tras la vuelta.
func TestFirmaV2VacioYAusenteFirmanIgual(t *testing.T) {
	m := newTestManager(t)
	s := snapDePrueba()
	s.Labels, s.EnvKeys = map[string]string{}, []string{}
	if err := m.firmar(s); err != nil {
		t.Fatal(err)
	}
	b, err := codificarMeta(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	leido, _, err := decodificarMeta("meta.json", b)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarFirma(leido); err != nil {
		t.Fatalf("vacío firmado, nil leído: %v", err)
	}
}
