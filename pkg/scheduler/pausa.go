package scheduler

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
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
	if g.PausedMiB > 0 && (g.pauseFn != nil || g.sabePausar(ctx)) {
		score := func(v dormida) float64 { return g.pop.score(v.service) / float64(v.memMiB) }
		cands := make([]dormida, 0, len(victims))
		for _, v := range victims {
			if v.memMiB > 0 && v.memMiB <= g.PausedMiB && g.pop.score(v.service) > 0 {
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
			if usado+v.memMiB <= g.PausedMiB {
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

// enfriarPausadas congela de verdad las pausadas que llevan más de pausedFor
// sin usarse.
func (g *Scheduler) enfriarPausadas(ctx context.Context) {
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
