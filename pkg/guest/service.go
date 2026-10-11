package guest

// El servicio supervisado (api.ServiceSpec).
//
// Hasta ahora el servicio de una imagen lo lanzaba el /entrypoint con un bucle
// de shell (`while :; do svc; done &`) ANTES de ceder el PID 1 al agente: sin
// usuario, sin directorio, sin señal de parada, y arrancando antes de que el
// agente montara los volúmenes, así que un servicio que escribe en un volumen
// escribía en el overlay. Una imagen de Docker necesita las cuatro cosas.
//
// Aquí lo lanza el agente, después de montar, con el usuario y el directorio
// de la imagen, en su propio grupo de procesos (para matarlo con sus hijos) y
// registrado en el cosechador (que no le robe el código de salida). Su salida
// va a la consola, que es lo que enseña `kling logs`, y a api.GuestServiceLog,
// que es lo que devuelve GET /service. Al apagar se le manda su señal de
// parada y, si no sale a tiempo, SIGKILL al grupo: antes de desmontar los
// volúmenes, o Postgres perdería lo último que escribió.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Service es el servicio supervisado de este invitado.
type Service struct {
	spec api.ServiceSpec
	// root es la raíz donde se buscan /etc/passwd y /etc/group (pruebas).
	root    string
	logPath string
	console io.Writer

	mu       sync.Mutex
	cmd      *exec.Cmd
	starts   int
	lastExit string
	err      string
	stopping bool
	// restartReq: Restart pidió relanzarlo. El bucle lo relanza en cuanto
	// sale, sin mirar la política de reinicio ni esperar.
	restartReq bool
	done       chan struct{} // se cierra cuando el bucle termina
	wake       chan struct{} // corta la espera entre reinicios al parar
	kick       chan struct{} // corta la espera entre reinicios para relanzar ya
}

var serviceState struct {
	mu  sync.Mutex
	svc *Service
}

// LoadService lee el servicio que declara la imagen en p. Sin fichero
// devuelve nil, nil: la imagen no tiene servicio.
func LoadService(p string) (*Service, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var spec api.ServiceSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if _, err := api.ParseSignal(spec.StopSignal); err != nil {
		// Por la señal de parada no se queda la imagen sin servicio: SIGTERM,
		// que es lo que manda Docker por defecto, y un aviso en la consola.
		log.Printf("service: %s: stop signal: %v; using SIGTERM", p, err)
		spec.StopSignal = ""
	}
	if err := checkServiceSpec(spec); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &Service{spec: spec, root: "/", logPath: api.GuestServiceLog, console: os.Stdout}, nil
}

func checkServiceSpec(s api.ServiceSpec) error {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return errors.New("argv is empty")
	}
	if _, err := api.ParseSignal(s.StopSignal); err != nil {
		return err
	}
	switch s.Restart {
	case "", api.RestartAlways, api.RestartOnFailure, api.RestartNo:
	default:
		return fmt.Errorf("unknown restart policy %q", s.Restart)
	}
	if s.StopTimeoutSeconds < 0 || s.StopTimeoutSeconds > 3600 {
		return errors.New("stop_timeout_seconds out of range")
	}
	if _, _, err := api.ParseOnRestore(s.OnRestore); err != nil {
		return err
	}
	return nil
}

// StartService arranca el servicio que declare la imagen, si declara uno.
// Hay que llamarlo después de New (con los volúmenes montados).
func (a *Agent) StartService() {
	svc, err := LoadService(api.GuestServiceSpec)
	if err != nil {
		log.Printf("service: %v", err)
		return
	}
	if svc == nil {
		return
	}
	a.startService(svc)
}

// startService arranca svc con el entorno del agente, salvo que falte el de
// la máquina (machine_env.go).
func (a *Agent) startService(svc *Service) {
	if a.envErr != nil {
		// Declarado pero sin arrancar: GET /service enseña por qué.
		svc.err = a.envErr.Error()
		serviceState.mu.Lock()
		serviceState.svc = svc
		serviceState.mu.Unlock()
		return
	}
	svc.Start(a.Env)
}

