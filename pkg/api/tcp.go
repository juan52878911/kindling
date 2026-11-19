package api

// Túnel TCP a un puerto del invitado: GET /machines/{ref}/tcp?port=N con
// Upgrade: kling-tcp/1. Tras el 101 la conexión lleva los bytes del puerto N
// de la máquina tal cual, en los dos sentidos (WebSocket, binario, lo que sea).
// Sirve igual por el socket unix que por SSH: el cliente solo habla con el
// daemon.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CapabilityTCPTunnel es la capacidad de GET /info que dice que el daemon
// sirve GET /machines/{ref}/tcp.
const CapabilityTCPTunnel = "tcp-tunnel"

// TCPProto es el nombre del protocolo en la cabecera Upgrade.
const TCPProto = "kling-tcp/1"

// ErrTCPUnsupported es lo que devuelve DialTCP cuando el daemon no tiene la
// ruta (uno anterior a la capacidad tcp-tunnel).
var ErrTCPUnsupported = errors.New("the daemon does not offer " + CapabilityTCPTunnel)

// DialTCP abre un túnel al puerto port de la máquina ref y devuelve la
// conexión cruda. El puerto tiene que estar en la etiqueta kling.ports de la
// máquina; si está congelada, el daemon la despierta.
//
// Al volver, la conexión ya no la gestiona el http.Client: cancelar el
// contexto NO la cierra, así que quien llama tiene que cerrarla él. Los
// plazos (SetDeadline) se ignoran: el transporte no los admite; ciérrala
// para cortar una lectura.
func (c *Client) DialTCP(ctx context.Context, ref string, port int) (net.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://kling/machines/"+url.PathEscape(ref)+"/tcp?port="+strconv.Itoa(port), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", TCPProto)

	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUpgradeRequired || resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			return nil, fmt.Errorf("%w: %s", ErrTCPUnsupported, resp.Status)
		}
		return nil, statusErrorFrom(resp)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("the connection could not be taken over for %s", TCPProto)
	}
	return &tcpConn{rwc: rwc, ref: ref, port: port}, nil
}

// tcpConn es la conexión del túnel como net.Conn.
type tcpConn struct {
	rwc  io.ReadWriteCloser
	ref  string
	port int
}

type tcpAddr struct {
	ref  string
	port int
}

func (a tcpAddr) Network() string { return "kling-tcp" }
func (a tcpAddr) String() string  { return a.ref + ":" + strconv.Itoa(a.port) }

func (c *tcpConn) Read(b []byte) (int, error)  { return c.rwc.Read(b) }
func (c *tcpConn) Write(b []byte) (int, error) { return c.rwc.Write(b) }
func (c *tcpConn) Close() error                { return c.rwc.Close() }
func (c *tcpConn) LocalAddr() net.Addr         { return tcpAddr{"local", 0} }
func (c *tcpConn) RemoteAddr() net.Addr        { return tcpAddr{c.ref, c.port} }

func (c *tcpConn) SetDeadline(time.Time) error      { return nil }
func (c *tcpConn) SetReadDeadline(time.Time) error  { return nil }
func (c *tcpConn) SetWriteDeadline(time.Time) error { return nil }
