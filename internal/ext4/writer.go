package ext4

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	blockSize      = 4096
	blocksPerGroup = 32768
	inodeSize      = 256
	inodesPerBlock = blockSize / inodeSize
	extraIsize     = 32
	firstIno       = 11
	descSize       = 32
	maxExtentLen   = 32768
	extentsPerLeaf = (blockSize - 12) / 12

	// En el superbloque.
	compatExtAttr      = 0x0008
	incompatFiletype   = 0x0002
	incompatExtents    = 0x0040
	incompat64bit      = 0x0080
	incompatFlexBG     = 0x0200
	incompatInlineData = 0x8000
	roCompatSparse     = 0x0001
	roCompatLargeFile  = 0x0002
	roCompatDirNlink   = 0x0020
	roCompatExtraIsize = 0x0040

	flagExtents    = 0x00080000
	flagInlineData = 0x10000000

	xattrMagic = 0xEA020000
)

// BlockSize es el tamaño de bloque de las imágenes que escribe este paquete.
const BlockSize = blockSize

// Options ajusta lo que escribe Write.
type Options struct {
	Label string
	// UUID del sistema de ficheros; a cero sale uno aleatorio.
	UUID [16]byte
	// HashSeed de los directorios con índice (no se usan aquí, pero el
	// núcleo los crea si alguien escribe en la imagen montada).
	HashSeed [16]byte
	// Time es la hora de creación y última escritura del superbloque.
	Time time.Time
	// SlackBlocks son bloques libres además de los que ocupa el contenido.
	SlackBlocks int64
	// SlackInodes son inodos libres de más.
	SlackInodes int
	// MinBlocks es el tamaño mínimo en bloques (un ext4 vacío de 2 GiB).
	MinBlocks int64
	// InodeRatio: un inodo por cada tantos bytes, como mkfs -i (0 = solo lo justo).
	InodeRatio int64
	// LostFound crea /lost+found (16 KiB, 0700) si no está.
	LostFound bool
	// ZeroHoles deja como hueco cada bloque de 4 KiB que sea todo ceros: se
	// lee igual y no ocupa.
	ZeroHoles bool
}

// Stream recorre una fuente de datos de principio a fin y llama a emit con
// la clave y los datos de cada fichero; los que no estén en el árbol (una
// entrada sustituida por otra capa) se saltan.
type Stream func(emit func(key int, r io.Reader) error) error

// Stats resume lo escrito.
type Stats struct {
	Blocks, FreeBlocks int64
	Inodes, FreeInodes int
	Files              int
}

// Bytes da el tamaño del fichero de la imagen.
func (s Stats) Bytes() int64 { return s.Blocks * blockSize }

type nodeInfo struct {
	parent  *Node
	extents []extent
	blocks  uint64 // bloques asignados (datos + árbol de extents)
	tree    []extent
	xblock  uint64
	xattrs  []xattrEntry
	inlineX bool
	dirData []byte
	written bool
	depth   int
}

type extent struct{ logical, phys, n uint64 }

type writer struct {
	f      *os.File
	opt    Options
	root   *Node
	nodes  []*Node // por número de inodo - firstIno ... en orden de asignación
	info   map[*Node]*nodeInfo
	geo    geometry
	used   []uint64 // mapa de bloques
	cursor uint64
	dw     dataWriter
	zero   [blockSize]byte
}

type geometry struct {
	groups, ipg, itb, gdtb int
	blocks                 uint64
	bb, ib, it             []uint64
}

