package main

// `kling up` en macOS: el backend vz (kling-vz sobre Virtualization.framework).
//
// Mismo espíritu que en Linux: diagnosticar TODO de una vez, con el porqué y
// la orden que lo arregla, y no ejecutar por el usuario nada que no haya
// pedido. La diferencia es que aquí no hay nada privilegiado: el daemon corre
// como el usuario, y lo que falta se instala con Homebrew o con `make vz`.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
)

// launchAgentLabel es la etiqueta del agente de launchd que docs/mac.md
// enseña a instalar para que el daemon arranque al iniciar sesión.
const launchAgentLabel = "dev.kindling.daemon"

// probeMac es la foto de un Mac como host de microVMs.
type probeMac struct {
	backend     string // machine.BackendVZ o machine.BackendHV
	motivo      string // por qué ese backend ("auto: …", "daemon.vmm hv")
	arch        string
	macOS       string // "15.5"; vacío si no se pudo leer
	vmm         string // ruta de kling-vz (o kling-hv); vacío si no está
	vmmVersion  string // salida de --version; vacío si no arranca
	vmmErr      string
	entitlement bool // firmado con el entitlement de su framework
	mkfs        bool
	debugfs     bool
	kernel      bool
	baseImage   bool
	root        string
}

func localProbeMac(root string) probeMac {
	// El backend que usaría el daemon (KLING_VMM, daemon.vmm o el de la
	// plataforma, auto resuelto como lo hará él al arrancar): con hv lo que
	// hay que tener es kling-hv.
	backend, motivo, err := resolverBackend(loadConfig().Daemon.VMM, os.Getenv("KLING_VMM"), runtime.GOOS, runtime.GOARCH, entornoAutoReal)
	if err != nil || (backend != machine.BackendVZ && backend != machine.BackendHV) {
		backend, motivo = machine.BackendVZ, ""
	}
	p := probeMac{backend: backend, motivo: motivo, arch: runtime.GOARCH, macOS: versionMacOS(), root: root}
	vmm := machine.ResolverVMM(backend, envVMM(), "")
	if path, err := exec.LookPath(vmm); err == nil {
		p.vmm = path
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
		cancel()
		if err == nil {
			p.vmmVersion = strings.TrimSpace(firstLine(string(out)))
		} else {
			p.vmmErr = strings.TrimSpace(firstLine(string(out)))
			if p.vmmErr == "" {
				p.vmmErr = err.Error()
			}
		}
		p.entitlement = tieneEntitlement(path, entitlementDe(backend))
	}
	p.mkfs = machine.E2fsDisponible("mkfs.ext4")
	p.debugfs = machine.E2fsDisponible("debugfs")
	p.kernel = fileExists(filepath.Join(root, "images", "vmlinux"))
	p.baseImage = fileExists(filepath.Join(root, "images", "min.ext4"))
	return p
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// entitlementDe es el permiso con el que tiene que ir firmado el VMM del
// backend: Virtualization.framework o Hypervisor.framework.
func entitlementDe(backend string) string {
	if backend == machine.BackendHV {
		return "com.apple.security.hypervisor"
	}
	return "com.apple.security.virtualization"
}

// tieneEntitlement mira si el binario está firmado con ese permiso. Sin él, el
// framework se niega a crear la VM y el error llega tarde y críptico, en el
// primer arranque.
func tieneEntitlement(path, ent string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "codesign", "-d", "--entitlements", "-", "--xml", path).CombinedOutput()
	if err != nil {
		// codesign antiguo sin --xml.
		out, err = exec.CommandContext(ctx, "codesign", "-d", "--entitlements", ":-", path).CombinedOutput()
		if err != nil {
			return false
		}
	}
	return strings.Contains(string(out), ent)
}

