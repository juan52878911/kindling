package main

// `kling up` y `kling status`: el arranque en frío y el diagnóstico.
//
// Existen porque instalar kindling exigía correr a mano tres scripts
// (20-install-firecracker.sh, 30-fetch-artifacts.sh, 70-build-minimal-image.sh)
// y porque los dos fallos que más veces dejan una instalación medio rota no se
// veían: faltar `nft` —y entonces las microVMs arrancan sin red— y faltar el
// usuario de sistema `kindling` —y entonces Firecracker corre como root—. Los
// dos quedaban como avisos enterrados en el journal, que es donde nadie mira.
//
// La regla que gobierna este fichero: los comandos privilegiados se IMPRIMEN,
// no se ejecutan. Un `up` que hace useradd por su cuenta es un `up` que nadie
// puede auditar antes de decir que sí; y el que lo teclea aprende dónde está su
// sistema. La única excepción es arrancar los servicios de systemd, que es
// literalmente lo que se le ha pedido, y aun así se imprime la orden antes de
// lanzarla.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/assets"
	"github.com/juan52878911/kindling/internal/config"
)

// runAsDefault es el usuario sin privilegios con el que el daemon lanza
// Firecracker. Coincide con el valor por defecto de `kling daemon -run-as`.
const runAsDefault = "kindling"

// ── kling up ──────────────────────────────────────────────────────────────────

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	host := hostFlag(fs)
	root := fs.String("root", envOr("KLING_ROOT", "/var/lib/kindling"), "daemon data directory")
	checkOnly := fs.Bool("check", false, "only check prerequisites; doesn't start anything")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	endpoint := loadConfig().Host(*host)

	// El orden importa y no es arbitrario. Primero el contexto: si alguien ya
	// dijo "mi runtime está en ese host", preguntar por /dev/kvm aquí sería
	// diagnosticar la máquina equivocada.
	switch {
	case strings.HasPrefix(endpoint, "ssh://"):
		return upRemote(endpoint, *root, *checkOnly)
	case hasKVM():
		return upHere(*root, *checkOnly)
	default:
		return explainNoRuntime()
	}
}

// upHere prepara y arranca el runtime en ESTA máquina.
func upHere(root string, checkOnly bool) error {
	fmt.Printf("kindling — local runtime  (data root: %s)\n", root)
	if isWSL2() {
		// El único "ya lo tienes" real fuera de Linux nativo: WSL2 con
		// virtualización anidada expone /dev/kvm de verdad, no emulado.
		fmt.Printf("  detected WSL2 with /dev/kvm: this machine can serve as host\n")
	}
	fmt.Println()

	p := localProbe(root)
	cs := checksOf(p)
	fatal, warn := printChecks(cs, "")
	if fatal > 0 {
		fmt.Println()
		return fmt.Errorf("missing prerequisites without which there are no microVMs; see above, with the command that installs them")
	}
	if checkOnly {
		return nil
	}

	fmt.Println()
	if err := materialize(root); err != nil {
		return err
	}

	fmt.Println()
	if err := startServices(p); err != nil {
		return err
	}

	// Los avisos se repiten AL FINAL a propósito: arriba quedan sepultados bajo
	// la salida de systemd, y son justo los dos que dejan una instalación que
	// parece funcionar y no funciona. Se repite la MISMA lista, no una nueva
	// comprobación, para que el número del encabezado y lo que se lee debajo no
	// puedan contradecirse.
	if warn > 0 {
		fmt.Println()
		fmt.Printf("PENDING (%d) — the daemon is already running, but this is still broken:\n", warn)
		printChecks(onlyFailed(cs), "")
	}

	fmt.Println()
	fmt.Println("Next step:")
	fmt.Println("  kling status                     checks that everything responds")
	fmt.Println("  kling add <server>               packages an MCP server as a service")
	fmt.Println("  kling connect -all -install all  plugs it into your agents")
	return nil
}

