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
		out := struct {
			OK      bool          `json:"ok"`
			Checks  []doctorCheck `json:"checks"`
			Runtime string        `json:"runtime_report,omitempty"`
		}{failed == 0, checks, runtimeOut}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			return err
		}
	} else {
		// La línea del runtime ya la ha impreso up; aquí solo lo demás.
		writeDoctor(os.Stdout, checks[1:])
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
