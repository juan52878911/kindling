package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/share"
)

// Una máquina con carpetas compartidas no se convierte en snapshot: 409.
func TestCommitConCarpetaSeRechaza(t *testing.T) {
	m := newTestManager(t)
	mc := m.addForTest("abcdef0123456789")
	m.mu.Lock()
	mc.Shares = []api.ShareAttachment{{Mode: share.ModeRW, Mount: "/work", Source: "/srv/x"}}
	m.mu.Unlock()
	_, err := m.Commit(context.Background(), mc.ID, "snap", false)
	if !errors.Is(err, ErrSharesCommit) {
		t.Fatalf("commit with a live share: %v, want ErrSharesCommit", err)
	}
	if !strings.Contains(err.Error(), "freeze/thaw") {
		t.Errorf("the message does not say what does work: %v", err)
	}
}

// Las carpetas se piden al arrancar en frío: con From, 400.
func TestRunFromConCarpetaSeRechaza(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Run(context.Background(), api.RunRequest{From: "snap",
		Shares: []api.ShareSpec{{Mode: share.ModeCopy, Mount: "/work", Upload: strings.Repeat("a", 32)}}})
	if !errors.Is(err, ErrShareRequest) {
		t.Fatalf("run -from with shares: %v, want ErrShareRequest", err)
	}
}

func TestResolverCarpetas(t *testing.T) {
	m := newTestManager(t)
	base := t.TempDir()
	permitido := filepath.Join(base, "code")
	if err := os.MkdirAll(filepath.Join(permitido, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	fuera := filepath.Join(base, "other")
	if err := os.MkdirAll(fuera, 0o755); err != nil {
		t.Fatal(err)
	}
	// Un enlace dentro de la raíz permitida que apunta fuera: se resuelve y se
	// rechaza.
	if err := os.Symlink(fuera, filepath.Join(permitido, "sneaky")); err != nil {
		t.Fatal(err)
	}
	up := strings.Repeat("b", 32)
	if err := os.MkdirAll(m.uploadsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.uploadPath(up), []byte("ext4"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolver := func(roots []string, vols []resolvedVolume, specs ...api.ShareSpec) ([]resolvedShare, error) {
		m.SetShareConfig(func() ShareConfig { return ShareConfig{Roots: roots} })
		return m.resolveShares(api.RunRequest{Shares: specs}, vols)
	}

	// Sin raíces: las vivas se rechazan diciendo cómo permitirlas.
	_, err := resolver(nil, nil, api.ShareSpec{Mode: share.ModeRW, Mount: "/work", Source: permitido})
	if err == nil || !strings.Contains(err.Error(), "daemon.share_roots") {
		t.Fatalf("live share without roots: %v", err)
	}
	// Dentro de la raíz: vale, y la ruta queda resuelta.
	rs, err := resolver([]string{permitido}, nil, api.ShareSpec{Mode: share.ModeRO, Mount: "/work", Source: filepath.Join(permitido, "repo")})
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(filepath.Join(permitido, "repo")); rs[0].att.Source != real {
		t.Errorf("source not resolved: %s", rs[0].att.Source)
	}
	for name, spec := range map[string]api.ShareSpec{
		"outside root":     {Mode: share.ModeRW, Mount: "/work", Source: fuera},
		"symlink outside":  {Mode: share.ModeRW, Mount: "/work", Source: filepath.Join(permitido, "sneaky")},
		"relative":         {Mode: share.ModeRW, Mount: "/work", Source: "code/repo"},
		"data root":        {Mode: share.ModeRW, Mount: "/work", Source: filepath.Dir(m.root)},
		"not a dir":        {Mode: share.ModeRW, Mount: "/work", Source: m.uploadPath(up)},
		"bad mount":        {Mode: share.ModeRW, Mount: "/etc/x", Source: permitido},
		"root mount":       {Mode: share.ModeCopy, Mount: "/", Upload: up},
		"bad mode":         {Mode: "rwx", Mount: "/work", Source: permitido},
		"copy no upload":   {Mode: share.ModeCopy, Mount: "/work"},
		"copy bad upload":  {Mode: share.ModeCopy, Mount: "/work", Upload: "../../etc/passwd"},
		"copy gone upload": {Mode: share.ModeCopy, Mount: "/work", Upload: strings.Repeat("c", 32)},
	} {
		roots := []string{permitido, filepath.Dir(m.root)}
		if name != "data root" {
			roots = []string{permitido}
		}
		if _, err := resolver(roots, nil, spec); !errors.Is(err, ErrShareRequest) {
			t.Errorf("%s: %v, want ErrShareRequest", name, err)
		}
	}
	// Puntos de montaje repetidos, anidados o encima de un volumen.
	vol := []resolvedVolume{{name: "data", mount: "/data"}}
	for name, specs := range map[string][]api.ShareSpec{
		"same as volume": {{Mode: share.ModeCopy, Mount: "/data", Upload: up}},
		"nested":         {{Mode: share.ModeRO, Mount: "/work", Source: permitido}, {Mode: share.ModeCopy, Mount: "/work/sub", Upload: up}},
		"under volume":   {{Mode: share.ModeCopy, Mount: "/data/x", Upload: up}},
		"twice":          {{Mode: share.ModeRO, Mount: "/w", Source: permitido}, {Mode: share.ModeRW, Mount: "/w", Source: permitido}},
	} {
		if _, err := resolver([]string{permitido}, vol, specs...); !errors.Is(err, ErrShareRequest) {
			t.Errorf("%s: %v, want ErrShareRequest", name, err)
		}
	}
	// Demasiadas.
	var muchas []api.ShareSpec
	for i := 0; i <= share.MaxShares; i++ {
		muchas = append(muchas, api.ShareSpec{Mode: share.ModeRO, Mount: "/w" + string(rune('a'+i)), Source: permitido})
	}
	if _, err := resolver([]string{permitido}, nil, muchas...); !errors.Is(err, ErrShareRequest) {
		t.Errorf("too many shares: %v", err)
	}
}

// El estado de las carpetas vivas se calcula al responder y no toca la
// máquina guardada.
func TestDecorarCarpetas(t *testing.T) {
	m := newTestManager(t)
	mc := m.addForTest("0123456789abcdef")
	m.mu.Lock()
	mc.Shares = []api.ShareAttachment{{Mode: share.ModeCopy, Mount: "/c"}, {Mode: share.ModeRW, Mount: "/w", Source: "/x"}}
	m.mu.Unlock()
	got, _ := m.Get(mc.ID)
	if got.Shares[0].Status != "" || got.Shares[1].Status != "detached" {
		t.Fatalf("statuses %+v", got.Shares)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mc.Shares[1].Status != "" {
		t.Fatal("decorating changed the stored machine")
	}
}
