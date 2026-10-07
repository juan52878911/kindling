package machine

// "Listo" según la imagen (ver pkg/api/ready.go y pkg/guest/ready.go).
//
// El agente de invitado sabe si su imagen declara una sonda o ganchos tras
// restaurar, y GET /ready lo dice. Aquí se decide cuándo preguntarlo:
//
//   - Antes de congelar un dorado (Commit, Fork): un dorado a medio arrancar
//     es un dorado roto en cada copia. Es la espera que motivó todo esto.
//   - Tras arrancar o restaurar, en segundo plano, solo para que `kling ps`
//     lo enseñe (Machine.Ready). Tras restaurar, la respuesta del /resync ya
//     trae el estado, así que una imagen que no declara nada no cuesta ni una
//     petición más; tras un arranque en frío, una por máquina.
//   - Cuando alguien lo pide: run -wait-ready, GET /machines/{ref}/ready.
//
// Compatibilidad: un agente anterior a /ready (404, o el 400 del puente MCP
// viejo que atiende "/" entero) cuenta como "nada que esperar", que es lo que
// pasaba hasta ahora.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// ErrNotReady es un invitado que no llegó a estar listo según su imagen: plazo
// agotado o un gancho tras restaurar que falló.
var ErrNotReady = errors.New("the guest is not ready")

// DefaultReadyWait es cuánto se espera a "listo" por defecto (commit, fork,
// run -wait-ready). Un Android en frío tarda 3–10 s en un Mac cargado, y
// hasta 20 s con el Mac bajo presión; el margen es para eso.
const DefaultReadyWait = 2 * time.Minute

// Ritmo de la espera. La sonda corre dentro del invitado en cada pregunta
// hasta que contesta 0, así que no se pregunta más a menudo que esto.
const (
	pasoListo = 250 * time.Millisecond
	// pasoListoPrimero es la primera espera: tras restaurar, lo único que
	// falta suele ser que acaben los ganchos (decenas de ms; Android, 50), y
	// esperar el paso entero sumaba 250 ms a cada `run -from -wait-ready`.
	// Se dobla hasta pasoListo.
	pasoListoPrimero = 25 * time.Millisecond
	// plazoPeticionListo acota UNA pregunta a /ready: la sonda corre dentro
	// con su plazo (10 s, o el de la imagen hasta api.MaxReadyTimeoutSeconds).
	plazoPeticionListo = api.MaxReadyTimeoutSeconds*time.Second + 5*time.Second
	// plazoPeticionAgente acota una pregunta a / (las capacidades del agente).
	plazoPeticionAgente = 15 * time.Second
	// vigiaListoMax es cuánto sigue mirando, como mucho, la vigía de fondo.
	vigiaListoMax = 10 * time.Minute
	// vigiaAgenteMax: si en este tiempo el agente no ha contestado ni una vez
	// (imagen sin agente), la vigía se rinde.
	vigiaAgenteMax = 30 * time.Second
)

var (
	errListoViejo    = errors.New("guest agent has no /ready route")
	errListoConexion = errors.New("guest agent not reachable")
)

