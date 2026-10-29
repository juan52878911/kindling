package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// kling doctor: todo lo que puede estar mal, de una vez y con su arreglo.
//
// `kling up -check` ya diagnostica el runtime; lo que no mira es lo que rodea al
// CLI: si el daemon contesta y con qué versión, si alguna extensión está rota
// o pide un kling más nuevo, si el completado está cargado. Son justo las
// cosas que fallan después de actualizar, y cada una se descubría por
// separado con un error distinto.
//
// Tres niveles y no dos: "fail" hace salir con 1 (algo no funciona) y "warn"
// no (el completado sin cargar no rompe nada; un doctor que falla por eso no
// se puede usar en un script).

const (
	doctorOK   = "ok"
	doctorWarn = "warn"
	doctorFail = "fail"
)

type doctorCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// doctorInput es lo que doctorChecks necesita saber del mundo; se recoge
// aparte para poder probar las decisiones sin daemon ni extensiones.
type doctorInput struct {
	endpoint   string
	cliVersion string
	info       *api.Info
	infoErr    error
	// snaps son los dorados del daemon (nil si no se pudieron listar): los
	// stale no se pueden restaurar con el VMM de ahora.
	snaps []*api.Snapshot
	// images: para avisar de agentes de invitado viejos (checkAgentesViejos).
	images     []api.Image
	plugins    []*plugin.Plugin
	completion bool // $_KLING_COMPLETION: lo exportan los scripts de completado
	// completionRC es que el rc de la shell carga el fichero que escribió
	// `kling completion install`: cargado en la próxima shell aunque no en
	// esta (la del instalador, por ejemplo).
	completionRC bool
	shell        string   // la de quien teclea ("zsh", "bash", "fish" o "")
	extDir       string   // directorio donde `kling plugin install` deja las extensiones
	extDirOK     bool     // existe
	companions   []string // compañeros (kling-bridge…) que viven en extDir
	pathDirs     []string // $PATH partido
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	upArgs := []string{"-check"}
	if *host != "" {
		upArgs = append(upArgs, "-H", *host)
	}
	// El runtime lo diagnostica `kling up -check`, que ya imprime cada
	// comprobación con su arreglo; no se duplica aquí. En JSON su texto va
	// dentro del informe.
	var runtimeOut string
	var runtimeErr error
	if *asJSON {
		runtimeOut, runtimeErr = captureStdout(func() error { return cmdUp(upArgs) })
	} else {
		fmt.Println("── runtime (kling up -check) ──")
		runtimeErr = cmdUp(upArgs)
		fmt.Println()
		fmt.Println("── kling ──")
	}

	endpoint := hostOf(*host)
	in := doctorInput{
		endpoint:   endpoint,
		cliVersion: Version,
		plugins:    extensions().Plugins,
		completion: os.Getenv("_KLING_COMPLETION") != "",
		shell:      userShell(),
		extDir:     extensionsDir(),
		pathDirs:   filepath.SplitList(os.Getenv("PATH")),
	}
	in.completionRC = completionInRC(in.shell)
	if fi, err := os.Stat(in.extDir); err == nil && fi.IsDir() {
		in.extDirOK = true
		in.companions = companionsInDir(in.extDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	in.info, in.infoErr = api.NewClient(endpoint).Info(ctx)
	if in.infoErr == nil {
		in.snaps, _ = api.NewClient(endpoint).Snapshots(ctx)
		in.images, _ = api.NewClient(endpoint).Images(ctx)
	}
	cancel()

	checks := doctorChecks(in)
	rt := doctorCheck{Name: "runtime", State: doctorOK, Detail: "kling up -check"}
	if runtimeErr != nil {
		rt.State, rt.Detail, rt.Fix = doctorFail, runtimeErr.Error(), "kling up"
	}
	checks = append([]doctorCheck{rt}, checks...)

	failed := 0
	for _, c := range checks {
		if c.State == doctorFail {
			failed++
		}
	}
	if *asJSON {
		var tuning map[string]string
		if in.info != nil {
			tuning = in.info.Tuning
		}
		out := struct {
			OK      bool              `json:"ok"`
			Checks  []doctorCheck     `json:"checks"`
			Runtime string            `json:"runtime_report,omitempty"`
			Tuning  map[string]string `json:"tuning,omitempty"`
		}{failed == 0, checks, runtimeOut, tuning}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			return err
		}
	} else {
		// La línea del runtime ya la ha impreso up; aquí solo lo demás.
		writeDoctor(os.Stdout, checks[1:])
		if in.info != nil {
			writeTuning(os.Stdout, in.info.Tuning)
		}
	}
	if failed > 0 {
		// El informe ya lo dice todo: solo el código de salida.
		return &errConCodigo{code: 1, err: errDoctorQuiet}
	}
	return nil
}

// errDoctorQuiet hace salir con 1 sin otro "error:" debajo del informe.
var errDoctorQuiet = errors.New("")

func doctorChecks(in doctorInput) []doctorCheck {
	var out []doctorCheck

	d := doctorCheck{Name: "daemon"}
	switch {
	case in.infoErr != nil || in.info == nil:
		d.State, d.Detail = doctorFail, fmt.Sprintf("not responding at %s: %v", in.endpoint, in.infoErr)
		d.Fix = "kling up   (or point kling at yours: kling context add <name> ssh://user@host)"
		if h := hintFor(in.infoErr); strings.HasPrefix(h, "add your user") {
			d.Fix = h
		}
	default:
		d.State, d.Detail = doctorOK, fmt.Sprintf("%s at %s, %d machine(s)", in.info.Version, in.endpoint, in.info.Machines)
	}
	out = append(out, d)

	if in.infoErr == nil && in.info != nil {
		v := doctorCheck{Name: "versions"}
		cli, dmn := strings.TrimPrefix(in.cliVersion, "v"), strings.TrimPrefix(in.info.Version, "v")
		switch {
		case cli == "dev" || dmn == "dev" || cli == "" || dmn == "":
			v.State, v.Detail = doctorOK, fmt.Sprintf("CLI %s, daemon %s (development build: not compared)", in.cliVersion, in.info.Version)
		case cli == dmn:
			v.State, v.Detail = doctorOK, "CLI and daemon are both "+in.cliVersion
		default:
			// No es un fallo: el API es compatible hacia atrás y un CLI nuevo
			// con un daemon viejo es el estado normal a mitad de actualizar.
			// Pero explica los 404 de las funciones nuevas.
			v.State = doctorWarn
			v.Detail = fmt.Sprintf("CLI %s, daemon %s: newer commands may fail with 404", in.cliVersion, in.info.Version)
			v.Fix = "upgrade the older one and restart the daemon (kling up)"
		}
		out = append(out, v)
		if c, ok := checkCoW(in.info.CoW); ok {
			out = append(out, c)
		}
		if c, ok := checkAuthz(in.info.Authz); ok {
			out = append(out, c)
		}
		if c, ok := checkStale(delMismoVMM(in.snaps, in.info)); ok {
			out = append(out, c)
		}
		if c, ok := checkOtroVMM(in.snaps, in.info); ok {
			out = append(out, c)
		}
		if c, ok := checkAgentesViejos(in.images, in.info.Version); ok {
			out = append(out, c)
		}
		// Máquinas a medio camino que el daemon sigue reintentando: un freeze
		// que no pudo reanudarla, un rm cuyo VMM sobrevivió al SIGKILL.
		for _, a := range in.info.Stuck {
			out = append(out, doctorCheck{Name: "machine " + a.Name, State: doctorWarn,
				Detail: a.Transition + ": " + a.Problem,
				Fix:    fmt.Sprintf("if it does not clear by itself, look at pid %d (ps -p %d) and restart the daemon", a.PID, a.PID)})
		}
		for _, p := range in.info.PoolProblems {
			c := doctorCheck{Name: "pool " + p.Template, State: doctorWarn,
				Detail: fmt.Sprintf("%d/%d ready; could not pre-warm a copy: %s", p.Ready, p.Size, firstLine(p.Error))}
			if p.Stale != "" {
				c.Detail = fmt.Sprintf("%d/%d ready; its golden is stale (%s)", p.Ready, p.Size, p.Stale)
				c.Fix = strings.Join(strings.Fields(strings.ReplaceAll(p.Fix, "\n", " | ")), " ")
			}
			out = append(out, c)
		}
	}

	for _, p := range in.plugins {
		c := doctorCheck{Name: "extension " + p.Name}
		src := p.Path
		if p.Builtin != nil {
			src = "built in"
		}
		switch {
		case p.Disabled:
			// Desactivarla es una decisión, no una avería.
			c.State, c.Detail = doctorOK, "disabled"
			c.Fix = ""
		case p.Err != nil:
			c.State, c.Detail = doctorFail, fmt.Sprintf("%v (%s)", p.Err, src)
			c.Fix = "kling plugin install " + p.Name
			if _, serr := plugin.ReadSidecar(p.Path); p.Path != "" && serr != nil && !strings.Contains(p.Err.Error(), "needs kling") {
				// Sin .json no la instaló kling: es un kling-* suelto en el
				// PATH que no contesta al manifiesto, casi siempre un
				// compañero de otra cosa.
				c.Fix = fmt.Sprintf("if it is not a kling extension, rename it or move it off PATH; else reinstall: kling plugin install %s", p.Name)
			}
			if strings.Contains(p.Err.Error(), "needs kling") {
				c.Fix = "upgrade kling, or install a matching extension: kling plugin install " + p.Name
			}
		default:
			ver := ""
			if p.Manifest != nil {
				ver = p.Manifest.Version
			}
			c.State, c.Detail = doctorOK, strings.TrimSpace(ver+" ("+src+")")
			if len(p.Shadowed) > 0 {
				c.State = doctorWarn
				c.Detail += "; ignored commands already taken: " + strings.Join(p.Shadowed, " ")
				c.Fix = "remove the older copy (kling plugin ls shows where each one comes from)"
			}
		}
		out = append(out, c)
	}

	comp := doctorCheck{Name: "completion", State: doctorOK, Detail: "loaded in this shell"}
	switch {
	case in.completion:
	case in.completionRC:
		comp.Detail = "installed for " + in.shell + " (loads in your next shell)"
	default:
		comp.State, comp.Detail = doctorWarn, "not loaded in this shell (reload it after installing an extension)"
		comp.Fix = "kling completion install   (then add the line it prints to " + rcFile(in.shell) + ")"
	}
	out = append(out, comp)

	dir := doctorCheck{Name: "extensions dir", State: doctorOK, Detail: in.extDir}
	switch {
	case !in.extDirOK:
		dir.Detail += " (does not exist yet; kling plugin install creates it)"
	case len(in.companions) > 0 && !inDirs(in.extDir, in.pathDirs):
		// kling encuentra las extensiones igual; lo que no se encuentra son los
		// ejecutables que las acompañan (kling-bridge) si alguien los llama a
		// mano. Sin compañeros ahí no hay nada que avisar.
		dir.State = doctorWarn
		dir.Detail += fmt.Sprintf(" (searched by kling, but not on PATH: %s installed there cannot be run by name)", strings.Join(in.companions, ", "))
		dir.Fix = fmt.Sprintf(`export PATH="%s:$PATH"`, in.extDir)
	}
	out = append(out, dir)
	return out
}

// checkStale avisa de los dorados que no se pueden restaurar con el VMM de
// ahora (otra versión de Firecracker o de kling-vz, docs/actualizar.md): run
// -from da 409 con ellos. Sin ninguno no hay línea: no es algo que haya que
// tener presente cuando todo está bien.
// printPoolProblems dice en `kling status` qué fondos precalentados no se
// reponen y qué teclear: tras cambiar de VMM (vz → hv) su dorado sale
// obsoleto y, sin esto, solo lo contaba el log del daemon.
func printPoolProblems(ps []api.PoolInfo) {
	for _, p := range ps {
		if p.Stale != "" {
			fmt.Printf("pool:         ✗ %s: %d/%d ready, its golden is stale (%s). Re-save it:\n", p.Template, p.Ready, p.Size, p.Stale)
			for _, l := range strings.Split(p.Fix, "\n") {
				fmt.Printf("              %s\n", strings.TrimSpace(l))
			}
			continue
		}
		fmt.Printf("pool:         ✗ %s: %d/%d ready, could not pre-warm a copy: %s\n", p.Template, p.Ready, p.Size, firstLine(p.Error))
	}
}

// vmmDeBackend es el nombre con que se sella un dorado hecho con ese
// backend (meta.go: "kling-hv 0.19.0").
func vmmDeBackend(b string) string {
	switch b {
	case "", "firecracker":
		return "firecracker"
	}
	return "kling-" + b
}

// checkOtroVMM lista los dorados sellados por otro VMM que el del daemon: lo
// que deja un cambio de vz a hv (daemon.vmm auto en un Mac que se actualizó).
// Ninguno se puede restaurar; se rehacen o se vuelve al VMM de antes.
func checkOtroVMM(snaps []*api.Snapshot, info *api.Info) (doctorCheck, bool) {
	if info == nil {
		return doctorCheck{}, false
	}
	ahora := vmmDeBackend(info.Backend)
	porVMM := map[string][]string{}
	var vmms []string
	for _, s := range snaps {
		if s == nil || s.VMM == "" {
			continue
		}
		v, _, _ := strings.Cut(s.VMM, " ")
		if v == ahora {
			continue
		}
		if porVMM[v] == nil {
			vmms = append(vmms, v)
		}
		porVMM[v] = append(porVMM[v], s.Name)
	}
	if len(vmms) == 0 {
		return doctorCheck{}, false
	}
	sort.Strings(vmms)
	var partes []string
	volver := ""
	for _, v := range vmms {
		ns := porVMM[v]
		sort.Strings(ns)
		shown := ns
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(ns)-5))
		}
		partes = append(partes, fmt.Sprintf("%d made with %s: %s", len(ns), v, strings.Join(shown, ", ")))
		if b, ok := strings.CutPrefix(v, "kling-"); ok && volver == "" && info.Backend != "firecracker" {
			volver = "; or go back to it: kling config set daemon.vmm " + b + " and restart the daemon"
		}
	}
	return doctorCheck{Name: "templates (vmm)", State: doctorWarn,
		Detail: fmt.Sprintf("this daemon runs %s; %s", ahora, strings.Join(partes, "; ")),
		Fix:    "re-save each: kling save -replace <machine> <name> (kling mcp import <name> -force for an imported MCP service)" + volver}, true
}

