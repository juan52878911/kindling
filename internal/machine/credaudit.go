package machine

// Lectura del registro de auditoría del proxy de credenciales (ver
// pkg/credproxy/auditoria.go), y dónde vive.
//
// Lo escribe quien sirve el proxy, y eso decide el sitio:
//
//   - En Linux, el propio daemon (root), en <root>/audit/<id>.jsonl: un
//     directorio 0700 de root, fuera del directorio de la máquina. Ese
//     directorio es del usuario sin privilegios del VMM, y un VMM comprometido
//     podía borrar, truncar o sustituir el registro que lo vigila (o plantar
//     un enlace para llevar la escritura de root a otro fichero). Fuera de su
//     alcance, lo que queda escrito lo ha escrito el daemon. Los registros de
//     versiones anteriores se migran al arrancar (prepararAuditoria), y rm
//     borra el de la máquina (borrarAuditoria).
//   - En macOS, el kling-vz de la máquina, junto a su socket
//     (machines/<id>/credaudit.jsonl): es el único sitio en que su sandbox le
//     deja escribir, y el que escribe es el mismo proceso que sirve el proxy,
//     así que sacarlo de ahí no protegería nada (docs/proxy-macos-separado.md).
//
// Aquí se lee, y por eso funciona igual con la máquina corriendo, congelada o
// parada. Ni Commit ni Fork lo copian (copian ficheros concretos del
// snapshot), así que una máquina nueva empieza con el suyo vacío.
//
// Se abre sin seguir enlaces, solo si es un fichero regular y con un único
// enlace: en macOS está en un directorio del VMM, y en Linux la migración lee
// el viejo de uno.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// credAuditMaxBytes es cuánto lee CredAudit como mucho entre el fichero y sus
// rotaciones (.1, .2...), del más nuevo al más viejo. Con los valores por
// defecto (credproxy.AuditMaxBytes y AuditGenerations, ~16 MiB) cabe todo.
const credAuditMaxBytes = 32 << 20

