// Package share es el protocolo entre el agente de invitado y el daemon para
// las carpetas compartidas en vivo (modos ro y rw). Ver docs/compartir.md.
//
// Lo importan los dos lados: el agente (pkg/guest), que traduce el protocolo
// FUSE del kernel a estas operaciones por ruta, y el daemon (internal/share),
// que las sirve desde el directorio del host. Por eso no depende de nada más
// que de la biblioteca estándar: el agente se compila estático para el invitado.
package share

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Proto es el nombre del protocolo en la cabecera Upgrade. La versión va dentro:
// un daemon nuevo contra un agente viejo recibe 404 o 426 y un mensaje claro,
// en vez de hablar dos idiomas por el mismo socket.
const Proto = "kling-share/1"

// AttachPath es la ruta del agente a la que el daemon hace el attach.
const AttachPath = "/share/attach"

// HeaderAgent lo pone el agente en TODAS sus respuestas a AttachPath, también
// en las de error. Es como anuncia que sabe servir carpetas: un agente anterior
// no conoce la ruta, y según quién la sirva (kling-guest da 404; el puente MCP,
// que se queda con todo lo que no conoce, un 400 propio) el código no basta.
const HeaderAgent = "X-Kling-Share"

// Modos de una carpeta compartida.
const (
	ModeCopy = "copy" // copia de solo lectura en un ext4 (un disco más)
	ModeRO   = "ro"   // directorio vivo, solo lectura
	ModeRW   = "rw"   // directorio vivo, lectura y escritura
)

// MaxShares es el máximo de carpetas compartidas por máquina. Las vivas se
// identifican por su posición (tag), y las de copia son un disco cada una.
const MaxShares = 8

// Límites del protocolo. Son contrato: los dos lados los comprueban.
const (
	// MaxIO es lo más que se lee o escribe en una operación. Coincide con el
	// max_write que el agente negocia con el kernel.
	MaxIO = 128 << 10
	// MaxFrame es la trama más grande: una escritura máxima con su ruta y su
	// cabecera, con margen.
	MaxFrame = MaxIO + 16<<10
	// MaxPath y MaxName son los de Linux (PATH_MAX, NAME_MAX).
	MaxPath = 4096
	MaxName = 255
)

// Attach es el cuerpo de POST /share/attach: qué carpeta se sirve por esta
// conexión y dónde debe estar montada dentro del invitado.
type Attach struct {
	Tag   int    `json:"tag"`
	Mount string `json:"mount"`
	Mode  string `json:"mode"`
}

// Validate comprueba un Attach. Lo usan los dos lados: el daemon antes de
// mandarlo y el agente antes de montar nada.
func (a Attach) Validate() error {
	if a.Tag < 0 || a.Tag >= MaxShares {
		return fmt.Errorf("share tag %d out of range (0-%d)", a.Tag, MaxShares-1)
	}
	if a.Mode != ModeRO && a.Mode != ModeRW {
		return fmt.Errorf("invalid live share mode %q: use %q or %q", a.Mode, ModeRO, ModeRW)
	}
	return ValidMount(a.Mount)
}

// reservedMounts son los sitios donde una carpeta compartida taparía algo que el
// invitado necesita para funcionar.
var reservedMounts = []string{"/proc", "/sys", "/dev", "/run", "/bin", "/sbin", "/lib", "/usr", "/etc"}

// ValidMount comprueba un punto de montaje dentro del invitado.
//
// Las mismas reglas que un volumen (viaja por la línea de comandos del kernel en
// el modo copy, separado por comas) y además nada de tapar el sistema: montar
// encima de /usr o /etc dejaría al invitado sin sus binarios.
func ValidMount(mp string) error {
	if !strings.HasPrefix(mp, "/") || strings.ContainsAny(mp, " \t\n\"',:\x00") || len(mp) > 512 {
		return fmt.Errorf("invalid mount point %q: absolute path, no spaces, commas or colons", mp)
	}
	clean := cleanAbs(mp)
	if clean != mp {
		return fmt.Errorf("invalid mount point %q: write it clean (%s)", mp, clean)
	}
	if mp == "/" {
		return errors.New("a share cannot be mounted at /")
	}
	for _, r := range reservedMounts {
		if mp == r || strings.HasPrefix(mp, r+"/") {
			return fmt.Errorf("a share cannot be mounted at %s: it would hide %s from the guest", mp, r)
		}
	}
	return nil
}

// cleanAbs limpia una ruta absoluta sin importar path (lo hace a mano para que
// el criterio sea exactamente el de ValidPath: nada de "." ni "..").
func cleanAbs(p string) string {
	parts := strings.Split(p, "/")
	var out []string
	for _, c := range parts {
		switch c {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, c)
		}
	}
	return "/" + strings.Join(out, "/")
}

