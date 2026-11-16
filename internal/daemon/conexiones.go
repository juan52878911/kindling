package daemon

import (
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// Topes de conexiones al socket del daemon (R8). Cada conexión es un
// descriptor y al menos una goroutine, y las largas (/events, shell, exec en
// curso, una llamada a /guest que espera al invitado) duran lo que quiera el
// cliente: sin tope, quien alcanza el socket podía agotar los descriptores del
// daemon y dejarlo sin poder aceptar a nadie más, admin incluido. Con
// política, cada uid que no es admin tiene además su parte, para que un
// inquilino no se quede con todas (un frontal que lleva a muchos inquilinos
// con tokens desde un solo uid cuenta como uno: su parte es holgada).
// Variables para los tests.
var (
	maxConexiones    = 4096
	maxConexionesUID = 1024
)

// sinUID es la clave de quien no cuenta por uid: admin, uid desconocido o
// daemon sin política. Solo le aplica el tope global.
const sinUID = -1

// escuchaConTope envuelve el listener del daemon con los topes de conexiones.
// Identifica a cada cliente al aceptarlo (lo que haría conContexto) y le pasa
// el resultado en la conexión, para no leer sus credenciales dos veces.
type escuchaConTope struct {
	net.Listener
	s *Server

	mu      sync.Mutex
	total   int
	porUID  map[int]int
	ultAvis time.Time
}

func (s *Server) escuchaConTope(ln net.Listener) *escuchaConTope {
	return &escuchaConTope{Listener: ln, s: s, porUID: map[int]int{}}
}

// conIdentificada es una conexión aceptada con quién es y cómo devolver su
// hueco al cerrarla.
type conIdentificada struct {
	net.Conn
	ll   Llamante
	llOK bool

	una    sync.Once
	soltar func()
}

func (c *conIdentificada) Close() error {
	c.una.Do(c.soltar)
	return c.Conn.Close()
}

func (l *escuchaConTope) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		id := l.s.identificar
		if id == nil {
			id = credencialesPar
		}
		ll, err := id(c)
		llOK := err == nil
		clave := sinUID
		// Solo los admin de la política se libran (sin resolver grupos aquí:
		// esto corre en el bucle que acepta, una conexión tras otra).
		if p := l.s.authz; p != nil && llOK && ll.Conocido && !p.admins[ll.UID] {
			clave = ll.UID
		}
		if !l.tomar(clave) {
			l.rechazar(c, clave)
			continue
		}
		return &conIdentificada{Conn: c, ll: ll, llOK: llOK, soltar: func() { l.soltar(clave) }}, nil
	}
}

func (l *escuchaConTope) tomar(clave int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= maxConexiones || (clave != sinUID && l.porUID[clave] >= maxConexionesUID) {
		return false
	}
	l.total++
	if clave != sinUID {
		l.porUID[clave]++
	}
	return true
}

func (l *escuchaConTope) soltar(clave int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if clave != sinUID {
		if l.porUID[clave]--; l.porUID[clave] <= 0 {
			delete(l.porUID, clave)
		}
	}
}

// respuestaSinSitio es lo que recibe una conexión de más antes de cerrarse:
// un 429 que un cliente HTTP entiende, en vez de un corte sin explicación.
var respuestaSinSitio = func() string {
	cuerpo := `{"message":"the daemon has too many connections open (from you, or in total); retry later","code":"too_many_requests"}`
	return "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nRetry-After: 1\r\n" +
		"Connection: close\r\nContent-Length: " + strconv.Itoa(len(cuerpo)) + "\r\n\r\n" + cuerpo
}()

// rechazar contesta 429 a una conexión de más y la cierra, avisando en el log
// como mucho una vez por minuto.
func (l *escuchaConTope) rechazar(c net.Conn, clave int) {
	_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = c.Write([]byte(respuestaSinSitio))
	c.Close()
	l.mu.Lock()
	avisar := time.Since(l.ultAvis) > time.Minute
	if avisar {
		l.ultAvis = time.Now()
	}
	total := l.total
	l.mu.Unlock()
	if avisar {
		if clave != sinUID {
			log.Printf("refusing connections from uid %d: it has %d open (limit %d)", clave, maxConexionesUID, maxConexionesUID)
		} else {
			log.Printf("refusing connections: %d open (limit %d)", total, maxConexiones)
		}
	}
}
