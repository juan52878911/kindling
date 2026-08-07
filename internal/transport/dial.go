// Package transport resuelve cómo llega el CLI al daemon.
//
// Local y remoto acaban en el mismo sitio: un socket Unix. El daemon nunca
// escucha en un puerto de red. Un daemon de microVMs es equivalente a root en su
// host — puede montar discos y arrancar kernels arbitrarios — así que exponerlo
// por TCP sería el mismo error que costó a Docker una década de servidores
// comprometidos.
//
// Para remoto se usa SSH con la misma técnica que `docker context`: en vez de
// exigir socat o nc en el destino, se invoca el propio binario con `dial-stdio`,
// que hace de puente entre la tubería SSH y el socket local del daemon.
package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"
)

// DefaultSocket es el socket del daemon local.
const DefaultSocket = "/run/kling.sock"

// Dialer abre conexiones al daemon según el endpoint configurado.
//
//	unix:///run/kling.sock        (o una ruta a secas)
//	ssh://usuario@host
type Dialer struct{ Endpoint string }

func New(endpoint string) *Dialer {
	if endpoint == "" {
		endpoint = DefaultSocket
	}
	return &Dialer{Endpoint: endpoint}
}

// Describe devuelve el endpoint en forma legible.
func (d *Dialer) Describe() string { return d.Endpoint }

func (d *Dialer) Dial(ctx context.Context) (net.Conn, error) {
	switch {
	case strings.HasPrefix(d.Endpoint, "ssh://"):
		return dialSSH(ctx, strings.TrimPrefix(d.Endpoint, "ssh://"))
	case strings.HasPrefix(d.Endpoint, "unix://"):
		return dialUnix(ctx, strings.TrimPrefix(d.Endpoint, "unix://"))
	default:
		return dialUnix(ctx, d.Endpoint)
	}
}

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("no puedo hablar con el daemon en %s: %w", path, err)
	}
	return c, nil
}

// dialSSH levanta `ssh <destino> kling dial-stdio` y envuelve sus tuberías.
func dialSSH(ctx context.Context, dest string) (net.Conn, error) {
	target, remoteBin := dest, "kling"
	if i := strings.Index(dest, "/"); i >= 0 { // ssh://host/ruta/al/kling
		target, remoteBin = dest[:i], dest[i:]
	}

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		target, remoteBin, "dial-stdio")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lanzando ssh a %s: %w", target, err)
	}
	return &pipeConn{r: stdout, w: stdin, cmd: cmd}, nil
}

// pipeConn presenta un par de tuberías como si fuera una conexión de red, para
// poder usar el mismo cliente HTTP con local y con SSH.
type pipeConn struct {
	r   io.ReadCloser
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }

func (p *pipeConn) Close() error {
	p.w.Close()
	p.r.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
	return nil
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "ssh" }
func (pipeAddr) String() string  { return "ssh" }

func (p *pipeConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (p *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

// ServeStdio conecta stdin/stdout con el socket local del daemon. Es el extremo
// remoto de dialSSH y no está pensado para uso manual.
func ServeStdio(socket string, in io.Reader, out io.Writer) error {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer c.Close()

	done := make(chan error, 2)
	go func() { _, err := io.Copy(c, in); done <- err }()
	go func() { _, err := io.Copy(out, c); done <- err }()
	<-done
	return nil
}
