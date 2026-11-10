package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plistDePrueba es como lo escribe `kling mcp memory install-service`.
func plistDePrueba(home, programa string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.kindling.bridge</string>
  <key>ProgramArguments</key><array>
    <string>` + programa + `</string>
    <string>-listen</string>
    <string>127.0.0.1:9100</string>
    <string>--</string>
    <string>/opt/homebrew/bin/engram</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>HOME</key><string>` + home + `</string>
  </dict>
  <key>RunAtLoad</key><true/>
</dict></plist>
`
}

// escribirPlist deja el plist del servicio en un HOME temporal.
func plistPuenteDePrueba(t *testing.T, programa string) string {
	t.Helper()
	return escribirPlistEn(t, t.TempDir(), programa)
}

func TestProgramaDePlist(t *testing.T) {
	got, err := programaDePlist([]byte(plistDePrueba("/Users/x", "/Users/x/.local/bin/kling-bridge")))
	if err != nil || got != "/Users/x/.local/bin/kling-bridge" {
		t.Fatalf("%q %v", got, err)
	}
	got, _ = programaDePlist([]byte(`<plist><dict><key>Label</key><string>l</string><key>Program</key><string>/bin/p</string></dict></plist>`))
	if got != "/bin/p" {
		t.Fatalf("Program: %q", got)
	}
}

// El caso del Mac de Juan: el plist apunta a ~/.local/bin/kling-bridge, que
// ya no existe (plugin install mcp lo deja junto a kling-mcp). doctor falla y
// da la orden que lo arregla.
func TestDoctorServicioPuenteRoto(t *testing.T) {
	home := t.TempDir()
	escribirPlistEn(t, home, filepath.Join(home, ".local", "bin", "kling-bridge"))
	s := leerServicioPuente(home)
	if s == nil || s.existe {
		t.Fatalf("no vio el programa que falta: %+v", s)
	}
	c, ok := checkServicioPuente(s, true)
	if !ok || c.State != doctorFail || !strings.Contains(c.Detail, "does not exist") || c.Fix != "kling mcp memory install-service -repair" {
		t.Fatalf("check = %+v", c)
	}
	if c, _ := checkServicioPuente(s, false); !strings.Contains(c.Fix, "kling plugin install mcp") {
		t.Fatalf("sin mcp instalada: %+v", c)
	}
	// Y sale en doctorChecks.
	cs := doctorChecks(doctorInput{infoErr: os.ErrNotExist, completion: true, bridge: s})
	if c := findCheck(cs, "bridge service"); c == nil || c.State != doctorFail {
		t.Fatalf("doctorChecks: %+v", c)
	}
}

func TestDoctorServicioPuenteSano(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "kling-bridge")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := leerServicioPuente(plistPuenteDePrueba(t, bin))
	if c, ok := checkServicioPuente(s, true); !ok || c.State != doctorOK {
		t.Fatalf("%+v", c)
	}
	s.ultimaSalida = "78"
	if c, _ := checkServicioPuente(s, true); c.State != doctorFail {
		t.Fatalf("78 con el programa en su sitio: %+v", c)
	}
	s.ultimaSalida = "1"
	if c, _ := checkServicioPuente(s, true); c.State != doctorWarn {
		t.Fatalf("salida 1: %+v", c)
	}
	if _, ok := checkServicioPuente(leerServicioPuente(t.TempDir()), true); ok {
		t.Fatal("sin plist no hay línea")
	}
}

// plugin install mcp / upgrade -cli relanzan install-service -repair con el
// kling-mcp recién instalado, solo si el plist apunta a algo que no existe.
func TestRepararServicioPuente(t *testing.T) {
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "kling-mcp"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var llamadas [][]string
	run := func(bin string, args ...string) error {
		llamadas = append(llamadas, append([]string{bin}, args...))
		return nil
	}
	home := plistPuenteDePrueba(t, "/no/existe/kling-bridge")
	if hecho, err := repararServicioPuente(home, ext, run); !hecho || err != nil {
		t.Fatalf("no reparó: %v %v", hecho, err)
	}
	want := filepath.Join(ext, "kling-mcp") + " memory install-service -repair"
	if len(llamadas) != 1 || strings.Join(llamadas[0], " ") != want {
		t.Fatalf("llamadas = %v", llamadas)
	}
	// Uno sano no se toca.
	llamadas = nil
	if hecho, _ := repararServicioPuente(plistPuenteDePrueba(t, filepath.Join(ext, "kling-mcp")), ext, run); hecho || len(llamadas) > 0 {
		t.Fatal("tocó un servicio sano")
	}
	// Sin plist, nada.
	if hecho, _ := repararServicioPuente(t.TempDir(), ext, run); hecho {
		t.Fatal("reparó sin plist")
	}
	// Sin kling-mcp con que rehacerlo, nada.
	if hecho, _ := repararServicioPuente(home, t.TempDir(), run); hecho {
		t.Fatal("reparó sin kling-mcp")
	}
}

func escribirPlistEn(t *testing.T, home, programa string) string {
	t.Helper()
	p := plistPuente(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(plistDePrueba(home, programa)), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}
