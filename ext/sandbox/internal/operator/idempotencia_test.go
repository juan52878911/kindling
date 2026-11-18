package operator

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un Create cuya respuesta llega tarde (el cliente ya venció su plazo) no deja
// dos sandboxes: el siguiente intento encuentra el creado por su UID y lo
// adopta.
func TestCreateQueRespondeTardeNoDuplica(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	fr.tardaCrear = 600 * time.Millisecond
	sb := newTestSandbox("default", "lento")
	sb.Metadata.UID = "uid-lento"
	k8s.seed(sb)

	fc := fr.client()
	fc.http.Timeout = 200 * time.Millisecond
	ctrl := NewController(k8s.client(), fc, nil)
	shrinkBackoff(ctrl)
	ctrl.Resync = 100 * time.Millisecond
	runController(t, ctrl)

	waitFor(t, "status.id to be set", func() bool {
		sb, ok := k8s.get("default/lento")
		return ok && sb.Status.ID != ""
	})
	time.Sleep(500 * time.Millisecond) // más vueltas de resync
	if n, c := fr.vivos(), fr.createdCount(); n != 1 || c != 1 {
		t.Fatalf("%d sandboxes vivos tras %d creates; quería 1 y 1", n, c)
	}
}

// Un objeto que se borra antes de que su status llegara a escribirse (id
// vacío) borra el sandbox que lleva su UID antes de soltar el finalizer.
func TestBorrarSinStatusBorraElSandboxPorUID(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	fr.sandboxes["host1/sbx-x"] = &frontal.Sandbox{ID: "host1/sbx-x", Host: "host1", State: api.StateRunning,
		Labels: map[string]string{LabelK8sUID: "uid-huerfano"}}
	fr.sandboxes["host1/sbx-otro"] = &frontal.Sandbox{ID: "host1/sbx-otro", Host: "host1", State: api.StateRunning,
		Labels: map[string]string{LabelK8sUID: "uid-ajeno"}}

	now := time.Now()
	sb := newTestSandbox("default", "huerfano")
	sb.Metadata.UID = "uid-huerfano"
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	sb.Metadata.DeletionTimestamp = &now
	k8s.seed(sb)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	shrinkBackoff(ctrl)
	runController(t, ctrl)

	waitFor(t, "the object to be gone", func() bool {
		_, ok := k8s.get("default/huerfano")
		return !ok
	})
	fr.mu.Lock()
	_, sigue := fr.sandboxes["host1/sbx-x"]
	_, ajeno := fr.sandboxes["host1/sbx-otro"]
	fr.mu.Unlock()
	if sigue || !ajeno {
		t.Fatalf("sbx-x sigue=%v, sbx-otro sigue=%v; quería borrado solo el suyo", sigue, ajeno)
	}
}
