//go:build unix

// Package linkbroker es el canal por el que kling-vz (macOS) pide al daemon
// una conexión YA ABIERTA hacia otra máquina: una arista link o credential de
// un grafo, o la copia de `kling db attach` (docs/grafos.md, SECURITY.md).
//
// POR QUÉ ASÍ: en Linux el proxy de cada arista es del daemon, y resuelve y
// marca bajo el candado del manager en cada conexión. En macOS la red del
// invitado vive dentro de su kling-vz, un proceso confinado que no conoce a
// las demás máquinas. Si el daemon le devolviera una DIRECCIÓN (el reenvío
// 127.0.0.1:29xxx del destino), entre la respuesta y el dial de kling-vz el
// destino podría congelarse y otra máquina heredar ese puerto: el invitado
// hablaría con el de otro (el TOCTOU que el diseño prohíbe). Por eso el flujo
// va al revés: kling-vz dice QUÉ quiere (la arista), el daemon comprueba,
// resuelve y MARCA él mismo, y le entrega el socket conectado (SCM_RIGHTS).
// kling-vz nunca ve ni elige una dirección.
//
// EL PROTOCOLO, una petición por conexión Unix:
//
//	kling-vz -> daemon   una línea JSON (Request), como mucho MaxRequest bytes
//	daemon  -> kling-vz  una línea JSON (Response); si OK, en el MISMO
//	                     mensaje, el descriptor del socket TCP ya conectado
//
// y la conexión Unix sigue abierta mientras dure la sesión: es su
// arrendamiento. Quien la cierra termina la sesión. kling-vz la cierra al
// acabar; el daemon, para invalidarla (el destino se congela, se para o se
// borra), hace además shutdown del socket TCP, que comparte con kling-vz, y la
// sesión muere en los dos lados aunque kling-vz no colabore. Si el daemon se
// reinicia, el arrendamiento se cae y kling-vz corta la sesión: ninguna
// sobrevive sin alguien que la pueda invalidar.
//
// QUIÉN PREGUNTA no va en la petición: el daemon lo sabe por el socket (el PID
// del otro extremo es el kling-vz de una máquina concreta). La petición solo
// nombra la arista, y el daemon la comprueba contra el grafo y las etiquetas
// de ESA máquina.
//
// Solo biblioteca estándar: lo usan el núcleo y kling-vz.
package linkbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"syscall"
	"time"
)

const (
	// Version es la versión del protocolo. Un daemon que no la entienda
	// rechaza la petición.
	Version = 1

	// KindLink: una arista link. Host es <nodo>.graph y Port el del destino.
	KindLink = "link"
	// KindMachine: una credencial Postgres con UpstreamMachine (una arista
	// credential o kling db attach). Machine es el ID que lleva la
	// credencial (el de un nodo o el de una copia), Owner su dueño.
	KindMachine = "machine"

	// MaxRequest y MaxResponse acotan una línea de cada lado.
	MaxRequest  = 4 << 10
	MaxResponse = 4 << 10

	// GraphSuffix es el sufijo de los nombres de grafo (api.GraphDomain).
	GraphSuffix = ".graph"
)

// Motivos de un rechazo (Response.Reason), los mismos que la auditoría de
// pkg/credproxy.
const (
	ReasonBusy               = "busy"
	ReasonNoCapacity         = "no_capacity"
	ReasonMachineUnavailable = "machine_unavailable"
	ReasonUpstreamError      = "upstream_error"
)

