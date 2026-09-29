package main

// Puntos de guardado de una copia: kling db snapshot | snapshots | undo.
//
// Un punto de guardado es una PLANTILLA de kindling hecha con `kling save` de
// la máquina viva (memoria y disco), y undo es un reset de la copia desde esa
// plantilla: misma nombre, mismo dueño y ttl, id nuevo y contraseña nueva.
//
// Identidad. Una plantilla es de una copia si cumple las tres:
//   - su nombre es dbsnap-<hash(dueño, nombre de la copia)>-<nombre>
//   - su etiqueta kling.db.owner es el dueño
//   - su etiqueta kling.db.snapshot-of es el "linaje" de la copia: el id de la
//     máquina en el primer punto de guardado. Undo crea una máquina con otro
//     id, y por eso lleva el linaje en la misma etiqueta; sin él perdería sus
//     puntos en cada undo. Un fork hereda la etiqueta, pero su nombre es otro,
//     así que no ve los puntos de su origen.
//
// Contraseña: la RAM y el disco guardados llevan el verificador de la clave que
// tenía la copia en ese momento. Undo lo sustituye: la copia nueva rota una
// clave propia antes de darse por lista, y la del punto no sirve para nada
// (ver docs/db.md).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	labelSnapshotOf = "kling.db.snapshot-of"
	snapPrefix      = "dbsnap-"
	// maxSnapshots es el límite de puntos de guardado por copia: cada uno es
	// una plantilla con su memoria y su disco.
	maxSnapshots = 16
)

// snapNamePattern: el nombre que pone el usuario. Corto para que el nombre de
// la plantilla (dbsnap- + 8 + - + nombre) quepa en los 64 de namePattern.
var snapNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

var lineagePattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// lineage es el id con el que esta copia firma sus puntos de guardado.
func lineage(mc *api.Machine) string {
	if v := mc.Labels[labelSnapshotOf]; lineagePattern.MatchString(v) {
		return v
	}
	return mc.ID
}

// snapTemplatePrefix es el prefijo de las plantillas de esa copia de ese dueño.
func snapTemplatePrefix(owner, copyName string) string {
	h := sha256.Sum256([]byte(owner + "\x00" + copyName))
	return snapPrefix + hex.EncodeToString(h[:4]) + "-"
}

func validSnapName(n string) error {
	if !snapNamePattern.MatchString(n) {
		return fmt.Errorf("invalid snapshot name %q: lowercase letters, digits and '-', at most 40, starting with a letter or digit", n)
	}
	return nil
}

// ── listado ──────────────────────────────────────────────────────────────────

type snapshotInfo struct {
	Name      string    `json:"name"`
	Template  string    `json:"template"`
	CreatedAt time.Time `json:"created_at"`
	DiskBytes int64     `json:"disk_bytes"`
	MemBytes  int64     `json:"mem_bytes"`
	Instances int       `json:"instances"`
}

// copySnapshots son las plantillas de puntos de guardado de la copia mc, de la
// más vieja a la más nueva.
func (a *app) copySnapshots(ctx context.Context, mc *api.Machine, owner string) ([]*api.Snapshot, error) {
	out, err := a.k.Run(ctx, nil, "template", "ls", "-json")
	if err != nil {
		return nil, err
	}
	if len(out) > maxJSON {
		return nil, errors.New("kling template ls: output too large")
	}
	var all []*api.Snapshot
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("kling template ls: %w", err)
	}
	prefix, lin := snapTemplatePrefix(owner, mc.Name), lineage(mc)
	var mine []*api.Snapshot
	for _, s := range all {
		if s == nil || !strings.HasPrefix(s.Name, prefix) || len(s.Name) == len(prefix) {
			continue
		}
		if s.Labels[labelOwner] != owner || s.Labels[labelGolden] == "" || s.Labels[labelSnapshotOf] != lin {
			continue
		}
		mine = append(mine, s)
	}
	sort.SliceStable(mine, func(i, j int) bool {
		if !mine[i].CreatedAt.Equal(mine[j].CreatedAt) {
			return mine[i].CreatedAt.Before(mine[j].CreatedAt)
		}
		return mine[i].Name < mine[j].Name
	})
	return mine, nil
}

func cmdSnapshots(args []string) error {
	fs, host, owner := newFlags("snapshots")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db snapshots <copy> [-json]")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	list, err := a.listSnapshots(ctx, pos[0], *owner)
	if err != nil {
		return err
	}
	return writeSnapshotList(a.stdout, list, *asJSON)
}

