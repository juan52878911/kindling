package imagen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Un Packages.xz que descomprimido pasa del tope es un error, no un índice
// que se lee entero en memoria (ni uno cortado).
func TestResolveIndexUnpackedCap(t *testing.T) {
	raw, err := os.ReadFile("../xz/testdata/text.xz") // 188422 bytes descomprimido
	if err != nil {
		t.Fatal(err)
	}
	const snap = "20250101T000000Z"
	src := IndexSource{URL: "https://example.invalid/Packages.xz", Base: "https://example.invalid"}
	oldSrc, oldMax := IndexSources, maxIndexUnpacked
	t.Cleanup(func() { IndexSources, maxIndexUnpacked = oldSrc, oldMax })
	IndexSources = func(string, string) []IndexSource { return []IndexSource{src} }
	maxIndexUnpacked = 64 << 10

	d := Debs{Cache: t.TempDir(), Snapshot: snap}
	dir := filepath.Join(d.Cache, "debian-index", snap)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(src.URL))
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(h[:8])+"."+path.Base(src.URL)), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = d.Resolve(context.Background(), "amd64", nil, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "once decompressed") {
		t.Fatalf("Resolve: %v", err)
	}
}
