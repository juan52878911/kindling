package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/units"
)

// cmdVolume gestiona el almacenamiento que sobrevive a las microVMs.
//
//	kling volume create notas -size 2G
//	kling volume ls
//	kling volume rm notas
//	kling volume populate libs -- npm install --prefix /data lodash
//	kling volume snapshot notas antes-de-migrar
//	kling volume restore notas antes-de-migrar
func cmdVolume(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling volume [create|ls|rm|populate|snapshot|snapshots|restore]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create", "add":
		return volumeCreate(rest)
	case "ls", "list":
		return volumeList(rest)
	case "rm", "remove":
		return volumeRemove(rest)
	case "populate", "install":
		return volumePopulate(rest)
	case "snapshot":
		return volumeSnapshot(rest)
	case "snapshots":
		return volumeSnapshots(rest)
	case "restore":
		return volumeRestore(rest)
	default:
		return fmt.Errorf("unknown subcommand %q: use create, ls, rm, populate, snapshot, snapshots, or restore", sub)
	}
}

func volumeCreate(args []string) error {
	fs := flag.NewFlagSet("volume create", flag.ExitOnError)
	host := hostFlag(fs)
	size := fs.String("size", "1G", "logical size: 512M, 2G, 10G")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling volume create <name> [-size 2G]")
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
	fmt.Printf("%s  created  (%s logical, %s on disk)\n", v.Name, human(v.SizeBytes), human(v.UsedBytes))
	fmt.Printf("It's sparse: it only uses what gets written inside.\n")
	next("kling run -image <image> -volume %s   (or kling mcp add <server> -volume %s)", v.Name, v.Name)
	return nil
}

