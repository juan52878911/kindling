//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
)

// virtio-fs es opt-in por carpeta: sin pedirlo, FUSE (con sus avisos de
// cambios del host); KLING_HV_VIRTIOFS=1 lo hace el defecto y =0 lo prohíbe.
// Una copia no tiene transporte.
func TestResolverTransporteCarpetas(t *testing.T) {
	m := newTestManager(t)
	raiz := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(raiz, 0o755); err != nil {
		t.Fatal(err)
	}
	m.SetShareConfig(func() ShareConfig { return ShareConfig{Roots: []string{raiz}} })
	resolver := func(env, transporte, modo string) (string, error) {
		t.Setenv("KLING_HV_VIRTIOFS", env)
		rs, err := m.resolveShares(context.Background(), api.RunRequest{Shares: []api.ShareSpec{
			{Mode: modo, Mount: "/w", Source: raiz, Transport: transporte}}}, nil)
		if err != nil {
			return "", err
		}
		return rs[0].att.Transport, nil
	}
	casos := []struct {
		env, transporte, quiero string
	}{
		{"", "", ""},
		{"", share.TransportVirtioFS, share.TransportVirtioFS},
		{"", share.TransportFUSE, ""},
		{"1", "", share.TransportVirtioFS},
		{"1", share.TransportFUSE, ""},
		{"0", "", ""},
		{"0", share.TransportFUSE, ""},
	}
	for _, c := range casos {
		got, err := resolver(c.env, c.transporte, share.ModeRW)
		if err != nil || got != c.quiero {
			t.Errorf("KLING_HV_VIRTIOFS=%q transport %q: %q, %v; want %q", c.env, c.transporte, got, err, c.quiero)
		}
	}
	if _, err := resolver("0", share.TransportVirtioFS, share.ModeRW); err == nil {
		t.Error("virtio-fs accepted with KLING_HV_VIRTIOFS=0")
	}
	if _, err := resolver("", "9p", share.ModeRW); err == nil {
		t.Error("unknown transport accepted")
	}
}
