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
	"github.com/juan52878911/kindling/pkg/transport"

	"errors"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const usageHead = `kling - Firecracker microVMs with a docker-style interface

USAGE
  kling <command> [options]

GETTING STARTED
  up                                               gets the runtime ready: KVM,
                                                   nftables, user, images,
                                                   daemon and extension units
  status [-json]                                   which piece is up and which is missing

VOLUMES
  volume create <name> [-size 2G]                  storage that survives
                                                   the microVM
  volume ls [-json] | rm <name>                    list / remove
  volume populate <name> [-image I] -- <cmd>       installs packages inside a microVM
  images ls [-json]                                lists built rootfs images
  images toolchain                                 builds the image with npm and pip (used by populate)
  images recipe <image>                            how it was built
  images build <name> -builder B [-spec f.json]    builds it with a builder installed
                                                   on the daemon (extensions ship them)
  images cat <image> <path> [-stat]                prints a file inside an image
  images put <image> <path> (-file F|-from-host N) puts a file inside a built image
      [-mode 0755] [-create]
  images rm <image>                                removes it (refuses if a layer,
                                                   a golden or a machine uses it)
  images copy <image> -from H [-to H]              streams an image (and its kernel and
                                                   base) from one daemon to another

MACHINES
  run [-name N] [-image I] [-cpus N] [-mem MiB]    creates and starts a microVM
      [-egress none|internet|allowlist]            network egress (default: none)
      [-allow dom1,dom2]                           domains allowed with allowlist
      [-ttl SECONDS] [-cpu-pct PCT]                auto-freeze and CPU ceiling
      [-service NAME] [-label k=v]                 grouping by service
      [-volume NAME[:/mount][:ro]] (repeatable)    storage that survives the machine
      [-allow-exec] [-on-ttl freeze|remove]        accept exec/cp; remove instead of freezing
      [-mem-max MiB]                               ceiling for kling resize
      [-share SRC:DST[:copy|ro|rw]] (repeatable)   host folder inside: a read-only copy
                                                   (default), or live (docs/compartir.md)
  ps [-a] [-q] [-json]                             lists the machines
  inspect <ref>                                    a machine in JSON, shares included
  logs <ref> [-tail N]                             microVM serial console
  freeze <ref>                                     freezes into a snapshot -> warm
  thaw <ref>                                       restores from snapshot (~ms)
  stop <ref>                                       terminates the machine
  rm <ref>                                         removes machine and snapshot
  resize <ref> -mem MiB                            changes its memory without restarting,
                                                   up to the -mem-max it was started with
  squeeze <ref>...                                 balloon: returns the guest's
                                                   free memory to the host
  mmds <ref> [-f store.json]                       injects a session secret via
                                                   MMDS (reads stdin if no -f); the
                                                   machine can no longer be frozen

SANDBOXES AND EXEC
  sandbox create [-image I | -from S] [-ttl 10m]   a throwaway microVM that runs code:
      [-egress none|internet|allowlist]            no network by default; when idle
      [-on-ttl remove|freeze]                      it is destroyed, or frozen at zero
      [-mem MiB] [-cpus N] [-volume ...] [-q]      cost and woken by the next exec
      [-share SRC:DST[:copy|ro|rw]]                host folder inside, as in run
  sandbox ls | renew <sb> [-ttl D] | rm <sb>...    list / extend / destroy
  exec [-i] [-e K=V] [-w DIR] [-timeout D]         runs a command inside, streaming its
      <ref> [--] <cmd> [args...]                   output; exits with its exit code
  cp <local|-> <ref>:<path>                        copies a file into a machine
  cp <ref>:<path> <local|->                        ... or out of it
  shell [-e K=V] [-w DIR] [-t TERM] <ref>          interactive terminal inside
      [--] [cmd [args...]]                         (Ctrl-C reaches the program)

GOLDEN SNAPSHOTS
  commit [-replace] <ref> <name>                   freezes a machine as a
                                                   reusable snapshot
  run -from <name>                                 instantiates from the snapshot
  snapshots                                        lists the snapshots
  rmi <name>                                       removes a snapshot

MODELS (VON: small LLMs, OpenAI-compatible API on port 8000)
  models ls [-json]                                catalog and the models on this daemon
  models add <name> -model ID [-quant Q]           builds the image (llama.cpp + GGUF) and a
      [-ctx N] [-cpus N] [-mem MiB] [-replace]     golden snapshot with the model loaded and
      [-url HF_URL -sha256 H] [-rebuild]           warm; serve it with run -from <name>
      [-build-only]                                only the image (to copy it to macOS)
      [-prefix system.txt]... [-cache-ram MiB]     leaves task prompts evaluated in the golden
  models ask <ref> [-max-tokens N] <prompt...>     asks a replica, prints answer and tok/s
  models rm <name> [-keep-image]                   removes its snapshot and image

OBSERVATION
  topo                                             ASCII diagram of everything
  top [-watch DUR] [-json]                         memory per microVM (PSS) and
                                                   the host; snapshot or refresh
  events                                           stream of daemon events
  info [-json]                                     daemon status

SMALL MODELS (JEV, runs locally, no daemon)
  jev train -data d.jsonl -o m.jev [-valid v]      tiny linear classifier: trains,
                                                   quantizes, calibrates, picks τ
  jev eval -model m.jev -data t.jsonl [-json]      accuracy, F1, ECE, coverage at τ
  jev predict -model m.jev [-text T] [-top N]      label, calibrated p, confident or
                                                   escalate, evidence (docs/jev.md)
  jev inspect <m.jev> [-json]                      spec, labels, thresholds, metadata
  domotica decide [-lang L] "<text>"               smart-home decision: demo templates →
                                                   JEV intent + slots, or escalate
  domotica eval -data t.jsonl [-challenge]         accuracy, slot F1, exact match, latency
  domotica train-slots -data d.jsonl -o m.jevs     trains the slot tagger (docs/domotica.md)
  domotica templates [-lang L]                     lists the predefined demo commands
  domotica eval-llm -von G -data t.jsonl           layer 4 (VON LLM) vs doing nothing on what
                                                   escalates; its record enables layer 4

`

