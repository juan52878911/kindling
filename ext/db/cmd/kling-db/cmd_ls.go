package main

// kling db ls: las copias de un dueño, con lo que hay que saber de cada una
// de un vistazo. Sobre todo lo que no se ve en `kling ps` sin saber buscarlo:
// una copia que el daemon pausó porque el almacén de copia al escribir se
// llenó (Hold) y una cuyo invitado vio errores de disco (sus datos pueden
// estar dañados).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// copiaLs es una fila de `kling db ls -json`. Sin contraseñas ni direcciones:
// para eso está connect.
type copiaLs struct {
	Name       string    `json:"name"`
	ID         string    `json:"id"`
	Engine     string    `json:"engine"`
	Golden     string    `json:"golden"`
	State      string    `json:"state"`
	Ready      bool      `json:"ready"`
	Created    time.Time `json:"created_at"`
	Hold       string    `json:"hold,omitempty"`
	DiskErrors int       `json:"disk_errors,omitempty"`
	DiskError  string    `json:"disk_error,omitempty"`
}

func cmdLs(args []string) error {
	fs, host, owner := newFlags("ls")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("usage: kling db ls [-owner T] [-json]")
	}
	if err := validOwner(*owner); err != nil {
		return err
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	return a.ls(ctx, *owner, *asJSON)
}

func (a *app) ls(ctx context.Context, owner string, asJSON bool) error {
	list, err := a.machines(ctx)
	if err != nil {
		return err
	}
	var filas []copiaLs
	for _, mc := range list {
		if mc.Labels[labelGolden] == "" || mc.Labels[labelOwner] != owner {
			continue
		}
		eng := mc.Labels[labelEngine]
		if eng == "" {
			eng = "postgres"
		}
		filas = append(filas, copiaLs{
			Name: mc.Name, ID: mc.ID, Engine: eng, Golden: mc.Labels[labelGolden],
			State: string(mc.State), Ready: mc.Labels[labelState] == api.DBStateReady, Created: mc.CreatedAt,
			Hold: mc.Hold, DiskErrors: mc.DiskErrors, DiskError: mc.DiskError,
		})
	}
	sort.Slice(filas, func(i, j int) bool { return filas[i].Name < filas[j].Name })
	if asJSON {
		if filas == nil {
			filas = []copiaLs{}
		}
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(filas)
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENGINE\tGOLDEN\tSTATE\tAGE")
	var avisos []string
	for _, f := range filas {
		st := f.State
		if !f.Ready {
			st += " (preparing)"
		}
		if f.Hold != "" || f.DiskErrors > 0 {
			st += "!"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Name, f.Engine, f.Golden, st, edad(f.Created))
		if f.Hold != "" {
			avisos = append(avisos, fmt.Sprintf("%s: on hold (%s); it resumes on its own once there is room: kling cow grow +4G", f.Name, f.Hold))
		}
		if f.DiskErrors > 0 {
			avisos = append(avisos, fmt.Sprintf("%s: its guest got %d disk I/O error(s) (%q); its data may be damaged: kling db doctor %s", f.Name, f.DiskErrors, f.DiskError, f.Name))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range avisos {
		fmt.Fprintln(a.stdout, "! "+s)
	}
	return nil
}

// edad es el tiempo desde t, compacto (3m, 2h, 5d).
func edad(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