// delMismoVMM quita de snaps los sellados por otro VMM: esos los cuenta
// checkOtroVMM, con su arreglo.
func delMismoVMM(snaps []*api.Snapshot, info *api.Info) []*api.Snapshot {
	ahora := vmmDeBackend(info.Backend)
	var out []*api.Snapshot
	for _, s := range snaps {
		if s != nil && s.VMM != "" {
			if v, _, _ := strings.Cut(s.VMM, " "); v != ahora {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// checkAgentesViejos avisa de las imágenes cuyo agente de invitado es viejo
// para este daemon (agenteViejo): siguen funcionando, degradadas.
func checkAgentesViejos(imgs []api.Image, kling string) (doctorCheck, bool) {
	var names []string
	var por string
	for _, img := range imgs {
		if p := agenteViejo(img.Agent, kling); p != "" {
			names = append(names, img.Name)
			por = p
		}
	}
	if len(names) == 0 {
		return doctorCheck{}, false
	}
	shown := names
	if len(shown) > 5 {
		shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(names)-5))
	}
	detail := fmt.Sprintf("%d with an old guest agent (they work, without what it lacks): %s", len(names), strings.Join(shown, ", "))
	if len(names) == 1 {
		detail = fmt.Sprintf("%s: %s (it works, without what it lacks)", names[0], por)
	}
	return doctorCheck{Name: "images", State: doctorWarn, Detail: detail,
		Fix: "kling image ls says which; " + pistaRehacer}, true
}

func checkStale(snaps []*api.Snapshot) (doctorCheck, bool) {

	var names []string
	for _, s := range snaps {
		if s != nil && s.Stale != "" {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return doctorCheck{}, false
	}
	shown := names
	if len(shown) > 5 {
		shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(names)-5))
	}
	return doctorCheck{Name: "templates", State: doctorWarn,
		Detail: fmt.Sprintf("%d stale (made with another VMM version, can't be restored): %s", len(names), strings.Join(shown, ", ")),
		Fix:    "kling template inspect <name> says why; re-save it with kling save -replace <machine> <name>"}, true
}

// completionInRC dice si el rc de la shell ya carga el completado instalado
// por `kling completion install` (el fichero existe y el rc lo nombra).
func completionInRC(shell string) bool {
	if shell == "" {
		return false
	}
	path, _ := completionInstallPath(shell)
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	rc := filepath.Join(home, strings.TrimPrefix(rcFile(shell), "~/"))
	b, err := os.ReadFile(rc)
	return err == nil && bytes.Contains(b, []byte(path))
}

// companionsInDir son los ejecutables compañeros (declarados en los .json de
// las extensiones instaladas) que están en dir.
func companionsInDir(dir string) []string {
	var out []string
	for _, c := range plugin.CompanionsIn(dir) {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func inDirs(dir string, dirs []string) bool {
	clean := filepath.Clean(dir)
	for _, d := range dirs {
		if d != "" && filepath.Clean(d) == clean {
			return true
		}
	}
	return false
}

func writeDoctor(w io.Writer, checks []doctorCheck) {
	m := uiMarks()
	mark := map[string]string{doctorOK: m.ok, doctorWarn: m.warn, doctorFail: m.fail}
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	failed, warned := 0, 0
	for _, c := range checks {
		fmt.Fprintf(w, "%s %-*s  %s\n", mark[c.State], width, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "  %-*s  fix: %s\n", width, "", c.Fix)
		}
		switch c.State {
		case doctorFail:
			failed++
		case doctorWarn:
			warned++
		}
	}
	fmt.Fprintln(w)
	switch {
	case failed > 0:
		fmt.Fprintf(w, "%d problem(s), %d warning(s): run the fix lines above\n", failed, warned)
	case warned > 0:
		fmt.Fprintf(w, "everything works; %d warning(s) above\n", warned)
	default:
		fmt.Fprintln(w, "everything looks good")
	}
}

// writeTuning enseña los ajustes efectivos del daemon (GET /info, tuning):
// con qué topes y políticas corre de verdad, que es lo primero que hay que
// saber cuando rechaza máquinas o expulsa dormidas. Un daemon anterior no los
// manda y no se imprime nada.
func writeTuning(w io.Writer, tuning map[string]string) {
	if len(tuning) == 0 {
		return
	}
	keys := make([]string, 0, len(tuning))
	width := 0
	for k := range tuning {
		keys = append(keys, k)
		width = max(width, len(k))
	}
	sort.Strings(keys)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "── daemon tuning (effective values; set on the daemon's environment) ──")
	for _, k := range keys {
		fmt.Fprintf(w, "  %-*s  %s\n", width, k, tuning[k])
	}
}

// extensionsDir es donde `kling plugin install` deja las extensiones; la
// regla (contrato plugin-dirs) vive en pkg/plugin para que install y doctor no
// puedan discrepar.
func extensionsDir() string {
	d, err := plugin.InstallDir()
	if err != nil {
		return ""
	}
	return d
}

// captureStdout ejecuta fn con os.Stdout apuntando a una tubería y devuelve lo
// que escribió (hasta 1 MiB). Sirve para meter el informe de `up -check`, que
// escribe directamente en os.Stdout, dentro del JSON de doctor.
func captureStdout(fn func() error) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", fn()
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, io.LimitReader(r, 1<<20))
		_, _ = io.Copy(io.Discard, r) // lo que pase del límite se tira, sin bloquear a fn
		done <- b.String()
	}()
	ferr := fn()
	os.Stdout = orig
	w.Close()
	out := <-done
	r.Close()
	return out, ferr
}