func (a *app) listSnapshots(ctx context.Context, ref, owner string) ([]snapshotInfo, error) {
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := owned(mc, owner); err != nil {
		return nil, err
	}
	snaps, err := a.copySnapshots(ctx, mc, owner)
	if err != nil {
		return nil, err
	}
	prefix := snapTemplatePrefix(owner, mc.Name)
	out := make([]snapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, snapshotInfo{Name: strings.TrimPrefix(s.Name, prefix), Template: s.Name,
			CreatedAt: s.CreatedAt, DiskBytes: s.DiskBytes, MemBytes: s.MemBytes, Instances: s.Instances})
	}
	return out, nil
}

func writeSnapshotList(w io.Writer, list []snapshotInfo, asJSON bool) error {
	if asJSON {
		if list == nil {
			list = []snapshotInfo{}
		}
		return json.NewEncoder(w).Encode(list)
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "no snapshots")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCREATED\tMEMORY\tDISK\tCOPIES BORN FROM IT")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", s.Name, s.CreatedAt.Format(time.RFC3339),
			humanBytes(s.MemBytes), humanBytes(s.DiskBytes), s.Instances)
	}
	return tw.Flush()
}

// ── snapshot y snapshot -rm ──────────────────────────────────────────────────

func cmdSnapshot(args []string) error {
	fs, host, owner := newFlags("snapshot")
	rm := fs.Bool("rm", false, "remove the snapshot instead of taking it")
	force := fs.Bool("force", false, "take it even with client connections open (their sockets would exist in every copy born from it)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("usage: kling db snapshot <copy> <name> [-force]   |   kling db snapshot -rm <copy> <name>")
	}
	if *rm && *force {
		return usageErr("-rm and -force exclude each other")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	if *rm {
		if err := a.snapshotRemove(ctx, pos[0], pos[1], *owner); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, pos[1])
		return nil
	}
	tpl, err := a.snapshot(ctx, pos[0], pos[1], *owner, *force)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s  snapshot of %s  (template %s)\n", pos[1], pos[0], tpl)
	fmt.Fprintf(a.stdout, "  back to it:  kling db undo %s %s\n", pos[0], pos[1])
	return nil
}

