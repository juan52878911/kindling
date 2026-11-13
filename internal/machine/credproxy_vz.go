//go:build darwin

package machine

// El proxy de credenciales aparte de cada máquina en macOS (kling-credproxy,
// docs/proxy-macos-separado.md): el único proceso con las claves de esa
// máquina. El VMM (kling-vz o kling-hv) atiende la red del invitado y le pasa
// las conexiones a la pasarela por un socket; las claves ya no están en él.
//
// QUIÉN LO LANZA: el daemon, al entregar las credenciales de una máquina cuyo
// VMM anuncia credproxy.CapSplit (registrarCredencialesPlataforma). El binario
// va junto al VMM; su directorio es machines/<id>/credproxy, que crea el
// daemon (0700) y donde el perfil del VMM no escribe.
//
// CUÁNTO VIVE: lo que su VMM. Lo vigila con kqueue y sale en cuanto muere,
// así que stop, freeze y rm (que matan al VMM) se lo llevan también con el
// daemon caído; tras un thaw el VMM es otro y el daemon lanza otro proxy al
// devolverle las claves. Un reinicio del daemon no lo toca: el siguiente que
// le habla lo encuentra por su socket (readopción).
//
// A QUIÉN SE FÍA: el daemon solo le da las claves a un proceso de este
// usuario cuyo ejecutable es el kling-credproxy junto al VMM y cuya línea de
// órdenes dice ese VMM (--vmm-pid): un VMM tomado que plantara un socket en
// ese directorio (su perfil no le deja, y además) no recibiría nada. Él solo
// acepta en su API al ejecutable del daemon, y en su reenvío al del VMM.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// plazoCredproxy es cuánto se espera a que un proxy recién lanzado conteste.
var plazoCredproxy = 5 * time.Second

// errCredproxyAjeno: al otro lado del socket del proxy hay otro programa.
var errCredproxyAjeno = errors.New("the credential proxy socket is held by another program")

// credproxyDeOtroVMM: el proxy que escucha es de otro VMM (uno anterior de la
// misma máquina que, contra lo esperado, sigue vivo).
type credproxyDeOtroVMM struct{ pid int }

func (e *credproxyDeOtroVMM) Error() string {
	return fmt.Sprintf("the credential proxy (pid %d) belongs to another VMM", e.pid)
}

// vmmDeSocket es el VMM al otro lado de su socket: lo rellena la primera
// petición de pidPorSocket.
type vmmDeSocket struct {
	mu  sync.Mutex
	pid int
	exe string
}

// pidPorSocket es un cliente del VMM de c que, al conectar, anota quién está
// al otro lado (LOCAL_PEERPID y la ruta de su ejecutable).
func pidPorSocket(c *fc.Client) (*vmmDeSocket, *fc.Client) {
	v := &vmmDeSocket{}
	sock := c.Socket()
	return v, fc.NewConDial(sock, "kling-vz", func(ctx context.Context) (net.Conn, error) {
		conn, err := fc.DialUnix(ctx, sock)
		if err != nil {
			return nil, err
		}
		if uc, ok := conn.(*net.UnixConn); ok {
			if pid, err := pidDelOtro(uc); err == nil {
				exe, _ := rutaEjecutable(pid)
				v.mu.Lock()
				v.pid, v.exe = pid, exe
				v.mu.Unlock()
			}
		}
		return conn, nil
	})
}

func (v *vmmDeSocket) datos() (int, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pid, v.exe
}

