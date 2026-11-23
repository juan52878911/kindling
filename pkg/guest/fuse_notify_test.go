package guest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	share "github.com/juan52878911/kindling/internal/shareproto"
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

// synthReply solo da por hechas las peticiones de la operación en curso; las
// demás mutadoras de su hilo se rechazan en local, sin llegar al host.
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
	if out, ok := m.fs.synthReply(req, m.fs.nodes.get(rootID)); !ok || int32(le.Uint32(out[4:])) != -int32(share.EPERM) {
		t.Fatalf("an unlink of another name was not refused locally: ok=%v", ok)
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

// El toque sintético que dispara inotify no puede acabar cambiando el host.
// Sobre un enlace simbólico, el stat/truncate del toque sigue el enlace y el
// SETATTR llega con el nodo destino, que synthReply no reconocía: se mandaba
// al host como un truncate de verdad (y lo escrito entre medias se perdía).
func TestToqueSinteticoNoMutaElHost(t *testing.T) {
	dir := carpeta(t)
	if err := os.Symlink("hello.txt", filepath.Join(dir, "enlace")); err != nil {
		t.Fatal(err)
	}
	m := montar(t, dir, false)
	var mu sync.Mutex
	var hechas []synthOp
	m.fs.touchHost = func(op synthOp, full string) {
		mu.Lock()
		hechas = append(hechas, op)
		mu.Unlock()
	}
	e, enlace, _ := m.lookup(t, rootID, "enlace")
	if e != 0 {
		t.Fatalf("lookup enlace: %d", e)
	}
	e, hello, _ := m.lookup(t, rootID, "hello.txt")
	if e != 0 {
		t.Fatalf("lookup hello: %d", e)
	}

	// Un Modify sobre algo que no es un fichero regular no se toca.
	m.fs.apply(pushEvent{ev: share.EventModify, path: "enlace"})
	mu.Lock()
	if len(hechas) != 0 {
		t.Errorf("touched a symlink: %+v", hechas)
	}
	mu.Unlock()

	// Con un toque en curso, una petición mutadora de su hilo que no es la
	// esperada (otro nodo) se contesta en local y no llega al host.
	const tid = 4242
	m.fs.synthTID.Store(tid)
	m.fs.synMu.Lock()
	m.fs.syn = synthOp{ev: share.EventModify, node: enlace, path: "enlace"}
	m.fs.synMu.Unlock()
	sa := make([]byte, 88)
	le.PutUint32(sa[0:], fattrSize)
	le.PutUint64(sa[16:], 0)
	m.fs.handle(fuseReq{opcode: opSetattr, unique: 77, nodeid: hello, pid: tid, arg: sa})
	m.fs.synMu.Lock()
	m.fs.syn = synthOp{}
	m.fs.synMu.Unlock()

	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello from the host\n" {
		t.Fatalf("the synthetic touch reached the host: content %q", b)
	}
}

// Al volver una sesión (thaw, reinicio del daemon, corte), el vigilante del
// host empieza vacío: lo que cambió mientras no había sesión no llegó como
// aviso. Lo que el kernel tenga en caché larga se tiene que invalidar.
func TestReconectarInvalidaLaCache(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	m.stop()
	m.stop = m.conectarConWatch(t)
	m.fs.touchHost = func(synthOp, string) {}

	e, hello, _ := m.lookup(t, rootID, "hello.txt")
	if e != 0 {
		t.Fatalf("lookup: %d", e)
	}
	// Vaciar lo que dejó la conexión anterior.
	for len(m.dev.notas) > 0 {
		<-m.dev.notas
	}

	// Corte y sesión nueva; entre medias el host cambia el fichero.
	m.stop()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("changed while detached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.stop = m.conectarConWatch(t)
	esperarNota(t, m.dev, "reattach", func(n nota) bool { return n.code == notifyInvalInode && n.a == hello })
}
