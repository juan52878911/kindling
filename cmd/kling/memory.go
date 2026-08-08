package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
)

// cmdMemory activa o desactiva la memoria de uso de herramientas.
//
// Viene APAGADA. kindling no escribe en la memoria de nadie sin que se lo pidan,
// aunque el binario del puente se instale siempre para que activarla sea un
// comando y no un proyecto.
func cmdMemory(args []string) error {
	if len(args) == 0 {
		return memoryStatus(nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status", "estado":
		return memoryStatus(rest)
	case "enable", "on":
		return memoryEnable(rest)
	case "disable", "off":
		return memoryDisable(rest)
	case "install-service":
		return memoryInstallService(rest)
	default:
		return fmt.Errorf("uso: kling memory [status|enable|disable|install-service]")
	}
}

func memoryStatus(args []string) error {
	fs := flag.NewFlagSet("memory status", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := loadConfig()

	if !cfg.Memory.Enabled {
		fmt.Println("Memoria de uso: DESACTIVADA")
		fmt.Println()
		fmt.Println("Cuando se activa, kindling apunta qué herramienta resolvió cada petición")
		fmt.Println("y usa ese historial para ordenar mejor las búsquedas siguientes.")
		fmt.Println()
		fmt.Println("Actívala con:")
		fmt.Println("  kling memory enable                 usa engram, que viene con kling")
		fmt.Println("  kling memory enable -service <svc>  usa otro servicio ya enlazado")
		return nil
	}

	fmt.Printf("Memoria de uso: ACTIVA sobre %q\n", cfg.Memory.Service)

	ctx, stop := ctxWithSignals()
	defer stop()
	links, err := api.NewClient(cfg.Host(*host)).Links(ctx)
	if err != nil {
		fmt.Printf("  aviso: no alcanzo el daemon (%v)\n", err)
		return nil
	}
	for _, l := range links {
		if l.Service() == cfg.Memory.Service || l.Name == cfg.Memory.Service {
			fmt.Printf("  enlazado a %s · %d herramienta(s)\n", l.URL, len(l.Tools))
			return nil
		}
	}
	fmt.Printf("  ⚠ %q no está enlazado. Enlázalo:\n", cfg.Memory.Service)
	fmt.Printf("     kling mcp link %s <url>\n", cfg.Memory.Service)
	return nil
}

func memoryEnable(args []string) error {
	fs := flag.NewFlagSet("memory enable", flag.ExitOnError)
	host := hostFlag(fs)
	service := fs.String("service", "engram", "servicio MCP donde guardar el historial")
	listen := fs.String("listen", "0.0.0.0:9100", "dónde exponer el servidor local, si hay que envolverlo")
	cmdline := fs.String("cmd", "engram mcp --tools=agent", "cómo arrancar el servidor de memoria si habla stdio")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	c := api.NewClient(cfg.Host(*host))

	// ¿Ya está enlazado? Entonces solo hay que encender el interruptor.
	links, err := c.Links(ctx)
	if err != nil {
		return fmt.Errorf("no alcanzo el daemon: %w", err)
	}
	for _, l := range links {
		if l.Service() == *service || l.Name == *service {
			return finishEnable(cfg, *service, l.URL, len(l.Tools))
		}
	}

	// No lo está. Si el servidor habla stdio hay que exponerlo por HTTP, y para
	// eso sirve el mismo puente que usan las microVMs.
	fmt.Printf("%q no está enlazado todavía.\n\n", *service)

	bin := bridgePath()
	if bin == "" {
		return fmt.Errorf("no encuentro kling-bridge. Compílalo con:\n  make bridge-local")
	}
	name := strings.Fields(*cmdline)[0]
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("no encuentro %q en el PATH.\n"+
			"Instálalo, o usa otro servidor:  kling memory enable -service <svc> -cmd '<comando>'", name)
	}

	fmt.Printf("Para exponerlo por HTTP, deja esto corriendo en otra terminal:\n\n")
	fmt.Printf("  %s -listen %s -- %s\n\n", bin, *listen, *cmdline)
	if runtime.GOOS == "darwin" {
		fmt.Printf("O instálalo como servicio permanente:\n")
		fmt.Printf("  kling memory install-service\n\n")
	}
	ip := localIP()
	fmt.Printf("Y luego enlázalo y actívalo:\n")
	fmt.Printf("  kling mcp link %s http://%s:%s/mcp\n", *service, ip, portOf(*listen))
	fmt.Printf("  kling memory enable\n")
	return nil
}

func finishEnable(cfg *config.Config, service, url string, tools int) error {
	cfg.Memory.Enabled = true
	cfg.Memory.Service = service
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Memoria de uso ACTIVADA sobre %q (%s, %d herramienta(s)).\n\n", service, url, tools)
	fmt.Println("A partir de ahora el gateway apunta qué herramienta resolvió cada petición")
	fmt.Println("y pone delante lo que ya funcionó al buscar.")
	fmt.Println()
	fmt.Println("Reinicia el gateway para que lo tome:")
	fmt.Println("  sudo systemctl restart kling-gateway")
	return nil
}

func memoryDisable(args []string) error {
	fs := flag.NewFlagSet("memory disable", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := loadConfig()
	cfg.Memory.Enabled = false
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println("Memoria de uso desactivada. El servicio enlazado sigue disponible como")
	fmt.Println("herramienta normal; solo deja de escribirse el historial.")
	return nil
}

// bridgePath busca el puente allí donde lo deja la instalación.
func bridgePath() string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "kling-bridge"),
		filepath.Join(home, "go", "bin", "kling-bridge"),
		"/usr/local/bin/kling-bridge",
		"./kling-bridge-local",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("kling-bridge"); err == nil {
		return p
	}
	return ""
}

