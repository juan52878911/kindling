package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Las variables del sondeo remoto son datos: una raíz, un usuario o una unidad
// con comillas, `;` o `$(...)` no ejecutan nada en el host remoto y llegan al
// guion tal cual.
func TestRemoteProbeScriptEntrecomilla(t *testing.T) {
	dir := t.TempDir()
	testigo := filepath.Join(dir, "ejecutado")
	root := "/var/lib/k'; touch " + testigo + "; echo '"
	runAs := "kindling$(touch " + testigo + ")"
	units := []string{"a.service", "`touch " + testigo + "`"}

	// El guion real, con lo del final cambiado por algo que enseñe las variables.
	sc := remoteProbeScript(runAs, root, units)
	sc = sc[:len(sc)-len(remoteScript)] + `printf 'root=%s\nrunas=%s\nunits=%s\n' "$ROOT" "$RUNAS" "$UNITS"` + "\n"
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(sc)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	if _, err := os.Stat(testigo); err == nil {
		t.Fatalf("a value was executed by the shell:\n%s", sc)
	}
	want := "root=" + root + "\nrunas=" + runAs + "\nunits=" + strings.Join(units, " ") + "\n"
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}
