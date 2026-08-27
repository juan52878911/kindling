package machine

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

func conVolumen(t *testing.T, nombre string) *Manager {
	t.Helper()
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath(nombre), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return m
}

// El fallo original: entre COMPROBAR que nadie escribe y PUBLICAR la maquina en
// byID habia una ventana en la que ningun cerrojo se sostenia. Dos arranques
// simultaneos —un `kling run -volume` del CLI y un scaleOut del gateway— veian
// los dos "cero escritores" y montaban el MISMO ext4 en escritura.
//
// No da error: corrompe el sistema de ficheros del usuario, y el sintoma llega
// mucho despues.
func TestDosArranquesSimultaneosNoMontanElMismoVolumenEnEscritura(t *testing.T) {
	m := conVolumen(t, "datos")
	req := api.RunRequest{Volume: "datos"}

	const intentos = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitidos []string

	arranque := make(chan struct{})
	for i := 0; i < intentos; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-arranque // todos a la vez, para que la ventana sea maxima
			id := newID()
			_, err := m.reservarVolumenes(req, id, "maquina")
			if err == nil {
				mu.Lock()
				admitidos = append(admitidos, id)
				mu.Unlock()
			}
		}(i)
	}
	close(arranque)
	wg.Wait()

	if len(admitidos) != 1 {
		t.Errorf("entraron %d escritores al mismo volumen; solo puede haber UNO", len(admitidos))
	}
}

// Y el control positivo, que es lo que demuestra que la reserva hace falta:
// comprobar SIN reservar admite a los dos, porque ninguno esta todavia en byID.
func TestComprobarSinReservarAdmiteADosYPorEsoNoSeUsaAlArrancar(t *testing.T) {
	m := conVolumen(t, "datos")
	req := api.RunRequest{Volume: "datos"}

	if _, err := m.resolveVolumes(req); err != nil {
		t.Fatalf("el primero deberia pasar: %v", err)
	}
	if _, err := m.resolveVolumes(req); err != nil {
		t.Fatalf("el segundo TAMBIEN pasa, y ese es justo el problema: %v", err)
	}
	// Con reserva, el segundo se queda fuera.
	if _, err := m.reservarVolumenes(req, newID(), "a"); err != nil {
		t.Fatalf("el primero deberia reservar: %v", err)
	}
	if _, err := m.reservarVolumenes(req, newID(), "b"); err == nil {
		t.Error("el segundo entro pese a la reserva del primero")
	}
}

// Una reserva que no se suelta bloquearia el volumen para siempre: un arranque
// que falla —disco lleno, imagen rota— dejaria el volumen inservible sin que
// nada lo explicara.
func TestUnaReservaSeSueltaYElVolumenVuelveAEstarLibre(t *testing.T) {
	m := conVolumen(t, "datos")
	req := api.RunRequest{Volume: "datos"}

	id := newID()
	if _, err := m.reservarVolumenes(req, id, "la-que-fallo"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.reservarVolumenes(req, newID(), "otra"); err == nil {
		t.Fatal("deberia estar reservado")
	}

	m.soltarReservas(id)

	if _, err := m.reservarVolumenes(req, newID(), "otra"); err != nil {
		t.Errorf("tras soltar, el volumen deberia estar libre: %v", err)
	}
}

// Varios lectores SI pueden convivir: la exclusion es para la escritura.
func TestVariosLectoresPuedenReservarElMismoVolumen(t *testing.T) {
	m := conVolumen(t, "datos")
	req := api.RunRequest{Volume: "datos", VolumeReadOnly: true}

	for i := 0; i < 3; i++ {
		if _, err := m.reservarVolumenes(req, newID(), "lector"); err != nil {
			t.Fatalf("el lector %d fue rechazado: %v", i+1, err)
		}
	}
	// Pero un escritor no entra mientras haya lectores.
	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "datos"}, newID(), "escritor"); err == nil {
		t.Error("un escritor entro con lectores dentro")
	}
}

// El mensaje tiene que decir QUIEN lo tiene, tambien cuando quien lo tiene aun
// esta arrancando: "lo tiene alguien" sin nombre no deja hacer nada.
func TestElErrorNombraAQuienLoTieneAunqueEsteArrancando(t *testing.T) {
	m := conVolumen(t, "datos")
	req := api.RunRequest{Volume: "datos"}

	if _, err := m.reservarVolumenes(req, newID(), "la-primera"); err != nil {
		t.Fatal(err)
	}
	_, err := m.reservarVolumenes(req, newID(), "la-segunda")
	if err == nil {
		t.Fatal("deberia haber sido rechazada")
	}
	if !strings.Contains(err.Error(), "la-primera") {
		t.Errorf("el error no dice quien lo tiene: %v", err)
	}
	if !strings.Contains(err.Error(), "starting") {
		t.Errorf("el error no dice que esta arrancando, que explica por que no sale en `kling ps`: %v", err)
	}
}
