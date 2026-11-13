package machine

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/pkg/api"
)

// Nunca se toca la imagen base de una microVM viva.
//
// Es la comprobación que evita el desastre: modificar en escritura un ext4 que
// otro sistema tiene montado lo corrompe, aunque él lo tenga en solo lectura. Y
// aquí "vivo" incluye a las warm: al descongelarse vuelven a leer de la imagen,
// que además está mapeada en su snapshot de memoria.
func TestNoSeTocaLaImagenDeUnaMaquinaViva(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ocupada", "libre"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	puente := filepath.Join(t.TempDir(), "kling-bridge")
	if err := os.WriteFile(puente, []byte("binario"), 0o755); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateWarm, Image: "ocupada"}
	m.mu.Unlock()

	res, err := m.PutImageFile(t.Context(), "ocupada", "/usr/local/bin/kling-bridge", puente, 0o755, false)
	if err == nil || !res.Busy {
		t.Fatalf("iba a tocar la imagen de una máquina viva: res=%+v err=%v", res, err)
	}
	if res.Updated {
		t.Error("dice que la actualizó pese a negarse")
	}
	// Y tiene que decir QUIÉN la retiene, o el usuario no sabe qué parar.
	if !strings.Contains(err.Error(), "svc-a") {
		t.Errorf("no dice quién la usa: %v", err)
	}

	// La libre sí se intenta (fallará al montar, que aquí no hay loop, pero no
	// se niega por estar en uso).
	res, _ = m.PutImageFile(t.Context(), "libre", "/usr/local/bin/kling-bridge", puente, 0o755, false)
	if res.Busy {
		t.Error("se negó con una imagen que no usa nadie")
	}
}

// Sin fichero de origen se dice cuál falta en vez de fallar seco.
func TestPutImageFileSinOrigen(t *testing.T) {
	m := newTestManager(t)
	_, err := m.PutImageFile(t.Context(), "x", "/etc/x", "/no/existe", 0o644, false)
	if err == nil || !strings.Contains(err.Error(), "/no/existe") {
		t.Fatalf("el error no dice qué fichero falta: %v", err)
	}
}

// overlay-template no es una imagen de rootfs: es el molde vacío del disco de
// escritura de cada máquina y no lleva puente dentro. Intentar refrescarlo daría
// un fallo confuso en cada ejecución.
func TestElMoldeDeOverlayNoEsUnaImagen(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"servicio", "overlay-template"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := m.Images()
	if len(got) != 1 || got[0] != "servicio" {
		t.Errorf("Images() = %v, quería solo [servicio]", got)
	}
}

