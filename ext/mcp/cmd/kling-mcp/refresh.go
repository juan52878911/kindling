package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/juan52878911/kindling-mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
	"net/http"
)

// imagesRefresh reemplaza el puente dentro de las imágenes.
//
// Hace falta porque el puente es el PID 1 del invitado y vive DENTRO de cada
// imagen: actualizar kindling en el anfitrión no toca los servicios ya
// empaquetados. Y no es una carencia de funciones sino un fallo desconcertante —
// un puente antiguo no entiende los parámetros nuevos del kernel, muere al
// arrancar, y como es PID 1 el invitado entra en pánico.
func imagesRefresh(args []string) error {
	fs := flag.NewFlagSet("mcp refresh-bridge", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	res, err := refreshBridges(ctx, api.NewClient(hostOf(*host)), fs.Args())
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Println("no images built yet")
		return nil
	}

	var actualizadas, saltadas, ocupadas, fallos int
	var cambiadas []string
	for _, r := range res {
		if r.Busy {
			ocupadas++
		}
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
			cambiadas = append(cambiadas, r.Image)
		default:
			fmt.Printf("     %-24s already up to date\n", r.Image)
		}
	}

	fmt.Println()
	fmt.Printf("%d updated, %d up to date, %d skipped, %d failed\n",
		actualizadas, len(res)-actualizadas-saltadas-fallos, saltadas, fallos)
	// Solo cuando hay algo que parar: una capa cuyo puente vive en su base
	// también sale saltada, y ahí no hay ninguna microVM que apagar.
	if ocupadas > 0 {
		fmt.Println("\nSome images were skipped because a microVM is using them: stop it and try again.")
		fmt.Println("  kling ps -a")
	}
	if actualizadas > 0 {
		// Y se GRABA en la salud, no solo se imprime. El dorado se congeló con
		// el puente ANTIGUO dentro y con esas páginas mapeadas: a partir de aquí
		// el servicio despierta y no sirve, con un "tool did not start listening"
		// que no menciona la imagen por ningún sitio. Un aviso en pantalla no
		// impide nada —basta no leerlo—; en la salud lo ve la sonda y lo cura
		// `kling mcp heal`.
		if n := marcarAfectados(ctx, api.NewClient(hostOf(*host)), cambiadas); n > 0 {
			fmt.Printf("\n%d service(s) marked unhealthy: their golden snapshot still has the old bridge.\n", n)
			fmt.Println("Rebuild them all at once:")
			fmt.Println("  kling mcp heal")
			return nil
		}
		fmt.Println("\nRe-import the affected services so their snapshots pick it up:")
		fmt.Println("  kling mcp import <service> -force")
	}
	if fallos > 0 {
		return fmt.Errorf("%d image(s) failed to update", fallos)
	}
	return nil
}

// marcarAfectados graba en la salud que estos servicios tienen un dorado viejo.
// Devuelve cuantos se marcaron.
func marcarAfectados(ctx context.Context, c *api.Client, imagenes []string) int {
	if len(imagenes) == 0 {
		return 0
	}
	tocada := map[string]bool{}
	for _, i := range imagenes {
		tocada[i] = true
	}
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range snaps {
		if !tocada[s.Image] {
			continue
		}
		nombre := s.Service()
		if nombre == "" {
			nombre = s.Name
		}
		if err := mcp.SetHealth(ctx, c, nombre, false, mcp.MotivoImagenCambiada); err == nil {
			n++
		}
	}
	return n
}

// guestBridgePath es donde vive el puente dentro de una imagen de servicio.
const guestBridgePath = "/usr/local/bin/kling-bridge"

// refreshBridges pone el puente actual (el que `make deploy` dejó en
// /usr/local/lib/kindling/kling-bridge del host del daemon) en las imágenes que
// ya lo llevan, con la API genérica de ficheros en imágenes. Una imagen sin
// puente —una base mínima, la de herramientas— se salta: no es de servicio.
func refreshBridges(ctx context.Context, c *api.Client, names []string) ([]api.ImageFileResult, error) {
	if len(names) == 0 {
		imgs, err := c.Images(ctx)
		if err != nil {
			return nil, err
		}
		for _, i := range imgs {
			names = append(names, i.Name)
		}
	}
	out := make([]api.ImageFileResult, 0, len(names))
	for _, name := range names {
		r, err := c.PutImageFile(ctx, name, api.PutImageFileRequest{
			Path: guestBridgePath, Mode: "0755", FromHost: "kling-bridge",
		})
		switch {
		case err != nil:
			out = append(out, api.ImageFileResult{Image: name, Error: err.Error(), Busy: isConflict(err)})
		case r.Skipped || r.Error != "":
			if r.Skipped {
				r.Error = "has no bridge: not a service image (or its bridge comes from its base)"
			}
			out = append(out, *r)
		default:
			out = append(out, *r)
		}
	}
	return out, nil
}

func isConflict(err error) bool {
	var se *api.StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict
}
