package frontal

import (
	"sync"
	"testing"
	"time"
)

func listadosDe(f *falso) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sandboxesLlamadas
}

// Un subdominio cuyo sandbox no existe se recuerda un momento: antes cada
// petición, también sin credencial, volvía a listar el host entero.
func TestIngresoCacheNegativa(t *testing.T) {
	b := montarIngreso(t, nil)
	b.id = "a/noexiste"
	antes := listadosDe(b.f)
	for range 5 {
		if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode == 200 {
			t.Fatalf("sandbox que no existe = %d", resp.StatusCode)
		}
	}
	if n := listadosDe(b.f) - antes; n != 1 {
		t.Errorf("%d listados del host para 5 peticiones al mismo id ausente, quería 1", n)
	}
}

// Las peticiones que llegan a la vez sin entrada en la caché comparten un
// listado del host en vez de lanzar uno cada una.
func TestIngresoListadoUnico(t *testing.T) {
	b := montarIngreso(t, nil)
	b.f.mu.Lock()
	b.f.sandboxesEspera, b.f.sandboxesEmpezado = make(chan struct{}), make(chan struct{})
	b.f.mu.Unlock()
	antes := listadosDe(b.f)
	var wg sync.WaitGroup
	codigos := make([]int, 8)
	for i := range codigos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := b.get(t, 3000, "/", nil)
			codigos[i] = resp.StatusCode
		}()
	}
	<-b.f.sandboxesEmpezado
	time.Sleep(200 * time.Millisecond)
	n := listadosDe(b.f) - antes
	b.f.mu.Lock()
	close(b.f.sandboxesEspera)
	b.f.mu.Unlock()
	wg.Wait()
	if n != 1 {
		t.Errorf("%d listados a la vez para el mismo host, quería 1", n)
	}
	for i, c := range codigos {
		if c != 401 {
			t.Errorf("petición %d = %d, quería 401 (sin token)", i, c)
		}
	}
}

// Borrar el sandbox (olvidar) no deja que un listado anterior lo resucite, y
// un id que estaba en la caché negativa vuelve a mirarse tras olvidar.
func TestIngresoCacheNegativaSeOlvida(t *testing.T) {
	b := montarIngreso(t, nil)
	real := b.id
	b.id = "a/noexiste"
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode == 200 {
		t.Fatalf("ausente = %d", resp.StatusCode)
	}
	b.s.ingreso.olvidar(b.id)
	antes := listadosDe(b.f)
	b.get(t, 3000, "/", nil)
	if n := listadosDe(b.f) - antes; n != 1 {
		t.Errorf("tras olvidar, %d listados; quería 1 (la caché negativa debía irse)", n)
	}
	b.id = real
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode != 401 {
		t.Errorf("el sandbox real = %d, quería 401", resp.StatusCode)
	}
}