// imageHasBridgeCached no debe volver a llamar a debugfs por el mismo par
// (base, capa) mientras ninguno de los dos ficheros cambie (M-14): antes,
// comprobar el puente de una imagen con volúmenes o carpetas compartidas
// costaba hasta 4 debugfs en CADA arranque en frío, aunque la imagen no se
// hubiera tocado desde el arranque anterior.
func TestImageHasBridgeCacheada(t *testing.T) {
	m := newTestManager(t)
	base := filepath.Join(t.TempDir(), "base.ext4")
	if err := os.WriteFile(base, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	contador := filepath.Join(dir, "llamadas")
	fakeDebugfs(t, dir, contador)

	// Dos llamadas con el mismo fichero: la segunda tiene que venir de la
	// caché, sin invocar debugfs otra vez.
	for i := 0; i < 2; i++ {
		has, err := m.imageHasBridgeCached(t.Context(), base, "")
		if err != nil {
			t.Fatalf("vuelta %d: %v", i, err)
		}
		if !has {
			t.Fatalf("vuelta %d: quería el puente detectado", i)
		}
	}
	if n := contarLlamadas(t, contador); n != 1 {
		t.Errorf("debugfs se llamó %d veces tras 2 consultas iguales; quería 1", n)
	}

	// Reescribir el fichero (mtime distinto) invalida la caché: la próxima
	// consulta vuelve a mirar el disco.
	time.Sleep(2 * time.Millisecond) // el mtime tiene que avanzar de verdad
	if err := os.WriteFile(base, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.imageHasBridgeCached(t.Context(), base, ""); err != nil {
		t.Fatal(err)
	}
	if n := contarLlamadas(t, contador); n != 2 {
		t.Errorf("debugfs se llamó %d veces tras cambiar el fichero; quería 2 (una por versión)", n)
	}
}

// fakeDebugfs pone en el PATH un debugfs falso que anota cada llamada en
// contador y siempre dice que el fichero preguntado existe.
func fakeDebugfs(t *testing.T, dir, contador string) {
	t.Helper()
	script := "#!/bin/sh\necho x >> " + contador + "\necho 'Inode: 12  Type: regular'\n"
	if err := os.WriteFile(filepath.Join(dir, "debugfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	// Un guion de sh no pasa el encierro de e2fsprogs (solo deja ejecutar el
	// binario, y escribir solo en su disco).
	antes := confinarDisco
	confinarDisco = func(*exec.Cmd, *os.File) (func(), error) { return func() {}, nil }
	t.Cleanup(func() { confinarDisco = antes })
}

// contarLlamadas cuenta las líneas que fakeDebugfs fue anotando.
func contarLlamadas(t *testing.T, contador string) int {
	t.Helper()
	b, err := os.ReadFile(contador)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// Comparar por CONTENIDO y no por fecha o tamaño: dos puentes distintos pueden
// pesar lo mismo, y una imagen reescrita sin necesidad pierde su dispersión en
// disco.
func TestSeComparaPorContenido(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("mismo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("mismo"), 0o755); err != nil {
		t.Fatal(err)
	}
	da, err := digest.File(a)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := digest.File(b)
	if da != db {
		t.Error("dos ficheros idénticos dieron huellas distintas")
	}
	if err := os.WriteFile(b, []byte("otro!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dc, _ := digest.File(b); dc == da {
		t.Error("dos ficheros distintos del mismo tamaño dieron la misma huella")
	}
}

// La imagen que se monta para un put no es de fiar: ni setuid, ni nodos de
// dispositivo, ni ejecutables, y el punto de montaje en un directorio del
// daemon al que nadie más llega, no en /tmp.
func TestMontajeDelPutEsPrivadoYSinSuid(t *testing.T) {
	args := argsMontajePut("/img.ext4", "/mnt")
	if len(args) != 4 || args[0] != "-o" || args[2] != "/img.ext4" || args[3] != "/mnt" {
		t.Fatalf("argumentos de mount: %q", args)
	}
	opts := map[string]bool{}
	for _, o := range strings.Split(args[1], ",") {
		opts[o] = true
	}
	for _, w := range []string{"loop", "nosuid", "nodev", "noexec"} {
		if !opts[w] {
			t.Errorf("mount del put sin %s: %s", w, args[1])
		}
	}

	m := newTestManager(t)
	// Un build/ que quedó abierto de antes se cierra.
	if err := os.Mkdir(filepath.Join(m.root, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	mnt, err := m.dirMontajePut()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(mnt)
	if filepath.Dir(mnt) != filepath.Join(m.root, "build") {
		t.Fatalf("punto de montaje fuera de la raíz del daemon: %s", mnt)
	}
	fi, err := os.Lstat(filepath.Dir(mnt))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("build/ es %o: otros usuarios llegarían a la imagen montada", fi.Mode().Perm())
	}

	// build/ cambiado por un enlace: no se monta a través de él.
	m2 := newTestManager(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(m2.root, "build")); err != nil {
		t.Fatal(err)
	}
	if mnt, err := m2.dirMontajePut(); err == nil {
		os.Remove(mnt)
		t.Fatal("dirMontajePut siguió un enlace en build/")
	}
}

// Tampoco la de un dorado (M13): sus clones restauran RAM y overlay congelados
// sobre ese disco. Se niega como con una máquina viva (Busy, que el daemon da
// como 409) y dice qué hacer.
func TestPutImageFileNoTocaLaImagenDeUnDorado(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgs, "dorada.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	escribirSnapshot(t, m, "plantilla", api.Snapshot{Image: "dorada"})
	agente := filepath.Join(t.TempDir(), "agente")
	if err := os.WriteFile(agente, []byte("binario"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := m.PutImageFile(t.Context(), "dorada", "/usr/local/bin/kling-guest", agente, 0o755, true)
	if err == nil || !res.Busy {
		t.Fatalf("iba a tocar la imagen de un dorado: res=%+v err=%v", res, err)
	}
	if res.Updated {
		t.Error("dice que la actualizó pese a negarse")
	}
	for _, quiero := range []string{"plantilla", "kling template rm"} {
		if !strings.Contains(err.Error(), quiero) {
			t.Errorf("el error no dice %q: %v", quiero, err)
		}
	}
}

// Con un dorado encima, lo que ya está al día sigue diciéndolo (refresh es
// idempotente) sin escribir en el ext4; lo que cambiaría, se niega.
// Sin debugfs, el lector de ext4 en Go dice lo mismo: al día, distinto, no
// está, o (con un ext4 que no entiende) "distinto", que es negarse.
func TestSinTocarEsIgualSinDebugfs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	raiz := ext4.NewDir(0o755, 0, 0, now)
	if err := raiz.Put("/bin/agente", &ext4.Node{Mode: ext4.ModeReg | 0o755, Size: 7, Data: ext4.Bytes("agente\n"), Mtime: now}, now); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "img.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ext4.Write(f, raiz, nil, ext4.Options{}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, igual := fichero(t, "agente\n")
	_, otro := fichero(t, "otro\n")

	if e, ok := sinTocarEsIgualGo(img, "/bin/agente", igual); !e || !ok {
		t.Errorf("al día: existe=%v igual=%v", e, ok)
	}
	if e, ok := sinTocarEsIgualGo(img, "/bin/agente", otro); !e || ok {
		t.Errorf("distinto: existe=%v igual=%v", e, ok)
	}
	if e, ok := sinTocarEsIgualGo(img, "/bin/nada", igual); e || ok {
		t.Errorf("no está: existe=%v igual=%v", e, ok)
	}
	basura := filepath.Join(t.TempDir(), "basura")
	if err := os.WriteFile(basura, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e, ok := sinTocarEsIgualGo(basura, "/bin/agente", igual); !e || ok {
		t.Errorf("ext4 ilegible: existe=%v igual=%v; quería negarse", e, ok)
	}
}

func TestPutImageFileConDoradoAlDiaNoSeNiega(t *testing.T) {
	actual, _ := fichero(t, "agente\n")
	_, im := ext4Prueba(t, 16, "mkdir /bin", "cd /bin", "write "+comillas(actual)+" agente")
	m := newTestManager(t)
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(im.file, m.imagePath("dorada")); err != nil {
		t.Fatal(err)
	}
	escribirSnapshot(t, m, "plantilla", api.Snapshot{Image: "dorada"})
	antes, err := os.ReadFile(m.imagePath("dorada"))
	if err != nil {
		t.Fatal(err)
	}

	igual, _ := fichero(t, "agente\n")
	res, err := m.PutImageFile(t.Context(), "dorada", "/bin/agente", igual, 0o755, false)
	if err != nil || res.Busy || res.Updated {
		t.Fatalf("al día con un dorado: res=%+v err=%v", res, err)
	}
	if despues, _ := os.ReadFile(m.imagePath("dorada")); !bytes.Equal(antes, despues) {
		t.Error("se escribió en la imagen del dorado aunque ya estaba al día")
	}

	nuevo, _ := fichero(t, "agente nuevo\n")
	if res, err := m.PutImageFile(t.Context(), "dorada", "/bin/agente", nuevo, 0o755, false); err == nil || !res.Busy {
		t.Fatalf("contenido nuevo con un dorado: res=%+v err=%v", res, err)
	}
}
