package main

// `kling up -install` en macOS: escribe el agente de launchd del daemon
// (dev.kindling.daemon) y lo arranca, lo que docs/mac.md enseñaba a hacer a
// mano. Un plist que ya existe y es distinto no se pisa: puede ser el que
// alguien ajustó, y con KeepAlive un error ahí deja el daemon caído.
//
// KLING_LAUNCH_AGENT_LABEL y KLING_LAUNCH_AGENT_PLIST cambian la etiqueta y
// la ruta del plist: para probarlo con un daemon privado sin tocar el agente
// de verdad (con un plist propio, el log va a su lado).

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/transport"
)

// agenteDaemon es el agente de launchd del daemon.
type agenteDaemon struct {
	label, plist, log string
}

// dominio es el de launchctl para este usuario ("gui/501").
func (a agenteDaemon) dominio() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// etiquetaValida es una etiqueta de launchd sin nada que haya que escapar.
var etiquetaValida = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// agenteDaemonMac es el agente que usan `kling up` y `kling up -install`.
func agenteDaemonMac() (agenteDaemon, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agenteDaemon{}, err
	}
	a := agenteDaemon{
		label: config.Or(os.Getenv("KLING_LAUNCH_AGENT_LABEL"), launchAgentLabel),
		log:   filepath.Join(home, "Library", "Logs", "kindling.log"),
	}
	if !etiquetaValida.MatchString(a.label) {
		return agenteDaemon{}, fmt.Errorf("KLING_LAUNCH_AGENT_LABEL %q is not a launchd label (letters, digits, . _ -)", a.label)
	}
	a.plist = filepath.Join(home, "Library", "LaunchAgents", a.label+".plist")
	if p := os.Getenv("KLING_LAUNCH_AGENT_PLIST"); p != "" {
		if !filepath.IsAbs(p) {
			return agenteDaemon{}, fmt.Errorf("KLING_LAUNCH_AGENT_PLIST must be an absolute path, not %q", p)
		}
		a.plist = p
		a.log = filepath.Join(filepath.Dir(p), a.label+".log")
	}
	return a, nil
}

// entornoDelAgente son las variables del daemon que el plist tiene que fijar
// (launchd no hereda las de la shell): solo las que cambian dónde vive o con
// qué VMM arranca, y solo si quien instala las tiene puestas.
func entornoDelAgente(root string) map[string]string {
	env := map[string]string{}
	if root != "" && root != transport.DefaultRoot() {
		env["KLING_ROOT"] = root
	}
	for _, k := range []string{"KLING_SOCKET", "KLING_VMM"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

func xmlTexto(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// plistDaemon es el plist del agente: el de docs/mac.md con las rutas de
// verdad (launchd no expande ~ ni $HOME) y el entorno de entornoDelAgente.
func plistDaemon(a agenteDaemon, exe string, env map[string]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlTexto(a.label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlTexto(exe) + `</string>
    <string>daemon</string>
  </array>
`)
	if len(env) > 0 {
		claves := make([]string, 0, len(env))
		for k := range env {
			claves = append(claves, k)
		}
		sort.Strings(claves)
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range claves {
			b.WriteString("    <key>" + xmlTexto(k) + "</key>\n    <string>" + xmlTexto(env[k]) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString(`  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <!-- SIGTERM al pararlo: el daemon escribe su estado y sale. -->
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>` + xmlTexto(a.log) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlTexto(a.log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// errPlistDistinto es que ya hay un plist con otro contenido.
var errPlistDistinto = errors.New("launchd agent already exists with different content")

// escribirPlist deja body en a.plist. Igual que el que hay: nada (instalado
// = false). Distinto: errPlistDistinto, sin tocarlo.
func escribirPlist(a agenteDaemon, body string) (instalado bool, err error) {
	if old, err := os.ReadFile(a.plist); err == nil {
		if string(old) == body {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s", errPlistDistinto, a.plist)
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(a.plist), 0o755); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(a.log), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.plist), "."+filepath.Base(a.plist)+".tmp-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	// Link y no Rename: si otro lo creó entre tanto, no se pisa.
	if err := os.Link(tmp.Name(), a.plist); err != nil {
		if os.IsExist(err) {
			return false, fmt.Errorf("%w: %s", errPlistDistinto, a.plist)
		}
		return false, err
	}
	return true, nil
}

// instalarAgente escribe el plist del daemon (exe daemon) para root.
func instalarAgente(a agenteDaemon, root string) error {
	exe, err := ejecutableReal()
	if err != nil {
		return err
	}
	nuevo, err := escribirPlist(a, plistDaemon(a, exe, entornoDelAgente(root)))
	switch {
	case errors.Is(err, errPlistDistinto):
		fmt.Printf("agent:       ✗ %s already exists and differs from what -install would write.\n", a.plist)
		fmt.Println("  To replace it:")
		fmt.Printf("    launchctl bootout %s/%s\n", a.dominio(), a.label)
		fmt.Printf("    rm %q\n", a.plist)
		fmt.Println("    kling up -install")
		return err
	case err != nil:
		return fmt.Errorf("writing %s: %w", a.plist, err)
	case nuevo:
		fmt.Printf("agent:       ✓ wrote %s (log: %s)\n", a.plist, a.log)
	default:
		fmt.Printf("agent:       ✓ %s already installed\n", a.plist)
	}
	return nil
}
