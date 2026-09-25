package scheduler

import (
	"context"
	"log"
	"sort"
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
		g.pausadas[v.id] = pausada{service: v.service, memMiB: v.memMiB, at: time.Now()}
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
	var viejas []string
	g.mu.Lock()
	for id, p := range g.pausadas {
		if time.Since(p.at) > g.pausedFor() && !g.adquiriendo[id] {
			viejas = append(viejas, id)
			delete(g.pausadas, id)
		}
	}
	g.mu.Unlock()
	for _, id := range viejas {
		g.congelarPausada(ctx, id)
	}
}

// pausadaMasVieja saca del registro la pausada más antigua (para evictLRU).
func (g *Scheduler) pausadaMasVieja() (string, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var id, svc string
	var at time.Time
	for k, p := range g.pausadas {
		if g.adquiriendo[k] {
			continue
		}
		if id == "" || p.at.Before(at) {
			id, svc, at = k, p.service, p.at
		}
	}
	if id != "" {
		delete(g.pausadas, id)
	}
	return id, svc
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
