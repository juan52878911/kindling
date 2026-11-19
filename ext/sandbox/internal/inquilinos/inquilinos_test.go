package inquilinos

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type reloj struct {
	mu sync.Mutex
	t  time.Time
}

func (r *reloj) ahora() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.t }
func (r *reloj) avanza(d time.Duration) {
	r.mu.Lock()
	r.t = r.t.Add(d)
	r.mu.Unlock()
}

func abrir(t *testing.T, dir string, rl *reloj) *Almacen {
	t.Helper()
	a, err := Abrir(dir, Opciones{Ahora: rl.ahora})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUltimoUsoSeVuelcaAmortiguado(t *testing.T) {
	dir, rl := t.TempDir(), &reloj{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := abrir(t, dir, rl)
	if _, err := a.Crear("ana", false, Topes{}); err != nil {
		t.Fatal(err)
	}
	_, k, _ := a.NuevaClave("ana", nil)
	leer := func() []byte { b, _ := os.ReadFile(filepath.Join(dir, Fichero)); return b }
	antes := string(leer())
	for i := 0; i < 5; i++ {
		rl.avanza(time.Second)
		if _, ok := a.Resolver(k); !ok {
			t.Fatal("clave rechazada")
		}
	}
	if string(leer()) != antes {
		t.Error("cada uso escribió a disco")
	}
	rl.avanza(time.Minute)
	a.Resolver(k)
	if string(leer()) == antes {
		t.Error("el último uso no se volcó pasado el intervalo")
	}
	// Lo pendiente se vuelca al cerrar.
	rl.avanza(time.Second)
	a.Resolver(k)
	a.Cerrar()
	b := abrir(t, dir, rl)
	q, _ := b.Obtener("ana")
	if q.Claves[0].UltimoUso == nil || !q.Claves[0].UltimoUso.Equal(rl.ahora()) {
		t.Errorf("último uso tras reabrir: %v", q.Claves[0].UltimoUso)
	}
}

func TestFicheroCorruptoYPermisos(t *testing.T) {
	dir, rl := t.TempDir(), &reloj{t: time.Now()}
	ruta := filepath.Join(dir, Fichero)
	if err := os.WriteFile(ruta, []byte("{no es json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Abrir(dir, Opciones{Ahora: rl.ahora}); err == nil {
		t.Fatal("abre un fichero corrupto")
	}
	// Un fichero legible por todos se aprieta al abrir.
	if err := os.WriteFile(ruta, []byte(`{"version":1,"tenants":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	abrir(t, dir, rl)
	if st, _ := os.Stat(ruta); st.Mode().Perm() != 0o600 {
		t.Errorf("permisos %v", st.Mode().Perm())
	}
}

func TestUnFalloDeEscrituraNoCambiaElEstado(t *testing.T) {
	dir, rl := t.TempDir(), &reloj{t: time.Now()}
	a := abrir(t, dir, rl)
	a.Crear("ana", false, Topes{})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root escribe igual")
	}
	if _, err := a.Crear("bob", false, Topes{}); err == nil {
		t.Fatal("Crear no falló sin poder escribir")
	}
	if _, err := a.Obtener("bob"); err == nil {
		t.Error("bob quedó en memoria sin estar en disco")
	}
}

func TestConcurrencia(t *testing.T) {
	dir, rl := t.TempDir(), &reloj{t: time.Now()}
	a := abrir(t, dir, rl)
	a.Crear("ana", false, Topes{})
	_, k, _ := a.NuevaClave("ana", nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, ok := a.Resolver(k); !ok {
					t.Error("la clave dejó de valer")
				}
				a.Crear(fmt.Sprintf("t%d-%d", i, j), false, Topes{})
				a.Listar()
			}
		}(i)
	}
	wg.Wait()
	if n := a.Cuantos(); n != 161 {
		t.Errorf("%d tenants, want 161", n)
	}
	b := abrir(t, dir, rl)
	if b.Cuantos() != 161 {
		t.Errorf("tras reabrir, %d", b.Cuantos())
	}
}
