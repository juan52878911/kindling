package guest

// La conexión con el daemon por la que viajan las operaciones de una carpeta
// compartida en vivo. La abre el daemon (POST /share/attach) y, una vez
// secuestrada, los papeles se invierten: el agente pide y el daemon contesta.

import (
	"io"
	"sync"

	"github.com/juan52878911/kindling/pkg/share"
)

// maxCalls acota las operaciones en vuelo hacia el daemon por carpeta. El
// lector de /dev/fuse deja de leer cuando se llena: la contrapresión llega al
// kernel, que es donde debe esperar un proceso que hace E/S.
const maxCalls = 64

type callResult struct {
	errno uint32
	body  []byte
}

// shareSession es UNA conexión con el daemon. Cuando se corta, todo lo que
// estaba en vuelo recibe EIO y la sesión no se reutiliza: la siguiente la abre
// el daemon con otro attach.
type shareSession struct {
	rw  io.ReadWriteCloser
	wmu sync.Mutex

	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan callResult
	done    chan struct{}
	closed  bool
}

func newShareSession(rw io.ReadWriteCloser) *shareSession {
	s := &shareSession{rw: rw, pending: map[uint64]chan callResult{}, done: make(chan struct{})}
	go s.readLoop()
	return s
}

func (s *shareSession) readLoop() {
	defer s.close()
	for {
		body, err := share.ReadFrame(s.rw)
		if err != nil {
			return
		}
		d := share.NewDec(body)
		id, errno := d.U64(), d.U32()
		if d.Err() != nil {
			return
		}
		s.mu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- callResult{errno: errno, body: d.B}
		}
	}
}

// close corta la sesión y despierta con EIO a quien estuviera esperando.
func (s *shareSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pend := s.pending
	s.pending = map[uint64]chan callResult{}
	close(s.done)
	s.mu.Unlock()
	_ = s.rw.Close()
	for _, ch := range pend {
		ch <- callResult{errno: share.EIO}
	}
}

func (s *shareSession) alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// call manda una operación y espera su respuesta. build escribe los argumentos.
func (s *shareSession) call(op byte, build func(*share.Enc)) (uint32, *share.Dec) {
	ch := make(chan callResult, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return share.EIO, nil
	}
	s.next++
	id := s.next
	s.pending[id] = ch
	s.mu.Unlock()

	e := share.Request(id, op)
	if build != nil {
		build(e)
	}
	s.wmu.Lock()
	err := share.WriteFrame(s.rw, e.B)
	s.wmu.Unlock()
	if err != nil {
		s.close()
		return share.EIO, nil
	}
	r := <-ch
	if r.errno != 0 {
		return r.errno, nil
	}
	return 0, share.NewDec(r.body)
}