// Write escribe el árbol root como un ext4 en f (que trunca). Los ficheros
// con StreamKey reciben sus datos de streams, en orden.
func Write(f *os.File, root *Node, streams []Stream, opt Options) (Stats, error) {
	if !root.IsDir() {
		return Stats{}, errors.New("the root must be a directory")
	}
	if opt.Time.IsZero() {
		opt.Time = time.Now()
	}
	if opt.UUID == ([16]byte{}) {
		_, _ = rand.Read(opt.UUID[:])
		opt.UUID[6] = opt.UUID[6]&0x0f | 0x40
		opt.UUID[8] = opt.UUID[8]&0x3f | 0x80
	}
	if opt.HashSeed == ([16]byte{}) {
		opt.HashSeed = opt.UUID
		opt.HashSeed[0] ^= 0x5a
	}
	w := &writer{f: f, opt: opt, root: root, info: map[*Node]*nodeInfo{}}
	if opt.LostFound && root.Child("lost+found") == nil {
		lf := NewDir(0o700, 0, 0, opt.Time)
		root.SetChild("lost+found", lf)
	}
	keys, err := w.assign()
	if err != nil {
		return Stats{}, err
	}
	data, err := w.estimate()
	if err != nil {
		return Stats{}, err
	}
	w.layout(data, len(w.nodes)+firstIno-1)
	if err := f.Truncate(0); err != nil {
		return Stats{}, err
	}
	if err := f.Truncate(int64(w.geo.blocks) * blockSize); err != nil {
		return Stats{}, err
	}
	w.dw = dataWriter{f: f}

	// Directorios y enlaces largos primero (cerca de las tablas de inodos),
	// luego los ficheros en el orden del árbol y al final los flujos.
	for _, n := range w.nodes {
		ni := w.info[n]
		switch {
		case n.IsDir():
			if err := w.writeData(n, bytes.NewReader(ni.dirData), int64(len(ni.dirData)), false); err != nil {
				return Stats{}, err
			}
		case n.IsLink() && len(n.Target) >= 60:
			if err := w.writeData(n, strings.NewReader(n.Target), int64(len(n.Target)), false); err != nil {
				return Stats{}, err
			}
		}
	}
	for _, n := range w.nodes {
		if !n.IsReg() {
			continue
		}
		if _, ok := n.Data.(StreamKey); ok {
			continue
		}
		r, err := n.Open()
		if err != nil {
			return Stats{}, fmt.Errorf("%s: %w", w.pathOf(n), err)
		}
		err = w.writeData(n, r, n.Size, opt.ZeroHoles)
		r.Close()
		if err != nil {
			return Stats{}, fmt.Errorf("%s: %w", w.pathOf(n), err)
		}
	}
	for i, s := range streams {
		m := keys[i]
		err := s(func(key int, r io.Reader) error {
			n := m[key]
			if n == nil || w.info[n].written {
				return nil
			}
			if err := w.writeData(n, r, n.Size, opt.ZeroHoles); err != nil {
				return fmt.Errorf("%s: %w", w.pathOf(n), err)
			}
			return nil
		})
		if err != nil {
			return Stats{}, err
		}
		for _, n := range m {
			if !w.info[n].written {
				return Stats{}, fmt.Errorf("%s: stream %d never delivered its data", w.pathOf(n), i)
			}
		}
	}
	for _, n := range w.nodes {
		ni := w.info[n]
		if !ni.inlineX && len(ni.xattrs) > 0 {
			b := w.alloc()
			ni.xblock = b
			if err := w.dw.write(b, xattrBlock(ni.xattrs)); err != nil {
				return Stats{}, err
			}
		}
	}
	if err := w.dw.flush(); err != nil {
		return Stats{}, err
	}
	return w.writeMetadata()
}

func (w *writer) pathOf(n *Node) string {
	var parts []string
	for n != w.root {
		ni := w.info[n]
		if ni == nil || ni.parent == nil {
			break
		}
		p := ni.parent
		for name, c := range p.children {
			if c == n {
				parts = append([]string{name}, parts...)
				break
			}
		}
		n = p
	}
	return "/" + strings.Join(parts, "/")
}

