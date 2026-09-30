package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Commit y fork esperan a "listo" con SinAgenteVale: sin nadie en el puerto
// del agente, se congela sin esperar. Pero si ya se sabe que la imagen declara
// sonda (su agente contestó "waiting" antes), nadie en el puerto es un invitado
// que aún arranca o cuyo agente se cayó, no uno listo: congelarlo daba un
// dorado a medio arrancar.
func TestListoParaCongelarNoDaPorListoSinAgenteSiLaImagenDeclara(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}}
	id := "abcdef0123456789"
	// Un puerto donde no escucha nadie: 127.0.0.1:1.
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, Ready: api.ReadyWaiting,
		Forwards: map[string]string{"8080": "127.0.0.1:1"}}

	err := m.listoParaCongelar(context.Background(), id, 300*time.Millisecond)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("listoParaCongelar = %v; con sonda declarada y sin agente no está listo", err)
	}

	// Sin nada declarado (o sin saberlo), lo de siempre: sin agente no hay
	// nada que esperar.
	m.mu.Lock()
	m.byID[id].Ready = api.ReadyUnknown
	m.mu.Unlock()
	t0 := time.Now()
	if err := m.listoParaCongelar(context.Background(), id, time.Minute); err != nil {
		t.Fatalf("sin nada declarado: %v", err)
	}
	if time.Since(t0) > 3*time.Second {
		t.Errorf("sin nada declarado no hay que esperar al plazo: %s", time.Since(t0))
	}
}

// Y antes de que el agente conteste nunca (recién arrancada), lo dice la
// imagen: una sonda o un directorio de ganchos en su rootfs.
func TestImagenDeclaraListo(t *testing.T) {
	m, im := ext4Prueba(t, 8, "mkdir etc", "mkdir etc/kindling", "mkdir etc/kindling/post-restore.d")
	_, sin := ext4Prueba(t, 8, "mkdir etc")
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	for nombre, src := range map[string]string{"con": im.file, "sin": sin.file} {
		if err := os.Rename(src, m.imagePath(nombre)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if decl, err := m.imagenDeclaraListo(ctx, "con"); err != nil || !decl {
		t.Fatalf("imagen con ganchos: declara=%v err=%v", decl, err)
	}
	if decl, err := m.imagenDeclaraListo(ctx, "sin"); err != nil || decl {
		t.Fatalf("imagen sin nada: declara=%v err=%v", decl, err)
	}

	m.byID = map[string]*api.Machine{}
	id := "0123456789abcdef"
	m.byID[id] = &api.Machine{ID: id, Name: "y", State: api.StateRunning, Image: "con",
		Forwards: map[string]string{"8080": "127.0.0.1:1"}}
	if err := m.listoParaCongelar(ctx, id, 300*time.Millisecond); !errors.Is(err, ErrNotReady) {
		t.Fatalf("recién arrancada, imagen con ganchos y sin agente: %v", err)
	}
}
