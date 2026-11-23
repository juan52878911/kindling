package guest

import (
	"net"
	"testing"

	hostshare "github.com/juan52878911/kindling/internal/share"
	share "github.com/juan52878911/kindling/internal/shareproto"
)

// avisoOlvido es el aviso «deja de fiarte de esta ruta» del daemon.
const avisoOlvido = eventForget

// El agente repite el código del aviso porque shareproto no lo tiene: tiene
// que ser el del daemon.
func TestEventForgetEsElDelDaemon(t *testing.T) {
	if eventForget != hostshare.EventForget {
		t.Fatalf("guest eventForget %d, daemon EventForget %d", eventForget, hostshare.EventForget)
	}
}

// sesionFalsa es una sesión cuyo otro extremo es el test: puede mandar los
// avisos que quiera sin un daemon detrás.
func sesionFalsa(t *testing.T) (*shareSession, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	s := newShareSession(b)
	s.watching.Store(true)
	t.Cleanup(func() { a.Close(); s.close() })
	return s, a
}

func mandarAviso(t *testing.T, c net.Conn, ev byte, p string) {
	t.Helper()
	if err := share.WriteFrame(c, share.Push(ev, p)); err != nil {
		t.Fatal(err)
	}
}

// vaciarNotas tira los avisos al kernel que hubiera hasta ahora.
func vaciarNotas(d *devFalso) {
	for {
		select {
		case <-d.notas:
		default:
			return
		}
	}
}

// lookupTTL es un LOOKUP que devuelve el nodo y la validez de la entrada.
func lookupTTL(t *testing.T, m *montaje, parent uint64, name string) (uint64, uint64) {
	t.Helper()
	e, out := m.dev.pedir(t, opLookup, parent, cstr(name))
	if e != 0 {
		t.Fatalf("lookup %s: %d", name, e)
	}
	return le.Uint64(out[0:]), le.Uint64(out[16:])
}

// Camino 2 (cola del notificador): si se pierde un aviso con la caché larga,
// el agente lo sabe: TTL corto mientras tanto e invalidación de todo lo que el
// kernel tiene, no solo de lo que nombraban los avisos que sí llegaron.
func TestAvisoPerdidoInvalidaTodo(t *testing.T) {
	m := montar(t, carpeta(t), false)
	m.stop()
	m.stop = m.conectarConWatch(t)
	hello, te := lookupTTL(t, m, rootID, "hello.txt")
	if hello == 0 || te != cacheLargo {
		t.Fatalf("lookup hello.txt: node %d ttl %d", hello, te)
	}

	// El notificador se queda atascado en la operación sintética del primer
	// aviso, y la cola se llena.
	suelta := make(chan struct{})
	atascado := make(chan struct{}, 1)
	m.fs.touchHost = func(synthOp, string) {
		select {
		case atascado <- struct{}{}:
		default:
		}
		<-suelta
	}
	m.fs.enqueue(pushEvent{share.EventModify, "hello.txt"})
	<-atascado
	for i := 0; i < maxPendingEvents+5; i++ {
		m.fs.enqueue(pushEvent{share.EventModify, "nada"})
	}
	if te, _ := m.fs.ttls(); te != cacheSeconds {
		t.Errorf("entry ttl %d with lost events, want %d", te, cacheSeconds)
	}
	vaciarNotas(m.dev)
	close(suelta)
	esperarNota(t, m.dev, "entry of hello.txt", func(n nota) bool {
		return n.code == notifyInvalEntry && n.a == rootID && n.nombre == "hello.txt"
	})
}

// Camino 2 (cola de la sesión): lo que la sesión tira porque nadie lo leía a
// tiempo también cuenta como perdido.
func TestSesionDesbordadaInvalidaTodo(t *testing.T) {
	m := montar(t, carpeta(t), false)
	m.stop()
	m.stop = m.conectarConWatch(t)
	if hello, _ := lookupTTL(t, m, rootID, "hello.txt"); hello == 0 {
		t.Fatal("no hello.txt")
	}
	s, c := sesionFalsa(t)
	for i := 0; i < maxPendingEvents+5; i++ {
		mandarAviso(t, c, share.EventModify, "nada")
	}
	vaciarNotas(m.dev)
	m.fs.attach(s)
	esperarNota(t, m.dev, "entry of hello.txt", func(n nota) bool {
		return n.code == notifyInvalEntry && n.a == rootID && n.nombre == "hello.txt"
	})
}

// Camino 1, lado del agente: el daemon dejó de vigilar una ruta y lo dice; el
// agente la saca de la caché del kernel (entrada y atributos). Con la ruta
// vacía, todo.
func TestOlvidoDelHostInvalida(t *testing.T) {
	m := montar(t, carpeta(t), false)
	m.stop()
	m.stop = m.conectarConWatch(t)
	hello, _ := lookupTTL(t, m, rootID, "hello.txt")
	lookupTTL(t, m, rootID, "sub")
	s, c := sesionFalsa(t)
	m.fs.attach(s)
	vaciarNotas(m.dev)

	mandarAviso(t, c, avisoOlvido, "hello.txt")
	esperarNota(t, m.dev, "entry of hello.txt", func(n nota) bool {
		return n.code == notifyInvalEntry && n.a == rootID && n.nombre == "hello.txt"
	})
	esperarNota(t, m.dev, "inode of hello.txt", func(n nota) bool { return n.code == notifyInvalInode && n.a == hello })

	mandarAviso(t, c, avisoOlvido, "")
	esperarNota(t, m.dev, "entry of sub", func(n nota) bool {
		return n.code == notifyInvalEntry && n.a == rootID && n.nombre == "sub"
	})
}

// Camino 3, lado del agente: una entrada negativa solo va larga si su padre
// se puede invalidar. La raíz no tiene entrada que invalidar (y sus negativas
// no se pueden tirar sin saber sus nombres): las suyas van cortas.
func TestNegativaEnLaRaizEsCorta(t *testing.T) {
	m := montar(t, carpeta(t), false)
	m.stop()
	m.stop = m.conectarConWatch(t)
	if n, te := lookupTTL(t, m, rootID, "no-existe"); n != 0 || te != cacheSeconds {
		t.Errorf("negative lookup at the root: node %d ttl %d, want 0 and %d", n, te, cacheSeconds)
	}
	sub, _ := lookupTTL(t, m, rootID, "sub")
	if n, te := lookupTTL(t, m, sub, "no-existe"); n != 0 || te != cacheLargo {
		t.Errorf("negative lookup under sub: node %d ttl %d, want 0 and %d", n, te, cacheLargo)
	}
}

// Camino 3: de un directorio sin foto el daemon solo dice «cambió» (no qué
// nombre). Sus entradas negativas tienen que caer también: se invalida la
// entrada del directorio, y con ella todo lo que cuelga de él.
func TestModifyDeDirectorioTiraSusEntradas(t *testing.T) {
	m := montar(t, carpeta(t), true)
	m.stop()
	m.stop = m.conectarConWatch(t)
	lookupTTL(t, m, rootID, "sub")
	s, c := sesionFalsa(t)
	m.fs.attach(s)
	vaciarNotas(m.dev)
	mandarAviso(t, c, share.EventModify, "sub")
	esperarNota(t, m.dev, "entry of sub", func(n nota) bool {
		return n.code == notifyInvalEntry && n.a == rootID && n.nombre == "sub"
	})
}
