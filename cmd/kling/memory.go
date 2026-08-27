package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

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
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
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
	listen := fs.String("listen", "127.0.0.1:9100", "where to expose the local server, if it needs wrapping")
	cmdline := fs.String("cmd", "engram mcp --tools=agent", "how to start the memory server if it speaks stdio")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
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
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
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

var reExitCode = regexp.MustCompile(`last exit code = (\d+)`)

// waitForStart confirma con launchd que el job existe y no murio al nacer.
//
// `launchctl load` devuelve 0 aunque el job no pueda arrancar nunca: una ruta que
// no existe da EX_CONFIG (78) y una que launchd no puede ejecutar da 126
// (Operation not permitted — pasa bajo ~/Documents, ~/Desktop o en un volumen
// externo, por TCC). En los dos casos stderr queda vacio y el log ni se crea, asi
// que sin esto el comando decia "Service installed" y salia 0.
//
// Se espera con margen porque `load` es asincrono: launchd acepta el plist y
// spawnea despues.
func waitForStart(bin string) error {
	label := "gui/" + strconv.Itoa(os.Getuid()) + "/com.kindling.bridge"
	last := "the service never appeared"
	for i := 0; i < 20; i++ {
		time.Sleep(150 * time.Millisecond)
		out, err := exec.Command("launchctl", "print", label).CombinedOutput()
		if err != nil {
			last = "launchd does not know the service"
			continue
		}
		txt := string(out)
		if strings.Contains(txt, "state = running") {
			return nil
		}
		m := reExitCode.FindStringSubmatch(txt)
		if m == nil {
			last = "the service has not started yet"
			continue
		}
		switch m[1] {
		case "0":
			return nil
		case "78":
			return fmt.Errorf("launchd cannot find %s (code 78).\n"+
				"launchd's cwd is \"/\": the path must be absolute and exist.", bin)
		case "126":
			return fmt.Errorf("launchd is not allowed to run %s (code 126).\n"+
				"This happens under ~/Documents, ~/Desktop or on external volumes.\n"+
				"Install it where it can: make install (puts it in ~/.local/bin).", bin)
		default:
			return fmt.Errorf("the service started and died (code %s).\n"+
				"Check ~/Library/Logs/kindling-bridge.log and `launchctl print %s`.", m[1], label)
		}
	}
	return fmt.Errorf("could not confirm the service started: %s.\n"+
		"Check with: launchctl print %s", last, label)
}

// warnIfExposed advierte cuando el puente deja de ser local.
//
// El default es loopback a proposito: lo que se envuelve suele ser la memoria
// personal del usuario, y el puente NO autentica — ni lo puede hacer de forma util,
// porque `kling mcp link` no manda cabeceras. Exponerlo a la LAN es legitimo cuando
// el gateway corre en otra maquina, pero tiene que ser una decision, no un default.
// `/reset` tambien queda accesible para cualquiera que alcance el puerto.
func warnIfExposed(listen string) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		fmt.Printf("WARNING: listening on %s — reachable from the whole network, with NO authentication.\n", listen)
		fmt.Printf("         Anyone who can reach the port can read the memory and call /reset.\n")
		fmt.Printf("         Firewall it, or use -listen 127.0.0.1:PORT if the gateway runs on this machine.\n\n")
		return
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		fmt.Printf("WARNING: %s is not loopback and the bridge does not authenticate. Restrict access.\n\n", listen)
	}
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
			// Absoluta SIEMPRE. El ultimo candidato es relativo —es donde lo deja
			// `make bridge-local`— y devolverlo tal cual mete "./kling-bridge-local"
			// en el ProgramArguments de un LaunchAgent. El cwd de launchd es "/", asi
			// que se resuelve a "/kling-bridge-local" y el job muere con EX_CONFIG
			// (78) sin escribir nada en el log.
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
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
	listen := fs.String("listen", "127.0.0.1:9100", "where to listen")
	cmdline := fs.String("cmd", "engram mcp --tools=agent", "stdio MCP server to wrap")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
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
	if err := waitForStart(bin); err != nil {
		return err
	}
	warnIfExposed(*listen)

	fmt.Printf("Service installed: %s\n", plist)
	fmt.Printf("  exposes: %s\n", *cmdline)
	fmt.Printf("  at:      http://%s:%s/mcp\n", localIP(), portOf(*listen))
	fmt.Printf("  log:     ~/Library/Logs/kindling-bridge.log\n\n")
	fmt.Printf("Starts automatically at login. To remove it:\n")
	fmt.Printf("  launchctl unload %s && rm %s\n", plist, plist)
	return nil
}
