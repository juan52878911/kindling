package guest

// El protocolo FUSE del kernel de Linux, a mano (include/uapi/linux/fuse.h).
//
// Sin libfuse ni cgo: el agente es un binario estático que corre como PID 1, y
// la parte del protocolo que hace falta —una veintena de operaciones con
// estructuras de tamaño fijo— cabe en este fichero. Es código portable: se
// compila y se prueba en cualquier sistema contra un /dev/fuse falso; solo el
// montaje (fuse_linux.go) es de Linux.
//
// Todo en little-endian: FUSE usa el orden del host, y los invitados de
// kindling son amd64 o arm64.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var le = binary.LittleEndian

// Versión del protocolo que hablamos. 7.31 es lo que tiene el kernel 6.1 del
// invitado de sobra (llega a 7.37) y fija el tamaño de fuse_init_out en 64
// bytes y el de las estructuras de petición que usamos.
const (
	fuseMajor = 7
	fuseMinor = 31
	// Por debajo de 7.12 cambian create_in y mknod_in; ningún kernel que
	// arranque kindling es tan viejo, y se rechaza en vez de adivinar.
	fuseMinMinor = 12
)

// Opcodes.
const (
	opLookup      = 1
	opForget      = 2
	opGetattr     = 3
	opSetattr     = 4
	opReadlink    = 5
	opSymlink     = 6
	opMknod       = 8
	opMkdir       = 9
	opUnlink      = 10
	opRmdir       = 11
	opRename      = 12
	opLink        = 13
	opOpen        = 14
	opRead        = 15
	opWrite       = 16
	opStatfs      = 17
	opRelease     = 18
	opFsync       = 19
	opFlush       = 25
	opInit        = 26
	opOpendir     = 27
	opReaddir     = 28
	opReleasedir  = 29
	opFsyncdir    = 30
	opAccess      = 34
	opCreate      = 35
	opInterrupt   = 36
	opDestroy     = 38
	opBatchForget = 42
	opRename2     = 45
)

// Flags de INIT que pedimos (y solo si el kernel los ofrece).
const (
	initAsyncRead     = 1 << 0
	initAtomicOTrunc  = 1 << 3
	initBigWrites     = 1 << 5
	initAutoInvalData = 1 << 12
	initParallelDirop = 1 << 18
	initMaxPages      = 1 << 22
)

// Bits de fuse_setattr_in.valid.
const (
	fattrMode     = 1 << 0
	fattrUID      = 1 << 1
	fattrGID      = 1 << 2
	fattrSize     = 1 << 3
	fattrAtime    = 1 << 4
	fattrMtime    = 1 << 5
	fattrFH       = 1 << 6
	fattrAtimeNow = 1 << 7
	fattrMtimeNow = 1 << 8
	fattrCtime    = 1 << 10
)

// FUSE_GETATTR_FH: getattr_in lleva un handle válido.
const getattrFH = 1

// Tamaños de las estructuras (con minor >= 12, que es lo que exigimos).
const (
	inHeaderSize   = 40
	outHeaderSize  = 16
	attrSize       = 88
	entryOutSize   = 40 + attrSize
	attrOutSize    = 16 + attrSize
	openOutSize    = 16
	initOutSize    = 64
	kstatfsSize    = 80
	writeOutSize   = 8
	direntBaseSize = 24
)

// Flags de open del kernel de Linux (los del invitado: son fijos, este código
// corre dentro de Linux aunque se pruebe fuera).
const (
	linuxOAccMode = 0o3
	linuxORdonly  = 0o0
	linuxOWronly  = 0o1
	linuxORdwr    = 0o2
	linuxOCreat   = 0o100
	linuxOExcl    = 0o200
	linuxOTrunc   = 0o1000
	linuxOAppend  = 0o2000
)

// fuseReq es una petición del kernel ya separada en cabecera y argumentos.
type fuseReq struct {
	opcode uint32
	unique uint64
	nodeid uint64
	uid    uint32
	gid    uint32
	pid    uint32
	arg    []byte
}

var errBadMsg = errors.New("malformed FUSE message")

