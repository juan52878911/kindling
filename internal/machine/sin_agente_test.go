package machine

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Commit apunta en el meta.json del dorado si el agente escuchaba al
// guardarlo, y se lee de vuelta de disco.
func TestCommitGrabaSinAgente(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, c := range []struct {
		id, addr string
		quiere   bool
	}{
		{"c0aa170000000031", ln.Addr().String(), false},
		{"c0aa170000000032", dirSinNadie, true},
	} {
		m, _ := managerConAlmacen(t, c.id, "dorado")
		m.mu.Lock()
		m.byID[c.id].Forwards = map[string]string{"8080": c.addr}
		m.byID[c.id].State = api.StateRunning
		m.mu.Unlock()
		snap, err := m.CommitWith(context.Background(), c.id, "dorado", CommitOptions{SkipReady: true})
		if err != nil {
			t.Fatal(err)
		}
		if snap.SinAgente != c.quiere {
			t.Errorf("%s: SinAgente=%v, quería %v", c.addr, snap.SinAgente, c.quiere)
		}
		leido, err := m.loadSnapshot("dorado")
		if err != nil || leido.SinAgente != c.quiere {
			t.Errorf("%s: en disco SinAgente=%v err=%v, quería %v", c.addr, leido != nil && leido.SinAgente, err, c.quiere)
		}
	}
}

// Una instancia de un dorado guardado sin agente no llama al resync; una de
// uno con agente (o anterior al campo) sí.
func TestResyncInstanciaRespetaSinAgente(t *testing.T) {
	var n atomic.Int32
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "resync") {
			n.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	ctx := context.Background()
	m.resyncInstancia(ctx, id, &api.Snapshot{Name: "a", SinAgente: true})
	if n.Load() != 0 {
		t.Fatalf("con SinAgente se llamó al resync %d veces", n.Load())
	}
	m.resyncInstancia(ctx, id, &api.Snapshot{Name: "b"})
	if n.Load() != 1 {
		t.Fatalf("sin el campo se llamó %d veces, quería 1", n.Load())
	}
}

// El sello de un Freeze lleva la misma marca y se lee tras reiniciar.
func TestSelloGuardaSinAgente(t *testing.T) {
	for _, v := range []bool{false, true} {
		dir := t.TempDir()
		for _, f := range []string{"snap.file", "mem.file"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := volcadoEnCurso(dir); err != nil {
			t.Fatal(err)
		}
		if err := sellarVolcado(dir, "", "", "", v); err != nil {
			t.Fatal(err)
		}
		if leerSello(dir).SinAgente != v {
			t.Errorf("sello SinAgente != %v", v)
		}
	}
}
