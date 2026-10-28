package guest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/share"
)

// conectarConWatch abre una sesión como hace el attach del agente: hello y
// watch antes de publicarla.
func (m *montaje) conectarConWatch(t *testing.T) func() {
	t.Helper()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.srv.Serve(ctx, a); close(done) }()
	s := newShareSession(b)
	s.hello()
	if !s.watch() {
		t.Fatal("the host does not watch")
	}
	m.fs.attach(s)
	return func() { cancel(); b.Close(); <-done }
}

type nota struct {
	code   uint32
	a, b   uint64
	nombre string
}

func leerNota(b []byte) nota {
	n := nota{code: le.Uint32(b[4:])}
	p := b[outHeaderSize:]
	switch n.code {
	case notifyInvalInode:
		n.a = le.Uint64(p)
	case notifyInvalEntry:
		n.a = le.Uint64(p)
		l := le.Uint32(p[8:])
		n.nombre = string(p[16 : 16+l])
	case notifyDelete:
		n.a, n.b = le.Uint64(p), le.Uint64(p[8:])
		l := le.Uint32(p[16:])
		n.nombre = string(p[24 : 24+l])
	}
	return n
}

// esperarNota espera un aviso que cumpla ok.
func esperarNota(t *testing.T, d *devFalso, que string, ok func(nota) bool) {
	t.Helper()
	tope := time.After(5 * time.Second)
	for {
		select {
		case b := <-d.notas:
			if ok(leerNota(b)) {
				return
			}
		case <-tope:
			t.Fatalf("no notification for %s", que)
		}
	}
}

// Un cambio hecho en el host llega al kernel del invitado como invalidación,
// y el agente hace (con su hilo) la operación que dispara inotify. Lo que
// cambia el propio invitado no vuelve como aviso.
func TestAvisosDelHost(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	m.stop()
	m.stop = m.conectarConWatch(t)

	var mu sync.Mutex
	var hechas []synthOp
	m.fs.touchHost = func(op synthOp, full string) {
		mu.Lock()
		hechas = append(hechas, op)
		mu.Unlock()
	}

	e, hello, _ := m.lookup(t, rootID, "hello.txt")
	if e != 0 {
		t.Fatalf("lookup: %d", e)
	}
	// El listado de la raíz hace que el host la vigile como directorio.
	if e, out := m.dev.pedir(t, opOpendir, rootID, u32s(0, 0)); e != 0 {
		t.Fatalf("opendir: %d", e)
	} else {
		m.dev.pedir(t, opReleasedir, rootID, u64s(le.Uint64(out)), u32s(0, 0))
	}

	// Modificar en el sitio.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("changed on the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	esperarNota(t, m.dev, "modify", func(n nota) bool { return n.code == notifyInvalInode && n.a == hello })

	// Crear en un directorio que el invitado listó.
	if err := os.WriteFile(filepath.Join(dir, "nuevo.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	esperarNota(t, m.dev, "create", func(n nota) bool { return n.code == notifyInvalEntry && n.nombre == "nuevo.txt" })

	// Borrar un fichero que el kernel conoce.
	if err := os.Remove(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	esperarNota(t, m.dev, "delete", func(n nota) bool { return n.code == notifyDelete && n.b == hello && n.nombre == "hello.txt" })

	// Esperar a que el notificador acabe con lo encolado.
	tope := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(hechas)
		mu.Unlock()
		if n >= 3 || time.Now().After(tope) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	vistos := map[byte]bool{}
	for _, op := range hechas {
		vistos[op.ev] = true
	}
	mu.Unlock()
	for _, ev := range []byte{share.EventModify, share.EventCreate, share.EventDelete} {
		if !vistos[ev] {
			t.Errorf("no in-guest operation for event %d (got %+v)", ev, hechas)
		}
	}

	// Lo que escribe el invitado no vuelve como aviso.
	e, sub, _ := m.lookup(t, rootID, "sub")
	if e != 0 {
		t.Fatalf("lookup sub: %d", e)
	}
	e, out := m.dev.pedir(t, opCreate, sub, u32s(linuxOWronly|linuxOCreat, 0o644, 0, 0), cstr("eco.txt"))
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	fh := le.Uint64(out[entryOutSize:])
	w := make([]byte, 40)
	le.PutUint64(w[0:], fh)
	le.PutUint32(w[16:], 3)
	if e, _ := m.dev.pedir(t, opWrite, le.Uint64(out[0:]), w, []byte("eco")); e != 0 {
		t.Fatalf("write: %d", e)
	}
	select {
	case b := <-m.dev.notas:
		if n := leerNota(b); n.nombre == "eco.txt" {
			t.Fatalf("the guest's own write came back as a notification: %+v", n)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// synthReply solo contesta las peticiones del hilo del notificador, y solo
// las de la operación en curso.
func TestSynthReplySoloSuHilo(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	e, hello, _ := m.lookup(t, rootID, "hello.txt")
	if e != 0 {
		t.Fatal(e)
	}
	n := m.fs.nodes.get(hello)
	m.fs.syn = synthOp{ev: share.EventDelete, node: hello, parent: rootID, name: "hello.txt"}
	req := fuseReq{opcode: opUnlink, unique: 9, nodeid: rootID, arg: cstr("otro.txt")}
	if _, ok := m.fs.synthReply(req, m.fs.nodes.get(rootID)); ok {
		t.Fatal("answered an unlink of another name")
	}
	req.arg = cstr("hello.txt")
	out, ok := m.fs.synthReply(req, m.fs.nodes.get(rootID))
	if !ok || le.Uint32(out[4:]) != 0 {
		t.Fatalf("synthetic unlink: ok=%v", ok)
	}
	if m.fs.nodes.child(m.fs.nodes.root, "hello.txt") != nil || n.parent != nil {
		t.Fatal("the node is still attached")
	}
	// En el host sigue: el borrado sintético no llega allí.
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatal(err)
	}
}
