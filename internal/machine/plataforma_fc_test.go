//go:build !darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func ficheroDisperso(t *testing.T, ruta string) {
	t.Helper()
	f, err := os.OpenFile(ruta, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("inicio"), 1<<20)
	f.Truncate(64 << 20)
	f.Close()
}

func bloquesDe(t *testing.T, p string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks
}

// H8: un sistema de ficheros que dice «todo son datos» (NFSv3, FUSE) hacía
// que la copia en proceso ocupara el tamaño lógico; sin huecos fiables se
// copia con cp --sparse=always, que detecta los ceros.
func TestCopiarDiscoSinHuecosFiablesNoSeDensifica(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	ficheroDisperso(t, src)
	antes := buscarEn
	buscarEn = func(f *os.File, off int64, whence int) (int64, error) {
		fi, _ := f.Stat()
		if whence == 3 {
			return off, nil
		}
		return fi.Size(), nil
	}
	t.Cleanup(func() { buscarEn = antes })
	huecosProbados.Store(dir, false)
	t.Cleanup(func() { huecosProbados.Delete(dir) })
	if out, err := copiarDisco(context.Background(), src, dst); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if bs, bd := bloquesDe(t, src), bloquesDe(t, dst); bd > bs+2048 {
		t.Fatalf("el destino ocupa %d bloques, el origen %d", bd, bs)
	}
}

// Si SEEK_HOLE no avanza el bucle se quedaba dando vueltas: ahora es un error.
func TestCopiarDispersoErrorSiSeekHoleNoAvanza(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	ficheroDisperso(t, src)
	antes := buscarEn
	buscarEn = func(f *os.File, off int64, whence int) (int64, error) { return off, nil }
	t.Cleanup(func() { buscarEn = antes })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := copiarDispersoEnProceso(ctx, src, dst)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("quería un error de SEEK_HOLE, obtuve %v (ctx %v)", err, ctx.Err())
	}
}
