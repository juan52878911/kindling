package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// VerifyCache es android-sh --verify-cache sin shell: cada fichero de las
// bibliotecas y binarios de Android, leído por la caché de páginas y con
// O_DIRECT (del disco), tiene que dar lo mismo. En el Mac bajo presión de
// memoria se han visto páginas de la caché a ceros (SIGILL en cada proceso
// nuevo, docs/telefono.md): un dorado guardado así reparte la página rota a
// todos sus clones, por eso kling phone golden lo comprueba antes de guardar.
func (o *androidOps) VerifyCache(ctx context.Context) (verifyResult, error) {
	root := o.s.cfg.Root
	dirs := []string{"system/lib64", "system/bin", "system/framework", "system/apex", "vendor/lib64", "vendor/bin"}
	for i, d := range dirs {
		dirs[i] = filepath.Join(root, d)
	}
	return verifyTrees(ctx, root, dirs)
}

// verifyTrees compara caché y disco de los ficheros regulares de dirs, sin
// salir del sistema de ficheros de cada uno (find -xdev). Las rutas de los que
// no casan salen relativas a root.
func verifyTrees(ctx context.Context, root string, dirs []string) (verifyResult, error) {
	t0 := time.Now()
	res := verifyResult{}
	buf, err := syscall.Mmap(-1, 0, 1<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return res, err
	}
	defer func() { _ = syscall.Munmap(buf) }()
	for _, d := range dirs {
		var st syscall.Stat_t
		if syscall.Stat(d, &st) != nil {
			continue
		}
		dev := st.Dev
		err := filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var s syscall.Stat_t
			if syscall.Lstat(p, &s) != nil || s.Dev != dev {
				if e.IsDir() && p != d {
					return filepath.SkipDir
				}
				return nil
			}
			if !e.Type().IsRegular() || s.Size == 0 {
				return nil
			}
			a, n, err1 := sumCached(p)
			b, _, err2 := sumDirect(p, buf)
			if err1 != nil || err2 != nil {
				res.Errors++
				return nil
			}
			res.Files++
			res.Bytes += n
			if a != b {
				rel, _ := filepath.Rel(root, p)
				res.Mismatches = append(res.Mismatches, "/"+rel)
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	res.Seconds = time.Since(t0).Seconds()
	res.OK = res.Files > 0 && len(res.Mismatches) == 0
	return res, nil
}

func sumCached(p string) ([32]byte, int64, error) {
	var out [32]byte
	f, err := os.Open(p)
	if err != nil {
		return out, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	copy(out[:], h.Sum(nil))
	return out, n, err
}

// sumDirect lee con O_DIRECT (buf alineado a página: mmap anónimo).
func sumDirect(p string, buf []byte) ([32]byte, int64, error) {
	var out [32]byte
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_DIRECT|syscall.O_CLOEXEC, 0)
	if err != nil {
		return out, 0, err
	}
	defer syscall.Close(fd)
	h := sha256.New()
	var total int64
	for {
		k, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return out, total, err
		}
		if k <= 0 {
			break
		}
		h.Write(buf[:k])
		total += int64(k)
	}
	copy(out[:], h.Sum(nil))
	return out, total, nil
}

// kernelRelease: uname -r del invitado (el kernel Android del daemon).
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
