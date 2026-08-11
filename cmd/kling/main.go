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

const usage = `kling - microVMs de Firecracker con interfaz tipo docker

USO
  kling <comando> [opciones]

EMPEZAR
  up                                               deja el runtime listo: KVM,
                                                   nftables, usuario, imágenes,
                                                   daemon y gateway
  status                                           qué pieza está y cuál falta

VOLÚMENES
  volume create <n> [-size 2G]                     almacenamiento que sobrevive
                                                   a la microVM
  volume ls | rm <n>                               listar / eliminar
  volume populate <n> [-image I] -- <cmd>           instala paquetes dentro de una microVM
  images refresh [imagen...]                       pone el puente actual dentro de las imágenes
  images toolchain                                 construye la imagen con npm y pip (la usa populate)
  images recipe <imagen>                           cómo se construyó

MÁQUINAS
  run [-name N] [-image I] [-cpus N] [-mem MiB]   crea y arranca una microVM
      [-egress none|internet]                      salida de red (por defecto: none)
      [-ttl SEGUNDOS] [-cpu PCT]                   congelado automático y techo de CPU
      [-service NOMBRE] [-label k=v]               agrupación por servicio MCP
      [-volume NOMBRE[:/punto][:ro]] (repetible)   almacenamiento que sobrevive a la máquina
  ps [-a]                                          lista las máquinas
  logs <ref> [-tail N]                             consola serie de la microVM
  freeze <ref>                                     congela en snapshot -> warm
  thaw <ref>                                       restaura desde snapshot (~ms)
  stop <ref>                                       termina la máquina
  rm <ref>                                         elimina máquina y snapshot

CATÁLOGO
  search <consulta>                                busca en el registro oficial
                                                   de servidores MCP
  add <servidor> [-as nombre] [-arg valor]         lo empaqueta, lo importa y lo
      [-volume NOMBRE[:/punto][:ro]] (repetible)   deja congelado como servicio

SERVICIOS MCP
  mcp import <servicio> -image <img>               convierte un servidor MCP en
      [-cpus N] [-mem MiB]                         servicio: arranca, pregunta
      [-egress none|internet]                      qué sabe hacer, lo congela y
      [-volume NOMBRE[:/punto][:ro]] (repetible)   guarda su catálogo. Todo esto
                                                   queda GRABADO en el snapshot
  mcp list [-v]                                    servicios y sus herramientas
  mcp refresh <servicio>                           vuelve a capturar el catálogo
  mcp link <nombre> <url>                          enlaza un servidor MCP EXTERNO
                                                   (p. ej. tu engram) sin meterlo
                                                   en una microVM
  mcp unlink <nombre>                              lo desenlaza

SNAPSHOTS DORADOS
  commit <ref> <nombre>                            congela una máquina como
                                                   snapshot reutilizable
  run -from <nombre>                               instancia desde el snapshot
  snapshots                                        lista los snapshots
  rmi <nombre>                                     elimina un snapshot

OBSERVACIÓN
  topo                                             diagrama ASCII de todo
  export [-o fichero.html]                         topología navegable en HTML
  events                                           stream de eventos del daemon
  info                                             estado del daemon

MEMORIA DE USO (opcional, apagada por defecto)
  memory status                                    si está activa y sobre qué
  memory enable [-service N]                       la activa; usa engram por defecto
  memory disable                                   la apaga
  memory install-service                           deja el puente local como
                                                   servicio permanente (macOS)

CONECTAR CON TU AGENTE
  connect                                          guía paso a paso
  connect -all                                     UNA entrada para todos los
                                                   servicios: inventario en el
                                                   handshake, esquemas a demanda
  connect -all -only eco,files                     solo esos servicios
  connect -all -expand                             catálogo completo (gasta más
                                                   contexto)
  connect <servicio>                               un servicio suelto
  connect ... -install all                         escribe en TODOS los agentes
                                                   detectados: Claude Code,
                                                   opencode, Cursor, VS Code,
                                                   Windsurf, Cline y Zed
  connect ... -install <cliente>                   solo en ese
  connect ... -token T                             usa ese token en vez del de
                                                   gateway.token

GATEWAY
  gateway [-listen ADDR] [-idle DUR] [-ephemeral]  enruta llamadas MCP a microVMs
                                                   bajo demanda. Con -ephemeral,
                                                   cada acción corre en su propia
                                                   máquina, que muere al terminar
          [-prewarm N]                             instancias listas por servicio
          [-memory SVC]                            servicio de memoria del agente
          [-no-auth] [-pprof]                      sin token / con perfiles; las
                                                   dos exigen escuchar en
                                                   loopback. Por defecto pide
                                                   Authorization: Bearer con el
                                                   token de gateway.token, que se
                                                   genera solo la primera vez

DAEMON
  daemon [-socket S] [-root R] [-firecracker BIN]  arranca el núcleo

CONFIGURACIÓN
  context [ls]                                     lista los daemons conocidos
  context add <nombre> <host>                      añade uno y lo activa
  context use <nombre>                             cambia de daemon
  context rm <nombre>                              lo elimina
  config [show|path]                               configuración actual
  config set <clave> <valor>                       p. ej. defaults.image min
  version                                          versión del CLI

CONEXIÓN
  Precedencia:  -H  >  $KLING_HOST  >  contexto activo  >  socket local

    kling context add lab ssh://juan@192.168.2.60
    kling context use lab

  El daemon jamás escucha en un puerto de red: controlar microVMs equivale a
  root en su host, así que el único acceso remoto es SSH.
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
		fmt.Fprintf(os.Stderr, "comando desconocido: %s\n\n%s", cmd, usage)
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
	return fs.String("H", "", "endpoint del daemon (socket o ssh://usuario@host)")
}

// loadConfig lee la configuración sin hacer fallar al CLI si está corrupta: una
// configuración ilegible no debe impedir listar máquinas.
func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "aviso: %v (sigo con los valores por defecto)\n", err)
		return &config.Config{}
	}
	return cfg
}

// hostOf resuelve a qué daemon hablar.
func hostOf(flagValue string) string { return loadConfig().Host(flagValue) }

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ── daemon ────────────────────────────────────────────────────────────────────

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", envOr("KLING_SOCKET", transport.DefaultSocket), "socket Unix a servir")
	root := fs.String("root", envOr("KLING_ROOT", "/var/lib/kindling"), "directorio de datos")
	fcBin := fs.String("firecracker", envOr("KLING_FIRECRACKER", "firecracker"), "binario de firecracker")
	sockUser := fs.String("socket-user", os.Getenv("KLING_SOCKET_USER"), "usuario al que ceder el socket (para el CLI por SSH)")
	runAs := fs.String("run-as", envOr("KLING_RUN_AS", "kindling"), "usuario sin privilegios con el que corre Firecracker")
	if err := fs.Parse(args); err != nil {
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
	listen := fs.String("listen", "", "dónde escuchar (por defecto: gateway.listen, o 127.0.0.1:8080)")
	idle := fs.Duration("idle", 0, "tiempo sin peticiones antes de congelar (por defecto: gateway.idle, o 5m)")
	ephemeral := fs.Bool("ephemeral", false, "una microVM por acción, destruida al terminar (máximo aislamiento, sin estado)")
	prewarm := fs.Int("prewarm", 1, "instancias pre-calentadas por servicio (0 = desactivado)")
	memory := fs.String("memory", "", "servicio MCP donde recordar qué herramienta resolvió cada petición")
	pprofOn := fs.Bool("pprof", false, "expone /debug/pprof; solo diagnóstico temporal y solo en loopback")
	noAuth := fs.Bool("no-auth", false, "sin token; solo para desarrollo y solo escuchando en loopback")
	if err := fs.Parse(args); err != nil {
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
		return fmt.Errorf("-pprof exige escuchar en loopback, y %q no lo es.\n"+
			"Diagnostica por un túnel:  ssh -L 8080:127.0.0.1:8080 <host>", addr)
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
		return fmt.Errorf("no alcanzo el daemon: %w", err)
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
	go gw.Reap(ctx)
	if *ephemeral {
		go gw.PrewarmAll(ctx)
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
	fmt.Printf("gateway en http://%s\n", *listen)
	fmt.Printf("  herramienta:  http://%s/mcp/<servicio>\n", *listen)
	fmt.Printf("  inventario:   http://%s/services\n", *listen)
	fmt.Printf("  ocioso:       %s antes de congelar\n", *idle)
	if token == "" {
		fmt.Printf("  auth:         DESACTIVADA (-no-auth) — solo vale porque escucha en loopback\n")
	} else {
		fmt.Printf("  auth:         Authorization: Bearer <token>  ·  /healthz abierto\n")
	}
	if *pprofOn {
		fmt.Printf("  pprof:        ACTIVO en http://%s/debug/pprof/ (tras el token) — apágalo al terminar\n", *listen)
	}
	if memSvc != "" {
		fmt.Printf("  memoria:      activa sobre %q — ordena las búsquedas por lo que ya funcionó\n", memSvc)
	}
	if *ephemeral {
		fmt.Printf("  modo:         EFÍMERO — cada acción en su propia microVM, destruida al terminar\n")
		if *prewarm > 0 {
			fmt.Printf("  pre-calentado: %d instancia(s) por servicio, listas para responder\n", *prewarm)
		}
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
			return "", fmt.Errorf("-no-auth exige escuchar en loopback, y %q no lo es.\n"+
				"Despertar un snapshot es ejecutar código: sin token, cualquiera que alcance\n"+
				"ese puerto ejecuta tus herramientas. Quita -no-auth o escucha en 127.0.0.1", addr)
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
		fmt.Printf("\nAVISO: generé un token pero no pude guardarlo en %s (%v).\n", config.Path(), err)
		fmt.Printf("       Va a CAMBIAR en cada reinicio. Fíjalo para que no pase:\n")
		fmt.Printf("         Environment=KLING_GATEWAY_TOKEN=%s\n\n", t)
		return t, nil
	}

	// La única vez que se imprime entero. A partir de aquí `config show` lo
	// enmascara, porque esa orden se teclea con gente mirando la pantalla.
	fmt.Printf("token generado y guardado en %s\n\n", config.Path())
	fmt.Printf("  En la máquina desde la que uses el CLI:\n")
	fmt.Printf("    kling config set gateway.token %s\n\n", t)
	return t, nil
}

// ── máquinas ──────────────────────────────────────────────────────────────────

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "nombre de la máquina")
	image := fs.String("image", "", "imagen de rootfs (por defecto: defaults.image, o 'default')")
	from := fs.String("from", "", "instanciar desde un snapshot dorado (~ms, sin arranque en frío)")
	cpus := fs.Int("cpus", 0, "vCPUs (por defecto: 1)")
	mem := fs.Int("mem", 0, "memoria en MiB (por defecto: 256)")
	egress := fs.String("egress", "", "salida de red: none | internet (nunca alcanza redes privadas)")
	ttl := fs.Int("ttl", 0, "segundos hasta congelarse sola (0 = nunca)")
	cpu := fs.Int("cpu", 0, "techo de CPU en porcentaje de un core (0 = por defecto)")
	service := fs.String("service", "", "servicio MCP al que pertenece (agrupa en topo y export)")
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volumen a montar: nombre[:/punto][:ro] (repetible)")
	mount := fs.String("mount", "", "dónde montar el volumen (por defecto /data; solo con uno)")
	volRO := fs.Bool("volume-ro", false, "montarlo en solo lectura: así lo comparten varias microVMs")
	var labels labelFlag
	fs.Var(&labels, "label", "etiqueta clave=valor (repetible)")
	if err := fs.Parse(args); err != nil {
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
		VCPUs:      config.Or(*cpus, cfg.Defaults.VCPUs, 1),
		MemMiB:     config.Or(*mem, cfg.Defaults.MemMiB, 256),
		Egress:     config.Or(*egress, cfg.Defaults.Egress, "none"),
		TTLSeconds: config.Or(*ttl, cfg.Defaults.TTL),
		CPUPct:     config.Or(*cpu, cfg.Defaults.CPUPct),
		Labels:     labels.merge(*service),
		// El volumen es una propiedad de la MÁQUINA, no solo de un servicio MCP:
		// arrancar una a mano con almacenamiento que sobreviva es tan legítimo
		// como importar un servicio con él.
		Volumes: vols,
	})
	if err != nil {
		return err
	}
	if mc.From != "" {
		fmt.Printf("%s  %s  instanciada desde %s en %d ms\n", mc.ID[:12], mc.Name, mc.From, mc.ThawMS)
	} else {
		fmt.Printf("%s  %s  arrancada en frío en %d ms\n", mc.ID[:12], mc.Name, mc.BootMS)
	}
	return nil
}

// labelFlag acumula -label k=v repetidos.
type labelFlag map[string]string

func (l *labelFlag) String() string { return "" }

func (l *labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("etiqueta inválida %q: usa clave=valor", v)
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
	out := fs.String("o", "kindling.html", "fichero de salida")
	// El mapa ya trae todo el detalle; la bandera sigue aceptándose para no
	// romper a quien la tuviera en un script.
	_ = fs.Bool("detail", false, "obsoleta: el informe siempre incluye el detalle")
	open := fs.Bool("open", false, "abrirlo al terminar")
	if err := fs.Parse(args); err != nil {
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
	fmt.Printf("%s  (%d máquinas, %d snapshots, %.0f KB)\n",
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
	tail := fs.Int("tail", 200, "últimas N líneas (0 = todo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling logs <ref> [-tail N]")
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("uso: kling commit <ref> <nombre-snapshot>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	snap, err := api.NewClient(hostOf(*host)).Commit(ctx, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	fmt.Printf("%s  snapshot dorado  (%s de memoria)\n", snap.Name, human(snap.MemBytes))
	fmt.Printf("instancia con:  kling run -from %s\n", snap.Name)
	return nil
}

func cmdSnapshots(args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "salida en JSON")
	if err := fs.Parse(args); err != nil {
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
	fmt.Fprintln(tw, "NOMBRE\tIMAGEN\tCPU/MEM\tMEMORIA\tDISCO\tINSTANCIAS\tEDAD")
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling rmi <nombre-snapshot>")
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
	all := fs.Bool("a", false, "incluye las paradas")
	asJSON := fs.Bool("json", false, "salida en JSON")
	if err := fs.Parse(args); err != nil {
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

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tNOMBRE\tIMAGEN\tESTADO\tCPU/MEM\tDISCO\tSALIDA\tEDAD\tÚLTIMA OP")
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
		fmt.Printf("\ndisco propio de las máquinas: %s (la imagen base se comparte)\n", human(totalDisk))
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling %s <ref>", op)
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
			fmt.Printf("%s  warm  (%d ms, %d MiB en disco)\n", mc.ID[:12], mc.FreezeMS, mc.SnapSize>>20)
		case op == "thaw":
			fmt.Printf("%s  running  (%d ms)\n", mc.ID[:12], mc.ThawMS)
		default:
			fmt.Printf("%s  %s\n", mc.ID[:12], mc.State)
		}
	}
	return nil
}

// ── observación ───────────────────────────────────────────────────────────────

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "una línea JSON por evento")
	if err := fs.Parse(args); err != nil {
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
	if err := fs.Parse(args); err != nil {
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

	kvm := "sin KVM"
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
			fmt.Printf("   %s◆ (arrancadas en frío)\n", branch)
		} else {
			var snap *api.Snapshot
			for _, s := range snaps {
				if s.Name == g {
					snap = s
				}
			}
			fmt.Printf("   %s◆ %-16s snapshot dorado · %s de memoria compartida\n",
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
			fmt.Printf("   %s └── (sin instancias)\n", cont)
		}
		if !last {
			fmt.Printf("   │\n")
		}
	}

	fmt.Printf("\n  %d running · %d warm · %d parada(s)   disco: %s propio + %s compartido\n",
		running, warm, stopped, human(diskOwn), human(diskShared))
	fmt.Printf("  salida:  ⌀ aislada   → internet (las redes privadas están bloqueadas siempre)\n")
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
	return "(sin red activa)"
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	i, err := c.Info(ctx)
	if err != nil {
		return err
	}
	kvm := "no"
	if i.KVM {
		kvm = "sí"
	}
	fmt.Printf("endpoint:     %s\n", c.Endpoint())
	fmt.Printf("daemon:       %s\n", i.Version)
	fmt.Printf("root:         %s\n", i.Root)
	fmt.Printf("KVM:          %s\n", kvm)
	fmt.Printf("firecracker:  %s\n", strings.TrimSpace(i.Firecrack))
	fmt.Printf("máquinas:     %d\n", i.Machines)
	return nil
}
