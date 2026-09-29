package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// SESIONES AISLADAS: una microVM por sesión (docs/aislamiento-por-sesion.md).
//
// Una instancia normal atiende a varias sesiones, cada una en su proceso, pero
// todas escriben en el MISMO overlay: lo que una deja en /tmp o en el directorio
// de datos del servidor lo lee la siguiente. Para un servicio multiusuario eso
// es una fuga de estado entre clientes.
//
// Una sesión aislada tiene su propia microVM, restaurada del snapshot dorado
// con su propio overlay (la copia dispersa que ya hace runFrom) y marcada con
// LabelIsolated. Nadie más la adopta ni la descongela (acquire la salta), el
// segador la congela por inactividad como a cualquier otra —con el proceso de
// la sesión dentro, así que la sesión sobrevive al thaw—, y al cerrarse la
// sesión o caducar (SessionTTL) la máquina se DESTRUYE: su overlay y su
// volcado desaparecen del host con ella.
//
// Por qué una microVM y no un overlayfs por sesión dentro del invitado: la
// frontera es la misma que justifica el proyecto. Dos procesos en el mismo
// kernel invitado se ven por /proc/<pid>/root aunque cada uno tenga su mount
// namespace, y un servidor MCP es código de terceros. Además no hay que tocar
// el invitado (ni el puente, ni las bases), vale igual para los servidores de
// HTTP nativo, que comparten un solo proceso, y el snapshot dorado hace que
// cada máquina de más cueste ~7 MiB de RAM y ~40 ms.

// LabelIsolated marca la máquina propia de una sesión aislada.
const LabelIsolated = "isolated"

// defaultSessionTTL es cuánto vive una sesión aislada sin uso. Más largo que
// idle a propósito: congelarla es barato y conserva la sesión; destruirla la
// pierde. Es media hora porque cada sesión abandonada ocupa en disco lo que
// una máquina congelada (~35-80 MB) hasta que caduca.
const defaultSessionTTL = 30 * time.Minute

// Cada cuánto se buscan máquinas aisladas huérfanas, y cuánto tiene que tener
// una para considerarla huérfana: la gracia cubre el hueco entre que el daemon
// la crea y que el planificador la registra.
const (
	barridoCada   = 5 * time.Minute
	barridoGracia = 2 * time.Minute
)

// aisladaReclaimGrace es lo que tiene que llevar ociosa una sesión aislada para
// reciclarla cuando el servicio está en su tope. La misma idea, y el mismo
// valor, que sessionReclaimGrace del puente: un cliente que se reconecta sin
// cerrar la sesión anterior no debe esperar a que caduque.
const aisladaReclaimGrace = 45 * time.Second

// ErrSessionLost es el error de IsolatedSession cuando la máquina de la sesión
// ya no existe (la retiró el recolector del daemon, o alguien la borró): el
// cliente tiene que empezar otra sesión.
var ErrSessionLost = errors.New("the isolated session lost its machine")

// ErrNoSuchSession es el error de IsolatedSession sin create para una clave que
// no es de ninguna sesión aislada viva.
var ErrNoSuchSession = errors.New("no isolated session with that key")

// aislada es una sesión con microVM propia. Existe mientras la sesión viva,
// esté su máquina despierta (en g.extra) o congelada.
type aislada struct {
	key       string
	service   string
	machineID string
	lastUse   time.Time
}

func (g *Scheduler) sessionTTL() time.Duration {
	if g.SessionTTL > 0 {
		return g.SessionTTL
	}
	return defaultSessionTTL
}

