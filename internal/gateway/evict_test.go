package gateway

import (
	"context"
	"testing"
	"time"
)

func gwConDosOciosas(t *testing.T) (*Gateway, *int) {
	t.Helper()
	congeladas := 0
	g := &Gateway{
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
	g := &Gateway{
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
