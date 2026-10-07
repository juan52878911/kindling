package main

import (
	"strings"
	"testing"
)

// Los alias que se van a retirar avisan; los permanentes y los nombres de
// ahora, no. El aviso dice el nombre nuevo sin repetir los argumentos.
func TestAliasWarning(t *testing.T) {
	hermetic(t)
	for in, want := range map[string]string{
		"add x":       "warning: kling add is now kling mcp add",
		"rmi x":       "warning: kling rmi is now kling template rm",
		"info -json":  "warning: kling info is now kling status -v",
		"models ls":   "warning: kling models is now kling ai model",
		"commit a b":  "",
		"snapshots":   "",
		"plugins ls":  "",
		"ps -a":       "",
		"template rm": "",
	} {
		f := strings.Fields(in)
		if got := aliasWarning(f[0], f[1:]); got != want {
			t.Errorf("aliasWarning(%q) = %q, want %q", in, got, want)
		}
	}
}
