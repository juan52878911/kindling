package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// R4-7: el dueño del fondo era sha256(listen + endpoint), así que cambiar
// -listen dejaba las máquinas del fondo anterior sin dueño que las barriera.
// Ahora es un id que se escribe una vez en el directorio de estado (0600) y se
// lee en cada arranque: no depende de -listen.
func TestPoolOwnerGuardado(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "estado")
	a, err := PoolOwnerGuardado(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a) {
		t.Fatalf("id %q: quiero 16 hex", a)
	}
	b, err := PoolOwnerGuardado(dir)
	if err != nil || b != a {
		t.Fatalf("segundo arranque: %q %v, quiero %q", b, err, a)
	}
	st, err := os.Stat(filepath.Join(dir, poolOwnerFile))
	if err != nil {
		t.Fatal(err)
	}
	if p := st.Mode().Perm(); p != 0o600 {
		t.Fatalf("permisos %o, quiero 600", p)
	}
	otro, err := PoolOwnerGuardado(t.TempDir())
	if err != nil || otro == a {
		t.Fatalf("otro directorio, otro id: %q %v", otro, err)
	}

	// Un fichero roto no se reescribe en silencio: cambiar el dueño dejaría
	// huérfanas las máquinas del fondo.
	roto := t.TempDir()
	if err := os.WriteFile(filepath.Join(roto, poolOwnerFile), []byte("no vale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PoolOwnerGuardado(roto); err == nil {
		t.Fatal("un id inválido en disco debe ser un error")
	}

	for _, v := range []string{"mi-gateway", "a.b_c-1", "0123456789abcdef"} {
		if err := ValidarPoolOwner(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"", "con espacio", "a/b", string(make([]byte, 64))} {
		if err := ValidarPoolOwner(v); err == nil {
			t.Errorf("%q debería ser inválido", v)
		}
	}
}
