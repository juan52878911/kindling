package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Una warm cuyo volcado no vale (un thaw que retiró el sello y el daemon murió
// antes de escribir el running) no puede seguir warm: no se descongela nunca y
// el error decía «kling rm», que se lleva el disco. Pasa a stopped, con el
// motivo en LastErr, y su disco se conserva para kling start.
func TestReconcileWarmSinVolcadoValidoPasaAStopped(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	if err := retirarSello(dir); err != nil {
		t.Fatal(err)
	}
	frozen := time.Now().Add(-time.Minute)
	m.byID[id] = &api.Machine{ID: id, Name: "w", State: api.StateWarm, FrozenAt: &frozen}

	m.reconcile()

	mc := m.byID[id]
	if mc.State != api.StateStopped {
		t.Fatalf("estado %q, quería stopped", mc.State)
	}
	if mc.LastErr == "" {
		t.Fatal("pasó a stopped sin decir por qué")
	}
	if _, err := os.Stat(filepath.Join(dir, "overlay.ext4")); err != nil {
		t.Fatalf("se llevó el disco: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mem.file")); err == nil {
		t.Fatal("el volcado que ya no carga nadie sigue en disco")
	}
}

// Y una warm con su volcado sellado sigue warm.
func TestReconcileWarmConVolcadoSigueWarm(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	frozen := time.Now()
	m.byID[id] = &api.Machine{ID: id, Name: "w", State: api.StateWarm, FrozenAt: &frozen}
	m.reconcile()
	if st := m.byID[id].State; st != api.StateWarm {
		t.Fatalf("estado %q, quería warm", st)
	}
}

// El thaw de una warm sin volcado válido no manda a borrarla: kling stop y
// kling start conservan el disco.
func TestThawSinVolcadoNoMandaABorrar(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	if err := retirarSello(dir); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "w", State: api.StateWarm}
	_, err := m.Thaw(context.Background(), id)
	if err == nil {
		t.Fatal("descongeló un volcado sin sello")
	}
	if strings.Contains(err.Error(), "kling rm") || !strings.Contains(err.Error(), "kling stop w") {
		t.Fatalf("mensaje: %v", err)
	}
}

// Una warm sin ficheros de volcado no es el caso de un sello retirado: sigue
// warm, como antes (el thaw dice qué falta).
func TestReconcileWarmSinFicherosSigueWarm(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "w", State: api.StateWarm}
	m.reconcile()
	if st := m.byID[id].State; st != api.StateWarm {
		t.Fatalf("estado %q, quería warm", st)
	}
}
