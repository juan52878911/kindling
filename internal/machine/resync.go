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
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
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

// resyncClient no reutiliza conexiones: cada llamada va a una microVM recién
// restaurada, y una conexión ociosa guardada hacia ella solo retendría un
// descriptor hasta que el invitado muera.
var resyncClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// resyncGuest resincroniza el invitado id. No devuelve error: un agente viejo
// (404), una máquina sin agente o uno que falla dejan la máquina como antes de
// esto —utilizable, con el reloj parado— y se avisa una vez por imagen.
// clave es la del snapshot del que se restauró (claveSnapshot) para recordar
// que no tiene agente, o "" para no recordar nada. Devuelve cuánto tardó y si
// el agente lo aplicó.
func (m *Manager) resyncGuest(ctx context.Context, id, clave string) (time.Duration, bool) {
	m.mu.RLock()
	mc := m.byID[id]
	var addr, image, name string
	if mc != nil && mc.Reachable() {
		addr, image, name = mc.Addr(api.GuestPort), mc.Image, mc.Name
	}
	m.mu.RUnlock()
	if addr == "" {
		return 0, false
	}
	if clave != "" {
		if hasta, ok := m.resyncSinAgente.Load(clave); ok && time.Now().Before(hasta.(time.Time)) {
			return 0, false
		}
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, resyncPlazo)
	defer cancel()
	res, err := resyncOnce(ctx, "http://"+addr)
	for err != nil && errors.Is(err, errResyncConexion) && time.Since(start) < resyncReintento {
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
		res, err = resyncOnce(ctx, "http://"+addr)
	}
	took := time.Since(start)
	if err != nil {
		if clave != "" && errors.Is(err, errResyncNadie) {
			m.resyncSinAgente.Store(clave, time.Now().Add(resyncSinAgenteTTL))
		}
		m.avisarResync(image, name, err)
		return took, false
	}
	if res.SkewMS > 1000 || res.SkewMS < -1000 {
		log.Printf("%s: guest clock was %s off; resynced in %s", name,
			(time.Duration(res.SkewMS) * time.Millisecond).Round(time.Millisecond), took.Round(time.Millisecond))
	}
	return took, true
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
			"kling-guest (kling images build), or refresh its MCP bridge (kling mcp refresh-bridge)",
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

func resyncOnce(ctx context.Context, base string) (api.GuestResyncResult, error) {
	var out api.GuestResyncResult
	ent := make([]byte, api.GuestResyncEntropy)
	if _, err := rand.Read(ent); err != nil {
		return out, err
	}
	// La hora se toma lo más tarde posible: lo que tarde la petición es el
	// error que queda en el invitado.
	body, err := json.Marshal(api.GuestResync{UnixNano: time.Now().UnixNano(), Entropy: ent})
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+api.GuestResyncPath, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
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
	if open, ok := m.ProbeGuestPort(ctx, id, api.GuestPort); ok {
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