// assign numera los inodos y cuenta enlaces. Devuelve, por flujo, qué nodo
// espera cada clave.
func (w *writer) assign() ([]map[int]*Node, error) {
	var keys []map[int]*Node
	next := uint32(firstIno)
	var visit func(n, parent *Node, depth int) error
	visit = func(n, parent *Node, depth int) error {
		if ni, ok := w.info[n]; ok {
			if n.IsDir() {
				return fmt.Errorf("%s: directory linked twice", w.pathOf(parent))
			}
			n.nlink++
			_ = ni
			return nil
		}
		ni := &nodeInfo{parent: parent, depth: depth}
		w.info[n] = ni
		n.nlink = 1
		switch {
		case n == w.root:
			n.ino = 2
		case parent == w.root && n.IsDir() && w.root.Child("lost+found") == n:
			n.ino = firstIno
		default:
			next++
			n.ino = next
		}
		switch n.Mode & ModeType {
		case ModeDir:
			n.nlink = 2
		case ModeReg:
			if n.Size < 0 {
				return errors.New("negative size")
			}
			if n.Data == nil && n.Size > 0 {
				return fmt.Errorf("regular file of %d bytes without data", n.Size)
			}
			if sk, ok := n.Data.(StreamKey); ok {
				for len(keys) <= sk.Stream {
					keys = append(keys, map[int]*Node{})
				}
				keys[sk.Stream][sk.Key] = n
			}
		case ModeLink:
			if n.Target == "" || len(n.Target) >= blockSize {
				return fmt.Errorf("symlink target of %d bytes", len(n.Target))
			}
		case ModeChar, ModeBlock, ModeFIFO, ModeSocket:
		default:
			return fmt.Errorf("unknown file type %o", n.Mode&ModeType)
		}
		xs, err := encodeXattrs(n.Xattrs)
		if err != nil {
			return err
		}
		ni.xattrs = xs
		if n.IsDir() {
			for _, name := range n.Children() {
				c := n.children[name]
				if err := visit(c, n, depth+1); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				if c.IsDir() {
					n.nlink++
				}
			}
		}
		return nil
	}
	// El orden de los inodos: primero root y lost+found, luego a lo ancho no;
	// en el orden de Walk, que es el de los directorios en disco.
	if err := visit(w.root, nil, 0); err != nil {
		return nil, err
	}
	w.nodes = make([]*Node, 0, len(w.info))
	for n := range w.info {
		w.nodes = append(w.nodes, n)
	}
	sort.Slice(w.nodes, func(i, j int) bool { return w.nodes[i].ino < w.nodes[j].ino })
	// Los números tienen que ser consecutivos desde firstIno (con el hueco
	// del 11 si no hay lost+found): se renumera por si acaso.
	if w.root.Child("lost+found") == nil || !w.root.Child("lost+found").IsDir() {
		for _, n := range w.nodes {
			if n.ino > firstIno {
				n.ino--
			}
		}
	}
	for _, n := range w.nodes {
		if n.IsDir() && n.nlink > 65000 {
			n.nlink = 1 // dir_nlink
		}
		if n.nlink > 65000 {
			return nil, fmt.Errorf("%s: too many hard links", w.pathOf(n))
		}
	}
	return keys, nil
}

// estimate calcula los bloques de datos que hacen falta como mucho y prepara
// los directorios.
func (w *writer) estimate() (uint64, error) {
	var total uint64
	for _, n := range w.nodes {
		ni := w.info[n]
		if len(ni.xattrs) > 0 {
			if fitsInInode(ni.xattrs) {
				ni.inlineX = true
			} else {
				if xattrBlockSize(ni.xattrs) > blockSize {
					return 0, fmt.Errorf("%s: extended attributes do not fit in one block", w.pathOf(n))
				}
				total++
			}
		}
		switch n.Mode & ModeType {
		case ModeDir:
			ni.dirData = w.dirBlocks(n)
			total += uint64(len(ni.dirData) / blockSize)
		case ModeLink:
			if len(n.Target) >= 60 {
				total++
			}
		case ModeReg:
			b := uint64((n.Size + blockSize - 1) / blockSize)
			total += b
			// Árbol de extents: un fichero asignado seguido solo se corta en
			// cada límite de grupo (copias del superbloque) y cada 32768 bloques.
			if b > 0 {
				ext := b/maxExtentLen + b/blocksPerGroup + 2
				if ext > 4 {
					total += (ext + extentsPerLeaf - 1) / extentsPerLeaf
				}
			}
		}
	}
	return total, nil
}

