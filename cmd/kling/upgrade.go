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
	"io"
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
	root := fs.String("root", "", "the daemon's data directory, for -rollback when no daemon answers (default: the one its unit or launchd agent gives)")
	timeout := fs.Duration("timeout", 60*time.Second, "how long the new daemon has to answer with its version")
	schemas := fs.Bool("schemas", false, "print this binary's version and the state formats it reads, as JSON (for another kling's upgrade)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: kling upgrade [-tag vX.Y.Z | -from-dir DIR] [-dry-run] [-force] [-cli] [-unit NAME]\n       kling upgrade -rollback [-dry-run] [-force] [-unit NAME] [-root DIR]")
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
			sshTarget(endpoint), sshTarget(endpoint), argsRemotos(*tag, *rollback, *dry || *check, *force, *unit, *root))
	}
	if remoto || *cliOnly {
		if err := prepararCLI(&o); err != nil {
			return err
		}
	} else if err := prepararDaemon(&o, endpoint, *unit, *root, *rollback); err != nil {
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

// argsRemotos son los argumentos que se repiten en el host del daemon. Los
// que cambian lo que hace van todos: un -dry-run que se pierde por el camino
// es una actualización de verdad al pegar la orden.
func argsRemotos(tag string, rollback, dry, force bool, unit, root string) string {
	var b strings.Builder
	switch {
	case rollback:
		b.WriteString(" -rollback")
	case tag != "":
		b.WriteString(" -tag " + tag)
	}
	if dry {
		b.WriteString(" -dry-run")
	}
	if force {
		b.WriteString(" -force")
	}
	if unit != "" && unit != "kling" {
		b.WriteString(" -unit " + unit)
	}
	if root != "" {
		b.WriteString(" -root " + root)
	}
	return b.String()
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
func prepararDaemon(o *upgrade.Opciones, endpoint, unit, root string, rollback bool) error {
	c := api.NewClient(endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	info, err := c.Info(ctx)
	cancel()
	if err != nil {
		if rollback {
			return prepararVueltaSinDaemon(o, c, unit, root, err)
		}
		return &errWithHint{
			err:  fmt.Errorf("no daemon answers at %s: %v", c.Endpoint(), err),
			hint: "kling upgrade replaces a running daemon; start it first, or upgrade only this binary with: kling upgrade -cli"}
	}
	if root != "" && filepath.Clean(root) != filepath.Clean(info.Root) {
		return fmt.Errorf("-root %s, but the daemon at %s runs on %s", root, c.Endpoint(), info.Root)
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
	environ, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	for _, a := range agentesFueraDeLib(environ, lib) {
		fmt.Printf("note: the daemon runs with %s, which kling upgrade does not replace; update it by hand (images without sh need a kling-guest with the Go init)\n", a)
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
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if v := valorEntorno(b, "KLING_LIB_DIR"); v != "" {
		return v
	}
	return "/usr/local/lib/kindling"
}

// valorEntorno es el valor de clave en un /proc/PID/environ ("" si no está).
func valorEntorno(environ []byte, clave string) string {
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte(clave+"=")); ok {
			return string(v)
		}
	}
	return ""
}

// agentesFueraDeLib son los agentes de invitado que el daemon da a los
// constructores (KLING_GUEST_AGENT y los de cada arquitectura) y que no viven
// en lib: upgrade no los cambia, y uno viejo no hace de init en Go (el
// constructor oci se niega entonces a construir una imagen sin sh). Solo se
// leen esas claves, que son rutas.
func agentesFueraDeLib(environ []byte, lib string) []string {
	var fuera []string
	for _, k := range []string{"KLING_GUEST_AGENT", "KLING_GUEST_AGENT_amd64", "KLING_GUEST_AGENT_arm64"} {
		if v := valorEntorno(environ, k); v != "" && filepath.Clean(v) != filepath.Join(lib, "kling-guest") {
			fuera = append(fuera, k+"="+v)
		}
	}
	return fuera
}

// ---- macOS: launchd

// agenteLaunchd es el daemon que arranca el agente de launchd de docs/mac.md.
type agenteLaunchd struct{ dominio, plist string }

func (a agenteLaunchd) String() string { return launchAgentLabel }

func (a agenteLaunchd) Parar(ctx context.Context) error {
	// Sin cargar ya está parado (un -rollback con el daemon caído).
	if exec.CommandContext(ctx, "launchctl", "print", a.dominio+"/"+launchAgentLabel).Run() != nil {
		return nil
	}
	return correrOrden(ctx, "launchctl", "bootout", a.dominio+"/"+launchAgentLabel)
}

func (a agenteLaunchd) Arrancar(ctx context.Context) error {
	return correrOrden(ctx, "launchctl", "bootstrap", a.dominio, a.plist)
}

func agenteDeEsteUsuario() (agenteLaunchd, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agenteLaunchd{}, err
	}
	return agenteLaunchd{
		dominio: "gui/" + strconv.Itoa(os.Getuid()),
		plist:   filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"),
	}, nil
}

func prepararLaunchd(o *upgrade.Opciones, c *api.Client, arch string) error {
	a, err := agenteDeEsteUsuario()
	if err != nil {
		return err
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
	// El agente del invitado de Linux que make install deja en lib/ de la raíz
	// de datos (libPorDefecto): el que meten en sus imágenes los constructores
	// del daemon de macOS. Si está, se cambia con lo demás; uno viejo no hace
	// de init en Go.
	if g := filepath.Join(libPorDefecto(o.Raiz), "kling-guest"); o.Raiz != "" && fileExists(g) {
		o.Piezas = append(o.Piezas, upgrade.Pieza{Asset: assetDe("kling-guest", "linux", arch), Destino: g})
	}
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
		o.CongeladaLegible = func(raiz, id string, viejos map[string]string) error {
			viejo, ok := viejos[vz]
			if !ok {
				return nil // kling-vz no se cambió
			}
			return volcadoLegible(filepath.Join(raiz, "machines", id, "snap.file"), formatoMaxVZ(viejo))
		}
	}
	return nil
}

// formatoMaxVZ es el formato de snapshot (kling_vz) más nuevo que lee el
// kling-vz bin: lo dice con -snapshot-formats; uno anterior a ese flag
// (v0.17 y antes) lee solo el 1.
func formatoMaxVZ(bin string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-snapshot-formats").Output()
	if err != nil {
		return 1
	}
	var min, max int
	if n, _ := fmt.Sscanf(string(out), "%d %d", &min, &max); n != 2 || max < 1 {
		return 1
	}
	return max
}

// volcadoLegible dice si el snapshot de kling-vz en p (su snap.file) es de un
// formato que lee un kling-vz que lee hasta max.
func volcadoLegible(p string, max int) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var s struct {
		KlingVZ int `json:"kling_vz"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&s); err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if s.KlingVZ > max {
		return fmt.Errorf("its snapshot is kling_vz %d and the previous kling-vz reads up to %d", s.KlingVZ, max)
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

// ---- -rollback sin daemon

// prepararVueltaSinDaemon prepara un -rollback cuando el daemon no contesta,
// que es justo cuando más falta hace: el nuevo no arranca tras un reinicio, o
// la vuelta atrás automática falló. La raíz (y con ella la copia) y el socket
// salen de cómo lo arranca su servicio; lo que se restaura, del manifest.json
// de la copia. No hay proceso al que mirar el PID: se comprueba en su lugar
// que el socket del servicio es el que se va a esperar.
func prepararVueltaSinDaemon(o *upgrade.Opciones, c *api.Client, unit, root string, causa error) error {
	var argv []string
	env := map[string]string{}
	switch runtime.GOOS {
	case "linux":
		if !reNombreUnidad.MatchString(unit) {
			return fmt.Errorf("invalid unit name %q", unit)
		}
		out, err := exec.Command("systemctl", "show", "-p", "ExecStart", "-p", "Environment", "-p", "EnvironmentFiles", unit).Output()
		if err != nil {
			return fmt.Errorf("asking systemd for %s: %v", unit, err)
		}
		var ficheros []string
		argv, env, ficheros = leerUnidadSystemd(out)
		if len(argv) == 0 {
			return &errWithHint{err: fmt.Errorf("no daemon answers at %s (%v), and %s.service has no ExecStart", c.Endpoint(), causa, unit),
				hint: "-unit NAME picks the unit that runs the daemon"}
		}
		// Como en systemd, lo del fichero manda sobre Environment=.
		for _, f := range ficheros {
			for k, v := range claveDeEntorno(f) {
				env[k] = v
			}
		}
		if os.Geteuid() != 0 {
			return &errWithHint{err: fmt.Errorf("kling upgrade -rollback restores the daemon's binaries and restarts %s.service: it needs root", unit),
				hint: "sudo kling upgrade -rollback"}
		}
		o.Servicio = unidadSystemd{nombre: unit}
	case "darwin":
		a, err := agenteDeEsteUsuario()
		if err != nil {
			return err
		}
		out, err := exec.Command("plutil", "-convert", "json", "-o", "-", a.plist).Output()
		if err != nil {
			return &errWithHint{err: fmt.Errorf("no daemon answers at %s (%v), and %s cannot be read: %v", c.Endpoint(), causa, a.plist, err),
				hint: "install the launchd agent (docs/mac.md), or roll back only this binary with: kling upgrade -cli -rollback"}
		}
		if argv, env, err = leerPlist(out); err != nil {
			return fmt.Errorf("%s: %v", a.plist, err)
		}
		o.Servicio = a
	default:
		return fmt.Errorf("kling upgrade does not know how to restart a daemon on %s", runtime.GOOS)
	}
	raiz, sock := raizYSocket(argv, env, transport.DefaultRoot(), transport.DefaultSocketPath())
	if root != "" {
		raiz = root
	}
	if rutaSocket(sock) != rutaSocket(c.Endpoint()) {
		return &errWithHint{err: fmt.Errorf("no daemon answers at %s, and %s serves %s instead", c.Endpoint(), o.Servicio, sock),
			hint: "-host unix://" + sock + " rolls back that one"}
	}
	o.Daemon, o.Raiz, o.Dir = c, raiz, filepath.Join(raiz, "upgrade")
	fmt.Printf("no daemon answers at %s (%v): rolling back from %s\n\n", c.Endpoint(), causa, o.Dir)
	return nil
}

func rutaSocket(endpoint string) string {
	return filepath.Clean(strings.TrimPrefix(endpoint, "unix://"))
}

// leerUnidadSystemd saca de `systemctl show -p ExecStart -p Environment -p
// EnvironmentFiles` los argumentos del daemon, su entorno y los ficheros de
// entorno.
func leerUnidadSystemd(out []byte) (argv []string, env map[string]string, ficheros []string) {
	env = map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "ExecStart":
			// { path=/usr/local/bin/kling ; argv[]=/usr/local/bin/kling daemon -root /x ; ... }
			if _, resto, ok := strings.Cut(v, "argv[]="); ok && argv == nil {
				a, _, _ := strings.Cut(resto, " ;")
				argv = strings.Fields(a)
			}
		case "Environment":
			for _, kv := range strings.Fields(v) {
				if k, v, ok := strings.Cut(strings.Trim(kv, `"`), "="); ok {
					env[k] = v
				}
			}
		case "EnvironmentFiles":
			// /etc/default/kling (ignore_errors=yes)
			if f := strings.Fields(v); len(f) > 0 {
				ficheros = append(ficheros, f[0])
			}
		}
	}
	return argv, env, ficheros
}

// claveDeEntorno lee de un fichero de entorno solo KLING_ROOT y KLING_SOCKET:
// lo demás puede ser un secreto y aquí no hace falta.
func claveDeEntorno(ruta string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(ruta)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "export ")), "=")
		if ok && (k == "KLING_ROOT" || k == "KLING_SOCKET") {
			out[k] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// leerPlist saca del plist del agente (en JSON, de plutil) los argumentos y
// el entorno del daemon.
func leerPlist(out []byte) ([]string, map[string]string, error) {
	var p struct {
		ProgramArguments     []string          `json:"ProgramArguments"`
		EnvironmentVariables map[string]string `json:"EnvironmentVariables"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, nil, err
	}
	if p.EnvironmentVariables == nil {
		p.EnvironmentVariables = map[string]string{}
	}
	return p.ProgramArguments, p.EnvironmentVariables, nil
}

// raizYSocket es la raíz y el socket con los que arranca `kling daemon` con
// esos argumentos y ese entorno, igual que los decide cmdDaemon: -root y
// -socket, si no KLING_ROOT y KLING_SOCKET, si no los de siempre.
func raizYSocket(argv []string, env map[string]string, defRaiz, defSocket string) (string, string) {
	raiz, sock := config0(env["KLING_ROOT"], defRaiz), config0(env["KLING_SOCKET"], defSocket)
	for i := 0; i < len(argv); i++ {
		nombre, valor, conIgual := strings.Cut(strings.TrimLeft(argv[i], "-"), "=")
		if !strings.HasPrefix(argv[i], "-") || (nombre != "root" && nombre != "socket") {
			continue
		}
		if !conIgual {
			if i+1 >= len(argv) {
				break
			}
			i++
			valor = argv[i]
		}
		if nombre == "root" {
			raiz = valor
		} else {
			sock = valor
		}
	}
	return raiz, sock
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
	hechas := actualizarExtensionesEn(dir, dirCopia, version, fromDir, plugin.InstallOptions{})
	if len(hechas) > 0 {
		fmt.Printf("extensions upgraded to %s: %s (services they run keep the old binary until restarted)\n", version, strings.Join(hechas, ", "))
	}
}

// actualizarExtensionesEn es el bucle de actualizarExtensiones sobre el
// directorio dir. base lleva el cliente y la URL de las releases (los tests).
// La versión del núcleo llega sin la "v" (main.Version la pone así
// release.yml); la etiqueta de la release la lleva.
func actualizarExtensionesEn(dir, dirCopia, version, fromDir string, base plugin.InstallOptions) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	etiqueta := "v" + strings.TrimPrefix(version, "v")
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
		op := base
		op.Name, op.Dir, op.CoreVersion = n, dir, strings.TrimPrefix(version, "v")
		if fromDir != "" {
			op.File = filepath.Join(fromDir, "kling-"+n+"-"+runtime.GOOS+"-"+runtime.GOARCH)
			if !fileExists(op.File) {
				op.File = filepath.Join(fromDir, "kling-"+n)
			}
		} else {
			op.Tag = etiqueta
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		_, err = plugin.Install(ctx, op)
		cancel()
		if err != nil {
			fmt.Printf("warning: extension %s not upgraded: %v\n  retry with: kling plugin install %s@%s\n", n, err, n, etiqueta)
			continue
		}
		hechas = append(hechas, n)
	}
	return hechas
}
