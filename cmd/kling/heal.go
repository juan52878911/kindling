package main

// Curar los dorados que un reinicio del anfitrion dejo inservibles.
//
// Firecracker ata cada snapshot a la frecuencia del TSC del host, que se mide de
// nuevo en cada arranque. Un reinicio los invalida TODOS a la vez. Y como la
// salud solo se registraba al atender una peticion, un servicio que nadie llama
// se quedaba roto en silencio: semgrep y playwright estuvieron 297 horas caidos
// sin que nada lo dijera.
//
// El vigia vive aqui y no en el daemon porque `mcp import` vive aqui: el daemon
// no tiene endpoint de importacion, solo las piezas (arrancar, congelar, grabar
// catalogo). Meterlo dentro seria duplicar la orquestacion entera.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// argvReimportar reconstruye la linea de `mcp import` que rehace un servicio TAL
// COMO ESTABA, leyendo lo que el propio dorado grabo de si mismo.
//
// Esto es lo que hace segura la autocuracion. Reimportar con los valores por
// defecto arreglaria la caida cambiando el servicio por debajo: un stateful
// pasaria a efimero y perderia lo que acumula, y un servicio con egress acotado
// se abriria a internet. Todo lo que el import necesita esta en api.Snapshot.
//
// Se devuelve un argv, y no una llamada directa, para reutilizar mcpImport
// entero: es el camino ya probado, y duplicarlo seria tener dos importaciones
// que divergen.
func argvReimportar(s *api.Snapshot, host string) []string {
	args := []string{"-force"}
	if host != "" {
		args = append(args, "-H", host)
	}
	if s.Image != "" {
		args = append(args, "-image", s.Image)
	}
	if s.MemMiB > 0 {
		args = append(args, "-mem", strconv.Itoa(s.MemMiB))
	}
	if s.VCPUs > 0 {
		args = append(args, "-cpus", strconv.Itoa(s.VCPUs))
	}
	if s.CPUPct > 0 {
		args = append(args, "-cpu-pct", strconv.Itoa(s.CPUPct))
	}
	if s.Egress != "" {
		args = append(args, "-egress", s.Egress)
	}
	if len(s.AllowDomains) > 0 {
		args = append(args, "-allow", strings.Join(s.AllowDomains, ","))
	}
	for _, v := range s.Volumes {
		args = append(args, "-volume", specVolumen(v))
	}
	// Explicito en ambos sentidos: el defecto de `mcp import` depende de lo que
	// detecte de la imagen, y aqui no queremos que decida — queremos lo que
	// habia.
	if s.Stateful() {
		args = append(args, "-stateful")
	} else {
		args = append(args, "-ephemeral")
	}
	return append(args, s.Service())
}

// specVolumen escribe un volumen en el formato que entiende -volume:
// nombre[:/punto][:ro]. El orden importa: volumeFlag.Set corta ":ro" por la
// derecha ANTES de partir el punto de montaje.
func specVolumen(v api.VolumeAttachment) string {
	spec := v.Name
	if v.Mount != "" {
		spec += ":" + v.Mount
	}
	if v.ReadOnly {
		spec += ":ro"
	}
	return spec
}

