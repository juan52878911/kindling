package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func gwConDosOciosas(t *testing.T) (*Scheduler, *int) {
	t.Helper()
	congeladas := 0
	g := &Scheduler{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		freezeFn: func(id string) error { congeladas++; return nil },
	}
	ahora := time.Now()
	g.services["vieja"] = &entry{machineID: "m-vieja", lastUse: ahora.Add(-10 * time.Minute)}
	g.services["nueva"] = &entry{machineID: "m-nueva", lastUse: ahora.Add(-1 * time.Minute)}
	return g, &congeladas
}

// Si la victima mas antigua esta ocupada en su propio ensure, evictLRU se
// rendia: devolvia "" y el llamador respondia 507 al cliente aunque hubiera
// otra instancia ociosa perfectamente sacrificable.
func TestSiLaVictimaEstaOcupadaSePruebaConOtra(t *testing.T) {
	g, congeladas := gwConDosOciosas(t)

	// Alguien tiene tomado el candado de la mas antigua.
	ocupado := g.ensureLock("vieja")
	ocupado.Lock()
	defer ocupado.Unlock()

	got := g.evictLRU(context.Background(), "otro-servicio", "")
	if got != "nueva" {
		t.Errorf("evictLRU = %q; deberia haber sacrificado 'nueva' en vez de rendirse", got)
	}
	if *congeladas != 1 {
		t.Errorf("se congelaron %d instancias, esperaba 1", *congeladas)
	}
}

// Y la que se probo sin exito tiene que VOLVER al mapa. Sacarla y no congelarla
// deja al gateway creyendo que no existe: el siguiente ensure la adopta desde
// List() cuando ya no toca, y espera 20 s a un invitado que se esta pausando.
func TestLaVictimaQueNoSePudoCongelarVuelveAlMapa(t *testing.T) {
	g, _ := gwConDosOciosas(t)

	ocupado := g.ensureLock("vieja")
	ocupado.Lock()
	defer ocupado.Unlock()

	g.evictLRU(context.Background(), "otro-servicio", "")

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.services["vieja"] == nil && len(g.extra["vieja"]) == 0 {
		t.Error("'vieja' se saco del mapa y no se repuso: el gateway cree que ya no existe")
	}
}

// Sin ninguna candidata, "" sigue siendo la respuesta correcta.
func TestSinCandidatasDevuelveVacio(t *testing.T) {
	g := &Scheduler{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		freezeFn: func(id string) error { return nil },
	}
	// Una sola, y es justo la que hay que salvar.
	g.services["yo"] = &entry{machineID: "m-yo", lastUse: time.Now()}
	if got := g.evictLRU(context.Background(), "yo", ""); got != "" {
		t.Errorf("evictLRU = %q; no habia a quien sacrificar", got)
	}
}

// Una instancia con peticiones en vuelo no se toca, aunque sea la mas antigua.
func TestNoSeSacrificaUnaConPeticionesEnVuelo(t *testing.T) {
	g, _ := gwConDosOciosas(t)
	g.services["vieja"].inflight = 1

	if got := g.evictLRU(context.Background(), "otro", ""); got != "nueva" {
		t.Errorf("evictLRU = %q; 'vieja' esta atendiendo y no debe tocarse", got)
	}
}

// El tope de máquinas del daemon no es falta de memoria: congelar no baja el
// contador, así que la única salida sin perder trabajo de nadie es soltar una
// precalentada. Antes el error ni se reconocía y el cliente se lo comía.
func TestTopeDeMaquinasSeDistingueDeLaFaltaDeMemoria(t *testing.T) {
	tope := &api.StatusError{Code: api.StatusMachineLimit,
		Message: "machine limit reached: 256 of 256 machines (200 running, 56 warm, 0 failed, 0 stopped)"}
	if !api.IsMachineLimit(tope) {
		t.Error("no reconoce el tope de máquinas")
	}
	if api.IsInsufficientMemory(tope) {
		t.Error("lo confunde con falta de memoria")
	}
	// Un 409 cualquiera no es el tope: hay más cosas que contestan 409.
	if api.IsMachineLimit(&api.StatusError{Code: 409, Message: "the machine is warm"}) {
		t.Error("cualquier 409 pasa por tope de máquinas")
	}
	if api.IsMachineLimit(&api.StatusError{Code: api.StatusInsufficientMemory, Message: "doesn't fit"}) {
		t.Error("un 507 pasa por tope de máquinas")
	}
}
