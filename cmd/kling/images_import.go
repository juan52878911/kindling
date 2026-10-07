package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
	"github.com/juan52878911/kindling/pkg/units"
)

// imagesImport es `kling image import <ref>`: una imagen de Docker/OCI como
// imagen de kindling, con el constructor "oci" del daemon (builder_oci.go).
//
//	kling image import postgres:17-alpine
//	kling image import docker.io/timescale/timescaledb:latest-pg16 -name tsdb -env-file pg.env
//	kling image import redis:7 -json -- redis-server --save ""
//
// Las variables -e KEY=valor van en la línea de órdenes (cualquiera las ve
// en /proc/<pid>/cmdline): para una contraseña, -e KEY (del entorno) o
// -env-file. En todos los casos quedan en la imagen (/etc/kling/env, 0600
// de root) y viajan al daemon en el cuerpo de la petición, no en un argv.
// Se mantiene por compatibilidad, con un aviso: lo que es de cada máquina
// va en `kling run -e`, que no lo hornea (pkg/api/machine_env.go).
func imagesImport(args []string) error {
	fs := flag.NewFlagSet("image import", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "image name (default: from the reference, e.g. postgres-17-alpine)")
	arch := fs.String("arch", "", "amd64 or arm64 (default: the daemon's)")
	var ef envFlags
	ef.register(fs)
	user := fs.String("user", "", "run the entrypoint as uid[:gid] or name[:group] instead of the image's USER")
	var entrypoint stringsFlag
	fs.Var(&entrypoint, "entrypoint", "replace the image's ENTRYPOINT (repeatable, one argument each; drops its CMD)")
	restart := fs.String("restart", "", "restart the service when it exits: always, on-failure or no (default on-failure)")
	maxSize := units.MiBVar(fs, "max-size", 0, "refuse images bigger than this, compressed: 2G, 800M (default 4G)")
	asJSON := fs.Bool("json", false, "print the result as JSON (for scripts and agents)")
	replace := fs.Bool("replace", false, "overwrite an image with that name, and re-import one already imported (re-resolves the tag)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] == "--" {
		return fmt.Errorf("usage: kling image import <ref> [-name N] [-replace] [-e K=V] [-env-file F] [-json] [-- cmd args...]")
	}
	ref, err := oci.ParseImageRef(rest[0])
	if err != nil {
		return err
	}
	env, err := ef.resolve()
	if err != nil {
		return err
	}
	if len(env) > 0 {
		fmt.Fprintln(os.Stderr, "warning: -e and -env-file bake the values into the image (/etc/kling/env) and every copy of it; "+
			"for a password or anything per machine, use kling run -image <image> -e KEY instead")
	}
	switch *restart {
	case "", api.RestartAlways, api.RestartOnFailure, api.RestartNo:
	default:
		return fmt.Errorf("-restart must be always, on-failure or no, not %q", *restart)
	}
	spec := OCISpec{Ref: rest[0], Arch: *arch, User: *user, MaxMB: *maxSize, Env: env,
		// Explícita en la receta: una sin ella es de antes de las políticas
		// (ver existingImport).
		Restart: cmp.Or(*restart, api.RestartOnFailure)}
	if len(entrypoint) > 0 {
		spec.Entrypoint = entrypoint
	}
	switch {
	case len(rest) > 2 && rest[1] == "--":
		spec.Cmd = rest[2:]
	case len(rest) > 1:
		return fmt.Errorf("unexpected argument %q; the command goes after --", rest[1])
	}
	for _, kv := range spec.Env {
		if !reBuildEnv.MatchString(kv) {
			// Sin el valor: puede ser una contraseña.
			k, _, _ := strings.Cut(kv, "=")
			return fmt.Errorf("invalid environment entry %q: use KEY=value, one line", k)
		}
	}
	if *name == "" {
		*name = imageNameFor(ref)
	}
	sb, _ := json.Marshal(spec)
	req := api.BuildImageRequest{Name: *name, Builder: "oci", Spec: sb}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	// El nombre por defecto sale solo del repositorio y la etiqueta: redis:7
	// y ghcr.io/x/redis:7 dan los dos redis-7. Sin -replace no se pisa una
	// imagen que no sea esta misma importación.
	output := ""
	already := false
	if !*replace {
		if already, err = existingImport(ctx, c, *name, spec); err != nil {
			return err
		}
	}
	if !already {
		if !*asJSON {
			fmt.Printf("importing %s as %s (the first time downloads it)...\n", ref, *name)
		}
		res, err := c.BuildImage(ctx, req)
		if err != nil {
			if res != nil && res.Output != "" && !*asJSON {
				fmt.Print(res.Output)
			}
			return err
		}
		*name, output = res.Name, res.Output
	}
	rec, err := c.ImageRecipe(ctx, *name)
	if err != nil {
		return err
	}
	var built struct {
		Ref      string          `json:"ref"`
		Digest   string          `json:"digest"`
		Manifest string          `json:"manifest"`
		Arch     string          `json:"arch"`
		Ports    []string        `json:"ports"`
		Volumes  []string        `json:"volumes"`
		Ready    string          `json:"ready"`
		Service  api.ServiceSpec `json:"service"`
	}
	_ = json.Unmarshal(rec.Built, &built)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"name": *name, "already_imported": already, "ref": built.Ref, "digest": built.Digest,
			"manifest": built.Manifest, "arch": built.Arch, "ports": built.Ports, "volumes": built.Volumes,
			"ready": built.Ready, "service": built.Service})
	}
	if already {
		dmn := ""
		if in, err := c.Info(ctx); err == nil {
			dmn = in.Version
		}
		fmt.Println(alreadyImportedLine(*name, ref.String(), rec.KlingVer, dmn))
	}
	fmt.Print(output)
	fmt.Printf("image %s: %s\n", *name, built.Digest)
	if len(built.Service.Argv) > 0 {
		fmt.Printf("  runs     %s", runsLine(built.Service.Argv, len(spec.Cmd)+len(spec.Entrypoint) > 0))
		if built.Service.User != "" {
			fmt.Printf("  (as %s)", built.Service.User)
		}
		fmt.Println()
	}
	if len(built.Ports) > 0 {
		fmt.Printf("  ports    %s\n", strings.Join(built.Ports, " "))
	}
	if r := readyLine(built.Ready, built.Ports); r != "" {
		fmt.Printf("  ready    %s\n", r)
	}
	if len(built.Volumes) > 0 {
		// No se sugiere montar el volumen justo ahí: un volumen de kling
		// trae lost+found, y initdb (y otros) no aceptan un directorio que
		// no esté vacío. En el padre, o con un subdirectorio (PGDATA).
		fmt.Printf("  volumes  %s  (keep data with -volume NAME:<a parent dir>)\n", strings.Join(built.Volumes, " "))
	}
	next("kling run -image %s -mem 512M -wait-ready", *name)
	return nil
}

