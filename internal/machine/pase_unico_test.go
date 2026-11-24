package machine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/juan52878911/kindling/internal/digest"
)

// puedeDigHoles dice si perforarHuecos (el camino de hoy) puede ir aquí: en
// Linux necesita fallocate; en macOS no hace nada y vale siempre.
func puedeDigHoles() (string, error) {
	if runtime.GOOS != "linux" {
		return "", nil
	}
	return exec.LookPath("fallocate")
}

// volcadoDeMuestra escribe en path un volcado como el de Firecracker: todo
// escrito (sin huecos), con trozos de datos, páginas a cero sueltas, un trozo
// de 4 MiB entero a cero y una cola que no llena el último trozo. Devuelve el
// contenido.
func volcadoDeMuestra(t testing.TB, path string) []byte {
	t.Helper()
	b := make([]byte, 13<<20+3*pagina)
	for i := range 1 << 20 {
		b[i] = byte(i*7 + 1) // el primer MiB, datos
	}
	// Páginas sueltas con un byte distinto de cero en medio y al final: no
	// son ceros y no se pueden perforar.
	b[2<<20+pagina/2] = 'x'
	b[3<<20+pagina-1] = 'y'
	// De 4 a 8 MiB, ceros escritos (un trozo entero).
	// De 8 MiB en adelante, una página de datos cada 64 KiB.
	for off := 8 << 20; off < len(b); off += 64 << 10 {
		copy(b[off:off+pagina], bytes.Repeat([]byte{'d'}, pagina))
	}
	b[len(b)-1] = 'z' // la cola
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

// El digest del pase único es el de hoy (fallocate --dig-holes y después
// digest.Tree) para el mismo contenido, y también el de digest.Tree sin
// perforar: un dorado nuevo vale lo mismo que uno de antes con esa memoria, y
// uno antiguo sigue verificando (verifyIntegrity no cambia).
func TestPaseUnicoDigestIgualQueTree(t *testing.T) {
	dir := t.TempDir()
	hoy, nuevo, crudo := filepath.Join(dir, "hoy"), filepath.Join(dir, "nuevo"), filepath.Join(dir, "crudo")
	want := volcadoDeMuestra(t, hoy)
	volcadoDeMuestra(t, nuevo)
	volcadoDeMuestra(t, crudo)

	if _, err := puedeDigHoles(); err == nil {
		if out, err := perforarHuecos(context.Background(), hoy); err != nil {
			t.Fatalf("fallocate: %v: %s", err, out)
		}
	}
	dHoy, err := digest.Tree(hoy)
	if err != nil {
		t.Fatal(err)
	}
	dCrudo, err := digest.Tree(crudo)
	if err != nil {
		t.Fatal(err)
	}
	dNuevo, err := paseUnico(context.Background(), nuevo)
	if err != nil {
		t.Fatal(err)
	}
	if dNuevo != dHoy || dNuevo != dCrudo {
		t.Errorf("pase único %s, fallocate+Tree %s, Tree sin perforar %s", dNuevo, dHoy, dCrudo)
	}
	if got, ok, err := digest.Matches(nuevo, dHoy); err != nil || !ok {
		t.Errorf("el volcado del pase único no verifica con el digest de hoy: %s, %v", got, err)
	}
	got, err := os.ReadFile(nuevo)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("el pase único cambió el contenido del volcado")
	}
}

