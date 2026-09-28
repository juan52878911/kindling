package machine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// volumenDePrueba crea volumes/<n>.ext4 con datos al principio y un hueco
// grande detrás: basta para comprobar contenido y dispersión sin mkfs.
func volumenDePrueba(t *testing.T, m *Manager, n string) []byte {
	t.Helper()
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	datos := bytes.Repeat([]byte("kindling-"), 8<<10) // ~72 KiB
	f, err := os.Create(m.volumePath(n))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(datos); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil { // 64 MiB lógicos
		t.Fatal(err)
	}
	f.Close()
	return datos
}

func mismoContenido(t *testing.T, a, b string) {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(x, y) {
		t.Fatalf("%s y %s difieren", filepath.Base(a), filepath.Base(b))
	}
}

// sinRestos comprueba que un clon, salga bien o mal, no deja ni el .tmp ni
// los ficheros de la sonda.
func sinRestos(t *testing.T, m *Manager) {
	t.Helper()
	for _, d := range dirsSonda {
		entries, _ := os.ReadDir(filepath.Join(m.root, d))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), ".kling-clone-probe") {
				t.Errorf("queda %s/%s", d, e.Name())
			}
		}
	}
}

// fingirReflink sustituye el clon por una copia que dice ser un clon: es la
// única forma de ejercitar el camino bueno en un host ext4, que es donde
// corren la mayoría de los tests.
func fingirReflink(t *testing.T) {
	t.Helper()
	orig := clonar
	clonar = func(src, dst string) error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	}
	t.Cleanup(func() { clonar = orig })
}

// El clon REAL de este host: en XFS/btrfs/APFS tiene que salir y ser idéntico;
// en ext4 tiene que negarse con errSinClon, sin copiar a escondidas y sin
// dejar nada a medias. Las dos ramas son el comportamiento correcto.
func TestCloneVolumeRealDeEsteHost(t *testing.T) {
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")

	res, err := m.CloneVolume(context.Background(), "origen", "rama", false)
	if err != nil {
		if !errors.Is(err, ErrSinClon) {
			t.Fatalf("un host sin reflink debe dar ErrSinClon, no: %v", err)
		}
		if !strings.Contains(err.Error(), "-copy") {
			t.Errorf("el error debe decir cómo seguir (-copy): %v", err)
		}
		if _, err := os.Stat(m.volumePath("rama")); !os.IsNotExist(err) {
			t.Errorf("sin clon no debe existir el destino (err=%v)", err)
		}
		t.Logf("este host no clona (%s): comprobado el rechazo", tipoFS(m.volumesDir()))
	} else {
		if res.Method != metodoClon {
			t.Errorf("método = %q, quiero %q", res.Method, metodoClon)
		}
		mismoContenido(t, m.volumePath("origen"), m.volumePath("rama"))
		t.Logf("clon real con %s sobre %s en %d ms", res.Method, res.Filesystem, res.ElapsedMS)
	}
	sinRestos(t, m)
}

