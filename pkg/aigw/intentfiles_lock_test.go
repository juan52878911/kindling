package aigw

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
	"github.com/juan52878911/kindling/pkg/codificador"
)

// getSlots (y getHead) tienen que leer el fichero FUERA de f.mu: una carga
// lenta de una tarea no puede parar las de las demás, que solo tocan un mapa
// ya en memoria. Este test deja la carga de una ruta bloqueada a propósito y
// comprueba que otra ruta no espera a que termine.
func TestIntentFilesGetSlotsCargaFueraDelCandado(t *testing.T) {
	orig := loadSlotsFn
	t.Cleanup(func() { loadSlotsFn = orig })

	started := make(chan struct{})
	block := make(chan struct{})
	loadSlotsFn = func(p string) (*slots.Model, error) {
		if p == "lenta" {
			close(started)
			<-block
		}
		return &slots.Model{}, nil
	}

	f := &intentFiles{}
	done := make(chan struct{})
	go func() {
		if _, err := f.getSlots("lenta"); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	<-started // la carga lenta ya está en marcha, sin el candado puesto

	otherDone := make(chan struct{})
	go func() {
		if _, err := f.getSlots("rapida"); err != nil {
			t.Error(err)
		}
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("getSlots de otra ruta esperó a la carga lenta: el candado se quedó puesto durante la carga")
	}
	close(block)
	<-done
}

// La misma carga a la vez, dos veces: la segunda comprobación bajo el
// candado asegura que solo una entrada gana y todas las llamadas concurrentes
// terminan viendo el mismo modelo.
func TestIntentFilesGetSlotsDobleComprobacion(t *testing.T) {
	orig := loadSlotsFn
	t.Cleanup(func() { loadSlotsFn = orig })

	var loads int32
	loadSlotsFn = func(p string) (*slots.Model, error) {
		atomic.AddInt32(&loads, 1)
		time.Sleep(5 * time.Millisecond)
		return &slots.Model{}, nil
	}

	f := &intentFiles{}
	const n = 8
	results := make([]*slots.Model, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := f.getSlots("mismo")
			if err != nil {
				t.Error(err)
			}
			results[i] = m
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("cargas concurrentes de la misma ruta no convergieron en el mismo modelo")
		}
	}
	if loads < 1 {
		t.Fatal("loadSlotsFn nunca se llamó")
	}
}

// getHead tiene el mismo candado, comprobado dos veces: mismo diseño.
func TestIntentFilesGetHeadCargaFueraDelCandado(t *testing.T) {
	orig := loadHeadFn
	t.Cleanup(func() { loadHeadFn = orig })

	started := make(chan struct{})
	block := make(chan struct{})
	loadHeadFn = func(p string) (*codificador.Head, error) {
		if p == "lenta" {
			close(started)
			<-block
		}
		return &codificador.Head{}, nil
	}

	f := &intentFiles{}
	done := make(chan struct{})
	go func() {
		if _, err := f.getHead("lenta"); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	<-started

	otherDone := make(chan struct{})
	go func() {
		if _, err := f.getHead("rapida"); err != nil {
			t.Error(err)
		}
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("getHead de otra ruta esperó a la carga lenta: el candado se quedó puesto durante la carga")
	}
	close(block)
	<-done
}
