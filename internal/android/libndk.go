package android

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
)

// LIBNDK_TRANSLATION DESDE LA IMAGEN DEL EMULADOR (arm_translation "libndk").
//
// La imagen de sistema del emulador es un zip con system.img: un disco GPT
// cuya partición "super" lleva las particiones dinámicas de Android (formato
// "liblp": geometría, cabecera y tablas de particiones y extents) y, dentro,
// el ext4 "system". De ahí salen los ficheros de la traducción, que quedan en
// la caché como un tar pequeño (~21 MiB); el zip (1,4 GiB) se borra después.
// Todo en Go y en streaming: el system.img entero (4 GiB) no se escribe al
// disco, solo la partición "system" (dispersa) mientras se lee.

// libndkCacheVersion cambia si cambia lo que se saca del zip.
const libndkCacheVersion = "v1"

// libndkWanted dice qué rutas del ext4 "system" (con la raíz del sistema en
// "/", system-as-root) son de la traducción arm64. Las de 32 bits (arm, lib/)
// no: la imagen de Redroid es 64only.
func libndkWanted(p string) bool {
	for _, d := range []string{"/system/lib64/arm64", "/system/bin/arm64", "/system/etc/binfmt_misc"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return !strings.HasPrefix(p, "/system/etc/binfmt_misc/arm_")
		}
	}
	if strings.HasPrefix(p, "/system/lib64/libndk_translation") && strings.HasSuffix(p, ".so") {
		return true
	}
	switch p {
	case "/system/bin/ndk_translation_program_runner_binfmt_misc_arm64",
		"/system/etc/init/ndk_translation.rc", "/system/etc/ld.config.arm64.txt":
		return true
	}
	return false
}

// libndkRequired son los ficheros sin los que la traducción no funciona:
// si falta uno, la imagen de origen no es la que se esperaba.
var libndkRequired = []string{
	"/system/lib64/libndk_translation.so",
	"/system/lib64/arm64/libc.so",
	"/system/lib64/arm64/libdl.so",
	"/system/bin/arm64/linker64",
	"/system/bin/ndk_translation_program_runner_binfmt_misc_arm64",
	"/system/etc/binfmt_misc/arm64_exe",
	"/system/etc/binfmt_misc/arm64_dyn",
	"/system/etc/init/ndk_translation.rc",
}

// libndkTar deja en la caché del daemon el tar con la traducción (bajando y
// comprobando el zip fijado si hace falta) y devuelve su ruta.
func (b *builder) libndkTar(ctx context.Context) (string, error) {
	dir := filepath.Join(b.cache, "android")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tarPath := filepath.Join(dir, "libndk-"+libndkPin.SHA256[:16]+"-"+libndkCacheVersion+".tar")
	if _, err := os.Stat(tarPath); err == nil {
		return tarPath, nil
	}
	zipPath := filepath.Join(dir, path.Base(libndkPin.URL))
	t0 := time.Now()
	b.logf("arm translation: downloading %s (%d MiB, sha256 %s...)", libndkPin.URL, libndkPin.Size>>20, libndkPin.SHA256[:12])
	if err := imagen.FetchVerified(ctx, libndkPin.URL, libndkPin.SHA256, libndkPin.Size, zipPath); err != nil {
		return "", fmt.Errorf("libndk_translation: %w", err)
	}
	b.logf("arm translation: sha256 verified (%.1f s); extracting %s from partition %q", time.Since(t0).Seconds(), libndkPin.Entry, libndkPin.Partition)
	t1 := time.Now()
	n, err := extractLibndk(zipPath, libndkPin.Entry, libndkPin.Partition, tarPath)
	if err != nil {
		return "", fmt.Errorf("libndk_translation: %w", err)
	}
	b.logf("arm translation: %d entries cached in %s (%.1f s)", n, tarPath, time.Since(t1).Seconds())
	// El zip ya no hace falta: el tar se saca de él de forma determinista.
	os.Remove(zipPath)
	return tarPath, nil
}

