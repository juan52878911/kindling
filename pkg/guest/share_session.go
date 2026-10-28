package guest

// La conexión con el daemon por la que viajan las operaciones de una carpeta
// compartida en vivo. La abre el daemon (POST /share/attach) y, una vez
// secuestrada, los papeles se invierten: el agente pide y el daemon contesta.

import (
	"io"
	"sync"
	"time"

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
	// retired: llegó share.Bye. Lo que quede pendiente al cerrar recibe
	// errnoRetry y no EIO: el daemon dijo que no lo ejecutó.
	retired bool

	// maxIO es lo más que se lee o escribe por operación en esta sesión:
	// share.BaseIO salvo que hello negocie más. Se fija antes de publicar la
	// sesión (fuseFS.attach) y no cambia después.
	maxIO uint32

	// events son los avisos de cambios del host (OpWatch). El lector nunca
	// espera por ellos: si el consumidor se atasca, se pierden (son pistas; las
	// cachés de 1 s siguen ahí).
	events chan pushEvent
}

// pushEvent es un aviso del daemon: algo cambió en path, en el host.
type pushEvent struct {
	ev   byte
	path string
}

// maxPendingEvents acota los avisos encolados por sesión.
const maxPendingEvents = 1024

func newShareSession(rw io.ReadWriteCloser) *shareSession {
	s := &shareSession{rw: rw, pending: map[uint64]chan callResult{}, done: make(chan struct{}), maxIO: share.BaseIO,
		events: make(chan pushEvent, maxPendingEvents)}
	go s.readLoop()
	return s
}

// helloTimeout acota la espera de la respuesta a OpHello. Un daemon que no
// contesta a la primera operación no contestará a ninguna: se corta la sesión
// y el daemon abre otra.
const helloTimeout = 10 * time.Second

// hello negocia el tamaño de E/S con el daemon. Hay que llamarla antes de
// publicar la sesión y sin nada más en vuelo: el daemon solo acepta tramas
// mayores después de contestar. Un daemon anterior contesta ENOSYS y la sesión
// se queda en share.BaseIO, que es lo que hablaba.
func (s *shareSession) hello() {
	t := time.AfterFunc(helloTimeout, s.close)
	defer t.Stop()
	errno, d := s.call(share.OpHello, func(e *share.Enc) { e.U32(share.MaxIO) })
	if errno != 0 {
		return
	}
	n := d.U32()
	if d.Err() != nil || n < share.BaseIO || n > share.MaxIO {
		return
	}
	s.maxIO = n
}

// watch pide los avisos de cambios del host. Igual que hello, antes de
// publicar la sesión. Un daemon anterior contesta ENOSYS: false.
func (s *shareSession) watch() bool {
	t := time.AfterFunc(helloTimeout, s.close)
	defer t.Stop()
	errno, _ := s.call(share.OpWatch, nil)
	return errno == 0
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
		if id == share.PushID && errno == share.ERetry {
			// share.Bye: el daemon cierra en orden y ya no ejecuta nada. Lo
			// que siga en vuelo no se hizo: se repite con la sesión siguiente.
			s.mu.Lock()
			s.retired = true
			s.mu.Unlock()
			return
		}
		if id == share.PushID {
			ev, p, err := share.ParsePush(d)
			if err != nil || errno != 0 {
				return // un daemon que no sigue el protocolo: fuera
			}
			select {
			case s.events <- pushEvent{ev, p}:
			default:
			}
			continue
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

// close corta la sesión y despierta con EIO a quien estuviera esperando (con
// errnoRetry si el daemon se despidió con share.Bye).
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
	errno := share.EIO
	if s.retired {
		errno = errnoRetry
	}
	s.mu.Unlock()
	_ = s.rw.Close()
	for _, ch := range pend {
		ch <- callResult{errno: errno}
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

// errnoRetry es la respuesta de call cuando la operación NO se hizo: la sesión
// ya estaba cerrada, la escritura falló, o el daemon la devolvió sin ejecutar
// porque se apaga (share.ERetry, el mismo valor). Se puede repetir con la
// sesión siguiente sin riesgo de hacerla dos veces. Nunca llega al kernel.
const errnoRetry = share.ERetry

// call manda una operación y espera su respuesta. build escribe los argumentos.
// Si la sesión se corta con la operación ya enviada, EIO: no se sabe si el
// daemon la hizo, y repetir un rename o un write a ciegas no es seguro.
func (s *shareSession) call(op byte, build func(*share.Enc)) (uint32, *share.Dec) {
	ch := make(chan callResult, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errnoRetry, nil
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
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		s.close()
		return errnoRetry, nil
	}
	r := <-ch
	if r.errno != 0 {
		return r.errno, nil
	}
	return 0, share.NewDec(r.body)
}
