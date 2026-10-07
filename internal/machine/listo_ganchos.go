package machine

// Ganchos tras restaurar (ver pkg/guest/ready.go): el daemon los lanza al final
// de cada restauración, y a petición (RunHooks).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// trasRestaurar es el último paso de una restauración (thaw, run -from, fork),
// con volúmenes montados y credenciales entregadas: lanza los ganchos de la
// imagen si los declara y deja a la vigía siguiendo el "listo". inicial es lo
// que contestó el /resync; nil es un agente anterior, una imagen sin agente o
// un /resync que falló: ver ganchosTrasResyncFallido.
// No espera a los ganchos: quien lo necesite llama a WaitReady.
func (m *Manager) trasRestaurar(ctx context.Context, id, kind string, inicial *api.GuestReady) {
	// Una descongelada ya sabe qué agente lleva; una copia de un dorado, no.
	m.conocerAgente(id)
	if inicial == nil {
		m.anotarListo(id, api.ReadyUnknown)
		m.ganchosTrasResyncFallido(id, kind)
		return
	}
	// Esta restauración sí contestó: una tanda pendiente de otra anterior ya
	// no hace falta, porque ahora se lanzan las de esta.
	m.ganchosPendientes.Delete(id)
	st := *inicial
	if st.HasHooks {
		lanzado, err := m.lanzarGanchos(ctx, id, kind)
		if err != nil {
			// No tumba la restauración: la máquina está en marcha; la vigía y
			// WaitReady dirán que no está lista.
			log.Printf("warning: %s: could not start its post-restore hooks: %v", shortID(id), err)
			st.Ready, st.Detail = false, err.Error()
		} else {
			st = lanzado
		}
	}
	m.vigilarListo(id, &st)
}

// ganchosPendientes es una tanda de ganchos que el daemon debe lanzar y aún no
// ha lanzado porque el /resync de la restauración falló. once: la lanzan una
// sola vez la vigía de fondo o el primer WaitReady, lo que llegue antes.
type ganchosPendientes struct {
	kind string
	once sync.Once
	st   api.GuestReady
	ok   bool // st vale: los ganchos se lanzaron (o fallaron al lanzarse)
}

// Cuántas veces y cada cuánto pregunta la vigía de un /resync fallido: tras
// restaurar, el agente que había al congelar ya escucha, así que pocas.
const (
	intentosGanchosPendientes = 4
	plazoGanchosPendientes    = 5 * time.Second
)

// ganchosTrasResyncFallido cubre una restauración sin respuesta del /resync.
// Si fue un agente anterior o una imagen sin agente, no hay nada que hacer;
// pero si el /resync falló con un agente al día, GET /ready contesta lo que
// quedó en la memoria del dorado (ganchos "done") y la copia se daría por
// lista sin haber corrido los suyos (identidad por copia). Se apunta la tanda
// como pendiente y se intenta lanzar en segundo plano; WaitReady la lanza
// también antes de creerse un "listo".
func (m *Manager) ganchosTrasResyncFallido(id, kind string) {
	p := &ganchosPendientes{kind: kind}
	m.ganchosPendientes.Store(id, p)
	go func() {
		for i := 0; i < intentosGanchosPendientes; i++ {
			if i > 0 {
				select {
				case <-m.quit:
					return
				case <-time.After(time.Duration(i) * pasoListo):
				}
			}
			if v, ok := m.ganchosPendientes.Load(id); !ok || v != p {
				return // ya se lanzaron, o hubo otra restauración
			}
			ctx, cancel := context.WithTimeout(context.Background(), plazoGanchosPendientes)
			st, err := m.consultarListo(ctx, id)
			cancel()
			switch {
			case errors.Is(err, errListoViejo):
				m.ganchosPendientes.CompareAndDelete(id, p)
				return
			case err == nil:
				st = m.ganchosPendientesAhora(context.Background(), id, st)
				m.vigilarListo(id, &st)
				return
			}
		}
	}()
}

// ganchosPendientesAhora lanza, si la máquina id tiene una tanda pendiente y
// la imagen declara ganchos, esa tanda, y devuelve el estado que sustituye a
// st (lo leído antes de lanzarla). Sin tanda pendiente devuelve st. Si no se
// pudieron lanzar, la tanda queda como fallida hasta que RunHooks las lance
// (kling machine hooks) u otra restauración la sustituya: el /ready del
// invitado seguiría diciendo lo del dorado.
func (m *Manager) ganchosPendientesAhora(ctx context.Context, id string, st api.GuestReady) api.GuestReady {
	v, ok := m.ganchosPendientes.Load(id)
	if !ok {
		return st
	}
	p := v.(*ganchosPendientes)
	p.once.Do(func() {
		var lanzado api.GuestReady
		var err error
		if st.HasHooks {
			// Sin el plazo de quien espera: si se cansa a mitad, la tanda no
			// debe quedar fallida (lanzarGanchos ya acota la petición).
			lanzado, err = m.lanzarGanchos(context.WithoutCancel(ctx), id, p.kind)
		}
		switch {
		case !st.HasHooks, errors.Is(err, errListoViejo):
			m.ganchosPendientes.CompareAndDelete(id, p)
			return
		case err == nil:
			m.ganchosPendientes.CompareAndDelete(id, p)
		default:
			log.Printf("warning: %s: resync failed and its post-restore hooks could not start: %v", shortID(id), err)
			lanzado = api.GuestReady{Probe: st.Probe, HasHooks: true, Hooks: api.HooksFailed,
				Detail: "resync failed, post-restore hooks not run: " + err.Error()}
		}
		p.st, p.ok = lanzado, true
	})
	if p.ok {
		return p.st
	}
	return st
}

