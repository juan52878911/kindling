package main

import (
	"os"
	"sync"
	"testing"
)

// TestMain aparta la caché de manifiestos de las extensiones del usuario para
// todo el paquete, también en los tests que no son herméticos y en los kling
// que lanzan: ninguno escribe en su ~/.local/state.
func TestMain(m *testing.M) {
	// El binario de test hace también de kling (correrKling): así se prueba
	// el main de verdad, con su os.Exit, sin compilar otro binario.
	if os.Getenv(envComoKling) == "1" {
		os.Unsetenv(envComoKling)
		main()
		os.Exit(0)
	}
	d, err := os.MkdirTemp("", "kling-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", d)
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

// hermetic aísla un test del Mac de quien lo corre: sin las extensiones que
// tenga instaladas (~/.local/share/kling/plugins, el PATH) ni su caché de
// manifiestos, sin su configuración, y con el registro de extensiones sin
// descubrir. Dos tests fallaban aquí porque encontraban una kling-mcp real;
// ahora solo ven lo que ellos ponen en KLING_PLUGIN_PATH.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("KLING_CONFIG", t.TempDir()+"/config.json")
	extOnce, extReg = sync.Once{}, nil
	salidaJSON.Store(false)
	t.Cleanup(func() { extOnce, extReg = sync.Once{}, nil; salidaJSON.Store(false) })
}
