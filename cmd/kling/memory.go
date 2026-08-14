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
		return fmt.Errorf("usage: kling memory [status|enable|disable|install-service]")
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
		fmt.Println("Usage memory: DISABLED")
		fmt.Println()
		fmt.Println("When enabled, kindling notes which tool resolved each request")
		fmt.Println("and uses that history to rank later searches better.")
		fmt.Println()
		fmt.Println("Enable it with:")
		fmt.Println("  kling memory enable                 uses engram, which ships with kling")
		fmt.Println("  kling memory enable -service <svc>  uses another already-linked service")
		return nil
	}

	fmt.Printf("Usage memory: ACTIVE on %q\n", cfg.Memory.Service)

	ctx, stop := ctxWithSignals()
	defer stop()
	links, err := api.NewClient(cfg.Host(*host)).Links(ctx)
	if err != nil {
		fmt.Printf("  warning: can't reach the daemon (%v)\n", err)
		return nil
	}
	for _, l := range links {
		if l.Service() == cfg.Memory.Service || l.Name == cfg.Memory.Service {
			fmt.Printf("  linked to %s · %d tool(s)\n", l.URL, len(l.Tools))
			return nil
		}
	}
	fmt.Printf("  ⚠ %q is not linked. Link it:\n", cfg.Memory.Service)
	fmt.Printf("     kling mcp link %s <url>\n", cfg.Memory.Service)
	return nil
}

func memoryEnable(args []string) error {
	fs := flag.NewFlagSet("memory enable", flag.ExitOnError)
	host := hostFlag(fs)
	service := fs.String("service", "engram", "MCP service where the history is stored")
	listen := fs.String("listen", "0.0.0.0:9100", "where to expose the local server, if it needs wrapping")
	cmdline := fs.String("cmd", "engram mcp --tools=agent", "how to start the memory server if it speaks stdio")
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
		return fmt.Errorf("can't reach the daemon: %w", err)
	}
	for _, l := range links {
		if l.Service() == *service || l.Name == *service {
			return finishEnable(cfg, *service, l.URL, len(l.Tools))
		}
	}

	// No lo está. Si el servidor habla stdio hay que exponerlo por HTTP, y para
	// eso sirve el mismo puente que usan las microVMs.
	fmt.Printf("%q is not linked yet.\n\n", *service)

	bin := bridgePath()
	if bin == "" {
		return fmt.Errorf("can't find kling-bridge. Build it with:\n  make bridge-local")
	}
	name := strings.Fields(*cmdline)[0]
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("can't find %q in PATH.\n"+
			"Install it, or use another server:  kling memory enable -service <svc> -cmd '<command>'", name)
	}

	fmt.Printf("To expose it over HTTP, leave this running in another terminal:\n\n")
	fmt.Printf("  %s -listen %s -- %s\n\n", bin, *listen, *cmdline)
	if runtime.GOOS == "darwin" {
		fmt.Printf("Or install it as a permanent service:\n")
		fmt.Printf("  kling memory install-service\n\n")
	}
	ip := localIP()
	fmt.Printf("Then link it and enable it:\n")
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
	fmt.Printf("Usage memory ENABLED on %q (%s, %d tool(s)).\n\n", service, url, tools)
	fmt.Println("From now on the gateway notes which tool resolved each request")
	fmt.Println("and puts what already worked at the front of searches.")
	fmt.Println()
	fmt.Println("Restart the gateway to pick it up:")
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
	fmt.Println("Usage memory disabled. The linked service is still available as a")
	fmt.Println("regular tool; only the history stops being written.")
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
	return "<your-ip>"
}

// memoryInstallService deja el puente local como servicio permanente, para que
// el servidor de memoria no dependa de una terminal abierta.
func memoryInstallService(args []string) error {
	fs := flag.NewFlagSet("memory install-service", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:9100", "where to listen")
	cmdline := fs.String("cmd", "engram mcp --tools=agent", "stdio MCP server to wrap")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	bin := bridgePath()
	if bin == "" {
		return fmt.Errorf("can't find kling-bridge. Build it with:\n  make bridge-local && make install")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS only for now; on Linux use a systemd unit:\n"+
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
		return fmt.Errorf("can't find %q in PATH: install it or provide the full path with -cmd", parts[0])
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

	fmt.Printf("Service installed: %s\n", plist)
	fmt.Printf("  exposes: %s\n", *cmdline)
	fmt.Printf("  at:      http://%s:%s/mcp\n", localIP(), portOf(*listen))
	fmt.Printf("  log:     ~/Library/Logs/kindling-bridge.log\n\n")
	fmt.Printf("Starts automatically at login. To remove it:\n")
	fmt.Printf("  launchctl unload %s && rm %s\n", plist, plist)
	return nil
}