// extractLibndk saca de entry (el system.img del zip) la partición dinámica
// part, lee su ext4 y escribe en dst un tar con lo de libndkWanted.
func extractLibndk(zipPath, entry, part, dst string) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	var zf *zip.File
	for _, f := range zr.File {
		if f.Name == entry {
			zf = f
		}
	}
	if zf == nil {
		return 0, fmt.Errorf("%s: no %s", filepath.Base(zipPath), entry)
	}
	rc, err := zf.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".libndk-"+part+"-*.ext4")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := copyDynamicPartition(rc, part, tmp); err != nil {
		return 0, fmt.Errorf("%s: %w", entry, err)
	}
	root, err := ext4.Read(tmp)
	if err != nil {
		return 0, fmt.Errorf("partition %s: %w", part, err)
	}
	return writeLibndkTar(root, dst)
}

// writeLibndkTar escribe el tar (padres antes que hijos, en orden de nombre:
// determinista) y comprueba que está lo imprescindible.
func writeLibndkTar(root *ext4.Node, dst string) (int, error) {
	for _, p := range libndkRequired {
		if n := root.Lookup(p); n == nil || !n.IsReg() {
			return 0, fmt.Errorf("no %s in the system image", p)
		}
	}
	part := dst + ".part"
	f, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	defer os.Remove(part)
	defer f.Close()
	tw := tar.NewWriter(f)
	n := 0
	err = root.Walk(func(p string, nd *ext4.Node) error {
		if !libndkWanted(p) {
			return nil
		}
		h := &tar.Header{Name: strings.TrimPrefix(p, "/"), Mode: int64(nd.Mode & ext4.ModePerm),
			Uid: int(nd.UID), Gid: int(nd.GID), ModTime: nd.Mtime.UTC().Truncate(time.Second), Format: tar.FormatPAX}
		var data []byte
		switch {
		case nd.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
		case nd.IsReg():
			d, err := nd.ReadAll()
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			h.Typeflag, h.Size, data = tar.TypeReg, int64(len(d)), d
		case nd.IsLink():
			h.Typeflag, h.Linkname = tar.TypeSymlink, nd.Target
		default:
			return fmt.Errorf("%s: unexpected file type %o", p, nd.Mode&ext4.ModeType)
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		n++
		_, err := tw.Write(data)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return 0, err
	}
	return n, os.Rename(part, dst)
}

// Formatos de disco: GPT y las particiones dinámicas de Android
// (system/core/fs_mgr/liblp/include/liblp/metadata_format.h).
const (
	sector          = 512
	lpReserved      = 4096           // LP_PARTITION_RESERVED_BYTES
	lpGeometrySize  = 4096           // LP_METADATA_GEOMETRY_SIZE (dos copias)
	lpGeometryMagic = 0x616c4467     // "gDla"
	lpHeaderMagic   = 0x414c5030     // "0PLA"
	lpMaxTables     = 1 << 20        // lo que se acepta leer de tablas
	gptMaxEntries   = 256            // entradas GPT que se miran
	maxPrefix       = 64 << 20       // hasta dónde se lee a memoria buscando las tablas
	zeroBlock       = ext4.BlockSize // bloques de ceros que quedan como hueco
)

// extent es un tramo de la partición: n bytes que en el disco empiezan en
// img y en la partición en logical (zero: tramo de ceros, sin datos).
type extent struct {
	img, logical, n int64
	zero            bool
}

// copyDynamicPartition lee el disco de r (de principio a fin, una vez) y
// escribe en w la partición dinámica name, dispersa.
func copyDynamicPartition(r io.Reader, name string, w *os.File) error {
	br := bufio.NewReaderSize(r, 1<<20)
	pre := &prefix{r: br}
	superOff, err := findGPTPartition(pre, "super")
	if err != nil {
		return err
	}
	exts, size, err := lpExtents(pre, superOff, name)
	if err != nil {
		return err
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].img < exts[j].img })
	buf := make([]byte, 1<<20)
	for _, e := range exts {
		if e.zero {
			continue
		}
		done := int64(0)
		// Lo que ya se leyó a memoria buscando las tablas.
		if e.img < int64(len(pre.b)) {
			end := min(e.img+e.n, int64(len(pre.b)))
			if err := writeSparse(w, pre.b[e.img:end], e.logical); err != nil {
				return err
			}
			done = end - e.img
		}
		if done < e.n {
			if err := pre.skipTo(e.img + done); err != nil {
				return fmt.Errorf("partition %s: %w", name, err)
			}
			for done < e.n {
				k := int(min(int64(len(buf)), e.n-done))
				if _, err := io.ReadFull(br, buf[:k]); err != nil {
					return fmt.Errorf("partition %s: %w", name, err)
				}
				if err := writeSparse(w, buf[:k], e.logical+done); err != nil {
					return err
				}
				done += int64(k)
				pre.pos += int64(k)
			}
		}
	}
	return w.Truncate(size)
}