func isBackupGroup(g int) bool {
	if g <= 1 {
		return true
	}
	for _, p := range []int{3, 5, 7} {
		x := p
		for x < g {
			x *= p
		}
		if x == g {
			return true
		}
	}
	return false
}

func (w *writer) layout(data uint64, inodes int) {
	opt := w.opt
	need := inodes + opt.SlackInodes
	groups := 1
	var g geometry
	for iter := 0; iter < 64; iter++ {
		ninodes := need
		if opt.InodeRatio > 0 {
			byRatio := int(int64(groups) * blocksPerGroup * blockSize / opt.InodeRatio)
			if byRatio > ninodes {
				ninodes = byRatio
			}
		}
		ipg := (ninodes + groups - 1) / groups
		ipg = (ipg + inodesPerBlock - 1) / inodesPerBlock * inodesPerBlock
		if ipg < inodesPerBlock {
			ipg = inodesPerBlock
		}
		if ipg > blocksPerGroup {
			groups++
			continue
		}
		itb := ipg / inodesPerBlock
		gdtb := (groups*descSize + blockSize - 1) / blockSize
		overhead := uint64(1 + gdtb + groups*(2+itb))
		for gi := 1; gi < groups; gi++ {
			if isBackupGroup(gi) {
				overhead += uint64(1 + gdtb)
			}
		}
		total := overhead + data + uint64(opt.SlackBlocks)
		if total < uint64(opt.MinBlocks) {
			total = uint64(opt.MinBlocks)
		}
		if total < 64 {
			total = 64
		}
		want := int((total + blocksPerGroup - 1) / blocksPerGroup)
		// El último grupo no puede quedarse en nada: si lleva copia del
		// superbloque, que quepa con holgura.
		last := total - uint64(want-1)*blocksPerGroup
		if want > 1 && last < 256 {
			total += 256 - last
		}
		g = geometry{groups: groups, ipg: ipg, itb: itb, gdtb: gdtb, blocks: total}
		if want == groups {
			break
		}
		groups = want
	}
	w.geo = g
	w.used = make([]uint64, (g.blocks+63)/64)
	w.mark(0, uint64(1+g.gdtb))
	for gi := 1; gi < g.groups; gi++ {
		if isBackupGroup(gi) {
			w.mark(uint64(gi)*blocksPerGroup, uint64(1+g.gdtb))
		}
	}
	w.cursor = uint64(1 + g.gdtb)
	g.bb = make([]uint64, g.groups)
	g.ib = make([]uint64, g.groups)
	g.it = make([]uint64, g.groups)
	for i := range g.bb {
		g.bb[i] = w.alloc()
	}
	for i := range g.ib {
		g.ib[i] = w.alloc()
	}
	for i := range g.it {
		g.it[i] = w.allocContig(uint64(g.itb))
	}
	w.geo = g
}

func (w *writer) isUsed(b uint64) bool { return w.used[b/64]&(1<<(b%64)) != 0 }
func (w *writer) mark(b, n uint64) {
	for i := b; i < b+n; i++ {
		w.used[i/64] |= 1 << (i % 64)
	}
}

// alloc da el siguiente bloque libre. No falla: estimate reserva de sobra.
func (w *writer) alloc() uint64 {
	for w.cursor < w.geo.blocks && w.isUsed(w.cursor) {
		w.cursor++
	}
	if w.cursor >= w.geo.blocks {
		panic("ext4: out of blocks (estimate was wrong)")
	}
	b := w.cursor
	w.mark(b, 1)
	w.cursor++
	return b
}

func (w *writer) allocContig(n uint64) uint64 {
	for {
		for w.cursor < w.geo.blocks && w.isUsed(w.cursor) {
			w.cursor++
		}
		start := w.cursor
		ok := true
		for i := uint64(0); i < n; i++ {
			if start+i >= w.geo.blocks {
				panic("ext4: out of blocks for an inode table")
			}
			if w.isUsed(start + i) {
				w.cursor = start + i
				ok = false
				break
			}
		}
		if ok {
			w.mark(start, n)
			w.cursor = start + n
			return start
		}
	}
}

