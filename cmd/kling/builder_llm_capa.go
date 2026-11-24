package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/juan52878911/kindling/internal/durable"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
)

// LA CAPA DEL MODELO, EN GO.
//
// Con la base propia (von.BaseImage) no hay paquetes que instalar, así que el
// chroot, el loop y el overlay de 81-base-image.sh sobran: aquí se arma en
// memoria el /upper que habría dejado ese script (lo que hace él sobre un
// overlay, paso a paso) y se escribe con internal/ext4 de una pasada, del
// tamaño justo, a un temporal en KLING_OUT_DIR que se renombra durable. Sin
// mkfs sobredimensionado, cp -a por loop, sync del host entero, e2fsck ni
// copia para encoger: el GGUF se escribe una vez (antes, tres).
//
// Lo que hace el script y se reproduce aquí, en su orden:
//
//  1. install -Dm755 kling-guest /usr/local/bin/kling-guest (root, 0755): los
//     directorios que ya están en la base suben al upper con sus permisos,
//     dueño y xattrs (el copy-up del overlay).
//  2. cp -a ROOTFS_DIR/. /: cada entrada con su modo, dueño y mtime; un
//     directorio que ya estaba toma los de ROOTFS_DIR (cp -a los fija al acabar).
//  3. /entrypoint, 0755 de root, con el texto de entrypointBase.
//  4. /etc/resolv.conf con los resolvers públicos, con el modo y dueño del de la
//     base si lo hay (copy-up y truncado) o 0644 de root si no.
//
// Sin ENV_FILE (el constructor llm no pasa variables), sin /work (el script
// lo borra) y con /lost+found en la raíz de la capa, como mkfs.

// capaLLM es lo que hace falta para escribir la capa.
type capaLLM struct {
	Name    string
	BaseImg string // la base (images/<base>.ext4), para el copy-up
	Agent   string // kling-guest
	Rootfs  string // ROOTFS_DIR
	Service string // ejecutable del servicio, ruta absoluta en la imagen
	Out     string // KLING_OUT_DIR
	T       time.Time
	// Reflink es el fichero de la caché del que salió Modelo (ruta dentro de
	// la imagen): donde el disco del host lo permite, la capa comparte sus
	// bloques en vez de duplicarlos.
	Reflink, Modelo string
}

// resultadoCapa resume lo escrito.
type resultadoCapa struct {
	Stats      ext4.Stats
	Compartido int64 // bytes del modelo que comparten bloques con la caché
}

// entrypointBase es el /entrypoint que escribe 81-base-image.sh (sin
// /etc/kling/env); TestCapaLLMIgualQueScript lo compara con el del script.
func entrypointBase(service string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Generado por el constructor base de kindling: el agente de invitado es PID 1.\n")
	b.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	b.WriteString("export HOME=/root\n")
	if service != "" {
		// El mismo bucle que el script, línea `service exited with …` incluida
		// (dorado.ServiceExitedMark la cuenta). La salida va al fichero y a la
		// consola del invitado (la salida del entrypoint), donde el fabricante
		// la lee sin exec.
		b.WriteString("( while :; do " + shQuote(service) +
			"; echo \"service exited with $?, restarting in 1s\"; sleep 1; done ) </dev/null 2>&1 | tee -a /var/log/service.log &\n")
	}
	b.WriteString("exec /usr/local/bin/kling-guest -listen :8080\n")
	return b.String()
}

// resolvConfInvitado es el resolver del invitado (internal/net.DNSResolver).
const resolvConfInvitado = "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"

