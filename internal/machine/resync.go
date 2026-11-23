package machine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/guesthttp"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// Tras cada restauración el invitado despierta con la memoria del snapshot: el
// reloj de pared parado en el instante del volcado y el CSPRNG del kernel
// idéntico en todas las instancias del mismo dorado. resyncGuest le manda la
// hora del host y entropía fresca (POST /resync del agente, ver pkg/guest).
//
// En Linux, VMGenID ya hace resembrar al kernel, pero el reloj seguía parado;
// en macOS no hay VMGenID y dos réplicas devolvían los mismos aleatorios.

// resyncPlazo acota la llamada entera. Tras un Resume el agente contesta en
// milisegundos; lo que tarde más es un invitado sin agente o colgado, y la
// restauración no debe esperarle.
const resyncPlazo = 2 * time.Second

// resyncReintento es cuánto se insiste si la conexión falla: justo tras
// reanudar, la pila de red del invitado (o el reenvío de kling-vz) puede tardar
// un instante en volver. Corto a propósito: una máquina sin agente lo paga en
// cada restauración.
const resyncReintento = 250 * time.Millisecond

// resyncPlazoConAgente sustituye a los dos anteriores cuando el dorado se
// guardó con el agente escuchando (api.Snapshot.ConAgente): ahí no hay
// máquina sin agente a la que no penalizar, y quien no contesta aún está
// despertando. Medido en el CT 105 con 10-20 restauraciones a la vez en 4
// núcleos: el ARP hacia el invitado tardaba más de 250 ms ("no route to
// host") y la copia se descartaba con el agente sano.
const resyncPlazoConAgente = 6 * time.Second

// Qué pasa cuando no hay agente. En Linux, justo tras restaurar, el primer SYN
// a un invitado sin nadie en el puerto suele perderse (o el invitado aún está
// arrancando y no contesta): cada restauración pagaría el plazo entero. Se
// evita con lo que se sabe de ANTES de restaurar:
//
//   - Thaw: Freeze sondea el puerto del agente justo antes de pausar (ver
//     agenteEscucha) y lo apunta en resyncSinAgente. Si nadie escuchaba, el
//     thaw no lo intenta: es la continuación de UNA máquina, sin clones con
//     los que compartir el CSPRNG, y lo único que se pierde es el reloj de un
//     invitado que no tiene quién lo ponga.
//   - run -from: un dorado se congela comprobando que sirve (kling commit), y
//     un "nadie escucha" definitivo —RST, o el cierre del reenvío en macOS—
//     se recuerda POR SNAPSHOT (nombre y fecha: su memoria no cambia) durante
//     resyncSinAgenteTTL. Un plazo agotado no se recuerda: puede ser un host
//     cargado, y saltarse el resync ahí sería repartir el mismo CSPRNG.
const resyncSinAgenteTTL = 10 * time.Minute

// recordarSinAgente apunta que la restauración de clave no tiene agente
// hasta ahora+resyncSinAgenteTTL, y de paso retira las claves de snapshot
// caducadas. Antes no se borraban nunca: una por dorado sin agente, y cada
// fork es un dorado nuevo, así que el mapa crecía con la vida del daemon.
// Las de thaw (claveThaw, sin caducidad) las gestionan Freeze, Thaw y rm.
func (m *Manager) recordarSinAgente(clave string, ahora time.Time) {
	m.resyncSinAgente.Range(func(k, v any) bool {
		if c, _ := k.(string); strings.HasPrefix(c, "s:") {
			if hasta, ok := v.(time.Time); ok && !ahora.Before(hasta) {
				m.resyncSinAgente.Delete(k)
			}
		}
		return true
	})
	m.resyncSinAgente.Store(clave, ahora.Add(resyncSinAgenteTTL))
}

// claveThaw y claveSnapshot son las claves de resyncSinAgente.
func claveThaw(id string) string { return "m:" + id }
func claveSnapshot(s *api.Snapshot) string {
	return "s:" + s.Name + "@" + s.CreatedAt.UTC().Format(time.RFC3339Nano)
}

