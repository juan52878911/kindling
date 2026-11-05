package server

// Carpetas vivas por virtio-fs (solo kling-hv; docs/backend-hv.md): PUT
// /kling/shares pide un dispositivo por carpeta antes de arrancar, y POST
// /kling/share/attach conecta la sesión kling-share/1 del daemon con el
// servidor FUSE de ese dispositivo, como hace el agente con las de FUSE.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// VirtioFSFactory es la Factory que sabe crear dispositivos virtio-fs
// (Spec.VirtioFS): kling-hv. kling-vz no la implementa y PUT /kling/shares
// le contesta que no, para que el núcleo siga por FUSE.
type VirtioFSFactory interface {
	VirtioFS() bool
}

// ShareVM es la VM con carpetas por virtio-fs. AttachShare pone la sesión
// kling-share/1 de la carpeta tag (la conexión ya es suya); ShareState da lo
// que el servidor FUSE de cada carpeta tiene que guardar en un snapshot, por
// tag, con la VM en pausa y su estado ya volcado.
type ShareVM interface {
	AttachShare(tag int, rwc io.ReadWriteCloser) error
	ShareState() (map[int][]byte, error)
}

// shareSockBuf es fc.ShareSockBuf del núcleo (dos escrituras máximas).
const shareSockBuf = 2 << 20

// errNoVirtioFS: este VMM no hace virtio-fs (o no tiene esa carpeta). 501, que
// el núcleo lee como "por FUSE" (fc.ErrNoVirtioFS).
var errNoVirtioFS = errors.New("this VMM has no virtio-fs")

func faultCode(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": err.Error()})
}

func (s *Server) putKlingShares(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Shares []spec.VirtioFS `json:"shares"`
	}
	if err := decode(r, maxConfigBody, &b); err != nil {
		fault(w, err)
		return
	}
	if f, ok := s.d.Factory.(VirtioFSFactory); !ok || !f.VirtioFS() {
		faultCode(w, http.StatusNotImplemented, errNoVirtioFS)
		return
	}
	for i := range b.Shares {
		b.Shares[i].State = nil // eso solo lo escribe el snapshot
	}
	if err := spec.ValidateVirtioFS(b.Shares); err != nil {
		fault(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	s.spec.VirtioFS = b.Shares
	noContent(w)
}

func (s *Server) postShareAttach(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), share.Proto) {
		faultCode(w, http.StatusUpgradeRequired, fmt.Errorf("this endpoint speaks %s; ask for it with Upgrade", share.Proto))
		return
	}
	var b struct {
		Tag int `json:"tag"`
	}
	if err := decode(r, maxConfigBody, &b); err != nil {
		fault(w, err)
		return
	}
	s.mu.Lock()
	vm, st := s.vm, s.st
	tiene := false
	for _, f := range s.spec.VirtioFS {
		tiene = tiene || f.Tag == b.Tag
	}
	s.mu.Unlock()
	sv, ok := vm.(ShareVM)
	if !ok || !tiene {
		faultCode(w, http.StatusNotImplemented, fmt.Errorf("%w for share %d", errNoVirtioFS, b.Tag))
		return
	}
	if st != stRunning && st != stPaused {
		faultCode(w, http.StatusServiceUnavailable, fmt.Errorf("the microVM is %s", st))
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		faultCode(w, http.StatusInternalServerError, err)
		return
	}
	// Sin plazos: la conexión vive lo que viva la sesión. Y con búferes
	// grandes, como en el otro extremo (fc.ShareSockBuf): los de un socket unix
	// de macOS son de 8 KiB.
	_ = conn.SetDeadline(time.Time{})
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.SetReadBuffer(shareSockBuf)
		_ = uc.SetWriteBuffer(shareSockBuf)
	}
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
		share.Proto + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		conn.Close()
		return
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return
	}
	if err := sv.AttachShare(b.Tag, &hijacked{r: brw.Reader, Conn: conn}); err != nil {
		s.d.Logf("share %d: %v", b.Tag, err)
		conn.Close()
	}
}

// hijacked junta el lector con búfer del servidor HTTP (que puede traer bytes
// ya leídos) y la conexión.
type hijacked struct {
	r *bufio.Reader
	net.Conn
}

func (h *hijacked) Read(p []byte) (int, error) { return h.r.Read(p) }

// guardarEstadoShares pone en el spec lo que el servidor FUSE de cada carpeta
// por virtio-fs tiene que llevar en el snapshot. Con s.mu y la VM en pausa.
func (s *Server) guardarEstadoShares() error {
	if len(s.spec.VirtioFS) == 0 {
		return nil
	}
	sv, ok := s.vm.(ShareVM)
	if !ok {
		return nil
	}
	st, err := sv.ShareState()
	if err != nil {
		return fmt.Errorf("saving the virtio-fs state: %w", err)
	}
	for i := range s.spec.VirtioFS {
		s.spec.VirtioFS[i].State = st[s.spec.VirtioFS[i].Tag]
	}
	return nil
}

// soltarEstadoShares lo quita del spec en memoria una vez escrito: es grande y
// solo vale para ese snapshot.
func (s *Server) soltarEstadoShares() {
	for i := range s.spec.VirtioFS {
		s.spec.VirtioFS[i].State = nil
	}
}
