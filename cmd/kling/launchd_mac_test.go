package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// El plist lleva lo de docs/mac.md (RunAtLoad, KeepAlive, ExitTimeOut 30,
// logs) con rutas absolutas y escapadas, y plutil lo acepta.
func TestPlistDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KLING_LAUNCH_AGENT_LABEL", "dev.kindling.prueba")
	t.Setenv("KLING_LAUNCH_AGENT_PLIST", filepath.Join(dir, "a.plist"))
	a, err := agenteDaemonMac()
	if err != nil {
		t.Fatal(err)
	}
	if a.label != "dev.kindling.prueba" || a.log != filepath.Join(dir, "dev.kindling.prueba.log") {
		t.Fatalf("agente = %+v", a)
	}
	t.Setenv("KLING_SOCKET", "/r & s/kling.sock")
	body := plistDaemon(a, "/u/<bin>/kling", entornoDelAgente("/r & s"))
	for _, w := range []string{
		"<string>dev.kindling.prueba</string>", "<string>/u/&lt;bin&gt;/kling</string>", "<string>daemon</string>",
		"<key>RunAtLoad</key>\n  <true/>", "<key>KeepAlive</key>\n  <true/>", "<key>ExitTimeOut</key>\n  <integer>30</integer>",
		"<key>KLING_ROOT</key>\n    <string>/r &amp; s</string>", "<key>KLING_SOCKET</key>",
		"<key>StandardErrorPath</key>\n  <string>" + a.log + "</string>",
	} {
		if !strings.Contains(body, w) {
			t.Errorf("falta %q en\n%s", w, body)
		}
	}
	if runtime.GOOS == "darwin" {
		p := filepath.Join(dir, "lint.plist")
		os.WriteFile(p, []byte(body), 0o644)
		if out, err := exec.Command("plutil", "-lint", p).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v: %s", err, out)
		}
	}
	// La raíz por defecto no se fija: el daemon ya la usa.
	t.Setenv("KLING_SOCKET", "")
	if env := entornoDelAgente(""); len(env) != 0 {
		t.Fatalf("sin nada que fijar: %v", env)
	}
	t.Setenv("KLING_LAUNCH_AGENT_LABEL", "mal/etiqueta")
	if _, err := agenteDaemonMac(); err == nil {
		t.Fatal("una etiqueta con / no vale")
	}
	t.Setenv("KLING_LAUNCH_AGENT_LABEL", "")
	t.Setenv("KLING_LAUNCH_AGENT_PLIST", "relativo.plist")
	if _, err := agenteDaemonMac(); err == nil {
		t.Fatal("una ruta relativa no vale")
	}
}

// Escribir: nuevo se crea; igual no hace nada; distinto no se pisa.
func TestEscribirPlist(t *testing.T) {
	dir := t.TempDir()
	a := agenteDaemon{label: "x", plist: filepath.Join(dir, "LaunchAgents", "x.plist"), log: filepath.Join(dir, "Logs", "x.log")}
	if nuevo, err := escribirPlist(a, "uno"); err != nil || !nuevo {
		t.Fatalf("nuevo = %v, %v", nuevo, err)
	}
	if st, _ := os.Stat(a.plist); st.Mode().Perm() != 0o644 {
		t.Fatalf("modo %v", st.Mode())
	}
	if nuevo, err := escribirPlist(a, "uno"); err != nil || nuevo {
		t.Fatalf("igual = %v, %v", nuevo, err)
	}
	if _, err := escribirPlist(a, "dos"); !errors.Is(err, errPlistDistinto) {
		t.Fatalf("distinto = %v", err)
	}
	if b, _ := os.ReadFile(a.plist); string(b) != "uno" {
		t.Fatalf("se pisó: %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(a.plist)); len(ents) != 1 {
		t.Fatalf("quedan temporales: %v", ents)
	}
}