// resyncInstancia es el resync de una instancia nacida de snap. Si la
// plantilla se guardó sin agente (snap.SinAgente, en su meta.json firmado), no
// se intenta: sobrevive a reinicios del daemon. Las anteriores al campo siguen
// por resyncGuest, que recuerda en memoria lo que descubra.
func (m *Manager) resyncInstancia(ctx context.Context, id string, snap *api.Snapshot) resultadoResync {
	return m.resyncInstanciaOpc(ctx, id, snap, opcionesResync{})
}

// opcionesResync afina una llamada a resyncGuestCon. El valor cero es el
// comportamiento de siempre.
type opcionesResync struct {
	// pid es el del VMM de la máquina, si quien llama lo conoce: runFrom no lo
	// escribe en la máquina hasta entregarla, y el diagnóstico de un fallo leía
	// un 0 ("no pid to inspect").
	pid int
	// plazo, si es >0, acota el intento entero (por debajo del habitual).
	plazo time.Duration
	// diagAsync saca el diagnóstico de un fallo a otra goroutine
	// (resultadoResync.diag avisa de cuándo acaba).
	diagAsync bool
}

func (m *Manager) resyncInstanciaOpc(ctx context.Context, id string, snap *api.Snapshot, opc opcionesResync) resultadoResync {
	if snap.SinAgente {
		return resultadoResync{}
	}
	if snap.ConAgente || snap.AllowExec {
		// Se guardó con el agente escuchando (o con AllowExec, que lo exige:
		// sin agente no hay exec): si ahora nadie contesta, es un fallo (S4),
		// y nada se recuerda.
		return m.resyncGuestOpc(ctx, id, "", api.ResyncInstance, true, opc)
	}
	return m.resyncGuestOpc(ctx, id, claveSnapshot(snap), api.ResyncInstance, false, opc)
}

// resyncCopia es el resync de una copia recién restaurada de snap (run -from,
// fork), cuyo VMM es pid. Un agente que debía contestar y no lo hizo dejaba
// antes la copia entregada igual, con el reloj, el machine-id y la generación
// del dorado, y en macOS (sin VMGenID) el CSPRNG de sus hermanas. Ahora no se
// entrega: un solo intento, con plazo corto (plazoCopiaViva), y si falla el
// error envuelve errCopiaMuerta para que runFrom la descarte y restaure OTRA
// copia, en vez de insistir 6 s más a la misma (que no contestó nunca en los 22
// casos medidos). Un agente anterior o una imagen sin agente no son fallo
// (resultadoResync.fallo).
func (m *Manager) resyncCopia(ctx context.Context, id string, snap *api.Snapshot, pid int) (resultadoResync, error) {
	res := m.resyncInstanciaOpc(ctx, id, snap, opcionesResync{pid: pid, plazo: plazoCopiaViva(), diagAsync: true})
	if res.fallo {
		copiasMuertas.Add(1)
		return res, fmt.Errorf("%w: the guest agent did not resync after the restore, so this copy would keep the clock and random state of the snapshot", errCopiaMuerta)
	}
	return res, nil
}

// resyncClient no reutiliza conexiones: cada llamada va a una microVM recién
// restaurada, y una conexión ociosa guardada hacia ella solo retendría un
// descriptor hasta que el invitado muera.
var resyncClient = guesthttp.New(nil, 0)

// ctxClienteAgente lleva en el contexto un cliente hacia el agente de UNA
// operación (clienteDeOperacion): el resync y el POST /machine/start de una
// copia por imagen van así por la misma conexión (G-05).
type ctxClienteAgente struct{}