// CredAudit devuelve las líneas del registro de auditoría del proxy de
// credenciales de ref que pasan el filtro q, las más antiguas primero. Sin
// registro (la máquina nunca tuvo credenciales) devuelve una lista vacía.
func (m *Manager) CredAudit(ref string, q api.CredAuditQuery) ([]api.CredAuditRecord, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, &api.StatusError{Code: 404, Message: fmt.Sprintf("machine %q does not exist", ref)}
	}
	// Del fichero actual hacia atrás por las generaciones, hasta el tope: lo
	// más reciente es lo que no debe faltar.
	ficheros := credproxy.AuditFiles(m.credAuditPath(mc.ID))
	trozos := make([][]byte, len(ficheros))
	resto := int64(credAuditMaxBytes)
	for i := len(ficheros) - 1; i >= 0 && resto > 0; i-- {
		b, err := leerAuditoria(ficheros[i], resto)
		if err != nil {
			return nil, err
		}
		trozos[i] = b
		resto -= int64(len(b))
	}

	var out []api.CredAuditRecord
	for _, datos := range trozos {
		sc := bufio.NewScanner(bytes.NewReader(datos))
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
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
	if !fi.Mode().IsRegular() || variosEnlaces(fi) {
		return nil, fmt.Errorf("reading the credential audit log: %s is not a regular file with a single link", path)
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

// variosEnlaces dice si fi tiene más de un enlace duro: un registro nunca lo
// tiene, y uno con dos es un fichero ajeno enganchado en su sitio.
func variosEnlaces(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Nlink) > 1
}

// dirAuditoria es donde el daemon guarda los registros cuando los escribe él
// (Linux).
func (m *Manager) dirAuditoria() string { return filepath.Join(m.root, "audit") }

// prepararAuditoria deja listo <root>/audit (0700, de root), migra ahí los
// registros que versiones anteriores dejaban en el directorio de cada máquina
// y borra los de máquinas que ya no existen. Tras load, que dice cuáles hay, y
// antes de reconcile, que vuelve a levantar los proxies. Sin el directorio el
// proxy no escribe (y lo avisa): nunca vuelve al directorio del VMM.
func (m *Manager) prepararAuditoria() {
	if !auditoriaEnElDaemon {
		return
	}
	if err := asegurarDirPrivado(m.dirAuditoria()); err != nil {
		log.Printf("warning: credential audit log directory: %v", err)
		return
	}
	m.mu.RLock()
	vivas := make(map[string]bool, len(m.byID))
	for id := range m.byID {
		vivas[id] = true
	}
	m.mu.RUnlock()
	for id := range vivas {
		viejo := filepath.Join(m.dir(id), credproxy.AuditFile)
		nuevo := m.credAuditPath(id)
		for _, suf := range []string{"", ".1"} {
			if err := migrarRegistro(viejo+suf, nuevo+suf); err != nil {
				log.Printf("warning: moving the credential audit log of %s: %v", id, err)
			}
		}
	}
	m.barrerAuditorias(vivas)
}

// asegurarDirPrivado crea dir si falta y lo deja 0700 y del usuario del
// daemon. Si en su sitio hay otra cosa (un enlace, un fichero) o es de otro,
// falla en vez de escribir ahí.
func asegurarDirPrivado(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if os.Geteuid() == 0 {
		if err := os.Lchown(dir, 0, 0); err != nil {
			return err
		}
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Geteuid() != 0 && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s belongs to uid %d, not to the daemon", dir, st.Uid)
	}
	return os.Chmod(dir, 0o700)
}

// migrarRegistro lleva el registro viejo (en el directorio de la máquina, del
// VMM) a nuevo. Si ya hay uno nuevo, manda ese y el viejo se descarta. Uno que
// no es un fichero regular con un solo enlace se descarta sin leerlo. Si falla
// la lectura o la escritura, el viejo se queda para el siguiente arranque.
//
// Lo migrado lo escribió en su día el daemon, pero en un directorio en el que
// el VMM podía escribir: vale lo que valía antes, ni más ni menos.
func migrarRegistro(viejo, nuevo string) error {
	fi, err := os.Lstat(viejo)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || variosEnlaces(fi) {
		_ = os.Remove(viejo)
		return fmt.Errorf("%s is not a regular file with a single link: discarded", viejo)
	}
	if _, err := os.Lstat(nuevo); err == nil {
		return os.Remove(viejo)
	}
	datos, err := leerAuditoria(viejo, credAuditMaxBytes)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(nuevo, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(datos)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(nuevo)
		return err
	}
	return os.Remove(viejo)
}

// barrerAuditorias borra de <root>/audit los registros de máquinas que no están
// en vivas: las que se borraron por un camino que no pasa por Remove (una
// creación a medias, un daemon muerto a mitad de rm).
func (m *Manager) barrerAuditorias(vivas map[string]bool) {
	entradas, err := os.ReadDir(m.dirAuditoria())
	if err != nil {
		return
	}
	for _, e := range entradas {
		nombre := e.Name()
		// <id>.jsonl y sus rotaciones <id>.jsonl.N.
		if i := strings.LastIndexByte(nombre, '.'); i > 0 && strings.Trim(nombre[i+1:], "0123456789") == "" && i+1 < len(nombre) {
			nombre = nombre[:i]
		}
		id, ok := strings.CutSuffix(nombre, ".jsonl")
		if !ok || vivas[id] {
			continue
		}
		if err := os.Remove(filepath.Join(m.dirAuditoria(), e.Name())); err != nil {
			log.Printf("warning: removing an orphan credential audit log: %v", err)
		}
	}
}

// borrarAuditoria borra el registro de id (y su rotación) cuando vive fuera
// del directorio de la máquina; si vive dentro, ya se fue con él.
func (m *Manager) borrarAuditoria(id string) {
	if !auditoriaEnElDaemon {
		return
	}
	for _, f := range credproxy.AuditFiles(m.credAuditPath(id)) {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("warning: removing the credential audit log of %s: %v", id, err)
		}
	}
}