const usageTail = `AI GATEWAY (JEV classifies, VON generates, models on demand; docs/ai-gateway.md)
  ai serve [-config ai.json] [-socket S]           serves /v1/classify, /v1/decide,
      [-listen ADDR] [-idle 2m] [-max-replicas 2]  /v1/generate and an OpenAI API; replicas
      [-keepwarm N] [-jev-mem MiB]                 wake per request and freeze when idle
  ai ls [-json]                                    models, tasks, cascades, samples
  ai test <task> [-mode cascade|jev] <text>        classifies one text through the gateway
  ai generate <task> [-var k=v] [<input>]          runs a generation task (stdin if no input)
  ai eval <task> -data t.jsonl [-von M]            JEV alone vs the JEV -> VON cascade on
                                                   labelled data; the record gates escalate_to
  ai calibrate <task> [-target P] [-dry-run]       re-tunes JEV thresholds on recent VON
                                                   answers; writes only if it improves
  ai reload                                        rereads the registry (says which cascades
                                                   are on, forced or refused)
  ai prime [<model>...] [-dry-run]                 remakes each VON golden snapshot with its
                                                   tasks' prompt prefixes already evaluated

DAEMON
  daemon [-socket S] [-root R] [-firecracker BIN]  starts the core (VMM: config daemon.vmm,
                                                   or $KLING_VMM with a name or a path)

CONFIGURATION
  context [ls]                                     lists known daemons
  context add <name> <host>                        adds one and activates it
  context use <name>                               switches daemon
  context rm <name>                                removes it
  config [show|path]                               current configuration
  config set <key> <value>                         e.g. defaults.image min
  completion [bash|zsh]                            shell completion script
  version                                          CLI version

EXTENSIONS
  plugins [ls] [-json]                             installed extensions (kling-<name>
                                                   binaries) and what they add

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
		printUsage(os.Stdout)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "daemon":
		err = cmdDaemon(args)
	case "up":
		err = cmdUp(args)
	case "status":
		err = cmdStatus(args)
	case "volume", "volumes":
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
			fmt.Fprintln(os.Stderr, "error:", xerr)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	case "shell":
		// Como exec: el código es el de la shell remota.
		code, xerr := cmdShell(args)
		if xerr != nil {
			fmt.Fprintln(os.Stderr, "error:", xerr)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	case "cp":
		err = cmdCp(args)
	case "sandbox", "sandboxes":
		err = cmdSandbox(args)
	case "inspect":
		err = cmdInspect(args)
	case "ps":
		err = cmdPS(args)
	case "logs":
		err = cmdLogs(args)
	case "freeze", "thaw", "stop", "rm":
		err = cmdLifecycle(cmd, args)
	case "squeeze":
		err = cmdSqueeze(args)
	case "resize":
		err = cmdResize(args)
	case "mmds":
		err = cmdMMDS(args)
	case "commit":
		err = cmdCommit(args)
	case "snapshots":
		err = cmdSnapshots(args)
	case "models":
		err = cmdModels(args)
	case "images":
		err = cmdImages(args)
	case "rmi":
		err = cmdRmi(args)
	case "topo":
		err = cmdTopo(args)
	case "top":
		err = cmdTop(args)
	case "events":
		err = cmdEvents(args)
	case "info":
		err = cmdInfo(args)
	case "context":
		err = cmdContext(args)
	case "config":
		err = cmdConfig(args)
	case "completion":
		err = cmdCompletion(args)
	case "version", "--version", "-v":
		fmt.Printf("kling %s\n", Version)
		return
	case "plugins":
		err = cmdPlugins(args)
	case "jev":
		err = cmdJev(args)
	case "domotica":
		err = cmdDomotica(args)
	case "ai":
		err = cmdAI(args)
	case "builder": // lo ejecuta el daemon como root; ver builder.go
		err = cmdBuilder(args)
	case "-h", "--help", "help":
		if len(args) > 0 {
			err = helpFor(args[0])
			break
		}
		printUsage(os.Stdout)
		return
	default:
		// Lo que no es del núcleo lo sirve una extensión: una incorporada corre
		// aquí mismo; una externa reemplaza este proceso y no vuelve.
		if p := extensions().Lookup(cmd); p != nil {
			err = plugin.Exec(p, cmd, args, config.Path())
			break
		}
		if hint, ok := movedToExtension[cmd]; ok {
			fmt.Fprintf(os.Stderr, "kling %s %s\n", cmd, hint)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(codigoDeSalida(err))
	}
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
	mem := fs.Int("mem", 0, "memory in MiB (default: 256)")
	memMax := fs.Int("mem-max", 0, "ceiling for resizing its memory later without restarting (kling resize)")
	egress := fs.String("egress", "", "network egress: none | internet | allowlist (never reaches private networks)")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist (comma-separated)")
	ttl := fs.Int("ttl", 0, "seconds until it freezes itself (0 = never)")
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
	// Reemplazar es opt-in: los snapshots quedan atados al TSC del host y un
	// reinicio los invalida todos, así que rehacerlos con el mismo nombre es
	// rutina — pero pisar uno por un nombre repetido sin querer no debe poder
	// pasar, y por eso no es el comportamiento por defecto.
	replace := fs.Bool("replace", false, "replace the snapshot if one with this name already exists")
	// Congelar un servidor que no sirve produce un snapshot que NO sirve, y el
	// fallo no aparece hasta que alguien lo despierta —minutos u horas despues—
	// con un "tool did not start listening" que no menciona el commit. Por eso
	// se comprueba antes, y por eso saltarselo es explicito.
	force := fs.Bool("force", false, "commit even if the guest is not serving (produces a snapshot that may not work)")
	// El hijo caliente vive DENTRO del dorado y lo engorda: medido, 39 MB -> 120 MB
	// en un servicio de node. Se cambia disco por latencia de despertar, y a partir
	// de unas decenas de servicios la cuenta puede no salir.
	warm := fs.Bool("warm", true, "ask the guest agent to start its runtime before freezing, if it supports it (bigger snapshot, much faster first wake)")
	espera := fs.Duration("wait", 60*time.Second, "how long to wait for the guest to serve before committing")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling commit [-replace] [-force] [-warm=false] <ref> <snapshot-name>")
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
	fmt.Printf("%s  golden snapshot  (%s of memory)\n", snap.Name, human(snap.MemBytes))
	fmt.Printf("instantiate with:  kling run -from %s\n", snap.Name)
	return nil
}

// listoParaCongelar exige que el invitado SIRVA antes de convertirse en dorado.
//
// `kling mcp import` ya hacia esta danza; `kling commit` a secas no, y es el
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
		"Or freeze anyway with:  kling commit -force ...", espera, ref)
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
	head := "ID\tNAME\tIMAGE\tSTATE\tCPU/MEM\tDISK\tEGRESS\tAGE\tLAST OP"
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
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%d/%dMiB\t%s\t%s\t%s\t%s",
			mc.ID[:12], mc.Name, mc.Image, mc.State,
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
	return nil
}

// movedToExtension son comandos que hasta v0.5 traía el núcleo y ahora aporta
// una extensión. Quien actualiza kindling sin instalarla teclea lo de siempre, y
// volcarle la ayuda entera no le dice qué le falta.
var movedToExtension = func() map[string]string {
	const mcp = "is provided by the kindling-mcp extension since kindling v0.6. Install it with:\n" +
		"  curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling-mcp/main/scripts/install.sh | sh"
	m := map[string]string{}
	for _, c := range []string{"mcp", "add", "search", "connect", "export", "memory", "migrate", "gateway"} {
		m[c] = mcp
	}
	return m
}()

// cmdResize es `kling resize <ref> -mem N`: sube o baja la memoria de una
// máquina sin reiniciarla, dentro del techo con el que arrancó.
func cmdResize(args []string) error {
	fs := flag.NewFlagSet("resize", flag.ExitOnError)
	host := hostFlag(fs)
	mem := fs.Int("mem", 0, "new memory in MiB")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *mem <= 0 {
		return fmt.Errorf("usage: kling resize <machine> -mem MiB")
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
