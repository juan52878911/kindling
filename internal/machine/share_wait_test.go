package machine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/share"
)

// kling.shares=N: el agente espera a las carpetas vivas pedidas, o a las
// de ShareWait (una plantilla preparada sin carpetas).
func TestShareWaitDe(t *testing.T) {
	vivas := []api.ShareAttachment{{Mode: share.ModeRW, Mount: "/app"}, {Mode: share.ModeCopy, Mount: "/c"}, {Mode: share.ModeRO, Mount: "/r"}}
	if n := shareWaitDe(0, vivas); n != 2 {
		t.Errorf("two live shares and a copy: waits for %d", n)
	}
	if n := shareWaitDe(3, nil); n != 3 {
		t.Errorf("share_wait 3 without shares: waits for %d", n)
	}
	if a := shareWaitBootArg(0); a != "" {
		t.Errorf("without shares the kernel line gets %q", a)
	}
	if a := shareWaitBootArg(2); a != " kling.shares=2" {
		t.Errorf("kernel line: %q", a)
	}
	if validarShareWait(-1) == nil || validarShareWait(share.MaxShares+1) == nil || validarShareWait(1) != nil {
		t.Error("share_wait bounds")
	}
}

// Con From, una carpeta viva solo entra en una plantilla cuyo agente la
// espera; share_wait no se pide a una copia.
func TestRunFromConCarpetaVivaPideShareWait(t *testing.T) {
	m := newTestManager(t)
	vivo := []api.ShareSpec{{Mode: share.ModeRW, Mount: "/app", Source: t.TempDir()}}
	_, err := m.Run(context.Background(), api.RunRequest{From: "snap", ShareWait: 1})
	if !errors.Is(err, ErrShareRequest) {
		t.Fatalf("run -from with share_wait: %v, want ErrShareRequest", err)
	}
	dir := m.snapDir("snap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(dir, []byte(`{"name":"snap","image":"img"}`)); err != nil {
		t.Fatal(err)
	}
	_, err = m.Run(context.Background(), api.RunRequest{From: "snap", Shares: vivo})
	if !errors.Is(err, ErrShareRequest) || !strings.Contains(err.Error(), "share_wait") {
		t.Fatalf("run -from a template that does not wait, with a live share: %v", err)
	}
}