// writeData copia size bytes de r a bloques nuevos y apunta los extents.
func (w *writer) writeData(n *Node, r io.Reader, size int64, holes bool) error {
	ni := w.info[n]
	if ni.written {
		return nil
	}
	ni.written = true
	buf := make([]byte, 1<<20)
	var logical uint64
	left := size
	for left > 0 {
		want := int64(len(buf))
		if want > left {
			want = left
		}
		if _, err := io.ReadFull(r, buf[:want]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return fmt.Errorf("short data: %d of %d bytes", size-left, size)
			}
			return err
		}
		for off := int64(0); off < want; off += blockSize {
			end := off + blockSize
			if end > want {
				end = want
			}
			b := buf[off:end]
			if holes && bytes.Equal(b, w.zero[:len(b)]) {
				logical++
				continue
			}
			if len(b) < blockSize {
				pad := make([]byte, blockSize)
				copy(pad, b)
				b = pad
			}
			phys := w.alloc()
			if err := w.dw.write(phys, b); err != nil {
				return err
			}
			ni.blocks++
			if k := len(ni.extents); k > 0 {
				e := &ni.extents[k-1]
				if e.logical+e.n == logical && e.phys+e.n == phys && e.n < maxExtentLen {
					e.n++
					logical++
					continue
				}
			}
			ni.extents = append(ni.extents, extent{logical, phys, 1})
			logical++
		}
		left -= want
	}
	if len(ni.extents) > 4 {
		leaves := (len(ni.extents) + extentsPerLeaf - 1) / extentsPerLeaf
		if leaves > 4 {
			return fmt.Errorf("file too fragmented (%d extents)", len(ni.extents))
		}
		for l := 0; l < leaves; l++ {
			part := ni.extents[l*extentsPerLeaf:]
			if len(part) > extentsPerLeaf {
				part = part[:extentsPerLeaf]
			}
			phys := w.alloc()
			blk := make([]byte, blockSize)
			putExtentHeader(blk, len(part), extentsPerLeaf, 0)
			for i, e := range part {
				putExtent(blk[12+12*i:], e)
			}
			if err := w.dw.write(phys, blk); err != nil {
				return err
			}
			ni.blocks++
			ni.tree = append(ni.tree, extent{logical: part[0].logical, phys: phys})
		}
	}
	return nil
}

func putExtentHeader(b []byte, entries, max, depth int) {
	binary.LittleEndian.PutUint16(b[0:], 0xF30A)
	binary.LittleEndian.PutUint16(b[2:], uint16(entries))
	binary.LittleEndian.PutUint16(b[4:], uint16(max))
	binary.LittleEndian.PutUint16(b[6:], uint16(depth))
}

func putExtent(b []byte, e extent) {
	binary.LittleEndian.PutUint32(b[0:], uint32(e.logical))
	binary.LittleEndian.PutUint16(b[4:], uint16(e.n))
	binary.LittleEndian.PutUint16(b[6:], uint16(e.phys>>32))
	binary.LittleEndian.PutUint32(b[8:], uint32(e.phys))
}

func putIndex(b []byte, e extent) {
	binary.LittleEndian.PutUint32(b[0:], uint32(e.logical))
	binary.LittleEndian.PutUint32(b[4:], uint32(e.phys))
	binary.LittleEndian.PutUint16(b[8:], uint16(e.phys>>32))
}

func fileType(mode uint32) byte {
	switch mode & ModeType {
	case ModeReg:
		return 1
	case ModeDir:
		return 2
	case ModeChar:
		return 3
	case ModeBlock:
		return 4
	case ModeFIFO:
		return 5
	case ModeSocket:
		return 6
	case ModeLink:
		return 7
	}
	return 0
}

