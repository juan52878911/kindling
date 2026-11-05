package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/pkg/api"
)

// GET /info trae el sha256 del vmlinux instalado y los dorados grabados con
// otro; los que no constan o coinciden no salen.
func TestInfoTraeElKernelYLosDoradosConOtro(t *testing.T) {
	root := t.TempDir()
	mgr := nuevoManager(t, root)
	s := &Server{mgr: mgr, root: root}

	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	vmlinux := mgr.KernelPath()
	if err := os.WriteFile(vmlinux, []byte("kernel nuevo"), 0o644); err != nil {
		t.Fatal(err)
	}
	actual, err := digest.File(vmlinux)
	if err != nil {
		t.Fatal(err)
	}
	for name, sha := range map[string]string{"viejo": "deadbeef", "igual": actual, "sin-campo": ""} {
		dir := filepath.Join(root, "snapshots", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta, _ := json.Marshal(map[string]string{"name": name, "image": "foo", "kernel_sha256": sha})
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/info", nil))
	var info api.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.KernelSHA256 != actual {
		t.Fatalf("kernel_sha256 = %q, quería %q", info.KernelSHA256, actual)
	}
	if len(info.KernelMismatch) != 1 || info.KernelMismatch[0] != (api.KernelMismatch{Kind: "snapshot", Name: "viejo", KernelSHA256: "deadbeef"}) {
		t.Fatalf("kernel_mismatch = %+v", info.KernelMismatch)
	}
}

func TestKernelesDistintos(t *testing.T) {
	snaps := []*api.Snapshot{{Name: "a", KernelSHA256: "k1"}, {Name: "b", KernelSHA256: "k2"}, {Name: "c"}, nil}
	congeladas := map[string]string{"z": "k1", "y": "k2", "x": ""}
	got := kernelesDistintos("k2", snaps, congeladas)
	want := []api.KernelMismatch{{Kind: "snapshot", Name: "a", KernelSHA256: "k1"}, {Kind: "machine", Name: "z", KernelSHA256: "k1"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %+v, quería %+v", i, got[i], want[i])
		}
	}
	if got := kernelesDistintos("", snaps, congeladas); got != nil {
		t.Fatalf("sin kernel instalado no hay con qué comparar: %+v", got)
	}
}

func TestKernelSelloCongelada(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "machines", "id1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := kernelSelloCongelada(root, "id1"); got != "" {
		t.Fatalf("sin sello = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "volcado.ok"), []byte(`{"schema":1,"kernel_sha256":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := kernelSelloCongelada(root, "id1"); got != "abc" {
		t.Fatalf("sello = %q", got)
	}
}
