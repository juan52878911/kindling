package main

import (
	"sync"
	"testing"
)

// hermetic aísla un test del Mac de quien lo corre: sin las extensiones que
// tenga instaladas (~/.local/share/kling/plugins, el PATH), sin su
// configuración, y con el registro de extensiones sin descubrir. Dos tests
// fallaban aquí porque encontraban una kling-mcp real; ahora solo ven lo que
// ellos ponen en KLING_PLUGIN_PATH.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("KLING_CONFIG", t.TempDir()+"/config.json")
	extOnce, extReg = sync.Once{}, nil
	t.Cleanup(func() { extOnce, extReg = sync.Once{}, nil })
}
