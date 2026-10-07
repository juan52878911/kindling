// kling gestiona microVMs de Firecracker con una interfaz al estilo de docker.
//
// El mismo binario hace de CLI y de daemon: `kling daemon` arranca el núcleo
// donde esté KVM, y el CLI le habla por un socket Unix, local o a través de SSH.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/daemon"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/plugin"
	"github.com/juan52878911/kindling/pkg/transport"
	"github.com/juan52878911/kindling/pkg/units"
)

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stdout)
		os.Exit(2)
	}
	// Los nombres de antes se traducen a los de ahora (tree.go), con un aviso
	// en stderr salvo los que se quedan para siempre.
	if w := aliasWarning(os.Args[1], os.Args[2:]); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	cmd, args := resolveAlias(os.Args[1], os.Args[2:])

	// `kling run -h`, `kling mcp add -h`: la misma ayuda que `kling help ...`,
	// salvo en el proceso que la genera (plugin.FlagHelp).
	if words, ok := helpRequest(cmd, args); ok {
		if err := cmdHelp(words); err != nil {
			printError(os.Stderr, err)
			os.Exit(codigoDeSalida(err))
		}
		return
	}

	var err error
	switch cmd {
	case "daemon":
		err = cmdDaemon(args)
	case "up":
		err = cmdUp(args)
	case "status":
		err = cmdStatus(args)
	case "doctor":
		err = cmdDoctor(args)
	case "try":
		// Como exec: termina con el código del comando de dentro.
		code, xerr := cmdTry(args)
		if xerr != nil {
			printError(os.Stderr, xerr)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	case "volume":
		err = cmdVolume(args)
	case "dial-stdio": // extremo remoto del transporte SSH, no para uso manual
		err = transport.ServeStdio(envOr("KLING_SOCKET", transport.DefaultSocketPath()), os.Stdin, os.Stdout)
	case "run":
		err = cmdRun(args)
	case "exec":
		// Termina con el código del comando remoto, sin el "error:" de siempre:
		// un 1 de grep no es un fallo de kling.
		code, xerr := cmdExec(args)
		if xerr != nil {
			printError(os.Stderr, xerr)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	case "shell":
		// Como exec: el código es el de la shell remota.
		code, xerr := cmdShell(args)
		if xerr != nil {
			printError(os.Stderr, xerr)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	case "cp":
		err = cmdCp(args)
	case "sandbox":
		err = cmdSandbox(args)
	case "graph":
		err = cmdGraph(args)
	case "inspect":
		err = cmdInspect(args)
	case "ps":
		err = cmdPS(args)
	case "logs":
		err = cmdLogs(args)
	case "freeze", "thaw", "pause", "stop", "rm":
		err = cmdLifecycle(cmd, args)
	case "start":
		err = cmdStart(args)
	case "machine":
		err = cmdMachine(args)
	case "save":
		err = cmdSave(args)
	case "template":
		err = cmdTemplate(args)
	case "image":
		err = cmdImages(args)
	case "topo":
		err = cmdTopo(args)
	case "top":
		err = cmdTop(args)
	case "events":
		err = cmdEvents(args)
	case "context":
		err = cmdContext(args)
	case "cow":
		err = cmdCoW(args)
	case "config":
		err = cmdConfig(args)
	case "completion":
		err = cmdCompletion(args)
	case "version", "--version", "-v":
		err = cmdVersion(args)
	case "plugin":
		err = cmdPlugins(args)
	case "builder": // lo ejecuta el daemon como root; ver builder.go
		err = cmdBuilder(args)
	case "-h", "--help", "help":
		err = cmdHelp(args)
	default:
		// Lo que no es del núcleo lo sirve una extensión: `kling mcp add x`
		// llega a kling-mcp como `add x`, y `kling connect` como `connect`.
		// Una incorporada corre aquí mismo; una externa reemplaza este proceso
		// y no vuelve.
		if p, argv := extensions().Resolve(cmd, args); p != nil {
			if len(argv) == 0 {
				// `kling mcp` a secas: su ayuda, como `kling` a secas.
				plugin.WriteNamespace(os.Stdout, "kling "+cmd, *p.Manifest)
				os.Exit(2)
			}
			err = plugin.Exec(p, argv[0], argv[1:], configPath())
			break
		}
		if p := extensions().DisabledFor(cmd); p != nil {
			err = &errConCodigo{code: 2, err: &errWithHint{
				err:  fmt.Errorf("kling %s comes from extension %q, which is disabled", cmd, p.Name),
				hint: "kling plugin enable " + p.Name}}
			break
		}
		if ext, ok := movedToExtension[cmd]; ok {
			err = movedError(cmd, ext)
			break
		}
		// Sin volcar la ayuda entera: tapa el error, que es lo que hay que leer.
		err = &errConCodigo{code: 2, err: &errWithHint{
			err: fmt.Errorf("unknown command %q", cmd), hint: "kling help"}}
	}

	if err != nil {
		printError(os.Stderr, err)
		os.Exit(codigoDeSalida(err))
	}
}

// cmdMachine agrupa lo que se hace a una máquina viva y casi nadie teclea:
// `kling machine resize|squeeze|secret|credential|audit`. Los nombres de antes (resize,
// squeeze, mmds) son alias.
func cmdMachine(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling machine <resize|squeeze|secret|credential|audit|ready|hooks> <ref> [...]")
	}
	switch args[0] {
	case "resize":
		return cmdResize(args[1:])
	case "squeeze":
		return cmdSqueeze(args[1:])
	case "secret", "mmds":
		return cmdMMDS(args[1:])
	case "credential":
		return cmdCredential(args[1:])
	case "audit":
		return cmdCredAudit(args[1:])
	case "ready":
		return cmdReady(args[1:])
	case "hooks":
		return cmdHooks(args[1:])
	}
	return fmt.Errorf("unknown subcommand %q: use resize, squeeze, secret, credential, audit, ready or hooks", args[0])
}

// errConCodigo deja que un comando pida un codigo de salida concreto.
//
// Existe por una distincion que systemd necesita y el codigo 1 no permite:
// "hice mi trabajo y algo sigue mal" no es lo mismo que "no pude ni empezar".
// La primera no debe marcar la unidad como fallida —el estado ya quedo grabado
// donde toca—; la segunda si, porque significa que nadie comprobo nada.
type errConCodigo struct {
	code int
	err  error
}

func (e *errConCodigo) Error() string { return e.err.Error() }
func (e *errConCodigo) Unwrap() error { return e.err }

func codigoDeSalida(err error) int {
	var e *errConCodigo
	if errors.As(err, &e) {
		return e.code
	}
	return plugin.ExitCode(err)
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

// egressForRun decide qué egress y qué dominios mandar en la petición de
// arranque. Con -from y sin -egress explícito en la línea de comandos, se
// mandan vacíos para que runFrom() (internal/machine/snapshot.go) herede la
// política de la plantilla: sin esto, una plantilla con credenciales —que
// exige egress allowlist— obligaba a repetir -egress allowlist -allow a mano
// en cada instancia, aunque la plantilla ya llevara esos datos consigo.
// Sin -from, o con -egress dado explícitamente, se mantiene el defecto de
// siempre: flag > configuración > "none".
func egressForRun(fs *flag.FlagSet, from, egress, allow string, cfg *config.Config) (string, []string) {
	if from != "" {
		explicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "egress" {
				explicit = true
			}
		})
		if !explicit {
			return "", nil
		}
	}
	return config.Or(egress, cfg.Defaults.Egress, "none"), splitDomains(allow)
}

