package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdImages opera sobre las imágenes de rootfs ya construidas.
func cmdImages(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling images <ls|rm|toolchain|recipe|build|cat|put|copy> [...]")
	}
	switch args[0] {
	case "ls", "list":
		return imagesList(args[1:])
	case "refresh", "refresh-bridge":
		// El puente es de kindling-mcp y su recambio también.
		return fmt.Errorf("the bridge belongs to kindling-mcp now: kling mcp refresh-bridge [image...]")
	case "toolchain":
		return imagesToolchain(args[1:])
	case "rm", "remove":
		return imagesRm(args[1:])
	case "recipe":
		return imagesRecipe(args[1:])
	case "build":
		return imagesBuild(args[1:])
	case "cat":
		return imagesCat(args[1:])
	case "put":
		return imagesPut(args[1:])
	case "copy", "cp":
		return imagesCopy(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q: use ls, rm, toolchain, recipe, build, cat, put or copy", args[0])
	}
}

// imagesList enumera las imágenes de rootfs construidas. USED BY cuenta los
// snapshots dorados que salen de cada una: una imagen con 0 es candidata a
// retirar; con >0, quitarla dejaría esos servicios sin base.
func imagesList(args []string) error {
	fs := flag.NewFlagSet("images ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	imgs, err := api.NewClient(hostOf(*host)).Images(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(imgs)
	}
	if len(imgs) == 0 {
		fmt.Println("No images built yet. Package one:  kling add <server>")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBASE\tLOGICAL\tON DISK\tRECIPE\tUSED BY")
	var total int64
	var porCapas bool
	for _, img := range imgs {
		recipe := "no"
		if img.HasRecipe {
			recipe = "yes"
		}
		// Las dos formas de estar en uso: snapshots que salieron de aquí, y capas
		// que se apoyan encima. Se dicen las dos porque se retiran distinto.
		var usos []string
		if img.UsedBy > 0 {
			usos = append(usos, fmt.Sprintf("%d snapshot(s)", img.UsedBy))
		}
		if img.Layers > 0 {
			usos = append(usos, fmt.Sprintf("%d layer(s)", img.Layers))
		}
		used := "—"
		if len(usos) > 0 {
			used = strings.Join(usos, ", ")
		}
		base := "—"
		if img.Base != "" {
			base, porCapas = img.Base, true
		}
		total += img.DiskBytes
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", img.Name, base,
			human(img.SizeBytes), human(img.DiskBytes), recipe, used)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// El total es la cifra que justifica las capas, y no es la suma de lo que
	// ocupa cada servicio "completo": la base aparece UNA vez y cada capa cuesta
	// solo su delta. Sin esta línea hay que sumarlo a mano para verlo.
	fmt.Printf("\nTotal on disk: %s across %d image(s)", human(total), len(imgs))
	if porCapas {
		fmt.Print(" — each base counted once, shared by its layers")
	}
	fmt.Println(".")
	return nil
}

// ToolchainImage es la imagen con instaladores que usa `volume populate`.
//
// Tiene nombre fijo y conocido a propósito: sin ella, poblar un volumen obligaba
// a pasar `-image` con alguna imagen de servicio que casualmente trajera npm.
// Eso funcionaba por accidente, no por diseño, y dejaba al usuario adivinando
// cuál de sus imágenes sirve.
const ToolchainImage = "toolchain"

// imagesToolchain construye la imagen con npm y pip dentro.
//
// No lleva servidor MCP: su PID 1 es el agente de invitado genérico,
// kling-guest, y lo único que se usa de ella es su /exec, que es como
// `volume populate` instala paquetes dentro de una microVM en vez de en el
// anfitrión. La construye el constructor "base" del núcleo.
func imagesToolchain(args []string) error {
	fs := flag.NewFlagSet("images toolchain", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("as", ToolchainImage, "image name")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	fmt.Printf("Building %q: node, npm, python3 and pip inside.\n", *name)
	fmt.Print("  (installs quite a bit; takes a few minutes)... ")

	// El constructor "base" del núcleo: paquetes del sistema y kling-guest como
	// PID 1, que es quien sirve /exec cuando populate enciende kling.exec=1.
	spec, _ := json.Marshal(BaseSpec{Packages: []string{"nodejs", "npm", "python3", "py3-pip"}})
	res, err := api.NewClient(hostOf(*host)).BuildImage(ctx, api.BuildImageRequest{
		Name:    *name,
		Base:    "min",
		GrowMB:  1536,
		Builder: "base",
		Spec:    spec,
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Printf("✓ %s\n\n", res.Path)
	fmt.Println("You can now populate volumes without installing anything on the host:")
	fmt.Printf("  kling volume populate <vol> -- npm install --prefix /data --ignore-scripts lodash\n")
	fmt.Printf("  kling volume populate <vol> -- pip install --target /data requests\n")
	return nil
}

// imagesRecipe enseña cómo se construyó una imagen.
//
// Existe porque hasta ahora una imagen no era reproducible: lo único que
// sobrevivía del comando era el /entrypoint de dentro —y leerlo exige montarla—
// mientras que los paquetes instalados no quedaban registrados en ninguna parte.
func imagesRecipe(args []string) error {
	fs := flag.NewFlagSet("images recipe", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling images recipe <image>")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	rec, err := api.NewClient(hostOf(*host)).ImageRecipe(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("%s  (built %s with kling %s)\n", rec.Name,
		rec.BuiltAt.Local().Format("2006-01-02 15:04"), rec.KlingVer)
	if rec.Base != "" {
		fmt.Printf("  base:      %s\n", rec.Base)
	}
	if len(rec.Packages) > 0 {
		fmt.Printf("  apk:       %s\n", strings.Join(rec.Packages, " "))
	}
	if len(rec.NPM) > 0 {
		fmt.Printf("  npm:       %s\n", strings.Join(rec.NPM, " "))
	}
	if len(rec.PIP) > 0 {
		fmt.Printf("  pip:       %s\n", strings.Join(rec.PIP, " "))
	}
	if len(rec.Cmd) > 0 {
		fmt.Printf("  command:   %s\n", strings.Join(rec.Cmd, " "))
	}
	// Desde v0.6 las imágenes las construye un constructor con nombre, y lo que
	// pidió va en su spec, que el núcleo no interpreta: se enseña tal cual.
	if rec.Builder != "" {
		fmt.Printf("  builder:   %s\n", rec.Builder)
	}
	if len(rec.Spec) > 0 && string(rec.Spec) != "null" {
		var buf bytes.Buffer
		if json.Indent(&buf, rec.Spec, "             ", "  ") == nil {
			fmt.Printf("  spec:      %s\n", buf.String())
		}
	}
	return nil
}

// imagesRm retira una imagen que ya no usa nadie.
//
// Sin esto, la unica forma de recuperar el espacio de una imagen era borrar su
// fichero a mano, sin ninguna comprobacion — y borrar la BASE de una capa, o la
// imagen de la que cuelga un dorado, no da un error al borrar: da un invitado
// que no arranca, mucho despues.
func imagesRm(args []string) error {
	fs := flag.NewFlagSet("images rm", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling images rm <image> [<image>...]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	var fallos int
	for _, n := range fs.Args() {
		if err := c.RemoveImage(ctx, n); err != nil {
			fmt.Printf("  ✗  %-24s %v\n", n, err)
			fallos++
			continue
		}
		fmt.Printf("  ✓  %-24s removed\n", n)
	}
	if fallos > 0 {
		return fmt.Errorf("%d image(s) could not be removed", fallos)
	}
	return nil
}

// imagesCopy copia una imagen de un daemon a otro: es como se consiguen
// imágenes en un Mac, donde no se pueden construir. El CLI hace de tubería
// entre los dos daemons (GET de uno, PUT al otro) y no guarda nada en disco.
func imagesCopy(args []string) error {
	fs := flag.NewFlagSet("images copy", flag.ExitOnError)
	from := fs.String("from", "", "daemon to copy from (ssh://user@host or a socket)")
	to := fs.String("to", "", "daemon to copy to (default: the usual one: -H, $KLING_HOST, context or local socket)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *from == "" {
		return fmt.Errorf("usage: kling images copy <name> -from <host> [-to <host>]\n" +
			"  e.g. kling images copy min -from ssh://user@linux-arm64-host")
	}
	name := fs.Arg(0)
	dst := hostOf(*to)
	if dst == *from {
		return fmt.Errorf("source and destination are the same daemon (%s)", *from)
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	src, dstC := api.NewClient(*from), api.NewClient(dst)
	fmt.Printf("copying %s  %s -> %s\n", name, src.Endpoint(), dstC.Endpoint())
	err := api.CopyImage(ctx, src, dstC, name, func(p api.CopyStep) {
		what := p.Name + " (" + p.Part + ")"
		if p.Name == api.KernelBlobName {
			what = "kernel"
		}
		size := (&api.BlobInfo{Size: p.Size}).SizeString()
		if p.Skipped {
			fmt.Printf("  %-32s %10s  already there\n", what, size)
			return
		}
		fmt.Printf("  %-32s %10s  copied\n", what, size)
	})
	if err != nil {
		return err
	}
	fmt.Printf("done: kling run -image %s\n", name)
	return nil
}