// Con -copy siempre sale: clon si se puede, copia dispersa si no. La copia
// tiene que conservar los huecos, o un volumen de 64 GiB casi vacío costaría
// 64 GiB.
func TestCloneVolumeConCopiaSiempreSale(t *testing.T) {
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")

	res, err := m.CloneVolume(context.Background(), "origen", "rama", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != metodoClon && res.Method != api.CloneCopy {
		t.Fatalf("método inesperado %q", res.Method)
	}
	mismoContenido(t, m.volumePath("origen"), m.volumePath("rama"))
	if res.Volume == nil || res.Volume.Name != "rama" || res.Volume.SizeBytes != 64<<20 {
		t.Fatalf("volumen devuelto = %+v", res.Volume)
	}
	if res.Method == api.CloneCopy && res.Volume.UsedBytes > 8<<20 {
		t.Errorf("la copia no conservó la dispersión: %d bytes asignados de 64 MiB", res.Volume.UsedBytes)
	}
	sinRestos(t, m)
}

// Con un clon que funciona, el resultado dice el método de la plataforma y el
// volumen nuevo aparece en la lista como uno más.
func TestCloneVolumeConReflink(t *testing.T) {
	fingirReflink(t)
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")

	res, err := m.CloneVolume(context.Background(), "origen", "rama", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != metodoClon {
		t.Errorf("método = %q, quiero %q", res.Method, metodoClon)
	}
	var nombres []string
	for _, v := range m.Volumes() {
		nombres = append(nombres, v.Name)
	}
	if strings.Join(nombres, ",") != "origen,rama" {
		t.Errorf("volúmenes = %v", nombres)
	}
	m.mu.RLock()
	quedan := len(m.volReservas)
	m.mu.RUnlock()
	if quedan != 0 {
		t.Errorf("quedan %d reservas tras el clon", quedan)
	}
}

// Un volumen con un escritor vivo no se clona: la foto saldría a medio
// escribir. Con lectores sí, porque nadie cambia los bloques.
func TestCloneVolumeSeNiegaConEscritor(t *testing.T) {
	fingirReflink(t)
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")

	mc := m.addForTest("escribe")
	m.mu.Lock()
	mc.Volumes = []api.VolumeAttachment{{Name: "origen", Mount: "/data"}}
	m.mu.Unlock()

	_, err := m.CloneVolume(context.Background(), "origen", "rama", true)
	if !errors.Is(err, ErrVolumenEnUso) || !strings.Contains(err.Error(), "escribe") {
		t.Fatalf("debería negarse nombrando al escritor, dio: %v", err)
	}
	if _, err := os.Stat(m.volumePath("rama")); !os.IsNotExist(err) {
		t.Error("no debe crear el destino")
	}

	m.mu.Lock()
	mc.Volumes[0].ReadOnly = true
	m.mu.Unlock()
	if _, err := m.CloneVolume(context.Background(), "origen", "rama", false); err != nil {
		t.Fatalf("con un lector debería clonar: %v", err)
	}
}

// Mientras el clon está en marcha, el origen no se puede montar en escritura
// ni borrar, y el destino no lo puede reclamar otro clon. Es lo que evita que
// una copia lenta (-copy) se lleve un volumen que alguien está cambiando.
func TestCloneVolumeReservaMientrasClona(t *testing.T) {
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")

	dentro, soltar := make(chan struct{}), make(chan struct{})
	orig := clonar
	clonar = func(src, dst string) error {
		close(dentro)
		<-soltar
		return orig(src, dst)
	}
	t.Cleanup(func() { clonar = orig })

	hecho := make(chan error, 1)
	go func() {
		_, err := m.CloneVolume(context.Background(), "origen", "rama", true)
		hecho <- err
	}()
	select {
	case <-dentro:
	case <-time.After(5 * time.Second):
		t.Fatal("el clon no llegó a empezar")
	}

	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "origen"}, "vm1", "vm1"); err == nil {
		t.Error("montar el origen en escritura durante el clon debería fallar")
	}
	if err := m.RemoveVolume("origen"); err == nil {
		t.Error("borrar el origen durante el clon debería fallar")
	}
	if _, err := m.CloneVolume(context.Background(), "origen", "rama", true); !errors.Is(err, ErrVolumenEnUso) {
		t.Errorf("otro clon al mismo destino debería dar ErrVolumenEnUso: %v", err)
	}
	// Leerlo sí se puede: un lector no cambia nada.
	if _, err := m.reservarVolumenes(api.RunRequest{Volumes: []api.VolumeAttachment{{Name: "origen", Mount: "/data", ReadOnly: true}}}, "vm2", "vm2"); err != nil {
		t.Errorf("montar el origen en lectura durante el clon debería poder: %v", err)
	}
	m.soltarReservas("vm2")

	close(soltar)
	if err := <-hecho; err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveVolume("origen"); err != nil {
		t.Errorf("terminado el clon, el origen se puede borrar: %v", err)
	}
}

func TestCloneVolumeValidaNombres(t *testing.T) {
	fingirReflink(t)
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")
	volumenDePrueba(t, m, "ya")

	casos := []struct {
		src, dst string
		quiero   error // nil = solo que falle
	}{
		{"origen", "../fuera", nil},
		{"../images/min", "rama", nil},
		{"origen", "origen", nil},
		{"no-existe", "rama", nil},
		{"origen", "ya", os.ErrExist},
	}
	for _, c := range casos {
		_, err := m.CloneVolume(context.Background(), c.src, c.dst, true)
		if err == nil {
			t.Errorf("%s → %s: debería fallar", c.src, c.dst)
			continue
		}
		if c.quiero != nil && !errors.Is(err, c.quiero) {
			t.Errorf("%s → %s: %v, quiero %v", c.src, c.dst, err, c.quiero)
		}
	}
	sinRestos(t, m)
}