// clienteDeOperacion da un contexto con un cliente hacia el agente que guarda
// UNA conexión ociosa, y la función que la cierra al acabar la operación. Así
// no queda ningún descriptor hacia la microVM fuera de la operación (lo que
// evita resyncClient), y el segundo viaje no paga otra conexión con el
// invitado recién despierto.
func clienteDeOperacion(ctx context.Context) (context.Context, func()) {
	t := &http.Transport{MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 5 * time.Second}
	return context.WithValue(ctx, ctxClienteAgente{}, guesthttp.New(t, 0)), t.CloseIdleConnections
}

// clienteAgenteDe es el cliente de la operación de ctx, o def si no lleva.
func clienteAgenteDe(ctx context.Context, def *http.Client) *http.Client {
	if c, ok := ctx.Value(ctxClienteAgente{}).(*http.Client); ok {
		return c
	}
	return def
}

// resultadoResync es lo que dio resyncGuest: cuánto tardó, si el agente lo
// aplicó, lo que contestó de su "listo" (nil si nada), y si falló con un
// agente que debía contestar. fallo es falso cuando no se intentó (sin
// dirección, un agente que no sabe hacerlo, una imagen que se sabe sin
// agente) o cuando contestó que no sabe (404) o nadie escuchaba: ahí no hay
// ganchos que lanzar después (ver ganchosTrasResyncFallido).
type resultadoResync struct {
	took  time.Duration
	ok    bool
	listo *api.GuestReady
	fallo bool
	// diag se cierra cuando acaba el diagnóstico del fallo, que con
	// opcionesResync.diagAsync va en otra goroutine; nil si no hay.
	diag <-chan struct{}
}

// resyncGuest resincroniza el invitado id. No devuelve error: un agente viejo
// (404), una máquina sin agente o uno que falla dejan la máquina como antes de
// esto —utilizable, con el reloj parado— y se avisa una vez por imagen.
// clave es la del snapshot del que se restauró (claveSnapshot) para recordar
// que no tiene agente, o "" para no recordar nada.
func (m *Manager) resyncGuest(ctx context.Context, id, clave, kind string) resultadoResync {
	return m.resyncGuestCon(ctx, id, clave, kind, false)
}

// resyncGuestCon es resyncGuest sabiendo, con conAgente, que el invitado
// tenía agente: entonces "nadie escucha" también es un fallo.
func (m *Manager) resyncGuestCon(ctx context.Context, id, clave, kind string, conAgente bool) resultadoResync {
	return m.resyncGuestOpc(ctx, id, clave, kind, conAgente, opcionesResync{})
}