// credproxyDe devuelve un cliente del API del proxy aparte de la máquina
// (cpdir es su directorio) cuyo VMM es el proceso vmm. Si no corre, lo lanza
// cuando lanzar; si no, devuelve nil sin error. Cada conexión del cliente
// comprueba de nuevo quién está al otro lado (dialCredproxy).
func credproxyDe(ctx context.Context, cpdir string, vmm *vmmDeSocket, lanzar bool) (*fc.Client, error) {
	vmmPID, vmmExe := vmm.datos()
	if vmmPID <= 0 || vmmExe == "" {
		return nil, errors.New("cannot tell which process is the machine's VMM (LOCAL_PEERPID on its socket)")
	}
	bin := filepath.Join(filepath.Dir(vmmExe), credproxy.SplitBinary)
	api := filepath.Join(cpdir, credproxy.SplitAPISock)
	dial := func(ctx context.Context) (net.Conn, error) {
		c, _, err := dialCredproxy(ctx, api, bin, vmmPID)
		return c, err
	}
	cp := fc.NewConDial(api, "kling-credproxy", dial)
	conn, pid, err := dialCredproxy(ctx, api, bin, vmmPID)
	if err == nil {
		conn.Close()
		// Es el suyo, pero puede no aceptar a este daemon: solo admite el
		// ejecutable con el que lo lanzaron, y uno actualizado en otra ruta
		// (Homebrew cambia de Cellar) es otro. No guarda nada que no se le
		// pueda volver a dar: se cambia por uno nuevo.
		_, ierr := cp.ConPlazo(2 * time.Second).KlingInfo(ctx)
		if ierr == nil {
			return cp, nil
		}
		if pid <= 0 {
			return nil, fmt.Errorf("the credential proxy does not answer: %w", ierr)
		}
		log.Printf("%s: the credential proxy (pid %d) does not answer this daemon (%v): replacing it", shortID(filepath.Base(filepath.Dir(cpdir))), pid, ierr)
		matarYEsperar(pid)
		err = ierr
	}
	var otro *credproxyDeOtroVMM
	switch {
	case errors.As(err, &otro):
		// Sale solo cuando muere su VMM; si aún está, que no se quede con el
		// socket (ni con unas claves que ya no son de nadie).
		matarYEsperar(otro.pid)
	case errors.Is(err, errCredproxyAjeno):
		return nil, err
	}
	if !lanzar {
		return nil, nil
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("%s not found next to %s: on macOS the credential proxy runs in its own process; reinstall kindling (or `make vz install`)", credproxy.SplitBinary, vmmExe)
	}
	if err := prepararDirCredproxy(cpdir); err != nil {
		return nil, err
	}
	if err := lanzarCredproxy(bin, cpdir, vmmPID, vmmExe); err != nil {
		return nil, err
	}
	limite := time.Now().Add(plazoCredproxy)
	espera := time.Millisecond
	for {
		conn, _, err := dialCredproxy(ctx, api, bin, vmmPID)
		if err == nil {
			conn.Close()
			return cp, nil
		}
		if errors.Is(err, errCredproxyAjeno) || ctx.Err() != nil || time.Now().After(limite) {
			msg := "launching kling-credproxy: it did not answer: %w"
			if l := ultimaLinea(filepath.Join(filepath.Dir(cpdir), credproxy.SplitLog)); l != "" {
				msg += "; last line of its log: " + l
			}
			return nil, fmt.Errorf(msg, err)
		}
		time.Sleep(espera)
		espera = min(2*espera, 20*time.Millisecond)
	}
}

// matarYEsperar manda SIGKILL a pid y espera (poco) a que deje de existir,
// para que el nuevo encuentre libre el socket.
func matarYEsperar(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

// dialCredproxy conecta al API del proxy y comprueba que al otro lado está el
// kling-credproxy bin de este usuario, y el del VMM vmmPID. Devuelve también
// su PID.
func dialCredproxy(ctx context.Context, api, bin string, vmmPID int) (net.Conn, int, error) {
	conn, err := fc.DialUnix(ctx, api)
	if err != nil {
		return nil, 0, err
	}
	pid, err := comprobarCredproxy(conn, bin, vmmPID)
	if err != nil {
		conn.Close()
		return nil, 0, err
	}
	return conn, pid, nil
}

func comprobarCredproxy(conn net.Conn, bin string, vmmPID int) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errCredproxyAjeno
	}
	uid, err := uidDelOtro(uc)
	if err != nil {
		return 0, err
	}
	if uid != os.Geteuid() {
		return 0, fmt.Errorf("%w: it runs as uid %d", errCredproxyAjeno, uid)
	}
	pid, err := pidDelOtro(uc)
	if err != nil {
		return 0, err
	}
	ruta, err := rutaEjecutable(pid)
	if err != nil {
		return 0, err
	}
	if !mismoEjecutable(ruta, bin) {
		return 0, fmt.Errorf("%w: pid %d runs %s", errCredproxyAjeno, pid, ruta)
	}
	linea, err := lineaDeOrdenes(int32(pid), make([]byte, procArgsMax))
	if err != nil {
		return 0, err
	}
	if v, _, ok := argsCredproxy(linea); !ok || v != vmmPID {
		return 0, &credproxyDeOtroVMM{pid: pid}
	}
	return pid, nil
}