// Un fallo que NO es "no sé clonar" (disco lleno, E/S) no cae a copia aunque
// se permita: copiar 50 GiB en un disco que acaba de decir ENOSPC solo lo
// llenaría más.
func TestCloneVolumeFalloRealNoCaeACopia(t *testing.T) {
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")
	orig, origCopiar := clonar, copiar
	clonar = func(src, dst string) error { return fmt.Errorf("no space left on device") }
	copiado := false
	copiar = func(ctx context.Context, src, dst string) error { copiado = true; return nil }
	t.Cleanup(func() { clonar, copiar = orig, origCopiar })

	if _, err := m.CloneVolume(context.Background(), "origen", "rama", true); err == nil {
		t.Fatal("debería fallar")
	}
	if copiado {
		t.Error("un fallo real no debe caer a copia")
	}
	sinRestos(t, m)
}

// La sonda prueba cada directorio que exista, no crea los que no, y no deja
// ficheros. En este host dirá lo que sea verdad; lo que se comprueba es que
// sea coherente: volumes/ siempre aparece, y Method solo si volumes/ clona.
func TestSondaDeClon(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"machines", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	info := sondar(root)
	vistos := map[string]api.CloneDir{}
	for _, d := range info.Dirs {
		vistos[d.Dir] = d
	}
	if _, ok := vistos["jails"]; ok {
		t.Error("jails/ no existe y no debería sondarse")
	}
	if _, err := os.Stat(filepath.Join(root, "jails")); !os.IsNotExist(err) {
		t.Error("la sonda no debe crear directorios que no existían")
	}
	vol, ok := vistos["volumes"]
	if !ok {
		t.Fatalf("falta volumes/ en %+v", info.Dirs)
	}
	if (info.Method != "") != vol.Reflink {
		t.Errorf("Method=%q pero volumes.Reflink=%v", info.Method, vol.Reflink)
	}
	if !vol.Reflink && vol.Error == "" {
		t.Error("un no tiene que decir por qué")
	}
	for _, d := range []string{"volumes", "machines", "snapshots"} {
		entries, _ := os.ReadDir(filepath.Join(root, d))
		for _, e := range entries {
			t.Errorf("la sonda dejó %s/%s", d, e.Name())
		}
	}
	t.Logf("sonda: método %q, %+v", info.Method, info.Dirs)
}

// Con un clon que funciona, la sonda lo dice para cada directorio.
func TestSondaDeClonConReflink(t *testing.T) {
	fingirReflink(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "machines"), 0o755); err != nil {
		t.Fatal(err)
	}
	info := sondar(root)
	if info.Method != metodoClon || len(info.Dirs) != 2 {
		t.Fatalf("sonda = %+v", info)
	}
	for _, d := range info.Dirs {
		if !d.Reflink || d.Error != "" {
			t.Errorf("%s: %+v", d.Dir, d)
		}
	}
}

// Un `volume create` del mismo nombre mientras el clon copia: el clon no
// pisa el volumen nuevo ni el create le borra el temporal. Gana quien llega
// primero y el otro se entera con ErrExist.
func TestCloneVolumeNoPisaUnVolumenCreadoALaVez(t *testing.T) {
	m := newTestManager(t)
	volumenDePrueba(t, m, "origen")
	orig := clonar
	clonar = func(src, dst string) error {
		// Mientras "clona", alguien crea el destino por otro camino.
		if err := os.WriteFile(m.volumePath("rama"), []byte("otro"), 0o644); err != nil {
			t.Error(err)
		}
		return orig(src, dst)
	}
	t.Cleanup(func() { clonar = orig })

	_, err := m.CloneVolume(context.Background(), "origen", "rama", true)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("debería dar ErrExist, dio: %v", err)
	}
	if b, _ := os.ReadFile(m.volumePath("rama")); string(b) != "otro" {
		t.Errorf("el clon pisó el volumen que se creó a la vez")
	}
	sinRestos(t, m)
}
