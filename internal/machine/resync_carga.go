package machine

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// Resync bajo carga (H2/H3): el plazo crece con los resync en vuelo, un thaw
// cuyo resync falla no se rinde a la primera, y cada fallo deja una línea con
// el estado de las vCPU del invitado y de su tap0.

const (
	// resyncPlazoCargaPaso es lo que se suma al plazo por cada resync en vuelo
	// por encima de NumCPU; resyncPlazoCargaMax, el techo.
	resyncPlazoCargaPaso = 250 * time.Millisecond
	resyncPlazoCargaMax  = 20 * time.Second
)

// resyncEnVuelo cuenta los resync con petición en curso.
var resyncEnVuelo atomic.Int64

// plazoResyncCarga es el plazo de un resync con agente conocido: 6 s más
// 250 ms por cada resync en vuelo por encima de ncpu, con techo de 20 s. El
// contexto de la petición, si trae un plazo menor, manda igualmente.
func plazoResyncCarga(enVuelo, ncpu int) time.Duration {
	p := resyncPlazoConAgente
	if extra := enVuelo - ncpu; extra > 0 {
		p += time.Duration(extra) * resyncPlazoCargaPaso
	}
	return min(p, resyncPlazoCargaMax)
}

// sinAgenteAlCongelar dice si Freeze debe apuntar que no hay agente: solo con
// un "cerrado" firme (RST). Un plazo agotado (sondeoNoSe) con el host cargado
// no prueba nada, y apuntarlo dejaba al thaw sin resync y al invitado con el
// reloj de cuando se congeló.
func (m *Manager) sinAgenteAlCongelar(ctx context.Context, id string) bool {
	return m.sondearAgente(ctx, id) == sondeoCerrado
}

// resyncThawAviso es lo que se añade al evento cuando el thaw entrega la
// máquina sin haberla puesto en hora.
const resyncThawAviso = "; WARNING: guest clock/RNG not resynced, retrying in the background"

// resyncTrasThaw es el resync de un thaw. Si falla y la máquina tiene
// AllowExec o agente conocido (un agente que debía contestar), lo repite con
// el plazo largo. Si aun así falla devuelve el aviso para el evento: la
// máquina se entrega igual (es del usuario), y quien llama lanza
// resyncThawReintentos.
func (m *Manager) resyncTrasThaw(ctx context.Context, id string, allowExec bool) (resultadoResync, string) {
	res := m.resyncGuest(ctx, id, "", api.ResyncThaw)
	if !res.fallo {
		return res, ""
	}
	if allowExec || m.agenteDe(id) != nil {
		log.Printf("%s: the resync after thaw failed; retrying it with the long timeout", shortID(id))
		if res = m.resyncGuestCon(ctx, id, "", api.ResyncThaw, true); !res.fallo {
			return res, ""
		}
	}
	return res, resyncThawAviso
}

// resyncThawEspera es el retroceso del resync de thaw en segundo plano (unos
// 3 minutos en total); los tests lo acortan.
var resyncThawEspera = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second,
	30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
}

// resyncThawReintentos repite en segundo plano, con retroceso, el resync de
// una máquina descongelada que se entregó sin él, hasta que entra, la máquina
// deja de estar en la ejecución ej, o se acaba la lista.
func (m *Manager) resyncThawReintentos(id string, ej ejecucion, estricto bool) {
	for i, espera := range resyncThawEspera {
		time.Sleep(espera)
		res, hecho := m.resyncEnFondoCon(id, ej, api.ResyncThaw, estricto)
		if !hecho {
			return
		}
		if !res.fallo {
			log.Printf("%s: background resync after thaw succeeded (attempt %d)", shortID(id), i+1)
			return
		}
	}
	log.Printf("%s: background resync after thaw gave up after %d attempts: the guest keeps the clock of the freeze", shortID(id), len(resyncThawEspera))
}

// procRaiz es dónde está /proc; los tests lo apuntan a un directorio falso.
var procRaiz = "/proc"

