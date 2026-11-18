package operator

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un objeto cuyo status.id apunta al sandbox de OTRO objeto (lo puede escribir
// quien tenga permiso sobre sandboxes/status) ni lo renueva ni, al borrarse,
// se lo lleva.
func TestStatusIDAjenoNiSeRenuevaNiSeBorra(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	fr.sandboxes["host1/sbx-b"] = &frontal.Sandbox{ID: "host1/sbx-b", Host: "host1", State: api.StateRunning,
		TTLSeconds: 300, Labels: map[string]string{LabelK8sUID: "uid-b"}}

	sb := newTestSandbox("ns-a", "a")
	sb.Metadata.UID = "uid-a"
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	sb.Metadata.Generation = 2
	sb.Spec.TTLSeconds = 900
	sb.Status = SandboxStatus{ID: "host1/sbx-b", Host: "host1", ObservedGeneration: 1}
	k8s.seed(sb)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx := testCtx(t)
	ctrl.remember(&sb)
	ctrl.reconcile(ctx, &sb)
	fr.mu.Lock()
	renews, ttl := fr.renews, fr.sandboxes["host1/sbx-b"].TTLSeconds
	fr.mu.Unlock()
	if renews != 0 || ttl != 300 {
		t.Fatalf("renewed another object's sandbox: %d renews, ttl %d", renews, ttl)
	}
	if got, _ := k8s.get("ns-a/a"); got.Status.Message == nil || !strings.Contains(*got.Status.Message, "not this object's sandbox") {
		t.Fatalf("status.message = %v", got.Status.Message)
	}

	// Borrado: suelta el finalizer sin tocar el sandbox ajeno.
	now := time.Now()
	sb.Metadata.DeletionTimestamp = &now
	k8s.seed(sb)
	ctrl = NewController(k8s.client(), fr.client(), nil)
	ctrl.remember(&sb)
	ctrl.reconcile(ctx, &sb)
	if fr.vivos() != 1 {
		t.Fatal("deleted another object's sandbox")
	}
	if got, ok := k8s.get("ns-a/a"); ok && got.Metadata.HasFinalizer() {
		t.Fatal("the finalizer was not removed")
	}
}

// status.id no se pega a la URL sin ser un id "host/máquina".
func TestRutaSandboxSoloAceptaIDs(t *testing.T) {
	if r, err := rutaSandbox("host1/abc123"); err != nil || r != "/v1/sandboxes/host1/abc123" {
		t.Fatalf("valid id: %q %v", r, err)
	}
	for _, id := range []string{"", "solo", "a/b/renew", "../x", "a/..", "a/b?x=1", "a/b%2f", "/a", "a/"} {
		if _, err := rutaSandbox(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

// Una pasada de resync pide UNA lista al frontal para todos los objetos, no un
// GET por objeto (que con el frontal caído sumaba 30 s cada uno).
func TestResyncUnaListaPorPasada(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	ctrl := NewController(k8s.client(), fr.client(), nil)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("host1/sbx-%d", i)
		uid := fmt.Sprintf("uid-%d", i)
		fr.sandboxes[id] = &frontal.Sandbox{ID: id, Host: "host1", State: api.StateRunning, Labels: map[string]string{LabelK8sUID: uid}}
		sb := newTestSandbox("default", fmt.Sprintf("s%d", i))
		sb.Metadata.UID = uid
		sb.Metadata.Finalizers = []string{CleanupFinalizer}
		sb.Status = SandboxStatus{ID: id, Host: "host1", State: string(api.StateRunning), ObservedGeneration: 1}
		k8s.seed(sb)
		ctrl.remember(&sb)
	}
	ctrl.resyncPasada(testCtx(t))
	fr.mu.Lock()
	listas, gets := fr.listas, fr.gets
	fr.mu.Unlock()
	if listas != 1 || gets != 0 {
		t.Fatalf("one resync pass made %d lists and %d gets, want 1 and 0", listas, gets)
	}
}

// Con un host caído, sus objetos no hacen cada uno su GET (y su lista) en
// serie: el primer GET que falla marca el host para el resto de la pasada, y
// los objetos sin status.id se quedan con la foto parcial.
func TestResyncConHostCaidoUnGETPorHost(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	fr.parcial = true
	fr.hostCaido = "host2"
	ctrl := NewController(k8s.client(), fr.client(), nil)
	for i := 0; i < 20; i++ {
		sb := newTestSandbox("default", fmt.Sprintf("s%d", i))
		sb.Metadata.UID = fmt.Sprintf("uid-%d", i)
		sb.Metadata.Finalizers = []string{CleanupFinalizer}
		if i%2 == 0 {
			id := fmt.Sprintf("host2/sbx-%d", i)
			sb.Status = SandboxStatus{ID: id, Host: "host2", State: string(api.StateRunning), ObservedGeneration: 1}
		}
		k8s.seed(sb)
		ctrl.remember(&sb)
	}
	ctrl.resyncPasada(testCtx(t))
	fr.mu.Lock()
	listas, gets := fr.listas, fr.getsCaido
	creates := fr.creates
	fr.mu.Unlock()
	if listas != 1 || gets != 1 {
		t.Fatalf("one resync pass with host2 down made %d lists and %d gets to it, want 1 and 1", listas, gets)
	}
	if creates != 0 {
		t.Fatalf("%d sandboxes created from a partial list", creates)
	}
}

// Un host que no contesta no deja en Terminating a los objetos cuyo sandbox
// está resuelto: se borra el del status y se suelta el finalizer. Sin status.id
// sí se espera a una lista completa (su sandbox podría estar en ese host).
func TestBorrarConListaParcial(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	fr.parcial = true
	fr.sandboxes["host1/sbx-1"] = &frontal.Sandbox{ID: "host1/sbx-1", Host: "host1", State: api.StateRunning,
		Labels: map[string]string{LabelK8sUID: "uid-1"}}
	now := time.Now()
	con := newTestSandbox("default", "con")
	con.Metadata.UID = "uid-1"
	con.Metadata.Finalizers = []string{CleanupFinalizer}
	con.Metadata.DeletionTimestamp = &now
	con.Status = SandboxStatus{ID: "host1/sbx-1", Host: "host1"}
	sin := newTestSandbox("default", "sin")
	sin.Metadata.UID = "uid-2"
	sin.Metadata.Finalizers = []string{CleanupFinalizer}
	sin.Metadata.DeletionTimestamp = &now
	k8s.seed(con)
	k8s.seed(sin)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctrl.remember(&con)
	ctrl.remember(&sin)
	ctrl.resyncPasada(testCtx(t))
	if fr.vivos() != 0 {
		t.Fatal("the status sandbox was not deleted")
	}
	if got, ok := k8s.get("default/con"); ok && got.Metadata.HasFinalizer() {
		t.Fatal("a partial list left the object with a resolved sandbox in Terminating")
	}
	if got, ok := k8s.get("default/sin"); !ok || !got.Metadata.HasFinalizer() {
		t.Fatal("without status.id the finalizer went away on a partial list")
	}
}

// El Deployment de ejemplo no levanta el operador nuevo antes de parar el
// viejo: sin lease, dos réplicas a la vez podían crear dos sandboxes por objeto.
func TestDeploymentRecreate(t *testing.T) {
	b, err := os.ReadFile("../../deploy/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "type: Recreate") {
		t.Fatal("deploy/deployment.yaml has no strategy Recreate")
	}
}

// Un "no está" de la foto de la pasada no se cree: un sandbox creado después
// (por el camino del watch) se adopta en vez de duplicarse.
func TestVistaViejaNoDuplica(t *testing.T) {
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	ctrl := NewController(k8s.client(), fr.client(), nil)
	v, err := ctrl.tomarVista(testCtx(t)) // la foto, antes de que exista
	if err != nil {
		t.Fatal(err)
	}
	fr.sandboxes["host1/sbx-w"] = &frontal.Sandbox{ID: "host1/sbx-w", Host: "host1", State: api.StateRunning,
		Labels: map[string]string{LabelK8sUID: "uid-w"}}
	sb := newTestSandbox("default", "w")
	sb.Metadata.UID = "uid-w"
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	k8s.seed(sb)
	ctrl.remember(&sb)
	ctrl.reconcileCon(testCtx(t), &sb, v)
	if fr.createdCount() != 0 || fr.vivos() != 1 {
		t.Fatalf("a stale view created a duplicate: %d created, %d alive", fr.createdCount(), fr.vivos())
	}
}
