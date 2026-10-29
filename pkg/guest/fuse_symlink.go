package guest

// SYMLINK: el daemon decide si el enlace vale (internal/share/symlink.go). El
// agente solo traduce un destino absoluto que apunta dentro del propio
// montaje (/app/node_modules/x desde /app/node_modules/.bin) a uno relativo,
// que es lo único que el host acepta; uno absoluto fuera del montaje
// (/usr/bin/python3) no tiene sentido en el host y da EPERM.

import (
	"path"
	"strings"

	"github.com/juan52878911/kindling/pkg/share"
)

func (fs *fuseFS) symlink(u uint64, parent *node, a []byte) []byte {
	name, rest, err := cstring(a)
	if err != nil || share.ValidName(name) != nil {
		return reply(u, nameErr(name, err))
	}
	target, _, err := cstring(rest)
	if err != nil || target == "" || len(target) >= share.MaxPath {
		return reply(u, share.EINVAL)
	}
	if fs.readOnly {
		return reply(u, share.EROFS)
	}
	p, e := fs.childPath(parent, name)
	if e != 0 {
		return reply(u, e)
	}
	if strings.HasPrefix(target, "/") {
		rel, ok := relativeTarget(fs.mount, p, target)
		if !ok {
			return reply(u, share.EPERM)
		}
		target = rel
	}
	errno, d := fs.call(share.OpSymlink, func(e *share.Enc) { e.Str(p); e.Str(target) })
	if errno == share.ENOSYS {
		errno = share.EPERM // un daemon anterior: como antes
	}
	if errno != 0 {
		return reply(u, errno)
	}
	at := share.GetAttr(d)
	if d.Err() != nil {
		return reply(u, share.EIO)
	}
	c := fs.nodes.lookup(parent, name)
	c.typ.Store(at.Mode & share.SIFMT)
	fs.linkChanged(c, at) // la versión de la que el kernel guardará el destino
	return reply(u, 0, entryOut(c.id, toFattr(at, c.id)))
}

// relativeTarget pasa el destino absoluto target de un enlace en p (ruta del
// protocolo) a relativo, si cae dentro del montaje mount.
func relativeTarget(mount, p, target string) (string, bool) {
	t := path.Clean(target)
	if t != mount && !strings.HasPrefix(t, mount+"/") {
		return "", false
	}
	t = strings.TrimPrefix(strings.TrimPrefix(t, mount), "/")
	from := path.Dir(p)
	if from == "." {
		from = ""
	}
	var up []string
	fp := strings.Split(from, "/")
	tp := strings.Split(t, "/")
	if from == "" {
		fp = nil
	}
	if t == "" {
		tp = nil
	}
	i := 0
	for i < len(fp) && i < len(tp) && fp[i] == tp[i] {
		i++
	}
	for range fp[i:] {
		up = append(up, "..")
	}
	r := strings.Join(append(up, tp[i:]...), "/")
	if r == "" {
		r = "."
	}
	return r, true
}
