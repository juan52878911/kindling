package durable

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Con un temporal de nombre fijo, dos Escribir a la vez sobre la misma ruta se
// truncaban el temporal el uno al otro: el rename publicaba una mezcla, o
// fallaba porque el otro ya lo habia movido.
func TestEscribirConcurrenteNoMezcla(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "estado.json")
	const n = 16
	contenidos := make(map[string]bool, n)
	var datos [n][]byte
	for i := range datos {
		datos[i] = bytes.Repeat([]byte{byte('a' + i)}, 1024*(i+1))
		contenidos[string(datos[i])] = true
	}
	var wg sync.WaitGroup
	errs := make(chan error, n*20)
	for i := range datos {
		wg.Add(1)
		go func(b []byte) {
			defer wg.Done()
			for range 20 {
				if err := Escribir(ruta, b, 0o640); err != nil {
					errs <- err
				}
			}
		}(datos[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Escribir concurrente: %v", err)
	}
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if !contenidos[string(b)] {
		t.Errorf("quedaron %d bytes que no son ninguna de las escrituras completas", len(b))
	}
	if fi, err := os.Stat(ruta); err == nil && fi.Mode().Perm() != 0o640 {
		t.Errorf("permisos = %v, esperaba 0640", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		var nombres []string
		for _, e := range ents {
			nombres = append(nombres, e.Name())
		}
		t.Errorf("quedaron temporales junto al fichero: %v", nombres)
	}
}

func TestEscribirDejaElContenidoYNingunTemporal(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "estado.json")

	if err := Escribir(ruta, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatalf("Escribir: %v", err)
	}
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatalf("no se escribio: %v", err)
	}
	if string(b) != `{"a":1}` {
		t.Errorf("contenido = %q", b)
	}
	if fi, err := os.Stat(ruta); err == nil && fi.Mode().Perm() != 0o644 {
		t.Errorf("permisos = %v, esperaba 0644", fi.Mode().Perm())
	}
	// Un .tmp olvidado al lado del bueno es basura permanente.
	if _, err := os.Stat(ruta + ".tmp"); !os.IsNotExist(err) {
		t.Error("quedo un .tmp tirado junto al fichero")
	}
}

func TestEscribirReemplazaEnteroSinDejarMezcla(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "estado.json")

	largo := make([]byte, 4096)
	for i := range largo {
		largo[i] = 'A'
	}
	if err := Escribir(ruta, largo, 0o644); err != nil {
		t.Fatal(err)
	}
	// Uno MAS CORTO: si se escribiera encima en vez de renombrar, quedaria la
	// cola del anterior pegada al final y el JSON seria ilegible.
	if err := Escribir(ruta, []byte("BB"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ruta)
	if string(b) != "BB" {
		t.Errorf("quedaron %d bytes (%.10q...); esperaba exactamente \"BB\"", len(b), b)
	}
}

func TestEscribirQueFallaNoDestruyeLoQueHabia(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "sub", "estado.json")
	if err := os.MkdirAll(filepath.Dir(ruta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Escribir(ruta, []byte("bueno"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Directorio sin permiso de escritura: no se puede crear el temporal.
	if err := os.Chmod(filepath.Dir(ruta), 0o500); err != nil {
		t.Skipf("no se pueden quitar permisos aqui: %v", err)
	}
	defer os.Chmod(filepath.Dir(ruta), 0o755)

	if os.Geteuid() == 0 {
		t.Skip("como root los permisos no frenan nada")
	}
	if err := Escribir(ruta, []byte("malo"), 0o644); err == nil {
		t.Error("Escribir no fallo sobre un directorio de solo lectura")
	}
	// Lo que importa: el contenido anterior sigue intacto.
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatalf("se perdio el fichero anterior: %v", err)
	}
	if string(b) != "bueno" {
		t.Errorf("el fichero anterior quedo en %q; una escritura fallida no debe tocarlo", b)
	}
}

func TestRenombrarPublicaElContenidoDelTemporal(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "img.ext4.tmp")
	destino := filepath.Join(dir, "img.ext4")

	if err := os.WriteFile(tmp, []byte("contenido"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Renombrar(tmp, destino); err != nil {
		t.Fatalf("Renombrar: %v", err)
	}
	b, err := os.ReadFile(destino)
	if err != nil {
		t.Fatalf("no llego a destino: %v", err)
	}
	if string(b) != "contenido" {
		t.Errorf("contenido = %q", b)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("el temporal debería haber desaparecido tras el rename")
	}
}

func TestRenombrarSinTemporalFalla(t *testing.T) {
	dir := t.TempDir()
	if err := Renombrar(filepath.Join(dir, "no-existe.tmp"), filepath.Join(dir, "destino")); err == nil {
		t.Error("esperaba un error si el temporal no existe")
	}
}
