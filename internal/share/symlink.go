package share

// Enlaces simbólicos creados por el invitado en una carpeta rw (OpSymlink).
//
// Hasta ahora se rechazaban todos (EPERM): un enlace creado por el invitado es
// un arma contra lo que el host haga luego con el directorio. Pero sin ellos
// `npm install` falla en la carpeta (node_modules/.bin son enlaces), y eso
// deja la carpeta viva fuera del bucle de desarrollo de casi cualquier
// proyecto de Node. Se aceptan los que no pueden sacar a nadie de la carpeta:
//
//   - destino relativo (uno absoluto nombra el árbol del invitado, no este);
//   - resuelto léxicamente desde donde está el enlace, dentro de la carpeta
//     (proto.CheckLink, la misma regla que los enlaces de una copia);
//   - sin atravesar otro enlace por el camino (proto.LinkTraverses), porque
//     entonces lo léxico y lo real pueden no coincidir.
//
// Un enlace válido podría dejar de serlo si se mueve a un sitio menos profundo
// ("a/b/l -> ../../x" movido a "l" apunta fuera). Por eso rename revalida: un
// enlace movido se comprueba en su sitio nuevo, y un directorio que sube de
// nivel se recorre (hasta maxRenameScan entradas) buscando enlaces que dejen
// de valer. Si alguno no vale, o el directorio es demasiado grande para
// mirarlo, EXDEV: mv y las bibliotecas de copia caen entonces a copiar y
// borrar, que vuelve a crear cada enlace por OpSymlink y su comprobación.

import (
	"errors"
	"io"
	"io/fs"
	"strings"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// maxRenameScan acota lo que se recorre al subir un directorio de nivel.
var maxRenameScan = 20000

func (s *Server) isLink(p string) bool {
	fi, err := s.root.Lstat(rel(p))
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// linkOK dice si un enlace en p con destino t cumple las reglas.
func (s *Server) linkOK(p, t string) bool {
	return proto.CheckLink(p, t) == nil && !proto.LinkTraverses(p, t, s.isLink)
}

func (s *Server) symlink(p, target string) (proto.Attr, uint32) {
	if s.readOnly {
		return proto.Attr{}, proto.EROFS
	}
	if p == "" {
		return proto.Attr{}, proto.EEXIST
	}
	if !s.linkOK(p, target) {
		return proto.Attr{}, proto.EPERM
	}
	dir, base, e := s.parent(p)
	if e != 0 {
		return proto.Attr{}, e
	}
	defer dir.Close()
	if err := symlinkat(target, dir, base); err != nil {
		return proto.Attr{}, Errno(err)
	}
	return s.stat(p)
}

// parentDepth es la profundidad del directorio que contiene p ("a" está en la
// raíz: 0).
func parentDepth(p string) int { return strings.Count(p, "/") }

// renameKeepsLinks dice si mover oldp a newp deja válidos los enlaces que se
// mueven con él. Ver el comentario del fichero.
func (s *Server) renameKeepsLinks(oldp, newp string) bool {
	fi, err := s.root.Lstat(rel(oldp))
	if err != nil {
		return true // que rename dé su propio error
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		dir, base, e := s.parent(oldp)
		if e != 0 {
			return true
		}
		t, err := readlinkat(dir, base)
		dir.Close()
		if err != nil || strings.HasPrefix(t, "/") {
			return true // los absolutos del host se leen traducidos; no los creó el invitado
		}
		return s.linkOK(newp, t)
	case fi.IsDir() && parentDepth(newp) < parentDepth(oldp):
		left := maxRenameScan
		return s.scanLinks(oldp, newp, &left)
	}
	return true
}

// scanLinks recorre el directorio from (que pasaría a llamarse to) y dice si
// todos sus enlaces relativos siguen dentro de la carpeta en el sitio nuevo.
func (s *Server) scanLinks(from, to string, left *int) bool {
	f, err := s.root.Open(rel(from))
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		ents, err := f.ReadDir(256)
		for _, e := range ents {
			if *left--; *left < 0 {
				return false
			}
			name := e.Name()
			switch {
			case e.Type()&fs.ModeSymlink != 0:
				t, err := readlinkat(f, name)
				if err != nil {
					return false
				}
				if !strings.HasPrefix(t, "/") && proto.CheckLink(proto.Join(to, name), t) != nil {
					return false
				}
			case e.IsDir():
				if !s.scanLinks(proto.Join(from, name), proto.Join(to, name), left) {
					return false
				}
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(ents) == 0) {
			return true
		}
		if err != nil {
			return false
		}
	}
}
