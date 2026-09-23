package guest

// POST /share/attach: el daemon conecta una carpeta compartida en vivo.
//
// La primera vez monta el sistema de ficheros FUSE en el punto pedido; las
// siguientes (tras descongelar, tras reiniciarse el daemon) reconocen el montaje
// por su tag y solo cambian de sesión. El montaje no viaja en la línea de
// comandos del kernel: así el mismo agente sirve igual a una máquina arrancada
// en frío que a una restaurada. Ver docs/compartir.md.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/share"
)

// Shares son las carpetas vivas montadas en este invitado, por tag.
type Shares struct {
	mu     sync.Mutex
	mounts map[int]*shareMount

	// mount monta el sistema de ficheros y devuelve su /dev/fuse. Variable
	// para probar el manejador sin root ni Linux.
	mount func(target string, readOnly bool) (fuseDev, func(), error)
	// fromGuest dice si una petición viene del propio invitado. Variable por lo
	// mismo: en los tests todo llega por loopback.
	fromGuest func(r *http.Request) bool
}

type shareMount struct {
	a       share.Attach
	fs      *fuseFS
	unmount func()
}

var shareState = newShares()

func newShares() *Shares {
	return &Shares{mounts: map[int]*shareMount{}, mount: mountFUSE, fromGuest: requestFromGuest}
}

// errNoFUSE lo devuelve mountFUSE cuando el kernel del invitado no tiene FUSE.
var errNoFUSE = errors.New("this guest kernel has no FUSE support")

// AttachHandler sirve POST /share/attach con Upgrade: kling-share/1.
func (s *Shares) AttachHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Anuncia que este agente sabe de carpetas, también en los errores.
		w.Header().Set(share.HeaderAgent, share.Proto)
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), share.Proto) {
			http.Error(w, "this endpoint speaks "+share.Proto+"; ask for it with Upgrade", http.StatusUpgradeRequired)
			return
		}
		// Solo el host: un proceso de dentro que se hiciera pasar por el daemon
		// vería todo lo que el invitado escribe en la carpeta, y decidiría lo que
		// lee.
		if s.fromGuest(r) {
			http.Error(w, "shares are attached by the host, not from inside the guest", http.StatusForbidden)
			return
		}
		var a share.Attach
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&a); err != nil {
			http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
			return
		}
		if err := a.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		m, code, err := s.mountFor(a)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "this server cannot take over the connection", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Sin plazos: la conexión vive lo que viva la máquina.
		_ = conn.SetDeadline(time.Time{})
		if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
			share.Proto + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			conn.Close()
			return
		}
		if err := brw.Flush(); err != nil {
			conn.Close()
			return
		}
		// El lector con búfer puede traer bytes que ya leyó el servidor HTTP.
		m.fs.attach(newShareSession(&hijacked{r: brw.Reader, Conn: conn}))
		log.Printf("share %d attached at %s (%s)", a.Tag, a.Mount, a.Mode)
	}
}

// mountFor devuelve el montaje de a.Tag, montándolo si es la primera vez.
func (s *Shares) mountFor(a share.Attach) (*shareMount, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.mounts[a.Tag]; m != nil {
		if m.a != a {
			return nil, http.StatusConflict, fmt.Errorf("share %d is already mounted at %s (%s)", a.Tag, m.a.Mount, m.a.Mode)
		}
		return m, 0, nil
	}
	for _, m := range s.mounts {
		if m.a.Mount == a.Mount {
			return nil, http.StatusConflict, fmt.Errorf("%s already has share %d mounted", a.Mount, m.a.Tag)
		}
	}
	dev, unmount, err := s.mount(a.Mount, a.Mode == share.ModeRO)
	if err != nil {
		if errors.Is(err, errNoFUSE) {
			return nil, http.StatusNotImplemented, err
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("mounting %s: %w", a.Mount, err)
	}
	fs := newFuseFS(dev, a.Mount, a.Mode == share.ModeRO)
	go fs.serve()
	m := &shareMount{a: a, fs: fs, unmount: unmount}
	s.mounts[a.Tag] = m
	log.Printf("share %d mounted at %s (%s)", a.Tag, a.Mount, a.Mode)
	return m, 0, nil
}

// Close desmonta las carpetas vivas (apagado ordenado).
func (s *Shares) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tag, m := range s.mounts {
		if m.unmount != nil {
			m.unmount()
		}
		delete(s.mounts, tag)
	}
}

// hijacked junta el lector con búfer del servidor HTTP y la conexión.
type hijacked struct {
	r *bufio.Reader
	net.Conn
}

func (h *hijacked) Read(p []byte) (int, error) { return h.r.Read(p) }

// requestFromGuest dice si r viene de dentro: por loopback, o desde la propia
// IP del invitado (misma dirección en los dos extremos).
func requestFromGuest(r *http.Request) bool {
	rh, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return true
	}
	rip := net.ParseIP(rh)
	if rip == nil || rip.IsLoopback() {
		return true
	}
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if lh, _, err := net.SplitHostPort(la.String()); err == nil {
			if lip := net.ParseIP(lh); lip != nil && lip.Equal(rip) {
				return true
			}
		}
	}
	return false
}
