package machine

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un pánico en un paso del vigilante se pierde ESE paso, no el resto de la
// vuelta: antes iba toda bajo un solo Contener y un pánico en sweep dejaba sin
// TTL, sin gc y sin presión de memoria a todas las máquinas, vuelta tras
// vuelta.
func TestPanicoEnUnPasoDelVigilanteNoSeLlevaLosDemas(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	var vistos []string
	m.pruebasPasoVigilante = func(nombre string) bool {
		vistos = append(vistos, nombre)
		if nombre == "sweep" {
			panic("sweep roto")
		}
		return false // no correr el paso de verdad
	}
	m.vueltaVigilante(context.Background())
	for _, quiero := range []string{"sweep", "ttl", "gcFailed", "presion"} {
		if !slices.Contains(vistos, quiero) {
			t.Fatalf("el paso %q no corrió (vistos: %v)", quiero, vistos)
		}
	}
}

// El TTL no frena al vigilante: una máquina cuyo cerrojo está tomado (un
// freeze lento) no retiene la vuelta, y las demás vencidas se atienden a la
// vez, de dos en dos.
func TestTTLEnSegundoPlanoNoBloqueaAlVigilante(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	viejo := time.Now().Add(-time.Hour)
	ids := []string{newID(), newID(), newID()}
	for i, id := range ids {
		if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		m.byID[id] = &api.Machine{ID: id, Name: "s" + string(rune('a'+i)), State: api.StateWarm,
			TTLSeconds: 60, OnTTL: api.OnTTLRemove, TTLAt: &viejo, CreatedAt: time.Now()}
	}
	soltar := m.lock(ids[0])
	defer soltar()

	vuelve := make(chan struct{})
	go func() {
		m.expireTTLEnSegundoPlano(context.Background())
		close(vuelve)
	}()
	select {
	case <-vuelve:
	case <-time.After(time.Second):
		t.Fatal("el vigilante se quedó esperando al TTL de una máquina")
	}
	limite := time.Now().Add(5 * time.Second)
	for {
		_, b := m.Get(ids[1])
		_, c := m.Get(ids[2])
		if !b && !c {
			break
		}
		if time.Now().After(limite) {
			t.Fatal("con una máquina retenida, las otras vencidas no se atendieron")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := m.Get(ids[0]); !ok {
		t.Fatal("se borró la que tenía el cerrojo tomado")
	}
}
