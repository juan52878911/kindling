package fc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// firecrackerFalso levanta un servidor HTTP sobre un socket Unix, que es
// exactamente como habla Firecracker de verdad.
func firecrackerFalso(t *testing.T, h http.HandlerFunc) (*Client, *http.Request, *[]byte) {
	t.Helper()
	// El socket va en un temporal corto: los sockets Unix tienen un limite de
	// ~104 caracteres en la ruta, y el TempDir de los tests es largo.
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aqui: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var visto *http.Request
	var cuerpo []byte
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cuerpo = b
		visto = r
		h(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	return New(sock), visto, &cuerpo
}

// El fault_message es lo que lleva el diagnostico. El fallo del TSC —que invalida
// TODOS los dorados tras reiniciar el anfitrion— llega EXACTAMENTE por ahi: si
// `do` se lo comiera, explainRestoreErr no tendria que traducir, EsFalloTSC no
// reconoceria nada, y `kling mcp heal` no curaria jamas. Toda esa cadena cuelga
// de esta linea.
func TestElFaultMessageDeFirecrackerLlegaAlLlamador(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"fault_message":"Load snapshot error: Failed to restore from snapshot: ` +
			`Could not set TSC scaling within the snapshot: Invalid argument (os error 22)"}`))
	})

	err := c.LoadSnapshot(context.Background(), "/s/snap.file", "/s/mem.file", true)
	if err == nil {
		t.Fatal("un 400 se dio por bueno")
	}
	if !strings.Contains(err.Error(), "TSC") {
		t.Errorf("el fault_message no llego al error; sin el, heal no puede curar nada:\n%v", err)
	}
	// Y tiene que seguir siendo reconocible por el predicado que usa el vigia.
	if !api.EsFalloTSC(err) {
		t.Errorf("EsFalloTSC no reconoce el error que produce el cliente:\n%v", err)
	}
	// El metodo y la ruta ayudan a situar el fallo en el log.
	for _, quiero := range []string{"PUT", "/snapshot/load"} {
		if !strings.Contains(err.Error(), quiero) {
			t.Errorf("el error no dice %q: %v", quiero, err)
		}
	}
}

func TestLoadSnapshotMandaElCuerpoQueFirecrackerEspera(t *testing.T) {
	c, _, cuerpo := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.LoadSnapshot(context.Background(), "/s/snap.file", "/s/mem.file", false); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	var d struct {
		SnapshotPath string `json:"snapshot_path"`
		MemBackend   struct {
			Path string `json:"backend_path"`
			Type string `json:"backend_type"`
		} `json:"mem_backend"`
		Resume bool `json:"resume_vm"`
	}
	if err := json.Unmarshal(*cuerpo, &d); err != nil {
		t.Fatalf("cuerpo ilegible %q: %v", *cuerpo, err)
	}
	if d.SnapshotPath != "/s/snap.file" || d.MemBackend.Path != "/s/mem.file" {
		t.Errorf("rutas mal: %+v", d)
	}
	// backend_type "File" NO es un detalle: es lo que hace que la memoria se mapee
	// desde el fichero en vez de reservarse anonima, y de ahi sale que lo residente
	// sea page cache COMPARTIDO entre instancias del mismo dorado. Con "Uffd" o sin
	// el campo, la densidad medida (142 microVMs en 3,9 GB) se vendria abajo.
	if d.MemBackend.Type != "File" {
		t.Errorf("backend_type = %q; con otro se pierde el mapeo compartido del mem.file", d.MemBackend.Type)
	}
	if d.Resume {
		t.Error("resume_vm deberia ser false: la microVM se deja pausada para reapuntar discos")
	}
}

func TestUn2xxNoEsUnError(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Resume(context.Background()); err != nil {
		t.Errorf("un 204 se reporto como fallo: %v", err)
	}
}

func TestUnErrorSinFaultMessageSigueSiendoUnError(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("no soy json"))
	})
	err := c.Resume(context.Background())
	if err == nil {
		t.Fatal("un 500 con cuerpo ilegible se dio por bueno")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("el error no menciona el codigo: %v", err)
	}
}
