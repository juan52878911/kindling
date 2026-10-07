package main

// `kling upgrade`: cambia lo que funciona por otra versión y, si no funciona,
// lo deja como estaba (docs/actualizar.md §3.4). El flujo está en
// internal/upgrade; aquí, lo que es de cada sistema: qué binarios hay que
// cambiar y dónde están, y cómo se para y se arranca el daemon (systemd en
// Linux, launchd en macOS).
//
// No es `kling up` con banderas: up es "que esto funcione" y nunca reinicia un
// daemon vivo; upgrade es justo reiniciarlo con otro binario.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/internal/upgrade"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
	"github.com/juan52878911/kindling/pkg/plugin"
	"github.com/juan52878911/kindling/pkg/transport"
)

func cmdUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	host := hostFlag(fs)
	tag := fs.String("tag", "", "release to upgrade to, vX.Y.Z (default: the latest)")
	fs.StringVar(tag, "version", "", "same as -tag")
	fromDir := fs.String("from-dir", "", "take the binaries from this directory (release names or plain ones, checked against its SHA256SUMS if there is one) instead of downloading them")
	dry := fs.Bool("dry-run", false, "download, verify and print the plan; change nothing")
	check := fs.Bool("check", false, "same as -dry-run")
	rollback := fs.Bool("rollback", false, "go back to the binaries (and migrated files) saved by the last upgrade")
	force := fs.Bool("force", false, "also reinstall the same version or go to an older one")
	cliOnly := fs.Bool("cli", false, "upgrade only this kling binary and its extensions, not a daemon")
	unit := fs.String("unit", "kling", "systemd unit that runs the daemon (Linux)")
	timeout := fs.Duration("timeout", 60*time.Second, "how long the new daemon has to answer with its version")
	schemas := fs.Bool("schemas", false, "print this binary's version and the state formats it reads, as JSON (for another kling's upgrade)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: kling upgrade [-tag vX.Y.Z | -from-dir DIR] [-dry-run] [-force] [-cli] [-unit NAME]\n       kling upgrade -rollback [-dry-run]")
		fs.VisitAll(func(f *flag.Flag) {
			if f.Name != "schemas" {
				fmt.Fprintf(fs.Output(), "  -%s\t%s\n", f.Name, f.Usage)
			}
		})
	}
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("upgrade takes no arguments (did you mean -tag %s?)", fs.Arg(0))
	}
	if *schemas {
		return json.NewEncoder(os.Stdout).Encode(upgrade.InfoBinario{
			Kling: Version, API: api.APIVersion, Esquemas: machine.EsquemasSoportados()})
	}
	if *tag != "" && !strings.HasPrefix(*tag, "v") {
		*tag = "v" + *tag
	}
	if *rollback && (*tag != "" || *fromDir != "") {
		return errors.New("-rollback goes back to the saved binaries: it takes no -tag or -from-dir")
	}
	if *fromDir != "" {
		abs, err := filepath.Abs(*fromDir)
		if err != nil {
			return err
		}
		*fromDir = abs
	}

	o := upgrade.Opciones{
		Etiqueta: *tag,
		Fuente:   &upgrade.Fuente{Dir: *fromDir},
		Forzar:   *force,
		EnSeco:   *dry || *check,
		Plazo:    *timeout,
		Out:      os.Stdout,
	}
	endpoint := loadConfig().Host(*host)
	remoto := strings.HasPrefix(endpoint, "ssh://")
	if remoto {
		// El daemon está en otro host: allí se actualiza con sus permisos,
		// y aquí, el CLI.
		fmt.Printf("the daemon runs on %s; upgrade it there:\n  ssh -t %s sudo kling upgrade%s\n\n",
			sshTarget(endpoint), sshTarget(endpoint), argsRemotos(*tag, *rollback))
	}
	if remoto || *cliOnly {
		if err := prepararCLI(&o); err != nil {
			return err
		}
	} else if err := prepararDaemon(&o, endpoint, *unit); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	if *rollback {
		_, err := upgrade.VolverAtras(ctx, o)
		return err
	}
	res, err := upgrade.Actualizar(ctx, o)
	if err != nil {
		var va *upgrade.ErrVueltaAtras
		if errors.As(err, &va) && va.Fallo == nil {
			fmt.Printf("rolled back: %s is running again\n", res.Desde)
		}
		return err
	}
	if res.AlDia || res.EnSeco {
		return nil
	}
	actualizarExtensiones(o.Dir, res.Hacia, *fromDir)
	return nil
}

