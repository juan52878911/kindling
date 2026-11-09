package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
