package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// Read lee un ext4 entero como árbol. Los ficheros regulares quedan con una
// fuente Extents que lee de r cuando se escriben o se abren, así que r tiene
// que seguir abierto mientras se use el árbol.
//
// Sirve para construir una imagen derivada de otra (la base con los ficheros
// de dm-verity) y para comparar imágenes sin montarlas. Entiende lo que
// escriben mkfs.ext4 y este paquete: extents (a cualquier profundidad),
// directorios lineales o con índice, datos en línea, enlaces rápidos y
// atributos en el inodo o en bloque. No entiende ficheros de mapa de bloques
// de ext2/ext3 (sin extents): da error.
//
// La imagen puede ser hostil (la de un sistema Android que se importa, una
// que se compara): lo que no cuadre da error, sin pánico, y lo que se lee o
// se pide de memoria queda acotado por el tamaño de la imagen.
func Read(r io.ReaderAt) (root *Node, err error) {
	defer func() {
		if p := recover(); p != nil {
			root, err = nil, fmt.Errorf("corrupt ext4 image: %v", p)
		}
	}()
	return read(r)
}

// Topes del lector, además de los que pone el tamaño de la imagen.
const (
	// maxNodosExtent y maxTramos son los nodos de índice y los tramos del
	// árbol de extents de un inodo: un árbol de 5 niveles de 340 índices al
	// mismo bloque eran 340^4 lecturas sobre una imagen de KiB.
	maxNodosExtent = 4096
	maxTramos      = 1 << 20
	// maxEnlace y maxDirectorio son lo que se lee a memoria de un enlace y de
	// un directorio: los huecos se leen como ceros, y uno que decía medir TiB
	// se leía entero.
	maxEnlace     = 4096
	maxDirectorio = 64 << 20
)

func read(r io.ReaderAt) (*Node, error) {
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, 1024); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if le.Uint16(sb[56:]) != 0xEF53 {
		return nil, errors.New("not an ext4 filesystem (bad magic)")
	}
	bs := uint64(1024) << le.Uint32(sb[24:])
	if bs != blockSize {
		return nil, fmt.Errorf("block size %d not supported (only 4096)", bs)
	}
	rd := &reader{r: r,
		ipg:       le.Uint32(sb[40:]),
		isz:       uint32(le.Uint16(sb[88:])),
		incompat:  le.Uint32(sb[96:]),
		inodes:    map[uint32]*Node{},
		groups:    0,
		firstData: uint64(le.Uint32(sb[20:])),
	}
	if rd.isz == 0 {
		rd.isz = 128
	}
	blocks := uint64(le.Uint32(sb[4:]))
	ds := uint32(32)
	if rd.incompat&incompat64bit != 0 {
		blocks |= uint64(le.Uint32(sb[336:])) << 32
		ds = uint32(le.Uint16(sb[254:]))
	}
	bpg := uint64(le.Uint32(sb[32:]))
	// Un superbloque corrupto no debe tumbar el proceso (división por cero) ni
	// pedir memoria sin tope para los descriptores: lo que no cuadre, error.
	switch {
	case rd.ipg == 0:
		return nil, errors.New("corrupt superblock: 0 inodes per group")
	case bpg == 0 || bpg > 8*blockSize:
		return nil, fmt.Errorf("corrupt superblock: %d blocks per group", bpg)
	case blocks <= rd.firstData:
		return nil, fmt.Errorf("corrupt superblock: %d blocks", blocks)
	case rd.isz < 128 || rd.isz > blockSize || rd.isz&(rd.isz-1) != 0:
		return nil, fmt.Errorf("corrupt superblock: inode size %d", rd.isz)
	case ds < 32 || ds > blockSize || ds&(ds-1) != 0:
		return nil, fmt.Errorf("corrupt superblock: group descriptor size %d", ds)
	}
	// Los bloques que declara tienen que estar en la imagen: sin esto, un
	// superbloque de 2^40 bloques pedía GiB para los descriptores de grupo.
	if err := enLaImagen(r, blocks); err != nil {
		return nil, err
	}
	rd.blocks = blocks
	rd.groups = int((blocks - rd.firstData + bpg - 1) / bpg)
	if inodes := uint64(le.Uint32(sb[0:])); uint64(rd.groups)*uint64(rd.ipg) != inodes {
		return nil, fmt.Errorf("corrupt superblock: %d inodes for %d groups of %d", inodes, rd.groups, rd.ipg)
	}
	gdt := make([]byte, uint64(rd.groups)*uint64(ds))
	if _, err := r.ReadAt(gdt, int64((rd.firstData+1)*blockSize)); err != nil {
		return nil, fmt.Errorf("reading group descriptors: %w", err)
	}
	rd.itable = make([]uint64, rd.groups)
	for g := 0; g < rd.groups; g++ {
		d := gdt[uint32(g)*ds:]
		t := uint64(le.Uint32(d[8:]))
		if ds >= 64 {
			t |= uint64(le.Uint32(d[40:])) << 32
		}
		rd.itable[g] = t
	}
	return rd.dir(2, 0)
}