// lanzarGanchos pide POST /hooks al agente de la máquina id.
func (m *Manager) lanzarGanchos(ctx context.Context, id, kind string) (api.GuestReady, error) {
	if f := m.pruebasGanchos; f != nil {
		return f(ctx, id, kind)
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
	// Un agente que anuncia lo que sabe y no dice "hooks" no tiene la ruta:
	// ni se le pregunta (agente.go).
	if ag.Lacks(api.GuestCapHooks) {
		return api.GuestReady{}, errListoViejo
	}
	ctx, cancel := context.WithTimeout(ctx, resyncPlazo)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+api.GuestHooksPath+"?restore="+kind, nil)
	if err != nil {
		return api.GuestReady{}, err
	}
	resp, err := listoClient.Do(req)
	if err != nil {
		return api.GuestReady{}, fmt.Errorf("%w: %v", errListoConexion, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return interpretarGanchos(resp.StatusCode, b)
}

// interpretarGanchos traduce la respuesta de POST /hooks.
func interpretarGanchos(code int, b []byte) (api.GuestReady, error) {
	var st api.GuestReady
	switch code {
	case http.StatusAccepted:
		if err := json.Unmarshal(b, &st); err != nil {
			// Se lanzaron, pero no se sabe su estado: corriendo, y GET /ready
			// dirá cuándo acaban.
			log.Printf("warning: guest answered %s with something that is not JSON: %v", api.GuestHooksPath, err)
			return api.GuestReady{HasHooks: true, Hooks: api.HooksRunning}, nil
		}
		return st, nil
	case http.StatusConflict:
		// Ya corrían (dos restauraciones seguidas): el estado dirá cuándo acaban.
		return api.GuestReady{HasHooks: true, Hooks: api.HooksRunning}, nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return st, errListoViejo
	}
	return st, fmt.Errorf("guest answered %d to %s: %s", code, api.GuestHooksPath, strings.TrimSpace(string(b)))
}

// memoriaInvitado pregunta GET /meminfo al agente de la máquina id (squeeze en
// macOS). Plazo corto: squeeze no debe quedarse esperando a un invitado mudo.
func (m *Manager) memoriaInvitado(ctx context.Context, id string) (api.GuestMemInfo, error) {
	var mi api.GuestMemInfo
	if f := m.pruebasMeminfo; f != nil {
		return f(ctx, id)
	}
	m.mu.RLock()
	mc := m.byID[id]
	var addr string
	if mc != nil && mc.Reachable() {
		addr = mc.Addr(api.GuestPort)
	}
	m.mu.RUnlock()
	if addr == "" {
		return mi, errListoConexion
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+api.GuestMemInfoPath, nil)
	if err != nil {
		return mi, err
	}
	resp, err := listoClient.Do(req)
	if err != nil {
		return mi, fmt.Errorf("%w: %v", errListoConexion, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mi, fmt.Errorf("guest answered %d to %s", resp.StatusCode, api.GuestMemInfoPath)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&mi); err != nil {
		return mi, err
	}
	if mi.TotalMiB <= 0 {
		return mi, fmt.Errorf("guest reported no memory in %s", api.GuestMemInfoPath)
	}
	return mi, nil
}

// RunHooks vuelve a lanzar los ganchos tras restaurar de la máquina ref (POST
// /machines/{ref}/hooks) y, con espera > 0, espera a que acaben. Es para lo
// que llega después de restaurar: el secreto que pone `kling machine secret`.
func (m *Manager) RunHooks(ctx context.Context, ref string, espera time.Duration) (api.ReadyResult, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return api.ReadyResult{}, fmt.Errorf("%w: %q", ErrNoMachine, ref)
	}
	res := api.ReadyResult{ID: mc.ID, Name: mc.Name}
	if mc.State != api.StateRunning {
		return res, fmt.Errorf("%w: %s is %s", ErrNotRunning, mc.Name, mc.State)
	}
	start := time.Now()
	gen := m.genSecreto(mc.ID)
	st, err := m.lanzarGanchos(ctx, mc.ID, "manual")
	switch {
	case errors.Is(err, errListoViejo):
		return res, fmt.Errorf("%w: the guest agent of %s predates post-restore hooks: rebuild the image "+
			"with a current kling-guest", ErrNotReady, mc.Name)
	case err != nil:
		return res, fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	// Lanzadas a mano: una tanda pendiente de un /resync fallido ya no lo está.
	m.ganchosPendientes.Delete(mc.ID)
	res.Guest, res.Ready = &st, estadoListo(st)
	m.anotarListo(mc.ID, res.Ready)
	if espera <= 0 {
		m.vigilarListo(mc.ID, &st)
		return res, nil
	}
	res, err = m.WaitReady(ctx, mc.ID, OpcionesListo{Plazo: espera})
	res.WaitedMS = time.Since(start).Milliseconds()
	if err == nil && res.Guest != nil && res.Guest.Hooks == api.HooksDone {
		// Terminaron con éxito tras la inyección gen: cuenta como que la
		// imagen consumió el secreto (ver PutMMDS).
		m.confirmarSecreto(mc.ID, gen)
	}
	return res, err
}
