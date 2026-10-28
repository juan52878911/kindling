//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// bootVZFalso arranca en el kling-vz falso una máquina con ese on_ttl y
// devuelve las llamadas que recibió.
func bootVZFalso(t *testing.T, onTTL string) []string {
	t.Helper()
	m, logPath := managerVZ(t)
	id := "dd11bb22cc33dd44"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz", State: api.StateCreated, OnTTL: onTTL}
	base := filepath.Join(m.root, "base.ext4")
	overlay := filepath.Join(m.dir(id), "overlay.ext4")
	for _, f := range []string{base, overlay} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pid, err := m.boot(ctx, id, 1, 256, 0, base, "", overlay, knet.Plan(1, id), nil, false, false, nil, 0)
	defer matarVMM(pid)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	return llamadas(t, logPath)
}

// Un sandbox (on_ttl=remove) manda su overlay como desechable, y solo el
// overlay: la base de solo lectura no lleva el campo.
func TestVZBootOverlayDesechableConOnTTLRemove(t *testing.T) {
	ls := bootVZFalso(t, api.OnTTLRemove)
	if indice(ls, "PUT /drives/overlay disposable=true") < 0 {
		t.Fatalf("el overlay de un sandbox no viajó como desechable:\n%s", strings.Join(ls, "\n"))
	}
	if i := indice(ls, "PUT /drives/rootfs"); i < 0 || strings.Contains(ls[i], "disposable") {
		t.Fatalf("la base no debe llevar kling_disposable:\n%s", strings.Join(ls, "\n"))
	}
}

// Una máquina que se congela al vencer (servicios, réplicas de un dorado) no:
// su overlay es parte del estado que vuelve con el thaw.
func TestVZBootOverlayNoDesechableConOnTTLFreeze(t *testing.T) {
	for _, onTTL := range []string{"", api.OnTTLFreeze} {
		ls := bootVZFalso(t, onTTL)
		i := indice(ls, "PUT /drives/overlay")
		if i < 0 || strings.Contains(ls[i], "disposable") {
			t.Fatalf("on_ttl=%q: el overlay no debe llevar kling_disposable:\n%s", onTTL, strings.Join(ls, "\n"))
		}
	}
}

// arranqueHost lee kern.boottime: una hora pasada y no muy lejana.
func TestArranqueHostEsPasado(t *testing.T) {
	a, ok := arranqueHost()
	if !ok {
		t.Fatal("kern.boottime no se pudo leer")
	}
	if !a.Before(time.Now()) || time.Since(a) > 5*365*24*time.Hour {
		t.Fatalf("arranque del host absurdo: %v", a)
	}
}
