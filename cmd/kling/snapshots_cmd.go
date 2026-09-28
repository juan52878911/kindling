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

// kling template ls|rm|inspect.
//
// "Plantilla" es el nombre público del snapshot dorado: lo que `save` crea y
// `run -from` instancia en milisegundos. `snapshots` y `rmi` siguen
// funcionando como alias (tree.go).

func cmdTemplate(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return snapshotsList(args)
	case "rm", "remove":
		return snapshotsRemove("template rm", args)
	case "inspect", "show":
		return snapshotsInspect(args)
	case "credential":
		return snapshotsCredential(args)
	}
	return fmt.Errorf("unknown subcommand %q: use ls, rm, inspect or credential", sub)
}

// snapshotsCredential es `kling template credential`: ata una clave a una
// plantilla para que cada instancia que nazca de ella (kling run -from, las
// réplicas del gateway MCP) la reciba en su proxy de credenciales al arrancar.
// Es el camino para un servicio MCP: nadie está delante para hacer `machine
// credential` a cada réplica. Como allí, la clave no viaja por la línea de
// comandos: -f o stdin.
func snapshotsCredential(args []string) error {
	fs := flag.NewFlagSet("template credential", flag.ExitOnError)
	host := hostFlag(fs)
	domain := fs.String("domain", "", "the only host the key is sent to, e.g. api.stripe.com")
	env := fs.String("env", "", "environment variable that receives the placeholder, e.g. STRIPE_API_KEY")
	file := fs.String("f", "", "file with the key (default: stdin)")
	clear := fs.Bool("clear", false, "remove every credential of the template")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || (!*clear && (*domain == "" || *env == "")) {
		return errors.New("usage: kling template credential <template> -domain api.example.com -env API_KEY [-f keyfile]  (reads stdin if no -f)\n" +
			"       kling template credential <template> -clear")
	}
	req := api.CredentialsRequest{Clear: *clear}
	if !*clear {
		secret, err := leerClave(*file)
		if err != nil {
			return err
		}
		req.Credentials = []api.CredentialSpec{{Domain: *domain, Env: *env, Secret: secret}}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	s, err := api.NewClient(hostOf(*host)).SetSnapshotCredentials(ctx, fs.Arg(0), req)
	if err != nil {
		return err
	}
	if *clear {
		fmt.Printf("%s  no credentials; new instances get none (running ones keep theirs)\n", s.Name)
		return nil
	}
	fmt.Printf("%s  every new instance gets a placeholder in %s; the key only goes to https://%s through its proxy\n",
		s.Name, *env, strings.ToLower(*domain))
	fmt.Printf("      point the SDK at http://%s; running instances are not changed (kling machine credential does that)\n",
		strings.ToLower(*domain))
	return nil
}

// leerClave lee la clave de un fichero o de stdin, sin espacios alrededor.
func leerClave(file string) (string, error) {
	var raw []byte
	var err error
	if file != "" {
		raw, err = os.ReadFile(file)
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	}
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", errors.New("the key is empty")
	}
	return secret, nil
}

func snapshotsList(args []string) error {
	fs := flag.NewFlagSet("template ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only names (for scripting)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	list, err := api.NewClient(hostOf(*host)).Snapshots(ctx)
	if err != nil {
		return err
	}
	if *quiet {
		for _, s := range list {
			fmt.Fprintln(os.Stdout, s.Name)
		}
		return nil
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
	force := fs.Bool("f", false, "do not ask for confirmation")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling %s <template>...", name)
	}
	if !*force && !confirmMany("template", fs.Args()) {
		return errAborted
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
	fs := flag.NewFlagSet("template inspect", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling template inspect <template> [-json]")
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
