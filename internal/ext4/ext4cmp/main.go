// ext4cmp compara dos imágenes ext4 fichero a fichero sin montarlas: tipo,
// modo (con setuid), dueño, tamaño, contenido (sha256), destino de los
// enlaces, dispositivos, xattrs y, con -mtime, la hora de modificación.
//
//	go run ./internal/ext4/ext4cmp [-sub /upper/android] [-mtime] A.ext4 B.ext4
//
// Sirve para comprobar que la imagen del constructor android es la misma que
// la de build-image.sh. Sale con 1 si hay diferencias.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/ext4"
)

func main() {
	sub := flag.String("sub", "/", "compare only this subtree")
	mtime := flag.Bool("mtime", false, "also compare modification times")
	max := flag.Int("max", 50, "print at most this many differences")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: ext4cmp [-sub PATH] [-mtime] A.ext4 B.ext4")
		os.Exit(2)
	}
	a, err := load(flag.Arg(0), *sub)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	b, err := load(flag.Arg(1), *sub)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var paths []string
	for p := range a {
		paths = append(paths, p)
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	diffs, same := 0, 0
	for _, p := range paths {
		x, y := a[p], b[p]
		var d string
		switch {
		case x == nil:
			d = "only in B"
		case y == nil:
			d = "only in A"
		default:
			d = compare(x, y, *mtime)
		}
		if d == "" {
			same++
			continue
		}
		diffs++
		if diffs <= *max {
			fmt.Printf("%s: %s\n", p, d)
		}
	}
	fmt.Printf("%d paths identical, %d differ (A: %d, B: %d)\n", same, diffs, len(a), len(b))
	if diffs > 0 {
		os.Exit(1)
	}
}

type entry struct {
	n   *ext4.Node
	sum string
}

func load(img, sub string) (map[string]*entry, error) {
	f, err := os.Open(img)
	if err != nil {
		return nil, err
	}
	root, err := ext4.Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", img, err)
	}
	top := root.Lookup(sub)
	if top == nil {
		return nil, fmt.Errorf("%s: no %s", img, sub)
	}
	out := map[string]*entry{}
	err = top.Walk(func(p string, n *ext4.Node) error {
		e := &entry{n: n}
		if n.IsReg() {
			r, err := n.Open()
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			h := sha256.New()
			_, err = io.Copy(h, r)
			r.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			e.sum = fmt.Sprintf("%x", h.Sum(nil))
		}
		out[p] = e
		return nil
	})
	return out, err
}

func compare(x, y *entry, mtime bool) string {
	var d []string
	a, b := x.n, y.n
	if a.Mode != b.Mode {
		d = append(d, fmt.Sprintf("mode %o vs %o", a.Mode, b.Mode))
	}
	if a.UID != b.UID || a.GID != b.GID {
		d = append(d, fmt.Sprintf("owner %d:%d vs %d:%d", a.UID, a.GID, b.UID, b.GID))
	}
	if a.IsReg() && (a.Size != b.Size || x.sum != y.sum) {
		d = append(d, fmt.Sprintf("content %d bytes %.12s vs %d bytes %.12s", a.Size, x.sum, b.Size, y.sum))
	}
	if a.Target != b.Target {
		d = append(d, fmt.Sprintf("target %q vs %q", a.Target, b.Target))
	}
	if a.Major != b.Major || a.Minor != b.Minor {
		d = append(d, fmt.Sprintf("device %d:%d vs %d:%d", a.Major, a.Minor, b.Major, b.Minor))
	}
	if xa, xb := xattrs(a), xattrs(b); xa != xb {
		d = append(d, fmt.Sprintf("xattrs {%s} vs {%s}", xa, xb))
	}
	if mtime && !a.Mtime.Equal(b.Mtime) {
		d = append(d, fmt.Sprintf("mtime %s vs %s", a.Mtime.UTC().Format("2006-01-02T15:04:05.999999999"), b.Mtime.UTC().Format("2006-01-02T15:04:05.999999999")))
	}
	return strings.Join(d, "; ")
}

func xattrs(n *ext4.Node) string {
	var ks []string
	for k, v := range n.Xattrs {
		ks = append(ks, fmt.Sprintf("%s=%x", k, v))
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}
