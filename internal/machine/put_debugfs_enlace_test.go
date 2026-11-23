package machine

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// El destino de un enlace de la imagen es entrada de la imagen, no del
// llamador: sus componentes acaban en órdenes de debugfs (mkdir, cd, write)
// igual que la ruta pedida, y han de pasar la misma validación. Aquí el
// enlace es lento (más de 60 bytes, va en un bloque de datos) y su destino
// lleva comillas y un salto de línea, que debugfs no sabe escapar.
func TestPutDebugfsRechazaDestinoDeEnlaceNoValido(t *testing.T) {
	marca := "ZQZQ"
	destino := "/nope/" + strings.Repeat("a", 60) + marca + "/b"
	m, im := ext4Prueba(t, 16, "symlink /l "+destino)

	// debugfs no crea un enlace con esos bytes: se ponen a mano en el
	// bloque de datos del enlace, con la misma longitud.
	raw, err := os.ReadFile(im.file)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte(marca)); n != 1 {
		t.Skipf("el destino del enlace aparece %d veces en la imagen; esperaba un bloque de datos", n)
	}
	raw = bytes.Replace(raw, []byte(marca), []byte("q\"\nq"), 1)
	if err := os.WriteFile(im.file, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	src, d := fichero(t, "x")
	_, err = m.intentarPutDebugfs(ctx, im.file, "/l/f", src, d, 0o644, true)
	if err == nil {
		t.Fatal("put siguió un enlace cuyo destino debugfs no puede llevar entre comillas")
	}
	// Ha de rechazarse al validar, antes de construir órdenes: que debugfs
	// falle después no basta, porque las órdenes ya le llegaron.
	if !strings.Contains(err.Error(), "debugfs cannot take") {
		t.Fatalf("el destino del enlace llegó a debugfs sin validar: %v", err)
	}
	if e, err := im.stat(ctx, "/nope"); err != nil || e.existe {
		t.Fatalf("el put rechazado dejó cambios en la imagen (/nope: %+v, %v)", e, err)
	}
}
