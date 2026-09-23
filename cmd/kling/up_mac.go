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
	arch        string
	macOS       string // "15.5"; vacío si no se pudo leer
	vmm         string // ruta de kling-vz; vacío si no está
	vmmVersion  string // salida de --version; vacío si no arranca
	vmmErr      string
	entitlement bool // firmado con com.apple.security.virtualization
	mkfs        bool
	debugfs     bool
	kernel      bool
	baseImage   bool
	root        string
}

func localProbeMac(root string) probeMac {
	p := probeMac{arch: runtime.GOARCH, macOS: versionMacOS(), root: root}
	vmm := machine.ResolverVMM(machine.BackendVZ, os.Getenv("KLING_VMM"), "")
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
		p.entitlement = tieneEntitlementVZ(path)
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

// tieneEntitlementVZ mira si el binario está firmado con el permiso de
// virtualización. Sin él, Virtualization.framework se niega a crear la VM y
// el error llega tarde y críptico, en el primer arranque.
func tieneEntitlementVZ(path string) bool {
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
	return strings.Contains(string(out), "com.apple.security.virtualization")
}

// versionMayor devuelve el número mayor de una versión "15.5.1"; 0 si no se
// puede leer.
func versionMayor(v string) int {
	mayor, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(mayor)
	if err != nil {
		return 0
	}
	return n
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
	copyHint := "kling images copy min -from ssh://user@linux-arm64-host   # also copies the kernel"
	return []check{
		{
			label: "Apple Silicon", ok: p.arch == "arm64", fatal: true,
			found: p.arch,
			why:   "kling-vz runs aarch64 guests on Virtualization.framework; Intel Macs are not supported",
			fix:   []string{"use a Linux host with KVM instead:  kling context add lab ssh://user@host"},
		},
		{
			label: "macOS 14+", ok: versionMayor(p.macOS) >= 14, fatal: true,
			found: macOS,
			why:   "freezing and thawing microVMs needs the save/restore of macOS 14 (Sonoma) or later",
			fix:   []string{"update macOS (System Settings > General > Software Update)"},
		},
		{
			label: "kling-vz", ok: p.vmm != "" && p.vmmVersion != "", fatal: true,
			found: vmmFound,
			why:   "it's the VMM on macOS: without it there's nothing to start",
			fix: []string{
				"make vz            # in the kindling repo; builds and signs vz/cmd/kling-vz",
				"# then put kling-vz next to kling, or set KLING_VMM=/path/to/kling-vz",
			},
		},
		{
			label: "vz entitlement", ok: p.vmm != "" && p.entitlement, fatal: true,
			found: presence(p.entitlement, "signed with com.apple.security.virtualization", "missing"),
			why:   "macOS refuses to create a VM from a binary without the virtualization entitlement",
			fix:   []string{"make vz            # signs it (ad-hoc is enough)"},
		},
		{
			label: "e2fsprogs", ok: p.mkfs && p.debugfs, fatal: true,
			found: presence(p.mkfs && p.debugfs, "mkfs.ext4 and debugfs found", "mkfs.ext4 or debugfs missing"),
			why:   "every microVM gets an ext4 overlay, and images are inspected with debugfs",
			fix:   []string{"brew install e2fsprogs      # kling finds it in Homebrew's prefix, no PATH change needed"},
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
	fmt.Printf("kindling — local runtime on macOS (vz)  (data root: %s)\n\n", root)
	cs := checksMac(localProbeMac(root))
	fatal, warn := printChecks(cs, "")
	if fatal > 0 {
		fmt.Println()
		return fmt.Errorf("missing prerequisites without which there are no microVMs; see above, with the command that fixes each")
	}
	if checkOnly {
		return nil
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