// resyncGuestOpc es resyncGuestCon con opc.
func (m *Manager) resyncGuestOpc(ctx context.Context, id, clave, kind string, conAgente bool, opc opcionesResync) resultadoResync {
	m.mu.RLock()
	mc := m.byID[id]
	var addr, image, name string
	var ag *api.GuestAgent
	var pid int
	if mc != nil && mc.Reachable() {
		addr, image, name, ag, pid = mc.Addr(api.GuestPort), mc.Image, mc.Name, mc.Agent, mc.PID
	}
	m.mu.RUnlock()
	if opc.pid > 0 {
		pid = opc.pid
	}
	if addr == "" {
		return resultadoResync{}
	}
	// El agente dijo en su /healthz lo que sabe hacer y resync no está: el
	// mismo aviso que daría el 404, sin la petición (agente.go).
	if ag.Lacks(api.GuestCapResync) {
		m.avisarResync(image, name, errResyncNoSoportado)
		return resultadoResync{}
	}
	if clave != "" {
		if hasta, ok := m.resyncSinAgente.Load(clave); ok && time.Now().Before(hasta.(time.Time)) {
			return resultadoResync{}
		}
	}

	enVuelo := resyncEnVuelo.Add(1)
	defer resyncEnVuelo.Add(-1)
	plazo, reintento := resyncPlazo, resyncReintento
	if conAgente {
		plazo = plazoResyncCarga(int(enVuelo), runtime.NumCPU())
		reintento = plazo
	}
	if opc.plazo > 0 {
		plazo = min(plazo, opc.plazo)
		reintento = min(reintento, plazo)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, plazo)
	defer cancel()
	res, err := resyncOnce(ctx, "http://"+addr, kind)
	for err != nil && errors.Is(err, errResyncConexion) && time.Since(start) < reintento {
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
		res, err = resyncOnce(ctx, "http://"+addr, kind)
	}
	took := time.Since(start)
	if err != nil && errors.Is(context.Cause(ctx), errResyncCede) {
		// Lo cortó resyncEnFondo para dejar paso a una operación de ciclo de
		// vida: no es un fallo del agente.
		return resultadoResync{took: took}
	}
	if err != nil {
		// Solo un rechazo de la conexión (RST: nadie en el puerto) se
		// recuerda. Un ECONNRESET o un EOF pueden ser un agente que sí estaba
		// y cortó bajo carga (S4): recordarlo saltaba el resync de las copias
		// siguientes durante resyncSinAgenteTTL.
		if clave != "" && errors.Is(err, errResyncRechazada) {
			m.recordarSinAgente(clave, time.Now())
		}
		m.avisarResync(image, name, err)
		// Un agente anterior o nadie escuchando no tienen ganchos que lanzar;
		// cualquier otro fallo (un plazo agotado, un 5xx) es de un agente que
		// sí debía contestar. Nadie escuchando también lo es si el dorado se
		// guardó con el agente escuchando.
		fallo := !errors.Is(err, errResyncNoSoportado) && (conAgente || !errors.Is(err, errResyncNadie))
		var diag <-chan struct{}
		if fallo {
			if opc.diagAsync {
				// Sobre copias de los datos (id, name, pid): sin m.mu ni el
				// cerrojo de ciclo de vida de la máquina.
				hecho := make(chan struct{})
				diag = hecho
				go func(n int) {
					defer close(hecho)
					m.diagnosticarResync(id, name, pid, n, err)
				}(int(enVuelo))
			} else {
				m.diagnosticarResync(id, name, pid, int(enVuelo), err)
			}
		}
		return resultadoResync{took: took, fallo: fallo, diag: diag}
	}
	if res.SkewMS > 1000 || res.SkewMS < -1000 {
		log.Printf("%s: guest clock was %s off; resynced in %s", name,
			(time.Duration(res.SkewMS) * time.Millisecond).Round(time.Millisecond), took.Round(time.Millisecond))
	}
	return resultadoResync{took: took, ok: true, listo: res.Ready}
}

// resyncArranque pone en hora el invitado id recién arrancado en frío, en
// cuanto su agente contesta (ag, de conocerAgenteY). El kernel toma la hora
// del RTC, que va a segundos, y le suma medio segundo: cada arranque nacía
// hasta ±0,5 s desfasado del host (medido en el M4 con kling-hv: de -492 a
// +416 ms en seis arranques). Solo a un agente que anuncia
// guest.CapResyncClock: uno anterior lo contaría como una restauración.
func (m *Manager) resyncArranque(id string, ag *api.GuestAgent) {
	if !ag.Has(guest.CapResyncClock) {
		return
	}
	if ej, ok := m.ejecucionDe(id); ok {
		m.resyncEnFondo(id, ej, guest.ResyncBoot)
	}
}

// resyncTrasReanudar pone en hora, en segundo plano, una máquina que se
// acaba de reanudar. Con kling-hv el contador virtual del invitado se para
// mientras está pausada y su reloj se queda atrás lo que duró la pausa; con
// KVM sigue al del host y el agente, al ver que no hay desfase, no lo toca
// (guest.ResyncResume). No retrasa la reanudación: quien despierta la
// máquina no espera a esto. A un agente anterior (sin guest.CapResyncClock)
// solo bajo kling-hv, donde el reloj sí se queda atrás: como api.ResyncThaw,
// lo único que entiende (le sube la generación, pero es eso o un reloj
// atrasado lo que duró la pausa).
func (m *Manager) resyncTrasReanudar(id string) {
	// La ejecución se toma ahora, con la reanudación recién hecha: si cuando
	// le toque al resync la máquina es otra (congelada y despertada, parada),
	// no se manda.
	ej, ok := m.ejecucionDe(id)
	if !ok {
		return
	}
	if m.agenteDe(id).Has(guest.CapResyncClock) {
		go m.resyncEnFondo(id, ej, guest.ResyncResume)
		return
	}
	if strings.HasPrefix(ej.vmm, "kling-hv") {
		go m.resyncEnFondo(id, ej, api.ResyncThaw)
	}
}