// aisladaLock devuelve el candado de una sesión aislada.
func (g *Scheduler) aisladaLock(key string) *sync.Mutex {
	v, _ := g.aisladaMu.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// isolatedSession devuelve la microVM de la sesión key del servicio, despierta.
//
// Si la sesión ya existe, es su máquina: la de g.extra si está despierta, o la
// misma descongelada. Si no existe y create es true, restaura una nueva del
// dorado; sin create devuelve ErrNoSuchSession. Nunca devuelve otra máquina que
// la de la sesión: es la garantía entera.
func (g *Scheduler) isolatedSession(ctx context.Context, service, key string, tnt *tenant, create bool) (*entry, error) {
	lock := g.aisladaLock(key)
	lock.Lock()
	defer lock.Unlock()

	g.mu.Lock()
	a := g.aisladas[key]
	if a != nil {
		if a.service != service {
			g.mu.Unlock()
			return nil, fmt.Errorf("%w: key belongs to another service", ErrNoSuchSession)
		}
		a.lastUse = time.Now()
		if e := g.entryByMachineLocked(service, a.machineID); e != nil {
			e.lastUse = time.Now()
			g.mu.Unlock()
			return e, nil
		}
		id := a.machineID
		g.mu.Unlock()
		// Congelada (o pausada): la MISMA máquina, que es la que tiene el
		// proceso y el disco de esta sesión.
		e, err := g.buildEntryWith(ctx, service, tnt, func(tr *WakeTrace) (*api.Machine, string, error) {
			return g.despertarAislada(ctx, id, tr)
		})
		if err != nil {
			if api.IsNotFound(err) {
				g.mu.Lock()
				if g.aisladas[key] == a {
					delete(g.aisladas, key)
					delete(g.routes, key)
				}
				g.mu.Unlock()
				return nil, fmt.Errorf("%w: %v", ErrSessionLost, err)
			}
			return nil, err
		}
		g.registrarAislada(key, service, e)
		return e, nil
	}
	g.mu.Unlock()
	if !create {
		return nil, ErrNoSuchSession
	}

	liberar, err := g.reservarAislada(ctx, service)
	if err != nil {
		return nil, err
	}
	defer liberar()

	var creada string
	e, err := g.buildEntryWith(ctx, service, tnt, func(tr *WakeTrace) (*api.Machine, string, error) {
		t := time.Now()
		mc, err := g.runFreshWith(ctx, service, map[string]string{LabelIsolated: "true"})
		tr.Wake += time.Since(t)
		if err != nil {
			return nil, "", err
		}
		creada = mc.ID
		g.mu.Lock()
		if g.adquiriendo == nil {
			g.adquiriendo = map[string]bool{}
		}
		g.adquiriendo[mc.ID] = true
		g.mu.Unlock()
		return mc, "restore", nil
	})
	if err != nil {
		// Nació pero no llegó a servir: es de una sesión que no va a existir, y
		// nadie más la puede adoptar. Se destruye ya en vez de esperar al barrido.
		if creada != "" {
			g.destruirAislada(ctx, &aislada{key: key, service: service, machineID: creada}, "failed to start")
		}
		return nil, err
	}
	g.registrarAislada(key, service, e)
	log.Printf("%s: isolated session %s on its own machine %s", service, short(key), short(e.machineID))
	return e, nil
}

// registrarAislada apunta e como la máquina despierta de la sesión key.
func (g *Scheduler) registrarAislada(key, service string, e *entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.aisladas == nil {
		g.aisladas = map[string]*aislada{}
	}
	e.aislada = key
	g.extra[service] = append(g.extra[service], e)
	delete(g.adquiriendo, e.machineID)
	g.soltarCuotaLocked(e)
	if a := g.aisladas[key]; a != nil {
		a.machineID, a.lastUse = e.machineID, time.Now()
		return
	}
	g.aisladas[key] = &aislada{key: key, service: service, machineID: e.machineID, lastUse: time.Now()}
}

// despertarAislada devuelve despierta la máquina id, sin elegir otra. Es acquire
// para una máquina concreta: la deja marcada en g.adquiriendo.
func (g *Scheduler) despertarAislada(ctx context.Context, id string, tr *WakeTrace) (*api.Machine, string, error) {
	t := time.Now()
	mc, err := g.client.Get(ctx, id)
	tr.List += time.Since(t)
	if err != nil {
		return nil, "", err
	}
	g.mu.Lock()
	if g.adquiriendo[id] {
		g.mu.Unlock()
		return nil, "", fmt.Errorf("machine %s of the isolated session is already being woken", short(id))
	}
	if g.adquiriendo == nil {
		g.adquiriendo = map[string]bool{}
	}
	g.adquiriendo[id] = true
	delete(g.pausadas, id)
	g.mu.Unlock()
	fallo := func(err error) (*api.Machine, string, error) {
		g.mu.Lock()
		delete(g.adquiriendo, id)
		g.mu.Unlock()
		return nil, "", err
	}

	switch mc.State {
	case api.StateRunning:
		if !mc.Reachable() {
			return fallo(fmt.Errorf("machine %s of the isolated session is running but unreachable", short(id)))
		}
		g.renovarTTL(ctx, id)
		return mc, "adopt", nil
	case api.StatePaused, api.StateWarm:
		// Igual que acquire: el TTL se renueva ANTES del thaw, o el vigilante
		// del daemon podría volver a congelarla entre medias.
		t := time.Now()
		g.renovarTTL(ctx, id)
		tr.Renew += time.Since(t)
		t = time.Now()
		th, err := g.client.Thaw(ctx, id)
		tr.Wake += time.Since(t)
		if th != nil {
			tr.Daemon = th.Wake
		}
		if err != nil {
			return fallo(err)
		}
		how := "thaw"
		if mc.State == api.StatePaused {
			how = "resume"
		}
		return th, how, nil
	}
	return fallo(fmt.Errorf("machine %s of the isolated session is %s", short(id), mc.State))
}

// reservarAislada aplica el tope de sesiones aisladas por servicio y reserva la
// plaza (en g.creando, como scaleOut) hasta que la sesión nueva se registra.
//
// Cada sesión aislada es una máquina, así que el tope es el de réplicas: con
// -max-replicas 16, dieciséis sesiones simultáneas, despiertas o congeladas.
// Al llegar al tope se recicla la sesión más ociosa si lleva más de
// aisladaReclaimGrace sin uso; si todas están vivas, ErrMaxReplicas.
func (g *Scheduler) reservarAislada(ctx context.Context, service string) (liberar func(), err error) {
	tope := g.topeReplicas(service)
	for {
		g.mu.Lock()
		n := g.creando[service]
		for _, a := range g.aisladas {
			if a.service == service {
				n++
			}
		}
		if tope <= 0 || n < tope {
			if g.creando == nil {
				g.creando = map[string]int{}
			}
			g.creando[service]++
			g.mu.Unlock()
			return func() {
				g.mu.Lock()
				if g.creando[service]--; g.creando[service] <= 0 {
					delete(g.creando, service)
				}
				g.mu.Unlock()
			}, nil
		}
		v := g.aisladaReciclableLocked(service)
		if v == nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("%w: service %q has %d isolated session(s) open (max %d)",
				ErrMaxReplicas, service, n, tope)
		}
		g.quitarAisladaLocked(v)
		g.mu.Unlock()
		g.destruirAislada(ctx, v, fmt.Sprintf("reclaimed after %s idle to make room",
			time.Since(v.lastUse).Round(time.Second)))
	}
}

