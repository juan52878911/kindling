package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/panico"
)

// REPUESTOS (C9-08, ronda 9).
//
// Con idle remove, cada despertar de un modelo es restaurar del dorado y pagar
// la primera petición: ~1 s medido en EC2 para VON. Un repuesto es una
// instancia restaurada, pasada por AfterRestore, por la sonda y por WarmSpare
// (la petición de calentamiento del contrato), que espera sin petición
// asignada. acquire la reclama antes de descongelar o restaurar, y el
// planificador repone otra en segundo plano.
//
// Un repuesto no es una instancia: no está en g.services ni en g.extra, así
// que no cuenta para el tope de réplicas, la cuota de instancias de un
// inquilino ni el trabajo en vuelo, y el segador no lo duerme. Sí ocupa
// memoria en el daemon (la admisión lo cuenta), y el desalojo por memoria lo
// sacrifica después de todo lo demás. Nunca se congela: al drenar, al
// retirarlo o al sobrar se borra.
//
// Lo distingue la etiqueta LabelSpare: acquire no adopta una máquina que la
// lleve, y quien arranca (el gateway de IA) borra los que dejó un proceso
// anterior.

// LabelSpare marca una máquina de repuesto. Solo la reclama el planificador
// que la creó, por tomarRepuesto.
const LabelSpare = "kling.spare"

// repuesto es una instancia de repuesto lista.
type repuesto struct {
	mc        *api.Machine
	port      int
	born      time.Time
	renewedAt time.Time
}

// errRetiradaAlNacer: un Retire del servicio llegó mientras la instancia
// pasaba por AfterRestore; lo que se le entregó puede ser de la generación
// anterior.
var errRetiradaAlNacer = errors.New("the service was retired while this replica started")

// spareFor es cuántos repuestos quiere el servicio (0 sin gancho). Con g.mu
// suelto.
func (g *Scheduler) spareFor(service string) int {
	if g.SpareFor == nil {
		return 0
	}
	return max(g.SpareFor(service), 0)
}

// retiroDe es la generación de retiro del servicio (ver Retire).
func (g *Scheduler) retiroDe(service string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.retiros[service]
}

// despuesDeRestaurar llama a AfterRestore, si está, con una instancia recién
// restaurada. El error no repite el del gancho en el log: lo devuelve a quien
// llama, y el gancho es quien garantiza que no lleva el secreto.
func (g *Scheduler) despuesDeRestaurar(ctx context.Context, service string, mc *api.Machine) error {
	if g.AfterRestore == nil {
		return nil
	}
	if err := g.AfterRestore(ctx, service, mc); err != nil {
		return fmt.Errorf("after-restore hook: %w", err)
	}
	return nil
}

// FillSpares apunta que el servicio mantiene repuestos (SpareFor) y repone
// en segundo plano los que falten. Si sobran (SpareFor bajó), borra los de
// más. El segador lo repite en cada vuelta para los servicios apuntados, así
// que basta con llamarlo una vez (el gateway de IA lo hace al arrancar y en
// cada Reload). Vuelve enseguida.
func (g *Scheduler) FillSpares(ctx context.Context, service string) {
	g.mu.Lock()
	if g.conRepuesto == nil {
		g.conRepuesto = map[string]bool{}
	}
	g.conRepuesto[service] = true
	g.mu.Unlock()
	g.reponerRepuestos(ctx, service)
}

// reponerRepuestos lanza las reposiciones que faltan para tener SpareFor
// repuestos (contando los que ya nacen) y borra los que sobran.
func (g *Scheduler) reponerRepuestos(ctx context.Context, service string) {
	want := g.spareFor(service)
	g.mu.Lock()
	if g.repuestosCerrado {
		g.mu.Unlock()
		return
	}
	var sobran []string
	if l := g.repuestos[service]; len(l) > want {
		for _, r := range l[want:] {
			sobran = append(sobran, r.mc.ID)
		}
		if want == 0 {
			delete(g.repuestos, service)
		} else {
			g.repuestos[service] = l[:want:want]
		}
	}
	faltan := want - len(g.repuestos[service]) - g.reponiendo[service]
	if faltan > 0 {
		if g.reponiendo == nil {
			g.reponiendo = map[string]int{}
		}
		g.reponiendo[service] += faltan
		// Add dentro del candado: DrainSpares cierra con él tomado, así que
		// ninguna reposición empieza después de que se ponga a esperar.
		g.repuestoWG.Add(faltan)
	}
	g.mu.Unlock()
	if len(sobran) > 0 {
		go g.borrarApartadas(context.WithoutCancel(ctx), service, sobran, "spare no longer wanted")
	}
	for i := 0; i < faltan; i++ {
		go func() {
			defer g.repuestoWG.Done()
			defer func() {
				g.mu.Lock()
				if g.reponiendo[service]--; g.reponiendo[service] <= 0 {
					delete(g.reponiendo, service)
				}
				g.mu.Unlock()
			}()
			panico.Contener("scheduler.spare", func() { g.unRepuesto(ctx, service) })
		}()
	}
}

