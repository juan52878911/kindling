package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// cmdCoW es `kling cow`: el estado de las copias de disco (la línea de
// `kling info`) y `kling cow grow <size>`, que amplía el almacén de copia al
// escribir sin parar nada (docs/cow.md).
func cmdCoW(args []string) error {
	if len(args) > 0 && args[0] == "grow" {
		return cmdCoWGrow(args[1:])
	}
	fs := flag.NewFlagSet("cow", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unknown subcommand %q: use grow", fs.Arg(0))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	info, err := api.NewClient(hostOf(*host)).Info(ctx)
	if err != nil {
		return err
	}
	if info.CoW == nil {
		return errors.New("this daemon does not report its disk clones (it predates daemon.cow)")
	}
	fmt.Println(lineaCoW(info.CoW))
	// Las que el daemon pausó porque el almacén se llenó (y las que vieron
	// errores de disco): es lo primero que hay que saber con el almacén lleno.
	if list, err := api.NewClient(hostOf(*host)).List(ctx); err == nil {
		for _, mc := range list {
			for _, a := range avisosMaquina(mc) {
				fmt.Println("! " + a)
			}
		}
	}
	if c := casiLlenoCoW(info.CoW.Store); c != "" {
		next("kling cow grow +8G   (%s)", c)
	}
	return nil
}

func cmdCoWGrow(args []string) error {
	fs := flag.NewFlagSet("cow grow", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return &errWithHint{err: errors.New("usage: kling cow grow <size>|+<size>"),
			hint: "kling cow grow 32G  (new size)   or   kling cow grow +8G  (how much to add)"}
	}
	req, err := parseCrecimiento(fs.Arg(0))
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if !info.Has("cow-grow") {
		return errors.New("this daemon can't grow its copy-on-write store: update it")
	}
	st, err := c.GrowCoWStore(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("copy-on-write store %s (%s): %d MiB, %d MiB free\n", st.Path, nombreFSAlmacen(st), st.SizeMiB, st.FreeMiB)
	return nil
}

// parseCrecimiento lee el tamaño de `kling cow grow`: "32G" es el tamaño
// nuevo y "+8G" lo que se añade (M, G o MiB sueltos, como en volume create).
func parseCrecimiento(s string) (api.GrowCoWStoreRequest, error) {
	resto, suma := strings.CutPrefix(strings.TrimSpace(s), "+")
	n, err := parseSizeMiB(resto)
	if err != nil {
		return api.GrowCoWStoreRequest{}, err
	}
	if suma {
		return api.GrowCoWStoreRequest{AddMiB: int64(n)}, nil
	}
	return api.GrowCoWStoreRequest{SizeMiB: int64(n)}, nil
}
