package guest

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hostshare "github.com/juan52878911/kindling/internal/share"
	"github.com/juan52878911/kindling/pkg/share"
)

// devFalso es un /dev/fuse en memoria: el test hace de kernel.
type devFalso struct {
	in     chan []byte
	mu     sync.Mutex
	outs   map[uint64]chan []byte
	unique uint64
}

func newDevFalso() *devFalso {
	return &devFalso{in: make(chan []byte, 16), outs: map[uint64]chan []byte{}}
}

func (d *devFalso) Read(buf []byte) (int, error) {
	m, ok := <-d.in
	if !ok {
		return 0, syscall.ENODEV
	}
	return copy(buf, m), nil
}

func (d *devFalso) Write(b []byte) (int, error) {
	u := binary.LittleEndian.Uint64(b[8:])
	d.mu.Lock()
	ch := d.outs[u]
	delete(d.outs, u)
	d.mu.Unlock()
	if ch == nil {
		return 0, syscall.ENOENT
	}
	ch <- append([]byte(nil), b...)
	return len(b), nil
}

// pedir manda una petición y espera la respuesta: errno y carga.
func (d *devFalso) pedir(t *testing.T, op uint32, nodeid uint64, arg ...[]byte) (uint32, []byte) {
	t.Helper()
	d.mu.Lock()
	d.unique++
	u := d.unique
	ch := make(chan []byte, 1)
	d.outs[u] = ch
	d.mu.Unlock()
	d.in <- mkReq(op, u, nodeid, arg...)
	select {
	case r := <-ch:
		if int(binary.LittleEndian.Uint32(r)) != len(r) {
			t.Fatalf("reply length mismatch")
		}
		return uint32(-int32(binary.LittleEndian.Uint32(r[4:]))), r[outHeaderSize:]
	case <-time.After(10 * time.Second):
		t.Fatalf("no reply to op %d", op)
	}
	return 0, nil
}

// soltar manda algo que no lleva respuesta (FORGET).
func (d *devFalso) soltar(op uint32, nodeid uint64, arg ...[]byte) {
	d.mu.Lock()
	d.unique++
	u := d.unique
	d.mu.Unlock()
	d.in <- mkReq(op, u, nodeid, arg...)
}

func mkReq(op uint32, unique, nodeid uint64, arg ...[]byte) []byte {
	var body []byte
	for _, a := range arg {
		body = append(body, a...)
	}
	b := make([]byte, inHeaderSize, inHeaderSize+len(body))
	le.PutUint32(b[0:], uint32(inHeaderSize+len(body)))
	le.PutUint32(b[4:], op)
	le.PutUint64(b[8:], unique)
	le.PutUint64(b[16:], nodeid)
	return append(b, body...)
}

func u32s(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		le.PutUint32(b[i*4:], v)
	}
	return b
}

func u64s(vs ...uint64) []byte {
	b := make([]byte, 8*len(vs))
	for i, v := range vs {
		le.PutUint64(b[i*8:], v)
	}
	return b
}

func cstr(s string) []byte { return append([]byte(s), 0) }

// montaje junta el agente (con su /dev/fuse falso) y el servidor del host
// sobre dir, unidos por una tubería.
type montaje struct {
	dev  *devFalso
	fs   *fuseFS
	srv  *hostshare.Server
	stop func()
}

func montar(t *testing.T, dir string, ro bool) *montaje {
	t.Helper()
	srv, err := hostshare.Open(dir, ro)
	if err != nil {
		t.Fatal(err)
	}
	dev := newDevFalso()
	fs := newFuseFS(dev, "/work", false) // el agente no sabe que es ro: lo impone el host
	fs.grace = 2 * time.Second
	go fs.serve()
	m := &montaje{dev: dev, fs: fs, srv: srv}
	m.stop = m.conectar(t)
	t.Cleanup(func() {
		m.stop()
		close(dev.in)
		srv.Close()
	})
	// INIT, como hace el kernel al montar.
	e, out := dev.pedir(t, opInit, 0, u32s(7, 38, 1<<20, 0xffffffff))
	if e != 0 || le.Uint32(out[0:]) != 7 || le.Uint32(out[4:]) != fuseMinor || le.Uint32(out[20:]) != share.MaxIO {
		t.Fatalf("init: errno %d reply %v", e, out[:24])
	}
	return m
}

