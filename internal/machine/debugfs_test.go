package machine

import (
	"context"
	"errors"
	"testing"
)

// Sin debugfs, hasFile tiene que decir ErrNoDebugfs y no un error cualquiera:
// quien lo llama (sandbox create, run con volúmenes) sigue adelante con ese y
// solo con ese. Antes buscaba con LookPath y /sbin, y en un Mac con e2fsprogs
// keg-only de Homebrew no lo encontraba aunque estuviera.
func TestHasFileSinDebugfs(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	viejo := dirsE2fsExtra
	dirsE2fsExtra = nil
	t.Cleanup(func() { dirsE2fsExtra = viejo })
	if debugfsBin() != "" {
		t.Skip("debugfs en /sbin o /usr/sbin: no se puede simular su ausencia")
	}
	_, err := hasFile(context.Background(), "/no/existe.ext4", "/x")
	if !errors.Is(err, ErrNoDebugfs) {
		t.Fatalf("err = %v; quería ErrNoDebugfs", err)
	}
}