// unRepuesto crea un repuesto y lo deja en la cola del servicio, o lo borra
// si mientras nacía se cerró, se retiró el servicio o ya no hace falta.
func (g *Scheduler) unRepuesto(ctx context.Context, service string) {
	retiro := g.retiroDe(service)
	r, err := g.crearRepuesto(ctx, service)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("%s: could not prepare a spare: %v", service, err)
		}
		return
	}
	want := g.spareFor(service)
	g.mu.Lock()
	sobra := g.repuestosCerrado || g.retiros[service] != retiro || len(g.repuestos[service]) >= want
	if !sobra {
		if g.repuestos == nil {
			g.repuestos = map[string][]*repuesto{}
		}
		g.repuestos[service] = append(g.repuestos[service], r)
	}
	g.mu.Unlock()
	if sobra {
		g.borrarApartadas(context.WithoutCancel(ctx), service, []string{r.mc.ID}, "spare not needed")
		return
	}
	log.Printf("%s: spare %s ready", service, short(r.mc.ID))
}

// crearRepuesto restaura una instancia del dorado y la deja lista: escucha,
// pasó AfterRestore, la sonda ReadyFor y WarmSpare. Si algo falla, la borra.
func (g *Scheduler) crearRepuesto(ctx context.Context, service string) (*repuesto, error) {
	mc, err := g.runFreshWith(ctx, service, map[string]string{LabelSpare: "true"})
	if err != nil {
		return nil, err
	}
	port := g.portFor(service)
	sonda := g.readyFor(service)
	espera := plazoListo(sonda, "restore")
	hasta := time.Now().Add(espera)
	err = g.esperarListoEn(ctx, mc, port, espera)
	if err == nil {
		err = g.despuesDeRestaurar(ctx, service, mc)
	}
	if err == nil && sonda != nil {
		err = esperarSonda(ctx, mc.Addr(port), sonda, hasta)
	}
	if err == nil && g.WarmSpare != nil {
		if werr := g.WarmSpare(ctx, service, mc.Addr(port)); werr != nil {
			err = fmt.Errorf("warming the spare: %w", werr)
		}
	}
	if err != nil {
		g.borrarApartadas(context.WithoutCancel(ctx), service, []string{mc.ID}, "spare failed to start")
		return nil, err
	}
	ahora := time.Now()
	return &repuesto{mc: mc, port: port, born: ahora, renewedAt: ahora}, nil
}

// tomarRepuesto reclama un repuesto del servicio para acquire: lo marca en
// g.adquiriendo (como toda máquina que acquire devuelve) y lanza su
// reposición. nil si no hay ninguno vivo. Comprueba con un dial que su puerto
// sigue aceptando: el daemon pudo borrarlo por debajo (TTL, falta de disco).
func (g *Scheduler) tomarRepuesto(ctx context.Context, service string) *api.Machine {
	for {
		g.mu.Lock()
		l := g.repuestos[service]
		if len(l) == 0 {
			g.mu.Unlock()
			return nil
		}
		r := l[0]
		if len(l) == 1 {
			delete(g.repuestos, service)
		} else {
			g.repuestos[service] = l[1:]
		}
		if g.adquiriendo == nil {
			g.adquiriendo = map[string]bool{}
		}
		g.adquiriendo[r.mc.ID] = true
		g.mu.Unlock()
		// Se repone ya, sin el plazo de la petición: el repuesto nuevo es
		// para la siguiente.
		g.reponerRepuestos(context.WithoutCancel(ctx), service)
		if aliveAddr(r.mc.Addr(r.port)) {
			// El TTL se renueva aparte, como al reanudar una pausada.
			go g.renovarTTL(context.WithoutCancel(ctx), r.mc.ID)
			log.Printf("%s: claimed spare %s", service, short(r.mc.ID))
			return r.mc
		}
		log.Printf("%s: spare %s no longer answers; dropping it", service, short(r.mc.ID))
		g.borrarApartadas(context.WithoutCancel(ctx), service, []string{r.mc.ID}, "dead spare")
	}
}

// evictRepuesto borra el repuesto más viejo de un servicio distinto de salvo
// para hacer sitio. Devuelve si borró alguno.
func (g *Scheduler) evictRepuesto(ctx context.Context, salvo string) bool {
	g.mu.Lock()
	var elegido *repuesto
	var deSvc string
	for svc, l := range g.repuestos {
		if svc == salvo || len(l) == 0 {
			continue
		}
		if elegido == nil || l[0].born.Before(elegido.born) {
			elegido, deSvc = l[0], svc
		}
	}
	if elegido != nil {
		if l := g.repuestos[deSvc][1:]; len(l) == 0 {
			delete(g.repuestos, deSvc)
		} else {
			g.repuestos[deSvc] = l
		}
	}
	g.mu.Unlock()
	if elegido == nil {
		return false
	}
	if err := g.borrarConPlazo(ctx, elegido.mc.ID); err != nil {
		log.Printf("%s: could not remove spare %s to make room: %v", deSvc, short(elegido.mc.ID), err)
		return false
	}
	log.Printf("%s: removed spare %s to make room", deSvc, short(elegido.mc.ID))
	return true
}

