package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
func Read(r io.ReaderAt) (*Node, error) {
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
	rd.groups = int((blocks - rd.firstData + bpg - 1) / bpg)
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
	firstData uint64
	itable    []uint64
	inodes    map[uint32]*Node
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
	has := func(off int) bool { return extra >= off-128+4 }
	n.Atime = decTime(le.Uint32(b[8:]), le.Uint32(b[140:]), has(140))
	n.Ctime = decTime(le.Uint32(b[12:]), le.Uint32(b[132:]), has(132))
	n.Mtime = decTime(le.Uint32(b[16:]), le.Uint32(b[136:]), has(136))
	n.Size = int64(uint64(le.Uint32(b[4:])) | uint64(le.Uint32(b[108:]))<<32)
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
		switch {
		case flags&flagInlineData != 0:
			d := append(append([]byte{}, iblock...), inline...)
			n.Target = string(d[:n.Size])
		case flags&flagExtents == 0 && n.Size < 60:
			n.Target = string(iblock[:n.Size])
		default:
			runs, err := rd.extents(iblock, flags)
			if err != nil {
				return nil, nil, fmt.Errorf("inode %d: %w", ino, err)
			}
			d, err := (&Node{Size: n.Size, Data: &Extents{r: rd.r, runs: runs}}).ReadAll()
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
			runs, err := rd.extents(iblock, flags)
			if err != nil {
				return nil, nil, fmt.Errorf("inode %d: %w", ino, err)
			}
			data, err = (&Node{Size: n.Size, Data: &Extents{r: rd.r, runs: runs}}).ReadAll()
			if err != nil {
				return nil, nil, err
			}
		}
		n.Size = 0
		return n, data, nil
	}
	return n, nil, nil
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