// conectar abre una sesión nueva entre el agente y el host, como hace el
// daemon con cada attach.
func (m *montaje) conectar(t *testing.T) func() {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.srv.Serve(ctx, a); close(done) }()
	m.fs.attach(newShareSession(b))
	return func() { cancel(); b.Close(); <-done }
}

func (m *montaje) lookup(t *testing.T, parent uint64, name string) (uint32, uint64, fattr) {
	t.Helper()
	e, out := m.dev.pedir(t, opLookup, parent, cstr(name))
	if e != 0 {
		return e, 0, fattr{}
	}
	return 0, le.Uint64(out[0:]), leerAttr(out[40:])
}

func leerAttr(b []byte) fattr {
	return fattr{ino: le.Uint64(b[0:]), size: le.Uint64(b[8:]), mode: le.Uint32(b[60:]), uid: le.Uint32(b[68:])}
}

func (m *montaje) open(t *testing.T, node uint64, flags uint32) (uint32, uint64) {
	t.Helper()
	e, out := m.dev.pedir(t, opOpen, node, u32s(flags, 0))
	if e != 0 {
		return e, 0
	}
	return 0, le.Uint64(out)
}

func readIn(fh, off uint64, size uint32) []byte {
	b := make([]byte, 40)
	le.PutUint64(b[0:], fh)
	le.PutUint64(b[8:], off)
	le.PutUint32(b[16:], size)
	return b
}

func carpeta(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "share")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(dir, "etc")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFUSELeer(t *testing.T) {
	m := montar(t, carpeta(t), true)

	e, id, a := m.lookup(t, rootID, "hello.txt")
	if e != 0 || a.size != 20 || a.mode&share.SIFMT != share.SIFREG || a.ino != id || a.uid != 0 {
		t.Fatalf("lookup: errno %d id %d attr %+v", e, id, a)
	}
	// Un segundo lookup del mismo nombre da el mismo nodo.
	if _, id2, _ := m.lookup(t, rootID, "hello.txt"); id2 != id {
		t.Fatalf("second lookup gave node %d, first %d", id2, id)
	}
	e, fh := m.open(t, id, linuxORdonly)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	e, data := m.dev.pedir(t, opRead, id, readIn(fh, 0, 4096))
	if e != 0 || string(data) != "hello from the host\n" {
		t.Fatalf("read: %d %q", e, data)
	}
	// GETATTR con handle.
	if e, out := m.dev.pedir(t, opGetattr, id, u32s(getattrFH, 0), u64s(fh)); e != 0 || leerAttr(out[16:]).size != 20 {
		t.Fatalf("getattr fh: %d", e)
	}

	// READDIR: ".", ".." y lo que hay, con inodos no nulos.
	e, out := m.dev.pedir(t, opOpendir, rootID, u32s(0, 0))
	if e != 0 {
		t.Fatalf("opendir: %d", e)
	}
	dfh := le.Uint64(out)
	e, raw := m.dev.pedir(t, opReaddir, rootID, readIn(dfh, 0, 4096))
	if e != 0 {
		t.Fatalf("readdir: %d", e)
	}
	var names []string
	for len(raw) >= direntBaseSize {
		ino, nl := le.Uint64(raw[0:]), int(le.Uint32(raw[16:]))
		if ino == 0 {
			t.Errorf("entry with d_ino 0")
		}
		names = append(names, string(raw[24:24+nl]))
		raw = raw[(direntBaseSize+nl+7)&^7:]
	}
	sort.Strings(names)
	if strings.Join(names, ",") != ".,..,etc,hello.txt,sub" {
		t.Errorf("readdir: %v", names)
	}

	// Solo lectura: lo impone el host aunque el agente no lo sepa.
	if e, _ := m.open(t, id, linuxOWronly); e != share.EROFS {
		t.Errorf("open for write on ro = %d", e)
	}
	if e, _ := m.dev.pedir(t, opMkdir, rootID, u32s(0o755, 0), cstr("x")); e != share.EROFS {
		t.Errorf("mkdir on ro = %d", e)
	}
}