// dirBlocks empaqueta las entradas de un directorio en bloques lineales.
func (w *writer) dirBlocks(n *Node) []byte {
	type ent struct {
		ino  uint32
		name string
		typ  byte
	}
	parent := w.info[n].parent
	if parent == nil {
		parent = n
	}
	ents := []ent{{n.ino, ".", 2}, {parent.ino, "..", 2}}
	for _, name := range n.Children() {
		c := n.children[name]
		ents = append(ents, ent{c.ino, name, fileType(c.Mode)})
	}
	var out []byte
	blk := make([]byte, blockSize)
	pos, lastPos := 0, -1
	flush := func() {
		if lastPos >= 0 {
			binary.LittleEndian.PutUint16(blk[lastPos+4:], uint16(blockSize-lastPos))
		}
		out = append(out, blk...)
		blk = make([]byte, blockSize)
		pos, lastPos = 0, -1
	}
	for _, e := range ents {
		rec := (8 + len(e.name) + 3) &^ 3
		if pos+rec > blockSize {
			flush()
		}
		binary.LittleEndian.PutUint32(blk[pos:], e.ino)
		binary.LittleEndian.PutUint16(blk[pos+4:], uint16(rec))
		blk[pos+6] = byte(len(e.name))
		blk[pos+7] = e.typ
		copy(blk[pos+8:], e.name)
		lastPos = pos
		pos += rec
	}
	flush()
	// lost+found lleva 4 bloques, como el de mkfs: e2fsck mete ahí lo que
	// encuentra sin tener que hacerlo crecer.
	if n == w.root.Child("lost+found") {
		for len(out) < 4*blockSize {
			e := make([]byte, blockSize)
			binary.LittleEndian.PutUint16(e[4:], blockSize)
			out = append(out, e...)
		}
	}
	return out
}

// dataWriter junta escrituras de bloques seguidos en una sola.
type dataWriter struct {
	f     *os.File
	start uint64
	buf   []byte
}

func (d *dataWriter) write(block uint64, b []byte) error {
	if len(d.buf) > 0 && (block != d.start+uint64(len(d.buf)/blockSize) || len(d.buf) >= 8<<20) {
		if err := d.flush(); err != nil {
			return err
		}
	}
	if len(d.buf) == 0 {
		d.start = block
	}
	d.buf = append(d.buf, b...)
	return nil
}

func (d *dataWriter) flush() error {
	if len(d.buf) == 0 {
		return nil
	}
	_, err := d.f.WriteAt(d.buf, int64(d.start)*blockSize)
	d.buf = d.buf[:0]
	return err
}

func encTime(t time.Time) (uint32, uint32) {
	s := t.Unix()
	lo := int32(s)
	epoch := uint32((s-int64(lo))>>32) & 3
	return uint32(lo), uint32(t.Nanosecond())<<2 | epoch
}

func (w *writer) inode(n *Node) []byte {
	ni := w.info[n]
	b := make([]byte, inodeSize)
	le := binary.LittleEndian
	le.PutUint16(b[0:], uint16(n.Mode))
	le.PutUint16(b[2:], uint16(n.UID))
	le.PutUint16(b[120:], uint16(n.UID>>16))
	le.PutUint16(b[24:], uint16(n.GID))
	le.PutUint16(b[122:], uint16(n.GID>>16))
	mt := n.Mtime
	at, ct := n.Atime, n.Ctime
	if at.IsZero() {
		at = mt
	}
	if ct.IsZero() {
		ct = mt
	}
	a, ax := encTime(at)
	c, cx := encTime(ct)
	m, mx := encTime(mt)
	le.PutUint32(b[8:], a)
	le.PutUint32(b[12:], c)
	le.PutUint32(b[16:], m)
	le.PutUint32(b[140:], ax)
	le.PutUint32(b[132:], cx)
	le.PutUint32(b[136:], mx)
	le.PutUint32(b[144:], c)
	le.PutUint32(b[148:], cx)
	le.PutUint16(b[26:], uint16(n.nlink))
	le.PutUint16(b[128:], extraIsize)

	var size uint64
	var flags uint32
	blocks := ni.blocks
	iblock := b[40:100]
	switch n.Mode & ModeType {
	case ModeReg, ModeDir:
		if n.IsDir() {
			size = uint64(len(ni.dirData))
		} else {
			size = uint64(n.Size)
		}
		flags |= flagExtents
		w.putExtents(iblock, ni)
	case ModeLink:
		size = uint64(len(n.Target))
		if len(n.Target) < 60 {
			copy(iblock, n.Target)
		} else {
			flags |= flagExtents
			w.putExtents(iblock, ni)
		}
	case ModeChar, ModeBlock:
		if n.Major < 256 && n.Minor < 256 {
			le.PutUint32(iblock[0:], n.Major<<8|n.Minor)
		} else {
			le.PutUint32(iblock[4:], n.Minor&0xff|n.Major<<8|(n.Minor&^0xff)<<12)
		}
	}
	le.PutUint32(b[4:], uint32(size))
	le.PutUint32(b[108:], uint32(size>>32))
	if ni.xblock != 0 {
		blocks++
		le.PutUint32(b[104:], uint32(ni.xblock))
		le.PutUint16(b[118:], uint16(ni.xblock>>32))
	}
	sectors := blocks * (blockSize / 512)
	le.PutUint32(b[28:], uint32(sectors))
	le.PutUint16(b[116:], uint16(sectors>>32))
	le.PutUint32(b[32:], flags)
	if ni.inlineX {
		putInodeXattrs(b[128+extraIsize:], ni.xattrs)
	}
	return b
}