// hostOf resuelve a qué daemon hablar.
func hostOf(flagValue string) string { return loadConfig().Host(flagValue) }

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ── daemon ────────────────────────────────────────────────────────────────────

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", envOr("KLING_SOCKET", transport.DefaultSocketPath()), "Unix socket to serve")
	root := fs.String("root", envOr("KLING_ROOT", transport.DefaultRoot()), "data directory")
	fcBin := fs.String("firecracker", envOr("KLING_FIRECRACKER", "firecracker"), "firecracker binary")
	sockUser := fs.String("socket-user", os.Getenv("KLING_SOCKET_USER"), "user to hand the socket to (for the CLI over SSH)")
	runAs := fs.String("run-as", envOr("KLING_RUN_AS", "kindling"), "unprivileged user Firecracker runs as")
	buildAs := fs.String("build-as", envOr("KLING_BUILD_AS", "kindling-build"), "unprivileged user the oci image builder runs as (not the Firecracker one)")
	authzPath := fs.String("authz", os.Getenv("KLING_AUTHZ"), "authz policy file (default "+daemon.RutaAuthzPorDefecto+" if it exists; see docs/authz.md)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	// Una ruta explícita tiene que existir: arrancar sin política creyendo
	// que se tiene sería peor que no arrancar.
	pol, err := daemon.CargarPolitica(config.Or(*authzPath, daemon.RutaAuthzPorDefecto), *authzPath != "")
	if err != nil {
		return err
	}

	backend, err := daemonBackend(loadConfig().Daemon.VMM, os.Getenv("KLING_VMM"), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	vmm := machine.ResolverVMM(backend, os.Getenv("KLING_VMM"), *fcBin)

	daemon.Version = strings.TrimPrefix(Version, "v")
	machine.SetCredAuditConfig(credAuditConfig())
	srv, err := daemon.New(*socket, *root, vmm, *sockUser, *runAs)
	if err != nil {
		return err
	}
	srv.SetShareConfig(shareConfig)
	srv.SetCoW(cowConfig())
	srv.SetAuthz(pol)
	srv.SetBuildUser(*buildAs)
	srv.SetBuildCache(buildCacheConfig)
	ctx, stop := ctxWithSignals()
	defer stop()
	return srv.Listen(ctx)
}

// shareConfig lee la configuración de carpetas compartidas del daemon. Se
// llama en cada petición que la necesita, así que cambiarla con `kling config
// set` no pide reiniciar el daemon. Las variables de entorno mandan sobre el
// fichero: en systemd es lo cómodo.
func shareConfig() machine.ShareConfig {
	cfg := loadConfig()
	roots := cfg.Daemon.ShareRoots
	if v, ok := os.LookupEnv("KLING_SHARE_ROOTS"); ok {
		r, err := config.ParseShareRoots(v)
		if err != nil {
			log.Printf("warning: KLING_SHARE_ROOTS: %v (no live shares allowed)", err)
		}
		roots = r
	}
	mib := cfg.Daemon.ShareCopyMaxMiB
	if v := os.Getenv("KLING_SHARE_COPY_MAX_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mib = n
		}
	}
	return machine.ShareConfig{Roots: roots, CopyMaxBytes: int64(mib) << 20}
}