type reader struct {
	r         io.ReaderAt
	ipg, isz  uint32
	incompat  uint32
	groups    int
	blocks    uint64
	firstData uint64
	itable    []uint64
	inodes    map[uint32]*Node
	// nodos y tramos son los nodos de índice y los tramos de extents leídos
	// en toda la imagen: en una sana no pasan de sus bloques (cada uno ocupa
	// o cubre al menos uno), así que lo que se guarda de memoria queda
	// acotado por el tamaño de la imagen aunque muchos inodos repitan el
	// mismo árbol.
	nodos, tramos uint64
}

// enLaImagen comprueba que los blocks bloques que declara el superbloque
// caben en r: con el tamaño si r lo dice, y si no, leyendo el último byte.
func enLaImagen(r io.ReaderAt, blocks uint64) error {
	if blocks > math.MaxInt64/blockSize {
		return fmt.Errorf("corrupt superblock: %d blocks", blocks)
	}
	need := int64(blocks * blockSize)
	var size int64 = -1
	switch v := r.(type) {
	case interface{ Size() int64 }:
		size = v.Size()
	case interface{ Stat() (os.FileInfo, error) }:
		if fi, err := v.Stat(); err == nil && fi.Mode().IsRegular() {
			size = fi.Size()
		}
	}
	if size < 0 {
		var b [1]byte
		if _, err := r.ReadAt(b[:], need-1); err != nil {
			return fmt.Errorf("corrupt superblock: %d blocks, but the image ends before (%v)", blocks, err)
		}
		return nil
	}
	if need > size {
		return fmt.Errorf("corrupt superblock: %d blocks (%d bytes) in an image of %d bytes", blocks, need, size)
	}
	return nil
}

func (rd *reader) inode(ino uint32) ([]byte, error) {
	g := (ino - 1) / rd.ipg
	if int(g) >= rd.groups {
		return nil, fmt.Errorf("inode %d out of range", ino)
	}
	idx := (ino - 1) % rd.ipg
	b := make([]byte, rd.isz)
	_, err := rd.r.ReadAt(b, int64(rd.itable[g]*blockSize)+int64(idx)*int64(rd.isz))
	return b, err
}

func decTime(lo, extra uint32, hasExtra bool) time.Time {
	s := int64(int32(lo))
	if !hasExtra {
		return time.Unix(s, 0)
	}
	s += int64(extra&3) << 32
	return time.Unix(s, int64(extra>>2))
}

