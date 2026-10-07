package ext4

import (
	"archive/tar"
	"bytes"
	"testing"
	"time"
)

// entrada es una entrada de un tar sintético: tipo, nombre y, según el
// tipo, contenido o destino.
type entrada struct {
	tipo   byte
	nombre string
	dato   string // contenido de un fichero, destino de un enlace
}

func capa(t *testing.T, es ...entrada) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Typeflag: e.tipo, Name: e.nombre, Mode: 0o644}
		switch e.tipo {
		case tar.TypeDir:
			h.Mode = 0o755
		case tar.TypeReg:
			h.Size = int64(len(e.dato))
		case tar.TypeSymlink, tar.TypeLink:
			h.Linkname = e.dato
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.tipo == tar.TypeReg {
			if _, err := tw.Write([]byte(e.dato)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

// aplica mete las capas en orden, como las de una imagen OCI.
func aplica(t *testing.T, capas ...*bytes.Buffer) *Node {
	t.Helper()
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	for i, c := range capas {
		if err := root.AddTar(c, TarOptions{Stream: i, Whiteouts: true, Time: time.Unix(0, 0)}); err != nil {
			t.Fatalf("capa %d: %v", i, err)
		}
	}
	return root
}

func dir(n string) entrada          { return entrada{tar.TypeDir, n, ""} }
func fich(n, d string) entrada      { return entrada{tar.TypeReg, n, d} }
func simb(n, dest string) entrada   { return entrada{tar.TypeSymlink, n, dest} }
func duro(n, dest string) entrada   { return entrada{tar.TypeLink, n, dest} }
func existe(r *Node, p string) bool { return r.Lookup(p) != nil }

// Un ".wh.x" borra x (y lo de debajo) de las capas anteriores.
func TestAddTarWhiteout(t *testing.T) {
	r := aplica(t,
		capa(t, dir("etc"), fich("etc/a", "a"), fich("etc/b", "b"), dir("etc/d"), fich("etc/d/x", "x")),
		capa(t, fich("etc/.wh.a", ""), fich("etc/.wh.d", ""), fich("etc/.wh.no-existe", "")),
	)
	for _, p := range []string{"/etc/a", "/etc/d", "/etc/d/x", "/etc/.wh.a"} {
		if existe(r, p) {
			t.Errorf("%s sigue ahí", p)
		}
	}
	if !existe(r, "/etc/b") {
		t.Error("/etc/b ha desaparecido")
	}
}

// Un ".wh..wh..opq" vacía lo que el directorio traía de capas anteriores,
// pero no lo que mete la propia capa, venga antes o después del opaco.
func TestAddTarOpaqueWhiteout(t *testing.T) {
	r := aplica(t,
		capa(t, dir("opt"), fich("opt/viejo", "v"), dir("opt/sub"), fich("opt/sub/y", "y"), fich("fuera", "f")),
		capa(t,
			dir("opt"), fich("opt/antes", "1"),
			fich("opt/.wh..wh..opq", ""),
			fich("opt/despues", "2"),
		),
	)
	for _, p := range []string{"/opt/viejo", "/opt/sub", "/opt/.wh..wh..opq"} {
		if existe(r, p) {
			t.Errorf("%s sigue ahí", p)
		}
	}
	for _, p := range []string{"/opt", "/opt/antes", "/opt/despues", "/fuera"} {
		if !existe(r, p) {
			t.Errorf("%s ha desaparecido", p)
		}
	}
}

// Con /usr unificado (bin -> usr/bin), un enlace duro a bin/busybox
// encuentra usr/bin/busybox, y uno cuyo nombre pasa por el enlace también.
func TestAddTarHardLinkThroughDirSymlink(t *testing.T) {
	r := aplica(t,
		capa(t, dir("usr"), dir("usr/bin"), simb("bin", "usr/bin"), fich("usr/bin/busybox", "bb")),
		capa(t, duro("bin/sh", "bin/busybox"), duro("usr/bin/ls", "/bin/busybox")),
	)
	bb := r.Lookup("/usr/bin/busybox")
	for _, p := range []string{"/usr/bin/sh", "/usr/bin/ls"} {
		if r.Lookup(p) != bb {
			t.Errorf("%s no es un enlace duro a /usr/bin/busybox", p)
		}
	}
	if !r.Lookup("/bin").IsLink() {
		t.Error("/bin ya no es un enlace simbólico")
	}
}

// Un enlace duro a un enlace simbólico enlaza al simbólico, no a su destino.
func TestAddTarHardLinkToSymlink(t *testing.T) {
	r := aplica(t, capa(t, fich("f", "x"), simb("s", "f"), duro("h", "s")))
	if h := r.Lookup("/h"); h == nil || h != r.Lookup("/s") || !h.IsLink() {
		t.Fatalf("/h = %+v, quiero el mismo nodo que el enlace /s", h)
	}
}

// Un enlace duro puede apuntar a un fichero que trajo una capa anterior.
func TestAddTarHardLinkToEarlierLayer(t *testing.T) {
	r := aplica(t,
		capa(t, dir("lib"), fich("lib/libc.so", "elf")),
		capa(t, dir("lib64"), duro("lib64/libc.so", "lib/libc.so")),
	)
	if a, b := r.Lookup("/lib/libc.so"), r.Lookup("/lib64/libc.so"); a == nil || a != b {
		t.Fatalf("/lib64/libc.so no es un enlace duro a /lib/libc.so")
	}
}

// Un enlace duro a lo que no existe, a un directorio o fuera de la raíz
// (un enlace que sale con "..") falla en vez de colgar algo del host.
func TestAddTarHardLinkErrors(t *testing.T) {
	for _, c := range []struct {
		nombre string
		capa   []entrada
	}{
		{"falta", []entrada{duro("h", "no-existe")}},
		{"directorio", []entrada{dir("d"), duro("h", "d")}},
		{"por un enlace que sale", []entrada{simb("x", "../../.."), duro("h", "x/etc/passwd")}},
	} {
		root := NewDir(0o755, 0, 0, time.Unix(0, 0))
		if err := root.AddTar(capa(t, c.capa...), TarOptions{Whiteouts: true}); err == nil {
			t.Errorf("%s: sin error", c.nombre)
		}
	}
}