// existingImport mira si ya hay una imagen con ese nombre. Sin ella, false.
// Si es esta misma importación (constructor oci y el mismo spec, con la
// referencia normalizada), true: no hace falta rehacerla. Cualquier otra
// cosa es un error: pisarla en silencio cambiaría lo que arrancan las
// máquinas y plantillas que la usan. Los valores del spec (puede llevar una
// contraseña en env) no salen en ningún mensaje.
func existingImport(ctx context.Context, c *api.Client, name string, spec OCISpec) (bool, error) {
	imgs, err := c.Images(ctx)
	if err != nil {
		return false, err
	}
	var img *api.Image
	for i := range imgs {
		if imgs[i].Name == name {
			img = &imgs[i]
		}
	}
	if img == nil {
		return false, nil
	}
	// Sin receta no se sabe de dónde salió (puede que sí de una imagen de
	// Docker, con un kling que no las guardaba): no se dice que no.
	if !img.HasRecipe {
		return false, fmt.Errorf("image %s already exists and has no recipe; pick another -name, or pass -replace to overwrite it", name)
	}
	rec, err := c.ImageRecipe(ctx, name)
	if err != nil {
		return false, err
	}
	var prev OCISpec
	if rec.Builder != "oci" || json.Unmarshal(rec.Spec, &prev) != nil {
		return false, fmt.Errorf("image %s already exists and wasn't imported from a Docker image; "+
			"pick another -name, or pass -replace to overwrite it", name)
	}
	pr, perr := oci.ParseImageRef(prev.Ref)
	nr, nerr := oci.ParseImageRef(spec.Ref)
	if perr != nil || nerr != nil || pr.String() != nr.String() || prev.Digest != spec.Digest {
		from := prev.Ref
		if perr == nil {
			from = pr.String()
		}
		return false, fmt.Errorf("image %s already exists, imported from %s; pick another -name, or pass -replace to overwrite it", name, from)
	}
	prev.Ref, spec.Ref = "", ""
	// Sin -restart la política es on-failure. Una receta sin política es de
	// antes de que existieran, y su servicio se relanza siempre: darla por
	// la misma dejaba relanzando un CMD que acaba con 0 (python:3.12-slim).
	if prev.Restart == "" {
		return false, fmt.Errorf("image %s was imported before restart policies (its service always restarts); "+
			"pass -replace to rebuild it", name)
	}
	spec.Restart = cmp.Or(spec.Restart, api.RestartOnFailure)
	a, _ := json.Marshal(prev)
	b, _ := json.Marshal(spec)
	if string(a) != string(b) {
		return false, fmt.Errorf("image %s was imported from %s with other options (env, user, entrypoint, command, arch, size limit or restart policy); "+
			"pass -replace to rebuild it with these", name, nr)
	}
	return true, nil
}