// Start lanza el bucle que arranca y relanza el servicio.
func (s *Service) Start(env []string) {
	s.mu.Lock()
	s.done, s.wake, s.kick = make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	s.mu.Unlock()
	serviceState.mu.Lock()
	serviceState.svc = s
	serviceState.mu.Unlock()
	go s.loop(env)
}

func (s *Service) loop(env []string) {
	defer close(s.done)
	out := &rotatingLog{path: s.logPath, max: api.GuestServiceLogMax}
	defer out.Close()
	w := io.Writer(out)
	if s.console != nil {
		w = io.MultiWriter(out, s.console)
	}
	backoff := time.Second
	for {
		t0 := time.Now()
		err := s.runOnce(env, w)
		s.mu.Lock()
		stopping, again := s.stopping, s.restartReq
		s.restartReq = false
		s.mu.Unlock()
		if stopping {
			return
		}
		if again {
			select { // el aviso de Restart ya no hace falta
			case <-s.kick:
			default:
			}
			fmt.Fprintf(w, "kling-guest: service restarted after a restore (%v)\n", errOrOK(err))
			backoff = time.Second
			continue
		}
		switch s.spec.Restart {
		case api.RestartNo:
			return
		case api.RestartOnFailure:
			if err == nil {
				return
			}
		}
		// Un servicio que aguantó un rato vuelve a empezar con 1 s; uno que
		// muere al arrancar espera cada vez más, hasta 30 s.
		if time.Since(t0) > 10*time.Second {
			backoff = time.Second
		}
		fmt.Fprintf(w, "kling-guest: service exited (%v), restarting in %s\n", errOrOK(err), backoff)
		select {
		case <-time.After(backoff):
		case <-s.kick:
			s.mu.Lock()
			s.restartReq = false
			s.mu.Unlock()
		case <-s.wake:
			return
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func errOrOK(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// runOnce lanza el servicio y espera a que acabe.
func (s *Service) runOnce(env []string, out io.Writer) error {
	cmd, err := s.command(env)
	if err == nil {
		cmd.Stdout, cmd.Stderr = out, out
		cmd.Stdin = nil
		var ch chan syscall.WaitStatus
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			return nil
		}
		ch, err = DefaultReaper.StartTracked(cmd)
		if err == nil {
			s.cmd, s.err = cmd, ""
			s.starts++
		}
		s.mu.Unlock()
		if err == nil {
			err = WaitFor(cmd, ch)
			DefaultReaper.Forget(cmd.Process.Pid)
			s.mu.Lock()
			s.cmd, s.lastExit = nil, errOrOK(err)
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Lock()
	s.err = err.Error()
	s.mu.Unlock()
	fmt.Fprintf(out, "kling-guest: can't start the service: %v\n", err)
	return err
}

// command arma el proceso: ejecutable buscado en el PATH de env, usuario y
// directorio de la imagen, HOME del usuario.
func (s *Service) command(env []string) (*exec.Cmd, error) {
	bin, err := lookPathEnv(s.spec.Argv[0], env, s.spec.WorkingDir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, s.spec.Argv[1:]...)
	cmd.Args[0] = s.spec.Argv[0]
	cmd.Dir = s.spec.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.Env = env
	if s.spec.User != "" {
		u, err := lookupUser(s.root, s.spec.User)
		if err != nil {
			return nil, err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups}}
		// Como Docker: HOME es la del usuario (el /entrypoint deja /root).
		if u.home != "" {
			cmd.Env = setEnv(env, "HOME", u.home)
		}
	}
	return cmd, nil
}

func setEnv(env []string, k, v string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return append(out, k+"="+v)
}

// lookPathEnv busca name en el PATH de env (no en el del proceso), como
// hace Docker con el ENTRYPOINT; un nombre con "/" es relativo a dir.
func lookPathEnv(name string, env []string, dir string) (string, error) {
	if strings.Contains(name, "/") {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dirOr(dir), p)
		}
		return p, isExec(p)
	}
	pathVar := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	for _, d := range filepath.SplitList(pathVar) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dirOr(dir), p)
		}
		if isExec(p) == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: executable not found in PATH (%s)", name, pathVar)
}