// parseReq valida la cabecera contra el tamaño real del mensaje. Lo que diga
// len y lo que se leyó tienen que coincidir: si no, el resto de campos no
// significan nada.
func parseReq(b []byte) (fuseReq, error) {
	if len(b) < inHeaderSize {
		return fuseReq{}, errBadMsg
	}
	n := le.Uint32(b[0:])
	if int(n) != len(b) {
		return fuseReq{}, fmt.Errorf("%w: header says %d bytes, got %d", errBadMsg, n, len(b))
	}
	return fuseReq{
		opcode: le.Uint32(b[4:]),
		unique: le.Uint64(b[8:]),
		nodeid: le.Uint64(b[16:]),
		uid:    le.Uint32(b[24:]),
		gid:    le.Uint32(b[28:]),
		pid:    le.Uint32(b[32:]),
		arg:    b[inHeaderSize:],
	}, nil
}

// reply construye la respuesta a una petición. errno es positivo (el de
// Linux); el kernel lo espera negado en la cabecera.
func reply(unique uint64, errno uint32, payload ...[]byte) []byte {
	n := outHeaderSize
	for _, p := range payload {
		n += len(p)
	}
	b := make([]byte, outHeaderSize, n)
	le.PutUint32(b[0:], uint32(n))
	le.PutUint32(b[4:], uint32(-int32(errno)))
	le.PutUint64(b[8:], unique)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

// cstring lee un nombre terminado en NUL al principio de b y devuelve el resto.
func cstring(b []byte) (string, []byte, error) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], nil
		}
	}
	return "", nil, errBadMsg
}

// fattr son los atributos en el formato de fuse_attr.
type fattr struct {
	ino                          uint64
	size, blocks                 uint64
	atime, mtime, ctime          uint64
	atimensec, mtimensec, ctimns uint32
	mode, nlink, uid, gid, rdev  uint32
	blksize                      uint32
}

func (a fattr) put(b []byte) {
	le.PutUint64(b[0:], a.ino)
	le.PutUint64(b[8:], a.size)
	le.PutUint64(b[16:], a.blocks)
	le.PutUint64(b[24:], a.atime)
	le.PutUint64(b[32:], a.mtime)
	le.PutUint64(b[40:], a.ctime)
	le.PutUint32(b[48:], a.atimensec)
	le.PutUint32(b[52:], a.mtimensec)
	le.PutUint32(b[56:], a.ctimns)
	le.PutUint32(b[60:], a.mode)
	le.PutUint32(b[64:], a.nlink)
	le.PutUint32(b[68:], a.uid)
	le.PutUint32(b[72:], a.gid)
	le.PutUint32(b[76:], a.rdev)
	le.PutUint32(b[80:], a.blksize)
	le.PutUint32(b[84:], 0) // flags
}

// Caducidad de las cachés del kernel: un cambio hecho en el host se ve en como
// mucho este tiempo.
const cacheSeconds = 1

func entryOut(nodeid uint64, a fattr) []byte {
	b := make([]byte, entryOutSize)
	le.PutUint64(b[0:], nodeid)
	le.PutUint64(b[8:], 0) // generation: los ids no se reutilizan nunca
	le.PutUint64(b[16:], cacheSeconds)
	le.PutUint64(b[24:], cacheSeconds)
	a.put(b[40:])
	return b
}

func attrOut(a fattr) []byte {
	b := make([]byte, attrOutSize)
	le.PutUint64(b[0:], cacheSeconds)
	a.put(b[16:])
	return b
}

func openOut(fh uint64) []byte {
	b := make([]byte, openOutSize)
	le.PutUint64(b[0:], fh)
	return b
}

// dirent añade una entrada de directorio a buf si cabe en max. Devuelve false
// si no cabía.
func appendDirent(buf []byte, max int, ino, off uint64, typ uint32, name string) ([]byte, bool) {
	size := direntBaseSize + len(name)
	padded := (size + 7) &^ 7
	if len(buf)+padded > max {
		return buf, false
	}
	var h [direntBaseSize]byte
	le.PutUint64(h[0:], ino)
	le.PutUint64(h[8:], off)
	le.PutUint32(h[16:], uint32(len(name)))
	le.PutUint32(h[20:], typ)
	buf = append(buf, h[:]...)
	buf = append(buf, name...)
	for i := size; i < padded; i++ {
		buf = append(buf, 0)
	}
	return buf, true
}
