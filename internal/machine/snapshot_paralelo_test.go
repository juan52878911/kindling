package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
)

// fakeRed cambia montarRedHost/desmontarRedHost por falsos y cuenta los desmontajes.
func fakeRed(t *testing.T, montar func() error) *int {
	t.Helper()
	oldM, oldD := montarRedHost, desmontarRedHost
	t.Cleanup(func() { montarRedHost, desmontarRedHost = oldM, oldD })
	desmontados := new(int)
	montarRedHost = func(*knet.Net, knet.Egress, []string, int) error { return montar() }
	desmontarRedHost = func(*knet.Net) { *desmontados++ }
	return desmontados
}

// El disco y la red se solapan: cada rama espera a que la otra haya empezado.
// En fila, la primera no ve nunca a la segunda y vence el plazo.
func TestClonarYMontarRedSolapa(t *testing.T) {
	m := newTestManager(t)
	clonIni, redIni := make(chan struct{}), make(chan struct{})
	fakeRed(t, func() error {
		close(redIni)
		select {
		case <-clonIni:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("la red no vio arrancar el clon: van en fila")
		}
	})
	dir := t.TempDir()
	id := "c0ffee00000000b1"
	modo, err := m.clonarYMontarRed(context.Background(), nuevoCronoCreacion(), dir, knet.Plan(30, id), id, knet.EgressNone, nil,
		func(context.Context) (string, error) {
			close(clonIni)
			select {
			case <-redIni:
				return cowModoReflink, nil
			case <-time.After(2 * time.Second):
				return "", errors.New("el clon no vio arrancar la red: van en fila")
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if modo != cowModoReflink {
		t.Errorf("modo = %q", modo)
	}
}

// Las fases llevan el tiempo de cada rama, no el de pared.
func TestClonarYMontarRedFasesPropias(t *testing.T) {
	m := newTestManager(t)
	fakeRed(t, func() error { time.Sleep(30 * time.Millisecond); return nil })
	f := nuevoCronoCreacion()
	id := "c0ffee00000000b2"
	_, err := m.clonarYMontarRed(context.Background(), f, t.TempDir(), knet.Plan(31, id), id, knet.EgressNone, nil,
		func(context.Context) (string, error) { time.Sleep(30 * time.Millisecond); return cowModoCopy, nil })
	if err != nil {
		t.Fatal(err)
	}
	if f.d[fcDisk] < 25*time.Millisecond || f.d[fcNet] < 25*time.Millisecond {
		t.Errorf("disk=%v net=%v, quería ~30 ms cada una", f.d[fcDisk], f.d[fcNet])
	}
}

// Si el clon falla con la red ya montada, se desmonta y se borra el directorio.
func TestClonarYMontarRedFalloClon(t *testing.T) {
	m := newTestManager(t)
	des := fakeRed(t, func() error { return nil })
	dir := filepath.Join(t.TempDir(), "maq")
	os.MkdirAll(dir, 0o755)
	id := "c0ffee00000000b3"
	_, err := m.clonarYMontarRed(context.Background(), nuevoCronoCreacion(), dir, knet.Plan(32, id), id, knet.EgressNone, nil,
		func(context.Context) (string, error) { return "", errors.New("sin sitio") })
	if err == nil {
		t.Fatal("quería error")
	}
	if *des != 1 {
		t.Errorf("desmontajes = %d, quería 1", *des)
	}
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Errorf("el directorio sigue ahí: %v", e)
	}
}

// Si falla la red, se borra el directorio (montarRed ya desmonta lo suyo).
func TestClonarYMontarRedFalloRed(t *testing.T) {
	m := newTestManager(t)
	fakeRed(t, func() error { return errors.New("ip falló") })
	dir := filepath.Join(t.TempDir(), "maq")
	os.MkdirAll(dir, 0o755)
	id := "c0ffee00000000b4"
	_, err := m.clonarYMontarRed(context.Background(), nuevoCronoCreacion(), dir, knet.Plan(33, id), id, knet.EgressNone, nil,
		func(context.Context) (string, error) { return cowModoCopy, nil })
	if err == nil {
		t.Fatal("quería error")
	}
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Errorf("el directorio sigue ahí: %v", e)
	}
}
