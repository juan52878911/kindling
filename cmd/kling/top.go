package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdTop muestra el consumo de las microVMs vivas: una foto tipo `top`,
// ordenada por memoria descendente, con su %CPU.
//
// En Linux la memoria es el PSS y no el RSS: las instancias de un mismo
// snapshot comparten su mem.file por copy-on-write, así que el RSS cuenta ese
// fichero entero en cada copia y las suma varias veces. El PSS reparte cada
// página compartida entre quienes la mapean, y es la única columna cuya suma
// coincide con lo que gasta el host. En macOS no hay /proc ni PSS: la columna
// es la huella (phys_footprint del VMM, el auxiliar de Apple y sus hijos), lo
// mismo que enseña Monitor de Actividad.
//
// El %CPU es la CPU que gastó cada VMM entre dos fotos: en -watch, entre una
// pantalla y la anterior; en una foto suelta, se toman dos a un segundo.
func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	host := hostFlag(fs)
	watch := fs.Duration("watch", 0, "refresh every interval (e.g. 2s); 0 = a single snapshot")
	asJSON := fs.Bool("json", false, "JSON output (a single snapshot; ignores -watch)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))

	// JSON es siempre una foto única: repintar la pantalla no tiene sentido para
	// un consumidor de máquina, así que -json ignora -watch.
	if *asJSON {
		ps, err := c.ProcStats(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(ps)
	}

	if *watch <= 0 {
		antes, err := c.ProcStats(ctx)
		if err != nil {
			return err
		}
		t0 := time.Now()
		ps := antes
		// Un daemon que da la CPU por máquina: una segunda foto para el %CPU.
		if conCPU(antes) {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			if ps, err = c.ProcStats(ctx); err != nil {
				return err
			}
		}
		printTop(os.Stdout, ps, antes, time.Since(t0), c.Endpoint(), time.Time{})
		return nil
	}

	// Modo -watch: repinta hasta que se cancele con Ctrl-C. \033[H\033[2J limpia
	// la pantalla y sube el cursor, como hace top.
	t := time.NewTicker(*watch)
	defer t.Stop()
	var antes *api.ProcStats
	var tAntes time.Time
	for {
		ps, err := c.ProcStats(ctx)
		if err != nil {
			return err
		}
		ahora := time.Now()
		fmt.Print("\033[H\033[2J")
		printTop(os.Stdout, ps, antes, ahora.Sub(tAntes), c.Endpoint(), ahora)
		antes, tAntes = ps, ahora
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// conCPU dice si el daemon da la CPU por máquina (uno anterior a 0.20 no).
func conCPU(ps *api.ProcStats) bool {
	for _, mc := range ps.Machines {
		if mc.CPUSeconds > 0 {
			return true
		}
	}
	return false
}

// pctCPU es el %CPU de mc entre antes y ahora (dt): 100 = un núcleo entero.
// -1 si no se puede decir (sin foto anterior, otro proceso, sin CPU).
func pctCPU(mc api.ProcStat, antes *api.ProcStats, dt time.Duration) (total, p float64) {
	if antes == nil || dt <= 0 || mc.CPUSeconds <= 0 {
		return -1, -1
	}
	for _, a := range antes.Machines {
		if a.ID == mc.ID && a.PID == mc.PID && a.CPUSeconds > 0 && mc.CPUSeconds >= a.CPUSeconds {
			s := dt.Seconds()
			return (mc.CPUSeconds - a.CPUSeconds) / s * 100, (mc.CPUPSeconds - a.CPUPSeconds) / s * 100
		}
	}
	return -1, -1
}

func printTop(w io.Writer, ps, antes *api.ProcStats, dt time.Duration, endpoint string, at time.Time) {
	// Solo las vivas tienen proceso y, por tanto, memoria.
	live := make([]api.ProcStat, 0, len(ps.Machines))
	for _, mc := range ps.Machines {
		if mc.PID > 0 {
			live = append(live, mc)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].PSSMiB > live[j].PSSMiB })

	huella := ps.MemoryKind == "footprint"
	columna := "PSS"
	if huella {
		columna = "MEM"
	}
	cpu := conCPU(ps)
	reparto := false
	for _, mc := range live {
		reparto = reparto || mc.CoreSplit
	}

	header := fmt.Sprintf("kindling  %s", endpoint)
	if !at.IsZero() {
		header += "  " + at.Format("15:04:05")
	}
	fmt.Fprintln(w, header)

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	cab := "ID\tSERVICE\tSTATE\t" + columna
	if cpu {
		cab += "\tCPU"
		if reparto {
			cab += "\tON P-CORES"
		}
	}
	fmt.Fprintln(tw, cab)
	for _, mc := range live {
		svc := mc.Service
		if svc == "" {
			svc = mc.From // sin servicio, muestra el snapshot de origen
		}
		if svc == "" {
			svc = "—"
		}
		fila := fmt.Sprintf("%s\t%s\t%s\t%d MiB", mc.ID[:min(12, len(mc.ID))], trunc(svc, 20), mc.State, mc.PSSMiB)
		if cpu {
			tot, p := pctCPU(mc, antes, dt)
			if tot < 0 {
				fila += "\t—"
				if reparto {
					fila += "\t—"
				}
			} else {
				fila += fmt.Sprintf("\t%.1f%%", tot)
				if reparto {
					if tot > 0.05 {
						fila += fmt.Sprintf("\t%.0f%%", min(max(p/tot*100, 0), 100))
					} else {
						fila += "\t—"
					}
				}
			}
		}
		fmt.Fprintln(tw, fila)
	}
	if err := tw.Flush(); err != nil {
		return
	}

	// Estados que no aparecen en la tabla (frozen/stopped) sí cuentan para el resumen.
	nombre := "PSS"
	if huella {
		nombre = "memory"
	}
	fmt.Fprintf(w, "\n%d alive · %d frozen · total %s %d MiB\n",
		len(live), ps.ByState[string(api.StateWarm)], nombre, ps.TotalPSSMiB)
	switch {
	case huella && ps.AvailableMiB > 0 && ps.TotalMiB > 0:
		fmt.Fprintf(w, "host: ~%d MiB available of %d MiB (macOS memory level)\n", ps.AvailableMiB, ps.TotalMiB)
	case ps.AvailableMiB > 0 || ps.FreeMiB > 0:
		fmt.Fprintf(w, "host: MemAvailable %d MiB · MemFree %d MiB\n", ps.AvailableMiB, ps.FreeMiB)
	default:
		fmt.Fprintf(w, "host: memory unavailable from this daemon\n")
	}
}