// upRemote comprueba el host del contexto activo, por SSH.
//
// No arranca nada allí: systemctl necesita sudo, y sudo por una conexión sin
// terminal se cuelga esperando una contraseña que nadie va a poder teclear. Se
// diagnostica y se imprime la orden exacta, que es lo útil.
func upRemote(endpoint, root string, checkOnly bool) error {
	target := sshTarget(endpoint)
	fmt.Printf("kindling — remote runtime on %s  (active context)\n\n", target)

	p, err := remoteProbe(target, root)
	if err != nil {
		return fmt.Errorf("couldn't check %s: %w\n"+
			"Try the connection by hand:  ssh %s uname -a", target, err, target)
	}
	fatal, _ := printChecks(checksOf(p), target)

	fmt.Println()
	switch {
	case fatal > 0:
		return fmt.Errorf("missing prerequisites on %s; see above, with the command that installs them", target)
	case !p.unit:
		fmt.Printf("The daemon isn't installed as a service on %s. Deploy it from this repo:\n", target)
		fmt.Printf("  make deploy HOST=%s\n", endpoint)
	case checkOnly:
	default:
		fmt.Printf("Everything's ready. Start it there (needs sudo, and sudo needs your terminal):\n")
		fmt.Printf("  ssh -t %s 'sudo systemctl enable --now kling && sudo systemctl start kling-gateway'\n", target)
	}
	fmt.Println()
	fmt.Println("And then, from here:  kling status")
	return nil
}

// explainNoRuntime es el caso de un Mac (o de cualquier sitio sin KVM).
//
// Merece una explicación larga porque la pregunta "¿y por qué no puedo tener un
// runtime aquí?" se repite, y la respuesta corta ("no hay KVM") suena a excusa
// cuando el usuario ve que Docker y OrbStack sí levantan Linux.
func explainNoRuntime() error {
	fmt.Println("There can't be a kindling runtime here, and that's not a limitation of kindling.")
	fmt.Println()
	fmt.Println("  Firecracker is a VMM that talks to KVM and only KVM: it has no software")
	fmt.Println("  emulation mode to fall back on. Without /dev/kvm there's no microVM.")
	fmt.Println()

	if runtime.GOOS == "darwin" {
		fmt.Println("  On macOS the hypervisor is Virtualization.framework. Since macOS 15 it can")
		fmt.Println("  expose nested virtualization on Apple Silicon, but only if the app that")
		fmt.Println("  creates the VM explicitly asks for that capability — and neither OrbStack,")
		fmt.Println("  nor Docker Desktop, nor Colima ask for it. That's why there's no /dev/kvm")
		fmt.Println("  inside their Linux VMs: it's not hidden, it just was never created.")
	} else if isWSL2() {
		fmt.Println("  You're in WSL2, which CAN have KVM, but this instance doesn't.")
		fmt.Println("  Enable it in the .wslconfig file in your Windows user folder:")
		fmt.Println("      [wsl2]")
		fmt.Println("      nestedVirtualization=true")
		fmt.Println("  and restart with:  wsl --shutdown")
		fmt.Println()
	} else {
		fmt.Println("  There's no /dev/kvm on this machine. If it's physical, check that")
		fmt.Println("  virtualization is enabled in the BIOS and that the module is loaded:")
		fmt.Println("      sudo modprobe kvm_intel   # or kvm_amd")
		fmt.Println("  If it's a VM, its hypervisor has to expose nested virtualization.")
	}

	fmt.Println()
	fmt.Println("The CLI doesn't need any of this: it runs where you work and talks to the")
	fmt.Println("daemon over SSH, wherever KVM lives. Point it at that machine:")
	fmt.Println()
	fmt.Println("  kling context add lab ssh://user@host")
	fmt.Println("  kling up                             checks prerequisites there")
	fmt.Println()
	fmt.Println("Any Linux with KVM works: an old laptop, a server, a bare-metal instance,")
	fmt.Println("or a VM whose host allows nesting.")
	return nil
}

// ── prerrequisitos ────────────────────────────────────────────────────────────

// probe es la foto del host donde corre (o va a correr) el runtime. Se rellena
// igual mirando el sistema local que leyendo la salida de un guion por SSH, y
// así las comprobaciones se escriben UNA vez.
type probe struct {
	remote      bool
	system      string // "Linux/x86_64"
	kvm         bool
	firecracker string // ruta del binario, vacío si no está
	ip          bool   // iproute2: crea taps y namespaces
	iptables    bool   // NAT y filtrado de salida
	nft         bool   // el backend real de iptables en las distros modernas
	runAs       string // nombre del usuario sin privilegios
	runAsExists bool
	systemd     bool
	unit        bool // /etc/systemd/system/kling.service
	unitGateway bool
	active      string // salida de systemctl is-active kling
	kernel      bool   // $root/images/vmlinux
	baseImage   bool   // $root/images/min.ext4
	root        string
}

