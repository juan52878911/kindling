package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Firecracker y jailer para `sudo kling up -install`: quien instaló kling con
// install.sh no tiene un checkout con scripts/20-install-firecracker.sh. Se
// baja como ese script: una versión FIJADA y el sha256 de su tgz fijado aquí
// (nunca «latest»), y solo corre como root, dentro de `up -install`. La tabla
// y la del script son la misma: un test las compara.

// fcVersion es la versión de Firecracker que se instala.
const fcVersion = "v1.17.0"

// fcPin es el sha256 del tgz de una versión para una arquitectura (la de
// `uname -m`).
type fcPin struct{ Version, Arch, SHA256 string }

var fcPins = []fcPin{
	{"v1.17.0", "x86_64", "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558"},
	{"v1.17.0", "aarch64", "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256"},
}

const (
	fcMaxTgz     = 200 << 20 // el tgz de la release pesa unas decenas de MiB
	fcMaxBinario = 256 << 20
	fcPlazo      = 5 * time.Minute
)

// fcDescarga es una descarga con todo lo que la comprueba.
type fcDescarga struct {
	URL      string
	SHA256   string // el esperado del tgz
	Version  string
	Arch     string
	Dir      string // donde quedan firecracker y jailer
	Cliente  *http.Client
	MaxBytes int64         // tope del tgz; 0, fcMaxTgz
	Plazo    time.Duration // 0, fcPlazo
}

func archFC() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	}
	return "", fmt.Errorf("no pinned Firecracker for %s", runtime.GOARCH)
}

// fcFijado es la descarga de la versión fijada para esta máquina.
func fcFijado(dir string) (fcDescarga, error) {
	arch, err := archFC()
	if err != nil {
		return fcDescarga{}, err
	}
	for _, p := range fcPins {
		if p.Version == fcVersion && p.Arch == arch {
			return fcDescarga{
				URL:     "https://github.com/firecracker-microvm/firecracker/releases/download/" + fcVersion + "/firecracker-" + fcVersion + "-" + arch + ".tgz",
				SHA256:  p.SHA256,
				Version: fcVersion, Arch: arch, Dir: dir,
			}, nil
		}
	}
	return fcDescarga{}, fmt.Errorf("no pinned sha256 for Firecracker %s (%s)", fcVersion, arch)
}