// aisladaReciclableLocked es la sesión aislada del servicio más ociosa que ya
// pasó la gracia y no tiene trabajo en vuelo, o nil. Se llama con g.mu tomado.
func (g *Scheduler) aisladaReciclableLocked(service string) *aislada {
	var v *aislada
	for _, a := range g.aisladas {
		if a.service != service || time.Since(a.lastUse) < aisladaReclaimGrace {
			continue
		}
		if e := g.entryByMachineLocked(a.service, a.machineID); e != nil && e.inflight > 0 {
			continue
		}
		if v == nil || a.lastUse.Before(v.lastUse) {
			v = a
		}
	}
	return v
}

// quitarAisladaLocked olvida la sesión: su registro, su ruta y su instancia
// despierta si la hay. La máquina la destruye quien llama, fuera del candado.
func (g *Scheduler) quitarAisladaLocked(a *aislada) {
	delete(g.aisladas, a.key)
	delete(g.routes, a.key)
	delete(g.pausadas, a.machineID)
	g.removeEntryLocked(a.service, a.machineID)
}

// caducarAisladasLocked quita las sesiones aisladas que llevan más de
// SessionTTL sin uso y las devuelve para destruir su máquina. Una con trabajo en
// vuelo no caduca. Se llama con g.mu tomado.
func (g *Scheduler) caducarAisladasLocked() []*aislada {
	ttl := g.sessionTTL()
	var out []*aislada
	for _, a := range g.aisladas {
		if time.Since(a.lastUse) <= ttl {
			continue
		}
		if e := g.entryByMachineLocked(a.service, a.machineID); e != nil && e.inflight > 0 {
			continue
		}
		out = append(out, a)
	}
	for _, a := range out {
		g.quitarAisladaLocked(a)
	}
	return out
}