// check es una comprobación con su porqué y su remedio.
//
// `why` no es decorativo: es la diferencia entre "falta nft" (que no significa
// nada para quien acaba de instalar) y "sin él las microVMs arrancan sin red".
type check struct {
	label string
	ok    bool
	found string   // qué se vio
	why   string   // qué se rompe si no está
	fix   []string // qué teclear; NO lo ejecutamos por ti
	fatal bool     // sin esto no hay runtime; lo demás degrada
}

func localProbe(root string) probe {
	p := probe{
		system: runtime.GOOS + "/" + runtime.GOARCH,
		kvm:    hasKVM(),
		runAs:  envOr("KLING_RUN_AS", runAsDefault),
		root:   root,
	}
	p.firecracker = lookFirecracker()
	p.ip = inPath("ip")
	p.iptables = inPath("iptables")
	p.nft = inPath("nft")
	_, err := user.Lookup(p.runAs)
	p.runAsExists = err == nil
	p.systemd = inPath("systemctl")
	p.unit = fileExists("/etc/systemd/system/kling.service")
	p.unitGateway = fileExists("/etc/systemd/system/kling-gateway.service")
	if p.systemd {
		// is-active devuelve estado 3 cuando no está activo: el error da igual,
		// lo que importa es la palabra que imprime.
		out, _ := exec.Command("systemctl", "is-active", "kling").Output()
		p.active = strings.TrimSpace(string(out))
	}
	p.kernel = fileExists(filepath.Join(root, "images", "vmlinux"))
	p.baseImage = fileExists(filepath.Join(root, "images", "min.ext4"))
	return p
}

// remoteScript es lo mismo que hace localProbe, pero en sh y en el host del
// contexto. Se manda por la entrada estándar de `ssh ... sh -s` para no pelearse
// con dos niveles de comillas.
const remoteScript = `
si() { [ "$1" ] && echo si || echo no; }
echo "system=$(uname -s)/$(uname -m)"
echo "kvm=$(si "$([ -e /dev/kvm ] && echo 1)")"
echo "firecracker=$(command -v firecracker 2>/dev/null || true)"
echo "ip=$(si "$(command -v ip 2>/dev/null)")"
echo "iptables=$(si "$(command -v iptables 2>/dev/null)")"
echo "nft=$(si "$(command -v nft 2>/dev/null)")"
echo "runas=$(si "$(id -u "$RUNAS" 2>/dev/null)")"
echo "systemd=$(si "$(command -v systemctl 2>/dev/null)")"
echo "unit=$(si "$([ -f /etc/systemd/system/kling.service ] && echo 1)")"
echo "unitgw=$(si "$([ -f /etc/systemd/system/kling-gateway.service ] && echo 1)")"
echo "active=$(systemctl is-active kling 2>/dev/null || true)"
echo "kernel=$(si "$([ -f "$ROOT/images/vmlinux" ] && echo 1)")"
echo "image=$(si "$([ -f "$ROOT/images/min.ext4" ] && echo 1)")"
`

func remoteProbe(target, root string) (probe, error) {
	p := probe{remote: true, runAs: envOr("KLING_RUN_AS", runAsDefault), root: root}

	// BatchMode: si las claves no están puestas queremos un error inmediato, no
	// una petición de contraseña en mitad de un diagnóstico.
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		target, "RUNAS="+p.runAs, "ROOT="+root, "sh", "-s")
	cmd.Stdin = strings.NewReader(remoteScript)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return p, err
	}

	vals := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			vals[k] = v
		}
	}
	yes := func(k string) bool { return vals[k] == "si" }
	p.system = vals["system"]
	p.kvm = yes("kvm")
	p.firecracker = vals["firecracker"]
	p.ip, p.iptables, p.nft = yes("ip"), yes("iptables"), yes("nft")
	p.runAsExists = yes("runas")
	p.systemd = yes("systemd")
	p.unit, p.unitGateway = yes("unit"), yes("unitgw")
	p.active = vals["active"]
	p.kernel, p.baseImage = yes("kernel"), yes("image")
	return p, nil
}

