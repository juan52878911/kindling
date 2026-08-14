// kling gestiona microVMs de Firecracker con una interfaz al estilo de docker.
//
// El mismo binario hace de CLI y de daemon: `kling daemon` arranca el núcleo
// donde esté KVM, y el CLI le habla por un socket Unix, local o a través de SSH.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
	"github.com/juan52878911/kindling/internal/daemon"
	"github.com/juan52878911/kindling/internal/gateway"
	"github.com/juan52878911/kindling/internal/report"
	"github.com/juan52878911/kindling/internal/transport"
)

const usage = `kling - Firecracker microVMs with a docker-style interface

USAGE
  kling <command> [options]

GETTING STARTED
  up                                               gets the runtime ready: KVM,
                                                   nftables, user, images,
                                                   daemon and gateway
  status [-json]                                   which piece is up and which is missing

VOLUMES
  volume create <name> [-size 2G]                  storage that survives
                                                   the microVM
  volume ls [-json] | rm <name>                    list / remove
  volume populate <name> [-image I] -- <cmd>       installs packages inside a microVM
  images refresh [image...]                        puts the current bridge inside the images
  images toolchain                                 builds the image with npm and pip (used by populate)
  images recipe <image>                            how it was built

MACHINES
  run [-name N] [-image I] [-cpus N] [-mem MiB]    creates and starts a microVM
      [-egress none|internet|allowlist]            network egress (default: none)
      [-allow dom1,dom2]                           domains allowed with allowlist
      [-ttl SECONDS] [-cpu-pct PCT]                auto-freeze and CPU ceiling
      [-service NAME] [-label k=v]                 grouping by MCP service
      [-volume NAME[:/mount][:ro]] (repeatable)    storage that survives the machine
  ps [-a] [-q] [-json]                             lists the machines
  logs <ref> [-tail N]                             microVM serial console
  freeze <ref>                                     freezes into a snapshot -> warm
  thaw <ref>                                       restores from snapshot (~ms)
  stop <ref>                                       terminates the machine
  rm <ref>                                         removes machine and snapshot
  mmds <ref> [-f store.json]                       injects a session secret via
                                                   MMDS (reads stdin if no -f); the
                                                   machine can no longer be frozen

CATALOG
  search <query>                                   searches the official
                                                   MCP server registry
  add <server> [-as name] [-arg value]             packages it, imports it and
      [-volume NAME[:/mount][:ro]] (repeatable)    leaves it frozen as a service

MCP SERVICES
  mcp import <service> -image <img>                turns an MCP server into a
      [-cpus N] [-mem MiB]                         service: it starts, asks
      [-egress none|internet|allowlist]            what it can do, freezes it and
      [-allow dom1,dom2]                           saves its catalog. All of this
      [-volume NAME[:/mount][:ro]] (repeatable)    ends up BAKED into the snapshot
  mcp list [-v] [-json]                            services and their tools
  mcp refresh <service>                            recaptures the catalog
  mcp link <name> <url>                            links an EXTERNAL MCP server
                                                   (e.g. your engram) without putting
                                                   it in a microVM
  mcp unlink <name>                                unlinks it

GOLDEN SNAPSHOTS
  commit <ref> <name>                              freezes a machine as a
                                                   reusable snapshot
  run -from <name>                                 instantiates from the snapshot
  snapshots                                        lists the snapshots
  rmi <name>                                       removes a snapshot

OBSERVATION
  topo                                             ASCII diagram of everything
  top [-watch DUR] [-json]                         memory per microVM (PSS) and
                                                   the host; snapshot or refresh
  export [-o file.html]                            browsable topology in HTML
  events                                           stream of daemon events
  info [-json]                                     daemon status

USAGE MEMORY (optional, off by default)
  memory status                                    whether it's active and on what
  memory enable [-service N]                       enables it; uses engram by default
  memory disable                                   disables it
  memory install-service                           installs the local bridge as a
                                                   permanent service (macOS)

CONNECT YOUR AGENT
  connect                                          step-by-step guide
  connect -all                                     ONE entry for all
                                                   services: inventory at
                                                   handshake, schemas on demand
  connect -all -only eco,files                     only those services
  connect -all -expand                             full catalog (uses more
                                                   context)
  connect <service>                                a single service
  connect ... -install all                         writes to ALL detected
                                                   agents: Claude Code,
                                                   opencode, Cursor, VS Code,
                                                   Windsurf, Cline and Zed
  connect ... -install <client>                    just that one
  connect ... -token T                             uses that token instead of
                                                   gateway.token
  migrate <mcp> -install <client>                  moves an existing MCP to
                                                   kindling WITHOUT rewriting the
                                                   skills that use it (keeps its
                                                   name and tools)

GATEWAY
  gateway [-listen ADDR] [-idle DUR] [-ephemeral]  routes MCP calls to microVMs
                                                   on demand. With -ephemeral,
                                                   each action runs in its own
                                                   machine, which dies when it ends
          [-prewarm N]                             ready instances per service
                                                   (only -ephemeral)
          [-keepwarm N]                            N popular services with their
                                                   primary warm (persistent;
                                                   avoids cold start on Mac)
          [-memory SVC]                            agent memory service
          [-no-auth] [-pprof]                      no token / with profiling; both
                                                   require listening on
                                                   loopback. Defaults to requiring
                                                   Authorization: Bearer with the
                                                   gateway.token token, which is
                                                   generated only the first time

DAEMON
  daemon [-socket S] [-root R] [-firecracker BIN]  starts the core

CONFIGURATION
  context [ls]                                     lists known daemons
  context add <name> <host>                        adds one and activates it
  context use <name>                               switches daemon
  context rm <name>                                removes it
  config [show|path]                               current configuration
  config set <key> <value>                         e.g. defaults.image min
  version                                          CLI version

CONNECTION
  Precedence:  -H  >  $KLING_HOST  >  active context  >  local socket

    kling context add lab ssh://juan@192.168.2.60
    kling context use lab

  The daemon never listens on a network port: controlling microVMs is
  equivalent to root on its host, so the only remote access is SSH.
`

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "daemon":
		err = cmdDaemon(args)
	case "gateway":
		err = cmdGateway(args)
	case "add":
		err = cmdAdd(args)
	case "search":
		err = cmdSearch(args)
	case "up":
		err = cmdUp(args)
	case "status":
		err = cmdStatus(args)
	case "volume", "volumes":
		err = cmdVolume(args)
	case "connect":
		err = cmdConnect(args)
	case "migrate":
		err = cmdMigrate(args)
	case "mcp":
		err = cmdMCP(args)
	case "memory":
		err = cmdMemory(args)
	case "dial-stdio": // extremo remoto del transporte SSH, no para uso manual
		err = transport.ServeStdio(envOr("KLING_SOCKET", transport.DefaultSocket), os.Stdin, os.Stdout)
	case "run":
		err = cmdRun(args)
	case "ps":
		err = cmdPS(args)
	case "logs":
		err = cmdLogs(args)
	case "freeze", "thaw", "stop", "rm":
		err = cmdLifecycle(cmd, args)
	case "squeeze":
		err = cmdSqueeze(args)
	case "mmds":
		err = cmdMMDS(args)
	case "commit":
		err = cmdCommit(args)
	case "snapshots":
		err = cmdSnapshots(args)
	case "images":
		err = cmdImages(args)
	case "rmi":
		err = cmdRmi(args)
	case "topo":
		err = cmdTopo(args)
	case "top":
		err = cmdTop(args)
	case "export":
		err = cmdExport(args)
	case "events":
		err = cmdEvents(args)
	case "info":
		err = cmdInfo(args)
	case "context":
		err = cmdContext(args)
	case "config":
		err = cmdConfig(args)
	case "version", "--version", "-v":
		fmt.Printf("kling %s\n", Version)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// hostFlag registra -H en cualquier subcomando del cliente.
