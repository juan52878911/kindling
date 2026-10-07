package main

import (
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
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] == "--" {
		return fmt.Errorf("usage: kling image import <ref> [-name N] [-e K=V] [-env-file F] [-json] [-- cmd args...]")
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
	spec := OCISpec{Ref: rest[0], Arch: *arch, User: *user, MaxMB: *maxSize, Env: env, Restart: *restart}
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
	rec, err := c.ImageRecipe(ctx, res.Name)
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
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"name": res.Name, "ref": built.Ref, "digest": built.Digest,
			"manifest": built.Manifest, "arch": built.Arch, "ports": built.Ports, "volumes": built.Volumes,
			"ready": built.Ready, "service": built.Service})
	}
	fmt.Print(res.Output)
	fmt.Printf("image %s: %s\n", res.Name, built.Digest)
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
	if built.Ready != "" {
		fmt.Printf("  ready    %s\n", built.Ready)
	}
	if len(built.Volumes) > 0 {
		// No se sugiere montar el volumen justo ahí: un volumen de kling
		// trae lost+found, y initdb (y otros) no aceptan un directorio que
		// no esté vacío. En el padre, o con un subdirectorio (PGDATA).
		fmt.Printf("  volumes  %s  (keep data with -volume NAME:<a parent dir>)\n", strings.Join(built.Volumes, " "))
	}
	next("kling run -image %s -mem 512M -wait-ready", res.Name)
	return nil
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

// runsLine es la orden del servicio para la salida. Los argumentos que puso
// quien importa (-entrypoint, después de --) no se repiten: pueden llevar una
// contraseña que no debe acabar en un log de CI.
func runsLine(argv []string, fromCommandLine bool) string {
	if !fromCommandLine || len(argv) < 2 {
		return strings.Join(argv, " ")
	}
	return fmt.Sprintf("%s … (%d arguments from the command line)", argv[0], len(argv)-1)
}