// esperarDaemon bloquea hasta que el daemon conteste, o hasta agotar el plazo.
func esperarDaemon(ctx context.Context, c *api.Client, limite time.Duration) error {
	fin := time.Now().Add(limite)
	var ultimo error
	for {
		if _, err := c.Info(ctx); err == nil {
			return nil
		} else {
			ultimo = err
		}
		if time.Now().After(fin) {
			return fmt.Errorf("the daemon didn't answer within %s: %w", limite, ultimo)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// seCuraReconstruyendo decide si reimportar arreglaria este servicio.
//
// `saludGrabada` es lo que habia en el meta ANTES de sondear, y hace falta: el
// sintoma de una imagen refrescada bajo el dorado es un "tool did not start
// listening" que no menciona la imagen por ningun sitio. Quien SI lo sabe es
// `images refresh`, que lo dejo escrito ahi al terminar.
func seCuraReconstruyendo(probeErr error, saludGrabada string) bool {
	if probeErr == nil {
		return false
	}
	if api.EsFalloTSC(probeErr) || api.EsImagenCambiada(probeErr) {
		return true
	}
	return api.EsImagenCambiada(errors.New(saludGrabada))
}

func mcpHeal(args []string) error {
	fs := flag.NewFlagSet("mcp heal", flag.ExitOnError)
	host := hostFlag(fs)
	wait := fs.Duration("wait", 45*time.Second, "maximum wait for the server to start")
	seco := fs.Bool("dry-run", false, "say what would be rebuilt, without rebuilding it")
	profundo := fs.Bool("deep", true, "also call one real tool, not just tools/list")
	arranque := fs.Duration("wait-daemon", 60*time.Second, "how long to wait for the daemon to answer")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	// El vigia arranca justo detras del daemon, y systemd da por "arrancado" un
	// Type=simple en cuanto hace fork: el socket puede no existir todavia. Sin
	// esta espera, el arranque del anfitrion —que es EL momento en el que hay
	// algo que curar— seria justo cuando el vigia no encuentra a nadie con quien
	// hablar. Observado en el laboratorio a la primera ejecucion.
	if err := esperarDaemon(ctx, c, *arranque); err != nil {
		return err
	}

	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Println("No services to probe. Import one:  kling mcp import <name> -image <image>")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	var rotos, curados int

	// En serie a proposito. Reconstruir arranca una microVM que reserva su
	// memoria por adelantado; en paralelo competirian por la RAM que necesitan
	// justo para arrancar, y fallarian todas a la vez.
	for _, s := range snaps {
		nombre := s.Service()
		if nombre == "" {
			nombre = s.Name
		}

		probeErr := probeHealth(ctx, c, nombre, *wait, *profundo, s.Egress)
		if _, err := c.SetHealth(ctx, nombre, probeErr == nil, errMsg(probeErr)); err != nil {
			fmt.Fprintf(tw, "  %s\t✗ couldn't record health: %v\n", nombre, err)
			continue
		}
		if probeErr == nil {
			fmt.Fprintf(tw, "  %s\t✓ healthy\n", nombre)
			continue
		}
		rotos++

		// Dos causas curables, y solo dos. Las une lo mismo: los ficheros del
		// servicio estan bien y lo que ya no vale es el SNAPSHOT.
		//
		//   - el TSC del anfitrion cambio (un reinicio del host);
		//   - la imagen se refresco por debajo del dorado.
		//
		// Un servicio roto por cualquier otra causa —el servidor MCP falla, falta
		// un secreto, la imagen no trae lo que dice— NO se arregla
		// reconstruyendolo: reimportar en bucle gastaria minutos por vuelta y
		// taparia el problema real bajo un "lo intente".
		if !seCuraReconstruyendo(probeErr, s.HealthErr) {
			fmt.Fprintf(tw, "  %s\t✗ unhealthy, and not a cause that rebuilding fixes — left alone\n", nombre)
			continue
		}
		if s.Service() == "" {
			// Un dorado sin servicio no se reimporta: se rehace con
			// `kling commit -replace`, que necesita una maquina viva.
			fmt.Fprintf(tw, "  %s\t✗ stale, but it isn't an MCP service: kling commit -replace <machine> %s\n", nombre, s.Name)
			continue
		}

		argv := argvReimportar(s, *host)
		if *seco {
			fmt.Fprintf(tw, "  %s\t… would rebuild:  kling mcp import %s\n", nombre, strings.Join(argv, " "))
			continue
		}

		// Vaciar antes de una reconstruccion que tarda entre 15 y 40 s: si no,
		// el tabwriter se guarda las lineas anteriores y parece colgado.
		_ = tw.Flush()
		fmt.Printf("  %s  rebuilding: its golden snapshot didn't survive the host reboot\n", nombre)
		if err := mcpImport(argv); err != nil {
			fmt.Fprintf(tw, "  %s\t✗ rebuild failed: %v\n", nombre, err)
			continue
		}
		curados++
		fmt.Fprintf(tw, "  %s\t✓ rebuilt\n", nombre)
	}
	_ = tw.Flush()

	switch {
	case rotos == 0:
		fmt.Printf("\n%d service(s), all healthy\n", len(snaps))
	case curados == rotos:
		fmt.Printf("\n%d of %d service(s) were stale after a host reboot; all rebuilt\n", curados, len(snaps))
	default:
		fmt.Printf("\n%d unhealthy, %d rebuilt\n", rotos, curados)
		// Codigo propio: el vigia SI hizo su trabajo —sondeo todo y grabo la
		// salud de cada uno—, solo que algo sigue roto por una causa que no se
		// cura reconstruyendo. Eso no es un fallo de la unidad; que no pudiera
		// hablar con el daemon, si.
		return &errConCodigo{code: 3, err: fmt.Errorf("%d service(s) still unhealthy", rotos-curados)}
	}
	return nil
}