//
// Por defecto vacío a propósito: así hostOf() distingue "no me lo han dicho" de
// "me han dicho esto", y puede aplicar la precedencia
// -H > $KLING_HOST > contexto activo > socket local.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("H", "", "daemon endpoint (socket or ssh://user@host)")
}

// loadConfig lee la configuración sin hacer fallar al CLI si está corrupta: una
// configuración ilegible no debe impedir listar máquinas.
func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (falling back to defaults)\n", err)
		return &config.Config{}
	}
	return cfg
}

// resolveCPUPct devuelve el techo de CPU eligiendo entre el flag nuevo -cpu-pct y
// el alias deprecado -cpu. -cpu se mantiene por compatibilidad pero avisa: colisiona
// visualmente con -cpus (número de vCPUs), a un solo carácter y en el mismo comando.
func resolveCPUPct(fs *flag.FlagSet, pct, old int) int {
	usedOld := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cpu" {
			usedOld = true
		}
	})
	if usedOld {
		fmt.Fprintln(os.Stderr, "warning: -cpu is deprecated; use -cpu-pct (it looks like -cpus, which sets vCPU count)")
		if pct == 0 {
			return old
		}
	}
	return pct
}

// hostOf resuelve a qué daemon hablar.
func hostOf(flagValue string) string { return loadConfig().Host(flagValue) }

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ── daemon ────────────────────────────────────────────────────────────────────

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", envOr("KLING_SOCKET", transport.DefaultSocket), "Unix socket to serve")
	root := fs.String("root", envOr("KLING_ROOT", "/var/lib/kindling"), "data directory")
	fcBin := fs.String("firecracker", envOr("KLING_FIRECRACKER", "firecracker"), "firecracker binary")
	sockUser := fs.String("socket-user", os.Getenv("KLING_SOCKET_USER"), "user to hand the socket to (for the CLI over SSH)")
	runAs := fs.String("run-as", envOr("KLING_RUN_AS", "kindling"), "unprivileged user Firecracker runs as")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	srv, err := daemon.New(*socket, *root, *fcBin, *sockUser, *runAs)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	return srv.Listen(ctx)
}