func (w *writer) putExtents(iblock []byte, ni *nodeInfo) {
	if len(ni.tree) > 0 {
		putExtentHeader(iblock, len(ni.tree), 4, 1)
		for i, e := range ni.tree {
			putIndex(iblock[12+12*i:], e)
		}
		return
	}
	putExtentHeader(iblock, len(ni.extents), 4, 0)
	for i, e := range ni.extents {
		putExtent(iblock[12+12*i:], e)
	}
}

func (w *writer) writeMetadata() (Stats, error) {
	g := w.geo
	le := binary.LittleEndian
	st := Stats{Blocks: int64(g.blocks), Inodes: g.groups * g.ipg}
	// Tablas de inodos, grupo a grupo, solo hasta el último inodo usado.
	usedInGroup := make([]int, g.groups)
	dirsInGroup := make([]int, g.groups)
	lastIno := uint32(firstIno - 1)
	tables := make([][]byte, g.groups)
	for _, n := range w.nodes {
		gi := int(n.ino-1) / g.ipg
		idx := int(n.ino-1) % g.ipg
		if tables[gi] == nil {
			tables[gi] = make([]byte, g.ipg*inodeSize)
		}
		copy(tables[gi][idx*inodeSize:], w.inode(n))
		if n.ino > lastIno {
			lastIno = n.ino
		}
		if n.IsDir() {
			dirsInGroup[gi]++
		}
		if n.IsReg() {
			st.Files++
		}
	}
	// Los inodos reservados (1-10) cuentan como usados.
	allocatedInodes := int(lastIno)
	for gi := 0; gi < g.groups; gi++ {
		lo := gi * g.ipg
		u := allocatedInodes - lo
		if u < 0 {
			u = 0
		}
		if u > g.ipg {
			u = g.ipg
		}
		usedInGroup[gi] = u
		if tables[gi] == nil {
			continue
		}
		end := u * inodeSize
		end = (end + blockSize - 1) / blockSize * blockSize
		if _, err := w.f.WriteAt(tables[gi][:end], int64(g.it[gi])*blockSize); err != nil {
			return st, err
		}
	}
	// Mapas de bits y descriptores.
	gdt := make([]byte, g.gdtb*blockSize)
	var freeBlocks uint64
	freeInodes := 0
	for gi := 0; gi < g.groups; gi++ {
		bm := make([]byte, blockSize)
		start := uint64(gi) * blocksPerGroup
		free := 0
		for i := uint64(0); i < blocksPerGroup; i++ {
			b := start + i
			if b >= g.blocks || w.isUsed(b) {
				bm[i/8] |= 1 << (i % 8)
			} else {
				free++
			}
		}
		im := make([]byte, blockSize)
		for i := 0; i < blocksPerGroup; i++ {
			if i < usedInGroup[gi] || i >= g.ipg {
				im[i/8] |= 1 << (i % 8)
			}
		}
		if _, err := w.f.WriteAt(bm, int64(g.bb[gi])*blockSize); err != nil {
			return st, err
		}
		if _, err := w.f.WriteAt(im, int64(g.ib[gi])*blockSize); err != nil {
			return st, err
		}
		fi := g.ipg - usedInGroup[gi]
		d := gdt[gi*descSize:]
		le.PutUint32(d[0:], uint32(g.bb[gi]))
		le.PutUint32(d[4:], uint32(g.ib[gi]))
		le.PutUint32(d[8:], uint32(g.it[gi]))
		le.PutUint16(d[12:], uint16(free))
		le.PutUint16(d[14:], uint16(fi))
		le.PutUint16(d[16:], uint16(dirsInGroup[gi]))
		freeBlocks += uint64(free)
		freeInodes += fi
	}
	st.FreeBlocks = int64(freeBlocks)
	st.FreeInodes = freeInodes
	for gi := 0; gi < g.groups; gi++ {
		if gi != 0 && !isBackupGroup(gi) {
			continue
		}
		sb := w.superblock(uint16(gi), freeBlocks, freeInodes)
		off := int64(gi) * blocksPerGroup * blockSize
		// El primario va en el byte 1024 del bloque 0; las copias, con
		// bloques de 4 KiB, al principio del primer bloque de su grupo.
		blk := make([]byte, blockSize)
		if gi == 0 {
			copy(blk[1024:], sb)
		} else {
			copy(blk, sb)
		}
		if _, err := w.f.WriteAt(blk, off); err != nil {
			return st, err
		}
		if _, err := w.f.WriteAt(gdt, off+blockSize); err != nil {
			return st, err
		}
	}
	return st, nil
}