// releaseIsolated cierra la sesión aislada key y destruye su máquina. false si
// no había tal sesión.
func (g *Scheduler) releaseIsolated(ctx context.Context, key string) bool {
	g.mu.Lock()
	a := g.aisladas[key]
	if a != nil {
		g.quitarAisladaLocked(a)
	}
	g.mu.Unlock()
	if a == nil {
		return false
	}
	g.destruirAislada(ctx, a, "closed")
	return true
}

// destruirAislada borra la máquina de una sesión aislada, y con ella su overlay
// y su volcado de memoria. Es el único final de una sesión aislada: congelarla
// la conserva, esto la libera.
func (g *Scheduler) destruirAislada(ctx context.Context, a *aislada, motivo string) {
	g.aisladaMu.Delete(a.key)
	borrar := g.removeFn
	if borrar == nil {
		if g.client == nil {
			return
		}
		borrar = func(ctx context.Context, id string) error { return g.client.Remove(ctx, id) }
	}
	// Sin el ctx de quien llama: un DELETE cuyo cliente ya se fue no debe dejar
	// la máquina a medio borrar.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := borrar(rctx, a.machineID); err != nil && !api.IsNotFound(err) {
		log.Printf("%s: isolated session %s %s, but destroying machine %s failed: %v "+
			"(the orphan sweep will retry)", a.service, short(a.key), motivo, short(a.machineID), err)
		return
	}
	log.Printf("%s: isolated session %s %s; machine %s destroyed", a.service, short(a.key), motivo, short(a.machineID))
}

// tocaBarrerLocked dice si toca buscar huérfanas, y apunta que se va a hacer.
// La primera vuelta del segador siempre barre: es la que limpia lo que dejó un
// gateway anterior. Se llama con g.mu tomado.
func (g *Scheduler) tocaBarrerLocked() bool {
	if g.client == nil || time.Since(g.barridoAt) < barridoCada {
		return false
	}
	g.barridoAt = time.Now()
	return true
}

// barrerAisladas destruye las máquinas aisladas que no son de ninguna sesión
// viva.
//
// Las sesiones viven en la memoria del gateway: si se reinicia, sus máquinas
// se quedan en el daemon sin dueño —congeladas por su TTL, ocupando disco— y
// nadie puede volver a usarlas, porque acquire no adopta una aislada. También
// recoge la que un destruirAislada no llegó a borrar. Solo las que llevan las
// etiquetas de este planificador (MachineLabels): otro producto sobre el mismo
// daemon tiene las suyas.
func (g *Scheduler) barrerAisladas(ctx context.Context) {
	ms, err := g.client.List(ctx)
	if err != nil {
		return
	}
	g.mu.Lock()
	suyas := map[string]bool{}
	for _, a := range g.aisladas {
		suyas[a.machineID] = true
	}
	var huerfanas []*aislada
	for _, m := range ms {
		if m.Labels[LabelIsolated] != "true" || suyas[m.ID] || g.adquiriendo[m.ID] {
			continue
		}
		ajena := false
		for k, v := range g.MachineLabels {
			if m.Labels[k] != v {
				ajena = true
			}
		}
		if ajena || time.Since(m.CreatedAt) < barridoGracia {
			continue
		}
		huerfanas = append(huerfanas, &aislada{key: "(orphan)", service: m.Service(), machineID: m.ID})
	}
	g.mu.Unlock()
	for _, a := range huerfanas {
		g.destruirAislada(ctx, a, "had no session")
	}
}

// soltarAisladas destruye las máquinas de todas las sesiones aisladas. Lo usa el
// apagado: las sesiones viven en la memoria del gateway y no sobreviven a su
// reinicio, así que sus máquinas solo ocuparían disco hasta el barrido del
// siguiente arranque. Si el plazo de quien llama vence antes, ese barrido
// recoge el resto.
func (g *Scheduler) soltarAisladas(ctx context.Context) {
	g.mu.Lock()
	var todas []*aislada
	for _, a := range g.aisladas {
		todas = append(todas, a)
	}
	for _, a := range todas {
		g.quitarAisladaLocked(a)
	}
	g.mu.Unlock()
	for _, a := range todas {
		if ctx.Err() != nil {
			return
		}
		g.destruirAislada(ctx, a, "closed on shutdown")
	}
}