func volumeList(args []string) error {
	fs := flag.NewFlagSet("volume ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only names (for scripting)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	vols, err := api.NewClient(hostOf(*host)).Volumes(ctx)
	if err != nil {
		return err
	}
	if *quiet {
		for _, v := range vols {
			fmt.Println(v.Name)
		}
		return nil
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(vols)
	}
	if len(vols) == 0 {
		fmt.Println("No volumes. Create one:  kling volume create notes -size 2G")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLOGICAL\tON DISK\tSNAPS\tUSED BY")
	for _, v := range vols {
		users := strings.Join(v.UsedBy, ", ")
		if users == "" {
			users = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", v.Name, human(v.SizeBytes), human(v.UsedBytes), v.Snapshots, users)
	}
	return tw.Flush()
}

func volumeRemove(args []string) error {
	fs := flag.NewFlagSet("volume rm", flag.ExitOnError)
	host := hostFlag(fs)
	force := fs.Bool("f", false, "do not ask for confirmation")
	snaps := fs.Bool("snapshots", false, "also remove the volume's snapshots (refused otherwise)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling volume rm [-snapshots] <name>... | <name>@<snapshot>...")
	}
	// Se valida todo antes de borrar nada: un argumento mal escrito al final
	// no puede dejar la mitad borrada.
	for _, a := range fs.Args() {
		if _, _, err := splitVolumeSnapshot(a); err != nil {
			return err
		}
	}
	if !*force && !confirmMany("volume", fs.Args()) {
		return errAborted
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	for _, a := range fs.Args() {
		vol, snap, _ := splitVolumeSnapshot(a)
		var err error
		switch {
		case snap != "":
			err = c.RemoveVolumeSnapshot(ctx, vol, snap)
		case *snaps:
			err = c.RemoveVolumeWithSnapshots(ctx, vol)
		default:
			err = c.RemoveVolume(ctx, vol)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s removed\n", a)
	}
	return nil
}

// splitVolumeSnapshot separa "vol@snap". Sin @, snap vacío. Un @ sin nada a un
// lado es un error: "notas@" borraría el volumen entero creyendo borrar un
// snapshot.
func splitVolumeSnapshot(a string) (vol, snap string, err error) {
	vol, snap, found := strings.Cut(a, "@")
	if vol == "" || (found && snap == "") || strings.Contains(snap, "@") {
		return "", "", fmt.Errorf("invalid argument %q: use <volume> or <volume>@<snapshot>", a)
	}
	return vol, snap, nil
}

// volumeSnapshot copia un volumen sin escritores.
func volumeSnapshot(args []string) error {
	fs := flag.NewFlagSet("volume snapshot", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("usage: kling volume snapshot <volume> [name]   (default name: UTC time, 20260928-153012)")
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	s, err := api.NewClient(hostOf(*host)).SnapshotVolume(ctx, fs.Arg(0), api.SnapshotVolumeRequest{Name: fs.Arg(1)})
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	fmt.Printf("%s@%s  taken  (%s, %s on disk)\n", s.Volume, s.Name, s.Mode, human(s.UsedBytes))
	if s.Mode == "copy" {
		fmt.Println("It is a full copy: this filesystem cannot share blocks (XFS with reflink or Btrfs can).")
	}
	next("kling volume restore %s %s   (to go back to it)", s.Volume, s.Name)
	return nil
}

// volumeSnapshots lista los snapshots de un volumen.
func volumeSnapshots(args []string) error {
	fs := flag.NewFlagSet("volume snapshots", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only names (for scripting)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling volume snapshots <volume> [-q] [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	l, err := api.NewClient(hostOf(*host)).VolumeSnapshots(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *quiet {
		for _, s := range l {
			fmt.Println(s.Name)
		}
		return nil
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(l)
	}
	if len(l) == 0 {
		fmt.Printf("No snapshots. Take one:  kling volume snapshot %s\n", fs.Arg(0))
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCREATED (UTC)\tLOGICAL\tON DISK")
	for _, s := range l {
		name := s.Name
		if s.Undo {
			name += " (before last restore)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, s.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
			human(s.SizeBytes), human(s.UsedBytes))
	}
	return tw.Flush()
}

// volumeRestore devuelve un volumen a un snapshot. Lo que había queda en
// <vol>@undo, así que se deshace con `kling volume restore <vol> undo`.
func volumeRestore(args []string) error {
	fs := flag.NewFlagSet("volume restore", flag.ExitOnError)
	host := hostFlag(fs)
	force := fs.Bool("f", false, "do not ask for confirmation")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: kling volume restore <volume> <snapshot> [-f]")
	}
	vol, snap := fs.Arg(0), fs.Arg(1)
	if !*force && !confirm(fmt.Sprintf("replace the contents of %s with %s@%s? (the current state is kept in %s@undo)",
		vol, vol, snap, vol)) {
		return errAborted
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	res, err := api.NewClient(hostOf(*host)).RestoreVolume(ctx, vol, api.RestoreVolumeRequest{Snapshot: snap})
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Printf("%s  restored to %s  (%s)\n", res.Volume, res.Snapshot, res.Mode)
	fmt.Printf("The previous state is in %s@undo; the next restore overwrites it.\n", res.Volume)
	next("kling volume restore %s undo   (to undo this)", res.Volume)
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
		return 0, fmt.Errorf("invalid size %q: use 512M, 2G, or a plain number in MiB", s)
	}
	return n * mult, nil
}

// volumePopulate instala paquetes DENTRO de una microVM desechable.
//
//	kling volume populate libs -image filesystem-mcp -- npm install --prefix /data lodash
//
// El sentido de que sea una microVM y no el anfitrión: instalar paquetes es
// ejecutar código de terceros, y hacerlo fuera de la frontera que kindling
// levanta contradice la razón de ser del proyecto. La máquina se destruye al
// terminar, salga bien o mal.
func volumePopulate(args []string) error {
	fs := flag.NewFlagSet("volume populate", flag.ExitOnError)
	host := hostFlag(fs)
	image := fs.String("image", ToolchainImage, "image that provides the installer (npm, pip...)")
	mount := fs.String("mount", "/data", "where the volume is mounted inside the microVM")
	mem := units.MiBVar(fs, "mem", 0, "memory for the installation microVM: 512M, 1G (bare number = MiB)")

	// El "--" se separa ANTES de parsear, y no se deja en manos de flag.
	//
	// Lo que va detrás es el comando del instalador, y trae sus PROPIOS flags:
	// `npm install --prefix /data`. Si se dejara pasar por el reordenador de
	// argumentos, --prefix se interpretaría como un flag de kling y el comando
	// llegaría descuartizado. Pasó exactamente eso la primera vez.
	nuestros, cmd := args, []string(nil)
	for i, a := range args {
		if a == "--" {
			nuestros, cmd = args[:i], args[i+1:]
			break
		}
	}
	if err := fs.Parse(reorderFor(fs, nuestros)); err != nil {
		return err
	}
	if fs.NArg() < 1 || len(cmd) == 0 {
		return fmt.Errorf("usage: kling volume populate <name> [-image IMG] -- <command>\n" +
			"  e.g.:  kling volume populate libs -- \\\n" +
			"             npm install --prefix /data --ignore-scripts lodash zod\n" +
			"  (uses the `toolchain` image by default: build it with `kling image toolchain`)")
	}
	name := fs.Arg(0)
	if *image == "" {
		return fmt.Errorf("missing -image: an image that provides the installer is required")
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	fmt.Printf("Populating %q inside a single-use microVM.\n", name)
	fmt.Printf("  image:    %s\n", *image)
	fmt.Printf("  mounted:  %s\n", *mount)
	fmt.Printf("  command:  %s\n\n", strings.Join(cmd, " "))
	fmt.Print("  installing (may take a while; third-party code runs inside)... ")

	res, err := api.NewClient(hostOf(*host)).PopulateVolume(ctx, api.PopulateRequest{
		Volume: name, Mount: *mount, Image: *image, Cmd: cmd, MemMiB: *mem,
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	if res.ExitCode != 0 {
		fmt.Println("✗")
		// La salida completa, no un resumen: si un install falla, el motivo
		// está en su propia salida y recortarla obliga a repetirlo todo para
		// verlo.
		fmt.Fprintln(os.Stderr, res.Output)
		return fmt.Errorf("the command exited with code %d", res.ExitCode)
	}
	fmt.Println("✓")
	if res.Output != "" {
		fmt.Println(indent(strings.TrimRight(res.Output, "\n")))
	}
	fmt.Printf("\n%s now uses %d MiB. Mount it wherever needed:\n", name, res.UsedMiB)
	fmt.Printf("  kling run -image <image> -volume %s:/libs:ro\n", name)
	return nil
}

// indent sangra un bloque de salida ajena para que se distinga de lo nuestro.
func indent(s string) string {
	if s == "" {
		return ""
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
