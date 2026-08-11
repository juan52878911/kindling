package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/internal/api"
)

// cmdVolume gestiona el almacenamiento que sobrevive a las microVMs.
//
//	kling volume create notas -size 2G
//	kling volume ls
//	kling volume rm notas
func cmdVolume(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("uso: kling volume [create|ls|rm]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create", "add":
		return volumeCreate(rest)
	case "ls", "list":
		return volumeList(rest)
	case "rm", "remove":
		return volumeRemove(rest)
	case "populate", "install":
		return volumePopulate(rest)
	default:
		return fmt.Errorf("subcomando desconocido %q: usa create, ls, rm o populate", sub)
	}
}

func volumeCreate(args []string) error {
	fs := flag.NewFlagSet("volume create", flag.ExitOnError)
	host := hostFlag(fs)
	size := fs.String("size", "1G", "tamaño lógico: 512M, 2G, 10G")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling volume create <nombre> [-size 2G]")
	}
	mib, err := parseSizeMiB(*size)
	if err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	v, err := api.NewClient(hostOf(*host)).CreateVolume(ctx, api.CreateVolumeRequest{
		Name: fs.Arg(0), SizeMiB: mib,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s  creado  (%s lógicos, %s en disco)\n", v.Name, human(v.SizeBytes), human(v.UsedBytes))
	fmt.Printf("\nEs disperso: solo ocupa lo que se escriba dentro.\n")
	fmt.Printf("Móntalo al importar un servicio:\n")
	fmt.Printf("  kling mcp import <servicio> -volume %s\n", v.Name)
	return nil
}

func volumeList(args []string) error {
	fs := flag.NewFlagSet("volume ls", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	vols, err := api.NewClient(hostOf(*host)).Volumes(ctx)
	if err != nil {
		return err
	}
	if len(vols) == 0 {
		fmt.Println("Sin volúmenes. Crea uno:  kling volume create notas -size 2G")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NOMBRE\tLÓGICO\tEN DISCO\tEN USO POR")
	for _, v := range vols {
		users := strings.Join(v.UsedBy, ", ")
		if users == "" {
			users = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.Name, human(v.SizeBytes), human(v.UsedBytes), users)
	}
	return tw.Flush()
}

func volumeRemove(args []string) error {
	fs := flag.NewFlagSet("volume rm", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling volume rm <nombre>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	if err := api.NewClient(hostOf(*host)).RemoveVolume(ctx, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Printf("%s eliminado\n", fs.Arg(0))
	return nil
}

// parseSizeMiB acepta 512M, 2G o un número suelto en MiB.
//
// Se acota arriba porque el fichero es disperso pero el tamaño lógico se graba
// en el ext4: pedir 10 TB "por si acaso" crea un sistema de ficheros cuyos
// metadatos ya no son gratis.
func parseSizeMiB(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1024, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1, strings.TrimSuffix(s, "M")
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("tamaño inválido %q: usa 512M, 2G o un número en MiB", s)
	}
	return n * mult, nil
}

// volumePopulate instala paquetes DENTRO de una microVM desechable.
//
//	kling volume populate libs -image filesystem-mcp -- npm install --prefix /data lodash
//
// El sentido de que sea una microVM y no el anfitrión: instalar paquetes es
// ejecutar código de terceros, y hacerlo fuera de la frontera que kindling
// levanta contradice la razón de ser del proyecto. La máquina se destruye al
// terminar, salga bien o mal.
func volumePopulate(args []string) error {
	fs := flag.NewFlagSet("volume populate", flag.ExitOnError)
	host := hostFlag(fs)
	image := fs.String("image", ToolchainImage, "imagen que trae el instalador (npm, pip...)")
	mount := fs.String("mount", "/data", "dónde se monta el volumen dentro de la microVM")
	mem := fs.Int("mem", 0, "memoria en MiB de la microVM de instalación")

	// El "--" se separa ANTES de parsear, y no se deja en manos de flag.
	//
	// Lo que va detrás es el comando del instalador, y trae sus PROPIOS flags:
	// `npm install --prefix /data`. Si se dejara pasar por el reordenador de
	// argumentos, --prefix se interpretaría como un flag de kling y el comando
	// llegaría descuartizado. Pasó exactamente eso la primera vez.
	nuestros, cmd := args, []string(nil)
	for i, a := range args {
		if a == "--" {
			nuestros, cmd = args[:i], args[i+1:]
			break
		}
	}
	if err := fs.Parse(reorder(nuestros)); err != nil {
		return err
	}
	if fs.NArg() < 1 || len(cmd) == 0 {
		return fmt.Errorf("uso: kling volume populate <nombre> -image IMG -- <comando>\n" +
			"  p. ej.:  kling volume populate libs -image node-base -- \\\n" +
			"             npm install --prefix /data --ignore-scripts lodash zod")
	}
	name := fs.Arg(0)
	if *image == "" {
		return fmt.Errorf("falta -image: hace falta una imagen que traiga el instalador")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	fmt.Printf("Poblando %q dentro de una microVM de un solo uso.\n", name)
	fmt.Printf("  imagen:   %s\n", *image)
	fmt.Printf("  montado:  %s\n", *mount)
	fmt.Printf("  comando:  %s\n\n", strings.Join(cmd, " "))
	fmt.Print("  instalando (puede tardar; el código de terceros corre dentro)... ")

	res, err := api.NewClient(hostOf(*host)).PopulateVolume(ctx, api.PopulateRequest{
		Volume: name, Mount: *mount, Image: *image, Cmd: cmd, MemMiB: *mem,
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	if res.ExitCode != 0 {
		fmt.Println("✗")
		// La salida completa, no un resumen: si un install falla, el motivo
		// está en su propia salida y recortarla obliga a repetirlo todo para
		// verlo.
		fmt.Fprintln(os.Stderr, res.Output)
		return fmt.Errorf("el comando terminó con código %d", res.ExitCode)
	}
	fmt.Println("✓")
	if res.Output != "" {
		fmt.Println(indent(strings.TrimRight(res.Output, "\n")))
	}
	fmt.Printf("\n%s ocupa ahora %d MiB. Móntalo donde haga falta:\n", name, res.UsedMiB)
	fmt.Printf("  kling mcp import <servicio> -volume %s:/libs:ro\n", name)
	return nil
}

// indent sangra un bloque de salida ajena para que se distinga de lo nuestro.
func indent(s string) string {
	if s == "" {
		return ""
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
