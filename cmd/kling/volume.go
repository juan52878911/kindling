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
	default:
		return fmt.Errorf("subcomando desconocido %q: usa create, ls o rm", sub)
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
