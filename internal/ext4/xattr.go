package ext4

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// Atributos extendidos en el formato de ext4: en el propio inodo si caben
// (96 bytes tras los campos extra) y si no en un bloque aparte.

type xattrEntry struct {
	index byte
	name  string
	value []byte
}

var xattrPrefixes = []struct {
	prefix string
	index  byte
	exact  bool
}{
	{"system.posix_acl_access", 2, true},
	{"system.posix_acl_default", 3, true},
	{"system.richacl", 8, true},
	{"user.", 1, false},
	{"trusted.", 4, false},
	{"security.", 6, false},
	{"system.", 7, false},
}

func encodeXattrs(m map[string][]byte) ([]xattrEntry, error) {
	var out []xattrEntry
	for full, v := range m {
		var e *xattrEntry
		for _, p := range xattrPrefixes {
			if (p.exact && full == p.prefix) || (!p.exact && strings.HasPrefix(full, p.prefix)) {
				name := ""
				if !p.exact {
					name = full[len(p.prefix):]
				}
				e = &xattrEntry{index: p.index, name: name, value: v}
				break
			}
		}
		if e == nil {
			return nil, fmt.Errorf("unsupported xattr namespace %q", full)
		}
		if len(e.name) > 255 {
			return nil, fmt.Errorf("xattr name too long: %q", full)
		}
		if e.index == 2 || e.index == 3 {
			acl, err := aclToDisk(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", full, err)
			}
			e.value = acl
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.index != b.index {
			return a.index < b.index
		}
		if len(a.name) != len(b.name) {
			return len(a.name) < len(b.name)
		}
		return a.name < b.name
	})
	return out, nil
}

func pad4(n int) int { return (n + 3) &^ 3 }

func entriesSize(xs []xattrEntry) (entries, values int) {
	for _, x := range xs {
		entries += pad4(16 + len(x.name))
		values += pad4(len(x.value))
	}
	return entries + 4, values // + el terminador
}

// Espacio del inodo para atributos: 256 - 128 - extra_isize, menos la magia.
const inodeXattrSpace = inodeSize - 128 - extraIsize - 4

func fitsInInode(xs []xattrEntry) bool {
	e, v := entriesSize(xs)
	return e+v <= inodeXattrSpace
}

func xattrBlockSize(xs []xattrEntry) int {
	e, v := entriesSize(xs)
	return 32 + e + v
}

func xattrHash(x xattrEntry) uint32 {
	var h uint32
	for i := 0; i < len(x.name); i++ {
		h = h<<5 ^ h>>27 ^ uint32(x.name[i])
	}
	v := make([]byte, pad4(len(x.value)))
	copy(v, x.value)
	for i := 0; i < len(v); i += 4 {
		h = h<<16 ^ h>>16 ^ binary.LittleEndian.Uint32(v[i:])
	}
	return h
}

// putXattrs escribe entradas y valores en region; los desplazamientos de los
// valores se cuentan desde base (el primer byte de las entradas en el inodo,
// el principio del bloque en un bloque).
func putXattrs(region []byte, entryOff, base int, xs []xattrEntry) {
	le := binary.LittleEndian
	vend := len(region)
	pos := entryOff
	for _, x := range xs {
		vs := pad4(len(x.value))
		vend -= vs
		copy(region[vend:], x.value)
		region[pos] = byte(len(x.name))
		region[pos+1] = x.index
		le.PutUint16(region[pos+2:], uint16(vend-base))
		le.PutUint32(region[pos+4:], 0)
		le.PutUint32(region[pos+8:], uint32(len(x.value)))
		le.PutUint32(region[pos+12:], xattrHash(x))
		copy(region[pos+16:], x.name)
		pos += pad4(16 + len(x.name))
	}
}

// putInodeXattrs escribe la magia y los atributos detrás de los campos extra.
func putInodeXattrs(b []byte, xs []xattrEntry) {
	binary.LittleEndian.PutUint32(b, xattrMagic)
	putXattrs(b[4:], 0, 0, xs)
}

func xattrBlock(xs []xattrEntry) []byte {
	b := make([]byte, blockSize)
	le := binary.LittleEndian
	le.PutUint32(b[0:], xattrMagic)
	le.PutUint32(b[4:], 1) // refcount
	le.PutUint32(b[8:], 1) // blocks
	putXattrs(b, 32, 0, xs)
	var h uint32
	for _, x := range xs {
		eh := xattrHash(x)
		if eh == 0 {
			h = 0
			break
		}
		h = h<<16 ^ h>>16 ^ eh
	}
	le.PutUint32(b[12:], h)
	return b
}

// aclToDisk pasa una ACL del formato de xattr del VFS (el de tar: versión 2,
// entradas de 8 bytes) al de disco de ext4 (versión 1, entradas cortas para
// las que no llevan id).
func aclToDisk(v []byte) ([]byte, error) {
	le := binary.LittleEndian
	if len(v) < 4 || le.Uint32(v) != 2 || (len(v)-4)%8 != 0 {
		return nil, fmt.Errorf("not a POSIX ACL in xattr format")
	}
	out := make([]byte, 4)
	le.PutUint32(out, 1)
	for p := 4; p < len(v); p += 8 {
		tag := le.Uint16(v[p:])
		perm := le.Uint16(v[p+2:])
		id := le.Uint32(v[p+4:])
		switch tag {
		case 0x01, 0x04, 0x10, 0x20: // USER_OBJ, GROUP_OBJ, MASK, OTHER
			e := make([]byte, 4)
			le.PutUint16(e, tag)
			le.PutUint16(e[2:], perm)
			out = append(out, e...)
		case 0x02, 0x08: // USER, GROUP
			e := make([]byte, 8)
			le.PutUint16(e, tag)
			le.PutUint16(e[2:], perm)
			le.PutUint32(e[4:], id)
			out = append(out, e...)
		default:
			return nil, fmt.Errorf("unknown ACL tag %#x", tag)
		}
	}
	return out, nil
}

// aclFromDisk es la inversa, para leer una imagen.
func aclFromDisk(v []byte) ([]byte, error) {
	le := binary.LittleEndian
	if len(v) < 4 || le.Uint32(v) != 1 {
		return nil, fmt.Errorf("not an ext4 ACL")
	}
	out := make([]byte, 4)
	le.PutUint32(out, 2)
	for p := 4; p < len(v); {
		if p+4 > len(v) {
			return nil, fmt.Errorf("truncated ACL")
		}
		tag := le.Uint16(v[p:])
		perm := le.Uint16(v[p+2:])
		id := uint32(0xFFFFFFFF)
		switch tag {
		case 0x02, 0x08:
			if p+8 > len(v) {
				return nil, fmt.Errorf("truncated ACL")
			}
			id = le.Uint32(v[p+4:])
			p += 8
		default:
			p += 4
		}
		e := make([]byte, 8)
		le.PutUint16(e, tag)
		le.PutUint16(e[2:], perm)
		le.PutUint32(e[4:], id)
		out = append(out, e...)
	}
	return out, nil
}

func xattrName(index byte, name string) (string, bool) {
	for _, p := range xattrPrefixes {
		if p.index == index {
			if p.exact {
				return p.prefix, true
			}
			return p.prefix + name, true
		}
	}
	return "", false
}
