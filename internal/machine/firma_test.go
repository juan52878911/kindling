package machine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

	// Otro host (otra clave) no la reconoce.
	otro := newTestManager(t)
	if err := otro.comprobarFirma(s); !errors.Is(err, errFirma) {
		t.Errorf("un snapshot de otro host pasó la firma: %v", err)
	}
}

// Los snapshots anteriores a las firmas siguen restaurándose, salvo que se exija.
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
