package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// escribirTarDe escribe en w el contenido de dir como tar (sin el propio dir).
// Los enlaces van como enlaces; lo que no es fichero, directorio o enlace
// (sockets, FIFO, dispositivos) se salta con un aviso.
func escribirTarDe(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		case !info.Mode().IsRegular() && !info.IsDir():
			fmt.Fprintf(os.Stderr, "kling: skipping %s (not a file, directory or symlink)\n", p)
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, io.LimitReader(f, h.Size))
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// extraerTarLocal extrae r en dst (que se crea). El tar viene de la máquina,
// que no es de fiar: guest.ExtractTar rechaza rutas que salen, enlaces que
// escapan y topes de tamaño.
func extraerTarLocal(r io.Reader, dst string) (api.TarResult, error) {
	dst = strings.TrimRight(dst, "/")
	if dst == "" {
		dst = "/"
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return api.TarResult{}, err
	}
	return guest.ExtractTar(r, dst, api.FileMaxDownload, api.FileTarMaxEntries)
}

// copiarDirAMaquina sube el contenido de src a dstPath (se crea si falta).
func copiarDirAMaquina(ctx context.Context, c *api.Client, src, ref, dstPath string, asJSON bool) error {
	if dstPath == "" {
		return errors.New("missing destination directory: <machine>:<dir>")
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(escribirTarDe(pw, src)) }()
	res, err := c.WriteTar(ctx, ref, dstPath, true, pr)
	pr.CloseWithError(err) // si el servidor cortó, que el escritor pare
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Fprintf(os.Stderr, "%s:%s  %d files, %d dirs, %d symlinks, %s\n", ref, dstPath, res.Files, res.Dirs, res.Symlinks, human(res.Bytes))
	return nil
}

// copiarDirDeMaquina baja el directorio srcPath de la máquina a dst (se crea).
func copiarDirDeMaquina(ctx context.Context, c *api.Client, ref, srcPath, dst string, asJSON bool) error {
	if dst == "-" {
		return errors.New("a directory can't go to stdout: give a local directory")
	}
	rc, err := c.ReadTar(ctx, ref, srcPath)
	if err != nil {
		return err
	}
	defer rc.Close()
	res, err := extraerTarLocal(rc, dst)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Fprintf(os.Stderr, "%s  %d files, %d dirs, %d symlinks, %s\n", dst, res.Files, res.Dirs, res.Symlinks, human(res.Bytes))
	return nil
}
