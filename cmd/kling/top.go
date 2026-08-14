package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// cmdTop muestra el consumo de memoria de las microVMs vivas: una foto tipo
// `top`, ordenada por PSS descendente.
//
// PSS y no RSS: las instancias de un mismo snapshot comparten su mem.file por
// copy-on-write, así que el RSS cuenta ese fichero entero en cada copia y las
// suma varias veces. El PSS reparte cada página compartida entre quienes la
// mapean, y es la única columna cuya suma coincide con lo que gasta el host.
func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	host := hostFlag(fs)
	watch := fs.Duration("watch", 0, "refresh every interval (e.g. 2s); 0 = a single snapshot")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))

	if *watch <= 0 {
		ps, err := c.ProcStats(ctx)
		if err != nil {
			return err
		}
		printTop(ps, c.Endpoint(), time.Time{})
		return nil
	}

	// Modo -watch: repinta hasta que se cancele con Ctrl-C. \033[H\033[2J limpia
	// la pantalla y sube el cursor, como hace top.
	t := time.NewTicker(*watch)
	defer t.Stop()
	for {
		ps, err := c.ProcStats(ctx)
		if err != nil {
			return err
		}
		fmt.Print("\033[H\033[2J")
		printTop(ps, c.Endpoint(), time.Now())
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func printTop(ps *api.ProcStats, endpoint string, at time.Time) {
	// Solo las vivas tienen proceso y, por tanto, PSS.
	live := make([]api.ProcStat, 0, len(ps.Machines))
	for _, mc := range ps.Machines {
		if mc.PID > 0 {
			live = append(live, mc)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].PSSMiB > live[j].PSSMiB })

	header := fmt.Sprintf("kindling  %s", endpoint)
	if !at.IsZero() {
		header += "  " + at.Format("15:04:05")
	}
	fmt.Println(header)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tSERVICE\tSTATE\tPSS")
	for _, mc := range live {
		svc := mc.Service
		if svc == "" {
			svc = mc.From // sin servicio, muestra el snapshot de origen
		}
		if svc == "" {
			svc = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d MiB\n",
			mc.ID[:min(12, len(mc.ID))], trunc(svc, 20), mc.State, mc.PSSMiB)
	}
	if err := tw.Flush(); err != nil {
		return
	}

	// Estados que no aparecen en la tabla (warm/stopped) sí cuentan para el resumen.
	fmt.Printf("\n%d alive · %d warm · total PSS %d MiB\n",
		len(live), ps.ByState[string(api.StateWarm)], ps.TotalPSSMiB)
	if ps.AvailableMiB > 0 || ps.FreeMiB > 0 {
		fmt.Printf("host: MemAvailable %d MiB · MemFree %d MiB\n", ps.AvailableMiB, ps.FreeMiB)
	} else {
		fmt.Printf("host: no /proc (not Linux); PSS and host memory unavailable\n")
	}
}
