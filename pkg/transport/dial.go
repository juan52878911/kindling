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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// DefaultSocket es el socket del daemon local en Linux. Se conserva como
// constante para quien ya la usa; el que vale en cada plataforma lo da
// DefaultSocketPath (en macOS vive en el directorio del usuario).
const DefaultSocket = "/run/kling.sock"

// Dialer abre conexiones al daemon según el endpoint configurado.
//
//	unix:///run/kling.sock        (o una ruta a secas)
//	ssh://usuario@host
type Dialer struct{ Endpoint string }

func New(endpoint string) *Dialer {
	if endpoint == "" {
		endpoint = DefaultSocketPath()
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
		return nil, fmt.Errorf("cannot talk to the daemon at %s: %w", path, err)
	}
	return c, nil
}

// userCacheDir y tmpBase son os.UserCacheDir y /tmp en producción; las
// pruebas los sustituyen para no tocar el HOME real.
var (
	userCacheDir = os.UserCacheDir
	tmpBase      = "/tmp"
)

// maxSunPath es el tamaño de sun_path: 104 bytes en macOS, 108 en Linux,
// contando el NUL final.
var maxSunPath = func() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}()

// sshTempSuffix es lo que ssh añade al ControlPath mientras crea el máster:
// escucha en "<ControlPath>.<16 caracteres>" y luego lo renombra. Si esa ruta
// temporal no cabe, ssh no multiplexa: sale con 255 ("too long for Unix domain
// socket") y la llamada entera falla.
const sshTempSuffix = 1 + 16

// sshControlPath devuelve el socket de control con el que ssh reutiliza la
// conexión entre invocaciones (`kling try` hace varias por llamada, y cada una
// paga el apretón de manos SSH entero si no), o "" si no hay dónde ponerlo:
// entonces se usa una conexión nueva por llamada, como siempre.
//
// El directorio tiene que ser privado de este usuario: en uno compartido
// cualquiera podría apuntar su propio ssh al mismo ControlPath y colarse en la
// conexión ya autenticada. Se prueba primero en el caché del usuario y, si la
// ruta no cabe en sun_path (un HOME largo en macOS), en /tmp/kling-<uid>.
//
// El nombre es un hash corto del destino y no el %C de ssh (40 caracteres): con
// %C la ruta en ~/Library/Caches ya no cabía en macOS.
func sshControlPath(target string) string {
	h := sha256.Sum256([]byte(target))
	name := "ssh-" + hex.EncodeToString(h[:8])
	var dirs []string
	if base, err := userCacheDir(); err == nil && base != "" {
		dirs = append(dirs, filepath.Join(base, "kindling", "ssh-control"))
	}
	dirs = append(dirs, filepath.Join(tmpBase, fmt.Sprintf("kling-%d", os.Getuid())))
	for _, dir := range dirs {
		path := filepath.Join(dir, name)
		if len(path)+sshTempSuffix >= maxSunPath {
			continue
		}
		if privateDir(dir) != nil {
			continue
		}
		return path
	}
	return ""
}

// privateDir crea dir si falta y comprueba que es un directorio (no un enlace)
// de este usuario y sin permisos para nadie más.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is not a private directory of this user", dir)
	}
	return nil
}

// sshMultiplexArgs arma las opciones de `ssh` para llegar a target. Si no hay
// dónde poner el socket de control, cae al modo de siempre: una conexión SSH
// nueva por llamada, sin fallar la llamada en sí.
func sshMultiplexArgs(target string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
	}
	path := sshControlPath(target)
	if path == "" {
		return args
	}
	return append(args,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+path,
		"-o", "ControlPersist=60s",
	)
}

// dialSSH levanta `ssh <destino> kling dial-stdio` y envuelve sus tuberías.
func dialSSH(ctx context.Context, dest string) (net.Conn, error) {
	target, remoteBin := dest, "kling"
	if i := strings.Index(dest, "/"); i >= 0 { // ssh://host/ruta/al/kling
		target, remoteBin = dest[:i], dest[i:]
	}

	args := append(sshMultiplexArgs(target), target, remoteBin, "dial-stdio")
	cmd := exec.CommandContext(ctx, "ssh", args...)

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
		return nil, fmt.Errorf("launching ssh to %s: %w", target, err)
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