func dirOr(d string) string {
	if d == "" {
		return "/"
	}
	return d
}

func isExec(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", p)
	}
	return nil
}

type userInfo struct {
	uid, gid uint32
	groups   []uint32
	home     string
}

// lookupUser resuelve "uid[:gid]" o "nombre[:grupo]" con el /etc/passwd y el
// /etc/group de la imagen (bajo root), sin libc: la imagen puede ser musl y
// el agente es estático.
func lookupUser(root, spec string) (userInfo, error) {
	name, group, hasGroup := strings.Cut(spec, ":")
	var u userInfo
	found := false
	passwd, _ := readColon(filepath.Join(root, "etc/passwd"), 7)
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		// Un uid que no está en /etc/passwd corre con el grupo 0, como en
		// Docker (runc): no con un gid igual al uid que nadie le dio.
		u.uid, u.gid = uint32(n), 0
		for _, f := range passwd {
			if f[2] == name {
				name = f[0]
				g, _ := strconv.ParseUint(f[3], 10, 32)
				u.gid, u.home, found = uint32(g), f[5], true
				break
			}
		}
	} else {
		for _, f := range passwd {
			if f[0] == name {
				uid, err1 := strconv.ParseUint(f[2], 10, 32)
				gid, err2 := strconv.ParseUint(f[3], 10, 32)
				if err1 != nil || err2 != nil {
					return u, fmt.Errorf("user %q: bad /etc/passwd entry", name)
				}
				u.uid, u.gid, u.home, found = uint32(uid), uint32(gid), f[5], true
				break
			}
		}
		if !found {
			return u, fmt.Errorf("user %q not found in /etc/passwd", name)
		}
	}
	groups, _ := readColon(filepath.Join(root, "etc/group"), 4)
	if hasGroup {
		if g, err := strconv.ParseUint(group, 10, 32); err == nil {
			u.gid = uint32(g)
		} else {
			ok := false
			for _, f := range groups {
				if f[0] == group {
					g, err := strconv.ParseUint(f[2], 10, 32)
					if err != nil {
						break
					}
					u.gid, ok = uint32(g), true
					break
				}
			}
			if !ok {
				return u, fmt.Errorf("group %q not found in /etc/group", group)
			}
		}
	}
	// Grupos suplementarios: aquellos en los que figura por nombre.
	u.groups = []uint32{u.gid}
	if found {
		for _, f := range groups {
			for _, m := range strings.Split(f[3], ",") {
				if m == name {
					if g, err := strconv.ParseUint(f[2], 10, 32); err == nil && uint32(g) != u.gid {
						u.groups = append(u.groups, uint32(g))
					}
				}
			}
		}
	}
	return u, nil
}

// readColon lee un fichero de campos separados por ":" (passwd, group) y
// devuelve las líneas con al menos n campos.
func readColon(p string, n int) ([][]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	for sc.Scan() {
		l := sc.Text()
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if f := strings.Split(l, ":"); len(f) >= n {
			out = append(out, f)
		}
	}
	return out, sc.Err()
}

// stopSignal es la señal de parada de spec; LoadService ya cambió por SIGTERM
// una que no se entiende.
func stopSignal(spec api.ServiceSpec) syscall.Signal {
	n, err := api.ParseSignal(spec.StopSignal)
	if err != nil {
		return syscall.SIGTERM
	}
	return syscall.Signal(n)
}

// ServiceDeclared dice si la imagen declaró un servicio (y se lanzó).
func ServiceDeclared() bool {
	serviceState.mu.Lock()
	defer serviceState.mu.Unlock()
	return serviceState.svc != nil
}

