package machine

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// volcadoSellado deja en dir un volcado completo y sellado.
func volcadoSellado(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", "", false); err != nil {
		t.Fatal(err)
	}
}

// Con el disco lleno, crear un fichero falla (ENOSPC) pero borrar y renombrar
// no. volcadoEnCurso borraba el sello y después no podía crear la marca: sin
// ninguna de las dos, el volcado viejo pasaba por anterior a los sellos y se
// daba por bueno. Ahora o queda inválido o no se tocó nada y vuelve el error.
func TestVolcadoEnCursoConElDiscoLlenoNoDejaUnVolcadoSinMarcas(t *testing.T) {
	dir := t.TempDir()
	volcadoSellado(t, dir)
	antes, err := os.ReadFile(filepath.Join(dir, marcaOK))
	if err != nil {
		t.Fatal(err)
	}
	viejo := escribirMarca
	escribirMarca = func(string, []byte, os.FileMode) error { return syscall.ENOSPC }
	t.Cleanup(func() { escribirMarca = viejo })

	err = volcadoEnCurso(dir)
	if err == nil {
		if volcadoValido(dir, nil) == nil {
			t.Fatal("volcadoEnCurso dijo que sí y el volcado sigue valiendo")
		}
		return
	}
	// Falló: entonces no puede haber tocado el sello.
	ahora, rerr := os.ReadFile(filepath.Join(dir, marcaOK))
	if rerr != nil || !bytes.Equal(antes, ahora) {
		t.Fatalf("volcadoEnCurso falló (%v) y aun así se llevó el sello (%v): el volcado pasa por anterior a los sellos", err, rerr)
	}
}

// Si el rename tampoco se puede, el error vuelve y el sello sigue.
func TestVolcadoEnCursoSiNoPuedeRenombrarDevuelveElError(t *testing.T) {
	dir := t.TempDir()
	volcadoSellado(t, dir)
	viejoR, viejoE := renombrarMarca, escribirMarca
	renombrarMarca = func(string, string) error { return syscall.EIO }
	escribirMarca = func(string, []byte, os.FileMode) error { return syscall.ENOSPC }
	t.Cleanup(func() { renombrarMarca, escribirMarca = viejoR, viejoE })
	if err := volcadoEnCurso(dir); err == nil {
		t.Fatal("volcadoEnCurso no devolvió el fallo")
	}
	if err := volcadoValido(dir, nil); err != nil {
		t.Fatalf("un fallo sin tocar nada no debería invalidar el volcado: %v", err)
	}
}

// Sin sello (ya en curso, o un volcado de antes de los sellos), la marca se
// escribe como siempre, y si ya estaba no hace falta nada.
func TestVolcadoEnCursoSinSello(t *testing.T) {
	dir := t.TempDir()
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, marcaEnCurso)); err != nil {
		t.Fatalf("sin sello no dejó la marca: %v", err)
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatalf("con la marca ya puesta: %v", err)
	}
}

// restaurarSello devuelve el sello que se convirtió en marca, y con un volcado
// de antes de los sellos lo deja como estaba (sin marcas).
func TestRestaurarSello(t *testing.T) {
	dir := t.TempDir()
	volcadoSellado(t, dir)
	if err := retirarSello(dir); err != nil {
		t.Fatal(err)
	}
	if volcadoValido(dir, nil) == nil {
		t.Fatal("retirado, el volcado sigue valiendo")
	}
	if err := restaurarSello(dir); err != nil {
		t.Fatal(err)
	}
	if err := volcadoValido(dir, nil); err != nil {
		t.Fatalf("restaurado, el volcado no vale: %v", err)
	}

	pre := t.TempDir()
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(pre, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := retirarSello(pre); err != nil {
		t.Fatal(err)
	}
	if err := restaurarSello(pre); err != nil {
		t.Fatal(err)
	}
	if err := volcadoValido(pre, nil); err != nil {
		t.Fatalf("un volcado de antes de los sellos debería volver a valer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pre, marcaOK)); err == nil {
		t.Fatal("restaurarSello inventó un sello que no había")
	}
}
