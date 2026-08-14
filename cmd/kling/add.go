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
	if err := fs.Parse(reorder(args)); err != nil {
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
	fmt.Println("\n\"kling add no\" means that server doesn't speak stdio over npm,")
	fmt.Println("which is the only thing kindling knows how to package on its own. See scripts/80-mcp-image.sh.")
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
	if err := fs.Parse(reorder(args)); err != nil {
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

	for _, want := range fs.Args() {
		if err := addOne(ctx, rc, *host, want, *as, vols, *extra, *dryRun, *bundle); err != nil {
			return fmt.Errorf("%s: %w", want, err)
		}
	}
	return nil
}

func addOne(ctx context.Context, rc *registry.Client, host, want, as string, vols []api.VolumeAttachment, extra []string, dryRun, bundle bool) error {
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

	// Secretos: el entrypoint que genera 80-mcp-image.sh solo fija PATH y HOME,
	// así que hoy NO hay forma de meterle variables de entorno al servidor.
	// Mejor negarse que construir un servicio que falle dentro del agente, que
	// es donde peor se diagnostica.
	if missing := pkg.MissingEnv(nil); len(missing) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s needs environment variables that kindling doesn't yet know how to inject:\n", srv.Name)
		for _, v := range missing {
			secret := ""
			if v.IsSecret {
				secret = "  [secret]"
			}
			fmt.Fprintf(&b, "  %s%s  %s\n", v.Name, secret, truncate(v.Description, 60))
		}
		b.WriteString("\nPackage it by hand by adding them to the entrypoint: scripts/80-mcp-image.sh")
		return fmt.Errorf("%s", b.String())
	}

	service := as
	if service == "" {
		service = serviceName(srv.Name)
	}

	// El invitado arranca SIN red, así que `npx -y` fallaría al descargar: el
	// paquete se preinstala y hay que llamar a su ejecutable por su nombre
	// real, que solo conoce npm.
	bin, err := registry.ResolveBin(ctx, &http.Client{Timeout: 30 * time.Second}, pkg.Identifier)
	if err != nil {
		return err
	}

	cmd, unmet := renderArgs(bin, pkg, extra)
	if len(unmet) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "missing %d required argument(s); pass them with -arg:\n", len(unmet))
		for _, a := range unmet {
			fmt.Fprintf(&b, "  %s  %s\n", argLabel(a), truncate(a.Description, 60))
		}
		fmt.Fprintf(&b, "\ne.g.:  kling add %s -arg /data", want)
		return fmt.Errorf("%s", b.String())
	}

	npmSpec := pkg.Identifier
	if pkg.Version != "" {
		npmSpec += "@" + pkg.Version
	}

	fmt.Printf("%s  v%s\n", srv.Name, srv.Version)
	fmt.Printf("  service:   %s\n", service)
	fmt.Printf("  npm:       %s\n", npmSpec)
	fmt.Printf("  command:   %s\n", strings.Join(cmd, " "))
	if dryRun {
		fmt.Println("\n(-dry-run: not doing anything)")
		return nil
	}
	fmt.Println()

	// 1. Empaquetar. Lo hace el daemon porque monta un loopback y hace chroot.
	fmt.Printf("  1/2  building the image (installs node, may take a while)... ")
	c := api.NewClient(hostOf(host))
	res, err := c.BuildImage(ctx, api.BuildImageRequest{
		Name:     service,
		Packages: []string{"nodejs", "npm"},
		NPM:      []string{npmSpec},
		Cmd:      cmd,
		Bundle:   bundle,
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
	if err := runImport(hostOf(host), service, vols, loadConfig()); err != nil {
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
	b.WriteString("\nIt publishes this, and kindling only automates npm over stdio:\n")
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