func TestFUSEEscribir(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)

	// CREATE + WRITE.
	e, out := m.dev.pedir(t, opCreate, rootID, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr("new.txt"))
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	node, fh := le.Uint64(out[0:]), le.Uint64(out[entryOutSize:])
	w := make([]byte, 40)
	le.PutUint64(w[0:], fh)
	le.PutUint32(w[16:], 5)
	if e, out := m.dev.pedir(t, opWrite, node, w, []byte("guest")); e != 0 || le.Uint32(out) != 5 {
		t.Fatalf("write: %d", e)
	}
	// Tamaño que no coincide con los datos: EINVAL.
	le.PutUint32(w[16:], 9)
	if e, _ := m.dev.pedir(t, opWrite, node, w, []byte("guest")); e != share.EINVAL {
		t.Errorf("lying write size = %d", e)
	}
	m.dev.pedir(t, opRelease, node, u64s(fh, 0, 0))
	if b, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(b) != "guest" {
		t.Fatalf("host sees %q", b)
	}

	// MKDIR, RENAME de un fichero dentro, y el nodo sigue sirviendo con la
	// ruta nueva.
	e, out = m.dev.pedir(t, opMkdir, rootID, u32s(0o755, 0), cstr("d"))
	if e != 0 {
		t.Fatalf("mkdir: %d", e)
	}
	d := le.Uint64(out)
	if e, _ := m.dev.pedir(t, opRename, rootID, u64s(d), cstr("new.txt"), cstr("moved.txt")); e != 0 {
		t.Fatalf("rename: %d", e)
	}
	if e, out := m.dev.pedir(t, opGetattr, node, u32s(0, 0), u64s(0)); e != 0 || leerAttr(out[16:]).size != 5 {
		t.Fatalf("getattr after rename: %d", e)
	}
	if _, err := os.Stat(filepath.Join(dir, "d", "moved.txt")); err != nil {
		t.Fatal(err)
	}
	// SETATTR: truncar por ruta.
	sa := make([]byte, 88)
	le.PutUint32(sa[0:], fattrSize)
	le.PutUint64(sa[16:], 2)
	if e, out := m.dev.pedir(t, opSetattr, node, sa); e != 0 || leerAttr(out[16:]).size != 2 {
		t.Fatalf("setattr: %d", e)
	}
	// uid/gid: no hacen nada, pero no fallan.
	le.PutUint32(sa[0:], fattrUID|fattrGID)
	if e, _ := m.dev.pedir(t, opSetattr, node, sa); e != 0 {
		t.Errorf("chown = %d", e)
	}
	// UNLINK y RMDIR.
	if e, _ := m.dev.pedir(t, opRmdir, rootID, cstr("d")); e != share.ENOTEMPTY {
		t.Errorf("rmdir non-empty = %d", e)
	}
	if e, _ := m.dev.pedir(t, opUnlink, d, cstr("moved.txt")); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if e, _ := m.dev.pedir(t, opRmdir, rootID, cstr("d")); e != 0 {
		t.Fatalf("rmdir: %d", e)
	}
	// El nodo borrado ya no tiene ruta.
	if e, _ := m.dev.pedir(t, opGetattr, node, u32s(0, 0), u64s(0)); e != share.ENOENT {
		t.Errorf("getattr of a removed node = %d", e)
	}
	// Enlaces y nodos especiales: prohibidos.
	if e, _ := m.dev.pedir(t, opSymlink, rootID, cstr("l"), cstr("/etc/passwd")); e != share.EPERM {
		t.Errorf("symlink = %d", e)
	}
	if e, _ := m.dev.pedir(t, opLink, rootID, u64s(node), cstr("hl")); e != share.EPERM {
		t.Errorf("link = %d", e)
	}
	if e, _ := m.dev.pedir(t, opMknod, rootID, u32s(syscall.S_IFCHR|0o666, 0, 0, 0), cstr("tty")); e != share.EPERM {
		t.Errorf("mknod = %d", e)
	}
	// Lo que no existe: ENOSYS.
	if e, _ := m.dev.pedir(t, 22 /* GETXATTR */, rootID, u32s(0, 0), cstr("user.x")); e != share.ENOSYS {
		t.Errorf("getxattr = %d", e)
	}
}

