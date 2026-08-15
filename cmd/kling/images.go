package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/internal/api"
)

// cmdImages opera sobre las imágenes de rootfs ya construidas.
//
//	kling images refresh            pone el puente actual dentro de todas
//	kling images refresh semgrep    solo en esa
func cmdImages(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling images [ls|refresh|toolchain|recipe]")
	}
	switch args[0] {
	case "ls", "list":
		return imagesList(args[1:])
	case "refresh", "refresh-bridge":
		return imagesRefresh(args[1:])
	case "toolchain":
		return imagesToolchain(args[1:])
	case "recipe":
		return imagesRecipe(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q: use ls, refresh, toolchain, or recipe", args[0])
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

// imagesRefresh reemplaza el puente dentro de las imágenes.
//
// Hace falta porque el puente es el PID 1 del invitado y vive DENTRO de cada
// imagen: actualizar kindling en el anfitrión no toca los servicios ya
// empaquetados. Y no es una carencia de funciones sino un fallo desconcertante —
// un puente antiguo no entiende los parámetros nuevos del kernel, muere al
// arrancar, y como es PID 1 el invitado entra en pánico.
func imagesRefresh(args []string) error {
	fs := flag.NewFlagSet("images refresh", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	res, err := api.NewClient(hostOf(*host)).RefreshBridges(ctx, fs.Args())
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Println("no images built yet")
		return nil
	}

	var actualizadas, saltadas, fallos int
	for _, r := range res {
		switch {
		case r.Error != "" && r.Skipped:
			// Saltada no es un fallo: es información. La imagen sigue con el
			// puente viejo y hay que saberlo.
			fmt.Printf("  ⏭  %-24s %s\n", r.Image, r.Error)
			saltadas++
		case r.Error != "":
			fmt.Printf("  ✗  %-24s %s\n", r.Image, r.Error)
			fallos++
		case r.Updated:
			fmt.Printf("  ✓  %-24s bridge updated\n", r.Image)
			actualizadas++
		default:
			fmt.Printf("     %-24s already up to date\n", r.Image)
		}
	}

	fmt.Println()
	fmt.Printf("%d updated, %d up to date, %d skipped, %d failed\n",
		actualizadas, len(res)-actualizadas-saltadas-fallos, saltadas, fallos)
	if saltadas > 0 {
		fmt.Println("\nSkipped images are in use by a microVM: stop it and try again.")
		fmt.Println("  kling ps -a")
	}
	if actualizadas > 0 {
		// El snapshot dorado se congeló con el puente ANTIGUO dentro, así que
		// las instancias siguen despertando con él hasta que se reimporte. Sin
		// este aviso, el comando parecería no haber servido de nada.
		fmt.Println("\nRe-import the affected services so their snapshots pick it up:")
		fmt.Println("  kling mcp import <service> -force")
	}
	if fallos > 0 {
		return fmt.Errorf("%d image(s) failed to update", fallos)
	}
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
// No lleva servidor MCP: su comando es un shell que nunca se invoca, porque a
// esta imagen no se le abren sesiones MCP. Lo único que se usa de ella es el
// puente y su /exec, que es como `volume populate` instala paquetes dentro de
// una microVM en vez de en el anfitrión.
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

	res, err := api.NewClient(hostOf(*host)).BuildImage(ctx, api.BuildImageRequest{
		Name: *name,
		// nodejs/npm para el mundo de node; python3/py3-pip para el de Python.
		// Nada más: cada paquete extra es peso en una imagen que solo existe
		// para instalar cosas en un volumen y morirse.
		Packages: []string{"nodejs", "npm", "python3", "py3-pip"},
		// Sitio para que quepan node y las ruedas de Python a la vez.
		GrowMB: 1536,
		// Un shell que nunca llega a ejecutarse: el puente solo lanza este
		// comando cuando alguien abre una sesión MCP, y a esta imagen no se le
		// abren. Existe porque el empaquetador exige un comando.
		Cmd: []string{"/bin/sh"},
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
	fmt.Printf("  command:   %s\n", strings.Join(rec.Cmd, " "))
	return nil
}