// prepararDirCredproxy crea el directorio del proxy (0700) o comprueba que el
// que hay es un directorio nuestro que nadie más toca.
func prepararDirCredproxy(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("credential proxy directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("credential proxy directory: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("credential proxy directory %s is not a directory of this user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("credential proxy directory: %w", err)
		}
	}
	return nil
}

// lanzarCredproxy arranca el proxy desacoplado del daemon (setsid: sobrevive
// a que este se reinicie), con su salida en machines/<id>/credproxy.log y el
// entorno justo: el del VMM (entornoProceso, sin KLING_TOKEN ni nada de la
// sesión) más quién es el daemon, quién el VMM, el broker y el tamaño del
// registro.
func lanzarCredproxy(bin, cpdir string, vmmPID int, vmmExe string) error {
	mdir := filepath.Dir(cpdir)
	logf, err := os.OpenFile(filepath.Join(mdir, credproxy.SplitLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("credential proxy log: %w", err)
	}
	defer logf.Close()
	daemon, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(daemon); err == nil {
		daemon = r
	}
	env := []string{"KLING_CREDPROXY_DAEMON=" + daemon, "KLING_CREDPROXY_VMM=" + vmmExe}
	// La raíz es la de machines/<id>: el broker es el suyo.
	if b, err := rutaBroker(filepath.Dir(filepath.Dir(mdir))); err == nil {
		env = append(env, "KLING_VZ_BROKER="+b)
	}
	if c := credAuditConfig(); c != (credproxy.AuditConfig{}) {
		env = append(env, "KLING_VZ_CREDAUDIT="+c.String())
	}
	cmd := exec.Command(bin, "--vmm-pid", strconv.Itoa(vmmPID), "--dir", cpdir)
	cmd.Env = entornoProceso(env)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching kling-credproxy: %w", err)
	}
	id := filepath.Base(mdir)
	log.Printf("%s: credential proxy started (kling-credproxy pid %d, VMM pid %d)", shortID(id), cmd.Process.Pid, vmmPID)
	// Sin Wait quedaría zombi al terminar.
	go func() {
		err := cmd.Wait()
		if syscall.Kill(vmmPID, 0) == nil {
			log.Printf("%s: kling-credproxy (pid %d) exited while its VMM is alive (%v): the machine's credentials don't work until the next thaw or credential change", shortID(id), cmd.Process.Pid, err)
		}
	}()
	return nil
}

// readoptarCredproxy: el daemon se reinició y la máquina siguió viva. Su
// proxy aparte también (es de su VMM, no del daemon): se comprueba que sigue
// ahí y se dice, sin entregarle nada. Si no está, lo dice: sus credenciales
// no funcionan hasta que se le vuelvan a dar (thaw, o un cambio de
// credenciales). Corre con m.mu tomado (reconcile): no toca el manager.
func readoptarCredproxy(cpdir string) {
	id := filepath.Base(filepath.Dir(cpdir))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := fc.DialUnix(ctx, filepath.Join(cpdir, credproxy.SplitAPISock))
	if err != nil {
		if _, serr := os.Stat(cpdir); serr == nil {
			log.Printf("%s: its credential proxy is not running (%v): its credentials won't work until the next thaw or credential change", shortID(id), err)
		}
		return
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	pid, err := pidDelOtro(uc)
	if err != nil {
		return
	}
	ruta, _ := rutaEjecutable(pid)
	if filepath.Base(ruta) != credproxy.SplitBinary {
		log.Printf("%s: WARNING: the credential proxy socket is held by pid %d (%s), not kling-credproxy", shortID(id), pid, ruta)
		return
	}
	log.Printf("%s: credential proxy readopted (kling-credproxy pid %d)", shortID(id), pid)
}

// ultimaLinea es la última línea no vacía de un fichero de log, o "".
func ultimaLinea(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}