// checksOf traduce la foto del host a la lista de comprobaciones con su remedio.
func checksOf(p probe) []check {
	firecrackerFound := "not in PATH or in /usr/local/bin"
	if p.firecracker != "" {
		firecrackerFound = p.firecracker
	}

	return []check{
		{
			label: "/dev/kvm", ok: p.kvm, fatal: true,
			found: presence(p.kvm, "present", "doesn't exist"),
			why:   "Firecracker talks to KVM and only KVM; there's no software mode",
			fix: []string{
				"sudo modprobe kvm_intel      # or kvm_amd, depending on the processor",
				"# if it's a VM, its host has to allow nested virtualization",
			},
		},
		{
			label: "firecracker", ok: p.firecracker != "", fatal: true,
			found: firecrackerFound,
			why:   "it's the VMM: without the binary there's nothing to start",
			fix:   []string{"sudo ./scripts/20-install-firecracker.sh"},
		},
		{
			label: "ip (iproute2)", ok: p.ip, fatal: true,
			found: presence(p.ip, "present", "not in PATH"),
			why:   "creates the tap and namespace for each microVM",
			fix: []string{
				"sudo apt install iproute2      # Debian/Ubuntu",
				"sudo dnf install iproute       # Fedora/RHEL",
			},
		},
		{
			label: "iptables", ok: p.iptables, fatal: true,
			found: presence(p.iptables, "present", "not in PATH"),
			why:   "sets up outbound NAT and blocks access to private networks",
			fix: []string{
				"sudo apt install iptables      # Debian/Ubuntu",
				"sudo dnf install iptables-nft  # Fedora/RHEL",
			},
		},
		{
			// El fallo real de una instalación reciente. iptables estaba, pero
			// en las distros modernas `iptables` es un envoltorio sobre
			// nftables: sin el backend, las reglas se aceptan y no existen.
			label: "nft (nftables)", ok: p.nft,
			found: presence(p.nft, "present", "not in PATH"),
			why: "on modern distros iptables is a facade over nftables: without it\n" +
				"      there's no NAT or egress filter, and microVMs start WITH NO NETWORK",
			fix: []string{
				"sudo apt install nftables      # Debian/Ubuntu",
				"sudo dnf install nftables      # Fedora/RHEL",
			},
		},
		{
			// El otro fallo real, y el más caro: sin el usuario, el daemon
			// arranca igual y solo deja un aviso en el journal.
			label: "user " + p.runAs, ok: p.runAsExists,
			found: presence(p.runAsExists, "exists", "doesn't exist"),
			why: "without it Firecracker runs as ROOT, and a guest escape leaves the\n" +
				"      attacker as root on the host instead of an unprivileged user",
			fix: []string{
				"sudo useradd --system --no-create-home --shell /usr/sbin/nologin " + p.runAs,
				"sudo usermod -aG kvm " + p.runAs,
			},
		},
	}
}

// printChecks imprime el informe y devuelve cuántos fallos son fatales y
// cuántos son avisos. Los que pasan ocupan una línea; los que no, lo que haga
// falta: es la única salida que alguien va a leer entera.
func printChecks(cs []check, host string) (fatal, warn int) {
	if host != "" {
		fmt.Printf("  (checked on %s)\n", host)
	}
	for _, c := range cs {
		mark := "✓"
		if !c.ok {
			mark = "✗"
			if c.fatal {
				fatal++
			} else {
				warn++
			}
		}
		fmt.Printf("  %s %-18s %s\n", mark, c.label, c.found)
		if c.ok {
			continue
		}
		fmt.Printf("      %s\n", c.why)
		for _, f := range c.fix {
			fmt.Printf("      %s\n", f)
		}
	}
	return fatal, warn
}

// onlyFailed filtra las que no pasaron, para poder repetirlas al final.
func onlyFailed(cs []check) []check {
	var out []check
	for _, c := range cs {
		if !c.ok {
			out = append(out, c)
		}
	}
	return out
}

// ── artefactos ────────────────────────────────────────────────────────────────

