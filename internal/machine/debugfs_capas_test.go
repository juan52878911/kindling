package machine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// crearExt4 formatea un ext4 en ruta y lo llena con órdenes de debugfs.
func crearExt4(t *testing.T, ruta string, ordenes ...string) imagenDebugfs {
	t.Helper()
	bin := debugfsBin()
	if bin == "" || buscarE2fs("mkfs.ext4") == "" {
		t.Skip("sin e2fsprogs (debugfs, mkfs.ext4)")
	}
	if err := os.MkdirAll(filepath.Dir(ruta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruta, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(ruta, 32<<20); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if out, err := e2fsCmd(ctx, "mkfs.ext4", "-q", "-F", ruta).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	im := imagenDebugfs{bin: bin, file: ruta}
	if len(ordenes) > 0 {
		if err := im.escribir(ctx, filepath.Dir(ruta), ordenes); err != nil {
			t.Fatal(err)
		}
	}
	return im
}

// imagenPorCapas deja en m una base "min" y una capa "svc" encima con un
// whiteout, un directorio opaco y un fichero propio.
func imagenPorCapas(t *testing.T, m *Manager) (base, capa imagenDebugfs) {
	t.Helper()
	src := func(contenido string) string {
		p, _ := fichero(t, contenido)
		return comillas(p)
	}
	base = crearExt4(t, m.imagePath(defaultBaseImage),
		"mkdir etc", "mkdir opaco",
		"write "+src("de la base")+" etc/borrado",
		"write "+src("visible")+" etc/visible",
		"write "+src("con espacio")+` "etc/a b"`,
		"write "+src("oculto")+" opaco/x",
	)
	capa = crearExt4(t, m.layerPath("svc"),
		"mkdir upper", "mkdir upper/etc", "mkdir upper/opaco",
		"ea_set upper/opaco trusted.overlay.opaque y",
		"cd upper/etc",
		"mknod borrado c 0 0",
		"write "+src("de la capa")+" nuevo",
	)
	return base, capa
}

// Leer de una imagen por capas como la ve el invitado: lo borrado en la capa
// (whiteout) o tapado por un directorio opaco no está, aunque la base lo
// tenga. Las rutas con espacios se leen enteras, y lo leído tiene tope.
func TestReadImageFileRespetaWhiteoutsComillasYTope(t *testing.T) {
	m := newTestManager(t)
	imagenPorCapas(t, m)
	ctx := context.Background()

	for _, p := range []string{"/etc/borrado", "/opaco/x"} {
		if b, err := m.ReadImageFile(ctx, "svc", p, 1<<20); !errors.Is(err, ErrNotInImage) {
			t.Errorf("%s está borrado en la capa y se leyó: %q, %v", p, b, err)
		}
	}
	casos := map[string]string{"/etc/visible": "visible", "/etc/nuevo": "de la capa", "/etc/a b": "con espacio"}
	for p, quiero := range casos {
		b, err := m.ReadImageFile(ctx, "svc", p, 1<<20)
		if err != nil || string(b) != quiero {
			t.Errorf("%s = %q, %v; quería %q", p, b, err, quiero)
		}
	}
	if b, err := m.ReadImageFile(ctx, "svc", "/etc/visible", 3); err == nil {
		t.Errorf("pasó el tope de 3 bytes: %q", b)
	}
	if st, err := m.StatImageFile(ctx, "svc", "/etc/borrado"); err != nil || st.Exists {
		t.Errorf("stat de un fichero borrado en la capa: %+v, %v", st, err)
	}
}

// Poner un fichero donde la capa tiene un whiteout: con create=false no está
// (se borró), con create=true el nuevo sustituye al whiteout.
func TestPutDebugfsSustituyeUnWhiteout(t *testing.T) {
	m := newTestManager(t)
	_, capa := imagenPorCapas(t, m)
	ctx := context.Background()
	src, quiero := fichero(t, "resucitado")
	dentro := layerGuestPath("etc/borrado")

	if _, err := m.intentarPutDebugfs(ctx, capa.file, dentro, src, quiero, 0o644, false); !errors.Is(err, errNoBridge) {
		t.Fatalf("reemplazar sobre un whiteout (create=false): %v, quería errNoBridge", err)
	}
	cambio, err := m.intentarPutDebugfs(ctx, capa.file, dentro, src, quiero, 0o644, true)
	if err != nil || !cambio {
		t.Fatalf("crear sobre un whiteout: %v, %v", cambio, err)
	}
	e, err := capa.stat(ctx, dentro)
	if err != nil || e.tipo != "regular" || e.blanqueo {
		t.Fatalf("tras el put: %+v, %v", e, err)
	}
	if b, err := m.ReadImageFile(ctx, "svc", "/etc/borrado", 1<<20); err != nil || string(b) != "resucitado" {
		t.Errorf("leído tras el put: %q, %v", b, err)
	}
}

// salidaAcotada corta sin cargar lo que sobra.
func TestSalidaAcotadaCorta(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("sin yes")
	}
	_, err := salidaAcotada(exec.Command("yes"), 1<<16)
	if !errors.Is(err, errSalidaGrande) {
		t.Fatalf("una salida sin fin: %v", err)
	}
	b, err := salidaAcotada(exec.Command("echo", "hola"), 1<<16)
	if err != nil || strings.TrimSpace(string(b)) != "hola" {
		t.Fatalf("salida corta: %q, %v", b, err)
	}
}
