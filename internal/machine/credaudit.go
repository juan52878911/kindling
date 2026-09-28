package machine

// Lectura del registro de auditoría del proxy de credenciales
// (machines/<id>/credaudit.jsonl, ver pkg/credproxy/auditoria.go).
//
// Lo escribe quien sirve el proxy: en Linux el propio daemon, en macOS el
// kling-vz de la máquina. Aquí solo se lee, y por eso funciona igual con la
// máquina corriendo, congelada o parada: es un fichero de su directorio. Ni
// Commit ni Fork lo copian (copian ficheros concretos del snapshot, no el
// directorio), así que una máquina nueva empieza con el suyo vacío; Remove lo
// borra con el resto del directorio.
//
// En Linux ese directorio es del usuario sin privilegios del VMM, y quien lee
// es root: se abre sin seguir enlaces y solo si es un fichero regular, para
// que un enlace plantado no convierta esta lectura en la de otro fichero.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// credAuditMaxBytes es cuánto lee CredAudit como mucho entre el fichero y su
// rotación (.1). Cada uno se rota a credproxy.AuditMaxBytes, así que en la
// práctica cabe todo.
const credAuditMaxBytes = 4 << 20

// CredAudit devuelve las líneas del registro de auditoría del proxy de
// credenciales de ref que pasan el filtro q, las más antiguas primero. Sin
// registro (la máquina nunca tuvo credenciales) devuelve una lista vacía.
func (m *Manager) CredAudit(ref string, q api.CredAuditQuery) ([]api.CredAuditRecord, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, &api.StatusError{Code: 404, Message: fmt.Sprintf("machine %q does not exist", ref)}
	}
	path := m.credAuditPath(mc.ID)
	actual, err := leerAuditoria(path, credAuditMaxBytes)
	if err != nil {
		return nil, err
	}
	var viejo []byte
	if resto := credAuditMaxBytes - int64(len(actual)); resto > 0 {
		if viejo, err = leerAuditoria(path+".1", resto); err != nil {
			return nil, err
		}
	}

	var out []api.CredAuditRecord
	for _, datos := range [][]byte{viejo, actual} {
		sc := bufio.NewScanner(bytes.NewReader(datos))
		sc.Buffer(make([]byte, 0, 64<<10), credproxy.AuditMaxBytes)
		for sc.Scan() {
			var r api.CredAuditRecord
			// Una línea a medias (el escritor murió a mitad) o ajena no para
			// la lectura: se salta.
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			if !q.Since.IsZero() && r.TS.Before(q.Since) {
				continue
			}
			// Las líneas de descartados salen siempre (también con Denied):
			// una pérdida no debe esconderse detrás de un filtro.
			if q.Denied && !r.Denied && r.Kind != credproxy.KindDropped {
				continue
			}
			out = append(out, r)
		}
	}
	if q.Tail > 0 && len(out) > q.Tail {
		out = out[len(out)-q.Tail:]
	}
	return out, nil
}

// leerAuditoria lee como mucho max bytes del final de path, empezando en una
// línea entera. Que no exista no es un error: no hay nada que leer.
func leerAuditoria(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the credential audit log: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("reading the credential audit log: %s is not a regular file", path)
	}
	b, err := leerColaDe(f, max)
	if err != nil {
		return nil, err
	}
	// Si se leyó solo la cola, la primera línea puede estar cortada.
	if fi.Size() > max {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
	}
	return b, nil
}