func (w *writer) superblock(group uint16, freeBlocks uint64, freeInodes int) []byte {
	g := w.geo
	o := w.opt
	b := make([]byte, 1024)
	le := binary.LittleEndian
	t := uint32(o.Time.Unix())
	le.PutUint32(b[0:], uint32(g.groups*g.ipg))
	le.PutUint32(b[4:], uint32(g.blocks))
	le.PutUint32(b[12:], uint32(freeBlocks))
	le.PutUint32(b[16:], uint32(freeInodes))
	le.PutUint32(b[20:], 0)
	le.PutUint32(b[24:], 2)
	le.PutUint32(b[28:], 2)
	le.PutUint32(b[32:], blocksPerGroup)
	le.PutUint32(b[36:], blocksPerGroup)
	le.PutUint32(b[40:], uint32(g.ipg))
	le.PutUint32(b[48:], t)
	le.PutUint16(b[54:], 0xFFFF)
	le.PutUint16(b[56:], 0xEF53)
	le.PutUint16(b[58:], 1)
	le.PutUint16(b[60:], 1)
	le.PutUint32(b[64:], t)
	le.PutUint32(b[76:], 1)
	le.PutUint32(b[84:], firstIno)
	le.PutUint16(b[88:], inodeSize)
	le.PutUint16(b[90:], group)
	le.PutUint32(b[92:], compatExtAttr)
	le.PutUint32(b[96:], incompatFiletype|incompatExtents|incompatFlexBG)
	le.PutUint32(b[100:], roCompatSparse|roCompatLargeFile|roCompatDirNlink|roCompatExtraIsize)
	copy(b[104:120], o.UUID[:])
	copy(b[120:136], o.Label)
	copy(b[236:252], o.HashSeed[:])
	b[252] = 1 // half_md4
	le.PutUint32(b[256:], 0x000C)
	le.PutUint32(b[264:], t)
	le.PutUint16(b[348:], extraIsize)
	le.PutUint16(b[350:], extraIsize)
	le.PutUint32(b[352:], 0x0002) // hash sin signo
	b[372] = 4                    // 16 grupos por flex_bg (informativo)
	return b
}

func openHost(p string, size int64) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if st.Size() != size {
		f.Close()
		return nil, fmt.Errorf("%s changed size (%d, expected %d)", p, st.Size(), size)
	}
	return f, nil
}
