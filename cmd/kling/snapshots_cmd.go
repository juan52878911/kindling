package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/api"
)

// kling snapshots ls|rm|inspect.
//
// Los snapshots eran el único sustantivo sin la forma de los demás: se creaban
// con commit, se listaban con `snapshots` y se borraban con `rmi`. Ahora siguen
// el ls/rm de volume, images o sandbox; `snapshots` a secas sigue listando y
// `rmi` se conserva como alias de `snapshots rm` para no romper scripts.

func cmdSnapshots(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return snapshotsList(args)
	case "rm", "remove":
		return snapshotsRemove("snapshots rm", args)
	case "inspect", "show":
		return snapshotsInspect(args)
	}
	return fmt.Errorf("unknown subcommand %q: use ls, rm or inspect", sub)
}

// cmdRmi es el alias histórico de `snapshots rm`.
func cmdRmi(args []string) error { return snapshotsRemove("rmi", args) }

func snapshotsList(args []string) error {
	fs := flag.NewFlagSet("snapshots ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	list, err := api.NewClient(hostOf(*host)).Snapshots(ctx)
	if err != nil {
		return err
	}
	return writeSnapshots(os.Stdout, list, *asJSON)
}

// writeSnapshots vive aparte de la llamada al daemon para poder probar la
// tabla y el JSON sin uno.
func writeSnapshots(w io.Writer, list []*api.Snapshot, asJSON bool) error {
	if asJSON {
		if list == nil {
			list = []*api.Snapshot{} // [] y no null: más fácil para jq
		}
		return json.NewEncoder(w).Encode(list)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tCPU/MEM\tMEMORY\tDISK\tINSTANCES\tAGE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%d/%dMiB\t%s\t%s\t%d\t%s\n",
			s.Name, s.Image, s.VCPUs, s.MemMiB,
			human(s.MemBytes), human(s.DiskBytes), s.Instances, since(s.CreatedAt))
	}
	return tw.Flush()
}

func snapshotsRemove(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling %s <snapshot>...", name)
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	for _, n := range fs.Args() {
		if err := c.RemoveSnapshot(ctx, n); err != nil {
			return err
		}
		fmt.Println(n)
	}
	return nil
}

func snapshotsInspect(args []string) error {
	fs := flag.NewFlagSet("snapshots inspect", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling snapshots inspect <snapshot> [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	s, err := api.NewClient(hostOf(*host)).Snapshot(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return writeSnapshot(os.Stdout, s, *asJSON)
}

// writeSnapshot imprime un snapshot. Las anotaciones son datos opacos de las
// extensiones: se enseña la clave y un extracto, no se interpretan.
func writeSnapshot(w io.Writer, s *api.Snapshot, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	fmt.Fprintf(w, "name:        %s\n", s.Name)
	fmt.Fprintf(w, "image:       %s\n", s.Image)
	fmt.Fprintf(w, "created:     %s (%s ago)\n", s.CreatedAt.Local().Format("2006-01-02 15:04:05"), since(s.CreatedAt))
	mem := fmt.Sprintf("%d MiB", s.MemMiB)
	if s.MemMaxMiB > 0 {
		mem += fmt.Sprintf(" (ceiling %d MiB)", s.MemMaxMiB)
	}
	fmt.Fprintf(w, "cpus/mem:    %d / %s\n", s.VCPUs, mem)
	fmt.Fprintf(w, "on disk:     %s memory, %s total\n", human(s.MemBytes), human(s.DiskBytes))
	fmt.Fprintf(w, "instances:   %d\n", s.Instances)
	for i, v := range s.VolumeSet() {
		label := "volumes:"
		if i > 0 {
			label = ""
		}
		ro := ""
		if v.ReadOnly {
			ro = " (ro)"
		}
		fmt.Fprintf(w, "%-13s%s -> %s%s\n", label, v.Name, v.Mount, ro)
	}
	if len(s.Annotations) > 0 {
		keys := make([]string, 0, len(s.Annotations))
		for k := range s.Annotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "annotations:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s  %s\n", k, excerpt(s.Annotations[k], 60))
		}
	}
	fmt.Fprintf(w, "\ninstantiate with:  kling run -from %s\n", s.Name)
	return nil
}

// excerpt acorta un JSON a n caracteres para una sola línea.
func excerpt(raw json.RawMessage, n int) string {
	r := []rune(strings.Join(strings.Fields(string(raw)), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
