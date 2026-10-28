package guest

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeIP deja en un PATH propio un `ip` de mentira que apunta sus argumentos y
// sale con el código pedido escribiendo msg en stderr.
func fakeIP(t *testing.T, code int, msg string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\necho '" + msg + "' >&2\nexit " +
		strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

func capturarLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// La ruta se pone a través del cosechador y un `ip` que va bien se reporta
// como bien (antes, siendo PID 1, el cosechador se llevaba el estado y salía
// "no child processes").
func TestSetupMMDSRoutePoneLaRuta(t *testing.T) {
	args := fakeIP(t, 0, "")
	buf := capturarLog(t)
	SetupMMDSRoute()

	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatalf("no se ejecutó ip: %v", err)
	}
	if strings.TrimSpace(string(got)) != "route add 169.254.169.254/32 dev eth0" {
		t.Errorf("argumentos = %q", got)
	}
	if !strings.Contains(buf.String(), "route to 169.254.169.254 via eth0 ready") {
		t.Errorf("log = %q, want la ruta lista", buf.String())
	}
}

// "File exists" no es un fallo: la ruta ya estaba.
func TestSetupMMDSRouteYaExistente(t *testing.T) {
	fakeIP(t, 2, "RTNETLINK answers: File exists")
	buf := capturarLog(t)
	SetupMMDSRoute()
	if buf.Len() != 0 {
		t.Errorf("registró algo con la ruta ya puesta: %q", buf.String())
	}
}

// Cualquier otro fallo se registra con la salida de ip.
func TestSetupMMDSRouteFalloSeRegistra(t *testing.T) {
	fakeIP(t, 1, "Cannot find device eth0")
	buf := capturarLog(t)
	SetupMMDSRoute()
	if !strings.Contains(buf.String(), "could not set the route") || !strings.Contains(buf.String(), "Cannot find device") {
		t.Errorf("log = %q", buf.String())
	}
}
