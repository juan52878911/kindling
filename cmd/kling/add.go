package main

// `kling search` y `kling add`: traer un servidor MCP del registro oficial y
// dejarlo listo como servicio, sin pasar por los scripts a mano.
//
// Es un envoltorio fino sobre lo que ya existe. Todo el trabajo real lo hacen
// 80-mcp-image.sh (empaquetar) y `mcp import` (arrancar, preguntar qué sabe
// hacer, congelar y guardar el catálogo). Lo único nuevo es traducir una
// entrada del registro a la invocación correcta, que tiene más trampas de las
// que parece.

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
	"github.com/juan52878911/kindling/internal/registry"
)

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	limit := fs.Int("n", 20, "how many results")
	fresh := fs.Bool("refresh", false, "ignore the cache")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kling search <query>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	rc := registry.New()
	rc.Fresh = *fresh
	servers, err := rc.Search(ctx, strings.Join(fs.Args(), " "), *limit)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		fmt.Println("no results")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tkling add\tDESCRIPTION")
	for _, s := range servers {
		ok := "no"
		if _, can := s.Stdio(); can {
			ok = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.Version, ok, truncate(s.Description, 60))
	}
	_ = w.Flush()
	fmt.Println("\n\"kling add no\" means that server doesn't speak stdio over npm or PyPI,")
	fmt.Println("which is what kindling knows how to package on its own. See scripts/80-mcp-image.sh.")
	return nil
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	host := hostFlag(fs)
	as := fs.String("as", "", "service name (default: the server's, without namespace)")
	extra := multiFlag(fs, "arg", "value for a required argument of the server; repeatable")
	dryRun := fs.Bool("dry-run", false, "show what it would do without doing it")
	// Sin esto, el camino recomendado para añadir un servidor no puede darle
	// almacenamiento persistente: habría que reimportarlo a mano después, y
	// reimportar es justo lo que un volumen obliga a hacer si se olvida (el
	// conjunto de discos queda fijado al congelar el snapshot dorado).
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volume to mount: name[:/mountpoint][:ro] (repeatable)")
	mount := fs.String("mount", "", "where to mount the volume (default /data; only with one)")
	volRO := fs.Bool("volume-ro", false, "mount it read-only: shareable between services")
	fresh := fs.Bool("refresh", false, "ignore the registry cache")
	bundle := fs.Bool("bundle", false, "bundle the node server into 1 file (esbuild) when building: starts much faster on cold start, especially on arm64/Mac")
	// Sin esto no se puede empaquetar sobre una base con el runtime dentro, que
	// es de donde sale TODO el ahorro de las imágenes por capas: sobre la base
	// mínima, la capa de un servidor node se lleva nodejs+npm dentro (~126 MiB) y
	// ahorra un par de megas. Sobre una base que ya los trae, baja a ~25 MiB.
	// Medido en fc-test — docs/three-layers.md. Sin el flag, se busca en el
	// daemon una base que se llame como la familia del runtime (node, python) —
	// las que construye 70-build-minimal-image.sh por nombre— y se usa sola.
	base := fs.String("base", "", "base image to build on (default: the runtime family's base if the daemon has one; e.g. 'node', 'python')")
	// Las variables van HORNEADAS en el entrypoint, en texto plano: sirven para
	// interruptores (SEMGREP_SEND_METRICS=off), no para secretos. Sin esto, un
	// servidor que exige una variable no se podía añadir por este camino.
	envs := multiFlag(fs, "env", "environment variable to bake into the image, KEY=value; repeatable (plaintext: not for secrets)")
	// La inferencia del ejecutable acierta en npm (el registro publica los bin)
	// pero en PyPI es una convención, y hay paquetes que no la siguen porque no
	// traen ejecutable ninguno: se arrancan con `python3 -m <módulo>`.
	cmd := fs.String("cmd", "", "command that starts the server, overriding the inferred one (e.g. \"python3 -m mcp_sqlite3\")")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kling add <server> [-as name] [-arg value]\n" +
			"     search for it first with:  kling search <query>")
	}

	vols, err := volumeSet(volumes, *mount, *volRO)
	if err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	rc := registry.New()
	rc.Fresh = *fresh

	opts := addOpts{as: *as, vols: vols, args: *extra, env: *envs,
		dryRun: *dryRun, bundle: *bundle, base: *base, cmd: *cmd}
	for _, want := range fs.Args() {
		if err := addOne(ctx, rc, *host, want, opts); err != nil {
			return fmt.Errorf("%s: %w", want, err)
		}
	}
	return nil
}