// leerRedInvitado devuelve, en una línea, el vecino y los contadores del tap0
// de la máquina id (dentro de su netns); "" si no se puede. Inyectable.
var leerRedInvitado = func(id string) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	ns := "kl-" + knet.Corto(id)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	salida := func(args ...string) string {
		b, err := exec.CommandContext(ctx, "ip", append([]string{"-n", ns}, args...)...).Output()
		if err != nil {
			return "?"
		}
		return strings.Join(strings.Fields(string(b)), " ")
	}
	return fmt.Sprintf("neigh[%s] link[%s]", salida("neigh", "show", "dev", knet.TapName), salida("-s", "link", "show", knet.TapName))
}

// muestraVCPU suma el tiempo de CPU y de espera en cola (ns) de los hilos
// vCPU del proceso pid, de /proc/<pid>/task/*/schedstat. Si ningún hilo se
// llama "...vcpu...", suma todos (otros VMM). ok es falso si no hay proceso.
func muestraVCPU(pid int) (n int, cpu, espera int64, ok bool) {
	tareas, err := filepath.Glob(filepath.Join(procRaiz, strconv.Itoa(pid), "task", "*"))
	if err != nil || len(tareas) == 0 {
		return 0, 0, 0, false
	}
	var cpuT, esperaT int64
	var nT int
	leer := func(dir string) (c, e int64, ok bool) {
		b, err := os.ReadFile(filepath.Join(dir, "schedstat"))
		if err != nil {
			return 0, 0, false
		}
		f := strings.Fields(string(b))
		if len(f) < 2 {
			return 0, 0, false
		}
		c, err1 := strconv.ParseInt(f[0], 10, 64)
		e, err2 := strconv.ParseInt(f[1], 10, 64)
		return c, e, err1 == nil && err2 == nil
	}
	for _, d := range tareas {
		c, e, bien := leer(d)
		if !bien {
			continue
		}
		cpuT, esperaT, nT = cpuT+c, esperaT+e, nT+1
		comm, _ := os.ReadFile(filepath.Join(d, "comm"))
		if strings.Contains(string(comm), "vcpu") {
			cpu, espera, n = cpu+c, espera+e, n+1
		}
	}
	if n == 0 {
		return nT, cpuT, esperaT, nT > 0
	}
	return n, cpu, espera, true
}

// diagnosticoInvitado es la línea de evidencia de un resync fallido: tiempo
// de CPU y de espera de las vCPU del proceso pid (total y lo ocurrido durante
// espera(), que deja pasar un rato entre dos muestras) y lo que diga red
// (vecino y contadores del tap0). Una vCPU sin avanzar en la ventana es un
// invitado que no corre; avanzando, uno que corre pero no atiende la red.
func diagnosticoInvitado(pid int, red string, espera func()) string {
	n, cpu0, esp0, ok := muestraVCPU(pid)
	if !ok {
		return fmt.Sprintf("no /proc data for pid %d net=%s", pid, red)
	}
	espera()
	_, cpu1, esp1, _ := muestraVCPU(pid)
	ms := func(ns int64) time.Duration { return time.Duration(ns).Round(time.Millisecond) }
	return fmt.Sprintf("vcpus=%d cpu_total=%v wait_total=%v cpu_delta=%v wait_delta=%v net=%s",
		n, ms(cpu1), ms(esp1), ms(cpu1-cpu0), ms(esp1-esp0), red)
}

// diagEspera es la ventana entre las dos muestras de la vCPU.
var diagEspera = func() { time.Sleep(250 * time.Millisecond) }

// diagnosticarResync registra la línea de evidencia de un resync que acaba de
// fallar con err: invitados en vuelo, vCPU y tap0. Solo log.
func (m *Manager) diagnosticarResync(id, name string, pid, enVuelo int, err error) {
	if pid <= 0 {
		log.Printf("%s: resync failed (%v); in_flight=%d, no pid to inspect", name, err, enVuelo)
		return
	}
	log.Printf("%s: resync failed (%v); in_flight=%d %s", name, err, enVuelo,
		diagnosticoInvitado(pid, leerRedInvitado(id), diagEspera))
}
