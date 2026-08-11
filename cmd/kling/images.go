package main

import (
	"flag"
	"fmt"

	"github.com/juan52878911/kindling/internal/api"
)

// cmdImages opera sobre las imágenes de rootfs ya construidas.
//
//	kling images refresh            pone el puente actual dentro de todas
//	kling images refresh semgrep    solo en esa
func cmdImages(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("uso: kling images refresh [imagen...]")
	}
	switch args[0] {
	case "refresh", "refresh-bridge":
		return imagesRefresh(args[1:])
	case "toolchain":
		return imagesToolchain(args[1:])
	default:
		return fmt.Errorf("subcomando desconocido %q: usa refresh o toolchain", args[0])
	}
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
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	res, err := api.NewClient(hostOf(*host)).RefreshBridges(ctx, fs.Args())
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Println("no hay imágenes construidas todavía")
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
			fmt.Printf("  ✓  %-24s puente actualizado\n", r.Image)
			actualizadas++
		default:
			fmt.Printf("     %-24s ya estaba al día\n", r.Image)
		}
	}

	fmt.Println()
	fmt.Printf("%d actualizada(s), %d al día, %d saltada(s), %d con fallo\n",
		actualizadas, len(res)-actualizadas-saltadas-fallos, saltadas, fallos)
	if saltadas > 0 {
		fmt.Println("\nLas saltadas las está usando alguna microVM: párala y repite.")
		fmt.Println("  kling ps -a")
	}
	if actualizadas > 0 {
		// El snapshot dorado se congeló con el puente ANTIGUO dentro, así que
		// las instancias siguen despertando con él hasta que se reimporte. Sin
		// este aviso, el comando parecería no haber servido de nada.
		fmt.Println("\nReimporta los servicios afectados para que sus snapshots lo estrenen:")
		fmt.Println("  kling mcp import <servicio> -force")
	}
	if fallos > 0 {
		return fmt.Errorf("%d imagen(es) no se pudieron actualizar", fallos)
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
	name := fs.String("as", ToolchainImage, "nombre de la imagen")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	fmt.Printf("Construyendo %q: node, npm, python3 y pip dentro.\n", *name)
	fmt.Print("  (instala bastante; tarda unos minutos)... ")

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
	fmt.Println("Ya puedes poblar volúmenes sin instalar nada en el anfitrión:")
	fmt.Printf("  kling volume populate <vol> -- npm install --prefix /data --ignore-scripts lodash\n")
	fmt.Printf("  kling volume populate <vol> -- pip install --target /data requests\n")
	return nil
}