// node lee un inodo y lo convierte (sin hijos; dir los añade).
func (rd *reader) node(ino uint32) (*Node, []byte, error) {
	b, err := rd.inode(ino)
	if err != nil {
		return nil, nil, err
	}
	le := binary.LittleEndian
	n := &Node{
		Mode: uint32(le.Uint16(b[0:])),
		UID:  uint32(le.Uint16(b[2:])) | uint32(le.Uint16(b[120:]))<<16,
		GID:  uint32(le.Uint16(b[24:])) | uint32(le.Uint16(b[122:]))<<16,
	}
	extra := 0
	if rd.isz > 128 {
		extra = int(le.Uint16(b[128:]))
	}
	// Los campos extra solo si caben en el inodo: con s_inode_size = 128 no
	// hay ninguno (se leían igual, fuera del inodo: pánico).
	if 128+extra > len(b) {
		return nil, nil, fmt.Errorf("inode %d: %d extra bytes in a %d-byte inode", ino, extra, len(b))
	}
	has := func(off int) bool { return extra >= off-128+4 }
	extraU32 := func(off int) uint32 {
		if !has(off) {
			return 0
		}
		return le.Uint32(b[off:])
	}
	n.Atime = decTime(le.Uint32(b[8:]), extraU32(140), has(140))
	n.Ctime = decTime(le.Uint32(b[12:]), extraU32(132), has(132))
	n.Mtime = decTime(le.Uint32(b[16:]), extraU32(136), has(136))
	n.Size = int64(uint64(le.Uint32(b[4:])) | uint64(le.Uint32(b[108:]))<<32)
	if n.Size < 0 {
		return nil, nil, fmt.Errorf("inode %d: negative size", ino)
	}
	xs := map[string][]byte{}
	if extra > 0 && 128+extra+4 <= len(b) && le.Uint32(b[128+extra:]) == xattrMagic {
		if err := parseXattrs(b[128+extra+4:], 0, 0, xs); err != nil {
			return nil, nil, fmt.Errorf("inode %d: %w", ino, err)
		}
	}
	if acl := uint64(le.Uint32(b[104:])) | uint64(le.Uint16(b[118:]))<<32; acl != 0 {
		blk := make([]byte, blockSize)
		if _, err := rd.r.ReadAt(blk, int64(acl*blockSize)); err != nil {
			return nil, nil, err
		}
		if le.Uint32(blk) != xattrMagic {
			return nil, nil, fmt.Errorf("inode %d: bad xattr block", ino)
		}
		if err := parseXattrs(blk, 32, 0, xs); err != nil {
			return nil, nil, fmt.Errorf("inode %d: %w", ino, err)
		}
	}
	inline := xs["system.data"]
	delete(xs, "system.data")
	if len(xs) > 0 {
		n.Xattrs = xs
	}
	flags := le.Uint32(b[32:])
	iblock := b[40:100]
	switch n.Mode & ModeType {
	case ModeReg:
		if flags&flagInlineData != 0 {
			d := append(append([]byte{}, iblock...), inline...)
			if int64(len(d)) < n.Size {
				return nil, nil, fmt.Errorf("inode %d: short inline data", ino)
			}
			n.Data = Bytes(d[:n.Size])
		} else {
			runs, err := rd.extents(iblock, flags)
			if err != nil {
				return nil, nil, fmt.Errorf("inode %d: %w", ino, err)
			}
			n.Data = &Extents{r: rd.r, runs: runs}
		}
	case ModeLink:
		if n.Size > maxEnlace {
			return nil, nil, fmt.Errorf("inode %d: symlink of %d bytes", ino, n.Size)
		}
		switch {
		case flags&flagInlineData != 0:
			d := append(append([]byte{}, iblock...), inline...)
			if int64(len(d)) < n.Size {
				return nil, nil, fmt.Errorf("inode %d: short inline data", ino)
			}
			n.Target = string(d[:n.Size])
		case flags&flagExtents == 0 && n.Size < 60:
			n.Target = string(iblock[:n.Size])
		default:
			d, err := rd.leerEntero(ino, iblock, flags, n.Size)
			if err != nil {
				return nil, nil, err
			}
			n.Target = string(d)
		}
		n.Size = 0
	case ModeChar, ModeBlock:
		if v := le.Uint32(iblock[0:]); v != 0 {
			n.Major, n.Minor = v>>8&0xff, v&0xff
		} else {
			v := le.Uint32(iblock[4:])
			n.Major = v >> 8 & 0xfff
			n.Minor = v&0xff | v>>12&0xfff00
		}
		n.Size = 0
	case ModeFIFO, ModeSocket:
		n.Size = 0
	}
	if n.IsDir() {
		var data []byte
		if flags&flagInlineData != 0 {
			data = append(append([]byte{}, iblock...), inline...)
		} else {
			if n.Size > maxDirectorio {
				return nil, nil, fmt.Errorf("directory inode %d: %d bytes", ino, n.Size)
			}
			data, err = rd.leerEntero(ino, iblock, flags, n.Size)
			if err != nil {
				return nil, nil, err
			}
		}
		n.Size = 0
		return n, data, nil
	}
	return n, nil, nil
}

// leerEntero lee a memoria los datos de un enlace o un directorio (size
// bytes, ya acotado por quien llama), que no pueden pasar de lo que cubren
// sus extents: lo de más serían ceros de un hueco.
func (rd *reader) leerEntero(ino uint32, iblock []byte, flags uint32, size int64) ([]byte, error) {
	runs, err := rd.extents(iblock, flags)
	if err != nil {
		return nil, fmt.Errorf("inode %d: %w", ino, err)
	}
	var fin uint64
	for _, r := range runs {
		fin = max(fin, r.logical+r.n)
	}
	if uint64(size) > fin*blockSize {
		return nil, fmt.Errorf("inode %d: %d bytes, but its extents cover %d", ino, size, fin*blockSize)
	}
	return (&Node{Size: size, Data: &Extents{r: rd.r, runs: runs}}).ReadAll()
}

