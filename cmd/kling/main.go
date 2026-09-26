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
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/daemon"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
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
	// Los nombres de antes se traducen en silencio a los de ahora (tree.go).
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
	case "inspect":
		err = cmdInspect(args)
	case "ps":
		err = cmdPS(args)
	case "logs":
		err = cmdLogs(args)
	case "freeze", "thaw", "pause", "stop", "rm":
		err = cmdLifecycle(cmd, args)
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
// `kling machine resize|squeeze|secret`. Los nombres de antes (resize,
// squeeze, mmds) son alias.
func cmdMachine(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling machine <resize|squeeze|secret> <ref> [...]")
	}
	switch args[0] {
	case "resize":
		return cmdResize(args[1:])
	case "squeeze":
		return cmdSqueeze(args[1:])
	case "secret", "mmds":
		return cmdMMDS(args[1:])
	}
	return fmt.Errorf("unknown subcommand %q: use resize, squeeze or secret", args[0])
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
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	backend, err := daemonBackend(loadConfig().Daemon.VMM, os.Getenv("KLING_VMM"), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	vmm := machine.ResolverVMM(backend, os.Getenv("KLING_VMM"), *fcBin)

	daemon.Version = strings.TrimPrefix(Version, "v")
	srv, err := daemon.New(*socket, *root, vmm, *sockUser, *runAs)
	if err != nil {
		return err
	}
	srv.SetShareConfig(shareConfig)
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
	egress := fs.String("egress", "", "network egress: none | internet | allowlist (never reaches private networks)")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist (comma-separated)")
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
	var labels labelFlag
	fs.Var(&labels, "label", "key=value label (repeatable)")
	var shares shareFlag
	fs.Var(&shares, "share", shareUsage)
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
	mc, err := client.Run(ctx, api.RunRequest{
		Name:  *name,
		From:  *from,
		Image: config.Or(*image, cfg.Defaults.Image, "default"),
		// El flag gana; si no se dio, manda la configuración; y si tampoco,
		// el valor incorporado.
		VCPUs:        config.Or(*cpus, cfg.Defaults.VCPUs, 1),
		MemMiB:       config.Or(*mem, cfg.Defaults.MemMiB, 256),
		MemMaxMiB:    *memMax,
		Egress:       config.Or(*egress, cfg.Defaults.Egress, "none"),
		AllowDomains: splitDomains(*allow),
		TTLSeconds:   config.Or(*ttl, cfg.Defaults.TTL),
		CPUPct:       config.Or(resolveCPUPct(fs, *cpuPct, *cpu), cfg.Defaults.CPUPct),
		Labels:       labels.merge(*service),
		// El volumen es una propiedad de la MÁQUINA, no solo de un servicio MCP:
		// arrancar una a mano con almacenamiento que sobreviva es tan legítimo
		// como importar un servicio con él.
		Volumes:   vols,
		Shares:    shareSpecs,
		AllowExec: *allowExec,
		OnTTL:     *onTTL,
	})
	if err != nil {
		return err
	}
	if mc.From != "" {
		fmt.Printf("%s  %s  instantiated from %s in %d ms\n", mc.ID[:12], mc.Name, mc.From, mc.ThawMS)
	} else {
		fmt.Printf("%s  %s  booted cold in %d ms\n", mc.ID[:12], mc.Name, mc.BootMS)
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
	espera := fs.Duration("wait", 60*time.Second, "how long to wait for the guest to serve before committing")
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

	snap, err := c.Commit(ctx, fs.Arg(0), fs.Arg(1), *replace)
	if err != nil {
		return err
	}
	fmt.Printf("%s  template  (%s of memory)\n", snap.Name, human(snap.MemBytes))
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
	conShares := false
	for _, mc := range list {
		if len(mc.Shares) > 0 {
			conShares = true
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	head := "ID\tNAME\tIMAGE/TEMPLATE\tSTATE\tCPU/MEM\tDISK\tEGRESS\tAGE\tLAST OP"
	if conShares {
		head += "\tSHARES"
	}
	fmt.Fprintln(tw, head)
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
		// Una máquina instanciada de una plantilla se identifica por ella: la
		// imagen es un detalle de la plantilla, no de la máquina.
		origin := mc.Image
		if mc.From != "" {
			origin = mc.From
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%d/%dMiB\t%s\t%s\t%s\t%s",
			mc.ID[:12], mc.Name, origin, mc.State,
			mc.VCPUs, mc.MemMiB, human(mc.DiskBytes), eg, since(mc.CreatedAt), lastOp(mc))
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
		return fmt.Errorf("usage: kling machine squeeze <ref>...")
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

// cmdMMDS es `kling machine secret` (antes `mmds`): inyecta un secreto de
// sesión en una microVM viva por MMDS. El store es
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
		return fmt.Errorf("usage: kling machine secret <ref> [-f store.json]  (reads stdin if no -f)")
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
	if i.EncryptedAtRest != nil {
		if *i.EncryptedAtRest {
			fmt.Printf("at rest:      encrypted (dm-crypt)\n")
		} else {
			fmt.Printf("at rest:      NOT encrypted: snapshots hold guest memory in clear; see docs/cifrado.md\n")
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
	return fmt.Sprintf("  %s wake %.1f ms: net %.1f, spawn %.1f, socket %.1f, load %.1f, resync %.1f, other %.1f",
		p.Tier, p.TotalMS, p.NetMS, p.SpawnMS, p.SocketMS, p.LoadMS, p.ResyncMS,
		p.WaitMS+p.CheckMS+p.ForwardsMS+p.CgroupMS+p.FinishMS)
}