// materialize deja en $root/images el kernel y la imagen base que falten.
func materialize(root string) error {
	dir := filepath.Join(root, "images")

	if !assets.Embedded() {
		if fileExists(filepath.Join(dir, "vmlinux")) && fileExists(filepath.Join(dir, "min.ext4")) {
			fmt.Printf("artifacts:   already in %s\n", dir)
			return nil
		}
		fmt.Printf("artifacts:   this binary doesn't include them, and they're missing from %s\n", dir)
		fmt.Printf("  Build the full daemon (`make daemon-full`) or get them by hand:\n")
		fmt.Printf("    sudo ./scripts/30-fetch-artifacts.sh      # guest kernel\n")
		fmt.Printf("    sudo ./scripts/70-build-minimal-image.sh  # 'min' base image\n")
		fmt.Printf("    sudo install -m644 /opt/fc/vmlinux %s/vmlinux\n", dir)
		return nil
	}

	written, err := assets.Materialize(dir)
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("can't write to %s: %w\nrun it again with sudo", dir, err)
		}
		return err
	}
	switch len(written) {
	case 0:
		fmt.Printf("artifacts:   were already in %s (not overwriting what's there: it might be yours)\n", dir)
	default:
		fmt.Printf("artifacts:   written to %s\n", dir)
		for _, w := range written {
			fmt.Printf("  %s\n", w)
		}
	}
	return nil
}

// ── arranque ──────────────────────────────────────────────────────────────────

