package main

import (
	"flag"
	"fmt"
	"github.com/juan52878911/kindling-mcp/internal/mcp"
	"github.com/juan52878911/kindling-mcp/internal/report"
	"github.com/juan52878911/kindling/pkg/api"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	host := hostFlag(fs)
	out := fs.String("o", "kindling.html", "output file")
	// El mapa ya trae todo el detalle; la bandera sigue aceptándose para no
	// romper a quien la tuviera en un script.
	_ = fs.Bool("detail", false, "deprecated: the report always includes detail")
	open := fs.Bool("open", false, "open it when done")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	machines, err := c.List(ctx)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}

	// El HTML se construye aquí, en la máquina del CLI: el fichero acaba donde
	// trabajas aunque el daemon esté al otro lado de un SSH.
	links, _ := mcp.Links(ctx, c) // un daemon antiguo puede no tenerlos: no es fatal
	memSvc := ""
	if on, svc := memoryConfig(loadConfig()); on {
		memSvc = svc
	}
	groups := report.BuildWith(machines, snaps, links)
	doc := report.RenderMap(info, groups, c.Endpoint(), time.Now(), memSvc)
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		return err
	}
	abs, _ := filepath.Abs(*out)
	fmt.Printf("%s  (%d machines, %d snapshots, %.0f KB)\n",
		abs, len(machines), len(snaps), float64(len(doc))/1024)

	if *open {
		_ = exec.Command(openCmd(), abs).Start()
	}
	return nil
}

func openCmd() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}