// construirCapaLLM escribe <Out>/<Name>.layer.ext4. Si falla no deja nada con
// ese nombre: se escribe al lado y se renombra al final, durable.
func construirCapaLLM(c capaLLM) (resultadoCapa, error) {
	var res resultadoCapa
	if !strings.HasPrefix(c.Service, "/") && c.Service != "" {
		return res, fmt.Errorf("SERVICE must be an absolute path inside the image: %s", c.Service)
	}
	if err := esELF(c.Agent); err != nil {
		return res, err
	}
	bf, err := os.Open(c.BaseImg)
	if err != nil {
		return res, fmt.Errorf("base image: %w", err)
	}
	base, err := ext4.Read(bf)
	bf.Close()
	if err != nil {
		return res, fmt.Errorf("reading the base image: %w", err)
	}
	t := c.T
	upper := ext4.NewDir(0o755, 0, 0, t)

	// 1. El agente.
	ast, err := os.Stat(c.Agent)
	if err != nil {
		return res, err
	}
	bin, err := dirCopyUp(upper, base, "/usr/local/bin", t)
	if err != nil {
		return res, err
	}
	bin.SetChild("kling-guest", &ext4.Node{Mode: ext4.ModeReg | 0o755, Mtime: t, Size: ast.Size(), Data: ext4.HostFile{Path: c.Agent}})

	// 2. ROOTFS_DIR, como cp -a.
	if c.Rootfs != "" {
		if err := copiarArbol(upper, base, c.Rootfs, t); err != nil {
			return res, err
		}
	}
	if c.Service != "" {
		n := upper.Lookup(c.Service)
		if n == nil {
			n = base.Lookup(c.Service)
		}
		if n == nil || !n.IsReg() || n.Mode&0o111 == 0 {
			return res, fmt.Errorf("SERVICE %s is not an executable inside the image", c.Service)
		}
	}

	// 3. El entrypoint.
	ep := entrypointBase(c.Service)
	upper.SetChild("entrypoint", &ext4.Node{Mode: ext4.ModeReg | 0o755, Mtime: t, Size: int64(len(ep)), Data: ext4.Bytes(ep)})

	// 4. El resolver.
	etc, err := dirCopyUp(upper, base, "/etc", t)
	if err != nil {
		return res, err
	}
	rc := etc.Child("resolv.conf")
	if rc == nil {
		if b := base.Lookup("/etc/resolv.conf"); b != nil {
			if !b.IsReg() {
				return res, fmt.Errorf("the base's /etc/resolv.conf is not a regular file")
			}
			rc = &ext4.Node{Mode: b.Mode, UID: b.UID, GID: b.GID, Xattrs: sinOverlay(b.Xattrs)}
		} else {
			rc = &ext4.Node{Mode: ext4.ModeReg | 0o644}
		}
		etc.SetChild("resolv.conf", rc)
	} else if !rc.IsReg() {
		return res, fmt.Errorf("ROOTFS_DIR's etc/resolv.conf is not a regular file")
	}
	rc.Mtime, rc.Size, rc.Data = t, int64(len(resolvConfInvitado)), ext4.Bytes(resolvConfInvitado)

	layer := ext4.NewDir(0o755, 0, 0, t)
	layer.SetChild("upper", upper)
	h := sha256.Sum256([]byte("kindling-llm-v1\x00" + c.Name + "\x00" + t.UTC().Format(time.RFC3339Nano)))
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return res, err
	}
	dst := filepath.Join(c.Out, c.Name+".layer.ext4")
	tmp := filepath.Join(c.Out, "."+c.Name+".layer.ext4.tmp")
	defer os.Remove(tmp)
	res.Stats, err = writeExt4(tmp, layer, nil, ext4.Options{
		Time: t, UUID: imagen.UUID("kindling-llm", "layer", h), LostFound: true, ZeroHoles: true,
		SlackBlocks: 16 << 20 / ext4.BlockSize, SlackInodes: 256,
	})
	if err != nil {
		return res, fmt.Errorf("writing the layer: %w", err)
	}
	if c.Reflink != "" && c.Modelo != "" {
		// Lo mejor posible: si el disco no sabe, la capa queda como está.
		res.Compartido, _ = compartirBloques(tmp, "/upper"+c.Modelo, c.Reflink)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return res, err
	}
	if err := durable.Renombrar(tmp, dst); err != nil {
		return res, err
	}
	return res, nil
}

// esELF es la comprobación del script: el agente tiene que ser un binario ELF.
func esELF(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var m [4]byte
	if _, err := io.ReadFull(f, m[:]); err != nil || string(m[:]) != "\x7fELF" {
		return fmt.Errorf("%s is not an ELF binary: build kling-guest for linux (make guest)", p)
	}
	return nil
}

// sinOverlay quita los xattrs del propio overlay, que el copy-up no copia.
func sinOverlay(x map[string][]byte) map[string][]byte {
	if len(x) == 0 {
		return nil
	}
	out := map[string][]byte{}
	for k, v := range x {
		if !strings.HasPrefix(k, "trusted.overlay.") {
			out[k] = append([]byte(nil), v...)
		}
	}
	return out
}

