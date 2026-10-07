package machine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// llamadaFC es una petición HTTP que recibió un fcFalso, con el cuerpo ya
// leído: lo que necesita un test para comprobar qué le pidió el código al VMM,
// con qué método y en qué orden.
type llamadaFC struct {
	Metodo string
	Ruta   string
	Cuerpo []byte
}

// fallosFC es el fallo inyectado en una ruta: el código HTTP que se contesta
// y, si no está vacío, el fault_message que fc.Client sabe extraer de un
// error (ver internal/fc/client.go: do()).
type fallosFC struct {
	codigo       int
	faultMessage string
}

// fcFalso es un servidor HTTP falso de la API de Firecracker sobre un socket
// unix: exactamente lo que fc.Client espera al otro lado (ver hallazgo 3 del
// plan). A diferencia de firecrackerFalso (internal/fc/client_test.go), que
// sirve una única respuesta fija para un test de una sola llamada, este:
//
//   - guarda el REGISTRO de todas las llamadas, en orden (para probar, por
//     ejemplo, que Commit pausa antes de volcar y reanuda después, o que un
//     Freeze fallido no llegó a mandar el snapshot);
//   - deja fallar rutas concretas (M-18: tests de inyección de fallos —
//     "¿qué hace Commit si PatchDrive falla al reapuntar el disco?");
//   - avisa a un hook justo antes de contestar cada petición, para sincronizar
//     el test con el código bajo prueba sin sondear con sleeps (el caso que lo
//     motiva: cancelar el ctx del test exactamente cuando llega el
//     PUT /snapshot/create, para probar la limpieza de un Commit cancelado).
type fcFalso struct {
	// Sock es la ruta del socket unix; fc.New(f.Sock) abre un cliente contra
	// este servidor.
	Sock string

	mu       sync.Mutex
	llamadas []llamadaFC
	fallos   map[string]fallosFC
	hook     func(metodo, ruta string)
	// globo, si no es nil, es lo que contesta GET /balloon/statistics; un
	// PATCH /balloon le pone ActualMiB a lo pedido, como si el invitado lo
	// entregara al instante.
	globo *fc.BalloonStats

	srv *http.Server
}