// listoClient, como resyncClient: sin conexiones guardadas hacia invitados.
var listoClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// consultarListo pregunta GET /ready al agente de la máquina id.
func (m *Manager) consultarListo(ctx context.Context, id string) (api.GuestReady, error) {
	if f := m.pruebasListo; f != nil {
		return f(ctx, id)
	}
	m.mu.RLock()
	mc := m.byID[id]
	var addr string
	var ag *api.GuestAgent
	if mc != nil && mc.Reachable() {
		addr, ag = mc.Addr(api.GuestPort), mc.Agent
	}
	m.mu.RUnlock()
	if addr == "" {
		return api.GuestReady{}, errListoConexion
	}
	if ag.Lacks(api.GuestCapReady) {
		return api.GuestReady{}, errListoViejo
	}
	ctx, cancel := context.WithTimeout(ctx, plazoPeticionListo)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+api.GuestReadyPath, nil)
	if err != nil {
		return api.GuestReady{}, err
	}
	resp, err := listoClient.Do(req)
	if err != nil {
		return api.GuestReady{}, fmt.Errorf("%w: %v", errListoConexion, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return interpretarListo(resp.StatusCode, b)
}

// interpretarListo traduce la respuesta de GET /ready.
func interpretarListo(code int, b []byte) (api.GuestReady, error) {
	var st api.GuestReady
	switch {
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		return st, errListoViejo
	case code == http.StatusBadRequest && strings.Contains(string(b), "Mcp-Session-Id"):
		// El puente MCP anterior contesta así a cualquier ruta (ver resyncOnce).
		return st, errListoViejo
	case code != http.StatusOK && code != http.StatusServiceUnavailable:
		return st, fmt.Errorf("guest answered %d to %s: %s", code, api.GuestReadyPath, strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, &st); err != nil {
		// Un 200 que no es el contrato de /ready es un servidor que contesta a
		// cualquier ruta (un agente anterior tras un proxy, un servicio propio en
		// el 8080): no tiene sonda, como el 404. Reintentarlo solo retrasaba cada
		// commit hasta agotar el plazo.
		if code == http.StatusOK {
			return st, errListoViejo
		}
		return st, fmt.Errorf("guest answered %d to %s with something that is not JSON", code, api.GuestReadyPath)
	}
	// El cuerpo manda, pero un 200 con ready=false (o al revés) es un agente
	// que no sigue el contrato: gana lo prudente.
	st.Ready = st.Ready && code == http.StatusOK
	return st, nil
}

// estadoListo es el Machine.Ready que corresponde a una respuesta del agente.
func estadoListo(st api.GuestReady) string {
	switch {
	case st.Ready && st.Declares():
		return api.ReadyYes
	case st.Ready:
		return api.ReadyUnknown
	case st.Hooks == api.HooksFailed:
		return api.ReadyFailed
	}
	return api.ReadyWaiting
}

// anotarListo guarda el estado en la máquina. No persiste a disco: tras
// reiniciar el daemon, la vigía no sigue y el dato se volvería a pedir.
func (m *Manager) anotarListo(id, estado string) {
	m.mu.Lock()
	if mc := m.byID[id]; mc != nil && mc.Ready != estado {
		mc.Ready = estado
	}
	m.mu.Unlock()
}

// OpcionesListo es cómo se espera.
type OpcionesListo struct {
	// Plazo total; 0 = DefaultReadyWait.
	Plazo time.Duration
	// SinAgenteVale: si nadie contesta en el puerto del agente, no hay nada
	// que esperar (commit de una imagen sin agente, como hasta ahora). Sin
	// esto, se espera a que conteste dentro del plazo.
	SinAgenteVale bool
	// UnaVez: preguntar una sola vez y devolver lo que haya, sin error por no
	// estar listo (GET /machines/{ref}/ready sin ?wait).
	UnaVez bool
}

// WaitReady espera a que la máquina ref esté lista según su imagen. Devuelve
// el resultado aunque falle (con Ready = waiting o failed); el error envuelve
// ErrNotReady si se agotó el plazo o falló un gancho.
func (m *Manager) WaitReady(ctx context.Context, ref string, o OpcionesListo) (api.ReadyResult, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return api.ReadyResult{}, fmt.Errorf("%w: %q", ErrNoMachine, ref)
	}
	res := api.ReadyResult{ID: mc.ID, Name: mc.Name}
	if mc.State != api.StateRunning {
		return res, fmt.Errorf("%w: %s is %s", ErrNotRunning, mc.Name, mc.State)
	}
	plazo := o.Plazo
	if plazo <= 0 {
		plazo = DefaultReadyWait
	}
	start := time.Now()
	defer func() { res.WaitedMS = time.Since(start).Milliseconds() }()

	if !mc.Reachable() {
		return res, nil // sin red hacia el invitado no hay a quién preguntar
	}
	if o.SinAgenteVale && !m.agenteEscucha(ctx, mc.ID) {
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, plazo)
	defer cancel()
	var ultimo error
	paso := pasoListoPrimero
	for {
		st, err := m.consultarListo(ctx, mc.ID)
		switch {
		case errors.Is(err, errListoViejo):
			m.anotarListo(mc.ID, api.ReadyUnknown)
			return res, nil
		case err == nil:
			res.Guest = &st
			res.Ready = estadoListo(st)
			m.anotarListo(mc.ID, res.Ready)
			if st.Ready {
				return res, nil
			}
			if res.Ready == api.ReadyFailed {
				return res, fmt.Errorf("%w: %s: %s", ErrNotReady, mc.Name, st.Detail)
			}
			if o.UnaVez {
				return res, nil
			}
		case errors.Is(err, errListoConexion) && o.SinAgenteVale && !m.agenteEscucha(ctx, mc.ID):
			// Contestó antes y ya no: no es un invitado arrancando.
			return res, nil
		default:
			ultimo = err
			if o.UnaVez {
				return res, nil
			}
		}
		if cur, ok := m.Get(mc.ID); !ok || cur.State != api.StateRunning {
			return res, fmt.Errorf("%w: %s stopped running while waiting for it to be ready", ErrNotRunning, mc.Name)
		}
		select {
		case <-ctx.Done():
			motivo := "its readiness probe never passed"
			switch {
			case res.Guest != nil && res.Guest.Detail != "":
				motivo = res.Guest.Detail
			case res.Guest == nil && ultimo != nil:
				motivo = ultimo.Error()
			}
			if res.Ready == api.ReadyUnknown {
				res.Ready = api.ReadyWaiting
			}
			return res, fmt.Errorf("%w: %s after %s: %s", ErrNotReady, mc.Name, plazo, motivo)
		case <-time.After(paso):
		}
		paso = min(2*paso, pasoListo)
	}
}

// vigilarListo sigue en segundo plano el "listo" de la máquina id para que
// `kling ps` lo enseñe. inicial es lo que ya se sabe (la respuesta del
// /resync), o nil para preguntar. Una vigía nueva de la misma máquina (un
// thaw) jubila a la anterior.
func (m *Manager) vigilarListo(id string, inicial *api.GuestReady) {
	if inicial != nil {
		estado := estadoListo(*inicial)
		m.anotarListo(id, estado)
		if estado != api.ReadyWaiting {
			return
		}
	}
	v, _ := m.vigiasListo.LoadOrStore(id, new(atomic.Uint64))
	gen := v.(*atomic.Uint64).Add(1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), vigiaListoMax)
		defer cancel()
		go func() {
			select {
			case <-m.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		start := time.Now()
		contestó := false
		for ctx.Err() == nil && v.(*atomic.Uint64).Load() == gen {
			mc, ok := m.Get(id)
			if !ok || mc.State != api.StateRunning {
				return
			}
			st, err := m.consultarListo(ctx, id)
			switch {
			case errors.Is(err, errListoViejo):
				m.anotarListo(id, api.ReadyUnknown)
				return
			case err == nil:
				contestó = true
				estado := estadoListo(st)
				if v.(*atomic.Uint64).Load() != gen {
					return
				}
				m.anotarListo(id, estado)
				if estado != api.ReadyWaiting {
					return
				}
			case !contestó && time.Since(start) > vigiaAgenteMax:
				return // sin agente
			}
			select {
			case <-ctx.Done():
			case <-time.After(4 * pasoListo):
			}
		}
	}()
}

// listoParaCongelar espera, antes de congelar un dorado (Commit, Fork), a que
// la máquina esté lista según su imagen. Una imagen sin agente, o que no
// declara nada, pasa sin esperar: lo de siempre.
//
// "Sin agente no hay nada que esperar" solo vale si la imagen no declara
// nada. Si declara sonda o ganchos, que nadie conteste en el puerto del agente
// es un invitado que aún arranca (o cuyo agente se cayó), no uno listo: dar
// eso por bueno congelaba el dorado a medio arrancar, que es justo lo que la
// sonda existe para impedir.
func (m *Manager) listoParaCongelar(ctx context.Context, ref string, plazo time.Duration) error {
	_, err := m.WaitReady(ctx, ref, OpcionesListo{Plazo: plazo, SinAgenteVale: !m.declaraListo(ctx, ref)})
	if err == nil || errors.Is(err, ErrNoMachine) || errors.Is(err, ErrNotRunning) {
		return nil // el commit dará su propio error, con su mensaje de siempre
	}
	return fmt.Errorf("%w.\nFreezing it now would give every copy a guest that has not finished booting. "+
		"Check it with `kling machine ready %s`, or skip the check (kling save -force, skip_ready)", err, ref)
}

// declaraListo dice si la máquina ref tiene sonda o ganchos que esperar antes
// de congelarla. Lo sabe por dos lados: lo que ya contestó su agente
// (Machine.Ready distinto de "desconocido" solo sale de una imagen que
// declara algo) y, si aún no ha contestado nunca, su imagen en disco.
// Sin poder saberlo, false: lo de siempre.
func (m *Manager) declaraListo(ctx context.Context, ref string) bool {
	mc, ok := m.Get(ref)
	if !ok {
		return false
	}
	switch mc.Ready {
	case api.ReadyWaiting, api.ReadyYes, api.ReadyFailed:
		return true
	}
	if mc.Image == "" {
		return false
	}
	decl, err := m.imagenDeclaraListo(ctx, mc.Image)
	return err == nil && decl
}

// imagenDeclaraListo mira en el rootfs de la imagen (capa y base) si trae
// api.GuestReadyProbe o api.GuestPostRestoreDir. Con caché por huella de los
// ficheros, como imageHasBridgeCached; el error (sin debugfs) no se cachea.
func (m *Manager) imagenDeclaraListo(ctx context.Context, image string) (bool, error) {
	base, layer, err := m.imageLayer(image)
	if err != nil {
		return false, err
	}
	key := bridgeFingerprint(base) + "\x00" + bridgeFingerprint(layer)
	if v, ok := m.listoDeclarado.Load(key); ok {
		return v.(bool), nil
	}
	decl := false
	for _, p := range []string{api.GuestReadyProbe, api.GuestPostRestoreDir} {
		if layer != "" {
			has, err := hasFile(ctx, layer, layerGuestPath(p))
			if err != nil {
				return false, err
			}
			if has {
				decl = true
				break
			}
		}
		has, err := hasFile(ctx, base, p)
		if err != nil {
			return false, err
		}
		if has {
			decl = true
			break
		}
	}
	m.listoDeclarado.Store(key, decl)
	return decl, nil
}