// mantenerRepuestos es la parte del segador de los repuestos: renueva el TTL
// de los que llevan umbral sin renovar (los que el daemon rechaza se
// descartan) y repone los servicios apuntados con FillSpares.
func (g *Scheduler) mantenerRepuestos(ctx context.Context, umbral time.Duration) {
	g.mu.Lock()
	if g.repuestosCerrado || len(g.conRepuesto) == 0 {
		g.mu.Unlock()
		return
	}
	type porRenovar struct {
		service string
		r       *repuesto
	}
	var renovar []porRenovar
	if g.ttlSeconds() > 0 {
		for svc, l := range g.repuestos {
			for _, r := range l {
				if time.Since(r.renewedAt) >= umbral {
					renovar = append(renovar, porRenovar{svc, r})
				}
			}
		}
	}
	svcs := make([]string, 0, len(g.conRepuesto))
	for svc := range g.conRepuesto {
		svcs = append(svcs, svc)
	}
	g.mu.Unlock()
	for _, x := range renovar {
		err := g.renovarTTL(ctx, x.r.mc.ID)
		g.mu.Lock()
		switch {
		case err == nil:
			x.r.renewedAt = time.Now()
		case renovacionRechazada(err):
			l := g.repuestos[x.service]
			for i, r := range l {
				if r == x.r {
					g.repuestos[x.service] = append(l[:i:i], l[i+1:]...)
					break
				}
			}
			if len(g.repuestos[x.service]) == 0 {
				delete(g.repuestos, x.service)
			}
		}
		g.mu.Unlock()
	}
	for _, svc := range svcs {
		g.reponerRepuestos(ctx, svc)
	}
}

// DrainSpares borra los repuestos y deja de reponerlos (para siempre: es para
// el cierre). Espera, acotado por drainGrace, a los que estén naciendo; los
// que acaben después se borran solos al ver el cierre. Drain lo llama; el
// gateway de IA, que no usa Drain, lo llama al cerrar.
func (g *Scheduler) DrainSpares(ctx context.Context) {
	g.mu.Lock()
	g.repuestosCerrado = true
	var ids []string
	for _, l := range g.repuestos {
		for _, r := range l {
			ids = append(ids, r.mc.ID)
		}
	}
	g.repuestos = nil
	g.mu.Unlock()

	done := make(chan struct{})
	go func() { g.repuestoWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(drainGrace):
		log.Printf("spares still starting after %s; they remove themselves when they finish", drainGrace)
	}
	if len(ids) > 0 {
		g.borrarApartadas(ctx, "", ids, "spare drained")
	}
}

// Retire saca de servicio todas las instancias compartidas de service (no las
// de sesiones aisladas), sus pausadas y sus repuestos, para que ninguna
// petición nueva llegue a ellas: el gateway de IA lo llama al rotar un
// secreto en claro, cuyas réplicas viejas llevan el valor anterior. Las
// ociosas se borran ya; las que tienen peticiones en vuelo, cuando acaba la
// última (end), sin cortarlas. La siguiente petición restaura otra con el
// valor nuevo, y los repuestos se reponen.
func (g *Scheduler) Retire(ctx context.Context, service string) {
	g.mu.Lock()
	if g.retiros == nil {
		g.retiros = map[string]int{}
	}
	g.retiros[service]++
	if g.adquiriendo == nil {
		g.adquiriendo = map[string]bool{}
	}
	var ya []string
	for _, e := range g.entriesLocked(service) {
		g.removeEntryLocked(service, e.machineID)
		// Marcada en adquiriendo hasta que se borre: fuera del mapa y
		// corriendo, un acquire la adoptaría de List().
		g.adquiriendo[e.machineID] = true
		if e.inflight > 0 {
			e.retirada = true
			continue
		}
		ya = append(ya, e.machineID)
	}
	for id, p := range g.pausadas {
		if p.service == service && p.aislada == "" && !g.adquiriendo[id] {
			delete(g.pausadas, id)
			g.adquiriendo[id] = true
			ya = append(ya, id)
		}
	}
	for _, r := range g.repuestos[service] {
		ya = append(ya, r.mc.ID)
	}
	delete(g.repuestos, service)
	g.mu.Unlock()
	g.avisarOlvidos()
	if len(ya) > 0 {
		go g.borrarApartadas(context.WithoutCancel(ctx), service, ya, "retired")
	}
	g.reponerRepuestos(ctx, service)
}

// borrarApartadas borra las máquinas ids (fuera ya de todo registro del
// planificador) y las desmarca de g.adquiriendo. motivo va al log.
func (g *Scheduler) borrarApartadas(ctx context.Context, service string, ids []string, motivo string) {
	for _, id := range ids {
		if err := g.borrarConPlazo(ctx, id); err != nil {
			log.Printf("%s: could not remove %s (%s): %v", service, short(id), motivo, err)
		}
		g.mu.Lock()
		delete(g.adquiriendo, id)
		g.mu.Unlock()
	}
}