// Request es lo que kling-vz pide.
type Request struct {
	V       int    `json:"v"`
	Kind    string `json:"kind"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port"`
	Machine string `json:"machine,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

// Response es lo que el daemon contesta.
type Response struct {
	OK bool `json:"ok"`
	// Machine es la máquina a la que va la conexión (para la auditoría).
	Machine string `json:"machine,omitempty"`
	Error   string `json:"error,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

var (
	reNodo    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	reMaquina = regexp.MustCompile(`^[0-9a-f]{16,64}$`)
	reDueño   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Validate comprueba la forma de la petición (no si está permitida: eso es
// del daemon).
func (r Request) Validate() error {
	if r.V != Version {
		return fmt.Errorf("unsupported link broker version %d (want %d)", r.V, Version)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("port %d out of range", r.Port)
	}
	switch r.Kind {
	case KindLink:
		if r.Machine != "" || r.Owner != "" {
			return errors.New("a link request names a host, not a machine")
		}
		if _, err := NodoDeHost(r.Host); err != nil {
			return err
		}
	case KindMachine:
		if r.Host != "" {
			return errors.New("a machine request names a machine, not a host")
		}
		if !reMaquina.MatchString(r.Machine) {
			return fmt.Errorf("machine %q is not a machine id", r.Machine)
		}
		if !reDueño.MatchString(r.Owner) {
			return fmt.Errorf("owner %q is not valid", r.Owner)
		}
	default:
		return fmt.Errorf("unknown request kind %q", r.Kind)
	}
	return nil
}

// NodoDeHost da el nodo de un nombre <nodo>.graph.
func NodoDeHost(host string) (string, error) {
	if len(host) <= len(GraphSuffix) || host[len(host)-len(GraphSuffix):] != GraphSuffix {
		return "", fmt.Errorf("host %q is not a graph name", host)
	}
	nodo := host[:len(host)-len(GraphSuffix)]
	if !reNodo.MatchString(nodo) {
		return "", fmt.Errorf("host %q is not a graph name", host)
	}
	return nodo, nil
}

// ── lado de kling-vz ─────────────────────────────────────────────────────────

// Pedir manda req por c (recién conectada al daemon) y espera la respuesta.
// Con OK devuelve el socket TCP entregado; c queda abierta como
// arrendamiento de la sesión y es de quien llama cerrarla. Con un rechazo,
// devuelve la respuesta y un error, y cierra c. El plazo es el de la
// resolución entera (despertar el destino incluido).
func Pedir(c *net.UnixConn, req Request, plazo time.Duration) (*net.TCPConn, Response, error) {
	var resp Response
	if err := req.Validate(); err != nil {
		c.Close()
		return nil, resp, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		c.Close()
		return nil, resp, err
	}
	_ = c.SetDeadline(time.Now().Add(plazo))
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, resp, fmt.Errorf("asking the daemon: %w", err)
	}
	linea, fds, err := leerLinea(c, MaxResponse)
	if err != nil {
		cerrarFDs(fds)
		c.Close()
		return nil, resp, fmt.Errorf("reading the daemon's answer: %w", err)
	}
	_ = c.SetDeadline(time.Time{})
	if err := json.Unmarshal(linea, &resp); err != nil {
		cerrarFDs(fds)
		c.Close()
		return nil, resp, fmt.Errorf("unreadable answer from the daemon: %w", err)
	}
	if !resp.OK {
		cerrarFDs(fds)
		c.Close()
		msg := resp.Error
		if msg == "" {
			msg = "refused"
		}
		return nil, resp, errors.New(msg)
	}
	if len(fds) != 1 {
		cerrarFDs(fds)
		c.Close()
		return nil, resp, fmt.Errorf("the daemon handed %d descriptors, want 1", len(fds))
	}
	tc, err := conexionTCP(fds[0])
	if err != nil {
		c.Close()
		return nil, resp, err
	}
	return tc, resp, nil
}

// conexionTCP convierte el descriptor recibido en una conexión TCP; cualquier
// otra cosa (un fichero, un socket Unix) se rechaza.
func conexionTCP(fd int) (*net.TCPConn, error) {
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "linkbroker")
	nc, err := net.FileConn(f)
	f.Close() // FileConn duplica el descriptor
	if err != nil {
		return nil, fmt.Errorf("the descriptor from the daemon is not a socket: %w", err)
	}
	tc, ok := nc.(*net.TCPConn)
	if !ok {
		nc.Close()
		return nil, fmt.Errorf("the descriptor from the daemon is not a TCP connection (%T)", nc)
	}
	return tc, nil
}

// ── lado del daemon ──────────────────────────────────────────────────────────

// LeerPeticion lee y valida la petición de c. Un descriptor que viniera con
// ella (nadie debe mandarlo) se cierra.
func LeerPeticion(c *net.UnixConn, plazo time.Duration) (Request, error) {
	var req Request
	_ = c.SetReadDeadline(time.Now().Add(plazo))
	linea, fds, err := leerLinea(c, MaxRequest)
	cerrarFDs(fds)
	if err != nil {
		return req, err
	}
	_ = c.SetReadDeadline(time.Time{})
	dec := json.NewDecoder(bytes.NewReader(linea))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid request: %w", err)
	}
	return req, req.Validate()
}

