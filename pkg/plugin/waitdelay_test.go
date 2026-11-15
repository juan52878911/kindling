package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Una extensión que imprime su manifiesto y sale bien, pero cuya salida sigue
// abierta en otro proceso (aquí un nieto en segundo plano; en
// TestCacheConcurrente, el hijo de otra goroutine entre fork y exec), no es un
// fallo: el manifiesto está entero. Antes daba "WaitDelay expired before I/O
// complete".
func TestManifiestoConLaSalidaRetenidaPorOtroProceso(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "kling-bg")
	body := "#!/bin/sh\n" +
		`echo '{"manifest_version":2,"name":"bg","version":"1","commands":[{"name":"bg"}]}'` + "\n" +
		"sleep 3 &\nexit 0\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	viejo := ManifestTimeout
	ManifestTimeout = 2 * time.Second
	t.Cleanup(func() { ManifestTimeout = viejo })

	inicio := time.Now()
	m, err := loadManifest(context.Background(), p)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.Name != "bg" || time.Since(inicio) > time.Second {
		t.Fatalf("manifiesto %+v en %s", m, time.Since(inicio))
	}
}