// ValidPath comprueba una ruta del protocolo: relativa a la carpeta, ya limpia.
// "" es la raíz. No se limpia aquí: una ruta que llega sucia viene de alguien
// que no sigue el protocolo, y eso se rechaza en vez de interpretarse.
func ValidPath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > MaxPath {
		return errors.New("path too long")
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("path %q is not relative and clean", p)
	}
	for _, c := range strings.Split(p, "/") {
		if err := ValidName(c); err != nil {
			return err
		}
	}
	return nil
}

// ValidName comprueba un componente de ruta.
func ValidName(n string) error {
	switch {
	case n == "" || n == "." || n == "..":
		return fmt.Errorf("invalid path component %q", n)
	case len(n) > MaxName:
		return errors.New("path component too long")
	case strings.ContainsAny(n, "/\x00"):
		return fmt.Errorf("invalid path component %q", n)
	}
	return nil
}

// Join une una ruta del protocolo y un nombre.
func Join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// Operaciones. Solo se añaden números; nunca se reutilizan.
const (
	OpStat     byte = 1  // path -> Attr
	OpFstat    byte = 2  // handle -> Attr
	OpList     byte = 3  // path, cookie u64, max u32 -> eof u8, n u32, n×(name, mode u32), next u64
	OpOpen     byte = 4  // path, flags u32 -> handle u64, Attr
	OpCreate   byte = 5  // path, flags u32, mode u32 -> handle u64, Attr
	OpRead     byte = 6  // handle, off u64, size u32 -> bytes
	OpWrite    byte = 7  // handle, off u64, bytes -> n u32
	OpRelease  byte = 8  // handle
	OpFsync    byte = 9  // handle
	OpSetattr  byte = 10 // path, handle u64, valid u32, mode u32, size u64, atime i64, mtime i64 -> Attr
	OpMkdir    byte = 11 // path, mode u32 -> Attr
	OpUnlink   byte = 12 // path
	OpRmdir    byte = 13 // path
	OpRename   byte = 14 // old, new, flags u32
	OpReadlink byte = 15 // path -> target
	OpStatfs   byte = 16 // -> Statfs
)

// Flags de apertura del protocolo. Propios, y no los O_* del sistema: los de
// Linux (el invitado) y los de macOS (un daemon con kling-vz) no coinciden.
const (
	FlagRead   uint32 = 1 << 0
	FlagWrite  uint32 = 1 << 1
	FlagTrunc  uint32 = 1 << 2
	FlagExcl   uint32 = 1 << 3
	FlagAppend uint32 = 1 << 4
)

// Bits de SETATTR.
const (
	SetMode     uint32 = 1 << 0
	SetSize     uint32 = 1 << 1
	SetAtime    uint32 = 1 << 2
	SetMtime    uint32 = 1 << 3
	SetAtimeNow uint32 = 1 << 4
	SetMtimeNow uint32 = 1 << 5
)

// RenameNoReplace es el único flag de rename que se admite.
const RenameNoReplace uint32 = 1

// Tipos de fichero, con los valores de Linux (S_IFMT…): viajan al kernel del
// invitado tal cual.
const (
	SIFMT  uint32 = 0o170000
	SIFDIR uint32 = 0o040000
	SIFREG uint32 = 0o100000
	SIFLNK uint32 = 0o120000
)

// Errno de Linux. El daemon contesta SIEMPRE con estos números, corra en Linux
// o en macOS: van directos al kernel del invitado.
const (
	EPERM        uint32 = 1
	ENOENT       uint32 = 2
	EIO          uint32 = 5
	EBADF        uint32 = 9
	EAGAIN       uint32 = 11
	EACCES       uint32 = 13
	EBUSY        uint32 = 16
	EEXIST       uint32 = 17
	EXDEV        uint32 = 18
	ENOTDIR      uint32 = 20
	EISDIR       uint32 = 21
	EINVAL       uint32 = 22
	EMFILE       uint32 = 24
	EFBIG        uint32 = 27
	ENOSPC       uint32 = 28
	EROFS        uint32 = 30
	ERANGE       uint32 = 34
	ENAMETOOLONG uint32 = 36
	ENOSYS       uint32 = 38
	ENOTEMPTY    uint32 = 39
	ELOOP        uint32 = 40
	EOPNOTSUPP   uint32 = 95
	ESTALE       uint32 = 116
)

// Attr son los atributos de un fichero tal como los ve el invitado.
type Attr struct {
	Mode   uint32 // tipo (SIF*) y permisos, ya recortados a 0777
	Size   uint64
	Blocks uint64 // de 512 bytes
	Nlink  uint32
	Atime  int64
	Atimen uint32
	Mtime  int64
	Mtimen uint32
	Ctime  int64
	Ctimen uint32
}

// Put escribe a en e.
func (a Attr) Put(e *Enc) {
	e.U32(a.Mode)
	e.U64(a.Size)
	e.U64(a.Blocks)
	e.U32(a.Nlink)
	e.I64(a.Atime)
	e.U32(a.Atimen)
	e.I64(a.Mtime)
	e.U32(a.Mtimen)
	e.I64(a.Ctime)
	e.U32(a.Ctimen)
}