// addOpts agrupa lo que el usuario pidió para todos los servidores de la
// invocación. Existe porque pasar cada flag como parámetro suelto ya había
// dejado a addOne con nueve, y el décimo era el que rompía la legibilidad.
type addOpts struct {
	as     string
	vols   []api.VolumeAttachment
	args   []string
	env    []string
	dryRun bool
	bundle bool
	base   string
	cmd    string
}

// buildPlan es lo que cambia entre ecosistemas al empaquetar: qué runtime pide
// apk, por dónde se preinstala el servidor y cómo se llama la base de runtime
// sobre la que conviene apoyar la capa.
type buildPlan struct {
	apk    []string // runtime del sistema, para apk
	npm    []string // paquete a preinstalar con npm (uno u otro, nunca ambos)
	pip    []string // paquete a preinstalar con pip
	spec   string   // paquete con versión, para enseñarlo
	family string   // nombre de la base de runtime que kling busca en el daemon
}

// planFor traduce el paquete del registro a su plan de construcción.
//
// El runtime va SIEMPRE en apk, también con una base que ya lo trae: apk sobre
// lo instalado no engorda la capa —medido: 28 MiB contra 31—, y pedirlo
// igualmente hace que -base valga con cualquier base, en vez de fallar con un
// "npm: not found" a mitad de construcción.
func planFor(pkg registry.Package) (buildPlan, error) {
	switch pkg.RegistryType {
	case "npm":
		spec := pkg.Identifier
		if pkg.Version != "" {
			spec += "@" + pkg.Version
		}
		return buildPlan{apk: []string{"nodejs", "npm"},
			npm: []string{spec}, spec: spec, family: "node"}, nil
	case "pypi":
		// El nombre se normaliza (PEP 503) para que la imagen no dependa de
		// cómo lo escribió el autor en el registro: pip trata Mcp_Server.Git y
		// mcp-server-git como el mismo paquete.
		spec := registry.NormalizePyPI(pkg.Identifier)
		if pkg.Version != "" {
			spec += "==" + pkg.Version
		}
		return buildPlan{apk: []string{"python3", "py3-pip"},
			pip: []string{spec}, spec: spec, family: "python"}, nil
	}
	// No debería llegar: Stdio() solo devuelve npm o pypi. Si llega, es que
	// alguien amplió Stdio() sin ampliar esto.
	return buildPlan{}, fmt.Errorf("unsupported package registry %q", pkg.RegistryType)
}

// resolveBin averigua el ejecutable que la instalación deja en la imagen. El
// invitado arranca SIN red, así que un `npx -y`/`uvx` que descargue al vuelo
// fallaría: el paquete se preinstala y hay que invocarlo por su nombre real.
func resolveBin(ctx context.Context, pkg registry.Package) (string, error) {
	cl := &http.Client{Timeout: 30 * time.Second}
	if pkg.RegistryType == "pypi" {
		return registry.ResolvePyPIBin(ctx, cl, pkg.Identifier)
	}
	return registry.ResolveBin(ctx, cl, pkg.Identifier)
}

// pickBase busca en el daemon una base que se llame como la familia del
// runtime ("node", "python" — los nombres con los que las construye
// 70-build-minimal-image.sh). Sin ella, el camino cómodo empaquetaría sobre
// `min` y la capa cargaría con el runtime entero: todo el ahorro de las
// imágenes por capas depende de esta elección — docs/three-layers.md.
//
// Si el daemon no contesta no se adivina nada: se devuelve vacío y el build
// decidirá (o fallará) él solo. Un -dry-run sin daemon a mano tiene que seguir
// funcionando.
func pickBase(ctx context.Context, c *api.Client, family string) string {
	imgs, err := c.Images(ctx)
	if err != nil {
		return ""
	}
	for _, im := range imgs {
		if im.Name == family {
			return family
		}
	}
	return ""
}