func cmdGateway(args []string) error {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	host := hostFlag(fs)
	listen := fs.String("listen", "", "where to listen (default: gateway.listen, or 127.0.0.1:8080)")
	idle := fs.Duration("idle", 0, "time without requests before freezing (default: gateway.idle, or 5m)")
	ephemeral := fs.Bool("ephemeral", false, "one microVM per action, destroyed when it's done (maximum isolation, stateless)")
	prewarm := fs.Int("prewarm", 1, "pre-warmed instances per service (0 = disabled; -ephemeral only)")
	keepwarm := fs.Int("keepwarm", 0, "N popular services with their primary warm in persistent mode (0 = disabled; avoids cold start, useful on Mac)")
	memory := fs.String("memory", "", "MCP service that remembers which tool resolved each request")
	pprofOn := fs.Bool("pprof", false, "exposes /debug/pprof; temporary diagnostics only, loopback only")
	noAuth := fs.Bool("no-auth", false, "no token; development only, and only when listening on loopback")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	addr := config.Or(*listen, cfg.Gateway.Listen, "127.0.0.1:8080")

	// Los argumentos se validan ANTES de hablar con nadie: un flag mal puesto
	// debe fallar al instante, no después de esperar a un daemon que quizá ni
	// esté. El flag registra los perfiles, pero no los protege — si el gateway
	// escucha fuera de loopback, activarlos regala volcados de goroutines y la
	// línea de comandos a quien alcance el puerto, y deja que quien llame elija
	// cuántos segundos de CPU consume /debug/pprof/profile.
	if *pprofOn && !gateway.IsLoopback(addr) {
		return fmt.Errorf("-pprof requires listening on loopback, and %q is not.\n"+
			"Diagnose over a tunnel:  ssh -L 8080:127.0.0.1:8080 <host>", addr)
	}

	// El token se resuelve aquí, con el resto de la validación de argumentos y
	// antes de hablar con nadie: -no-auth mal puesto debe fallar al instante, no
	// después de esperar a un daemon que quizá ni esté.
	token, err := resolveGatewayToken(cfg, *noAuth, addr)
	if err != nil {
		return err
	}

	c := api.NewClient(cfg.Host(*host))
	if _, err := c.Info(ctx); err != nil {
		return fmt.Errorf("can't reach the daemon: %w", err)
	}

	wait := *idle
	if wait == 0 {
		wait, _ = time.ParseDuration(cfg.Gateway.Idle)
	}
	if wait == 0 {
		wait = 5 * time.Minute
	}
	listen, idle = &addr, &wait

	memSvc := *memory
	if memSvc == "" && cfg.Memory.Enabled {
		memSvc = cfg.Memory.Service
	}
	gw := gateway.New(c, *idle, *ephemeral, *prewarm, memSvc)
	gw.PprofEnabled = *pprofOn
	gw.KeepWarm = *keepwarm
	// Cuotas por token/tenant, si la configuración las trae. Retrocompatible: sin
	// tokens con nombre, el token único sigue siendo el tenant "default" sin
	// límites. Es reparto justo, no una frontera de seguridad (todo comparte
	// daemon y bridge).
	if len(cfg.Gateway.Tokens) > 0 {
		tenants := make([]gateway.TenantLimit, 0, len(cfg.Gateway.Tokens))
		for _, t := range cfg.Gateway.Tokens {
			tenants = append(tenants, gateway.TenantLimit{
				Name:         t.Name,
				Token:        t.Token,
				MaxInstances: t.MaxInstances,
				MaxInflight:  t.MaxInflight,
			})
		}
		gw.SetTenants(tenants)
	}
	go gw.Reap(ctx)
	if *ephemeral {
		go gw.PrewarmAll(ctx)
	}
	// Calienta ya al arrancar, sin esperar al primer tick del segador (idle/3): así
	// los servicios populares están listos antes de la primera petición. No se ata a
	// -ephemeral a propósito —el keep-warm es justo para el modo persistente—.
	if *keepwarm > 0 {
		go gw.KeepWarmAll(ctx)
	}
	// Contexto propio y ACOTADO: no puede ser el del proceso, que ya está
	// cancelado cuando llega el apagado (los Remove no se harían), ni uno sin
	// límite, que dejaría a Ctrl-C esperando indefinidamente a un daemon que no
	// responde. Retirar las pre-calentadas es deseable, no obligatorio.
	defer func() {
		dc, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		gw.Drain(dc)
	}()

	srv := &http.Server{
		Addr:    *listen,
		Handler: gw.Handler(token),
		// Mismas razones que en el daemon: gateway es la única superficie
		// que escucha en TCP, y un cliente que no termina el header
		// mantiene goroutine + FD indefinidamente. /mcp/{svc} puede ser
		// streaming (SSE), así que ReadTimeout va holgado.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()

	// El gateway escucha en red; el daemon no. Por defecto solo en loopback:
	// abrirlo al mundo debe ser una decisión consciente.
	fmt.Printf("gateway at http://%s\n", *listen)
	fmt.Printf("  tool:         http://%s/mcp/<service>\n", *listen)
	fmt.Printf("  inventory:    http://%s/services\n", *listen)
	fmt.Printf("  idle:         %s before freezing\n", *idle)
	if token == "" {
		fmt.Printf("  auth:         DISABLED (-no-auth) — only valid because it listens on loopback\n")
	} else {
		fmt.Printf("  auth:         Authorization: Bearer <token>  ·  /healthz open\n")
	}
	if *pprofOn {
		fmt.Printf("  pprof:        ACTIVE at http://%s/debug/pprof/ (behind the token) — turn it off when done\n", *listen)
	}
	if memSvc != "" {
		fmt.Printf("  memory:       active on %q — ranks searches by what already worked\n", memSvc)
	}
	if *ephemeral {
		fmt.Printf("  mode:         EPHEMERAL — each action in its own microVM, destroyed when done\n")
		if *prewarm > 0 {
			fmt.Printf("  pre-warmed:   %d instance(s) per service, ready to respond\n", *prewarm)
		}
	}
	if *keepwarm > 0 {
		fmt.Printf("  keep-warm:    %d popular service(s) with their primary warm (no cold start)\n", *keepwarm)
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// resolveGatewayToken decide con qué token arranca el gateway.
//
// No hay flag `-token` a propósito: la línea de comandos de un proceso la lee
// cualquier usuario del host en /proc, así que un secreto no puede viajar por
// ahí. Variable de entorno para systemd, fichero de configuración para el resto,
// y si no hay ninguno se genera y se guarda: que el gateway quede abierto no
// puede ser lo que pasa cuando no configuras nada.
func resolveGatewayToken(cfg *config.Config, noAuth bool, addr string) (string, error) {
	if noAuth {
		if !gateway.IsLoopback(addr) {
			return "", fmt.Errorf("-no-auth requires listening on loopback, and %q is not.\n"+
				"Waking a snapshot means executing code: without a token, anyone who reaches\n"+
				"that port runs your tools. Remove -no-auth or listen on 127.0.0.1", addr)
		}
		return "", nil
	}
	if t := os.Getenv("KLING_GATEWAY_TOKEN"); t != "" {
		return t, nil
	}
	if cfg.Gateway.Token != "" {
		return cfg.Gateway.Token, nil
	}

	t, err := gateway.NewToken()
	if err != nil {
		return "", err
	}
	cfg.Gateway.Token = t

	// Si no se puede guardar NO se aborta: un gateway que se niega a arrancar
	// porque no pudo persistir un token es peor que uno que arranca y avisa.
	// Pasa con systemd, donde ProtectHome deja su configuración en solo lectura,
	// y ahí lo grave no es el fallo sino el silencio: el token cambiaría en cada
	// reinicio y todos los agentes ya configurados dejarían de entrar.
	if err := cfg.Save(); err != nil {
		fmt.Printf("\nWARNING: I generated a token but couldn't save it to %s (%v).\n", config.Path(), err)
		fmt.Printf("       It WILL CHANGE on every restart. Pin it so that doesn't happen:\n")
		fmt.Printf("         Environment=KLING_GATEWAY_TOKEN=%s\n\n", t)
		return t, nil
	}

	// La única vez que se imprime entero. A partir de aquí `config show` lo
	// enmascara, porque esa orden se teclea con gente mirando la pantalla.
	fmt.Printf("token generated and saved to %s\n\n", config.Path())
	fmt.Printf("  On the machine where you use the CLI:\n")
	fmt.Printf("    kling config set gateway.token %s\n\n", t)
	return t, nil
}

// ── máquinas ──────────────────────────────────────────────────────────────────

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "machine name")
	image := fs.String("image", "", "rootfs image (default: defaults.image, or 'default')")
	from := fs.String("from", "", "instantiate from a golden snapshot (~ms, no cold start)")
	cpus := fs.Int("cpus", 0, "vCPUs (default: 1)")
	mem := fs.Int("mem", 0, "memory in MiB (default: 256)")
	egress := fs.String("egress", "", "network egress: none | internet | allowlist (never reaches private networks)")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist (comma-separated)")
	ttl := fs.Int("ttl", 0, "seconds until it freezes itself (0 = never)")
	cpuPct := fs.Int("cpu-pct", 0, "CPU ceiling as a percentage of one core (0 = default)")
	cpu := fs.Int("cpu", 0, "deprecated alias of -cpu-pct")
	service := fs.String("service", "", "MCP service it belongs to (groups in topo and export)")
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volume to mount: name[:/mount][:ro] (repeatable)")
	mount := fs.String("mount", "", "where to mount the volume (default /data; only with one)")
	volRO := fs.Bool("volume-ro", false, "mount it read-only: so several microVMs can share it")
	var labels labelFlag
	fs.Var(&labels, "label", "key=value label (repeatable)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	vols, err := volumeSet(volumes, *mount, *volRO)
	if err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	mc, err := api.NewClient(cfg.Host(*host)).Run(ctx, api.RunRequest{
		Name:  *name,
		From:  *from,
		Image: config.Or(*image, cfg.Defaults.Image, "default"),
		// El flag gana; si no se dio, manda la configuración; y si tampoco,
		// el valor incorporado.
		VCPUs:        config.Or(*cpus, cfg.Defaults.VCPUs, 1),
		MemMiB:       config.Or(*mem, cfg.Defaults.MemMiB, 256),
		Egress:       config.Or(*egress, cfg.Defaults.Egress, "none"),
		AllowDomains: splitDomains(*allow),
		TTLSeconds:   config.Or(*ttl, cfg.Defaults.TTL),
		CPUPct:       config.Or(resolveCPUPct(fs, *cpuPct, *cpu), cfg.Defaults.CPUPct),
		Labels:       labels.merge(*service),
		// El volumen es una propiedad de la MÁQUINA, no solo de un servicio MCP:
		// arrancar una a mano con almacenamiento que sobreviva es tan legítimo
		// como importar un servicio con él.
		Volumes: vols,
	})
	if err != nil {
		return err
	}
	if mc.From != "" {
		fmt.Printf("%s  %s  instantiated from %s in %d ms\n", mc.ID[:12], mc.Name, mc.From, mc.ThawMS)
	} else {
		fmt.Printf("%s  %s  booted cold in %d ms\n", mc.ID[:12], mc.Name, mc.BootMS)
	}
	return nil
}

// labelFlag acumula -label k=v repetidos.
type labelFlag map[string]string

func (l *labelFlag) String() string { return "" }

func (l *labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("invalid label %q: use key=value", v)
	}
	if *l == nil {
		*l = labelFlag{}
	}
	(*l)[k] = val
	return nil
}

// merge añade -service como la etiqueta convencional "service".
func (l labelFlag) merge(service string) map[string]string {
	if service == "" && len(l) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range l {
		out[k] = v
	}
	if service != "" {
		out[api.LabelService] = service
	}
	return out
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	host := hostFlag(fs)
	out := fs.String("o", "kindling.html", "output file")
	// El mapa ya trae todo el detalle; la bandera sigue aceptándose para no
	// romper a quien la tuviera en un script.
	_ = fs.Bool("detail", false, "deprecated: the report always includes detail")
	open := fs.Bool("open", false, "open it when done")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	machines, err := c.List(ctx)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}

	// El HTML se construye aquí, en la máquina del CLI: el fichero acaba donde
	// trabajas aunque el daemon esté al otro lado de un SSH.
	links, _ := c.Links(ctx) // un daemon antiguo puede no tenerlos: no es fatal
	memSvc := ""
	if cfg := loadConfig(); cfg.Memory.Enabled {
		memSvc = cfg.Memory.Service
	}
	groups := report.BuildWith(machines, snaps, links)
	doc := report.RenderMap(info, groups, c.Endpoint(), time.Now(), memSvc)
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		return err
	}
	abs, _ := filepath.Abs(*out)
	fmt.Printf("%s  (%d machines, %d snapshots, %.0f KB)\n",
		abs, len(machines), len(snaps), float64(len(doc))/1024)

	if *open {
		_ = exec.Command(openCmd(), abs).Start()
	}
	return nil
}