// startServices levanta daemon y gateway con systemd, si la unit está puesta.
func startServices(p probe) error {
	if !p.systemd || !p.unit {
		fmt.Println("services:    no systemd units installed.")
		fmt.Println("  Deploy them from this repo (installs binary, units, and token):")
		fmt.Println("    make deploy HOST=ssh://user@this-host")
		fmt.Println()
		fmt.Println("  Or start it by hand, in two terminals:")
		fmt.Printf("    sudo kling daemon -root %s\n", p.root)
		fmt.Println("    kling gateway -listen 127.0.0.1:8080")
		return nil
	}

	units := []string{"kling"}
	if p.unitGateway {
		units = append(units, "kling-gateway")
	}
	for _, u := range units {
		// La orden se imprime ANTES de lanzarla: quien la ve puede pararla, y
		// sobre todo puede repetirla sin kindling delante.
		argv := privileged("systemctl", "enable", "--now", u)
		fmt.Printf("$ %s\n", strings.Join(argv, " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
	}

	// Que systemd diga "started" no significa que el daemon esté sirviendo: el
	// socket tarda un instante. Se comprueba de verdad.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info := waitDaemon(ctx); info != nil {
		fmt.Printf("daemon:      %s · %d machine(s) · root %s\n", info.Version, info.Machines, info.Root)
		return nil
	}
	fmt.Printf("daemon:      started, but not yet responding on the socket.\n")
	fmt.Printf("  See why:  journalctl -u kling -n 50 --no-pager\n")
	return nil
}

// waitDaemon espera a que el socket local conteste. Devuelve nil si se agota.
func waitDaemon(ctx context.Context) *api.Info {
	c := api.NewClient("")
	for {
		if info, err := c.Info(ctx); err == nil {
			return info
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// privileged antepone sudo si no somos root. Si ya lo somos, sudo sobra y
// además puede no estar instalado.
func privileged(argv ...string) []string {
	if os.Geteuid() == 0 {
		return argv
	}
	return append([]string{"sudo"}, argv...)
}

// ── kling status ──────────────────────────────────────────────────────────────

// cmdStatus responde a "¿está esto en pie?" mirando las cuatro capas que
// pueden fallar por separado: daemon, KVM/firecracker, gateway y agentes.
//
// No devuelve error cuando algo está caído: la respuesta ES el informe. Un
// diagnóstico que aborta en la primera línea rota es justo el que no sirve.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	host := hostFlag(fs)
	gwFlag := fs.String("gateway", "", "gateway URL (default: gateway.url, or inferred from context)")
	asJSON := fs.Bool("json", false, "JSON output (daemon, gateway and detected agents)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	c := api.NewClient(cfg.Host(*host))
	gw := strings.TrimSuffix(config.Or(*gwFlag, cfg.Gateway.URL, guessGateway(cfg.Host(*host))), "/")
	if *asJSON {
		return statusJSON(ctx, c, gw, cfg.Gateway.Token)
	}
	fmt.Printf("endpoint:     %s\n", c.Endpoint())

	info, err := c.Info(ctx)
	daemonUp := err == nil
	if err != nil {
		fmt.Printf("daemon:       ✗ not responding (%v)\n", err)
		fmt.Printf("              diagnose and start it with:  kling up\n")
		// KVM y firecracker los reporta el propio daemon: sin él no se sabe si
		// faltan o si simplemente no hay quien conteste, y decir "no" sería
		// mentir sobre una máquina que quizá está perfecta.
		fmt.Printf("KVM:          ? (reported by the daemon)\n")
		fmt.Printf("firecracker:  ? (reported by the daemon)\n")
	} else {
		fmt.Printf("daemon:       ✓ %s · %d machine(s) · root %s\n", info.Version, info.Machines, info.Root)
		fmt.Printf("KVM:          %s\n", markYes(info.KVM, "available", "NOT available — no microVM can be started"))
		fc := strings.TrimSpace(info.Firecrack)
		if fc == "" {
			fc = "✗ not found — install it with scripts/20-install-firecracker.sh"
		} else {
			fc = "✓ " + fc
		}
		fmt.Printf("firecracker:  %s\n", fc)
	}

	// Gateway: dos preguntas distintas y hay que hacer las dos. /healthz está
	// abierto a propósito (systemd y los monitores tienen que poder mirar) y
	// solo dice "el proceso está vivo". /services va detrás del token, así que
	// es lo único que demuestra que el token configurado AQUÍ sirve para entrar.
	fmt.Printf("gateway:      %s\n", gw)
	if err := httpOK(gw + "/healthz"); err != nil {
		fmt.Printf("  health:     ✗ not responding (%v)\n", err)
		fmt.Printf("              start it on the daemon's host:  kling gateway -listen 0.0.0.0:8080\n")
	} else {
		fmt.Printf("  health:     ✓ alive\n")
		fmt.Printf("  services:   %s\n", servicesLine(gw+"/services", cfg.Gateway.Token))
	}

	// Salud de los servicios MCP: el catálogo dice cuántos hay; esto, cuántos
	// CONTESTAN. Sin esta línea, "services: ✓ 9" era una afirmación sobre el
	// inventario que se leía como una sobre la salud — y se observaron nueve
	// servicios 26 horas caídos detrás de ese ✓. El dato sale del meta de cada
	// snapshot (lo escribe `kling mcp health`); aquí no se despierta nada.
	if daemonUp {
		if snaps, serr := c.Snapshots(ctx); serr == nil && len(snaps) > 0 {
			fmt.Printf("mcp health:   %s\n", mcpHealthLine(snaps))
		}
	}

	// Agentes: sin esto, "el gateway funciona" y "mi editor lo usa" siguen
	// siendo dos cosas distintas, y la segunda es la que importa.
	det := detectedClients()
	if len(det) == 0 {
		fmt.Printf("agents:       none detected on this machine\n")
	} else {
		var names []string
		for _, cl := range det {
			names = append(names, cl.label)
		}
		fmt.Printf("agents:       %s\n", strings.Join(names, ", "))
		fmt.Printf("              plug them in with:  kling connect -all -install all\n")
	}
	return nil
}

// statusJSON emite el mismo diagnóstico que `status`, pero como un objeto
// estable para consumo de máquina. Cada bloque (daemon, gateway) sobrevive al
// fallo del otro: un daemon caído no impide reportar el gateway, y al revés.
func statusJSON(ctx context.Context, c *api.Client, gw, token string) error {
	type daemonInfo struct {
		OK          bool   `json:"ok"`
		Version     string `json:"version,omitempty"`
		Machines    int    `json:"machines"`
		Root        string `json:"root,omitempty"`
		KVM         bool   `json:"kvm"`
		Firecracker string `json:"firecracker,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	type gatewayInfo struct {
		URL      string   `json:"url"`
		Healthy  bool     `json:"healthy"`
		Services []string `json:"services,omitempty"`
		Error    string   `json:"error,omitempty"`
	}
	var report struct {
		Endpoint string      `json:"endpoint"`
		Daemon   daemonInfo  `json:"daemon"`
		Gateway  gatewayInfo `json:"gateway"`
		Agents   []string    `json:"agents"`
	}
	report.Endpoint = c.Endpoint()

	if info, err := c.Info(ctx); err != nil {
		report.Daemon.Error = err.Error()
	} else {
		report.Daemon.OK = true
		report.Daemon.Version = info.Version
		report.Daemon.Machines = info.Machines
		report.Daemon.Root = info.Root
		report.Daemon.KVM = info.KVM
		report.Daemon.Firecracker = strings.TrimSpace(info.Firecrack)
	}

	report.Gateway.URL = gw
	if err := httpOK(gw + "/healthz"); err != nil {
		report.Gateway.Error = err.Error()
	} else {
		report.Gateway.Healthy = true
		if names, err := gatewayServiceNames(gw+"/services", token); err != nil {
			report.Gateway.Error = err.Error()
		} else {
			report.Gateway.Services = names
		}
	}

	report.Agents = []string{}
	for _, cl := range detectedClients() {
		report.Agents = append(report.Agents, cl.label)
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

// gatewayServiceNames pide /services (texto plano, un servicio por línea) y
// devuelve solo los nombres. Es la variante para JSON de servicesLine, sin sus
// mensajes de remediación.
func gatewayServiceNames(url, token string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names, nil
}

// mcpHealthLine resume el último sondeo de salud de los servicios importados.
//
// Solo LEE los veredictos que dejó `kling mcp health` en el meta de cada
// snapshot: sondear aquí arrancaría una microVM por servicio y `status` debe
// ser instantáneo. Por eso "never probed" es un estado que se muestra y no se
// disimula: sin sondeo no hay dato, y fingir salud es lo que tapó la caída.
func mcpHealthLine(snaps []*api.Snapshot) string {
	var healthy, unknown int
	var sick []string
	for _, s := range snaps {
		switch s.Health {
		case "healthy":
			healthy++
		case "unhealthy":
			n := s.Name
			if svc := s.Service(); svc != "" {
				n = svc
			}
			sick = append(sick, n)
		default:
			unknown++
		}
	}
	switch {
	case len(sick) > 0:
		line := fmt.Sprintf("✗ %d unhealthy (%s) · %d healthy", len(sick), strings.Join(sick, ", "), healthy)
		if unknown > 0 {
			line += fmt.Sprintf(" · %d never probed", unknown)
		}
		return line + " — details: kling mcp ls"
	case unknown == len(snaps):
		return fmt.Sprintf("? none of the %d service(s) has ever been probed — probe them: kling mcp health", unknown)
	case unknown > 0:
		return fmt.Sprintf("✓ %d healthy · %d never probed — probe them: kling mcp health", healthy, unknown)
	default:
		return fmt.Sprintf("✓ %d healthy", healthy)
	}
}

// servicesLine resume /services, que responde texto plano, una línea por
// servicio.
func servicesLine(url, token string) string {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "✗ " + err.Error()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "✗ " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if token == "" {
			return "✗ requires a token and none is configured here\n" +
				"              copy it from the host:  kling config set gateway.token <t>"
		}
		return "✗ the gateway rejects the token from gateway.token (401)"
	}
	if resp.StatusCode >= 300 {
		return "✗ HTTP " + resp.Status
	}

	// Acotado: leer sin límite una respuesta ajena es regalarle a quien esté al
	// otro lado la memoria de este proceso. 64 KiB dan para miles de servicios.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	if len(names) == 0 {
		return "✓ none yet — package one with:  kling add <server>"
	}
	return fmt.Sprintf("✓ %d: %s", len(names), strings.Join(names, ", "))
}

// ── utilidades ────────────────────────────────────────────────────────────────

func hasKVM() bool { return fileExists("/dev/kvm") }

// isWSL2 mira /proc/version. Es la única forma fiable de reconocerlo, y el
// único entorno fuera de Linux nativo donde el usuario ya tiene KVM de verdad.
func isWSL2() bool {
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func inPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// lookFirecracker busca el VMM. Además del PATH mira /usr/local/bin, que es
// donde lo deja 20-install-firecracker.sh y que no siempre está en el PATH de
// una shell no interactiva.
func lookFirecracker() string {
	name := envOr("KLING_FIRECRACKER", "firecracker")
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if filepath.IsAbs(name) {
		return ""
	}
	if p := filepath.Join("/usr/local/bin", name); fileExists(p) {
		return p
	}
	return ""
}

// sshTarget extrae el destino de un endpoint ssh://usuario@host[/ruta/al/kling],
// con la misma regla que usa el transporte para marcar el binario remoto.
func sshTarget(endpoint string) string {
	t := strings.TrimPrefix(endpoint, "ssh://")
	if i := strings.Index(t, "/"); i >= 0 {
		t = t[:i]
	}
	return t
}

func presence(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func markYes(ok bool, yes, no string) string {
	if ok {
		return "✓ " + yes
	}
	return "✗ " + no
}

// httpOK comprueba que una URL responde 2xx. Se usa con /healthz, que está
// abierto por diseño y por tanto no necesita token.
func httpOK(url string) error {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}