func addOne(ctx context.Context, rc *registry.Client, host, want string, o addOpts) error {
	srv, candidates, err := rc.Get(ctx, want, 30)
	if err != nil {
		if len(candidates) > 0 {
			fmt.Printf("%v:\n", err)
			for _, c := range candidates {
				fmt.Printf("  %s  (%s)\n", c.Name, truncate(c.Description, 60))
			}
		}
		return err
	}

	pkg, ok := srv.Stdio()
	if !ok {
		return explainUnpackageable(srv)
	}

	plan, err := planFor(pkg)
	if err != nil {
		return err
	}

	// -bundle colapsa node_modules con esbuild: en Python no hay nada
	// equivalente que colapsar. Se avisa y se sigue, en vez de fallar: el flag
	// puede venir de un alias o de la costumbre, y el resto de la orden vale.
	bundle := o.bundle
	if bundle && len(plan.npm) == 0 {
		fmt.Println("note: -bundle only applies to node servers; ignored for this one")
		bundle = false
	}

	// Variables de entorno: las que el usuario dio con -env satisfacen las que
	// el servidor declara obligatorias. Las que falten paran aquí: mejor
	// negarse que construir un servicio que falle dentro del agente, que es
	// donde peor se diagnostica.
	given := map[string]string{}
	for _, kv := range o.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("invalid -env %q: expected KEY=value", kv)
		}
		given[k] = v
	}
	if missing := pkg.MissingEnv(given); len(missing) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s needs environment variables; pass them with -env NAME=value:\n", srv.Name)
		for _, v := range missing {
			secret := ""
			if v.IsSecret {
				secret = "  [secret]"
			}
			fmt.Fprintf(&b, "  %s%s  %s\n", v.Name, secret, truncate(v.Description, 60))
		}
		b.WriteString("\nCareful: -env bakes the value in PLAINTEXT into the image — fine for\n" +
			"switches, wrong for secrets. For secrets, package by hand: scripts/80-mcp-image.sh")
		return fmt.Errorf("%s", b.String())
	}
	// Un secreto por -env no se prohíbe —el dueño de la imagen decide—, pero
	// tampoco se deja pasar callando: queda en texto plano en la imagen Y en su
	// receta, y eso hay que saberlo antes de compartir ninguna de las dos.
	for _, v := range pkg.EnvironmentVars {
		if _, ok := given[v.Name]; ok && v.IsSecret {
			fmt.Printf("warning: %s is declared secret and will be baked in PLAINTEXT into the image and its recipe\n", v.Name)
		}
	}

	service := o.as
	if service == "" {
		service = serviceName(srv.Name)
	}

	// El comando dado a mano gana sobre el inferido, y ni siquiera se consulta a
	// PyPI: es la salida para los paquetes que no traen ejecutable.
	//
	// Hace falta más de lo que parece. Un servidor de PyPI que solo trae
	// __main__.py —mcp-sqlite3, el primero que probamos— se arranca con
	// `python3 -m mcp_sqlite3`, y no hay convención que lo adivine: el wheel no
	// declara ningún console_script. Sin esta puerta, esos servidores solo se
	// podían empaquetar bajando al script a mano.
	var bin string
	if o.cmd == "" {
		var err error
		if bin, err = resolveBin(ctx, pkg); err != nil {
			return err
		}
	}

	cmd, unmet := renderArgs(bin, pkg, o.args)
	if o.cmd != "" {
		// strings.Fields y no una sola palabra: lo natural aquí es escribir
		// `-cmd "python3 -m mcp_sqlite3"`, que son tres argumentos.
		cmd = strings.Fields(o.cmd)
		unmet = nil
	}
	if len(unmet) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "missing %d required argument(s); pass them with -arg:\n", len(unmet))
		for _, a := range unmet {
			fmt.Fprintf(&b, "  %s  %s\n", argLabel(a), truncate(a.Description, 60))
		}
		fmt.Fprintf(&b, "\ne.g.:  kling add %s -arg /data", want)
		return fmt.Errorf("%s", b.String())
	}

	// Elegir base: la explícita manda; sin ella se busca la de la familia del
	// runtime en el daemon. `-base min` sigue valiendo para forzar la mínima.
	c := api.NewClient(hostOf(host))
	base, autoBase := o.base, false
	if base == "" {
		// Acotado en corto: elegir base es una comodidad, y no puede convertir
		// un daemon caído en un cuelgue — un -dry-run tiene que contestar
		// aunque no haya nadie al otro lado.
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if base = pickBase(pctx, c, plan.family); base != "" {
			autoBase = true
		}
		cancel()
	}

	fmt.Printf("%s  v%s\n", srv.Name, srv.Version)
	fmt.Printf("  service:   %s\n", service)
	fmt.Printf("  %-9s  %s\n", pkg.RegistryType+":", plan.spec)
	fmt.Printf("  command:   %s\n", strings.Join(cmd, " "))
	for _, kv := range o.env {
		fmt.Printf("  env:       %s\n", kv)
	}
	switch {
	case autoBase:
		fmt.Printf("  base:      %s  (found the runtime base on the daemon; only the delta goes in this image)\n", base)
	case base != "":
		fmt.Printf("  base:      %s  (layered: only the delta goes in this image)\n", base)
	default:
		// Sin base de runtime la capa carga con el runtime entero y el ahorro
		// de las capas se esfuma. Se avisa AQUÍ, antes de construir, porque
		// después la única cura es reimportar.
		fmt.Printf("  base:      (none: layer will carry the whole runtime; build one with\n"+
			"              sudo ./scripts/70-build-minimal-image.sh %s  and re-add)\n", plan.family)
	}
	if o.dryRun {
		fmt.Println("\n(-dry-run: not doing anything)")
		return nil
	}
	fmt.Println()

	// 1. Empaquetar. Lo hace el daemon porque monta un loopback y hace chroot.
	fmt.Printf("  1/2  building the image (installs the runtime, may take a while)... ")
	res, err := c.BuildImage(ctx, api.BuildImageRequest{
		Name:     service,
		Packages: plan.apk,
		NPM:      plan.npm,
		PIP:      plan.pip,
		Env:      o.env,
		Cmd:      cmd,
		Bundle:   bundle,
		Base:     base,
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Printf("✓ %s\n", res.Path)

	// 2. Y el ciclo de importación que ya existe: arranca la plantilla, le
	//    pregunta qué sabe hacer, la congela como snapshot dorado y guarda el
	//    catálogo. Se reutiliza tal cual en vez de duplicar los cinco pasos.
	fmt.Printf("  2/2  importing the service\n")
	if err := runImport(hostOf(host), service, o.vols, loadConfig()); err != nil {
		return err
	}

	fmt.Printf("\nDone. Connect it:  kling connect %s -install all\n", service)
	return nil
}

// runImport ejecuta el `mcp import`, en local o en el host del daemon.
//
// Tiene que distinguirlo porque la introspección marca DIRECTAMENTE contra la
// IP de la microVM (172.30.x.x), que solo es enrutable desde el host donde
// corre el daemon. Desde un portátil apuntando por SSH, ese paso da un timeout
// que parece del servidor MCP y no lo es.
func runImport(endpoint, service string, vols []api.VolumeAttachment, cfg *config.Config) error {
	args := []string{service, "-image", service, "-force"}
	// Un -volume por cada uno, en el mismo ORDEN: ese orden es el de los discos
	// dentro de la microVM. Antes se pasaba un solo volumen como cadena, así que
	// un segundo -volume se perdía en silencio — y perder un volumen en silencio
	// significa que el servicio escribe en un overlay que muere con la máquina.
	for _, v := range vols {
		spec := v.Name
		if v.Mount != "" {
			spec += ":" + v.Mount
		}
		if v.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-volume", spec)
	}

	// Los valores por defecto se resuelven AQUÍ y viajan explícitos. Por SSH se
	// ejecuta el `kling` del otro lado, que leería SU configuración: los
	// defaults del portátil —donde vive el criterio de su dueño— no llegarían,
	// y el servicio se quedaría con los 256 MiB del daemon. Se nota tarde y
	// mal, porque la memoria queda grabada en el snapshot dorado.
	if m := config.Or(cfg.Defaults.MemMiB, 0); m > 0 {
		args = append(args, "-mem", strconv.Itoa(m))
	}
	if e := cfg.Defaults.Egress; e != "" {
		args = append(args, "-egress", e)
	}

	if !strings.HasPrefix(endpoint, "ssh://") {
		return mcpImport(args)
	}

	target := endpoint
	if rest, ok := strings.CutPrefix(target, "ssh://"); ok {
		target, _, _ = strings.Cut(rest, "/")
	}
	remote := append([]string{target, "kling", "mcp", "import"}, args...)

	cmd := exec.Command("ssh", remote...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("import failed on %s: %w\n"+
			"The image WAS built; retry that step there:\n"+
			"  ssh %s 'kling mcp import %s'",
			target, err, target, strings.Join(args, " "))
	}
	return nil
}

// explainUnpackageable dice por qué no se puede y qué hacer, en vez de un "no".
func explainUnpackageable(srv *registry.Server) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s can't be packaged automatically.\n", srv.Name)
	if len(srv.Remotes) > 0 {
		b.WriteString("\nIt's already hosted by its author, so there's no need to put it in a microVM:\n")
		for _, r := range srv.Remotes {
			fmt.Fprintf(&b, "  kling mcp link %s %s\n", serviceName(srv.Name), r.URL)
		}
		return fmt.Errorf("%s", b.String())
	}
	if len(srv.Packages) == 0 {
		b.WriteString("It doesn't publish any installable package.")
		return fmt.Errorf("%s", b.String())
	}
	b.WriteString("\nIt publishes this, and kindling only automates npm and PyPI over stdio:\n")
	for _, p := range srv.Packages {
		fmt.Fprintf(&b, "  %s %s (%s)\n", p.RegistryType, p.Identifier, p.Transport.Type)
	}
	b.WriteString("\nIt can still be done by hand: scripts/80-mcp-image.sh")
	return fmt.Errorf("%s", b.String())
}