// argsRemotos son los argumentos que se repiten en el host del daemon.
func argsRemotos(tag string, rollback bool) string {
	switch {
	case rollback:
		return " -rollback"
	case tag != "":
		return " -tag " + tag
	}
	return ""
}

// assetDe es el nombre en la release de un binario de esta plataforma.
func assetDe(bin, goos, goarch string) upgrade.Asset {
	return upgrade.Asset{Nombre: bin + "-" + goos + "-" + goarch, Corto: bin}
}

// prepararCLI cambia solo este kling: sin daemon que parar ni verificar.
func prepararCLI(o *upgrade.Opciones) error {
	exe, err := ejecutableReal()
	if err != nil {
		return err
	}
	o.Actual = Version
	o.Piezas = []upgrade.Pieza{{Asset: assetDe("kling", runtime.GOOS, runtime.GOARCH), Destino: exe}}
	o.Dir, err = dirEstadoUsuario()
	return err
}

func ejecutableReal() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// dirEstadoUsuario es ~/.local/state/kling/upgrade: donde van las copias de
// una actualización del CLI, que no tiene raíz de daemon.
func dirEstadoUsuario() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "kling", "upgrade"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".local", "state", "kling", "upgrade"), nil
}

// prepararDaemon averigua qué daemon corre aquí, quién lo arranca y qué
// binarios usa.
func prepararDaemon(o *upgrade.Opciones, endpoint, unit string) error {
	c := api.NewClient(endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	info, err := c.Info(ctx)
	cancel()
	if err != nil {
		return &errWithHint{
			err:  fmt.Errorf("no daemon answers at %s: %v", c.Endpoint(), err),
			hint: "kling upgrade replaces a running daemon; start it first, or upgrade only this binary with: kling upgrade -cli"}
	}
	o.Daemon, o.Raiz = c, info.Root
	o.Dir = filepath.Join(info.Root, "upgrade")
	arch := config0(info.Arch, runtime.GOARCH)
	switch runtime.GOOS {
	case "linux":
		return prepararSystemd(o, c, unit, arch)
	case "darwin":
		return prepararLaunchd(o, c, arch)
	}
	return fmt.Errorf("kling upgrade does not know how to restart a daemon on %s", runtime.GOOS)
}

func config0(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ---- Linux: systemd

// unidadSystemd es el daemon que arranca una unidad de systemd.
type unidadSystemd struct{ nombre string }

func (u unidadSystemd) String() string { return u.nombre + ".service" }

func (u unidadSystemd) Parar(ctx context.Context) error {
	return correrOrden(ctx, "systemctl", "stop", u.nombre)
}

func (u unidadSystemd) Arrancar(ctx context.Context) error {
	return correrOrden(ctx, "systemctl", "start", u.nombre)
}

func correrOrden(ctx context.Context, argv ...string) error {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func prepararSystemd(o *upgrade.Opciones, c *api.Client, unit, arch string) error {
	if !reNombreUnidad.MatchString(unit) {
		return fmt.Errorf("invalid unit name %q", unit)
	}
	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", unit).Output()
	if err != nil {
		return fmt.Errorf("asking systemd for %s: %v (kling upgrade restarts the daemon through its unit)", unit, err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if pid <= 0 {
		return &errWithHint{
			err:  fmt.Errorf("the daemon answering on the socket is not run by %s.service", unit),
			hint: "-unit NAME picks the unit that runs it; without one, stop it, replace the binary by hand and start it"}
	}
	if err := esElDelSocket(c, unit+".service", pid); err != nil {
		return &errWithHint{err: err,
			hint: "-unit NAME picks the unit that runs it, and -host (or KLING_HOST) the socket of the daemon to upgrade"}
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return fmt.Errorf("finding the daemon's binary: %v (run it with sudo)", err)
	}
	// Un binario ya sustituido sale como "/ruta (deleted)".
	exe = strings.TrimSuffix(exe, " (deleted)")
	if os.Geteuid() != 0 {
		return &errWithHint{
			err:  fmt.Errorf("kling upgrade replaces %s and restarts %s.service: it needs root", exe, unit),
			hint: "sudo kling upgrade"}
	}
	lib := libDirDe(pid)
	o.Servicio = unidadSystemd{nombre: unit}
	o.Piezas = []upgrade.Pieza{{Asset: assetDe("kling", "linux", arch), Destino: exe}}
	// Los agentes de invitado solo se usan al construir imágenes; se cambian
	// los que haya instalados (make deploy los pone en /usr/local/lib/kindling).
	for _, g := range []string{"kling-guest", "kling-chispa"} {
		if p := filepath.Join(lib, g); fileExists(p) {
			o.Piezas = append(o.Piezas, upgrade.Pieza{Asset: assetDe(g, "linux", arch), Destino: p})
		}
	}
	if yo, err := ejecutableReal(); err == nil && yo != exe {
		fmt.Printf("note: this kling (%s) is not the daemon's (%s); upgrade it afterwards with: kling upgrade -cli\n\n", yo, exe)
	}
	return nil
}

// esElDelSocket comprueba que el daemon que contesta en el socket de c es el
// proceso pid que arranca servicio. Sin esto, con KLING_HOST apuntando a un
// daemon privado, upgrade cambiaría y reiniciaría el del servicio (otro) y
// verificaría, y al volver atrás restauraría, la raíz del privado.
func esElDelSocket(c *api.Client, servicio string, pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.New(c.Endpoint()).Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%s is not a local socket", c.Endpoint())
	}
	par, err := pidDelPar(uc)
	if err != nil {
		return fmt.Errorf("finding the process behind %s: %v", c.Endpoint(), err)
	}
	return mismoProceso(c.Endpoint(), servicio, pid, par)
}

// mismoProceso es la comparación de esElDelSocket.
func mismoProceso(endpoint, servicio string, pidServicio, pidSocket int) error {
	if pidServicio <= 0 {
		return fmt.Errorf("%s is not running, but a daemon answers on %s (pid %d)", servicio, endpoint, pidSocket)
	}
	if pidSocket != pidServicio {
		return fmt.Errorf("the daemon answering on %s (pid %d) is not the one %s runs (pid %d)", endpoint, pidSocket, servicio, pidServicio)
	}
	return nil
}

var reNombreUnidad = lazyre.New(`^[A-Za-z0-9@_.:-]+$`)

// libDirDe es el KLING_LIB_DIR con el que corre el daemon pid, o el de
// siempre. Del entorno del proceso solo se lee esa clave.
func libDirDe(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err == nil {
		for _, kv := range bytes.Split(b, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, []byte("KLING_LIB_DIR=")); ok && len(v) > 0 {
				return string(v)
			}
		}
	}
	return "/usr/local/lib/kindling"
}

// ---- macOS: launchd

// agenteLaunchd es el daemon que arranca el agente de launchd de docs/mac.md.
type agenteLaunchd struct{ dominio, plist string }

func (a agenteLaunchd) String() string { return launchAgentLabel }

func (a agenteLaunchd) Parar(ctx context.Context) error {
	return correrOrden(ctx, "launchctl", "bootout", a.dominio+"/"+launchAgentLabel)
}

func (a agenteLaunchd) Arrancar(ctx context.Context) error {
	return correrOrden(ctx, "launchctl", "bootstrap", a.dominio, a.plist)
}

func prepararLaunchd(o *upgrade.Opciones, c *api.Client, arch string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	a := agenteLaunchd{
		dominio: "gui/" + strconv.Itoa(os.Getuid()),
		plist:   filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"),
	}
	out, err := exec.Command("launchctl", "print", a.dominio+"/"+launchAgentLabel).Output()
	if err != nil || !fileExists(a.plist) {
		return &errWithHint{
			err:  errors.New("the daemon is not run by launchd here, so kling upgrade cannot restart it"),
			hint: "install the launchd agent (docs/mac.md), or stop the daemon and use: kling upgrade -cli"}
	}
	if err := esElDelSocket(c, launchAgentLabel, pidLaunchd(out)); err != nil {
		return &errWithHint{err: err,
			hint: "-host (or KLING_HOST) picks the socket of the daemon to upgrade; a daemon not run by launchd: stop it and use kling upgrade -cli"}
	}
	exe := programaLaunchd(out)
	if exe == "" {
		if exe, err = ejecutableReal(); err != nil {
			return err
		}
	}
	o.Servicio = a
	o.Piezas = []upgrade.Pieza{{Asset: assetDe("kling", "darwin", arch), Destino: exe}}
	// Sin el permiso de virtualización, el daemon nuevo arrancaría y la
	// primera microVM fallaría: se mira antes de parar nada.
	o.Validar = func(bajados map[string]string) error {
		if p, ok := bajados["kling-vz-darwin-arm64"]; ok && !tieneEntitlementVZ(p) {
			return fmt.Errorf("the downloaded kling-vz is not signed with com.apple.security.virtualization: nothing was changed")
		}
		return nil
	}
	if vz := filepath.Join(filepath.Dir(exe), "kling-vz"); fileExists(vz) && arch == "arm64" {
		o.Piezas = append(o.Piezas, upgrade.Pieza{Asset: assetDe("kling-vz", "darwin", "arm64"), Destino: vz})
	}
	return nil
}

// pidLaunchd saca de `launchctl print` el PID del agente (0 si no corre).
func pidLaunchd(out []byte) int {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "pid = "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// programaLaunchd saca de `launchctl print` el binario que lanza el agente.
func programaLaunchd(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "program = "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---- extensiones

// actualizarExtensiones pasa a la versión nueva las extensiones instaladas con
// `kling plugin install` (las que tienen su kling-<n>.json), con sus
// compañeros. Antes guarda lo de ahora en la copia de la actualización, para
// que -rollback las devuelva también. Un fallo aquí no deshace el núcleo, que
// ya está verificado: se dice y se da la orden para reintentarlo.
func actualizarExtensiones(dirCopia, version, fromDir string) {
	if os.Getenv("SUDO_USER") != "" && os.Geteuid() == 0 {
		fmt.Printf("\nextensions: each user upgrades theirs with:  kling upgrade -cli\n")
		return
	}
	dir, err := plugin.InstallDir()
	if err != nil {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var hechas []string
	for _, e := range ents {
		n, ok := strings.CutPrefix(e.Name(), "kling-")
		if !ok || strings.Contains(n, ".") || !plugin.ValidName(n) {
			continue
		}
		bin := filepath.Join(dir, e.Name())
		sc, err := plugin.ReadSidecar(bin)
		if err != nil || sc.Version == version || strings.TrimPrefix(sc.Version, "v") == strings.TrimPrefix(version, "v") {
			continue
		}
		guardar := []string{bin, plugin.SidecarPath(bin)}
		for _, c := range sc.Companions {
			if p := filepath.Join(dir, c); fileExists(p) {
				guardar = append(guardar, p)
			}
		}
		if err := upgrade.AñadirACopia(dirCopia, guardar); err != nil {
			fmt.Printf("warning: extension %s not upgraded: backing it up: %v\n", n, err)
			continue
		}
		op := plugin.InstallOptions{Name: n, Dir: dir, CoreVersion: strings.TrimPrefix(version, "v")}
		if fromDir != "" {
			op.File = filepath.Join(fromDir, "kling-"+n+"-"+runtime.GOOS+"-"+runtime.GOARCH)
			if !fileExists(op.File) {
				op.File = filepath.Join(fromDir, "kling-"+n)
			}
		} else {
			op.Tag = version
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		_, err = plugin.Install(ctx, op)
		cancel()
		if err != nil {
			fmt.Printf("warning: extension %s not upgraded: %v\n  retry with: kling plugin install %s@%s\n", n, err, n, version)
			continue
		}
		hechas = append(hechas, n)
	}
	if len(hechas) > 0 {
		fmt.Printf("extensions upgraded to %s: %s (services they run keep the old binary until restarted)\n", version, strings.Join(hechas, ", "))
	}
}