// buildCacheConfig lee daemon.build_cache_max_gib y
// daemon.build_cache_max_days; KLING_BUILD_CACHE_MAX_GIB y
// KLING_BUILD_CACHE_MAX_DAYS mandan sobre el fichero. Se llama en cada
// construcción: cambiarlos no pide reiniciar el daemon. 0, un valor que no
// se entiende o uno por encima del tope (daemon.BuildCacheMax*Tope) = el de
// por defecto.
func buildCacheConfig() daemon.LimitesCacheConstruccion {
	cfg := loadConfig()
	l := daemon.LimitesCacheConstruccion{MaxGiB: cfg.Daemon.BuildCacheMaxGiB, MaxDays: cfg.Daemon.BuildCacheMaxDays}
	for _, v := range []struct {
		env string
		dst *int
		max int
	}{{"KLING_BUILD_CACHE_MAX_GIB", &l.MaxGiB, daemon.BuildCacheMaxGiBTope}, {"KLING_BUILD_CACHE_MAX_DAYS", &l.MaxDays, daemon.BuildCacheMaxDaysTope}} {
		if s := os.Getenv(v.env); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				log.Printf("warning: %s=%q is not a whole number (using the default)", v.env, s)
				n = 0
			} else if n > v.max {
				log.Printf("warning: %s=%q is over %d (using the default)", v.env, s, v.max)
				n = 0
			}
			*v.dst = n
		}
	}
	return l
}

// credAuditConfig lee daemon.credaudit_max_mib y daemon.credaudit_generations;
// KLING_CREDAUDIT ("MIB:GENERACIONES") manda sobre el fichero. Un valor que no
// se entiende se avisa y se queda en los de por defecto.
func credAuditConfig() credproxy.AuditConfig {
	cfg := loadConfig()
	c := credproxy.AuditConfig{MaxBytes: int64(cfg.Daemon.CredAuditMiB) << 20, Generations: cfg.Daemon.CredAuditGenerations}
	if v := os.Getenv("KLING_CREDAUDIT"); v != "" {
		p, err := credproxy.ParseAuditConfig(v)
		if err != nil {
			log.Printf("warning: KLING_CREDAUDIT: %v (using the defaults)", err)
			return credproxy.AuditConfig{}
		}
		c = p
	}
	return c
}

// cowConfig lee daemon.cow y daemon.cow_store_gib; KLING_COW y
// KLING_COW_STORE_GIB mandan sobre el fichero. Un valor que no se entiende se
// avisa y se queda en auto.
func cowConfig() machine.CoWConfig {
	cfg := loadConfig()
	c := machine.CoWConfig{Mode: cfg.Daemon.CoW, StoreGiB: cfg.Daemon.CoWStoreGiB}
	if v, ok := os.LookupEnv("KLING_COW"); ok {
		c.Mode = v
	}
	if err := config.ValidateCoW(c.Mode); err != nil {
		log.Printf("warning: %v (using %q)", err, config.CoWAuto)
		c.Mode = config.CoWAuto
	}
	if c.Mode == "" {
		c.Mode = config.CoWAuto
	}
	if v := os.Getenv("KLING_COW_STORE_GIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.StoreGiB = n
		}
	}
	return c
}

