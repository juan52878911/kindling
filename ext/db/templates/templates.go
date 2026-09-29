// Package templates trae las plantillas de base de datos que `kling db golden
// build -template` construye de un comando. Van embebidas en el binario:
//
//	<nombre>/migrations/*.sql   en orden alfabético, como el rol de la aplicación
//	<nombre>/seed.sql           datos, después de las migraciones
//	<nombre>/README.md          la primera línea "# nombre" y su resumen
//
// Se escriben a un directorio temporal 0700 solo al construir: db-golden.sh
// las lee del disco (-migrations DIR, -seed FILE).
package templates

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed empty crm-demo
var files embed.FS

// namePattern es un nombre de plantilla incluida: sin barras ni puntos.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

// Info describe una plantilla.
type Info struct {
	Name    string
	Summary string
}

// List devuelve las plantillas incluidas, por nombre.
func List() ([]Info, error) {
	ents, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !e.IsDir() || !namePattern.MatchString(e.Name()) {
			continue
		}
		out = append(out, Info{Name: e.Name(), Summary: summary(e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// summary es la primera línea de texto de README.md tras el título.
func summary(name string) string {
	b, err := fs.ReadFile(files, path.Join(name, "README.md"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "kling ") {
			continue
		}
		return l
	}
	return ""
}

// Dir es una plantilla escrita en disco.
type Dir struct {
	Root       string // directorio temporal 0700
	Migrations string // Root/migrations
	Seed       string // Root/seed.sql
}

// Cleanup borra el directorio temporal.
func (d *Dir) Cleanup() { _ = os.RemoveAll(d.Root) }

// Materialize escribe la plantilla name en un directorio temporal nuevo (0700,
// ficheros 0600). El nombre solo se acepta si es una plantilla incluida: nunca
// entra en una ruta del host.
func Materialize(name string) (*Dir, error) {
	list, err := List()
	if err != nil {
		return nil, err
	}
	found := false
	var names []string
	for _, t := range list {
		names = append(names, t.Name)
		if t.Name == name {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown template %q (available: %s; see kling db templates)", name, strings.Join(names, ", "))
	}
	root, err := os.MkdirTemp("", "kling-db-template-*")
	if err != nil {
		return nil, err
	}
	d := &Dir{Root: root, Migrations: filepath.Join(root, "migrations"), Seed: filepath.Join(root, "seed.sql")}
	fail := func(err error) (*Dir, error) { d.Cleanup(); return nil, err }
	if err := os.Chmod(root, 0o700); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(d.Migrations, 0o700); err != nil {
		return fail(err)
	}
	werr := fs.WalkDir(files, name, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, name+"/")
		if rel == "README.md" {
			return nil
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), b, 0o600)
	})
	if werr != nil {
		return fail(werr)
	}
	if ents, _ := os.ReadDir(d.Migrations); len(ents) == 0 {
		return fail(errors.New("template has no migrations"))
	}
	if _, err := os.Stat(d.Seed); err != nil {
		return fail(err)
	}
	return d, nil
}
