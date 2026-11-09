package machine

import (
	"os"
	"path/filepath"
	"testing"
)

// Un fallo al crear la clave no se queda cacheado: antes un sync.Once lo
// guardaba y el daemon no firmaba ni verificaba un dorado más hasta
// reiniciarse, aunque la causa (un disco lleno, un permiso) ya no estuviera.
func TestClaveFirmaNoCacheaElFallo(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root}
	// secrets como fichero: MkdirAll falla.
	if err := os.WriteFile(filepath.Join(root, "secrets"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.claveFirma(); err == nil {
		t.Fatal("claveFirma no vio el fallo")
	}
	if err := os.Remove(filepath.Join(root, "secrets")); err != nil {
		t.Fatal(err)
	}
	k, err := m.claveFirma()
	if err != nil || len(k) != 32 {
		t.Fatalf("con la causa resuelta sigue fallando: %v (%d bytes)", err, len(k))
	}
	k2, err := m.claveFirma()
	if err != nil || string(k2) != string(k) {
		t.Fatal("la clave cambió entre llamadas")
	}
	en, err := os.ReadFile(filepath.Join(root, "secrets", "snapshot.key"))
	if err != nil || string(en) != string(k) {
		t.Fatalf("la clave en disco no es la que se usa: %v", err)
	}
}

// Una clave a medias (un corte con la clave sin llegar al disco deja el
// fichero vacío) nunca firmó nada: se rehace. Antes O_EXCL fallaba sobre ella
// y el daemon ya no firmaba nunca.
func TestClaveFirmaRehaceUnaClaveAMedias(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "snapshot.key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root}
	k, err := m.claveFirma()
	if err != nil || len(k) != 32 {
		t.Fatalf("clave a medias: %v (%d bytes)", err, len(k))
	}
	if ms, _ := filepath.Glob(filepath.Join(root, "secrets", "*.tmp*")); len(ms) != 0 {
		t.Fatalf("quedaron temporales: %v", ms)
	}
}

// Dos Manager sobre la misma raíz (dos daemons a la vez) acaban con la misma
// clave.
func TestClaveFirmaDosALaVezLaMisma(t *testing.T) {
	root := t.TempDir()
	a, b := &Manager{root: root}, &Manager{root: root}
	ka, err := a.claveFirma()
	if err != nil {
		t.Fatal(err)
	}
	kb, err := b.claveFirma()
	if err != nil || string(ka) != string(kb) {
		t.Fatalf("claves distintas: %v", err)
	}
}