// ejecucion identifica una ejecución de una máquina: el mismo VMM (proceso y
// binario) desde el mismo arranque o reanudación.
type ejecucion struct {
	vmm     string
	pid     int
	startAt time.Time
}

// ejecucionDe es la ejecución de la máquina id si está running.
func (m *Manager) ejecucionDe(id string) (ejecucion, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	if mc == nil || mc.State != api.StateRunning {
		return ejecucion{}, false
	}
	ej := ejecucion{vmm: mc.VMM, pid: mc.PID}
	if mc.StartedAt != nil {
		ej.startAt = *mc.StartedAt
	}
	return ej, true
}

// errResyncCede es la causa con la que resyncEnFondo corta su resync para
// dejar paso a otra operación sobre la máquina.
var errResyncCede = errors.New("resync yields to a lifecycle operation")

// resyncCedeMax es lo que se deja acabar a un resync en segundo plano cuando
// otra operación (un Freeze, un Pause) espera el cerrojo de la máquina. Uno
// sano tarda milisegundos; uno más lento es un invitado colgado, y esperarle
// el resyncPlazo entero sería retrasar esa operación por nada.
const resyncCedeMax = 100 * time.Millisecond

// pasoCede es cada cuánto mira resyncEnFondo si alguien espera el cerrojo.
const pasoCede = 5 * time.Millisecond

// resyncEnFondo es el resync que nadie espera (tras arrancar o reanudar), con
// el cerrojo de ciclo de vida de la máquina tomado y solo si sigue en la
// ejecución ej. Sin el cerrojo, un Freeze justo tras la reanudación volcaba
// el invitado con el POST /resync a medias: al despertar, el agente acababa
// de aplicar una hora vieja. Con él, el Freeze espera a que acabe o, si
// tarda, lo corta (resyncCedeMax); y si el Freeze llegó antes, el resync ya
// no ve la misma ejecución y no se manda.
func (m *Manager) resyncEnFondo(id string, ej ejecucion, kind string) {
	m.resyncEnFondoCon(id, ej, kind, false)
}

// resyncEnFondoCon es resyncEnFondo con el resultado y, con estricto, el
// fallo de "nadie escucha" (agente conocido). hecho es falso si no se mandó
// porque la máquina ya no está en la ejecución ej.
func (m *Manager) resyncEnFondoCon(id string, ej ejecucion, kind string, estricto bool) (res resultadoResync, hecho bool) {
	defer m.lock(id)()
	if ahora, ok := m.ejecucionDe(id); !ok || ahora != ej || m.enTransicion(id) {
		return resultadoResync{}, false
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		t := time.NewTicker(pasoCede)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !m.lifecycle.esperan(id) {
				continue
			}
			select {
			case <-ctx.Done():
			case <-time.After(resyncCedeMax):
				cancel(errResyncCede)
			}
			return
		}
	}()
	return m.resyncGuestCon(ctx, id, "", kind, estricto), true
}

// enTransicion dice si la máquina id tiene una transición marcada
// (marcarTransicion): se va a congelar, parar o borrar, o un volcado fallido
// la dejó en pausa a la espera del vigilante.
func (m *Manager) enTransicion(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.transicion[id] != ""
}

// esperan dice si alguien, además de quien lo tiene, espera el cerrojo de id.
// refs cuenta a los que lo tienen y a los que lo esperan (ver cerrojo).
func (c *cerrojos) esperan(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[id]
	return e != nil && e.refs > 1
}