// GetAttr lee un Attr.
func GetAttr(d *Dec) Attr {
	return Attr{Mode: d.U32(), Size: d.U64(), Blocks: d.U64(), Nlink: d.U32(),
		Atime: d.I64(), Atimen: d.U32(), Mtime: d.I64(), Mtimen: d.U32(),
		Ctime: d.I64(), Ctimen: d.U32()}
}

// Statfs es lo que contesta OpStatfs.
type Statfs struct {
	Blocks, Bfree, Bavail, Files, Ffree uint64
	Bsize, Namelen                      uint32
}

// ── tramas ──────────────────────────────────────────────────────────────────

// WriteFrame escribe una trama. No es concurrente: quien escriba desde varias
// goroutines tiene que serializar.
func WriteFrame(w io.Writer, body []byte) error {
	if len(body) > MaxFrame {
		return fmt.Errorf("frame of %d bytes; the limit is %d", len(body), MaxFrame)
	}
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(body)))
	// Una sola escritura: cabecera y cuerpo juntos no se entrelazan con nada
	// aunque el que llama se equivoque y no serialice del todo.
	buf := make([]byte, 0, 4+len(body))
	buf = append(append(buf, head[:]...), body...)
	_, err := w.Write(buf)
	return err
}

// ReadFrame lee una trama. El tope no es una cortesía: sin él, el otro extremo
// decide cuánta memoria reservamos.
func ReadFrame(r io.Reader) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > MaxFrame {
		return nil, fmt.Errorf("frame of %d bytes; the limit is %d", n, MaxFrame)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ── códec ───────────────────────────────────────────────────────────────────

// Enc construye un cuerpo.
type Enc struct{ B []byte }

func (e *Enc) U8(v byte)    { e.B = append(e.B, v) }
func (e *Enc) U32(v uint32) { e.B = binary.BigEndian.AppendUint32(e.B, v) }
func (e *Enc) U64(v uint64) { e.B = binary.BigEndian.AppendUint64(e.B, v) }
func (e *Enc) I64(v int64)  { e.U64(uint64(v)) }

// Str escribe una cadena de hasta 64 KiB.
func (e *Enc) Str(s string) {
	if len(s) > 0xffff {
		s = s[:0xffff]
	}
	e.B = binary.BigEndian.AppendUint16(e.B, uint16(len(s)))
	e.B = append(e.B, s...)
}

// Bytes escribe un bloque de datos.
func (e *Enc) Bytes(b []byte) {
	e.U32(uint32(len(b)))
	e.B = append(e.B, b...)
}

// Dec lee un cuerpo con comprobación de límites. El primer error se queda
// pegado y todo lo que se lea después vale cero: quien decodifica mira Err()
// una vez al final en vez de después de cada campo.
type Dec struct {
	B   []byte
	err error
}

// NewDec prepara la lectura de b.
func NewDec(b []byte) *Dec { return &Dec{B: b} }

// Err devuelve el primer error de lectura.
func (d *Dec) Err() error { return d.err }

var errShort = errors.New("truncated message")

func (d *Dec) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || len(d.B) < n {
		d.err = errShort
		return nil
	}
	b := d.B[:n]
	d.B = d.B[n:]
	return b
}

func (d *Dec) U8() byte {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (d *Dec) U32() uint32 {
	if b := d.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (d *Dec) U64() uint64 {
	if b := d.take(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}

func (d *Dec) I64() int64 { return int64(d.U64()) }

func (d *Dec) Str() string {
	b := d.take(2)
	if b == nil {
		return ""
	}
	return string(d.take(int(binary.BigEndian.Uint16(b))))
}

// Bytes lee un bloque de datos de hasta max bytes. Devuelve una subslice del
// cuerpo, no una copia.
func (d *Dec) Bytes(max int) []byte {
	n := d.U32()
	if d.err != nil {
		return nil
	}
	if int64(n) > int64(max) {
		d.err = fmt.Errorf("data block of %d bytes; the limit is %d", n, max)
		return nil
	}
	return d.take(int(n))
}

// Done exige que no quede nada por leer: un mensaje con bytes de más no sigue
// el protocolo, y aceptarlo esconde errores del otro lado.
func (d *Dec) Done() error {
	if d.err != nil {
		return d.err
	}
	if len(d.B) != 0 {
		return fmt.Errorf("%d unexpected trailing bytes", len(d.B))
	}
	return nil
}

// Request empieza el cuerpo de una petición.
func Request(id uint64, op byte) *Enc {
	e := &Enc{B: make([]byte, 0, 64)}
	e.U64(id)
	e.U8(op)
	return e
}

// Response empieza el cuerpo de una respuesta.
func Response(id uint64, errno uint32) *Enc {
	e := &Enc{B: make([]byte, 0, 64)}
	e.U64(id)
	e.U32(errno)
	return e
}
