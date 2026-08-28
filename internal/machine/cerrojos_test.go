package machine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// La propiedad que importa: NUNCA dos dentro a la vez para el mismo id.
//
// El registro anterior la perdia justo al borrar una maquina: quien esperaba se
// quedaba con el mutex viejo y el siguiente creaba otro. Aqui se fuerza mucha
// rotacion —cada suelta puede retirar la entrada— para dar todas las
// oportunidades a esa carrera.
func TestNuncaDosDentroDelMismoCerrojo(t *testing.T) {
	c := nuevosCerrojos()
	var dentro int32
	var choques int32

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				soltar := c.tomar("la-misma")
				if atomic.AddInt32(&dentro, 1) != 1 {
					atomic.AddInt32(&choques, 1)
				}
				time.Sleep(time.Microsecond)
				atomic.AddInt32(&dentro, -1)
				soltar()
			}
		}()
	}
	wg.Wait()

	if choques > 0 {
		t.Errorf("%d veces hubo dos dentro a la vez: la exclusión no se sostiene", choques)
	}
	if n := c.vivos(); n != 0 {
		t.Errorf("quedaron %d entradas; el registro debe vaciarse solo", n)
	}
}

// Ids distintos no se estorban: si se serializaran, dos microVMs no podrian
// arrancar a la vez y el arranque en lote se volveria secuencial.
func TestIdsDistintosNoSeBloquean(t *testing.T) {
	c := nuevosCerrojos()
	soltarA := c.tomar("a")
	defer soltarA()

	hecho := make(chan struct{})
	go func() { c.tomar("b")(); close(hecho) }()

	select {
	case <-hecho:
	case <-time.After(2 * time.Second):
		t.Fatal("tomar 'b' se bloqueó teniendo 'a': los ids se están serializando entre sí")
	}
}

// La entrada sobrevive mientras alguien la ESPERE. Si se retirara con el
// contador aun por encima de cero, quien espera acabaria sosteniendo un mutex
// que ya no es el de nadie — que es exactamente el fallo del registro anterior.
func TestLaEntradaSobreviveMientrasAlguienEspera(t *testing.T) {
	c := nuevosCerrojos()
	soltar1 := c.tomar("x")

	esperando := make(chan struct{})
	dentro := make(chan struct{})
	go func() {
		close(esperando)
		soltar2 := c.tomar("x") // se queda esperando
		close(dentro)
		soltar2()
	}()
	<-esperando
	time.Sleep(50 * time.Millisecond) // que llegue a esperar de verdad

	if n := c.vivos(); n != 1 {
		t.Fatalf("hay %d entradas; con uno dentro y otro esperando debe haber 1", n)
	}
	soltar1()

	select {
	case <-dentro:
	case <-time.After(2 * time.Second):
		t.Fatal("el que esperaba no entró tras soltar")
	}
	// Y ahora sí se retira.
	limite := time.Now().Add(time.Second)
	for time.Now().Before(limite) {
		if c.vivos() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("la entrada no se retiró: quedan %d", c.vivos())
}

// Un registro recien creado por valor (como el campo del Manager) tiene que
// funcionar sin inicializar: es como se usa en NewManager.
func TestElRegistroFuncionaSinInicializar(t *testing.T) {
	var c cerrojos
	soltar := c.tomar("y")
	if n := c.vivos(); n != 1 {
		t.Errorf("vivos = %d, esperaba 1", n)
	}
	soltar()
	if n := c.vivos(); n != 0 {
		t.Errorf("vivos = %d tras soltar, esperaba 0", n)
	}
}
