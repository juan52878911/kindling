package deb

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// RESOLVER DEPENDENCIAS, como apt-get install --no-install-recommends pero
// sin apt: Depends y Pre-Depends, la primera alternativa que exista, y lo que
// ya está instalado (o lo provee) no se vuelve a añadir. No mira versiones:
// sirve porque el índice y lo instalado salen del mismo día (un snapshot).

// Package es un paquete de un índice (Packages).
type Package struct {
	Name, Version string
	Filename      string // ruta dentro del archivo (pool/...)
	SHA256        string
	Size          int64
	Base          string // URL del archivo del que salió (sin / final)
	depends       string // Pre-Depends y Depends, con coma
}

// Index son los paquetes disponibles de uno o varios índices.
type Index struct {
	avail    map[string]*Package
	provides map[string]string // paquete virtual -> el primero que lo provee
}

// NewIndex devuelve un índice vacío.
func NewIndex() *Index {
	return &Index{avail: map[string]*Package{}, provides: map[string]string{}}
}

// Len es cuántos paquetes tiene.
func (ix *Index) Len() int { return len(ix.avail) }

// Add lee un fichero Packages (ya descomprimido) del archivo base. De un
// paquete que aparece en varios índices se queda la versión más alta.
func (ix *Index) Add(r io.Reader, base string) error {
	ps, err := ParseParagraphs(r)
	if err != nil {
		return err
	}
	for _, p := range ps {
		name, ver := p.Get("Package"), p.Get("Version")
		if name == "" {
			continue
		}
		if old, ok := ix.avail[name]; ok && CompareVersions(old.Version, ver) >= 0 {
			continue
		}
		size, _ := strconv.ParseInt(p.Get("Size"), 10, 64)
		dep := p.Get("Pre-Depends")
		if d := p.Get("Depends"); d != "" {
			if dep != "" {
				dep += ", "
			}
			dep += d
		}
		ix.avail[name] = &Package{Name: name, Version: ver, Filename: p.Get("Filename"),
			SHA256: p.Get("SHA256"), Size: size, Base: strings.TrimSuffix(base, "/"), depends: dep}
		for _, grp := range ParseDepends(p.Get("Provides")) {
			for _, rel := range grp {
				if _, ok := ix.provides[rel.Name]; !ok {
					ix.provides[rel.Name] = name
				}
			}
		}
	}
	return nil
}

// Installed lee un /var/lib/dpkg/status: nombre (y lo que provee) ->
// versión de lo que está instalado.
func Installed(status io.Reader) (map[string]string, error) {
	ps, err := ParseParagraphs(status)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range ps {
		if !strings.Contains(p.Get("Status"), "installed") || strings.Contains(p.Get("Status"), "not-installed") {
			continue
		}
		out[p.Get("Package")] = p.Get("Version")
		for _, grp := range ParseDepends(p.Get("Provides")) {
			for _, r := range grp {
				out[r.Name] = p.Get("Version")
			}
		}
	}
	return out, nil
}

// Resolve cierra las dependencias de want sobre lo instalado y devuelve lo
// que hay que añadir, ordenado por nombre.
func (ix *Index) Resolve(want []string, installed map[string]string) ([]*Package, error) {
	chosen := map[string]*Package{}
	var order []*Package
	satisfied := func(grp []Relation) bool {
		for _, r := range grp {
			if _, ok := installed[r.Name]; ok {
				return true
			}
			if _, ok := chosen[r.Name]; ok {
				return true
			}
			if real, ok := ix.provides[r.Name]; ok && chosen[real] != nil {
				return true
			}
		}
		return false
	}
	var add func(name, from string, depth int) error
	add = func(name, from string, depth int) error {
		if depth > 64 {
			return fmt.Errorf("%s: dependency chain too deep", name)
		}
		if _, ok := installed[name]; ok {
			return nil
		}
		if _, ok := chosen[name]; ok {
			return nil
		}
		p := ix.avail[name]
		if p == nil {
			if real, ok := ix.provides[name]; ok {
				return add(real, from, depth+1)
			}
			return fmt.Errorf("%s (needed by %s) is not in the archive", name, from)
		}
		chosen[name] = p
		for _, grp := range ParseDepends(p.depends) {
			if satisfied(grp) {
				continue
			}
			pick := ""
			for _, r := range grp {
				if ix.avail[r.Name] != nil || ix.provides[r.Name] != "" {
					pick = r.Name
					break
				}
			}
			if pick == "" {
				return fmt.Errorf("%s: no candidate for %v", name, grp)
			}
			if err := add(pick, name, depth+1); err != nil {
				return err
			}
		}
		order = append(order, p)
		return nil
	}
	for _, w := range want {
		if err := add(w, "request", 0); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Name < order[j].Name })
	return order, nil
}