// kindSinRestaurar es el Restore del resync de una máquina que sigue siendo
// la misma (reanudada, o el host despertó): guest.ResyncResume si su agente
// lo entiende, que no lo cuenta como restauración; si no, api.ResyncThaw,
// lo único que entiende uno anterior.
func (m *Manager) kindSinRestaurar(id string) string {
	if m.agenteDe(id).Has(guest.CapResyncClock) {
		return guest.ResyncResume
	}
	return api.ResyncThaw
}

// avisarResync registra por qué no se resincronizó, una vez por imagen y
// motivo: con un agente viejo cada thaw daría el mismo aviso.
func (m *Manager) avisarResync(image, name string, err error) {
	// La clave es el TIPO de fallo, no su texto: el texto lleva el puerto de
	// cada máquina y volvería a avisar en cada restauración.
	tipo := "otro"
	switch {
	case errors.Is(err, errResyncNoSoportado):
		tipo = "viejo"
	case errors.Is(err, errResyncNadie):
		tipo = "nadie"
	case errors.Is(err, errResyncConexion):
		tipo = "conexion"
	}
	if _, ya := m.resyncAvisado.LoadOrStore(image+"\x00"+tipo, true); ya {
		return
	}
	if errors.Is(err, errResyncNoSoportado) {
		log.Printf("warning: %s (image %s): its guest agent predates %s, so instances restored "+
			"from the same snapshot share clock and RNG state. Rebuild the image with a current "+
			"kling-guest (kling image build), or refresh its MCP bridge (kling mcp refresh-bridge)",
			name, image, api.GuestResyncPath)
		return
	}
	if errors.Is(err, errResyncNadie) {
		log.Printf("%s (image %s): no guest agent listens on port %d, so its clock and RNG are not "+
			"resynced after restore", name, image, api.GuestPort)
		return
	}
	log.Printf("warning: %s (image %s): could not resync the guest clock and RNG after restore: %v",
		name, image, err)
}

var (
	errResyncConexion    = errors.New("guest agent not reachable")
	errResyncNoSoportado = errors.New("guest agent has no resync route")
	// errResyncNadie acompaña a errResyncConexion cuando nadie escucha en el
	// puerto del agente: el RST (ECONNREFUSED).
	errResyncNadie = errors.New("nothing listens on the guest agent port")
	// errResyncRechazada acompaña a errResyncNadie cuando fue un RST
	// (ECONNREFUSED): lo único que se recuerda por snapshot.
	errResyncRechazada = errors.New("connection refused")
)