// dirCopyUp da el directorio p del upper, creándolo como lo subiría el
// overlay: cada componente que está en la base, con su modo, dueño y xattrs;
// el que no, 0755 de root (install -D, mkdir -p). Un componente que en la base
// no es un directorio (un enlace) no se sigue: el script escribiría a través
// de él, fuera de la ruta, y eso no se reproduce.
func dirCopyUp(upper, base *ext4.Node, p string, t time.Time) (*ext4.Node, error) {
	cur, lower := upper, base
	for _, c := range strings.Split(strings.Trim(p, "/"), "/") {
		var lc *ext4.Node
		if lower != nil {
			lc = lower.Child(c)
		}
		nx := cur.Child(c)
		if nx == nil {
			switch {
			case lc == nil:
				nx = ext4.NewDir(0o755, 0, 0, t)
			case lc.IsDir():
				nx = ext4.NewDir(lc.Mode&ext4.ModePerm, lc.UID, lc.GID, t)
				nx.Xattrs = sinOverlay(lc.Xattrs)
			default:
				return nil, fmt.Errorf("the base has a non-directory at %s", path.Join("/", p))
			}
			cur.SetChild(c, nx)
		} else if !nx.IsDir() {
			return nil, fmt.Errorf("%s is not a directory in the layer", path.Join("/", p))
		}
		cur = nx
		if lc != nil && lc.IsDir() {
			lower = lc
		} else {
			lower = nil
		}
	}
	return cur, nil
}

// copiarArbol hace lo de `cp -a dir/. /` sobre el upper.
func copiarArbol(upper, base *ext4.Node, dir string, t time.Time) error {
	var walk func(rel string, dst *ext4.Node) error
	walk = func(rel string, dst *ext4.Node) error {
		ents, err := os.ReadDir(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, e := range ents {
			name := e.Name()
			if err := ext4.CheckName(name); err != nil {
				return err
			}
			r := path.Join(rel, name)
			fi, err := os.Lstat(filepath.Join(dir, r))
			if err != nil {
				return err
			}
			uid, gid := dueño(fi)
			perm := uint32(fi.Mode().Perm())
			if fi.Mode()&fs.ModeSetuid != 0 {
				perm |= 0o4000
			}
			if fi.Mode()&fs.ModeSetgid != 0 {
				perm |= 0o2000
			}
			if fi.Mode()&fs.ModeSticky != 0 {
				perm |= 0o1000
			}
			switch {
			case fi.IsDir():
				d, err := dirCopyUp(upper, base, "/"+r, t)
				if err != nil {
					return err
				}
				if err := walk(r, d); err != nil {
					return err
				}
				// cp -a fija modo, dueño y hora del directorio al acabar.
				d.Mode, d.UID, d.GID, d.Mtime = ext4.ModeDir|perm, uid, gid, fi.ModTime()
			case fi.Mode().IsRegular():
				dst.SetChild(name, &ext4.Node{Mode: ext4.ModeReg | perm, UID: uid, GID: gid, Mtime: fi.ModTime(),
					Size: fi.Size(), Data: ext4.HostFile{Path: filepath.Join(dir, r)}})
			case fi.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(filepath.Join(dir, r))
				if err != nil {
					return err
				}
				dst.SetChild(name, &ext4.Node{Mode: ext4.ModeLink | 0o777, UID: uid, GID: gid, Mtime: fi.ModTime(), Target: target})
			default:
				return fmt.Errorf("ROOTFS_DIR/%s: unsupported file type %s", r, fi.Mode().Type())
			}
		}
		return nil
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("ROOTFS_DIR %s is not a directory", dir)
	} else {
		// La raíz también: cp -a de "dir/." fija los atributos de "/".
		if err := walk("", upper); err != nil {
			return err
		}
		uid, gid := dueño(fi)
		upper.Mode, upper.UID, upper.GID, upper.Mtime = ext4.ModeDir|uint32(fi.Mode().Perm()), uid, gid, fi.ModTime()
	}
	return nil
}

func dueño(fi fs.FileInfo) (uint32, uint32) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid, st.Gid
	}
	return 0, 0
}

// REFLINK. compartirBloques hace que los bloques del fichero p de la imagen img
// sean los mismos que los de src (de donde se copió), con FICLONERANGE: en
// XFS o btrfs la capa deja de ocupar el modelo otra vez. No hace falta tocar el
// contenido (ya es igual): solo dónde vive. En ext4, tmpfs u otro sistema la
// primera llamada falla y no se hace nada más. copy_file_range no vale aquí:
// donde no hay reflink copia los datos, y sería una escritura más del modelo.
// Solo bloques enteros: el último, si va a medias, se queda como está.
func compartirBloques(img, p, src string) (int64, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	runs, err := bloquesDe(f, p)
	if err != nil {
		return 0, err
	}
	var hecho int64
	for _, r := range runs {
		if err := clonarRango(f, s, r.fichero*ext4.BlockSize, r.n*ext4.BlockSize, r.imagen*ext4.BlockSize); err != nil {
			return hecho, err
		}
		hecho += r.n * ext4.BlockSize
	}
	return hecho, nil
}

