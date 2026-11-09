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
	"strings"
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
	if snap.SinAgente {
		return resultadoResync{}
	}
	return m.resyncGuest(ctx, id, claveSnapshot(snap), api.ResyncInstance)
}

// resyncClient no reutiliza conexiones: cada llamada va a una microVM recién
// restaurada, y una conexión ociosa guardada hacia ella solo retendría un
// descriptor hasta que el invitado muera.
var resyncClient = guesthttp.New(nil, 0)

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
}

// resyncGuest resincroniza el invitado id. No devuelve error: un agente viejo
// (404), una máquina sin agente o uno que falla dejan la máquina como antes de
// esto —utilizable, con el reloj parado— y se avisa una vez por imagen.
// clave es la del snapshot del que se restauró (claveSnapshot) para recordar
// que no tiene agente, o "" para no recordar nada.
func (m *Manager) resyncGuest(ctx context.Context, id, clave, kind string) resultadoResync {
	m.mu.RLock()
	mc := m.byID[id]
	var addr, image, name string
	var ag *api.GuestAgent
	if mc != nil && mc.Reachable() {
		addr, image, name, ag = mc.Addr(api.GuestPort), mc.Image, mc.Name, mc.Agent
	}
	m.mu.RUnlock()
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

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, resyncPlazo)
	defer cancel()
	res, err := resyncOnce(ctx, "http://"+addr, kind)
	for err != nil && errors.Is(err, errResyncConexion) && time.Since(start) < resyncReintento {
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
		if clave != "" && errors.Is(err, errResyncNadie) {
			m.resyncSinAgente.Store(clave, time.Now().Add(resyncSinAgenteTTL))
		}
		m.avisarResync(image, name, err)
		// Un agente anterior o nadie escuchando no tienen ganchos que lanzar;
		// cualquier otro fallo (un plazo agotado, un 5xx) es de un agente que
		// sí debía contestar.
		fallo := !errors.Is(err, errResyncNoSoportado) && !errors.Is(err, errResyncNadie)
		return resultadoResync{took: took, fallo: fallo}
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
	defer m.lock(id)()
	if ahora, ok := m.ejecucionDe(id); !ok || ahora != ej || m.enTransicion(id) {
		return
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
	m.resyncGuest(ctx, id, "", kind)
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
	// puerto del agente: RST en Linux, cierre del reenvío en macOS.
	errResyncNadie = errors.New("nothing listens on the guest agent port")
)

func resyncOnce(ctx context.Context, base, kind string) (api.GuestResyncResult, error) {
	var out api.GuestResyncResult
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
	resp, err := resyncClient.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
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
	if open, ok := m.sondearPuerto(ctx, id, api.GuestPort, false); ok {
		return open
	}
	m.mu.RLock()
	mc := m.byID[id]
	var addr string
	if mc != nil && mc.Reachable() {
		addr = mc.Addr(api.GuestPort)
	}
	m.mu.RUnlock()
	if addr == "" {
		return true
	}
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