// Rechazar contesta que no.
func Rechazar(c *net.UnixConn, resp Response) error {
	resp.OK = false
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write(append(b, '\n'))
	return err
}

// Entregar contesta que sí y pasa el descriptor de tc en el mismo mensaje. tc
// sigue abierta en quien llama: es su copia del socket, con la que puede
// cortarlo (Cortar) mientras dure el arrendamiento.
func Entregar(c *net.UnixConn, resp Response, tc *net.TCPConn) error {
	resp.OK = true
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	var werr error
	if err := rc.Control(func(fd uintptr) {
		var n int
		n, _, werr = c.WriteMsgUnix(b, syscall.UnixRights(int(fd)), nil)
		if werr == nil && n != len(b) {
			werr = fmt.Errorf("short write (%d of %d bytes)", n, len(b))
		}
	}); err != nil {
		return err
	}
	return werr
}

// Cortar termina la sesión en los DOS lados: shutdown actúa sobre el socket,
// no sobre el descriptor, así que la copia de kling-vz deja de leer y de
// escribir aunque siga abierta. Luego suelta la copia propia.
func Cortar(tc *net.TCPConn) {
	_ = tc.CloseRead()
	_ = tc.CloseWrite()
	_ = tc.Close()
}

// EsperarFin bloquea hasta que el otro extremo cierre c (o mande algo, que
// en este protocolo no toca): el fin del arrendamiento.
func EsperarFin(c net.Conn) {
	var b [1]byte
	_, _ = c.Read(b[:])
}

// ── comunes ──────────────────────────────────────────────────────────────────

// leerLinea lee hasta '\n' (sin incluirlo), como mucho max bytes, y recoge los
// descriptores que lleguen por el camino (el daemon no espera ninguno, pero
// se recogen igual para cerrarlos).
func leerLinea(c *net.UnixConn, max int) ([]byte, []int, error) {
	var (
		datos []byte
		fds   []int
		buf   = make([]byte, 512)
		oob   = make([]byte, syscall.CmsgSpace(4*4))
	)
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if oobn > 0 {
			fds = append(fds, derechos(oob[:oobn])...)
		}
		if n > 0 {
			datos = append(datos, buf[:n]...)
			if i := bytes.IndexByte(datos, '\n'); i >= 0 {
				return datos[:i], fds, nil
			}
			if len(datos) > max {
				return nil, fds, fmt.Errorf("line longer than %d bytes", max)
			}
		}
		if err != nil {
			return nil, fds, err
		}
		if n == 0 && oobn == 0 {
			return nil, fds, errors.New("connection closed before a full line")
		}
	}
}

// derechos extrae los descriptores de un mensaje de control.
func derechos(oob []byte) []int {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var out []int
	for _, m := range msgs {
		if fds, err := syscall.ParseUnixRights(&m); err == nil {
			out = append(out, fds...)
		}
	}
	return out
}

func cerrarFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}
