package guest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Con VolumeWait el agente arranca sin los volúmenes montados: NODE_PATH no
// puede salir de ellos hasta que el host los monta. Entonces se recalcula, y
// los exec que llegan después lo ven (leen el entorno en cada petición).
func TestBibliotecaSeRecalculaAlMontar(t *testing.T) {
	vol := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vol, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	v := &Volumes{}
	a := &Agent{Volumes: v}
	// Lo que hace New sin volúmenes montados.
	a.Env = LibraryEnv([]string{"PATH=/usr/bin:/bin"}, v.Specs())
	h := ExecHandlerFunc(a.Entorno)
	if slices.ContainsFunc(a.Entorno(), func(kv string) bool { return strings.HasPrefix(kv, "NODE_PATH=") }) {
		t.Fatal("NODE_PATH antes de montar")
	}

	// El host monta (volume_wait.go) y avisa.
	v.mu.Lock()
	v.mounted = []VolumeSpec{{device: "/dev/vdb", mount: vol}}
	v.noteAcquiredLocked()
	v.mu.Unlock()
	<-v.acquiredCh()
	a.recalcularBiblioteca()
	a.recalcularBiblioteca() // una sola vez: no duplica la ruta

	want := "NODE_PATH=" + filepath.Join(vol, "node_modules")
	if got := a.Entorno(); !slices.Contains(got, want) {
		t.Fatalf("entorno = %v, quería %s", got, want)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	body, _ := json.Marshal(execRequest{Cmd: []string{"sh", "-c", "echo $NODE_PATH"}})
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader(body)))
	var res execResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if strings.TrimSpace(res.Output) != filepath.Join(vol, "node_modules") {
		t.Fatalf("exec vio NODE_PATH=%q", res.Output)
	}
}