func resyncOnce(ctx context.Context, base, kind string) (api.GuestResyncResult, error) {
	var out api.GuestResyncResult
	ini := time.Now()
	ent := make([]byte, api.GuestResyncEntropy)
	if _, err := rand.Read(ent); err != nil {
		return out, err
	}
	body, err := nuevoCuerpoResync(ent, kind)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+api.GuestResyncPath, body)
	if err != nil {
		return out, err
	}
	req.ContentLength = int64(body.n)
	req.Header.Set("Content-Type", "application/json")
	traza := trazaResyncNueva(base)
	req = traza.sobre(req)
	resp, err := clienteAgenteDe(ctx, resyncClient).Do(req)
	traza.fin(err)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return out, fmt.Errorf("%w: %w: %w: %v", errResyncConexion, errResyncNadie, errResyncRechazada, err)
		}
		// En Linux un ECONNRESET o un EOF son un agente que sí estaba y cortó
		// bajo carga: fallo, no "nadie" (solo el RST lo es). En macOS el
		// reenvío del VMM cierra así cuando no hay nadie al otro lado, así que
		// ahí sigue siendo "nadie"; un dorado con agente conocido (ConAgente o
		// AllowExec) falla igual, porque ese caso no acepta "nadie".
		if runtime.GOOS == "darwin" && (errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			return out, fmt.Errorf("%w: %w: %v", errResyncConexion, errResyncNadie, err)
		}
		return out, fmt.Errorf("%w: %v", errResyncConexion, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return out, errResyncNoSoportado
	case resp.StatusCode == http.StatusBadRequest && bytes.Contains(b, []byte("Mcp-Session-Id")):
		// El puente MCP anterior atiende "/" entero y contesta a /resync como a
		// una petición MCP sin sesión. Su texto ya no va a cambiar: es el de
		// binarios publicados.
		return out, errResyncNoSoportado
	case resp.StatusCode != http.StatusOK:
		return out, fmt.Errorf("guest answered %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	// Un cuerpo ilegible no invalida el resync: el agente ya contestó 200.
	_ = json.Unmarshal(b, &out)
	// Un agente anterior no manda critical_us: no hay reparto que anotar.
	if out.CriticalUS > 0 {
		// Lo manda el invitado: no puede pasar del tiempo que tardó la
		// petición entera, o falsearía las métricas globales.
		total := time.Since(ini)
		if max := total.Microseconds(); out.CriticalUS > max {
			out.CriticalUS = max
		}
		tel.resyncRepartido(total, out.CriticalUS)
		traza.reparto(out)
	}
	return out, nil
}

// cuerpoResync es el cuerpo de POST /resync con la hora tomada lo más tarde
// posible: cuando el cliente HTTP lo lee para mandarlo, con la conexión ya
// hecha. Lo que tarde la petición desde ese instante es el error que queda en
// el invitado; tomada antes de conectar, sumaba el connect entero (por el
// reenvío y el vsock de kling-vz/kling-hv), varios ms.
type cuerpoResync struct {
	ent  []byte
	kind string
	// n es la longitud que se anuncia (Content-Length): la del cuerpo hecho
	// al crearlo. UnixNano tiene 19 cifras de 2001 a 2286, así que el de
	// la primera lectura mide lo mismo; si no, se manda el primero.
	n     int
	listo []byte
	r     *bytes.Reader
}

func nuevoCuerpoResync(ent []byte, kind string) (*cuerpoResync, error) {
	b, err := json.Marshal(api.GuestResync{UnixNano: time.Now().UnixNano(), Entropy: ent, Restore: kind})
	if err != nil {
		return nil, err
	}
	return &cuerpoResync{ent: ent, kind: kind, n: len(b), listo: b}, nil
}

func (c *cuerpoResync) Read(p []byte) (int, error) {
	if c.r == nil {
		b, err := json.Marshal(api.GuestResync{UnixNano: time.Now().UnixNano(), Entropy: c.ent, Restore: c.kind})
		if err != nil || len(b) != c.n {
			b = c.listo
		}
		c.r = bytes.NewReader(b)
	}
	return c.r.Read(p)
}

// resyncNota es el añadido al mensaje del evento: deja ver en `kling events`
// cuánto cuesta el resync sin instrumentar nada.
func resyncNota(t time.Duration, ok bool) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf(", guest resynced in %.1f ms", float64(t.Microseconds())/1000)
}

// agenteEscucha dice si algo escucha en el puerto del agente de la máquina
// id, que corre. Lo usa Freeze justo antes de pausar (ver resyncSinAgenteTTL).
//
// Un plazo agotado cuenta como "nadie": un invitado en marcha con su agente
// contesta al SYN en microsegundos, y uno que no contesta en 200 ms es uno que
// aún arranca o sin red —el caso que tras restaurar costaba el plazo entero del
// resync—. Equivocarse aquí solo deja sin poner el reloj de UNA máquina: un
// thaw no tiene clones con los que compartir el CSPRNG.
func (m *Manager) agenteEscucha(ctx context.Context, id string) bool {
	return m.sondearAgente(ctx, id) == sondeoAbierto
}

// sondeoAgente es lo que contesta el puerto del agente: alguien escucha,
// nadie (un RST, o el "cerrado" del VMM) o nada claro (un plazo u otro fallo,
// que con el host cargado no prueba que no haya agente).
type sondeoAgente uint8