func openCmd() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	host := hostFlag(fs)
	tail := fs.Int("tail", 200, "last N lines (0 = all)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling logs <ref> [-tail N]")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	out, err := api.NewClient(hostOf(*host)).Logs(ctx, fs.Arg(0), *tail)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdCommit(args []string) error {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling commit <ref> <snapshot-name>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	snap, err := api.NewClient(hostOf(*host)).Commit(ctx, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	fmt.Printf("%s  golden snapshot  (%s of memory)\n", snap.Name, human(snap.MemBytes))
	fmt.Printf("instantiate with:  kling run -from %s\n", snap.Name)
	return nil
}

func cmdSnapshots(args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	list, err := api.NewClient(hostOf(*host)).Snapshots(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(list)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tCPU/MEM\tMEMORY\tDISK\tINSTANCES\tAGE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%d/%dMiB\t%s\t%s\t%d\t%s\n",
			s.Name, s.Image, s.VCPUs, s.MemMiB,
			human(s.MemBytes), human(s.DiskBytes), s.Instances, since(s.CreatedAt))
	}
	return tw.Flush()
}

func cmdRmi(args []string) error {
	fs := flag.NewFlagSet("rmi", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling rmi <snapshot-name>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	for _, n := range fs.Args() {
		if err := c.RemoveSnapshot(ctx, n); err != nil {
			return err
		}
		fmt.Println(n)
	}
	return nil
}

func cmdPS(args []string) error {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	host := hostFlag(fs)
	all := fs.Bool("a", false, "include stopped ones")
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only machine IDs (for scripting)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	list, err := api.NewClient(hostOf(*host)).List(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(list)
	}
	// -q imprime solo IDs (12 chars), una por línea: pensado para tuberías como
	// `kling rm $(kling ps -q)`. Respeta -a igual que la tabla.
	if *quiet {
		for _, mc := range list {
			if !*all && (mc.State == api.StateStopped || mc.State == api.StateFailed) {
				continue
			}
			fmt.Println(mc.ID[:12])
		}
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tCPU/MEM\tDISK\tEGRESS\tAGE\tLAST OP")
	var totalDisk int64
	for _, mc := range list {
		if !*all && (mc.State == api.StateStopped || mc.State == api.StateFailed) {
			continue
		}
		totalDisk += mc.DiskBytes
		eg := mc.Egress
		if eg == "" {
			eg = "none"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%dMiB\t%s\t%s\t%s\t%s\n",
			mc.ID[:12], mc.Name, mc.Image, mc.State,
			mc.VCPUs, mc.MemMiB, human(mc.DiskBytes), eg, since(mc.CreatedAt), lastOp(mc))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if totalDisk > 0 {
		fmt.Printf("\nmachines' own disk: %s (base image is shared)\n", human(totalDisk))
	}
	return nil
}

// human formatea bytes de forma compacta.
func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dM", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%dK", b>>10)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// lastOp resume el coste de la última transición: es el número que justifica
// todo el diseño de snapshot/restore.
func lastOp(mc *api.Machine) string {
	switch {
	case mc.ThawMS > 0 && mc.State == api.StateRunning:
		return fmt.Sprintf("thaw %dms", mc.ThawMS)
	case mc.State == api.StateWarm:
		return fmt.Sprintf("freeze %dms, %dMiB", mc.FreezeMS, mc.SnapSize>>20)
	case mc.BootMS > 0:
		return fmt.Sprintf("boot %dms", mc.BootMS)
	}
	return "-"
}

func since(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func cmdLifecycle(op string, args []string) error {
	fs := flag.NewFlagSet(op, flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling %s <ref>", op)
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	for _, ref := range fs.Args() {
		var mc *api.Machine
		var err error
		switch op {
		case "freeze":
			mc, err = c.Freeze(ctx, ref)
		case "thaw":
			mc, err = c.Thaw(ctx, ref)
		case "stop":
			mc, err = c.Stop(ctx, ref)
		case "rm":
			err = c.Remove(ctx, ref)
		}
		if err != nil {
			return err
		}
		switch {
		case op == "rm":
			fmt.Println(ref)
		case op == "freeze":
			fmt.Printf("%s  warm  (%d ms, %d MiB on disk)\n", mc.ID[:12], mc.FreezeMS, mc.SnapSize>>20)
		case op == "thaw":
			fmt.Printf("%s  running  (%d ms)\n", mc.ID[:12], mc.ThawMS)
		default:
			fmt.Printf("%s  %s\n", mc.ID[:12], mc.State)
		}
	}
	return nil
}

// cmdSqueeze aprieta el globo de una o varias microVMs running para devolver al
// host la RAM que el invitado tiene libre, sin congelarlas. A diferencia de
// freeze, la máquina sigue viva y atendiendo: es el ahorro barato entre sesiones.
func cmdSqueeze(args []string) error {
	fs := flag.NewFlagSet("squeeze", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling squeeze <ref>...")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	for _, ref := range fs.Args() {
		res, err := c.Squeeze(ctx, ref)
		if err != nil {
			return err
		}
		fmt.Printf("%s  ~%d MiB returned to the host  (guest free %d MiB, RSS now %d MiB)\n",
			res.ID[:12], res.ReclaimedMiB, res.GuestFreeMiB, res.RSSMiB)
	}
	return nil
}

// cmdMMDS inyecta un secreto de sesión en una microVM viva por MMDS. El store es
// un documento JSON que se lee de -f o de stdin. Es sobre todo para pruebas en el
// lab: en producción quien inyecta es el gateway al resolver una sesión.
//
// Esquema del store (lo entiende el bridge de dentro):
//
//	{
//	  "env": { "VAR_COMUN": "valor" },
//	  "sessions": { "<Mcp-Session-Id>": { "TOKEN": "secreto-de-esa-sesion" } }
//	}
//
// El secreto NO viaja por la línea de comandos (cualquiera lee /proc/<pid>/cmdline):
// se lee de un fichero o de la entrada estándar.
func cmdMMDS(args []string) error {
	fs := flag.NewFlagSet("mmds", flag.ExitOnError)
	host := hostFlag(fs)
	file := fs.String("f", "", "JSON file with the MMDS store (default: stdin)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling mmds <ref> [-f store.json]  (reads stdin if no -f)")
	}

	var raw []byte
	var err error
	if *file != "" {
		raw, err = os.ReadFile(*file)
	} else {
		raw, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}

	// Se valida que sea JSON antes de mandarlo: un store inválido lo rechazaría
	// Firecracker con un error mucho menos claro.
	var data json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("the MMDS store is not valid JSON: %w", err)
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	mc, err := api.NewClient(hostOf(*host)).PutMMDS(ctx, fs.Arg(0), data)
	if err != nil {
		return err
	}
	fmt.Printf("%s  secrets injected via MMDS (can no longer be frozen)\n", mc.ID[:12])
	return nil
}

// ── observación ───────────────────────────────────────────────────────────────

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "one JSON line per event")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	return api.NewClient(hostOf(*host)).Events(ctx, func(ev api.Event) {
		if *asJSON {
			_ = enc.Encode(ev)
			return
		}
		line := fmt.Sprintf("%s  %-16s %s", ev.Time.Format("15:04:05"), ev.Type, ev.Name)
		if ev.Message != "" {
			line += "  " + ev.Message
		}
		fmt.Println(line)
	})
}

// cmdTopo dibuja la topología: qué snapshots hay, qué instancias cuelgan de cada
// uno y por dónde se alcanzan. Agrupa por snapshot porque es la relación que
// determina el coste: las instancias de un mismo snapshot comparten memoria.
func cmdTopo(args []string) error {
	fs := flag.NewFlagSet("topo", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	machines, err := c.List(ctx)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}

	kvm := "no KVM"
	if info.KVM {
		kvm = "KVM ok"
	}
	fmt.Printf("kindling  %s\n", c.Endpoint())
	fmt.Printf("          %s · %s\n\n", kvm, strings.TrimSpace(info.Firecrack))

	// agrupar por snapshot de origen
	byFrom := map[string][]*api.Machine{}
	for _, mc := range machines {
		byFrom[mc.From] = append(byFrom[mc.From], mc)
	}

	groups := make([]string, 0, len(snaps)+1)
	for _, s := range snaps {
		groups = append(groups, s.Name)
	}
	if len(byFrom[""]) > 0 {
		groups = append(groups, "")
	}

	fmt.Printf("  host  %s\n", netRange(machines))
	var running, warm, stopped int
	var diskOwn, diskShared int64
	for _, s := range snaps {
		diskShared += s.DiskBytes
	}

	for gi, g := range groups {
		last := gi == len(groups)-1
		branch, cont := "├─", "│ "
		if last {
			branch, cont = "└─", "  "
		}

		if g == "" {
			fmt.Printf("   %s◆ (booted cold)\n", branch)
		} else {
			var snap *api.Snapshot
			for _, s := range snaps {
				if s.Name == g {
					snap = s
				}
			}
			fmt.Printf("   %s◆ %-16s golden snapshot · %s shared memory\n",
				branch, g, human(snap.MemBytes))
		}

		list := byFrom[g]
		for mi, mc := range list {
			mbranch := "├──"
			if mi == len(list)-1 {
				mbranch = "└──"
			}
			ip := mc.IP
			if mc.State != api.StateRunning || ip == "" {
				ip = "—"
			}
			diskOwn += mc.DiskBytes
			switch mc.State {
			case api.StateRunning:
				running++
			case api.StateWarm:
				warm++
			default:
				stopped++
			}
			egMark := "⌀" // aislada
			if mc.Egress == "internet" {
				egMark = "→" // sale a internet (nunca a redes privadas)
			}
			fmt.Printf("   %s %s %-14s %-8s %-15s %s %6s  %s\n",
				cont, mbranch, trunc(mc.Name, 14), mc.State, ip, egMark, human(mc.DiskBytes), lastOp(mc))
		}
		if len(list) == 0 {
			fmt.Printf("   %s └── (no instances)\n", cont)
		}
		if !last {
			fmt.Printf("   │\n")
		}
	}

	fmt.Printf("\n  %d running · %d warm · %d stopped   disk: %s own + %s shared\n",
		running, warm, stopped, human(diskOwn), human(diskShared))
	fmt.Printf("  egress:  ⌀ isolated   → internet (private networks are always blocked)\n")
	return nil
}

// netRange resume el rango en uso a partir de las máquinas vivas.
func netRange(ms []*api.Machine) string {
	for _, mc := range ms {
		if mc.IP != "" {
			if i := strings.LastIndex(mc.IP, "."); i > 0 {
				if j := strings.Index(mc.IP, "."); j > 0 {
					return mc.IP[:strings.Index(mc.IP[j+1:], ".")+j+1] + ".0.0/16"
				}
			}
		}
	}
	return "(no active network)"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	i, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(i)
	}
	kvm := "no"
	if i.KVM {
		kvm = "yes"
	}
	fmt.Printf("endpoint:     %s\n", c.Endpoint())
	fmt.Printf("daemon:       %s\n", i.Version)
	fmt.Printf("root:         %s\n", i.Root)
	fmt.Printf("KVM:          %s\n", kvm)
	fmt.Printf("firecracker:  %s\n", strings.TrimSpace(i.Firecrack))
	fmt.Printf("machines:     %d\n", i.Machines)
	return nil
}