// daemonBackend decide con qué VMM arranca el daemon: KLING_VMM si es un
// nombre de backend, si no la clave daemon.vmm, si no el de la plataforma. Y
// comprueba que ESTE binario sepa hablarlo: lo que hace el host alrededor del
// VMM (red, cgroups, /proc) va compilado para un sistema concreto.
func daemonBackend(ajuste, env, goos, goarch string) (string, error) {
	backend := ajuste
	if env == config.VMMFirecracker || env == config.VMMVZ {
		backend = env
	}
	if backend == "" {
		backend = config.DefaultVMM(goos)
	}
	if err := config.ValidateVMM(backend, goos, goarch); err != nil {
		return "", fmt.Errorf("daemon.vmm: %w", err)
	}
	if c := machine.BackendCompilado(); backend != c {
		return "", fmt.Errorf("daemon.vmm is %q but this kling binary was built for %q", backend, c)
	}
	return backend, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "machine name")
	image := fs.String("image", "", "rootfs image (default: defaults.image, or 'default')")
	from := fs.String("from", "", "instantiate from a golden snapshot (~ms, no cold start)")
	cpus := fs.Int("cpus", 0, "vCPUs (default: 1)")
	mem := units.MiBVar(fs, "mem", 0, "memory: 512M, 2G (bare number = MiB; default: 256)")
	memMax := units.MiBVar(fs, "mem-max", 0, "ceiling for resizing its memory later without restarting (kling machine resize)")
	disk := units.MiBVar(fs, "disk", 0, "writable disk of the machine: 2G, 8G (bare number = MiB; default: 512; ignored with -from)")
	var ef envFlags
	ef.register(fs)
	egress := fs.String("egress", "", "network egress: none | internet | allowlist (never reaches private networks)")
	var allow domainsFlag
	fs.Var(&allow, "allow", "domain allowed with -egress allowlist (repeatable, or comma-separated)")
	ttl := units.SecondsVar(fs, "ttl", 0, "time until it freezes itself: 10m, 1h (bare number = seconds; 0 = never)")
	cpuPct := fs.Int("cpu-pct", 0, "CPU ceiling as a percentage of one core (0 = default)")
	cpu := fs.Int("cpu", 0, "deprecated alias of -cpu-pct")
	service := fs.String("service", "", "service it belongs to (groups machines in topo and metrics)")
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volume to mount: name[:/mount][:ro] (repeatable)")
	mount := fs.String("mount", "", "where to mount the volume (default /data; only with one)")
	volRO := fs.Bool("volume-ro", false, "mount it read-only: so several microVMs can share it")
	allowExec := fs.Bool("allow-exec", false, "enable kling exec and kling cp on this machine (decided at boot; snapshots keep it)")
	onTTL := fs.String("on-ttl", "", "what happens when -ttl runs out: freeze (default) or remove")
	waitReady := fs.Bool("wait-ready", false, "return once the guest is ready by its image's own probe and post-restore hooks, not just when its agent answers")
	readyTimeout := fs.Duration("ready-timeout", 0, "with -wait-ready, how long to wait (default 2m)")
	var labels labelFlag
	fs.Var(&labels, "label", "key=value label (repeatable)")
	var shares shareFlag
	fs.Var(&shares, "share", shareUsage)
	asJSON := fs.Bool("json", false, "print the machine as JSON (id, name, ip, ready...) for scripts and agents")
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
	endpoint := cfg.Host(*host)
	client := api.NewClient(endpoint)
	shareSpecs, err := prepareShares(ctx, client, endpoint, shares)
	if err != nil {
		return err
	}
	egressReq, allowReq := egressForRun(fs, *from, *egress, allow.String(), cfg)
	// Una referencia de Docker en -image se importa sola (run_docker.go). El
	// entorno de -e es de la máquina, no de la imagen: viaja en el cuerpo de
	// la petición y el daemon se lo da al invitado por MMDS
	// (pkg/api/machine_env.go). Con -from no hay: la copia lleva el del dorado.
	imagen := config.Or(*image, cfg.Defaults.Image, "default")
	env, err := ef.resolve()
	if err != nil {
		return err
	}
	if len(env) > 0 && *from != "" {
		return fmt.Errorf("-e and -env-file don't apply with -from: the copy runs with the environment of the machine " +
			"the template was saved from (its service is already running with it)")
	}
	if _, err := api.MachineEnvMap(env); err != nil {
		return err
	}
	if len(env) > 0 || (*disk > 0 && *from == "") {
		// Un daemon anterior ignoraría el campo y la máquina arrancaría sin
		// su entorno, o con el disco fijo de 512 MiB, sin un solo error.
		info, err := client.Info(ctx)
		if err != nil {
			return err
		}
		if len(env) > 0 && !slices.Contains(info.Capabilities, api.CapabilityMachineEnv) {
			return fmt.Errorf("the daemon (%s) does not take -e at run: update it", info.Version)
		}
		if *disk > 0 && *from == "" && !slices.Contains(info.Capabilities, api.CapabilityDisk) {
			return fmt.Errorf("the daemon (%s) does not take -disk: update it", info.Version)
		}
	}
	if *from == "" && esRefDocker(imagen) {
		if imagen, err = asegurarImagenDocker(ctx, client, imagen); err != nil {
			return err
		}
	}
	mc, err := client.Run(ctx, api.RunRequest{
		Name:  *name,
		From:  *from,
		Image: imagen,
		// El flag gana; si no se dio, manda la configuración; y si tampoco,
		// el valor incorporado.
		VCPUs:        config.Or(*cpus, cfg.Defaults.VCPUs, 1),
		MemMiB:       config.Or(*mem, cfg.Defaults.MemMiB, 256),
		MemMaxMiB:    *memMax,
		DiskMiB:      *disk,
		Egress:       egressReq,
		AllowDomains: allowReq,
		TTLSeconds:   config.Or(*ttl, cfg.Defaults.TTL),
		// El flag va en CPUPct; el valor por defecto de la configuración, aparte:
		// el del dorado y el de la receta de la imagen le ganan (ver RunRequest).
		CPUPct:              resolveCPUPct(fs, *cpuPct, *cpu),
		CPUPctDefault:       cfg.Defaults.CPUPct,
		WaitReady:           *waitReady,
		ReadyTimeoutSeconds: int(readyTimeout.Seconds()),
		Labels:              labels.merge(*service),
		// El volumen es una propiedad de la MÁQUINA, no solo de un servicio MCP:
		// arrancar una a mano con almacenamiento que sobreviva es tan legítimo
		// como importar un servicio con él.
		Volumes:   vols,
		Shares:    shareSpecs,
		AllowExec: *allowExec,
		OnTTL:     *onTTL,
		Env:       env,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(mc); err != nil {
			return err
		}
		if *waitReady && (mc.Ready == api.ReadyWaiting || mc.Ready == api.ReadyFailed) {
			return fmt.Errorf("%s is running but not ready (%s)", mc.Name, mc.Ready)
		}
		return nil
	}
	if mc.From != "" {
		fmt.Printf("%s  %s  instantiated from %s in %d ms\n", mc.ID[:12], mc.Name, mc.From, mc.ThawMS)
	} else {
		fmt.Printf("%s  %s  booted cold in %d ms\n", mc.ID[:12], mc.Name, mc.BootMS)
	}
	if *waitReady {
		switch mc.Ready {
		case api.ReadyYes:
			fmt.Printf("  ready (its image's probe passed)\n")
		case api.ReadyWaiting, api.ReadyFailed:
			return &errWithHint{err: fmt.Errorf("%s is running but not ready (%s)", mc.Name, mc.Ready),
				hint: "kling machine ready " + mc.Name + "   (says why)"}
		}
	}
	for _, s := range mc.Shares {
		fmt.Printf("  %s  %s  (%s)\n", s.Mount, s.Source, s.Mode)
	}
	if *allowExec {
		next("kling exec %s -- <cmd>   ·   kling save %s <name>", mc.ID[:12], mc.ID[:12])
	} else {
		next("kling logs %s   ·   kling save %s <name>", mc.ID[:12], mc.ID[:12])
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

// cmdSave es `kling save <ref> <name>` (antes `commit`): una máquina viva se
// convierte en plantilla de la que `run -from` instancia en milisegundos.
func cmdSave(args []string) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	host := hostFlag(fs)
	// Reemplazar es opt-in: los snapshots quedan atados al TSC del host y un
	// reinicio los invalida todos, así que rehacerlos con el mismo nombre es
	// rutina — pero pisar uno por un nombre repetido sin querer no debe poder
	// pasar, y por eso no es el comportamiento por defecto.
	replace := fs.Bool("replace", false, "replace the template if one with this name already exists")
	// Congelar un servidor que no sirve produce un snapshot que NO sirve, y el
	// fallo no aparece hasta que alguien lo despierta —minutos u horas despues—
	// con un "tool did not start listening" que no menciona el commit. Por eso
	// se comprueba antes, y por eso saltarselo es explicito.
	force := fs.Bool("force", false, "save even if the guest is not serving (produces a template that may not work)")
	// El hijo caliente vive DENTRO del dorado y lo engorda: medido, 39 MB -> 120 MB
	// en un servicio de node. Se cambia disco por latencia de despertar, y a partir
	// de unas decenas de servicios la cuenta puede no salir.
	warm := fs.Bool("warm", true, "ask the guest agent to start its runtime before freezing, if it supports it (bigger snapshot, much faster first wake)")
	// El mismo plazo que el daemon da a commit sin ready_timeout_seconds: con
	// menos, el CLI recortaba en silencio la espera a la sonda de la imagen.
	espera := fs.Duration("wait", machine.DefaultReadyWait, "how long to wait for the guest to serve (and to be ready by its image's probe) before saving")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling save [-replace] [-force] [-warm=false] <ref> <template-name>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	if !*force {
		if err := listoParaCongelar(ctx, c, fs.Arg(0), *espera, *warm); err != nil {
			return err
		}
	}

	// El daemon además espera a que el invitado esté listo según su imagen
	// (sonda y ganchos): un dorado a medio arrancar lo está en cada copia.
	snap, err := c.CommitWith(ctx, fs.Arg(0), api.CommitRequest{
		Name: fs.Arg(1), Replace: *replace, SkipReady: *force,
		ReadyTimeoutSeconds: int(espera.Seconds()),
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s  template  (%s of memory)\n", snap.Name, human(snap.MemBytes))
	for _, w := range snap.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	next("kling run -from %s", snap.Name)
	return nil
}

// listoParaCongelar exige que el invitado SIRVA antes de convertirse en dorado.
//
// `kling mcp import` ya hacia esta danza; `kling save` a secas no, y es el
// camino que documentamos para crear dorados a mano. El resultado era un snapshot
// que restaura en 26 ms y luego no contesta: la microVM arranca, el proceso del
// servidor no esta escuchando, y el gateway devuelve 502 tras esperar en balde.
//
// Ademas del puerto se pide el /reset del puente, que cierra las sesiones y mata
// los hijos: un dorado congelado en estado post-handshake rechaza el siguiente
// initialize con 400 "Server already initialized". Si el invitado no habla por
// puente (HTTP nativo) el /reset no existe y no es un fallo — el puerto abierto
// es cuanto se puede comprobar.
func listoParaCongelar(ctx context.Context, c *api.Client, ref string, espera time.Duration, warm bool) error {
	fmt.Printf("checking the guest is serving before freezing... ")
	if err := waitGuest(ctx, c, ref, espera); err != nil {
		fmt.Println("✗")
		if api.IsNotFound(err) {
			// No hay máquina: ni puerto ni plazo que explicar (fallaba en
			// milisegundos diciendo "not serving ... after 2m0s").
			return fmt.Errorf("%s: %w", ref, err)
		}
		return fmt.Errorf("%s: %w", mensajeNoSirve(ref, espera.String()), err)
	}
	// El /reset deja el servidor como recien arrancado. 404 = no hay puente, que
	// es legitimo; solo se informa de lo que se pudo comprobar.
	ruta := "/reset"
	if !warm {
		ruta = "/reset?warm=0"
	}
	resp, err := c.Guest(ctx, ref, api.GuestRequest{Path: ruta, Method: "POST"})
	switch {
	case err == nil && resp.Status == 204 && warm:
		fmt.Println("✓ (serving, session state reset, runtime prewarmed)")
	case err == nil && resp.Status == 204:
		fmt.Println("✓ (serving, session state reset, no prewarm)")
	default:
		fmt.Println("✓ (serving)")
	}
	return nil
}

// mensajeNoSirve explica la cadena entera: que pasa ahora, que pasaria al
// despertar, como diagnosticarlo y como saltarselo. Vive aparte para poder
// comprobar que no se queda a medias.
func mensajeNoSirve(ref, espera string) string {
	return fmt.Sprintf("the guest is not serving on port 8080 after %s\n"+
		"A golden snapshot of a server that is not listening restores fine and then "+
		"fails on wake with \"tool did not start listening\".\n"+
		"Check it with:  kling logs %s\n"+
		"Or freeze anyway with:  kling save -force ...", espera, ref)
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

	// La columna de carpetas solo aparece si alguna máquina tiene: quien no
	// las usa no paga el ancho, y los scripts que leen la tabla de siempre
	// siguen viendo la misma.
	conShares, conListo := false, false
	for _, mc := range list {
		if len(mc.Shares) > 0 {
			conShares = true
		}
		// Igual con READY: solo si alguna imagen declara sonda o ganchos, o
		// alguna arranca con impulso de CPU.
		if mc.Ready != "" || mc.CPUBoostPct > 0 {
			conListo = true
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	head := "ID\tNAME\tIMAGE/TEMPLATE\tSTATE\tCPU/MEM\tDISK\tEGRESS\tAGE\tLAST OP"
	if conListo {
		head += "\tREADY"
	}
	if conShares {
		head += "\tSHARES"
	}
	fmt.Fprintln(tw, head)
	var totalDisk int64
	var avisos []string
	for _, mc := range list {
		if !*all && (mc.State == api.StateStopped || mc.State == api.StateFailed) {
			continue
		}
		totalDisk += mc.DiskBytes
		avisos = append(avisos, avisosMaquina(mc)...)
		eg := mc.Egress
		if eg == "" {
			eg = "none"
		}
		// Una máquina instanciada de una plantilla se identifica por ella: la
		// imagen es un detalle de la plantilla, no de la máquina.
		origin := mc.Image
		if mc.From != "" {
			origin = mc.From
		}
		estado := string(mc.State)
		if mc.Hold != "" || mc.DiskErrors > 0 {
			estado += "!"
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%d/%dMiB\t%s\t%s\t%s\t%s",
			mc.ID[:12], mc.Name, origin, estado,
			mc.VCPUs, mc.MemMiB, human(mc.DiskBytes), eg, since(mc.CreatedAt), lastOp(mc))
		if conListo {
			listo := mc.Ready
			if listo == "" || mc.State != api.StateRunning {
				listo = "-"
			}
			// Hasta que termina de arrancar corre con más CPU que su techo
			// (internal/machine/arranque_cpu.go): que se vea.
			if mc.CPUBoostPct > 0 && mc.State == api.StateRunning {
				listo += fmt.Sprintf(" (cpu boost %d%%)", mc.CPUBoostPct)
			}
			row += "\t" + listo
		}
		if conShares {
			row += "\t" + sharesColumn(mc)
		}
		fmt.Fprintln(tw, row)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if totalDisk > 0 {
		fmt.Printf("\nmachines' own disk: %s (base image is shared)\n", human(totalDisk))
	}
	if len(avisos) > 0 {
		fmt.Println()
		for _, a := range avisos {
			fmt.Println("! " + a)
		}
	}
	return nil
}

// avisosMaquina son las líneas de aviso de mc bajo la tabla de ps: parada por
// el daemon (Hold) o con errores de disco en su invitado.
func avisosMaquina(mc *api.Machine) []string {
	var out []string
	if mc.Hold != "" {
		out = append(out, fmt.Sprintf("%s: on hold (%s); it resumes on its own once there is room (kling cow grow)", mc.Name, mc.Hold))
	}
	if mc.DiskErrors > 0 {
		cuando := ""
		if mc.DiskErrorAt != nil {
			cuando = ", last " + since(*mc.DiskErrorAt) + " ago"
		}
		out = append(out, fmt.Sprintf("%s: its guest got %d disk I/O error(s)%s: %q; its data may be damaged", mc.Name, mc.DiskErrors, cuando, mc.DiskError))
	}
	return out
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
	var force *bool
	if op == "rm" {
		force = fs.Bool("f", false, "do not ask for confirmation")
	}
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling %s <ref>...", op)
	}
	if op == "rm" && !*force && !confirmMany("machine", fs.Args()) {
		return errAborted
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
		case "pause":
			mc, err = c.Pause(ctx, ref)
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
			fmt.Printf("%s  frozen  (%d ms, %d MiB on disk)\n", mc.ID[:12], mc.FreezeMS, mc.SnapSize>>20)
		case op == "thaw":
			fmt.Printf("%s  running  (%d ms)%s\n", mc.ID[:12], mc.ThawMS, wakeNote(mc.Wake))
		default:
			fmt.Printf("%s  %s\n", mc.ID[:12], mc.State)
		}
	}
	return nil
}

// cmdStart es `kling start [-e K=V|K] [-env-file F] <ref>...`: arranca otra
// vez una máquina parada, en frío sobre su propio disco. El entorno de -e no
// se guarda (solo sus nombres), así que hay que volver a darlo; el daemon dice
// qué claves faltan.
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	host := hostFlag(fs)
	var ef envFlags
	ef.register(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling start [-e K=V|K] [-env-file F] <ref>...")
	}
	env, err := ef.resolve()
	if err != nil {
		return err
	}
	if _, err := api.MachineEnvMap(env); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	for _, ref := range fs.Args() {
		mc, err := c.Start(ctx, ref, env)
		if err != nil {
			// Un daemon anterior no tiene la ruta: el 404 del enrutador no
			// dice nada útil.
			var se *api.StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				if info, ierr := c.Info(ctx); ierr == nil && !slices.Contains(info.Capabilities, api.CapabilityStart) {
					return fmt.Errorf("the daemon (%s) can't start a stopped machine: update it", info.Version)
				}
			}
			return err
		}
		fmt.Printf("%s  %s  (cold boot from its disk, %d ms)\n", mc.ID[:12], mc.State, mc.BootMS)
	}
	return nil
}

// cmdSqueeze aprieta el globo de una o varias microVMs running para devolver al
// host la RAM que el invitado tiene libre, sin congelarlas. A diferencia de
// freeze, la máquina sigue viva y atendiendo: es el ahorro barato entre sesiones.
func cmdSqueeze(args []string) error {
	fs := flag.NewFlagSet("squeeze", flag.ExitOnError)
	host := hostFlag(fs)
	force := fs.Bool("force", false, "squeeze also a copy that shares memory with its template (Firecracker): usually makes the host use MORE memory")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling machine squeeze <ref>...")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	for _, ref := range fs.Args() {
		res, err := c.SqueezeWith(ctx, ref, *force)
		if err != nil {
			return err
		}
		fmt.Printf("%s  ~%d MiB returned to the host  (guest free %d MiB, RSS now %d MiB)\n",
			res.ID[:12], res.ReclaimedMiB, res.GuestFreeMiB, res.RSSMiB)
	}
	return nil
}

// cmdMMDS es `kling machine secret` (antes `mmds`): inyecta secretos comunes
// en una microVM viva por MMDS. El store es un documento JSON que se lee de -f
// o de stdin. Es sobre todo para pruebas en el lab: en producción quien inyecta
// es el gateway al resolver una máquina.
//
// Esquema del store (lo entiende el bridge de dentro):
//
//	{
//	  "env": { "VAR_COMUN": "valor" }
//	}
//
// El campo "sessions" está RETIRADO (no es seguro): se sigue aceptando para
// atrás-compatibilidad, pero se ignora. Ver pkg/guest/mmds.go para alternativas
// seguras (credenciales de plantilla, proxy de credenciales, VM efímera).
//
// El secreto NO viaja por la línea de comandos (cualquiera lee /proc/<pid>/cmdline):
// se lee de un fichero o de la entrada estándar.
func cmdMMDS(args []string) error {
	fs := flag.NewFlagSet("mmds", flag.ExitOnError)
	host := hostFlag(fs)
	file := fs.String("f", "", "JSON file with the MMDS store (default: stdin)")
	hooks := fs.Bool("hooks", false, "then run the image's post-restore hooks and wait for them (they read the secret)")
	hooksWait := fs.Duration("hooks-wait", time.Minute, "with -hooks, how long to wait for them")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling machine secret <ref> [-f store.json] [-hooks]  (reads stdin if no -f)")
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

	c := api.NewClient(hostOf(*host))
	mc, err := c.PutMMDS(ctx, fs.Arg(0), data)
	if err != nil {
		return err
	}
	if *hooks {
		res, err := c.RunHooks(ctx, fs.Arg(0), *hooksWait)
		if err != nil {
			return err
		}
		if !res.OK() {
			detalle := ""
			if res.Guest != nil {
				detalle = ": " + res.Guest.Detail
			}
			return fmt.Errorf("%s: post-restore hooks did not finish (%s)%s", mc.Name, res.Ready, detalle)
		}
		fmt.Printf("%s  post-restore hooks done in %d ms\n", mc.ID[:12], res.WaitedMS)
	}
	if !mc.HasSecrets {
		fmt.Printf("%s  MMDS store emptied (no secrets: it can be frozen)\n", mc.ID[:12])
		return nil
	}
	if vacio := strings.Join(strings.Fields(string(data)), ""); vacio == "{}" || vacio == "null" {
		fmt.Printf("%s  MMDS store emptied, still marked with secrets: no post-restore hook consumed them "+
			"after the last injection (use -hooks when injecting)\n", mc.ID[:12])
		return nil
	}
	fmt.Printf("%s  secrets injected via MMDS (can no longer be frozen)\n", mc.ID[:12])
	return nil
}

// cmdCredential es `kling machine credential`: entrega una clave al proxy de
// credenciales de una máquina con egress allowlist. La clave no entra en el
// invitado; en la variable -env recibe un marcador que el proxy cambia por la
// clave solo en peticiones a http://<dominio> (el SDK debe usar http://, el
// proxy sale por HTTPS). Como en cmdMMDS, la clave NO viaja por la línea de
// comandos: se lee de -f o de stdin.
func cmdCredential(args []string) error {
	fs := flag.NewFlagSet("credential", flag.ExitOnError)
	host := hostFlag(fs)
	cf := credentialFlags(fs)
	rm := fs.Bool("rm", false, "withdraw the credential in -env from the machine (its placeholder stops working; open postgres sessions that used it are cut)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *rm {
		if fs.NArg() < 1 || *cf.env == "" {
			return fmt.Errorf("usage: kling machine credential -rm <ref> -env NAME")
		}
		ctx, stop := ctxWithSignals()
		defer stop()
		mc, err := api.NewClient(hostOf(*host)).RemoveCredential(ctx, fs.Arg(0), *cf.env, "")
		if err != nil {
			return err
		}
		fmt.Printf("%s  %s withdrawn from the credential proxy\n", mc.ID[:12], *cf.env)
		return nil
	}
	if fs.NArg() < 1 || *cf.domain == "" || *cf.env == "" {
		return fmt.Errorf("usage: kling machine credential <ref> -domain api.example.com -env API_KEY [-allow-request 'GET /v1/balance']... [-header X-Name]... [-query] [-body] [-f keyfile]  (reads stdin if no -f)\n" +
			"       kling machine credential <ref> -type postgres -domain db.example.com -user app (-database appdb | -any-database) [-port 5432] [-ca-file ca.pem] [-upstream host:port] [-upstream-tls verify-full|disable] [-tls-server-name N] -env PGPASSWORD [-f passfile]\n" +
			"       kling machine credential <ref> -type mysql -domain db.example.com -user app (-database appdb | -any-database) [-port 3306] [...same as postgres] -env MYSQL_PWD [-f passfile]\n" +
			"       kling machine credential -rm <ref> -env NAME")
	}
	spec, err := cf.spec()
	if err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	mc, err := api.NewClient(hostOf(*host)).SetCredentials(ctx, fs.Arg(0), api.CredentialsRequest{
		Credentials: []api.CredentialSpec{spec},
	})
	if err != nil {
		return err
	}
	if spec.Type == credproxy.KindPostgres || spec.Type == credproxy.KindMySQL {
		fmt.Printf("%s  %s now holds a placeholder; the password only goes to %s through the proxy\n",
			mc.ID[:12], spec.Env, pgDestino(spec))
		fmt.Printf("      %s\n", pgConexion(spec))
		fmt.Printf("      the password survives freeze/thaw and daemon restarts\n")
		return nil
	}
	fmt.Printf("%s  %s now holds a placeholder; the key only goes to https://%s through the proxy\n",
		mc.ID[:12], spec.Env, strings.ToLower(spec.Domain))
	fmt.Printf("      %s\n", describirAllow(spec.Allow))
	fmt.Printf("      %s\n", describirSitios(spec))
	fmt.Printf("      point the SDK at http://%s (the proxy adds TLS); the key survives freeze/thaw and daemon restarts\n",
		strings.ToLower(spec.Domain))
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
			fmt.Printf("   %s◆ %-16s template · %s shared memory\n",
				branch, g, human(snap.MemBytes))
		}

		list := byFrom[g]
		for mi, mc := range list {
			mbranch := "├──"
			if mi == len(list)-1 {
				mbranch = "└──"
			}
			ip := mc.IP
			// En macOS la IP es la misma para todos los invitados: lo que
			// distingue a cada uno es su reenvío en loopback.
			if len(mc.Forwards) > 0 {
				ip = mc.Addr(api.GuestPort)
			}
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

	fmt.Printf("\n  %d running · %d frozen · %d stopped   disk: %s own + %s shared\n",
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

// writeInfo son los detalles del daemon: lo que era `kling status -v` y ahora
// enseña `kling status -v`.
func writeInfo(c *api.Client, i *api.Info) {
	kvm := "no"
	if i.KVM {
		kvm = "yes"
	}
	fmt.Printf("endpoint:     %s\n", c.Endpoint())
	fmt.Printf("daemon:       %s\n", i.Version)
	fmt.Printf("root:         %s\n", i.Root)
	backend := i.Backend
	if backend == "" {
		backend = "firecracker" // daemon anterior a v0.9: siempre lo era
	}
	fmt.Printf("backend:      %s\n", backend)
	if i.Arch != "" {
		fmt.Printf("arch:         %s\n", i.Arch)
	}
	if backend == "firecracker" {
		fmt.Printf("KVM:          %s\n", kvm)
	}
	// El campo se llama firecracker por historia; es la versión del VMM, sea cual sea.
	vmm := strings.TrimSpace(i.Firecrack)
	if vmm == "" {
		vmm = "(not found: the daemon could not run it)"
	}
	fmt.Printf("%-14s%s\n", backend+":", vmm)
	fmt.Printf("machines:     %d\n", i.Machines)
	if len(i.Capabilities) > 0 {
		fmt.Printf("capabilities: %s\n", strings.Join(i.Capabilities, ", "))
	}
	if i.Has("shares-live") {
		roots := "none (live shares disabled; see daemon.share_roots)"
		if len(i.ShareRoots) > 0 {
			roots = strings.Join(i.ShareRoots, ", ")
		}
		fmt.Printf("share roots:  %s\n", roots)
	}
	if l := lineaCoW(i.CoW); l != "" {
		fmt.Printf("disk clones:  %s\n", l)
	}
	if i.EncryptedAtRest != nil {
		if *i.EncryptedAtRest {
			fmt.Printf("at rest:      encrypted (dm-crypt)\n")
		} else {
			fmt.Printf("at rest:      NOT encrypted: snapshots hold guest memory in clear; see docs/cifrado.md\n")
		}
	}
	if a := i.Authz; a != nil {
		if a.Enabled {
			fmt.Printf("authz:        policy on; you are %s\n", a.Role)
		} else {
			fmt.Printf("authz:        no policy (whoever reaches the socket controls everything; see docs/authz.md)\n")
		}
	}
}

// cmdResize es `kling machine resize <ref> -mem N`: sube o baja la memoria de
// una máquina sin reiniciarla, dentro del techo con el que arrancó.
func cmdResize(args []string) error {
	fs := flag.NewFlagSet("machine resize", flag.ExitOnError)
	host := hostFlag(fs)
	mem := units.MiBVar(fs, "mem", 0, "new memory: 512M, 2G (bare number = MiB)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *mem <= 0 {
		return fmt.Errorf("usage: kling machine resize <machine> -mem 512M")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	mc, err := api.NewClient(hostOf(*host)).Resize(ctx, fs.Arg(0), *mem)
	if err != nil {
		return err
	}
	fmt.Printf("%s  %d MiB (ceiling %d)\n", mc.Name, mc.MemMiB, mc.MemMaxMiB)
	return nil
}

// wakeNote es el desglose de un despertar para `kling thaw`: el total que
// esperó la llamada y sus fases más caras (docs/despertar.md).
func wakeNote(p *api.WakePhases) string {
	if p == nil {
		return ""
	}
	// La memoria de una copia en diferencial (almacén, espejo y montaje) va
	// aparte: el primer thaw de una copia puede pasar ahí casi todo.
	mem := ""
	if m := p.StoreMS + p.MirrorMS + p.MemoryMS; m > 0 {
		mem = fmt.Sprintf(", memory %.1f (store %.1f, mirror %.1f)", m, p.StoreMS, p.MirrorMS)
	}
	return fmt.Sprintf("  %s wake %.1f ms: net %.1f, spawn %.1f, socket %.1f, load %.1f, resync %.1f%s, other %.1f",
		p.Tier, p.TotalMS, p.NetMS, p.SpawnMS, p.SocketMS, p.LoadMS, p.ResyncMS, mem,
		p.WaitMS+p.CheckMS+p.ForwardsMS+p.CgroupMS+p.FinishMS)
}