// prefix es el principio del disco en memoria (para GPT y liblp) y la
// posición del lector detrás.
type prefix struct {
	r   *bufio.Reader
	b   []byte
	pos int64 // bytes consumidos de r
}

// need lee hasta tener n bytes en memoria.
func (p *prefix) need(n int64) error {
	if n > maxPrefix {
		return fmt.Errorf("partition tables beyond %d MiB", maxPrefix>>20)
	}
	if int64(len(p.b)) >= n {
		return nil
	}
	if p.pos != int64(len(p.b)) {
		return errors.New("disk read out of order")
	}
	more := make([]byte, n-int64(len(p.b)))
	if _, err := io.ReadFull(p.r, more); err != nil {
		return fmt.Errorf("disk too short: %w", err)
	}
	p.b = append(p.b, more...)
	p.pos = int64(len(p.b))
	return nil
}

func (p *prefix) at(off, n int64) ([]byte, error) {
	if err := p.need(off + n); err != nil {
		return nil, err
	}
	return p.b[off : off+n], nil
}

// skipTo descarta hasta la posición off del disco.
func (p *prefix) skipTo(off int64) error {
	if off < p.pos {
		return errors.New("extents overlap or go backwards")
	}
	if _, err := io.CopyN(io.Discard, p.r, off-p.pos); err != nil {
		return err
	}
	p.pos = off
	return nil
}

// findGPTPartition devuelve el desplazamiento de la partición GPT name.
func findGPTPartition(p *prefix, name string) (int64, error) {
	le := binary.LittleEndian
	h, err := p.at(sector, 92)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(h[:8], []byte("EFI PART")) {
		return 0, errors.New("not a GPT disk")
	}
	lba, num, esz := int64(le.Uint64(h[72:])), int64(le.Uint32(h[80:])), int64(le.Uint32(h[84:]))
	if esz < 128 || esz > 4096 || num <= 0 {
		return 0, fmt.Errorf("bad GPT entry table (%d entries of %d bytes)", num, esz)
	}
	num = min(num, gptMaxEntries)
	t, err := p.at(lba*sector, num*esz)
	if err != nil {
		return 0, err
	}
	for i := int64(0); i < num; i++ {
		e := t[i*esz : (i+1)*esz]
		u := make([]uint16, 36)
		for j := range u {
			u[j] = le.Uint16(e[56+2*j:])
		}
		n := strings.TrimRight(string(utf16.Decode(u)), "\x00")
		if n == name {
			return int64(le.Uint64(e[32:])) * sector, nil
		}
	}
	return 0, fmt.Errorf("no GPT partition %q", name)
}