// descargarFirecracker baja el tgz con plazo y tope de tamaño, comprueba su
// sha256 ANTES de abrirlo, saca firecracker y jailer a temporales de d.Dir y
// los renombra a su sitio. Solo se miran las dos rutas exactas del tgz
// (release-<v>-<arch>/firecracker-<v>-<arch> y jailer-…): cualquier otra
// entrada, con `../` o no, se ignora, y no se escribe fuera de d.Dir.
func descargarFirecracker(ctx context.Context, d fcDescarga) error {
	if d.MaxBytes == 0 {
		d.MaxBytes = fcMaxTgz
	}
	if d.Plazo == 0 {
		d.Plazo = fcPlazo
	}
	cli := d.Cliente
	if cli == nil {
		cli = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, d.Plazo)
	defer cancel()

	tgz, err := os.CreateTemp(d.Dir, ".fc-*.tgz")
	if err != nil {
		return err
	}
	defer os.Remove(tgz.Name())
	defer tgz.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("downloading Firecracker %s: %w", d.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading Firecracker %s: %s", d.Version, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tgz, h), io.LimitReader(resp.Body, d.MaxBytes+1))
	if err != nil {
		return fmt.Errorf("downloading Firecracker %s: %w", d.Version, err)
	}
	if n > d.MaxBytes {
		return fmt.Errorf("Firecracker %s tgz is too large (more than %d bytes)", d.Version, d.MaxBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != d.SHA256 {
		return fmt.Errorf("sha256 of Firecracker %s (%s) is %s, expected %s: not installing it", d.Version, d.Arch, got, d.SHA256)
	}
	if _, err := tgz.Seek(0, io.SeekStart); err != nil {
		return err
	}

	gz, err := gzip.NewReader(tgz)
	if err != nil {
		return err
	}
	defer gz.Close()
	base := "release-" + d.Version + "-" + d.Arch + "/"
	quiero := map[string]string{
		base + "firecracker-" + d.Version + "-" + d.Arch: "firecracker",
		base + "jailer-" + d.Version + "-" + d.Arch:      "jailer",
	}
	temporales := map[string]string{}
	defer func() {
		for _, t := range temporales {
			os.Remove(t) // los que ya se renombraron no existen: no pasa nada
		}
	}()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return err
		}
		dest, ok := quiero[h.Name]
		if !ok || h.Typeflag != tar.TypeReg || path.Clean(h.Name) != h.Name {
			continue
		}
		f, err := os.CreateTemp(d.Dir, ".fc-bin-*")
		if err != nil {
			return err
		}
		temporales[dest] = f.Name()
		m, err := io.Copy(f, io.LimitReader(tr, fcMaxBinario+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if m > fcMaxBinario {
			return fmt.Errorf("%s in the Firecracker tgz is too large", dest)
		}
		if err := os.Chmod(f.Name(), 0o755); err != nil {
			return err
		}
	}
	for _, dest := range quiero {
		if temporales[dest] == "" {
			return fmt.Errorf("%s not found in the Firecracker %s tgz", dest, d.Version)
		}
	}
	for dest, t := range temporales {
		if err := os.Rename(t, filepath.Join(d.Dir, dest)); err != nil {
			return err
		}
	}
	return nil
}

// ── el host para `up -install` ────────────────────────────────────────────────

// dirBinariosRoot es donde se instalan firecracker y jailer (en el
// secure_path de sudo y fuera del alcance de un usuario).
const dirBinariosRoot = "/usr/local/bin"

// opcionesHostLinux son las piezas de prepararHostLinux, inyectables para
// probarlo sin root ni red.
type opcionesHostLinux struct {
	dryRun     bool
	out        io.Writer
	lookPath   func(string) (string, error)
	userExists func(string) bool
	run        func(name string, args ...string) error
	bajar      func(context.Context) error
	runAs      string
	buildAs    string
}

func hostLinuxReal(dryRun bool) opcionesHostLinux {
	return opcionesHostLinux{
		dryRun: dryRun,
		out:    os.Stdout,
		lookPath: func(n string) (string, error) {
			p := lookFirecracker()
			if n == "jailer" {
				p = lookJailer()
			}
			if p == "" {
				return "", exec.ErrNotFound
			}
			return p, nil
		},
		userExists: func(n string) bool { _, err := user.Lookup(n); return err == nil },
		run: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		},
		bajar: func(ctx context.Context) error {
			d, err := fcFijado(dirBinariosRoot)
			if err != nil {
				return err
			}
			return descargarFirecracker(ctx, d)
		},
		runAs:   envOr("KLING_RUN_AS", runAsDefault),
		buildAs: envOr("KLING_BUILD_AS", buildAsDefault),
	}
}

// prepararHostLinux es lo que `kling up` pedía a mano antes de poder escribir
// la unidad: firecracker y jailer si faltan, y los dos usuarios de sistema.
// Solo corre bajo `up -install`, la orden privilegiada explícita. Cada paso se
// imprime antes de hacerse; con dryRun solo se imprimen.
func prepararHostLinux(ctx context.Context, o opcionesHostLinux) error {
	paso := func(desc string, f func() error) error {
		if o.dryRun {
			fmt.Fprintf(o.out, "would: %s\n", desc)
			return nil
		}
		fmt.Fprintf(o.out, "$ %s\n", desc)
		return f()
	}
	_, e1 := o.lookPath("firecracker")
	_, e2 := o.lookPath("jailer")
	if e1 != nil || e2 != nil {
		desc := fmt.Sprintf("download firecracker and jailer %s into %s (sha256 pinned in kling)", fcVersion, dirBinariosRoot)
		if err := paso(desc, func() error { return o.bajar(ctx) }); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(o.out, "firecracker, jailer: already installed")
	}
	for _, u := range []string{o.runAs, o.buildAs} {
		if o.userExists(u) {
			fmt.Fprintf(o.out, "user %s: already exists\n", u)
			continue
		}
		cmds := [][]string{{"useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", u}}
		if u == o.runAs {
			// El usuario del VMM abre /dev/kvm por el grupo.
			cmds = append(cmds, []string{"usermod", "-aG", "kvm", u})
		}
		for _, c := range cmds {
			if err := paso(strings.Join(c, " "), func() error { return o.run(c[0], c[1:]...) }); err != nil {
				return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
			}
		}
	}
	return nil
}
