package ext4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Un nombre de más de 255 bytes no cabe en name_len (un byte): AddTar tiene
// que rechazarlo en vez de dejar un dirent corrupto.
func TestAddTarNombreLargo(t *testing.T) {
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	err := root.AddTar(capa(t, fich("d/"+strings.Repeat("a", 300), "x")), TarOptions{Whiteouts: true})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("nombre de 300 bytes: %v", err)
	}
	// 255 bytes sí vale, y su whiteout (".wh." + 255 = 259 bytes) también.
	ok := strings.Repeat("b", 255)
	root = aplica(t, capa(t, fich(ok, "x")), capa(t, fich(".wh."+ok, "")))
	if existe(root, "/"+ok) {
		t.Fatal("el whiteout de un nombre de 255 bytes no lo borró")
	}
}

func TestCheckName(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", "a\x00b", strings.Repeat("a", 256)} {
		if CheckName(n) == nil {
			t.Errorf("CheckName(%q) aceptó un nombre inválido", n)
		}
	}
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	if err := root.Put("x\x00y", &Node{Mode: ModeReg | 0o644}, time.Unix(0, 0)); err == nil {
		t.Fatal("Put aceptó un NUL")
	}
	// SetChild no valida: Write tiene que pararlo.
	root.SetChild(strings.Repeat("c", 256), &Node{Mode: ModeReg | 0o644})
	f, err := os.Create(filepath.Join(t.TempDir(), "t.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Write(f, root, nil, Options{}); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("Write con un nombre de 256 bytes: %v", err)
	}
}

// Un fichero de 16 MiB que alterna bloques de datos y de ceros daría 2048
// extents con un hueco por bloque a cero; el árbol de profundidad 1 admite
// 1360. Con ZeroHoles tiene que escribirse igual (los ceros que no caben
// como huecos van como datos) y leerse idéntico.
func TestZeroHolesFragmentado(t *testing.T) {
	data := make([]byte, 16<<20)
	for b := 0; b < len(data)/blockSize; b += 2 {
		for i := 0; i < blockSize; i++ {
			data[b*blockSize+i] = byte(b>>1) | 1
		}
	}
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	root.SetChild("frag", &Node{Mode: ModeReg | 0o644, Size: int64(len(data)), Data: Bytes(data)})
	img := filepath.Join(t.TempDir(), "t.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Write(f, root, nil, Options{ZeroHoles: true})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	fsck(t, img)
	checkBack(t, img, map[string][]byte{"/frag": data})
}

// Un xattr de un namespace que ext4 no tiene, o de user.* demasiado grande,
// se descarta con aviso; security.capability sigue entrando; uno esencial
// que no cabe sigue siendo error.
func TestXattrsDescartados(t *testing.T) {
	capab := bytes.Repeat([]byte{1}, 20)
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	root.SetChild("f", &Node{Mode: ModeReg | 0o755, Xattrs: map[string][]byte{
		"com.apple.quarantine": []byte("x"),
		"user.big":             bytes.Repeat([]byte{2}, 5000),
		"security.capability":  capab,
		"user.small":           []byte("ok"),
	}})
	img := filepath.Join(t.TempDir(), "t.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	var avisos []string
	_, err = Write(f, root, nil, Options{Logf: func(format string, a ...any) {
		avisos = append(avisos, fmt.Sprintf(format, a...))
	}})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(avisos) != 2 || !strings.Contains(strings.Join(avisos, "\n"), "com.apple.quarantine") ||
		!strings.Contains(strings.Join(avisos, "\n"), "user.big") {
		t.Fatalf("avisos: %q", avisos)
	}
	fsck(t, img)
	rf, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	back, err := Read(rf)
	if err != nil {
		t.Fatal(err)
	}
	x := back.Lookup("/f").Xattrs
	if !bytes.Equal(x["security.capability"], capab) || string(x["user.small"]) != "ok" || len(x) != 2 {
		t.Fatalf("xattrs leídos: %v", x)
	}

	root = NewDir(0o755, 0, 0, time.Unix(0, 0))
	root.SetChild("f", &Node{Mode: ModeReg | 0o644, Xattrs: map[string][]byte{
		"security.selinux": bytes.Repeat([]byte{3}, 5000),
	}})
	f, err = os.Create(filepath.Join(t.TempDir(), "u.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Write(f, root, nil, Options{Logf: func(string, ...any) {}}); err == nil {
		t.Fatal("un security.* que no cabe tiene que ser error")
	}
}

// Un superbloque con 0 inodos o 0 bloques por grupo daba división por cero.
func TestReadSuperbloqueCorrupto(t *testing.T) {
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	img := filepath.Join(t.TempDir(), "t.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Write(f, root, nil, Options{})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	orig, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		nombre string
		off    int
	}{{"inodes per group", 40}, {"blocks per group", 32}} {
		b := bytes.Clone(orig)
		binary.LittleEndian.PutUint32(b[1024+c.off:], 0)
		_, err := Read(bytes.NewReader(b))
		if err == nil || !strings.Contains(err.Error(), "corrupt superblock") {
			t.Errorf("%s = 0: %v", c.nombre, err)
		}
	}
}
