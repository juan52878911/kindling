package main

// -share SRC:DST[:copy|ro|rw] en `kling run` y `kling sandbox create`, y
// `kling inspect`. Ver docs/compartir.md.

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/share"
)

// shareFlag acumula -share repetidos. Se validan al parsear: un error de
// sintaxis no debe esperar a que se haya subido medio repositorio.
type shareFlag []api.ShareSpec

func (s *shareFlag) String() string { return "" }

func (s *shareFlag) Set(v string) error {
	spec, err := api.ParseShare(v)
	if err != nil {
		return err
	}
	*s = append(*s, spec)
	return nil
}

const shareUsage = "host folder inside the machine: SRC:DST[:copy|ro|rw] (repeatable). " +
	"copy (default) uploads a read-only snapshot of a local folder; ro/rw serve a folder on the daemon host live"

// remoteEndpoint dice si el daemon está al otro lado de SSH: entonces las rutas
// locales no significan nada allí.
func remoteEndpoint(endpoint string) bool { return strings.HasPrefix(endpoint, "ssh://") }

// prepareShares sube las copias y resuelve las rutas de las vivas. Devuelve lo
// que va en RunRequest.Shares.
func prepareShares(ctx context.Context, c *api.Client, endpoint string, specs shareFlag) ([]api.ShareSpec, error) {
	out := make([]api.ShareSpec, 0, len(specs))
	for _, s := range specs {
		switch s.Mode {
		case share.ModeCopy:
			up, err := uploadDir(ctx, c, s.Source)
			if err != nil {
				return nil, fmt.Errorf("share %s: %w", s.Source, err)
			}
			fmt.Fprintf(os.Stderr, "share %s -> %s: copied %d files, %s\n", s.Source, s.Mount, up.Files, human(up.Bytes))
			abs, _ := filepath.Abs(s.Source)
			s.Upload, s.Source = up.ID, abs
		default:
			// La ruta es del host del DAEMON. Con un daemon local, una relativa
			// se resuelve aquí; con uno remoto no se puede adivinar.
			if !filepath.IsAbs(s.Source) {
				if remoteEndpoint(endpoint) {
					return nil, fmt.Errorf("share %s: a live share (%s) uses a folder on the daemon host; pass its absolute path there", s.Source, s.Mode)
				}
				abs, err := filepath.Abs(s.Source)
				if err != nil {
					return nil, err
				}
				s.Source = abs
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// uploadDir empaqueta dir en un tar y lo sube mientras lo recorre.
func uploadDir(ctx context.Context, c *api.Client, dir string) (*api.ShareUpload, error) {
	// Si la carpeta es un enlace, se sube a lo que apunta: el recorrido no
	// entra en enlaces, y sin esto subiría un árbol vacío.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	pr, pw := io.Pipe()
	skipped := make(chan []string, 1)
	go func() {
		sk, err := writeTar(pw, dir)
		skipped <- sk
		pw.CloseWithError(err)
	}()
	up, err := c.UploadShare(ctx, pr)
	// Si la subida falla, el escritor se desbloquea con el error y termina.
	pr.CloseWithError(errors.New("upload finished"))
	sk := <-skipped
	for i, s := range sk {
		if i == 10 {
			fmt.Fprintf(os.Stderr, "  ... and %d more skipped\n", len(sk)-10)
			break
		}
		fmt.Fprintf(os.Stderr, "  skipped %s\n", s)
	}
	return up, err
}

// writeTar escribe dir como tar en w con lo que el daemon acepta: directorios,
// ficheros regulares (los enlaces duros como copias) y enlaces relativos que no
// salen del árbol. Lo demás se salta y se devuelve para avisar.
func writeTar(w io.Writer, dir string) ([]string, error) {
	var skipped []string
	// Los enlaces primero, para saber cuáles atraviesan otros.
	links := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(dir, p)
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			links[filepath.ToSlash(rel)] = t
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	isLink := func(p string) bool { _, ok := links[p]; return ok }

	tw := tar.NewWriter(w)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := int64(info.Mode().Perm())
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: perm, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: perm,
				Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			if _, err := io.CopyN(tw, f, info.Size()); err != nil {
				return fmt.Errorf("%s changed while it was being copied: %w", p, err)
			}
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			t := links[name]
			if share.CheckLink(name, t) != nil || share.LinkTraverses(name, t, isLink) {
				skipped = append(skipped, name+" (symlink pointing outside: "+t+")")
				return nil
			}
			return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: t, Mode: 0o777, ModTime: info.ModTime()})
		default:
			skipped = append(skipped, name+" (not a regular file, directory or symlink)")
			return nil
		}
	})
	if err != nil {
		return skipped, err
	}
	return skipped, tw.Close()
}

// cmdInspect es `kling inspect <ref>`: la máquina entera en JSON, con el estado
// de sus carpetas compartidas.
func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling inspect <ref>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	mc, err := api.NewClient(hostOf(*host)).Get(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(mc)
}

// sharesColumn resume las carpetas de una máquina para `kling ps`.
func sharesColumn(mc *api.Machine) string {
	if len(mc.Shares) == 0 {
		return "-"
	}
	var parts []string
	for _, s := range mc.Shares {
		p := s.Mount + ":" + s.Mode
		if s.Live() && s.Status != "" && s.Status != "attached" && mc.State == api.StateRunning {
			p += "(" + strings.SplitN(s.Status, ":", 2)[0] + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}
