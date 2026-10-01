package machine

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// esperarQueEspere espera a que alguien más que quien lo tiene esté esperando
// el cerrojo de ciclo de vida de id: es el momento en que la operación bajo
// prueba ya tomó su decisión con la foto y va a por el cerrojo.
func esperarQueEspere(t *testing.T, m *Manager, id string) {
	t.Helper()
	limite := time.Now().Add(5 * time.Second)
	for {
		m.lifecycle.mu.Lock()
		refs := 0
		if e := m.lifecycle.m[id]; e != nil {
			refs = e.refs
		}
		m.lifecycle.mu.Unlock()
		if refs >= 2 {
			return
		}
		if time.Now().After(limite) {
			t.Fatal("nadie llegó a esperar el cerrojo de la máquina")
		}
		time.Sleep(time.Millisecond)
	}
}

// renovarAMano hace lo que Renew hace con el reloj, sin su cerrojo (que en la
// prueba tiene tomado el propio test).
func renovarAMano(m *Manager, id string) {
	m.mu.Lock()
	ahora := time.Now()
	m.byID[id].TTLAt = &ahora
	m.mu.Unlock()
}

// El TTL decide con una foto y borra (on_ttl=remove) después. Un renew que
// llegaba entre las dos cosas contestaba que sí, y la máquina se borraba igual.
func TestTTLRemoveNoBorraUnaMaquinaRenovadaEntreMedias(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	viejo := time.Now().Add(-time.Hour)
	m.byID[id] = &api.Machine{ID: id, Name: "sandbox", State: api.StateRunning,
		TTLSeconds: 60, OnTTL: api.OnTTLRemove, TTLAt: &viejo}

	soltar := m.lock(id)
	hecho := make(chan struct{})
	go func() {
		m.expireTTL(context.Background())
		close(hecho)
	}()
	esperarQueEspere(t, m, id)
	renovarAMano(m, id)
	soltar()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("expireTTL no terminó")
	}
	if _, ok := m.Get(id); !ok {
		t.Fatal("el TTL borró una máquina que se había renovado antes de tomar su cerrojo")
	}
}

// Lo mismo con on_ttl=freeze: congelar una máquina recién renovada.
func TestTTLFreezeNoCongelaUnaMaquinaRenovadaEntreMedias(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	viejo := time.Now().Add(-time.Hour)
	m.byID[id] = &api.Machine{ID: id, Name: "servicio", State: api.StateRunning,
		TTLSeconds: 60, TTLAt: &viejo}
	falso := nuevoFcFalso(t)
	m.socket[id] = falso.Sock

	soltar := m.lock(id)
	hecho := make(chan struct{})
	go func() {
		m.expireTTL(context.Background())
		close(hecho)
	}()
	esperarQueEspere(t, m, id)
	renovarAMano(m, id)
	soltar()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("expireTTL no terminó")
	}
	if n := len(falso.llamadasA("PATCH", "/vm")); n != 0 {
		t.Fatalf("el TTL pausó (%d veces) una máquina que se había renovado", n)
	}
	if mc, _ := m.Get(id); mc.State != api.StateRunning {
		t.Fatalf("estado = %s; la renovada debía seguir running", mc.State)
	}
}

// Renew toma el cerrojo de ciclo de vida: es lo que hace que la segunda
// mirada del TTL (con el cerrojo) no pueda quedarse vieja antes de matar.
func TestRenewEsperaAlCerrojoDeCicloDeVida(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, TTLSeconds: 60}

	soltar := m.lock(id)
	hecho := make(chan struct{})
	go func() {
		_, _ = m.Renew(id, 0)
		close(hecho)
	}()
	select {
	case <-hecho:
		soltar()
		t.Fatal("Renew no esperó al cerrojo: puede renovar debajo de un TTL que ya decidió matar")
	case <-time.After(200 * time.Millisecond):
	}
	soltar()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("Renew no terminó tras soltar el cerrojo")
	}
}

// La recogida de disco elige congeladas con una foto y las borra después. Si
// entre medias la despertaron, borrarla era matar una máquina en uso.
func TestGCDiskNoBorraUnaCongeladaQueSeDesperto(t *testing.T) {
	t.Setenv("KLING_GC_DISK_HIGH", "1") // cualquier disco está "lleno"
	m := newTestManager(t)
	m.bus = events.New()
	if m.diskUsedPct() < 1 {
		t.Skip("no se puede medir el disco del temporal")
	}
	escribirSnapshot(t, m, "dorado", api.Snapshot{})
	if _, err := m.loadSnapshot("dorado"); err != nil {
		t.Skipf("el snapshot de prueba no carga: %v", err)
	}
	id := newID()
	congelada := time.Now().Add(-time.Hour)
	m.byID[id] = &api.Machine{ID: id, Name: "dormida", State: api.StateWarm,
		From: "dorado", FrozenAt: &congelada, Labels: map[string]string{api.LabelService: "s"}}

	soltar := m.lock(id)
	hecho := make(chan struct{})
	go func() {
		m.gcDisk(context.Background())
		close(hecho)
	}()
	esperarQueEspere(t, m, id)
	// Un thaw (que tiene el cerrojo) la despierta.
	m.mu.Lock()
	m.byID[id].State = api.StateRunning
	m.byID[id].FrozenAt = nil
	m.mu.Unlock()
	soltar()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("gcDisk no terminó")
	}
	if _, ok := m.Get(id); !ok {
		t.Fatal("la recogida de disco borró una máquina que se despertó antes de tomar su cerrojo")
	}
}