// nuevoFcFalso levanta el servidor falso y lo cierra al acabar el test.
//
// El socket va en su propio temporal corto bajo /tmp y NO en el directorio de
// la máquina (m.dir(id)): los sockets unix tienen un límite de unos 104-108
// bytes en sun_path (ver internal/fc/dial.go), y el TempDir de los tests es
// demasiado largo para eso.
func nuevoFcFalso(t *testing.T) *fcFalso {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kfc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}

	f := &fcFalso{Sock: sock, fallos: map[string]fallosFC{}}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.atender)}
	go f.srv.Serve(ln)
	// Server.Close cierra también el listener: no hace falta guardarlo aparte.
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *fcFalso) atender(w http.ResponseWriter, r *http.Request) {
	cuerpo, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.llamadas = append(f.llamadas, llamadaFC{Metodo: r.Method, Ruta: r.URL.Path, Cuerpo: cuerpo})
	fallo, hayFallo := f.fallos[r.Method+" "+r.URL.Path]
	hook := f.hook
	var globo fc.BalloonStats
	if f.globo != nil {
		if r.Method == http.MethodPatch && r.URL.Path == "/balloon" {
			var p struct {
				AmountMiB int `json:"amount_mib"`
			}
			if json.Unmarshal(cuerpo, &p) == nil {
				f.globo.ActualMiB, f.globo.TargetMiB = p.AmountMiB, p.AmountMiB
			}
		}
		globo = *f.globo
	}
	f.mu.Unlock()

	// Fuera del lock: el hook puede tardar (por ejemplo, esperar a que el test
	// cancele un ctx), y mientras tanto no hay razón para bloquear a quien
	// consulta el registro de llamadas desde otra goroutine.
	if hook != nil {
		hook(r.Method, r.URL.Path)
	}

	if hayFallo {
		w.WriteHeader(fallo.codigo)
		if fallo.faultMessage != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": fallo.faultMessage})
		}
		return
	}

	if r.URL.Path == "/balloon/statistics" {
		// Sin un cuerpo, BalloonStats no tiene JSON que decodificar. Todo a
		// cero vale como "el invitado aún no ha reportado" (ver
		// estadisticasDesconocidas en plataforma_fc.go).
		_ = json.NewEncoder(w).Encode(globo)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fallar hace que metodo+ruta conteste codigo (y faultMessage, si no está
// vacío) desde ya y hasta que se retire con dejarDeFallar. ruta es la que de
// verdad pide fc.Client, tal cual (p. ej. "/drives/rootfs", "/vm").
func (f *fcFalso) fallar(metodo, ruta string, codigo int, faultMessage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallos[metodo+" "+ruta] = fallosFC{codigo: codigo, faultMessage: faultMessage}
}

// dejarDeFallar retira el fallo inyectado en metodo+ruta, si había alguno.
func (f *fcFalso) dejarDeFallar(metodo, ruta string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fallos, metodo+" "+ruta)
}

// enGancho instala el hook, o lo retira con nil.
func (f *fcFalso) enGancho(hook func(metodo, ruta string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

// todas devuelve el registro completo de llamadas recibidas, en el orden en
// que llegaron.
func (f *fcFalso) todas() []llamadaFC {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llamadaFC(nil), f.llamadas...)
}

// llamadasA filtra el registro a las llamadas hechas a metodo+ruta.
func (f *fcFalso) llamadasA(metodo, ruta string) []llamadaFC {
	var out []llamadaFC
	for _, l := range f.todas() {
		if l.Metodo == metodo && l.Ruta == ruta {
			out = append(out, l)
		}
	}
	return out
}

// cliente abre un fc.Client de verdad contra este servidor falso.
func (f *fcFalso) cliente() *fc.Client { return fc.New(f.Sock) }

// maquinaConVMM registra en m una máquina con un VMM falso COMPLETO detrás:
// el proceso que liveVMs y sweepOrphanVMMs encuentran en /proc (vmmFalso,
// definido junto a los tests del ciclo de vida) y el servidor HTTP que habla
// la API de Firecracker de verdad (fcFalso). Es lo que hace falta para probar
// Run, Freeze, Thaw, Squeeze o Commit contra un fc.Client real, sin KVM, sin
// jailer y sin el binario de firecracker instalado.
//
// Los dos VMM falsos usan sockets DISTINTOS a propósito: el de vmmFalso solo
// existe en la línea de comandos del proceso, para que el barrido de
// huérfanos lo reconozca; el de fcFalso es el que de verdad escucha HTTP. Un
// PID falso más un socket real de la API es toda la superficie que el
// gestor necesita para no distinguir esto de un firecracker de verdad.
func maquinaConVMM(t *testing.T, m *Manager, id string) (mc *api.Machine, falso *fcFalso, muerto <-chan struct{}) {
	t.Helper()
	_, muerto = vmmFalso(t, m, id)
	falso = nuevoFcFalso(t)
	mc = m.addForTest(id)
	m.mu.Lock()
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	return mc, falso, muerto
}

// A partir de aquí, pruebas del propio arnés: si fcFalso mintiera sobre el
// registro de llamadas, los fallos inyectados o el hook, cualquier test que
// construido sobre él en los nodos siguientes (N1, N2, N3, N10) estaría de
// pie sobre algo que no se puede confiar.

// TestFcFalsoRegistraLasLlamadasEnOrden comprueba el registro de llamadas: la
// pieza que permite a un test futuro afirmar "Commit pausó antes de volcar y
// reanudó después", no solo "las tres llamadas pasaron alguna vez".
func TestFcFalsoRegistraLasLlamadasEnOrden(t *testing.T) {
	f := nuevoFcFalso(t)
	c := f.cliente()
	ctx := context.Background()

	if err := c.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := c.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got := f.todas()
	if len(got) != 2 {
		t.Fatalf("llamadas registradas = %d, want 2: %+v", len(got), got)
	}
	if got[0].Ruta != "/vm" || !strings.Contains(string(got[0].Cuerpo), "Paused") {
		t.Errorf("primera llamada = %+v, quería PATCH /vm con Paused", got[0])
	}
	if got[1].Ruta != "/vm" || !strings.Contains(string(got[1].Cuerpo), "Resumed") {
		t.Errorf("segunda llamada = %+v, quería PATCH /vm con Resumed", got[1])
	}
}

// TestFcFalsoInyectaFalloPorRuta: solo la ruta marcada falla; el resto sigue
// contestando 2xx. Es la base de M-18 (tests de inyección de fallos): sin
// esto no se puede simular "PatchDrive falló al reapuntar el disco, pero
// Resume debe seguir yendo bien" con un solo servidor falso.
func TestFcFalsoInyectaFalloPorRuta(t *testing.T) {
	f := nuevoFcFalso(t)
	f.fallar(http.MethodPatch, "/drives/rootfs", http.StatusBadRequest, "disk is busy")
	c := f.cliente()
	ctx := context.Background()

	err := c.PatchDrive(ctx, "rootfs", "/x")
	if err == nil {
		t.Fatal("PatchDrive con fallo inyectado no devolvió error")
	}
	if !strings.Contains(err.Error(), "disk is busy") {
		t.Errorf("el fault_message inyectado no llegó al error: %v", err)
	}

	// Otra ruta no marcada sigue contestando bien.
	if err := c.Resume(ctx); err != nil {
		t.Errorf("una ruta sin fallo inyectado también falló: %v", err)
	}

	// Retirado el fallo, la misma ruta vuelve a ir bien.
	f.dejarDeFallar(http.MethodPatch, "/drives/rootfs")
	if err := c.PatchDrive(ctx, "rootfs", "/x"); err != nil {
		t.Errorf("PatchDrive tras dejarDeFallar: %v", err)
	}
}

// TestFcFalsoHookSeLlamaAntesDeContestar: el hook es lo que permite
// sincronizar el test con el código bajo prueba sin sleeps —por ejemplo,
// cancelar un ctx justo cuando llega la llamada que se quiere interrumpir.
func TestFcFalsoHookSeLlamaAntesDeContestar(t *testing.T) {
	f := nuevoFcFalso(t)
	ctx, cancel := context.WithCancel(context.Background())

	var vistos []string
	var mu sync.Mutex
	f.enGancho(func(metodo, ruta string) {
		mu.Lock()
		vistos = append(vistos, metodo+" "+ruta)
		mu.Unlock()
		if ruta == "/snapshot/create" {
			cancel() // como cancelaría el ctx del test a mitad de un Commit
		}
	})

	c := f.cliente()
	if err := c.Snapshot(context.Background(), "/s", "/m"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	mu.Lock()
	n := len(vistos)
	mu.Unlock()
	if n != 1 || vistos[0] != "PUT /snapshot/create" {
		t.Fatalf("el hook vio %v, quería una llamada a PUT /snapshot/create", vistos)
	}
	if ctx.Err() == nil {
		t.Fatal("el hook no llegó a cancelar el ctx del test")
	}
}

// TestMaquinaConVMMDejaUnClienteQueFunciona comprueba la pieza que van a usar
// los nodos siguientes: tras maquinaConVMM, m.socket[id] apunta a un servidor
// que de verdad contesta, y la máquina ya está en byID como cualquier otra.
func TestMaquinaConVMMDejaUnClienteQueFunciona(t *testing.T) {
	m := newTestManager(t)
	id := "fc0000000000001"

	mc, falso, muerto := maquinaConVMM(t, m, id)

	if mc.ID != id {
		t.Fatalf("addForTest devolvió la máquina %q, quería %q", mc.ID, id)
	}
	if !sigueVivo(muerto) {
		t.Fatal("el VMM falso ya está muerto justo tras crearlo")
	}

	m.mu.RLock()
	sock := m.socket[id]
	m.mu.RUnlock()
	if sock != falso.Sock {
		t.Fatalf("m.socket[id] = %q, quería el socket del fcFalso (%q)", sock, falso.Sock)
	}

	if err := falso.cliente().Ping(context.Background()); err != nil {
		t.Fatalf("Ping contra el socket registrado: %v", err)
	}
}
