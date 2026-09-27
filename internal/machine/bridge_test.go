package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/digest"
)

// Nunca se toca la imagen base de una microVM viva.
//
// Es la comprobación que evita el desastre: modificar en escritura un ext4 que
// otro sistema tiene montado lo corrompe, aunque él lo tenga en solo lectura. Y
// aquí "vivo" incluye a las warm: al descongelarse vuelven a leer de la imagen,
// que además está mapeada en su snapshot de memoria.
func TestNoSeTocaLaImagenDeUnaMaquinaViva(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ocupada", "libre"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	puente := filepath.Join(t.TempDir(), "kling-bridge")
	if err := os.WriteFile(puente, []byte("binario"), 0o755); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateWarm, Image: "ocupada"}
	m.mu.Unlock()

	res, err := m.PutImageFile(t.Context(), "ocupada", "/usr/local/bin/kling-bridge", puente, 0o755, false)
	if err == nil || !res.Busy {
		t.Fatalf("iba a tocar la imagen de una máquina viva: res=%+v err=%v", res, err)
	}
	if res.Updated {
		t.Error("dice que la actualizó pese a negarse")
	}
	// Y tiene que decir QUIÉN la retiene, o el usuario no sabe qué parar.
	if !strings.Contains(err.Error(), "svc-a") {
		t.Errorf("no dice quién la usa: %v", err)
	}

	// La libre sí se intenta (fallará al montar, que aquí no hay loop, pero no
	// se niega por estar en uso).
	res, _ = m.PutImageFile(t.Context(), "libre", "/usr/local/bin/kling-bridge", puente, 0o755, false)
	if res.Busy {
		t.Error("se negó con una imagen que no usa nadie")
	}
}

// Sin fichero de origen se dice cuál falta en vez de fallar seco.
func TestPutImageFileSinOrigen(t *testing.T) {
	m := newTestManager(t)
	_, err := m.PutImageFile(t.Context(), "x", "/etc/x", "/no/existe", 0o644, false)
	if err == nil || !strings.Contains(err.Error(), "/no/existe") {
		t.Fatalf("el error no dice qué fichero falta: %v", err)
	}
}

// overlay-template no es una imagen de rootfs: es el molde vacío del disco de
// escritura de cada máquina y no lleva puente dentro. Intentar refrescarlo daría
// un fallo confuso en cada ejecución.
func TestElMoldeDeOverlayNoEsUnaImagen(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"servicio", "overlay-template"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := m.Images()
	if len(got) != 1 || got[0] != "servicio" {
		t.Errorf("Images() = %v, quería solo [servicio]", got)
	}
}

// imageHasBridgeCached no debe volver a llamar a debugfs por el mismo par
// (base, capa) mientras ninguno de los dos ficheros cambie (M-14): antes,
// comprobar el puente de una imagen con volúmenes o carpetas compartidas
// costaba hasta 4 debugfs en CADA arranque en frío, aunque la imagen no se
// hubiera tocado desde el arranque anterior.
func TestImageHasBridgeCacheada(t *testing.T) {
	m := newTestManager(t)
	base := filepath.Join(t.TempDir(), "base.ext4")
	if err := os.WriteFile(base, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	contador := filepath.Join(dir, "llamadas")
	fakeDebugfs(t, dir, contador)

	// Dos llamadas con el mismo fichero: la segunda tiene que venir de la
	// caché, sin invocar debugfs otra vez.
	for i := 0; i < 2; i++ {
		has, err := m.imageHasBridgeCached(t.Context(), base, "")
		if err != nil {
			t.Fatalf("vuelta %d: %v", i, err)
		}
		if !has {
			t.Fatalf("vuelta %d: quería el puente detectado", i)
		}
	}
	if n := contarLlamadas(t, contador); n != 1 {
		t.Errorf("debugfs se llamó %d veces tras 2 consultas iguales; quería 1", n)
	}

	// Reescribir el fichero (mtime distinto) invalida la caché: la próxima
	// consulta vuelve a mirar el disco.
	time.Sleep(2 * time.Millisecond) // el mtime tiene que avanzar de verdad
	if err := os.WriteFile(base, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.imageHasBridgeCached(t.Context(), base, ""); err != nil {
		t.Fatal(err)
	}
	if n := contarLlamadas(t, contador); n != 2 {
		t.Errorf("debugfs se llamó %d veces tras cambiar el fichero; quería 2 (una por versión)", n)
	}
}

// fakeDebugfs pone en el PATH un debugfs falso que anota cada llamada en
// contador y siempre dice que el fichero preguntado existe.
func fakeDebugfs(t *testing.T, dir, contador string) {
	t.Helper()
	script := "#!/bin/sh\necho x >> " + contador + "\necho 'Inode: 12  Type: regular'\n"
	if err := os.WriteFile(filepath.Join(dir, "debugfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// contarLlamadas cuenta las líneas que fakeDebugfs fue anotando.
func contarLlamadas(t *testing.T, contador string) int {
	t.Helper()
	b, err := os.ReadFile(contador)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// Comparar por CONTENIDO y no por fecha o tamaño: dos puentes distintos pueden
// pesar lo mismo, y una imagen reescrita sin necesidad pierde su dispersión en
// disco.
func TestSeComparaPorContenido(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("mismo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("mismo"), 0o755); err != nil {
		t.Fatal(err)
	}
	da, err := digest.File(a)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := digest.File(b)
	if da != db {
		t.Error("dos ficheros idénticos dieron huellas distintas")
	}
	if err := os.WriteFile(b, []byte("otro!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dc, _ := digest.File(b); dc == da {
		t.Error("dos ficheros distintos del mismo tamaño dieron la misma huella")
	}
}