// alreadyImportedLine es lo que se dice cuando la importación ya estaba. Si la
// construyó otra versión del daemon (otro init, otro agente), se dice cuál y
// que -replace la rehace con esta: si no, una imagen importada hace varias
// versiones se queda así para siempre sin que nadie lo note. Con una versión
// de desarrollo, o sin saber la del daemon, no se compara (como doctor).
func alreadyImportedLine(name, ref, builtBy, daemon string) string {
	b, d := strings.TrimPrefix(builtBy, "v"), strings.TrimPrefix(daemon, "v")
	if d == "" || d == "dev" || b == "dev" || b == d {
		return fmt.Sprintf("image %s: already imported from %s (-replace re-resolves the tag and rebuilds it)", name, ref)
	}
	by := "an earlier kling"
	if b != "" {
		by = "kling " + builtBy
	}
	return fmt.Sprintf("image %s: already imported from %s, built by %s; -replace rebuilds it with this version (%s, and re-resolves the tag)",
		name, ref, by, daemon)
}

var reNameJunk = lazyre.New(`[^a-z0-9_-]+`)

// imageNameFor es el nombre por defecto: el último trozo del repositorio y
// la etiqueta si no es latest ("postgres-17-alpine", "timescaledb-latest-pg16").
func imageNameFor(r oci.ImageRef) string {
	n := path.Base(r.Repo)
	if r.Tag != "" && r.Tag != "latest" {
		n += "-" + r.Tag
	}
	n = strings.Trim(reNameJunk.ReplaceAllString(strings.ToLower(n), "-"), "-_")
	if len(n) > 64 {
		n = strings.TrimRight(n[:64], "-_")
	}
	if n == "" {
		n = "image"
	}
	return n
}

// readyLine es la línea "ready" de la salida. Sin sonda pero con puertos
// (solo UDP: la sonda solo sabe de TCP) se dice, para que -wait-ready no se
// lea como "el servicio contesta". Sin puertos ni sonda, nada.
func readyLine(ready string, ports []string) string {
	if ready != "" || len(ports) == 0 {
		return ready
	}
	return "none: no HEALTHCHECK and no TCP port in EXPOSE (" + strings.Join(ports, " ") + "), so -wait-ready won't wait for the service"
}

// runsLine es la orden del servicio para la salida. Los argumentos que puso
// quien importa (-entrypoint, después de --) no se repiten: pueden llevar una
// contraseña que no debe acabar en un log de CI.
func runsLine(argv []string, fromCommandLine bool) string {
	if !fromCommandLine || len(argv) < 2 {
		return strings.Join(argv, " ")
	}
	return fmt.Sprintf("%s … (%d arguments from the command line)", argv[0], len(argv)-1)
}