// ServiceStopHandler sirve POST /service/stop: para el servicio y contesta
// su estado cuando ya salió.
func ServiceStopHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		StopService()
		serviceState.mu.Lock()
		s := serviceState.svc
		serviceState.mu.Unlock()
		st := api.GuestService{}
		if s != nil {
			st = s.Status(0)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}
}

// StopService para el servicio, si hay uno (ver Service.Stop).
func StopService() {
	serviceState.mu.Lock()
	s := serviceState.svc
	serviceState.mu.Unlock()
	if s != nil {
		s.Stop()
	}
}

// Stop le manda la señal de parada y espera a que salga; pasado el plazo,
// SIGKILL a su grupo. No vuelve a arrancarlo.
func (s *Service) Stop() {
	s.mu.Lock()
	if s.done == nil {
		s.mu.Unlock()
		return
	}
	if s.stopping {
		// Otro ya lo está parando (el daemon y luego el apagado): esperar
		// a que acabe, no volver como si ya hubiera salido.
		s.mu.Unlock()
		<-s.done
		return
	}
	s.stopping = true
	close(s.wake)
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		sig := stopSignal(s.spec)
		// Al proceso y no al grupo, como Docker: el que reparte la señal a
		// sus hijos (o no) es él.
		_ = cmd.Process.Signal(sig)
	}
	timeout := time.Duration(s.spec.StopTimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-s.done:
		return
	case <-time.After(timeout):
	}
	s.mu.Lock()
	cmd = s.cmd
	s.mu.Unlock()
	log.Printf("service: still running %s after the stop signal; killing it", timeout)
	KillGroup(cmd)
	<-s.done
}

// Restart para el proceso del servicio con su señal de parada (SIGKILL a su
// grupo pasado StopTimeoutSeconds) y espera a que el bucle lo vuelva a lanzar,
// sea cual sea su política de reinicio. Si estaba esperando entre dos
// reinicios, lo lanza ya. Es lo que hace OnRestoreRestart en cada copia de un
// dorado: un proceso nuevo siembra sus aleatorios del kernel ya resembrado.
func (s *Service) Restart() error {
	s.mu.Lock()
	if s.done == nil || s.stopping {
		s.mu.Unlock()
		return errors.New("the service is stopped")
	}
	select {
	case <-s.done:
		s.mu.Unlock()
		return errors.New("the service exited and its restart policy keeps it down")
	default:
	}
	s.restartReq = true
	cmd, starts := s.cmd, s.starts
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(stopSignal(s.spec))
	}
	timeout := time.Duration(s.spec.StopTimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	deadline, killed := time.Now().Add(timeout), false
	for {
		s.mu.Lock()
		n, cur, pending, launchErr := s.starts, s.cmd, s.restartReq, s.err
		s.mu.Unlock()
		switch {
		case n > starts && cur != nil:
			return nil
		case !pending && cur == nil && launchErr != "":
			return fmt.Errorf("restarting the service: %s", launchErr)
		}
		select {
		case <-s.done:
			return errors.New("the service loop ended while restarting it")
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			if killed || cur == nil {
				return errors.New("the service did not come back after the restart")
			}
			log.Printf("service: still running %s after the stop signal; killing it to restart it", timeout)
			KillGroup(cur)
			deadline, killed = time.Now().Add(5*time.Second), true
		}
	}
}

// restartListenTimeout es cuánto se espera, tras reiniciar el servicio, a que
// vuelva a escuchar en sus puertos.
var restartListenTimeout = 60 * time.Second