func checksMac(p probeMac) []check {
	vmmFound := "not next to kling nor in PATH"
	switch {
	case p.vmm != "" && p.vmmVersion != "":
		vmmFound = p.vmm + " (" + p.vmmVersion + ")"
	case p.vmm != "":
		vmmFound = p.vmm + " — does not run: " + p.vmmErr
	}
	macOS := p.macOS
	if macOS == "" {
		macOS = "unknown version"
	}
	copyHint := "kling image copy min -from ssh://user@linux-arm64-host   # also copies the kernel"
	backend := p.backend
	if backend == "" {
		backend = machine.BackendVZ
	}
	nombre := machine.NombreVMM(backend)
	framework, build := "Virtualization.framework", "make vz            # in the kindling repo; builds and signs vz/cmd/kling-vz"
	if backend == machine.BackendHV {
		framework, build = "Hypervisor.framework", "make vz            # in the kindling repo; builds and signs kling-vz and kling-hv"
	}
	// vz necesita el save/restore de macOS 14; hv, el GIC de
	// Hypervisor.framework y un binario compilado para 15.2 (version_macos.go).
	// Se compara la versión entera: 15.1 no es 15.2.
	minMacOS := macOSMinDe(backend)
	whyMacOS := "freezing and thawing microVMs needs the save/restore of macOS 14 (Sonoma) or later"
	if backend == machine.BackendHV {
		whyMacOS = "kling-hv needs the in-kernel GIC of Hypervisor.framework and is built for macOS " + macOSMinHV + " or later"
	}
	return []check{
		{
			label: "Apple Silicon", ok: p.arch == "arm64", fatal: true,
			found: p.arch,
			why:   nombre + " runs aarch64 guests on " + framework + "; Intel Macs are not supported",
			fix:   []string{"use a Linux host with KVM instead:  kling context add lab ssh://user@host"},
		},
		{
			label: "macOS " + minMacOS + "+", ok: versionAlMenos(p.macOS, minMacOS), fatal: true,
			found: macOS,
			why:   whyMacOS,
			fix:   []string{"update macOS (System Settings > General > Software Update)"},
		},
		{
			label: nombre, ok: p.vmm != "" && p.vmmVersion != "", fatal: true,
			found: vmmFound,
			why:   "it's the VMM on macOS (daemon.vmm " + backend + "): without it there's nothing to start",
			fix: []string{
				build,
				"# then put " + nombre + " next to kling, or set KLING_VMM=/path/to/" + nombre,
			},
		},
		{
			label: backend + " entitlement", ok: p.vmm != "" && p.entitlement, fatal: true,
			found: presence(p.entitlement, "signed with "+entitlementDe(backend), "missing"),
			why:   "macOS refuses to create a VM from a binary without the " + framework + " entitlement",
			fix:   []string{"make vz            # signs it (ad-hoc is enough)"},
		},
		{
			label: "mkfs.ext4", ok: p.mkfs, fatal: true,
			found: presence(p.mkfs, "found", "missing"),
			why:   "every microVM gets an ext4 overlay",
			fix:   []string{"brew install e2fsprogs      # kling finds it in Homebrew's prefix, no PATH change needed"},
		},
		{
			// Sin debugfs se arranca igual: solo se pierde la inspección de
			// imágenes. Por eso no es fatal.
			label: "debugfs", ok: p.debugfs,
			found: presence(p.debugfs, "found", "missing"),
			why:   "inspects images (images cat, agent and layer checks); microVMs run without it",
			fix:   []string{"brew install e2fsprogs"},
		},
		{
			label: "guest kernel", ok: p.kernel,
			found: presence(p.kernel, "present", "missing from "+filepath.Join(p.root, "images")),
			why:   "images can't be built on macOS: they are copied from a Linux arm64 daemon",
			fix:   []string{copyHint},
		},
		{
			label: "base image", ok: p.baseImage,
			found: presence(p.baseImage, "min.ext4 present", "min.ext4 missing"),
			why:   "the base rootfs every layered image boots on",
			fix:   []string{copyHint},
		},
	}
}

// upMac diagnostica este Mac y, si se puede, arranca el daemon.
func upMac(root string, checkOnly bool) error {
	p := localProbeMac(root)
	fmt.Printf("kindling — local runtime on macOS (%s)  (data root: %s)\n", p.backend, root)
	if p.motivo != "" {
		fmt.Printf("vmm:         %s (%s)\n", p.backend, p.motivo)
		// Con auto, quedarse en vz no es un fallo (vz funciona, y -check no
		// debe salir con 1 en un Sonoma): una nota con cómo llegar a hv.
		if strings.HasPrefix(p.motivo, "auto: ") && p.backend == machine.BackendVZ {
			fmt.Println("             hv restores in ms and shares a golden's memory: it needs macOS " + macOSMinHV + "+ and kling-hv")
			fmt.Println("             signed next to kling (the release installer ships it, or: make vz && make install)")
		}
	}
	fmt.Println()
	cs := checksMac(p)
	fatal, warn := printChecks(cs, "")
	if fatal > 0 {
		fmt.Println()
		return fmt.Errorf("missing prerequisites without which there are no microVMs; see above, with the command that fixes each")
	}
	if checkOnly {
		return checkResult(warn)
	}
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	info := waitDaemon(ctx)
	cancel()
	if info != nil {
		fmt.Printf("daemon:      already running (%s, backend %s)\n", info.Version, info.Backend)
	} else if err := startMacDaemon(); err != nil {
		return err
	}
	if warn > 0 {
		fmt.Println()
		fmt.Printf("PENDING (%d):\n", warn)
		printChecks(onlyFailed(cs), "")
	}
	fmt.Println()
	fmt.Println("Next step:")
	fmt.Println("  kling status                     checks that everything responds")
	fmt.Println("  kling run -image min             a first microVM")
	return nil
}

// startMacDaemon arranca el agente de launchd si está instalado; si no, dice
// cómo instalarlo. Es lo único que up ejecuta por su cuenta, igual que en
// Linux arranca los servicios de systemd, y también se imprime antes.
func startMacDaemon() error {
	home, _ := os.UserHomeDir()
	plist := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if !fileExists(plist) {
		fmt.Println("daemon:      not running, and there's no launchd agent to start it.")
		fmt.Println("  Run it in a terminal:        kling daemon")
		fmt.Println("  Or at every login: install " + plist)
		fmt.Println("  (the plist is in docs/mac.md) and run `kling up` again.")
		return nil
	}
	argv := []string{"launchctl", "bootstrap", "gui/" + strconv.Itoa(os.Getuid()), plist}
	fmt.Printf("daemon:      $ %s\n", strings.Join(argv, " "))
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		// Ya cargado: bootstrap falla, pero kickstart lo arranca.
		argv = []string{"launchctl", "kickstart", "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchAgentLabel}
		fmt.Printf("             (%s) $ %s\n", strings.TrimSpace(string(out)), strings.Join(argv, " "))
		if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info := waitDaemon(ctx); info != nil {
		fmt.Printf("daemon:      ✓ responding (%s, backend %s)\n", info.Version, info.Backend)
		return nil
	}
	fmt.Println("daemon:      started, but not yet responding on the socket.")
	fmt.Println("  See why:  tail -n 50 \"$HOME/Library/Logs/kindling.log\"")
	return nil
}