// tramo son n bloques seguidos del fichero (desde el bloque fichero) que viven
// seguidos en la imagen (desde el bloque imagen).
type tramo struct{ fichero, imagen, n int64 }

// sondeo es un io.ReaderAt de la imagen que, una vez leídos los metadatos,
// no lee nada: apunta en qué desplazamiento pediría cada bloque de datos.
type sondeo struct {
	r       io.ReaderAt
	apuntar bool
	ultimo  int64
	leido   bool
}

func (s *sondeo) ReadAt(p []byte, off int64) (int, error) {
	if !s.apuntar {
		return s.r.ReadAt(p, off)
	}
	s.ultimo, s.leido = off, true
	clear(p)
	return len(p), nil
}

// bloquesDe da dónde están, en la imagen, los bloques enteros del fichero p,
// leyendo la imagen con internal/ext4 (sin leer los datos).
func bloquesDe(f *os.File, p string) ([]tramo, error) {
	sd := &sondeo{r: f}
	root, err := ext4.Read(sd)
	if err != nil {
		return nil, err
	}
	n := root.Lookup(p)
	if n == nil || !n.IsReg() {
		return nil, fmt.Errorf("%s is not a file in the image", p)
	}
	rc, err := n.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	sd.apuntar = true
	var runs []tramo
	buf := make([]byte, ext4.BlockSize)
	for b := int64(0); b < n.Size/ext4.BlockSize; b++ {
		sd.leido = false
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, err
		}
		if !sd.leido {
			continue // un hueco
		}
		if sd.ultimo%ext4.BlockSize != 0 {
			return nil, errors.New("unaligned data block")
		}
		pb := sd.ultimo / ext4.BlockSize
		if k := len(runs); k > 0 && runs[k-1].fichero+runs[k-1].n == b && runs[k-1].imagen+runs[k-1].n == pb {
			runs[k-1].n++
			continue
		}
		runs = append(runs, tramo{b, pb, 1})
	}
	return runs, nil
}

// LA CACHÉ DEL MODELO TRAS CONSTRUIR. Con la capa hecha, los pesos están en
// ella (y, tras el dorado, en su mem.file): el GGUF de cache/von/models era la
// tercera copia, 1 GiB más por GiB de modelo. Se borra, con su sello, salvo
// que:
//   - el spec pida keep_cache (reconstruir sin volver a bajar);
//   - lo haya convertido kindling (los codificadores: no hay de dónde
//     bajarlo, scripts/encoder-gguf.sh tendría que volver a correr);
//   - la capa comparta sus bloques (reflink): entonces no ocupa nada más.
//
// Lo mejor posible: si no se puede borrar, se queda.
func soltarCacheModelo(p string, size int64, keep, convertido bool, compartido int64) {
	if keep || convertido || (size > 0 && compartido >= size-ext4.BlockSize) {
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return
	}
	os.Remove(p + selloExt)
	fmt.Printf("model cache: dropped %s (the layer has it; keep_cache keeps it for rebuilds)\n", filepath.Base(p))
}

// keepCacheDe lee keep_cache del spec del modelo (lo pone `kling ai model add
// -keep-cache`).
func keepCacheDe(spec []byte) bool {
	var s struct {
		KeepCache bool `json:"keep_cache"`
	}
	return len(spec) > 0 && json.Unmarshal(spec, &s) == nil && s.KeepCache
}

// ficlonerange es _IOW(0x94, 13, struct file_clone_range) en Linux.
const ficlonerange = 0x4020940d

func clonarRango(dst, src *os.File, srcOff, n, dstOff int64) error {
	arg := struct {
		SrcFD     int64
		SrcOffset uint64
		SrcLength uint64
		DstOffset uint64
	}{int64(src.Fd()), uint64(srcOff), uint64(n), uint64(dstOff)}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlonerange, uintptr(unsafe.Pointer(&arg)))
	if e != 0 {
		return e
	}
	return nil
}