// lpExtents lee la geometría y la primera copia de las tablas de liblp de la
// partición super que empieza en superOff, y devuelve los tramos de la
// partición dinámica name (en desplazamientos del disco) y su tamaño.
func lpExtents(p *prefix, superOff int64, name string) ([]extent, int64, error) {
	le := binary.LittleEndian
	g, err := p.at(superOff+lpReserved, 52)
	if err != nil {
		return nil, 0, err
	}
	if le.Uint32(g) != lpGeometryMagic {
		return nil, 0, errors.New("super: no liblp geometry")
	}
	if bs := le.Uint32(g[48:]); bs%sector != 0 || bs == 0 {
		return nil, 0, fmt.Errorf("super: logical block size %d", bs)
	}
	hoff := superOff + lpReserved + 2*lpGeometrySize
	h, err := p.at(hoff, 128)
	if err != nil {
		return nil, 0, err
	}
	if le.Uint32(h) != lpHeaderMagic {
		return nil, 0, errors.New("super: no liblp metadata header")
	}
	if major := le.Uint16(h[4:]); major != 10 {
		return nil, 0, fmt.Errorf("super: liblp metadata version %d not supported", major)
	}
	hsz, tsz := int64(le.Uint32(h[8:])), int64(le.Uint32(h[44:]))
	if hsz < 128 || hsz > 4096 || tsz > lpMaxTables {
		return nil, 0, errors.New("super: bad liblp header sizes")
	}
	t, err := p.at(hoff+hsz, tsz)
	if err != nil {
		return nil, 0, err
	}
	desc := func(i int) (off, num, esz int64) {
		d := h[80+12*i:]
		return int64(le.Uint32(d)), int64(le.Uint32(d[4:])), int64(le.Uint32(d[8:]))
	}
	table := func(i int, minSize int64) ([]byte, int64, int64, error) {
		off, num, esz := desc(i)
		if esz < minSize || off+num*esz > tsz {
			return nil, 0, 0, errors.New("super: liblp table out of range")
		}
		return t[off : off+num*esz], num, esz, nil
	}
	parts, np, psz, err := table(0, 52)
	if err != nil {
		return nil, 0, err
	}
	exts, ne, xsz, err := table(1, 24)
	if err != nil {
		return nil, 0, err
	}
	for i := int64(0); i < np; i++ {
		e := parts[i*psz:]
		if strings.TrimRight(string(e[:36]), "\x00") != name {
			continue
		}
		first, n := int64(le.Uint32(e[40:])), int64(le.Uint32(e[44:]))
		if first+n > ne {
			return nil, 0, errors.New("super: extent index out of range")
		}
		var out []extent
		var logical int64
		for j := first; j < first+n; j++ {
			x := exts[j*xsz:]
			ns, typ, data, src := int64(le.Uint64(x)), le.Uint32(x[8:]), int64(le.Uint64(x[12:])), le.Uint32(x[20:])
			switch {
			case typ == 1: // LP_TARGET_TYPE_ZERO
				out = append(out, extent{logical: logical, n: ns * sector, zero: true})
			case typ == 0 && src == 0: // LP_TARGET_TYPE_LINEAR en el propio super
				out = append(out, extent{img: superOff + data*sector, logical: logical, n: ns * sector})
			default:
				return nil, 0, fmt.Errorf("super: extent type %d on block device %d not supported", typ, src)
			}
			logical += ns * sector
		}
		return out, logical, nil
	}
	return nil, 0, fmt.Errorf("super: no dynamic partition %q", name)
}

// writeSparse escribe b en off saltándose los bloques que son todo ceros
// (quedan como hueco: el ext4 system tiene ~0,9 GiB y se usa una parte).
func writeSparse(w *os.File, b []byte, off int64) error {
	for len(b) > 0 {
		k := int(zeroBlock - off%zeroBlock)
		if k > len(b) {
			k = len(b)
		}
		if !allZero(b[:k]) {
			if _, err := w.WriteAt(b[:k], off); err != nil {
				return err
			}
		}
		b, off = b[k:], off+int64(k)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