// snapshot guarda la copia viva como plantilla y devuelve su nombre.
func (a *app) snapshot(ctx context.Context, ref, name, owner string, force bool) (string, error) {
	if err := validOwner(owner); err != nil {
		return "", err
	}
	if err := validSnapName(name); err != nil {
		return "", err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return "", err
	}
	if err := checkReady(mc, owner); err != nil {
		return "", err
	}
	if err := requirePostgres(mc, "snapshot"); err != nil {
		return "", err
	}
	existing, err := a.copySnapshots(ctx, mc, owner)
	if err != nil {
		return "", err
	}
	if len(existing) >= maxSnapshots {
		return "", fmt.Errorf("%s already has %d snapshots (the limit): remove one with kling db snapshot -rm %s <name>", mc.Name, len(existing), mc.Name)
	}
	tpl := snapTemplatePrefix(owner, mc.Name) + name
	for _, s := range existing {
		if s.Name == tpl {
			return "", fmt.Errorf("%s already has a snapshot called %q", mc.Name, name)
		}
	}
	if !force {
		out, err := a.k.Run(ctx, strings.NewReader(clientsSQL), "exec", "-i", "-timeout", "60s", mc.ID, "--",
			"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
		if err != nil {
			return "", fmt.Errorf("counting the connections of %s: %w", mc.Name, err)
		}
		if n := strings.TrimSpace(string(out)); n != "0" {
			return "", fmt.Errorf("%s has %s client connection(s) open; close them first, or -force", mc.Name, n)
		}
	}
	// A disco lo pendiente: el punto es coherente aunque no se haga (se guarda
	// la RAM entera), pero así la recuperación tras un undo es más corta.
	if _, err := a.k.Run(ctx, strings.NewReader("CHECKPOINT;\n"), "exec", "-i", "-timeout", "5m", mc.ID, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper); err != nil {
		return "", fmt.Errorf("checkpoint of %s: %w", mc.Name, err)
	}
	// Commit copia las etiquetas de la máquina a la plantilla: el linaje se
	// pone antes, en la máquina. (El núcleo no borra etiquetas: se queda en ella,
	// y es su propio linaje.)
	lin := lineage(mc)
	if err := a.k.SetLabels(ctx, mc.ID, map[string]string{labelSnapshotOf: lin}); err != nil {
		return "", fmt.Errorf("labelling %s: %w", mc.Name, err)
	}
	// Sin -replace: nunca se pisa una plantilla ajena con ese nombre.
	if _, err := a.k.Run(ctx, nil, "save", "-warm=false", mc.ID, tpl); err != nil {
		return "", err
	}
	// Se comprueba lo que quedó: si la plantilla no lleva lo que la identifica,
	// nadie la vería y ocuparía disco para siempre.
	s, err := a.template(ctx, tpl)
	if err == nil && (s.Labels[labelSnapshotOf] != lin || s.Labels[labelOwner] != owner || s.Labels[labelGolden] == "") {
		err = errors.New("the saved template lacks the labels that identify it")
	}
	if err != nil {
		if _, rerr := a.k.Run(context.WithoutCancel(ctx), nil, "template", "rm", "-f", tpl); rerr != nil {
			fmt.Fprintf(a.stderr, "warning: could not remove the template %s: %v\n", tpl, rerr)
		}
		return "", fmt.Errorf("snapshot %s of %s discarded: %w", name, mc.Name, err)
	}
	return tpl, nil
}

const clientsSQL = "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid();\n"

func (a *app) snapshotRemove(ctx context.Context, ref, name, owner string) error {
	if err := validOwner(owner); err != nil {
		return err
	}
	if err := validSnapName(name); err != nil {
		return err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if err := owned(mc, owner); err != nil {
		return err
	}
	snaps, err := a.copySnapshots(ctx, mc, owner)
	if err != nil {
		return err
	}
	tpl := snapTemplatePrefix(owner, mc.Name) + name
	for _, s := range snaps {
		if s.Name == tpl {
			// -f solo evita la pregunta: kling se niega igualmente si hay copias
			// vivas nacidas de ella (un undo las deja así), y cuenta el motivo.
			_, err := a.k.Run(ctx, nil, "template", "rm", "-f", tpl)
			return err
		}
	}
	return fmt.Errorf("%s has no snapshot called %q", mc.Name, name)
}

// ── undo ─────────────────────────────────────────────────────────────────────

func cmdUndo(args []string) error {
	fs, host, owner := newFlags("undo")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageErr("usage: kling db undo <copy> [<snapshot>]")
	}
	name := ""
	if len(pos) == 2 {
		name = pos[1]
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	mc, err := a.undo(ctx, pos[0], name, *owner)
	if err != nil {
		return err
	}
	a.printReady(mc)
	return nil
}

// undo devuelve la copia al punto de guardado name (el último si name es
// vacío): la copia actual se borra y otra con su nombre, dueño y ttl nace de la
// plantilla, con id y contraseña nuevos.
func (a *app) undo(ctx context.Context, ref, name, owner string) (*api.Machine, error) {
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	if name != "" {
		if err := validSnapName(name); err != nil {
			return nil, err
		}
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := owned(mc, owner); err != nil {
		return nil, err
	}
	if st := mc.Labels[labelState]; st != stateReady {
		return nil, fmt.Errorf("%s is not ready (%s=%q)", mc.Name, labelState, st)
	}
	if err := requirePostgres(mc, "undo"); err != nil {
		return nil, err
	}
	golden := mc.Labels[labelGolden]
	snaps, err := a.copySnapshots(ctx, mc, owner)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("%s has no snapshots (kling db snapshot %s <name>)", mc.Name, mc.Name)
	}
	pick := snaps[len(snaps)-1]
	if name != "" {
		want := snapTemplatePrefix(owner, mc.Name) + name
		pick = nil
		for _, s := range snaps {
			if s.Name == want {
				pick = s
			}
		}
		if pick == nil {
			return nil, fmt.Errorf("%s has no snapshot called %q", mc.Name, name)
		}
	}
	lin := lineage(mc)
	ttl := time.Duration(mc.TTLSeconds) * time.Second
	if err := a.remove(ctx, mc); err != nil {
		return nil, err
	}
	nmc, err := a.upFrom(ctx, pick.Name, golden, mc.Name, ttl, owner, [][2]string{{labelSnapshotOf, lin}})
	if err != nil {
		return nil, fmt.Errorf("%w (the snapshot is intact; the copy was removed: recreate it with kling db up %s -name %s)", err, pick.Name, mc.Name)
	}
	return nmc, nil
}
