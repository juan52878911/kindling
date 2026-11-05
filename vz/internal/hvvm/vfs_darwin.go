//go:build darwin && arm64

package hvvm

// Las carpetas vivas por virtio-fs (vfs.c): el C transporta las peticiones
// FUSE de las colas y aquí las atiende el mismo servidor FUSE del agente
// (pkg/guest.HostFS), con su sesión kling-share/1 hacia internal/share en el
// daemon (POST /kling/share/attach). Ver docs/backend-hv.md.

/*
#include "khv.h"
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/guest"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// MaxVirtioFS son los dispositivos virtio-fs que caben en una VM.
const MaxVirtioFS = int(C.KHV_MAX_VFS)

// VirtioFS: kling-hv sabe crear dispositivos virtio-fs (server.VirtioFSFactory).
// KLING_HV_VIRTIOFS=0 lo apaga (las carpetas vuelven a FUSE por vsock): para
// comparar, o si algo falla.
func (f *Factory) VirtioFS() bool { return os.Getenv("KLING_HV_VIRTIOFS") != "0" }

// vfsDev es el dispositivo virtio-fs idx visto como /dev/fuse: cada Read
// espera una petición entera y cada Write manda una respuesta entera.
type vfsDev struct{ idx C.int }

func (d vfsDev) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, syscall.EINVAL
	}
	n := C.khv_vfs_leer(d.idx, unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n < 0 {
		return 0, syscall.ENODEV // cerrado: serve termina sin decir nada
	}
	return int(n), nil
}

func (d vfsDev) Write(msg []byte) (int, error) {
	if len(msg) == 0 {
		return 0, syscall.EINVAL
	}
	if C.khv_vfs_responder(d.idx, unsafe.Pointer(&msg[0]), C.uint32_t(len(msg))) != 0 {
		return 0, syscall.ENOENT // nadie la esperaba (un reset entre medias)
	}
	return len(msg), nil
}

// carpeta es un dispositivo virtio-fs de la VM y su servidor.
type carpeta struct {
	tag int
	idx C.int
	fs  *guest.HostFS
}

// configurarVirtioFS rellena la parte virtio-fs de cfg. Los tags (strings de C)
// los libera quien llama con libres.
func configurarVirtioFS(cfg *C.struct_khv_config, v []spec.VirtioFS, cstr func(string) *C.char) error {
	if len(v) > MaxVirtioFS {
		return fmt.Errorf("kling-hv supports at most %d virtio-fs shares", MaxVirtioFS)
	}
	cfg.nvfs = C.int(len(v))
	for i, f := range v {
		cfg.vfs_tags[i] = cstr(share.VirtioFSTag(f.Tag))
	}
	return nil
}

// crearCarpetas pone en marcha el servidor de cada dispositivo (tras
// khv_create): desde cero o, al restaurar, con lo que guardó el snapshot.
// Cada uno lee en su goroutine; khv_vfs_leer espera a que la VM corra.
func crearCarpetas(v []spec.VirtioFS) ([]*carpeta, error) {
	var cs []*carpeta
	for i, f := range v {
		dev := vfsDev{idx: C.int(i)}
		nombre := share.VirtioFSTag(f.Tag)
		var fs *guest.HostFS
		if len(f.State) > 0 {
			var err error
			if fs, err = guest.NewHostFSFromState(dev, nombre, f.ReadOnly, f.State); err != nil {
				return nil, fmt.Errorf("share %d: %w", f.Tag, err)
			}
		} else {
			fs = guest.NewHostFS(dev, nombre, f.ReadOnly)
		}
		cs = append(cs, &carpeta{tag: f.Tag, idx: C.int(i), fs: fs})
	}
	for _, c := range cs {
		go c.fs.Serve()
	}
	return cs, nil
}

func cerrarCarpetas(cs []*carpeta) {
	for _, c := range cs {
		C.khv_vfs_cerrar(c.idx)
	}
}

// AttachShare pone la sesión de la carpeta tag (server.ShareVM).
func (m *machine) AttachShare(tag int, rwc io.ReadWriteCloser) error {
	for _, c := range m.carpetas {
		if c.tag == tag {
			c.fs.Attach(rwc)
			return nil
		}
	}
	return fmt.Errorf("this VM has no virtio-fs device for share %d", tag)
}

// ShareState es lo que el servidor de cada carpeta guarda en el snapshot.
func (m *machine) ShareState() (map[int][]byte, error) {
	out := map[int][]byte{}
	for _, c := range m.carpetas {
		b, err := c.fs.State()
		if err != nil {
			return nil, fmt.Errorf("share %d: %w", c.tag, err)
		}
		out[c.tag] = b
	}
	return out, nil
}