func TestFUSEEscapes(t *testing.T) {
	m := montar(t, carpeta(t), false)
	for _, name := range []string{"..", ".", "a/b", strings.Repeat("x", 256)} {
		if e, _, _ := m.lookup(t, rootID, name); e != share.EINVAL && e != share.ENAMETOOLONG {
			t.Errorf("lookup %q = %d", name, e)
		}
	}
	// Nombre sin NUL.
	if e, _ := m.dev.pedir(t, opLookup, rootID, []byte("hello.txt")); e != share.EINVAL {
		t.Errorf("unterminated name = %d", e)
	}
	// El enlace a /etc se ve como enlace, y nada lo atraviesa.
	e, etc, a := m.lookup(t, rootID, "etc")
	if e != 0 || a.mode&share.SIFMT != share.SIFLNK {
		t.Fatalf("lookup etc: %d %+v", e, a)
	}
	if e, out := m.dev.pedir(t, opReadlink, etc); e != 0 || string(out) != "/etc" {
		t.Fatalf("readlink: %d %q", e, out)
	}
	if e, _, _ := m.lookup(t, etc, "passwd"); e == 0 {
		t.Fatal("looked up /etc/passwd through the share")
	}
	if e, _ := m.open(t, etc, linuxORdonly); e == 0 {
		t.Fatal("opened the /etc symlink")
	}
	if e, _ := m.dev.pedir(t, opCreate, etc, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr("evil")); e == 0 {
		t.Fatal("created a file through the /etc symlink")
	}
	// Nodos y handles inventados.
	if e, _ := m.dev.pedir(t, opGetattr, 999999, u32s(0, 0), u64s(0)); e != share.ESTALE {
		t.Errorf("unknown node = %d", e)
	}
	if e, _ := m.dev.pedir(t, opRead, rootID, readIn(424242, 0, 10)); e != share.EBADF {
		t.Errorf("unknown handle = %d", e)
	}
	if e, _ := m.dev.pedir(t, opRead, rootID, readIn(1, 0, share.MaxIO+1)); e != share.EINVAL {
		t.Errorf("oversized read = %d", e)
	}
	// Mensajes truncados: error, no pánico.
	for _, op := range []uint32{opGetattr, opSetattr, opOpen, opCreate, opRead, opWrite, opRename, opRename2, opMkdir, opReaddir, opRelease} {
		if e, _ := m.dev.pedir(t, op, rootID, []byte{1}); e == 0 {
			t.Errorf("op %d accepted a 1-byte argument", op)
		}
	}
}

// TestFUSEReconexion: al cortarse la sesión (congelar, reiniciar el daemon) lo
// que llega espera; con la sesión nueva, un fichero que ya estaba abierto se
// reabre solo y el mismo nodo sigue valiendo.
func TestFUSEReconexion(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	_, id, _ := m.lookup(t, rootID, "hello.txt")
	_, fh := m.open(t, id, linuxORdwr)

	m.stop() // la sesión muere; el daemon cierra sus handles
	if m.fs.attached() {
		time.Sleep(50 * time.Millisecond)
	}
	// Sin sesión: tras la gracia, EIO (no se cuelga).
	m.fs.grace = 200 * time.Millisecond
	if e, _ := m.dev.pedir(t, opRead, id, readIn(fh, 0, 5)); e != share.EIO {
		t.Fatalf("read while detached = %d, want EIO", e)
	}
	m.fs.grace = 2 * time.Second
	m.stop = m.conectar(t)
	e, data := m.dev.pedir(t, opRead, id, readIn(fh, 0, 5))
	if e != 0 || string(data) != "hello" {
		t.Fatalf("read after reattach: %d %q", e, data)
	}
	// Y una operación que espera mientras vuelve la sesión, sale bien.
	m.stop()
	res := make(chan uint32, 1)
	go func() {
		e, _ := m.dev.pedir(t, opGetattr, id, u32s(0, 0), u64s(0))
		res <- e
	}()
	time.Sleep(100 * time.Millisecond)
	m.stop = m.conectar(t)
	if e := <-res; e != 0 {
		t.Fatalf("getattr that waited for the session = %d", e)
	}
}