// restartAndListen es Restart esperando, además, a que el proceso nuevo vuelva
// a escuchar en los puertos TCP en que escuchaba el viejo. Sin eso, una imagen
// sin sonda de "listo" (sin HEALTHCHECK ni EXPOSE) se daba por lista con el
// servicio aún arrancando, y la primera conexión a la copia se rechazaba.
func (s *Service) restartAndListen(root string, timeout time.Duration) error {
	antes := map[int]bool{}
	if st := s.Status(0); st.Running {
		antes = listeningPorts(root, st.PID)
	}
	if err := s.Restart(); err != nil {
		return err
	}
	if len(antes) == 0 {
		return nil
	}
	for end := time.Now().Add(timeout); ; time.Sleep(20 * time.Millisecond) {
		st := s.Status(0)
		if !st.Running {
			return fmt.Errorf("the service exited after the restart (%s)", st.LastExit)
		}
		ahora := listeningPorts(root, st.PID)
		falta := 0
		for p := range antes {
			if !ahora[p] {
				falta = p
				break
			}
		}
		if falta == 0 {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("the service is not listening on port %d %s after the restart", falta, timeout)
		}
	}
}

// Signal manda sig al proceso del servicio (no a su grupo, como la señal de
// parada).
func (s *Service) Signal(sig syscall.Signal) error {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("the service is not running")
	}
	return cmd.Process.Signal(sig)
}

// onRestore es lo que hay que hacerle al servicio en una copia de un dorado
// (api.ServiceSpec.OnRestore), o nil si nada: sin servicio, sin OnRestore, o
// arrancado sin el entorno de la máquina.
func onRestore() func() error {
	serviceState.mu.Lock()
	s := serviceState.svc
	serviceState.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	started := s.done != nil
	s.mu.Unlock()
	if !started {
		return nil
	}
	action, sig, err := api.ParseOnRestore(s.spec.OnRestore)
	if err != nil {
		return nil // LoadService ya lo validó
	}
	switch action {
	case api.OnRestoreRestart:
		return func() error { return s.restartAndListen(procRoot, restartListenTimeout) }
	case api.OnRestoreSignal:
		return func() error { return s.Signal(syscall.Signal(sig)) }
	}
	return nil
}

// Status es lo que devuelve GET /service, con los últimos tail bytes del log.
func (s *Service) Status(tail int64) api.GuestService {
	s.mu.Lock()
	st := api.GuestService{Declared: true, Argv: s.spec.Argv, Starts: s.starts, LastExit: s.lastExit, Error: s.err}
	if s.cmd != nil && s.cmd.Process != nil {
		st.Running, st.PID = true, s.cmd.Process.Pid
	}
	s.mu.Unlock()
	if tail > 0 {
		st.Log = tailFile(s.logPath, tail)
	}
	return st
}

func tailFile(p string, n int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		_, _ = f.Seek(st.Size()-n, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}

// ServiceHandler sirve GET /service[?tail=N].
func ServiceHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		tail := int64(16 << 10)
		if v := r.URL.Query().Get("tail"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || n > 1<<20 {
				http.Error(w, "tail must be 0..1048576", http.StatusBadRequest)
				return
			}
			tail = n
		}
		serviceState.mu.Lock()
		s := serviceState.svc
		serviceState.mu.Unlock()
		st := api.GuestService{}
		if s != nil {
			st = s.Status(tail)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}
}

// rotatingLog escribe en path y, al pasar de max, lo rota a path.1.
type rotatingLog struct {
	path string
	max  int64
	f    *os.File
	n    int64
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	if l.f == nil || l.n+int64(len(p)) > l.max {
		if l.f != nil {
			l.f.Close()
			_ = os.Rename(l.path, l.path+".1")
		}
		_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			// Sin fichero se sigue: la consola recibe la salida igual.
			return len(p), nil
		}
		st, _ := f.Stat()
		l.f, l.n = f, 0
		if st != nil {
			l.n = st.Size()
		}
	}
	n, _ := l.f.Write(p)
	l.n += int64(n)
	// Siempre "todo escrito": un error aquí cortaría la copia a la consola
	// (io.MultiWriter para en el primero que falla).
	return len(p), nil
}

func (l *rotatingLog) Close() {
	if l.f != nil {
		l.f.Close()
	}
}
