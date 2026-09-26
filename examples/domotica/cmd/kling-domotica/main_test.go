package main

import (
	"strings"
	"testing"
)

// El manifiesto tiene que validar: si no, kling lo lista con su error y no le
// pasa ningún comando.
func TestManifestValid(t *testing.T) {
	m := manifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.Command("domotica") == nil {
		t.Fatal("the manifest must declare the domotica command")
	}
}

// Cada subcomando del completado lo despacha cmdDomotica (y viceversa, lo
// comprueba el texto de ayuda): un subcomando que se completa pero no existe
// solo daría "unknown domotica command".
func TestSubcommandsDispatched(t *testing.T) {
	for _, s := range domoticaSubcommands {
		if !strings.Contains(domoticaUsage, "  "+s+" ") {
			t.Errorf("%s is completed but not in the usage", s)
		}
		if !strings.Contains(domoticaHelp, "domotica "+s+" ") {
			t.Errorf("%s is completed but not in kling help", s)
		}
	}
	if err := cmdDomotica([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "unknown domotica command") {
		t.Fatalf("unknown subcommand: %v", err)
	}
}
