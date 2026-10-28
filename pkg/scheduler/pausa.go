package scheduler

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// EL NIVEL "PAUSADA" (docs/despertar.md).
//
// Al quedarse ociosa, una instancia se congela (warm: su memoria a disco, sin
// proceso) o, si es pequeña y popular y cabe en PausedMiB, se PAUSA: el VMM
// sigue vivo con los vCPU parados y despertarla es solo reanudarla (~1 ms
// frente a ~30 ms de un thaw). Cuesta la RAM que retiene, así que:
//
//   - se reparte un presupuesto (PausedMiB, en mem_mib configurada: cota
//     superior de lo que retiene) por popularidad × coste: puntuación =
//     popularidad / mem_mib, de mayor a menor. Una réplica de Chispa (128 MiB)
//     cabe veinte veces donde cabe una sola de un VON de 1,5 GiB;
//   - una pausada que no se vuelve a usar en PausedFor se congela de verdad;
//   - con el host sin memoria, evictLRU congela antes las pausadas que nada
//     que esté despierto.
//
// PausedMiB < 0 es el presupuesto AUTOMÁTICO (el de `kling ai serve` sobre el
// backend vz, docs/mac.md): en cada vuelta se deriva de la memoria del host
// que da el daemon (presupuestoPausadas), y si baja (el nivel de memoria de
// macOS cae) enfriarPausadas congela las pausadas que ya no caben, de la más
// vieja a la más nueva.

// pausada es una instancia que el segador pausó en vez de congelar.
type pausada struct {
	service string
	memMiB  int
	at      time.Time
	// aislada es la clave de la sesión aislada dueña de la máquina, si lo es
	// (ver candadoPausada).
	aislada string
}

// pausedFor es cuánto dura una pausada antes de congelarse de verdad.
func (g *Scheduler) pausedFor() time.Duration {
	if g.PausedFor > 0 {
		return g.PausedFor
	}
	return 10 * g.idle
}

// sabePausar pregunta UNA vez al daemon si anuncia la capacidad "pause".
func (g *Scheduler) sabePausar(ctx context.Context) bool {
	switch g.pauseCap.Load() {
	case 1:
		return true
	case 2:
		return false
	}
	if g.client == nil {
		return false
	}
	info, err := g.client.Info(ctx)
	if err != nil {
		return false
	}
	if info.Has("pause") {
		g.pauseCap.Store(1)
		return true
	}
	g.pauseCap.Store(2)
	return false
}

type dormida struct {
	service, id string
	memMiB      int
	aislada     string // clave de la sesión, si es la máquina de una aislada
}

// dormir pone a dormir las víctimas del segador: pausa las que mejor
// puntúan mientras quepan en el presupuesto y congela el resto. Sin
// presupuesto (o sin un daemon que sepa pausar) congela todas, como siempre.
func (g *Scheduler) dormir(ctx context.Context, victims []dormida) {
	pausar := g.pauseFn
	if pausar == nil {
		pausar = func(id string) error { _, err := g.client.Pause(ctx, id); return err }
	}
	congelar := g.freezeFn
	if congelar == nil {
		congelar = func(id string) error { _, err := g.client.Freeze(ctx, id); return err }
	}
	var paused []dormida
	presupuesto := 0
	if len(victims) > 0 && g.PausedMiB != 0 && (g.pauseFn != nil || g.sabePausar(ctx)) {
		presupuesto = g.presupuestoPausadas(ctx)
	}
	if presupuesto > 0 {
		score := func(v dormida) float64 { return g.pop.score(v.service) / float64(v.memMiB) }
		cands := make([]dormida, 0, len(victims))
		for _, v := range victims {
			if v.memMiB > 0 && v.memMiB <= presupuesto && g.pop.score(v.service) > 0 {
				cands = append(cands, v)
			}
		}
		sort.SliceStable(cands, func(i, j int) bool { return score(cands[i]) > score(cands[j]) })
		g.mu.Lock()
		usado := 0
		for _, p := range g.pausadas {
			usado += p.memMiB
		}
		for _, v := range cands {
			if usado+v.memMiB <= presupuesto {
				usado += v.memMiB
				paused = append(paused, v)
			}
		}
		g.mu.Unlock()
	}
	elegida := map[string]bool{}
	for _, v := range paused {
		if err := pausar(v.id); err != nil {
			log.Printf("reap %s: pause: %v (freezing instead)", v.service, err)
			continue
		}
		elegida[v.id] = true
		g.mu.Lock()
		if g.pausadas == nil {
			g.pausadas = map[string]pausada{}
		}
		g.pausadas[v.id] = pausada{service: v.service, memMiB: v.memMiB, at: time.Now(), aislada: v.aislada}
		g.mu.Unlock()
		log.Printf("%s: paused due to inactivity", v.service)
	}
	for _, v := range victims {
		if elegida[v.id] {
			continue
		}
		if err := congelar(v.id); err != nil {
			log.Printf("reap %s: %v", v.service, err)
			continue
		}
		log.Printf("%s: frozen due to inactivity", v.service)
	}
}