func parseXattrs(region []byte, entryOff, base int, out map[string][]byte) error {
	le := binary.LittleEndian
	pos := entryOff
	for pos+16 <= len(region) && le.Uint32(region[pos:]) != 0 {
		nl := int(region[pos])
		idx := region[pos+1]
		voff := int(le.Uint16(region[pos+2:]))
		vinum := le.Uint32(region[pos+4:])
		vsize := int(le.Uint32(region[pos+8:]))
		if pos+16+nl > len(region) {
			return errors.New("truncated xattr entry")
		}
		name := string(region[pos+16 : pos+16+nl])
		if vinum != 0 {
			return errors.New("xattr values in inodes (ea_inode) not supported")
		}
		if base+voff+vsize > len(region) {
			return errors.New("xattr value out of bounds")
		}
		full, ok := xattrName(idx, name)
		if !ok {
			return fmt.Errorf("unknown xattr index %d", idx)
		}
		v := append([]byte{}, region[base+voff:base+voff+vsize]...)
		if idx == 2 || idx == 3 {
			if a, err := aclFromDisk(v); err == nil {
				v = a
			}
		}
		out[full] = v
		pos += pad4(16 + nl)
	}
	return nil
}

func (rd *reader) extents(iblock []byte, flags uint32) ([]run, error) {
	if flags&flagExtents == 0 {
		return nil, errors.New("block-mapped (non-extent) files are not supported")
	}
	var runs []run
	var nodos int
	var walk func(b []byte, depth int) error
	walk = func(b []byte, lvl int) error {
		le := binary.LittleEndian
		if le.Uint16(b) != 0xF30A {
			return errors.New("bad extent header")
		}
		n := int(le.Uint16(b[2:]))
		depth := int(le.Uint16(b[6:]))
		if 12+12*n > len(b) || lvl > 5 {
			return errors.New("corrupt extent node")
		}
		if depth == 0 {
			rd.tramos += uint64(n)
			if len(runs)+n > maxTramos || rd.tramos > rd.blocks {
				return errors.New("too many extents")
			}
		}
		for i := 0; i < n; i++ {
			e := b[12+12*i:]
			if depth == 0 {
				l := uint64(le.Uint16(e[4:]))
				phys := uint64(le.Uint16(e[6:]))<<32 | uint64(le.Uint32(e[8:]))
				if l > maxExtentLen {
					l -= maxExtentLen // sin inicializar: se lee como ceros
					phys = 0
				}
				runs = append(runs, run{uint64(le.Uint32(e[0:])), phys, l})
				continue
			}
			nodos++
			rd.nodos++
			if nodos > maxNodosExtent || rd.nodos > rd.blocks {
				return errors.New("too many extent index nodes")
			}
			leaf := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
			blk := make([]byte, blockSize)
			if _, err := rd.r.ReadAt(blk, int64(leaf*blockSize)); err != nil {
				return err
			}
			if err := walk(blk, lvl+1); err != nil {
				return err
			}
		}
		return nil
	}
	return runs, walk(iblock, 0)
}

func (rd *reader) dir(ino uint32, depth int) (*Node, error) {
	if depth > 256 {
		return nil, errors.New("directory tree too deep")
	}
	n, data, err := rd.node(ino)
	if err != nil {
		return nil, err
	}
	if !n.IsDir() {
		return nil, fmt.Errorf("inode %d is not a directory", ino)
	}
	n.children = map[string]*Node{}
	le := binary.LittleEndian
	inlineDir := len(data) > 0 && len(data)%blockSize != 0
	pos := 0
	if inlineDir {
		// Directorio en línea: 4 bytes con el inodo del padre y entradas.
		pos = 4
	}
	for pos+8 <= len(data) {
		cino := le.Uint32(data[pos:])
		rec := int(le.Uint16(data[pos+4:]))
		nl := int(data[pos+6])
		if rec < 8 || pos+rec > len(data) || 8+nl > rec {
			return nil, fmt.Errorf("directory inode %d: corrupt entry", ino)
		}
		name := string(data[pos+8 : pos+8+nl])
		pos += rec
		if cino == 0 || name == "." || name == ".." {
			continue
		}
		if c, ok := rd.inodes[cino]; ok {
			if c.IsDir() {
				return nil, fmt.Errorf("directory inode %d linked twice", cino)
			}
			n.children[name] = c
			continue
		}
		cn, _, err := rd.node(cino)
		if err != nil {
			return nil, err
		}
		if cn.IsDir() {
			cn, err = rd.dir(cino, depth+1)
			if err != nil {
				return nil, err
			}
		}
		rd.inodes[cino] = cn
		n.children[name] = cn
	}
	return n, nil
}