// checkAuthz cuenta si el daemon autoriza por operación (docs/authz.md) y con
// qué rol ve a quien pregunta. Sin política es un aviso, no un fallo: en un
// host de un solo usuario es lo normal, pero conviene saber que quien alcance
// el socket manda sobre todo. Un daemon anterior no dice nada y no se opina.
func checkAuthz(a *api.AuthzInfo) (doctorCheck, bool) {
	if a == nil {
		return doctorCheck{}, false
	}
	c := doctorCheck{Name: "authz"}
	switch {
	case !a.Enabled:
		c.State = doctorWarn
		c.Detail = "no authz policy: whoever reaches the daemon's socket controls everything"
		c.Fix = "fine on a single-user host; to share it, write /etc/kling/authz.json and restart the daemon (docs/authz.md)"
	case a.Role == "" || a.Role == "none":
		c.State = doctorFail
		c.Detail = "the daemon has an authz policy and gives you no role"
		if a.UID != nil {
			c.Detail = fmt.Sprintf("the daemon has an authz policy and gives uid %d no role", *a.UID)
		}
		c.Fix = "ask the daemon's admin for a rule in its authz policy (docs/authz.md)"
	default:
		c.State, c.Detail = doctorOK, "policy on; you are "+a.Role
	}
	return c, true
}
