// Package deb lee paquetes .deb de Debian sin dpkg: el archivo ar, el
// control (control.tar.xz o .gz) y los datos (data.tar.xz o .gz), y parsea
// los párrafos de control (Packages, status).
package deb

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/internal/xz"
)

// Deb es un .deb abierto.
type Deb struct {
	Path    string
	members map[string][2]int64 // nombre -> desplazamiento, tamaño
}

// Open lee el índice del archivo ar.
func Open(path string) (*Deb, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, 8)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != "!<arch>\n" {
		return nil, fmt.Errorf("%s: not a .deb (ar) archive", path)
	}
	d := &Deb{Path: path, members: map[string][2]int64{}}
	off := int64(8)
	h := make([]byte, 60)
	for {
		if _, err := f.ReadAt(h, off); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if string(h[58:60]) != "`\n" {
			return nil, fmt.Errorf("%s: corrupt ar header", path)
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(h[:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%s: corrupt ar size", path)
		}
		d.members[name] = [2]int64{off + 60, size}
		off += 60 + size + size%2
	}
	if _, ok := d.members["debian-binary"]; !ok {
		return nil, fmt.Errorf("%s: no debian-binary member", path)
	}
	return d, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// member abre el tar descomprimido de control.tar.* o data.tar.*.
func (d *Deb) member(prefix string) (io.ReadCloser, error) {
	for _, ext := range []string{".xz", ".gz", ""} {
		m, ok := d.members[prefix+ext]
		if !ok {
			continue
		}
		f, err := os.Open(d.Path)
		if err != nil {
			return nil, err
		}
		sr := bufio.NewReaderSize(io.NewSectionReader(f, m[0], m[1]), 1<<16)
		switch ext {
		case ".xz":
			r, err := xz.NewReader(sr)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("%s %s: %w", d.Path, prefix+ext, err)
			}
			return readCloser{r, f.Close}, nil
		case ".gz":
			r, err := gzip.NewReader(sr)
			if err != nil {
				f.Close()
				return nil, err
			}
			return readCloser{r, f.Close}, nil
		default:
			return readCloser{sr, f.Close}, nil
		}
	}
	for n := range d.members {
		if strings.HasPrefix(n, prefix) {
			return nil, fmt.Errorf("%s: %s is compressed in a format this reader does not know", d.Path, n)
		}
	}
	return nil, fmt.Errorf("%s: no %s", d.Path, prefix)
}

// Data abre data.tar descomprimido.
func (d *Deb) Data() (io.ReadCloser, error) { return d.member("data.tar") }

// Control devuelve los ficheros de control.tar (control, md5sums,
// conffiles, postinst...), por nombre.
func (d *Deb) Control() (map[string][]byte, error) {
	rc, err := d.member("control.tar")
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(h.Name, "./")
		if strings.Contains(name, "/") || h.Size > 16<<20 {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
}

// Paragraph es un párrafo de control, con sus campos en orden.
type Paragraph struct {
	Keys   []string
	Values map[string]string
}

// Get devuelve un campo (sin distinguir mayúsculas).
func (p Paragraph) Get(k string) string {
	for _, kk := range p.Keys {
		if strings.EqualFold(kk, k) {
			return p.Values[kk]
		}
	}
	return ""
}

// String vuelve a escribir el párrafo.
func (p Paragraph) String() string {
	var b strings.Builder
	for _, k := range p.Keys {
		v := p.Values[k]
		if strings.HasPrefix(v, "\n") {
			fmt.Fprintf(&b, "%s:%s\n", k, v)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	return b.String()
}

// ParseParagraphs lee párrafos separados por líneas en blanco (un Packages,
// un status, un control).
func ParseParagraphs(r io.Reader) ([]Paragraph, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var out []Paragraph
	cur := Paragraph{Values: map[string]string{}}
	last := ""
	flush := func() {
		if len(cur.Keys) > 0 {
			out = append(out, cur)
		}
		cur = Paragraph{Values: map[string]string{}}
		last = ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if last == "" {
				return nil, fmt.Errorf("continuation line without a field: %q", line)
			}
			cur.Values[last] += "\n" + line
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("bad control line %q", line)
		}
		k = strings.TrimSpace(k)
		cur.Keys = append(cur.Keys, k)
		cur.Values[k] = strings.TrimSpace(v)
		last = k
	}
	flush()
	return out, sc.Err()
}

// ParseParagraph lee un solo párrafo (el control de un paquete).
func ParseParagraph(b []byte) (Paragraph, error) {
	ps, err := ParseParagraphs(bytes.NewReader(b))
	if err != nil {
		return Paragraph{}, err
	}
	if len(ps) != 1 {
		return Paragraph{}, fmt.Errorf("expected one control paragraph, got %d", len(ps))
	}
	return ps[0], nil
}

// Relation es una alternativa de Depends: "libc6 (>= 2.38)".
type Relation struct{ Name, Op, Version, Arch string }

// ParseDepends parte un campo Depends en grupos de alternativas.
func ParseDepends(s string) [][]Relation {
	var out [][]Relation
	for _, grp := range strings.Split(s, ",") {
		grp = strings.TrimSpace(grp)
		if grp == "" {
			continue
		}
		var alts []Relation
		for _, a := range strings.Split(grp, "|") {
			a = strings.TrimSpace(a)
			var r Relation
			if i := strings.Index(a, "("); i >= 0 {
				rel := strings.TrimSuffix(strings.TrimSpace(a[i+1:]), ")")
				a = strings.TrimSpace(a[:i])
				for _, op := range []string{">=", "<=", ">>", "<<", "="} {
					if strings.HasPrefix(rel, op) {
						r.Op, r.Version = op, strings.TrimSpace(rel[len(op):])
						break
					}
				}
			}
			if i := strings.Index(a, "["); i >= 0 {
				a = strings.TrimSpace(a[:i])
			}
			name, arch, _ := strings.Cut(a, ":")
			r.Name, r.Arch = name, arch
			alts = append(alts, r)
		}
		out = append(out, alts)
	}
	return out
}
