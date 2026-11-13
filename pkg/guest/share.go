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
	"strconv"
	"strings"
	"sync"
	"time"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// Shares son las carpetas vivas montadas en este invitado, por tag.
type Shares struct {
	mu     sync.Mutex
	mounts map[int]*shareMount

	// mount monta el sistema de ficheros y devuelve su /dev/fuse. Variable
	// para probar el manejador sin root ni Linux.
	mount func(target string, readOnly bool) (fuseDev, func(), error)
	// mountVirtio monta el dispositivo virtio-fs de tag (kling-hv) y
	// afterVirtio corre tras contestar (la lectura adelantada). Variables
	// por lo mismo.
	mountVirtio func(tag, target string, readOnly bool) (func(), error)
	afterVirtio func(target string)
	// fromGuest dice si una petición viene del propio invitado. Variable por lo
	// mismo: en los tests todo llega por loopback.
	fromGuest func(r *http.Request) bool

	// withSession son las carpetas que ya tuvieron sesión, y changed se
	// cierra con cada una nueva (share_wait.go).
	withSession map[int]bool
	changed     chan struct{}
}

type shareMount struct {
	a       share.Attach
	fs      *fuseFS // nil por virtio-fs: el servidor FUSE está en el host
	unmount func()
}

// virtio dice si el montaje es por virtio-fs.
func (m *shareMount) virtio() bool { return m.a.Transport == share.TransportVirtioFS }

// mismaCarpeta compara dos Attach sin mirar por dónde llegan.
func mismaCarpeta(a, b share.Attach) bool {
	return a.Tag == b.Tag && a.Mount == b.Mount && a.Mode == b.Mode
}

var shareState = newShares()

func newShares() *Shares {
	return &Shares{mounts: map[int]*shareMount{}, mount: mountFUSE, fromGuest: requestFromGuest,
		mountVirtio: mountVirtioFS, afterVirtio: readaheadVirtioFS}
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

		if a.Transport == share.TransportVirtioFS {
			s.attachVirtio(w, a)
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
		sess := newShareSession(&hijacked{r: brw.Reader, Conn: conn})
		sess.hello()
		watching := sess.watch()
		m.fs.attach(sess)
		s.noteAttached(a.Tag)
		log.Printf("share %d attached at %s (%s, %d KiB I/O, host changes: %v)", a.Tag, a.Mount, a.Mode, sess.maxIO>>10, watching)
	}
}

// mountFor devuelve el montaje de a.Tag, montándolo si es la primera vez.
func (s *Shares) mountFor(a share.Attach) (*shareMount, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.mounts[a.Tag]; m != nil {
		if !mismaCarpeta(m.a, a) || m.virtio() {
			return nil, http.StatusConflict, fmt.Errorf("share %d is already mounted at %s (%s%s)", a.Tag, m.a.Mount, m.a.Mode, transporte(m.a))
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
	fs.afterInit = setReadahead
	go fs.serve()
	m := &shareMount{a: a, fs: fs, unmount: unmount}
	s.mounts[a.Tag] = m
	log.Printf("share %d mounted at %s (%s)", a.Tag, a.Mount, a.Mode)
	return m, 0, nil
}

// transporte es ", virtio-fs" para los mensajes de un montaje por virtio-fs.
func transporte(a share.Attach) string {
	if a.Transport == share.TransportVirtioFS {
		return ", virtio-fs"
	}
	return ""
}

// attachVirtio atiende un attach por virtio-fs: monta el dispositivo de la
// carpeta la primera vez y contesta 200 (la sesión no va por aquí: el daemon
// la conecta con kling-hv). 501 con HeaderTransport "fuse" si no se puede, para
// que el daemon repita por FUSE: un kernel sin virtio-fs, una máquina sin el
// dispositivo (un VMM que no lo tiene) o una carpeta ya montada por FUSE.
func (s *Shares) attachVirtio(w http.ResponseWriter, a share.Attach) {
	code, err := s.mountVirtioFor(a)
	if err != nil {
		if code == http.StatusNotImplemented {
			w.Header().Set(share.HeaderTransport, share.TransportFUSE)
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set(share.HeaderTransport, share.TransportVirtioFS)
	w.WriteHeader(http.StatusOK)
	s.noteAttached(a.Tag)
	if s.afterVirtio != nil {
		go s.afterVirtio(a.Mount)
	}
}

func (s *Shares) mountVirtioFor(a share.Attach) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.mounts[a.Tag]; m != nil {
		if !mismaCarpeta(m.a, a) {
			return http.StatusConflict, fmt.Errorf("share %d is already mounted at %s (%s%s)", a.Tag, m.a.Mount, m.a.Mode, transporte(m.a))
		}
		if !m.virtio() {
			return http.StatusNotImplemented, fmt.Errorf("share %d is mounted over FUSE", a.Tag)
		}
		return http.StatusOK, nil
	}
	for _, m := range s.mounts {
		if m.a.Mount == a.Mount {
			return http.StatusConflict, fmt.Errorf("%s already has share %d mounted", a.Mount, m.a.Tag)
		}
	}
	unmount, err := s.mountVirtio(share.VirtioFSTag(a.Tag), a.Mount, a.Mode == share.ModeRO)
	if err != nil {
		return http.StatusNotImplemented, fmt.Errorf("mounting %s over virtio-fs: %w", a.Mount, err)
	}
	s.mounts[a.Tag] = &shareMount{a: a, unmount: unmount}
	log.Printf("share %d mounted at %s (%s, virtio-fs)", a.Tag, a.Mount, a.Mode)
	return http.StatusOK, nil
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

// requestFromGuest dice si r NO viene del host. Es una lista positiva: por
// TCP solo cuenta como host quien llega por la ruta por defecto del invitado
// (ver llegaPorLaPasarela); todo lo demás —loopback, la propia IP, otra de sus
// direcciones, un contenedor con red puente o cualquier red que el invitado
// tenga conectada directamente— es de dentro. Por vsock, todo CID que no sea
// el del host.
func requestFromGuest(r *http.Request) bool {
	rh, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return true
	}
	// Por vsock (kling-vz, kling-hv) la dirección es un CID: solo el host
	// (2) cuenta como de fuera. Antes se miraba como IP, no lo era, y toda
	// carpeta viva conectada por vsock se rechazaba.
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && la.Network() == "vsock" {
		return rh != strconv.Itoa(vmaddrCIDHost)
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
	// Una dirección propia sin ruta en la tabla principal (una /32 en una
	// interfaz dummy) caería en la ruta por defecto: se descarta antes.
	if esDireccionPropia(rip) {
		return true
	}
	return !llegaPorLaPasarela(rip)
}

// direccionesPropias da las IPs de las interfaces del invitado (variable para
// los tests). Un error deja la lista vacía: lo de arriba ya cubre loopback y
// la IP por la que entró la conexión, que es el caso de siempre.
var direccionesPropias = func() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ips = append(ips, n.IP)
		}
	}
	return ips
}

func esDireccionPropia(ip net.IP) bool {
	for _, p := range direccionesPropias() {
		if p.Equal(ip) {
			return true
		}
	}
	return false
}
