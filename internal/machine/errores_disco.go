package machine

// Errores de disco del invitado.
//
// Cuando el host no puede escribir el overlay (el almacén de copia al escribir
// lleno, un disco que falla), el VMM se lo pasa al invitado como un error de
// E/S, y el núcleo del invitado lo escribe en su consola serie, que es
// firecracker.log ("I/O error, dev vdb, sector ...", "Aborting journal on
// device vdb"). Desde fuera la máquina sigue "running": lo que falla es lo de
// dentro (Postgres hace PANIC), y nadie lo relacionaba con el disco.
//
// El vigilante lee lo nuevo de cada consola en cada vuelta y lo anota en la
// máquina (DiskErrors, DiskErrorAt, DiskError), para que lo digan `kling ps`
// y `kling db doctor`. El texto es del invitado: solo se usa para avisar de su
// propia máquina, y se limpia antes de guardarlo o de llevarlo al log.

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// posConsola es hasta dónde se leyó la consola de una máquina: el inodo
// distingue una consola nueva (cada arranque la recrea) de la misma que creció.
type posConsola struct {
	ino uint64
	off int64
}

// lecturaConsolaMax es lo más que se lee de una consola por vuelta: si
// escribió más, se salta al final (un invitado ruidoso no frena al vigilante).
const lecturaConsolaMax = 1 << 20

// esErrorDisco dice si una línea de la consola es un error de disco del
// núcleo del invitado sobre uno de sus discos virtio (vda, vdb...).
func esErrorDisco(l string) bool {
	switch {
	case strings.Contains(l, "I/O error") && strings.Contains(l, " vd"):
		return true // "I/O error, dev vdb, sector", "Buffer I/O error on dev vdb"
	case strings.Contains(l, "EXT4-fs error"), strings.Contains(l, "EXT4-fs (vd") && strings.Contains(l, "I/O error"):
		return true
	case strings.Contains(l, "Aborting journal on device vd"):
		return true
	}
	return false
}

// limpiarLineaConsola deja solo ASCII imprimible y como mucho 160 caracteres.
func limpiarLineaConsola(l string) string {
	var b strings.Builder
	for _, r := range l {
		if b.Len() >= 160 {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// escanearConsola lee de r las líneas con error de disco: cuántas y la
// última.
func escanearConsola(r io.Reader) (n int, ultima string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		if l := sc.Text(); esErrorDisco(l) {
			n++
			ultima = l
		}
	}
	return n, limpiarLineaConsola(ultima)
}

// revisarErroresDisco lee lo nuevo de la consola de cada máquina viva (ver
// arriba). La llama el vigilante.
func (m *Manager) revisarErroresDisco() {
	m.mu.RLock()
	type viva struct {
		id, name string
		started  time.Time
	}
	var vivas []viva
	for id, mc := range m.byID {
		if mc.State != api.StateRunning && mc.State != api.StatePaused {
			continue
		}
		var t time.Time
		if mc.StartedAt != nil {
			t = *mc.StartedAt
		}
		vivas = append(vivas, viva{id, mc.Name, t})
	}
	m.mu.RUnlock()

	m.consolaMu.Lock()
	defer m.consolaMu.Unlock()
	if m.consolaLeida == nil {
		m.consolaLeida = map[string]posConsola{}
	}
	vistas := make(map[string]bool, len(vivas))
	for _, v := range vivas {
		vistas[v.id] = true
		n, ultima := m.leerConsolaNueva(v.id, v.started)
		if n == 0 {
			continue
		}
		now := time.Now()
		m.mu.Lock()
		live := m.byID[v.id]
		primera := live != nil && live.DiskErrors == 0
		if live != nil {
			live.DiskErrors += n
			live.DiskErrorAt = &now
			live.DiskError = ultima
			m.persist()
		}
		m.mu.Unlock()
		if primera {
			log.Printf("warning: %s: its guest got disk I/O errors (%q); its data may be damaged. Check the copy-on-write store (kling cow) and the host disk", v.name, ultima)
		}
	}
	for id := range m.consolaLeida {
		if !vistas[id] {
			delete(m.consolaLeida, id)
		}
	}
}

// leerConsolaNueva lee lo que la consola de id escribió desde la última
// vuelta. Una máquina que no se había visto empieza desde el final si ya corría
// antes de que arrancase el daemon (lo de antes ya se contó, o es de otra
// vida), y desde el principio si arrancó después. Con m.consolaMu.
func (m *Manager) leerConsolaNueva(id string, started time.Time) (int, string) {
	f, err := abrirConsolaExistente(filepath.Join(m.dir(id), "firecracker.log"), os.O_RDONLY)
	if err != nil {
		return 0, ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, ""
	}
	tam := fi.Size()
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	pos, visto := m.consolaLeida[id]
	desde := pos.off
	switch {
	case !visto && started.Before(m.inicioDaemon):
		desde = tam
	case !visto, pos.ino != ino, desde > tam:
		desde = 0 // nueva (tras un thaw) o rotada (consola.go)
	}
	if tam-desde > lecturaConsolaMax {
		desde = tam - lecturaConsolaMax
	}
	m.consolaLeida[id] = posConsola{ino, desde}
	if tam <= desde {
		return 0, ""
	}
	buf := make([]byte, tam-desde)
	k, _ := f.ReadAt(buf, desde)
	buf = buf[:k]
	// Solo líneas enteras: la última, a medias, se lee en la vuelta siguiente.
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 {
		return 0, ""
	}
	m.consolaLeida[id] = posConsola{ino, desde + int64(i) + 1}
	buf = buf[:i+1]
	return escanearConsola(bytes.NewReader(buf))
}
