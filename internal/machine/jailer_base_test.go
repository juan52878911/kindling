package machine

import (
	"os"
	"path/filepath"
	"testing"
)

// jailer canonicaliza --chroot-base-dir antes de crear nada: en una raíz
// nueva, el segundo de dos arranques a la vez fallaba con "Failed to
// canonicalize path .../jails" (lab, CT 105) porque nadie lo había creado
// aún. spawnJailed lo crea antes de lanzar jailer, 0700, aunque el
// lanzamiento falle después.
func TestSpawnJailedCreaLaBaseDelJailAntes(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = "kling-no-hay-firecracker"
	id := "1a1a000000000001"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.jailBase()); err == nil {
		t.Fatal("la base del jail ya existía: la prueba no prueba nada")
	}
	if _, _, _, err := m.spawnJailed(id, nil, nil); err == nil {
		t.Fatal("spawnJailed sin firecracker no falló")
	}
	fi, err := os.Stat(m.jailBase())
	if err != nil || !fi.IsDir() {
		t.Fatalf("la base del jail no se creó: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("base del jail %v, quería 0700", fi.Mode().Perm())
	}
	if filepath.Base(m.jailBase()) != "jails" {
		t.Fatalf("jailBase = %s", m.jailBase())
	}
}