// renderArgs construye el comando del servidor y dice qué le falta.
//
// Los valores que trae el registro se usan; los obligatorios que vienen vacíos
// se toman de -arg, en orden. Lo opcional sin valor se omite: inventarse un
// valor por defecto es cómo se acaba con un servicio que arranca y no sirve.
func renderArgs(bin string, pkg registry.Package, extra []string) ([]string, []registry.Argument) {
	cmd := []string{bin}
	var unmet []registry.Argument

	for _, a := range pkg.PackageArguments {
		value := a.Value
		if value == "" && a.IsRequired {
			if len(extra) > 0 {
				value, extra = extra[0], extra[1:]
			} else {
				unmet = append(unmet, a)
				continue
			}
		}
		if value == "" && !a.IsRequired {
			continue
		}
		if a.Type == "named" {
			cmd = append(cmd, flagName(a.Name))
		}
		if value != "" {
			cmd = append(cmd, value)
		}
	}
	// Lo que sobre de -arg se añade al final: cubre a los servidores que no
	// declaran sus argumentos en el registro, que son bastantes.
	return append(cmd, extra...), unmet
}

// flagName normaliza el nombre de un argumento con nombre. El registro los
// publica de las dos formas: unos ya traen los guiones y otros no.
func flagName(name string) string {
	if strings.HasPrefix(name, "-") {
		return name
	}
	return "--" + name
}

func argLabel(a registry.Argument) string {
	if a.Type == "named" {
		return flagName(a.Name)
	}
	if a.ValueHint != "" {
		return "<" + a.ValueHint + ">"
	}
	return "<value>"
}

// serviceName reduce "io.github.usuario/servidor" a "servidor", que es lo que
// va a teclear la gente y lo que verá el agente.
func serviceName(full string) string {
	if _, last, ok := strings.Cut(full, "/"); ok {
		full = last
	}
	full = strings.ToLower(full)
	// El nombre acaba siendo un componente de ruta y un nombre de servicio; el
	// daemon lo valida, así que conviene no mandarle basura.
	full = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, full)
	return strings.Trim(full, "-")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// multiFlag registra un flag repetible.
func multiFlag(fs *flag.FlagSet, name, usage string) *[]string {
	v := &stringList{}
	fs.Var(v, name, usage)
	return (*[]string)(v)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
