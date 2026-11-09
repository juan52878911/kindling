package guest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// POST /drop-caches suelta la caché de páginas del invitado (escribe 1 en
// drop_caches) y dice cuánta había antes y cuánta queda. Es de control: el
// gateway no la reenvía. Y se anuncia, para que el host no la pida a un
// agente anterior.
func TestDropCaches(t *testing.T) {
	dir := t.TempDir()
	viejoMem, viejoDrop := meminfoPath, dropCachesPath
	defer func() { meminfoPath, dropCachesPath = viejoMem, viejoDrop }()
	meminfoPath = filepath.Join(dir, "meminfo")
	dropCachesPath = filepath.Join(dir, "drop_caches")
	os.WriteFile(meminfoPath, []byte("MemTotal: 262144 kB\nMemAvailable: 131072 kB\nCached:  102400 kB\n"), 0o644)
	os.WriteFile(dropCachesPath, nil, 0o644)

	h := DropCachesHandler()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, api.GuestDropCachesPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
	if b, _ := os.ReadFile(dropCachesPath); len(b) != 0 {
		t.Fatalf("GET dropped caches: %q", b)
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, api.GuestDropCachesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(dropCachesPath); string(b) != "1" {
		t.Fatalf("drop_caches got %q, want 1", b)
	}
	var res api.GuestDropCaches
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.CachedBeforeMiB != 100 || res.CachedAfterMiB != 100 {
		t.Fatalf("%s: %+v %v", rec.Body, res, err)
	}

	// Sin drop_caches (fuera de Linux), un error y no un 200 que mienta.
	dropCachesPath = filepath.Join(dir, "no", "drop_caches")
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, api.GuestDropCachesPath, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("without drop_caches: %d", rec.Code)
	}

	if !IsControlPath(api.GuestDropCachesPath) {
		t.Fatal("/drop-caches is not a control path")
	}
	if a := (&Agent{}); !slices.Contains(a.Caps(), api.GuestCapDropCaches) {
		t.Fatalf("caps %v", a.Caps())
	}
	mux := http.NewServeMux()
	(&Agent{preparado: make(chan struct{})}).Register(mux)
	if _, pat := mux.Handler(httptest.NewRequest(http.MethodPost, api.GuestDropCachesPath, nil)); pat != api.GuestDropCachesPath {
		t.Fatalf("not registered: %q", pat)
	}
}
