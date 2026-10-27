package imagen

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/pkg/api"
)

// escribirImagen deja una imagen oci importada en images: su .ext4 y su receta.
func escribirImagen(t *testing.T, images, name string, ids []string, init string) {
	t.Helper()
	built, _ := json.Marshal(BuiltOCI{Arch: "amd64", DiffIDs: ids, Init: init, Agent: "ag"})
	rec, _ := json.Marshal(api.ImageRecipe{Name: name, Builder: "oci", Built: built})
	if err := os.WriteFile(filepath.Join(images, name+".recipe.json"), rec, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(images, name+".ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBestBasePrefijo(t *testing.T) {
	images := t.TempDir()
	escribirImagen(t, images, "corta", []string{"a", "b"}, "sh")
	escribirImagen(t, images, "larga", []string{"a", "b", "c"}, "sh")
	escribirImagen(t, images, "otra", []string{"a", "x", "c"}, "sh")
	bases := IndexBases(images, "amd64")
	if len(bases) != 3 {
		t.Fatalf("bases = %d, quiero 3", len(bases))
	}
	// El prefijo más largo gana.
	if b := BestBase(bases, []string{"a", "b", "c", "d"}, "ag"); b == nil || b.Name != "larga" {
		t.Fatalf("base = %+v, quiero larga", b)
	}
	// Prefijo distinto: ninguna.
	if b := BestBase(bases, []string{"z", "b", "c", "d"}, "ag"); b != nil {
		t.Fatalf("base = %+v, quiero ninguna", b)
	}
	// La misma imagen entera no es una capa nueva: no se reutiliza.
	if b := BestBase(bases, []string{"a", "b", "c"}, "ag"); b == nil || b.Name != "corta" {
		t.Fatalf("misma cadena: base = %+v, quiero corta (la larga no deja nada)", b)
	}
	// Otro agente: la base lleva otro kling-guest.
	if b := BestBase(bases, []string{"a", "b", "c", "d"}, "otro"); b != nil {
		t.Fatalf("otro agente: base = %+v", b)
	}
	// Otra arquitectura.
	if len(IndexBases(images, "arm64")) != 0 {
		t.Fatal("otra arquitectura indexada")
	}
}

func TestIndexBasesBaseBorrada(t *testing.T) {
	images := t.TempDir()
	escribirImagen(t, images, "pg", []string{"a", "b"}, "sh")
	if err := os.Remove(filepath.Join(images, "pg.ext4")); err != nil {
		t.Fatal(err)
	}
	if b := BestBase(IndexBases(images, "amd64"), []string{"a", "b", "c"}, "ag"); b != nil {
		t.Fatalf("base borrada reutilizada: %+v", b)
	}
}

func TestIndexBasesNoCuentaCapas(t *testing.T) {
	images := t.TempDir()
	escribirImagen(t, images, "hija", []string{"a", "b", "c"}, "sh")
	if err := os.WriteFile(filepath.Join(images, "hija.layer.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(IndexBases(images, "amd64")) != 0 {
		t.Fatal("una imagen por capas no es base")
	}
}

func fichero(t time.Time, k int) *ext4.Node {
	return &ext4.Node{Mode: ext4.ModeReg | 0o644, Mtime: t, Size: 1, Data: ext4.StreamKey{Stream: k, Key: 1}}
}

func TestUpperSoloLaDiferencia(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	root := ext4.NewDir(0o755, 0, 0, now)
	usr, _ := root.MkdirAll("/usr/lib", 0o755, 0, 0, now)
	usr.SetChild("a", fichero(now, 0))
	usr.SetChild("viejo", fichero(now, 0))
	root.Put("/etc/conf", fichero(now, 0), now)
	foto := Fotografiar(root)

	// La capa nueva: añade, pisa, borra.
	usr.SetChild("nuevo", fichero(now, 1))
	usr.SetChild("a", fichero(now, 1))
	usr.RemoveChild("viejo")
	up, err := foto.Upper(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := up.Lookup("/usr/lib/nuevo"); n == nil {
		t.Fatal("falta lo nuevo")
	}
	if n := up.Lookup("/usr/lib/a"); n == nil {
		t.Fatal("falta lo pisado")
	}
	if w := up.Lookup("/usr/lib/viejo"); w == nil || w.Mode&ext4.ModeType != ext4.ModeChar || w.Major != 0 || w.Minor != 0 {
		t.Fatalf("falta la marca de borrado: %+v", w)
	}
	if up.Lookup("/etc") != nil {
		t.Fatal("/etc no cambió y está en la capa")
	}
}

func TestUpperPrefijoCompartido(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	root := ext4.NewDir(0o755, 0, 0, now)
	root.SetChild("f", fichero(now, 0))
	foto := Fotografiar(root)
	// Un enlace duro nuevo a un fichero de la base.
	root.SetChild("g", root.Child("f"))
	if _, err := foto.Upper(root, 1); !errors.Is(err, ErrPrefijoCompartido) {
		t.Fatalf("err = %v", err)
	}
}
