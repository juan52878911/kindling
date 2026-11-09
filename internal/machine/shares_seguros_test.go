package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostshare "github.com/juan52878911/kindling/internal/share"
	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
)

// Tampoco se comparte nada DENTRO de la raíz de datos: con share_roots
// cubriendo su padre, <root>/volumes se servía rw.
func TestCarpetaDentroDeLaRaizDeDatos(t *testing.T) {
	m := newTestManager(t)
	vols := filepath.Join(m.root, "volumes")
	if err := os.MkdirAll(vols, 0o755); err != nil {
		t.Fatal(err)
	}
	m.SetShareConfig(func() ShareConfig { return ShareConfig{Roots: []string{filepath.Dir(m.root)}} })
	for _, src := range []string{vols, m.root} {
		_, err := m.resolveShares(context.Background(), api.RunRequest{Shares: []api.ShareSpec{
			{Mode: share.ModeRW, Mount: "/work", Source: src}}}, nil)
		if !errors.Is(err, ErrShareRequest) || !strings.Contains(err.Error(), "data root") {
			t.Errorf("%s: %v, want a data root refusal", src, err)
		}
	}
}

// Entre validar la carpeta y abrirla en un attach (o un thaw, o un reinicio)
// se puede cambiar por un enlace: lo abierto tiene que ser lo validado.
func TestCarpetaCambiadaPorEnlaceSeRechaza(t *testing.T) {
	m := newTestManager(t)
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	fuera := t.TempDir()
	m.SetShareConfig(func() ShareConfig { return ShareConfig{Roots: []string{base}} })
	rs, err := m.resolveShares(context.Background(), api.RunRequest{Shares: []api.ShareSpec{
		{Mode: share.ModeRW, Mount: "/work", Source: proj}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := rs[0].att
	t.Cleanup(func() { identCarpetas.Delete(s.Source) })
	comprobar := func() error {
		t.Helper()
		srv, err := hostshare.Open(s.Source, false, "")
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		return m.comprobarCarpeta(s, srv)
	}
	if err := comprobar(); err != nil {
		t.Fatalf("the validated directory: %v", err)
	}
	// Sin identidad guardada (daemon reiniciado) también vale y la guarda.
	identCarpetas.Delete(s.Source)
	if err := comprobar(); err != nil {
		t.Fatalf("after a restart: %v", err)
	}

	if err := os.Rename(s.Source, s.Source+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fuera, s.Source); err != nil {
		t.Fatal(err)
	}
	if err := comprobar(); err == nil {
		t.Fatal("a share swapped for a symlink outside share_roots was served")
	}
	// Ni tras un reinicio, que revalida la ruta.
	identCarpetas.Delete(s.Source)
	if err := comprobar(); err == nil {
		t.Fatal("after a restart, a share swapped for a symlink was served")
	}
}