// En Linux el pase perfora las páginas a cero: el volcado queda disperso,
// con lo mismo que perforaría fallocate --dig-holes (como mucho, lo mismo
// asignado), y las páginas con un solo byte distinto de cero se quedan.
func TestPaseUnicoPerforaCeros(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("solo Linux perfora el volcado (en macOS el mem.file no es RAM cruda)")
	}
	dir := t.TempDir()
	if !sabeDeHuecos(t, dir) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	nuevo := filepath.Join(dir, "nuevo")
	want := volcadoDeMuestra(t, nuevo)
	antes := allocatedBytes(nuevo)
	if _, err := paseUnico(context.Background(), nuevo); err != nil {
		t.Fatal(err)
	}
	despues := allocatedBytes(nuevo)
	// Datos: 1 MiB + 2 páginas sueltas + una página cada 64 KiB desde 8 MiB +
	// la cola. Lo demás era cero.
	datos := int64(1<<20 + 2*pagina + ((len(want)-(8<<20))/(64<<10)+1)*pagina + pagina)
	if despues >= antes || despues > datos+(64<<10) {
		t.Errorf("asignado %d -> %d bytes; con datos en %d, el pase no perforó los ceros", antes, despues, datos)
	}
	if _, err := puedeDigHoles(); err == nil {
		hoy := filepath.Join(dir, "hoy")
		volcadoDeMuestra(t, hoy)
		if out, err := perforarHuecos(context.Background(), hoy); err != nil {
			t.Fatalf("fallocate: %v: %s", err, out)
		}
		if despues > allocatedBytes(hoy) {
			t.Errorf("el pase deja %d bytes asignados y fallocate %d", despues, allocatedBytes(hoy))
		}
	}
	got, err := os.ReadFile(nuevo)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("perforar cambió el contenido del volcado")
	}
}

// Un volcado que cambia de tamaño a mitad no es este caso, pero uno que no
// se puede abrir para escribir (perforar necesita escribir) es un error, no
// un digest de lo que se pudo leer.
func TestPaseUnicoSinFichero(t *testing.T) {
	if _, err := paseUnico(context.Background(), filepath.Join(t.TempDir(), "no-esta")); err == nil {
		t.Error("el pase de un volcado que no existe no dio error")
	}
}

// El commit de un pase rellena el mismo digest que daría hoy: el de
// digest.Tree del contenido que volcó el VMM.
func TestCommitDigestDeMemoriaIgualQueHoy(t *testing.T) {
	puedePerforar(t)
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	id := "c9aa170000000001"
	falso, _ := plantillaParaCommit(t, m, id)
	ref := filepath.Join(t.TempDir(), "ref")
	volcadoDeMuestra(t, ref)
	want, err := digest.Tree(ref)
	if err != nil {
		t.Fatal(err)
	}
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			volcadoFalso(m, id, dir)
			mem := filepath.Join(dir, "mem.file")
			if volcadoEnLaMaquina {
				mem = filepath.Join(m.dir(id), volcadoTmpMem)
			}
			volcadoDeMuestra(t, mem)
		}
	})
	snap, err := m.Commit(context.Background(), id, "dorado", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if snap.MemDigest != want {
		t.Errorf("mem_digest %s, el de hoy para ese contenido %s", snap.MemDigest, want)
	}
	if _, ok, err := digest.Matches(filepath.Join(dir, "mem.file"), snap.MemDigest); err != nil || !ok {
		t.Errorf("el mem.file del dorado no verifica con su digest: %v", err)
	}
}

// BenchmarkPerforarYDigest mide lo que el commit hace tras el volcado con la
// memoria: hoy dos pases (fallocate --dig-holes y digest.Tree), y el pase
// único. En macOS no se perfora (los dos caminos son digest.Tree), así que la
// cifra que importa es la de Linux.
func BenchmarkPerforarYDigest(b *testing.B) {
	dir := b.TempDir()
	p := filepath.Join(dir, "mem")
	b.Run("dos-pases", func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			volcadoDeMuestra(b, p)
			b.StartTimer()
			if _, err := puedeDigHoles(); err == nil {
				if out, err := perforarHuecos(context.Background(), p); err != nil {
					b.Fatalf("%v: %s", err, out)
				}
			}
			if _, err := digest.Tree(p); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("pase-unico", func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			volcadoDeMuestra(b, p)
			b.StartTimer()
			if _, err := paseUnico(context.Background(), p); err != nil {
				b.Fatal(err)
			}
		}
	})
}