// Presupuesto automático (PausedMiB < 0).
const (
	// fraccionPausadasHost: lo pausado puede retener hasta esta fracción de la
	// RAM física menos lo que ya ocupan las máquinas despiertas...
	fraccionPausadasHost = 50
	// ...y nunca más de esta fracción de lo que el host da por disponible.
	fraccionPausadasLibre = 50
	// nivelFrenoPausadas es el nivel de memoria (disponible / física, en %)
	// por debajo del cual no se pausa nada y se congela lo pausado. Es el
	// umbral con el que la compuerta de restores del daemon deja de admitir.
	nivelFrenoPausadas = 25
	// presupuestoPausadasDef es el de siempre, para cuando el daemon no da su
	// memoria (no se puede derivar).
	presupuestoPausadasDef = 256
)

// procStatsDe trae la foto de memoria del daemon; variable para las pruebas.
var procStatsDe = func(ctx context.Context, g *Scheduler) (*api.ProcStats, error) {
	if g.client == nil {
		return nil, errors.New("no daemon client")
	}
	return g.client.ProcStats(ctx)
}

// presupuestoPausadas es cuántos MiB (de mem_mib configurada) puede retener
// ahora el nivel pausada: PausedMiB si es fijo, o el derivado del host.
func (g *Scheduler) presupuestoPausadas(ctx context.Context) int {
	if g.PausedMiB >= 0 {
		return g.PausedMiB
	}
	ps, err := procStatsDe(ctx, g)
	if err != nil {
		return presupuestoPausadasDef
	}
	return presupuestoDeHost(ps)
}

// presupuestoDeHost deriva el presupuesto de la foto del daemon: la mitad de
// la RAM física menos lo despierto (su huella de verdad), sin pasar de la
// mitad de lo disponible, y 0 con el nivel de memoria en el freno.
func presupuestoDeHost(ps *api.ProcStats) int {
	if ps.AvailableMiB <= 0 {
		return presupuestoPausadasDef
	}
	libre := ps.AvailableMiB * fraccionPausadasLibre / 100
	if ps.TotalMiB <= 0 {
		return int(libre) // daemon sin TotalMiB: solo lo disponible
	}
	if ps.AvailableMiB*100/ps.TotalMiB <= nivelFrenoPausadas {
		return 0
	}
	var despiertas int64
	for _, m := range ps.Machines {
		if m.State == api.StateRunning {
			despiertas += m.PSSMiB
		}
	}
	return int(max(0, min(ps.TotalMiB*fraccionPausadasHost/100-despiertas, libre)))
}

// ajustarPausadas congela, de la más vieja a la más nueva, las pausadas que
// no caben en el presupuesto automático de ahora: es lo que suelta su RAM
// cuando el nivel de memoria del host baja.
func (g *Scheduler) ajustarPausadas(ctx context.Context) {
	if g.PausedMiB >= 0 {
		return
	}
	g.mu.Lock()
	hay := len(g.pausadas) > 0
	g.mu.Unlock()
	if !hay {
		return
	}
	presupuesto := g.presupuestoPausadas(ctx)
	for {
		g.mu.Lock()
		usado := 0
		for _, p := range g.pausadas {
			usado += p.memMiB
		}
		g.mu.Unlock()
		if usado <= presupuesto {
			return
		}
		id, p, soltar := g.tomarPausadaMasVieja()
		if soltar == nil {
			return // las que quedan las está despertando alguien
		}
		ok := g.congelarPausada(ctx, id)
		soltar(ok)
		if !ok {
			return
		}
		log.Printf("%s: paused instance frozen, host memory is low (paused budget %d MiB)", p.service, presupuesto)
	}
}