const (
	sondeoAbierto sondeoAgente = iota
	sondeoCerrado
	sondeoNoSe
)

// dialAgente es el SYN al puerto del agente; los tests lo sustituyen para
// simular un plazo agotado.
var dialAgente = func(addr string, plazo time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, plazo)
}

// sondearAgente sondea el puerto del agente de id distinguiendo los tres
// casos. Sin dirección dice "abierto", como siempre: ante la duda, sí.
func (m *Manager) sondearAgente(ctx context.Context, id string) sondeoAgente {
	if open, ok := m.sondearPuerto(ctx, id, api.GuestPort, false); ok {
		if open {
			return sondeoAbierto
		}
		return sondeoCerrado
	}
	m.mu.RLock()
	mc := m.byID[id]
	var addr string
	if mc != nil && mc.Reachable() {
		addr = mc.Addr(api.GuestPort)
	}
	m.mu.RUnlock()
	if addr == "" {
		return sondeoAbierto
	}
	conn, err := dialAgente(addr, 200*time.Millisecond)
	switch {
	case err == nil:
		conn.Close()
		return sondeoAbierto
	case errors.Is(err, syscall.ECONNREFUSED):
		return sondeoCerrado
	default:
		return sondeoNoSe
	}
}

// trazaResync parte el resync en tramos con KLING_RESYNC_TRACE=1 (solo log, como
// KLING_NET_TRACE): "conexión" es ARP + SYN/SYN-ACK, es decir, el kernel del
// invitado recién reanudado; "respuesta" es lo que tarda el agente desde que
// recibe la petición. Sin la variable es nil y no cambia nada: ni el resultado
// ni los reintentos.
type trazaResync struct {
	mu                                 sync.Mutex // los ganchos del cliente corren en otras goroutines
	base                               string
	ini, conectado, escrito, respuesta time.Time
}

func trazaResyncNueva(base string) *trazaResync {
	if os.Getenv("KLING_RESYNC_TRACE") != "1" {
		return nil
	}
	return &trazaResync{base: base, ini: time.Now()}
}

func (t *trazaResync) sobre(req *http.Request) *http.Request {
	if t == nil {
		return req
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		ConnectDone:          func(string, string, error) { t.marca(&t.conectado) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.marca(&t.escrito) },
		GotFirstResponseByte: func() { t.marca(&t.respuesta) },
	}))
}

func (t *trazaResync) marca(p *time.Time) {
	t.mu.Lock()
	*p = time.Now()
	t.mu.Unlock()
}

func (t *trazaResync) fin(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	desde := func(a, b time.Time) time.Duration {
		if a.IsZero() || b.IsZero() {
			return -1
		}
		return b.Sub(a).Round(10 * time.Microsecond)
	}
	log.Printf("resync trace %s: connect=%v write=%v agent=%v total=%v err=%v", t.base,
		desde(t.ini, t.conectado), desde(t.conectado, t.escrito), desde(t.escrito, t.respuesta),
		time.Since(t.ini).Round(10*time.Microsecond), err)
}

// reparto escribe, con la traza activa, de qué está hecha la espera del
// resync: connect y first_byte son del lado del daemon (ARP + SYN, y hasta el
// primer byte de la respuesta); guest_critical y guest_origin los midió el
// agente. first_byte − guest_critical es el despertar del invitado.
func (t *trazaResync) reparto(res api.GuestResyncResult) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	desde := func(b time.Time) time.Duration {
		if b.IsZero() {
			return -1
		}
		return b.Sub(t.ini).Round(10 * time.Microsecond)
	}
	log.Printf("resync split: connect=%v first_byte=%v guest_critical=%v guest_origin=%v",
		desde(t.conectado), desde(t.respuesta),
		time.Duration(res.CriticalUS)*time.Microsecond, time.Duration(res.OriginUS)*time.Microsecond)
}