func portOf(listen string) string {
	if _, port, ok := strings.Cut(listen, ":"); ok {
		return port
	}
	return "9100"
}

// localIP devuelve una IP de la LAN con la que el gateway pueda alcanzarnos.
func localIP() string {
	out, err := exec.Command("sh", "-c",
		`ipconfig getifaddr en0 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}'`).Output()
	if ip := strings.TrimSpace(string(out)); err == nil && ip != "" {
		return ip
	}
	return "<tu-ip>"
}

// memoryInstallService deja el puente local como servicio permanente, para que
// el servidor de memoria no dependa de una terminal abierta.
func memoryInstallService(args []string) error {
	fs := flag.NewFlagSet("memory install-service", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:9100", "dónde escuchar")
	cmdline := fs.String("cmd", "engram mcp --tools=agent", "servidor MCP de stdio a envolver")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	bin := bridgePath()
	if bin == "" {
		return fmt.Errorf("no encuentro kling-bridge. Compílalo con:\n  make bridge-local && make install")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("por ahora solo macOS; en Linux usa un unit de systemd:\n"+
			"  ExecStart=%s -listen %s -- %s", bin, *listen, *cmdline)
	}

	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	plist := filepath.Join(dir, "com.kindling.bridge.plist")

	// launchd NO hereda el PATH de tu shell: un comando escrito por nombre falla
	// con "executable file not found in $PATH", igual que le pasaba al init de
	// las microVMs. Se resuelve aquí a ruta absoluta.
	parts := strings.Fields(*cmdline)
	if abs, err := exec.LookPath(parts[0]); err == nil {
		parts[0] = abs
	} else {
		return fmt.Errorf("no encuentro %q en el PATH: instálalo o dame la ruta completa con -cmd", parts[0])
	}

	var argsXML strings.Builder
	for _, a := range append([]string{bin, "-listen", *listen, "--"}, parts...) {
		fmt.Fprintf(&argsXML, "    <string>%s</string>\n", a)
	}
	// Agente de usuario, no demonio del sistema: envuelve un programa del usuario
	// y no tiene por qué correr con privilegios ni antes de que inicie sesión.
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.kindling.bridge</string>
  <key>ProgramArguments</key><array>
%s  </array>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>%s/.local/bin:%s/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    <key>HOME</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s/Library/Logs/kindling-bridge.log</string>
  <key>StandardErrorPath</key><string>%s/Library/Logs/kindling-bridge.log</string>
</dict></plist>
`, argsXML.String(), home, home, home, home, home)

	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", plist).Run()
	if out, err := exec.Command("launchctl", "load", plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %v: %s", err, out)
	}

	fmt.Printf("Servicio instalado: %s\n", plist)
	fmt.Printf("  expone: %s\n", *cmdline)
	fmt.Printf("  en:     http://%s:%s/mcp\n", localIP(), portOf(*listen))
	fmt.Printf("  log:    ~/Library/Logs/kindling-bridge.log\n\n")
	fmt.Printf("Arranca solo al iniciar sesión. Para quitarlo:\n")
	fmt.Printf("  launchctl unload %s && rm %s\n", plist, plist)
	return nil
}
