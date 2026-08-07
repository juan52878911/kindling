// kling gestiona microVMs de Firecracker con una interfaz al estilo de docker.
//
// El mismo binario hace de CLI y de daemon: `kling daemon` arranca el núcleo
// donde esté KVM, y el CLI le habla por un socket Unix, local o a través de SSH.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/daemon"
	"github.com/juan52878911/kindling/internal/transport"
)

const usage = `kling - microVMs de Firecracker con interfaz tipo docker

USO
  kling <comando> [opciones]

MÁQUINAS
  run [-name N] [-image I] [-cpus N] [-mem MiB]   crea y arranca una microVM
  ps [-a]                                          lista las máquinas
  freeze <ref>                                     congela en snapshot -> warm
  thaw <ref>                                       restaura desde snapshot (~ms)
  stop <ref>                                       termina la máquina
  rm <ref>                                         elimina máquina y snapshot

OBSERVACIÓN
  events                                           stream de eventos del daemon
  info                                             estado del daemon

DAEMON
  daemon [-socket S] [-root R] [-firecracker BIN]  arranca el núcleo

CONEXIÓN
  El CLI usa $KLING_HOST, o -H, o el socket local por defecto.
    export KLING_HOST=ssh://juan@192.168.2.60     daemon remoto por SSH
    export KLING_HOST=/run/kling.sock             daemon local

  El daemon jamás escucha en un puerto de red: controlar microVMs equivale a
  root en su host, así que el único acceso remoto es SSH.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "daemon":
		err = cmdDaemon(args)
	case "dial-stdio": // extremo remoto del transporte SSH, no para uso manual
		err = transport.ServeStdio(envOr("KLING_SOCKET", transport.DefaultSocket), os.Stdin, os.Stdout)
	case "run":
		err = cmdRun(args)
	case "ps":
		err = cmdPS(args)
	case "freeze", "thaw", "stop", "rm":
		err = cmdLifecycle(cmd, args)
	case "events":
		err = cmdEvents(args)
	case "info":
		err = cmdInfo(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "comando desconocido: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// hostFlag registra -H en cualquier subcomando del cliente.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("H", os.Getenv("KLING_HOST"), "endpoint del daemon (socket o ssh://usuario@host)")
}

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ── daemon ────────────────────────────────────────────────────────────────────

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", envOr("KLING_SOCKET", transport.DefaultSocket), "socket Unix a servir")
	root := fs.String("root", envOr("KLING_ROOT", "/var/lib/kindling"), "directorio de datos")
	fcBin := fs.String("firecracker", envOr("KLING_FIRECRACKER", "firecracker"), "binario de firecracker")
	sockUser := fs.String("socket-user", os.Getenv("KLING_SOCKET_USER"), "usuario al que ceder el socket (para el CLI por SSH)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	srv, err := daemon.New(*socket, *root, *fcBin, *sockUser)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	return srv.Listen(ctx)
}

// ── máquinas ──────────────────────────────────────────────────────────────────

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "nombre de la máquina")
	image := fs.String("image", "default", "imagen de rootfs")
	cpus := fs.Int("cpus", 1, "vCPUs")
	mem := fs.Int("mem", 256, "memoria en MiB")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	mc, err := api.NewClient(*host).Run(ctx, api.RunRequest{
		Name: *name, Image: *image, VCPUs: *cpus, MemMiB: *mem,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s  arrancada en frío en %d ms\n", mc.ID[:12], mc.Name, mc.BootMS)
	return nil
}

func cmdPS(args []string) error {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	host := hostFlag(fs)
	all := fs.Bool("a", false, "incluye las paradas")
	asJSON := fs.Bool("json", false, "salida en JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	list, err := api.NewClient(*host).List(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(list)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tNOMBRE\tIMAGEN\tESTADO\tCPU/MEM\tDISCO\tEDAD\tÚLTIMA OP")
	var totalDisk int64
	for _, mc := range list {
		if !*all && (mc.State == api.StateStopped || mc.State == api.StateFailed) {
			continue
		}
		totalDisk += mc.DiskBytes
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%dMiB\t%s\t%s\t%s\n",
			mc.ID[:12], mc.Name, mc.Image, mc.State,
			mc.VCPUs, mc.MemMiB, human(mc.DiskBytes), since(mc.CreatedAt), lastOp(mc))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if totalDisk > 0 {
		fmt.Printf("\ndisco propio de las máquinas: %s (la imagen base se comparte)\n", human(totalDisk))
	}
	return nil
}

// human formatea bytes de forma compacta.
func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dM", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%dK", b>>10)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// lastOp resume el coste de la última transición: es el número que justifica
// todo el diseño de snapshot/restore.
func lastOp(mc *api.Machine) string {
	switch {
	case mc.ThawMS > 0 && mc.State == api.StateRunning:
		return fmt.Sprintf("thaw %dms", mc.ThawMS)
	case mc.State == api.StateWarm:
		return fmt.Sprintf("freeze %dms, %dMiB", mc.FreezeMS, mc.SnapSize>>20)
	case mc.BootMS > 0:
		return fmt.Sprintf("boot %dms", mc.BootMS)
	}
	return "-"
}

func since(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func cmdLifecycle(op string, args []string) error {
	fs := flag.NewFlagSet(op, flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling %s <ref>", op)
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(*host)

	for _, ref := range fs.Args() {
		var mc *api.Machine
		var err error
		switch op {
		case "freeze":
			mc, err = c.Freeze(ctx, ref)
		case "thaw":
			mc, err = c.Thaw(ctx, ref)
		case "stop":
			mc, err = c.Stop(ctx, ref)
		case "rm":
			err = c.Remove(ctx, ref)
		}
		if err != nil {
			return err
		}
		switch {
		case op == "rm":
			fmt.Println(ref)
		case op == "freeze":
			fmt.Printf("%s  warm  (%d ms, %d MiB en disco)\n", mc.ID[:12], mc.FreezeMS, mc.SnapSize>>20)
		case op == "thaw":
			fmt.Printf("%s  running  (%d ms)\n", mc.ID[:12], mc.ThawMS)
		default:
			fmt.Printf("%s  %s\n", mc.ID[:12], mc.State)
		}
	}
	return nil
}

// ── observación ───────────────────────────────────────────────────────────────

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "una línea JSON por evento")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	return api.NewClient(*host).Events(ctx, func(ev api.Event) {
		if *asJSON {
			_ = enc.Encode(ev)
			return
		}
		line := fmt.Sprintf("%s  %-16s %s", ev.Time.Format("15:04:05"), ev.Type, ev.Name)
		if ev.Message != "" {
			line += "  " + ev.Message
		}
		fmt.Println(line)
	})
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(*host)
	i, err := c.Info(ctx)
	if err != nil {
		return err
	}
	kvm := "no"
	if i.KVM {
		kvm = "sí"
	}
	fmt.Printf("endpoint:     %s\n", c.Endpoint())
	fmt.Printf("daemon:       %s\n", i.Version)
	fmt.Printf("root:         %s\n", i.Root)
	fmt.Printf("KVM:          %s\n", kvm)
	fmt.Printf("firecracker:  %s\n", strings.TrimSpace(i.Firecrack))
	fmt.Printf("máquinas:     %d\n", i.Machines)
	return nil
}
