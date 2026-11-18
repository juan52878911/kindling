package machine

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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

// S6: un plazo agotado en el sondeo (host cargado) no prueba que no haya
// agente. Antes el dorado nacía sin_agente y todas sus copias se saltaban el
// resync; ahora solo un RST lo hace, y entonces lo avisa.
func TestCommitSinAgenteSoloConRST(t *testing.T) {
	plazo := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	orig := dialAgente
	t.Cleanup(func() { dialAgente = orig })
	for _, c := range []struct {
		que     string
		err     error
		quiere  bool
		ninguno bool // ni sin_agente ni con_agente
	}{
		{"plazo agotado", plazo, false, true},
		{"RST", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true, false},
	} {
		dialAgente = func(string, time.Duration) (net.Conn, error) { return nil, c.err }
		m, id := plantillaSinAgente(t)
		m.mu.Lock()
		m.byID[id].Forwards = map[string]string{"8080": dirSinNadie}
		m.mu.Unlock()
		snap, err := m.CommitWith(context.Background(), id, "dorado", CommitOptions{SkipReady: true})
		if err != nil {
			t.Fatal(err)
		}
		if snap.SinAgente != c.quiere || (c.ninguno && snap.ConAgente) {
			t.Errorf("%s: SinAgente=%v ConAgente=%v, quería SinAgente=%v", c.que, snap.SinAgente, snap.ConAgente, c.quiere)
		}
		if avisa := slices.Contains(snap.Warnings, avisoSinAgente); avisa != c.quiere {
			t.Errorf("%s: aviso de sin agente=%v, quería %v", c.que, avisa, c.quiere)
		}
	}
}

// S6: un fork o un grafo no sondea; si el agente ya contestó a /healthz,
// el dorado dice con_agente y una copia sin resync no se entrega.
func TestCommitSinSondeoConAgenteConocido(t *testing.T) {
	m, id := plantillaSinAgente(t)
	m.mu.Lock()
	m.byID[id].Agent = &api.GuestAgent{}
	m.mu.Unlock()
	snap, err := m.commit(context.Background(), id, "dorado", false, nil, false, false, agenteSinSondear)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ConAgente || snap.SinAgente {
		t.Errorf("ConAgente=%v SinAgente=%v, quería con_agente", snap.ConAgente, snap.SinAgente)
	}
}

// plantillaSinAgente es una máquina en marcha lista para un commit de
// prueba (plantillaParaCommit con el volcado falso).
func plantillaSinAgente(t *testing.T) (*Manager, string) {
	t.Helper()
	puedePerforar(t)
	m := newTestManager(t)
	id := "c0aa170000000041"
	falso, _ := plantillaParaCommit(t, m, id)
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			volcadoFalso(m, id, dir)
		}
	})
	return m, id
}

// Fuera de commit: el sondeo distingue los tres casos.
func TestSondearAgenteTresEstados(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	m := &Manager{byID: map[string]*api.Machine{
		"si": {ID: "si", Forwards: map[string]string{"8080": ln.Addr().String()}},
		"no": {ID: "no", Forwards: map[string]string{"8080": dirSinNadie}},
	}, socket: map[string]string{}}
	ctx := context.Background()
	if e := m.sondearAgente(ctx, "si"); e != sondeoAbierto {
		t.Errorf("si: %v", e)
	}
	if e := m.sondearAgente(ctx, "no"); e != sondeoCerrado {
		t.Errorf("no: %v", e)
	}
	orig := dialAgente
	t.Cleanup(func() { dialAgente = orig })
	dialAgente = func(string, time.Duration) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	if e := m.sondearAgente(ctx, "no"); e != sondeoNoSe {
		t.Errorf("plazo: %v", e)
	}
	if m.agenteEscucha(ctx, "no") {
		t.Error("agenteEscucha dio sí con un plazo agotado")
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
