package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRutaCanonica(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(d, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if got := rutaCanonica(link); got != real {
		t.Errorf("symlink: %q, quería %q", got, real)
	}
	if got, want := rutaCanonica(filepath.Join(link, "nuevo", "x")), filepath.Join(real, "nuevo", "x"); got != want {
		t.Errorf("inexistente: %q, quería %q", got, want)
	}
	t.Chdir(d)
	if got := rutaCanonica("real"); got != real {
		t.Errorf("relativa: %q, quería %q", got, real)
	}
}

// El overlay que lee el daemon no puede ser un enlace (lo escribe el VMM) ni
// cambiar mientras se copia.
func TestFijarOverlayParaLeer(t *testing.T) {
	d := t.TempDir()
	ok := filepath.Join(d, "overlay.ext4")
	if err := os.WriteFile(ok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, tras, err := fijarOverlayParaLeer(ok)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tras(); err != nil {
		t.Errorf("sin cambios: %v", err)
	}
	// Cambiado por otro fichero durante la copia.
	if err := os.Remove(ok); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ok, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tras(); err == nil {
		t.Error("no detectó que el fichero se reemplazó")
	}
	// Enlace simbólico a un fichero de fuera.
	secreto := filepath.Join(d, "secreto")
	if err := os.WriteFile(secreto, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	enlace := filepath.Join(d, "enlace.ext4")
	if err := os.Symlink(secreto, enlace); err != nil {
		t.Skip(err)
	}
	if _, _, err := fijarOverlayParaLeer(enlace); err == nil {
		t.Error("siguió un enlace simbólico")
	}
	// Un directorio no es un overlay.
	if _, _, err := fijarOverlayParaLeer(d); err == nil {
		t.Error("aceptó un directorio")
	}
}

// El ataque: el VMM cambia el overlay por un enlace a un fichero de root
// DESPUÉS de la comprobación y lo devuelve a su sitio antes de la
// comprobación final. La copia sale del descriptor comprobado, así que el
// dorado tiene el overlay y no el otro fichero, aunque tras() pase.
func TestCopiarOverlayDesdeDescriptorNoSigueLaRuta(t *testing.T) {
	d := t.TempDir()
	ruta := filepath.Join(d, "overlay.ext4")
	if err := os.WriteFile(ruta, []byte("overlay de verdad"), 0o600); err != nil {
		t.Fatal(err)
	}
	secreto := filepath.Join(d, "shadow")
	if err := os.WriteFile(secreto, []byte("SECRETO DE ROOT"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, tras, err := fijarOverlayParaLeer(ruta)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Entre la comprobación y la copia: la ruta pasa a ser un enlace.
	aparte := filepath.Join(d, "aparte")
	if err := os.Rename(ruta, aparte); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secreto, ruta); err != nil {
		t.Skip(err)
	}
	m := &Manager{}
	dst := filepath.Join(d, "dorado.ext4")
	cedido := false
	own := func(*os.File) error { cedido = true; return nil }
	if _, err := m.copiarOverlayDesde(context.Background(), f, dst, own); err != nil {
		t.Fatal(err)
	}
	// Y se deja todo como estaba antes de la comprobación final.
	if err := os.Remove(ruta); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aparte, ruta); err != nil {
		t.Fatal(err)
	}
	if err := tras(); err != nil {
		t.Fatalf("la ruta vuelve a ser el mismo fichero: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "overlay de verdad" {
		t.Errorf("el dorado tiene %q: se copió por ruta", b)
	}
	if !cedido {
		t.Error("no cedió el fichero creado")
	}
}

// El destino puede estar en un directorio del VMM (el jail): un enlace
// plantado en su lugar no se sigue, ni para escribir ni para ceder.
func TestCopiarOverlayDesdeNoEscribeEnUnEnlace(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "overlay.ext4")
	if err := os.WriteFile(src, []byte("overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	victima := filepath.Join(d, "victima")
	if err := os.WriteFile(victima, []byte("intacto"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(d, "dorado.ext4")
	if err := os.Symlink(victima, dst); err != nil {
		t.Skip(err)
	}
	f, _, err := fijarOverlayParaLeer(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := &Manager{}
	if _, err := m.copiarOverlayDesde(context.Background(), f, dst, nil); err == nil {
		t.Error("escribió a través de un enlace")
	}
	if b, _ := os.ReadFile(victima); string(b) != "intacto" {
		t.Errorf("la víctima tiene %q", b)
	}
}

// Un cow/m/<id> residual de una máquina que no está viva se reemplaza; el de
// una viva no se toca.
func TestAlmacenReemplazaInstanciaResidual(t *testing.T) {
	root := t.TempDir()
	a := nuevoAlmacenFalso(t, root, &almacenFalso{})
	src := escribirDorado(t, root, "d", "disco")
	vivas := map[string]bool{}
	a.viva = func(id string) bool { return vivas[id] }
	resto := filepath.Join(a.dirInstancia("id1"), "basura")
	if err := os.MkdirAll(a.dirInstancia("id1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resto, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ruta, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0)
	if err != nil {
		t.Fatalf("residual: %v", err)
	}
	if _, err := os.Stat(resto); err == nil {
		t.Error("el residuo sigue ahí")
	}
	if b, _ := os.ReadFile(ruta); string(b) != "disco" {
		t.Errorf("contenido %q", b)
	}
	vivas["id1"] = true
	if _, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0); err == nil {
		t.Error("pisó el directorio de una máquina viva")
	}
	if _, err := os.Stat(ruta); err != nil {
		t.Errorf("borró el overlay de una máquina viva: %v", err)
	}
}
