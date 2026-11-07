package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Como root, el directorio de trabajo (descarga, copias, cerrojo) tiene que
// ser de root: uno del usuario (el ~ que sudo conserva en macOS) le deja
// cambiar los binarios entre la verificación y la instalación, o las copias
// que un -rollback instala.
func TestBloquearComoRootRechazaDirAjeno(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("como root el directorio de prueba ya es de root")
	}
	viejo := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = viejo }()
	dir := filepath.Join(t.TempDir(), "upgrade")
	soltar, err := bloquear(dir)
	if err == nil {
		soltar()
		t.Fatal("como root, un directorio del usuario aceptado")
	}
	if !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("error inesperado: %v", err)
	}
}

func TestDirSoloDe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "upgrade")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dirSoloDe(dir, os.Getuid()); err != nil {
		t.Fatalf("directorio propio: %v", err)
	}
	// Escribible por otros: rechazado aunque sea nuestro.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := dirSoloDe(dir, os.Getuid()); err == nil {
		t.Fatal("directorio escribible por otros aceptado")
	}
	// Un padre escribible por otros sin bit pegajoso: rechazado (podrían
	// renombrar el directorio y poner otro).
	padre := filepath.Join(t.TempDir(), "padre")
	hijo := filepath.Join(padre, "upgrade")
	if err := os.MkdirAll(hijo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(padre, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := dirSoloDe(hijo, os.Getuid()); err == nil {
		t.Fatal("padre escribible por otros aceptado")
	}
	// Con bit pegajoso (como /tmp): vale.
	if err := os.Chmod(padre, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := dirSoloDe(hijo, os.Getuid()); err != nil {
		t.Fatalf("padre pegajoso: %v", err)
	}
}