// enfriarPausadas congela de verdad las pausadas que llevan más de pausedFor
// sin usarse, y las que ya no caben en el presupuesto automático.
func (g *Scheduler) enfriarPausadas(ctx context.Context) {
	defer g.ajustarPausadas(ctx)
	vieja := func(p pausada) bool { return time.Since(p.at) > g.pausedFor() }
	type candidata struct {
		id string
		p  pausada
	}
	var viejas []candidata
	g.mu.Lock()
	for id, p := range g.pausadas {
		if vieja(p) && !g.adquiriendo[id] {
			viejas = append(viejas, candidata{id, p})
		}
	}
	g.mu.Unlock()
	for _, v := range viejas {
		soltar := g.tomarPausada(v.id, v.p, vieja)
		if soltar == nil {
			continue // alguien la está despertando: ya no es vieja
		}
		// Si el freeze falla, soltar la repone: perderla del registro la
		// dejaría reteniendo su RAM sin ningún dueño en el planificador (G-04)
		// — solo el TTL del daemon (2×idle) acabaría congelándola, y
		// renovarTTL puede seguir aplazándolo si algo la adopta.
		soltar(g.congelarPausada(ctx, v.id))
	}
}

// tomarPausadaMasVieja aparta para congelarla (ver tomarPausada) la pausada
// más antigua que nadie esté despertando, para evictLRU. Devuelve su valor y
// con qué soltarla; soltar es nil si no hay ninguna.
func (g *Scheduler) tomarPausadaMasVieja() (string, pausada, func(congelada bool)) {
	type candidata struct {
		id string
		p  pausada
	}
	var cands []candidata
	g.mu.Lock()
	for id, p := range g.pausadas {
		if !g.adquiriendo[id] {
			cands = append(cands, candidata{id, p})
		}
	}
	g.mu.Unlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].p.at.Before(cands[j].p.at) })
	for _, c := range cands {
		if soltar := g.tomarPausada(c.id, c.p, nil); soltar != nil {
			return c.id, c.p, soltar
		}
	}
	return "", pausada{}, nil
}

// candadoPausada es el candado bajo el que se despierta esa máquina: el del
// servicio (ensure → acquire) o, si es de una sesión aislada, el de la sesión
// (isolatedSession → despertarAislada).
func (g *Scheduler) candadoPausada(p pausada) *sync.Mutex {
	if p.aislada != "" {
		return g.aisladaLock(p.aislada)
	}
	return g.ensureLock(p.service)
}

// tomarPausada saca del registro la pausada id para congelarla, con el mismo
// TryLock que evictLRU toma sobre sus víctimas despiertas, y la marca en
// g.adquiriendo mientras dura el freeze. Sin las dos cosas, un acquire
// concurrente la veía "paused" en su List(), la reanudaba y el freeze, que
// llegaba después, congelaba una instancia recién adoptada: 502 en su primera
// petición. El candado detiene a ensure y a isolatedSession hasta que acabe el
// freeze (y entonces la descongelan); la marca, al scale-out, que no lo toma.
//
// Devuelve nil si el candado está ocupado (alguien la está despertando), si ya
// no figura en el registro, si alguien la eligió o si deja de cumplir vale
// (nil: cualquiera). Si no, devuelve con qué soltarla al acabar: con
// congelada=false la repone en el registro (G-04).
func (g *Scheduler) tomarPausada(id string, p pausada, vale func(pausada) bool) func(congelada bool) {
	l := g.candadoPausada(p)
	if !l.TryLock() {
		return nil
	}
	g.mu.Lock()
	actual, sigue := g.pausadas[id]
	if !sigue || g.adquiriendo[id] || (vale != nil && !vale(actual)) {
		g.mu.Unlock()
		l.Unlock()
		return nil
	}
	delete(g.pausadas, id)
	if g.adquiriendo == nil {
		g.adquiriendo = map[string]bool{}
	}
	g.adquiriendo[id] = true
	g.mu.Unlock()
	return func(congelada bool) {
		g.mu.Lock()
		if !congelada {
			if g.pausadas == nil {
				g.pausadas = map[string]pausada{}
			}
			g.pausadas[id] = actual
		}
		delete(g.adquiriendo, id)
		g.mu.Unlock()
		l.Unlock()
	}
}

func (g *Scheduler) congelarPausada(ctx context.Context, id string) bool {
	congelar := g.freezeFn
	if congelar == nil {
		congelar = func(id string) error { _, err := g.client.Freeze(ctx, id); return err }
	}
	if err := congelar(id); err != nil {
		log.Printf("freeze paused %s: %v", short(id), err)
		return false
	}
	return true
}