// TestFUSEOlvidos: FORGET suelta nodos, y sin FORGET la tabla no pasa del tope.
func TestFUSEOlvidos(t *testing.T) {
	dir := carpeta(t)
	for i := 0; i < 40; i++ {
		os.WriteFile(filepath.Join(dir, "sub", "f"+string(rune('a'+i%26))+strings.Repeat("x", i/26)), nil, 0o644)
	}
	m := montar(t, dir, true)
	_, sub, _ := m.lookup(t, rootID, "sub")
	_, f, _ := m.lookup(t, sub, "fa")
	m.dev.soltar(opForget, f, u64s(1))
	m.dev.soltar(opBatchForget, 0, u32s(1, 0), u64s(sub, 1))
	deadline := time.Now().Add(2 * time.Second)
	for m.fs.nodes.size() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := m.fs.nodes.size(); n != 1 {
		t.Fatalf("after forgetting everything the table has %d nodes", n)
	}

	m.fs.nodes.max = 16
	_, sub, _ = m.lookup(t, rootID, "sub")
	ents, _ := os.ReadDir(filepath.Join(dir, "sub"))
	for _, de := range ents {
		if e, _, _ := m.lookup(t, sub, de.Name()); e != 0 {
			t.Fatalf("lookup %s: %d", de.Name(), e)
		}
	}
	if n := m.fs.nodes.size(); n > 16 {
		t.Fatalf("the table grew to %d nodes with a cap of 16", n)
	}
	// Un nodo expulsado da ESTALE, y volver a buscarlo lo recupera.
	stale := 0
	for id := uint64(2); id < 50; id++ {
		if m.fs.nodes.get(id) == nil {
			if e, _ := m.dev.pedir(t, opGetattr, id, u32s(0, 0), u64s(0)); e == share.ESTALE {
				stale++
			}
		}
	}
	if stale == 0 {
		t.Fatal("no evicted node answered ESTALE")
	}
	if e, _, _ := m.lookup(t, sub, "fa"); e != 0 {
		t.Fatalf("lookup after eviction: %d", e)
	}
}

func TestFUSEInitViejo(t *testing.T) {
	fs := newFuseFS(newDevFalso(), "/x", false)
	r := fs.init(fuseReq{unique: 1, arg: u32s(7, 8, 0, 0)})
	if e := int32(le.Uint32(r[4:])); e != -71 {
		t.Fatalf("init with 7.8 = %d, want -EPROTO", e)
	}
	r = fs.init(fuseReq{unique: 1, arg: u32s(8, 0, 0, 0)})
	if le.Uint32(r[4:]) != 0 || le.Uint32(r[outHeaderSize:]) != 7 {
		t.Fatalf("init with major 8 must answer our major")
	}
}

func TestParseReq(t *testing.T) {
	if _, err := parseReq(make([]byte, 10)); err == nil {
		t.Error("short header accepted")
	}
	b := mkReq(opLookup, 1, 1, cstr("x"))
	le.PutUint32(b, uint32(len(b)+5))
	if _, err := parseReq(b); err == nil {
		t.Error("length mismatch accepted")
	}
	if _, rest, err := cstring([]byte("abc\x00def")); err != nil || !bytes.Equal(rest, []byte("def")) {
		t.Error("cstring")
	}
}
