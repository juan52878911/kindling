package main

// El lanzador: lo que hacía image/android-launch.sh, en Go.
//
// Proceso: `kling-phoned` (el SERVICE de la imagen, hijo del entrypoint y
// nieto de kling-guest, que sigue siendo PID 1 de la VM) arranca un hijo con
// espacios de nombres nuevos de montajes, PIDs, IPC y UTS (y de red salvo con
// ANDROID_NET=shared). El hijo (`kling-phoned stage2`) es PID 1 de su espacio
// de PIDs: espera a que el padre le monte la red, prepara el rootfs
// (pseudo-sistemas, /dev, binderfs, /data), hace pivot_root y exec de /init.
// Así init de Android queda como PID 1 de su espacio, que es lo que espera, y
// como hijo directo de kling-phoned: si kling-phoned muere, PDEATHSIG mata a
// Android (lo que era --kill-child de unshare) y el bucle del entrypoint
// relanza los dos. Si muere Android, kling-phoned lo relanza.

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const hostIf = "kandroid0"

// supervisor es el estado del lanzador; android_linux.go lo usa para operar
// el teléfono.
type supervisor struct {
	cfg  config
	logs *ringLog

	mu       sync.Mutex
	pid      int // init de Android (0 si no corre)
	netnsFD  int // su espacio de red (-1 con ANDROID_NET=shared o si no corre)
	netMode  string
	restarts int
	started  time.Time
	state    string
	zygEnv   []string // entorno de zygote, por PID de init
	zygPID   int
}

func setState(s string) {
	_ = os.MkdirAll(stateDir, 0o755)
	_ = os.WriteFile(statePath, []byte(s+"\n"), 0o644)
}

func (s *supervisor) setState(st string) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	setState(st)
}

// fatal deja el motivo en state (fase0.sh y android-sh --state lo leen) y se
// duerme: un fallo que no se arregla relanzando no debe llenar el log.
func (s *supervisor) fatal(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	log.Printf("FATAL: %s", msg)
	s.setState("failed: " + msg)
	time.Sleep(time.Hour)
	return errors.New(msg)
}

func runDaemon() error {
	logs := newRingLog(256 << 10)
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("kling-phoned: ")
	s := &supervisor{logs: logs, netnsFD: -1}
	s.setState("starting")
	cfg, err := loadConfig()
	if err != nil {
		return s.fatal("config: %v", err)
	}
	s.cfg = cfg
	log.Printf("%s: rootfs %s, %sx%s@%sdpi, /data %s, net %s, api %s, ports %v",
		version, cfg.Root, cfg.Width, cfg.Height, cfg.DPI, cfg.DataMode, cfg.Net, cfg.Listen, cfg.Ports)
	if err := s.checkKernel(); err != nil {
		return s.fatal("%v", err)
	}
	// El PID del padre de init: android-sh y uidump buscan a Android como el
	// hijo con comm "init" del PID de este fichero (antes, el de unshare).
	_ = os.MkdirAll(stateDir, 0o755)
	if err := os.WriteFile(parentPIDPth, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return s.fatal("%v", err)
	}

	ops := &androidOps{s: s}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return s.fatal("api listen %s: %v", cfg.Listen, err)
	}
	srv := apiServer(ops)
	go func() { log.Printf("api: %v", srv.Serve(ln)) }()
	if cfg.Net != "shared" {
		for _, p := range cfg.Ports {
			go s.forward(p)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		s.mu.Lock()
		pid := s.pid
		s.mu.Unlock()
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		os.Exit(0)
	}()

	backoff := time.Second
	for {
		t0 := time.Now()
		err := s.launchOnce()
		if err != nil {
			log.Printf("android: %v", err)
		}
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		if time.Since(t0) > time.Minute {
			backoff = time.Second
		}
		s.setState(fmt.Sprintf("restarting (%v)", err))
		time.Sleep(backoff)
		if backoff < 16*time.Second {
			backoff *= 2
		}
	}
}

// checkKernel: lo que Android necesita y, si falta, dice qué.
func (s *supervisor) checkKernel() error {
	fs, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return err
	}
	has := func(name string) bool {
		for _, l := range strings.Split(string(fs), "\n") {
			f := strings.Fields(l)
			if len(f) > 0 && f[len(f)-1] == name {
				return true
			}
		}
		return false
	}
	if !has("binder") {
		return errors.New("kernel without binderfs (CONFIG_ANDROID_BINDERFS): boot this image with the kernel from prototypes/android/kernel")
	}
	if !has("cgroup2") {
		return errors.New("kernel without cgroup2")
	}
	if _, err := os.Stat("/proc/pressure/memory"); err != nil {
		log.Printf("aviso: sin PSI (/proc/pressure/memory): lmkd irá a ciegas")
	}
	// El comprobador completo, solo informativo (las obligatorias van arriba).
	if f, err := os.Open("/proc/config.gz"); err == nil {
		defer f.Close()
		if zr, err := gzip.NewReader(f); err == nil {
			if _, err := os.Stat(checkPath); err == nil {
				cmd := exec.Command(checkPath, "-")
				cmd.Stdin = zr
				cmd.Stdout, cmd.Stderr = log.Writer(), log.Writer()
				if err := cmd.Run(); err != nil {
					log.Printf("aviso: el comprobador de Android encontró problemas (arriba)")
				}
			}
		}
	}
	// /init es un enlace absoluto (/init -> /system/bin/init): se resuelve
	// dentro del rootfs, que es donde lo resolverá Android.
	init := filepath.Join(s.cfg.Root, "init")
	if t, err := os.Readlink(init); err == nil {
		init = filepath.Join(s.cfg.Root, t)
	}
	if st, err := os.Stat(init); err != nil || st.Mode()&0o111 == 0 {
		return fmt.Errorf("no %s/init: the image has no Android rootfs", s.cfg.Root)
	}
	return nil
}

// launchOnce arranca Android y espera a que muera.
func (s *supervisor) launchOnce() error {
	cfg := s.cfg
	mode := cfg.Net
	ipt := ""
	if mode == "veth" {
		if ipt = iptablesPath(); ipt == "" {
			log.Printf("aviso: sin iptables en la base no hay NAT ni aislamiento del enlace veth; Android arranca con la red aislada (solo lo)")
			mode = "isolated"
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	flags := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS)
	if mode != "shared" {
		flags |= syscall.CLONE_NEWNET
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, stage2Arg)
	cmd.ExtraFiles = []*os.File{r}
	cmd.Stdout, cmd.Stderr = s.logs.tee(os.Stdout), s.logs.tee(os.Stderr)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, Pdeathsig: syscall.SIGKILL}

	// PDEATHSIG va con el HILO que hace el clone: el hilo se queda bloqueado a
	// esta gorrutina mientras Android viva, para que Go no lo termine antes.
	type result struct{ err error }
	done := make(chan result, 1)
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		done <- result{cmd.Wait()}
	}()
	if err := <-started; err != nil {
		return fmt.Errorf("start: %w", err)
	}
	r.Close()
	pid := cmd.Process.Pid
	s.mu.Lock()
	s.pid, s.netMode, s.started, s.zygEnv, s.zygPID = pid, mode, time.Now(), nil, 0
	s.mu.Unlock()

	var dnsArgs []string
	netErr := func() error {
		if mode == "shared" {
			log.Printf("aviso: red compartida: netd tocará las rutas de la VM (docs/telefono.md)")
			return nil
		}
		fd, err := syscall.Open(fmt.Sprintf("/proc/%d/ns/net", pid), syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.netnsFD = fd
		s.mu.Unlock()
		if mode == "isolated" {
			return loUp(fd)
		}
		hostIP, andIP := net.ParseIP(cfg.VethHost), net.ParseIP(cfg.VethAndroid)
		if hostIP.To4() == nil || andIP.To4() == nil {
			return fmt.Errorf("bad veth addresses %q %q", cfg.VethHost, cfg.VethAndroid)
		}
		if err := vethSetup(hostIf, hostIP, andIP, pid, fd); err != nil {
			return err
		}
		if err := natSetup(ipt, hostIf, cfg.VethAndroid); err != nil {
			return err
		}
		if dns := firstNameserver(); dns != "" {
			dnsArgs = []string{"androidboot.redroid_net_ndns=1", "androidboot.redroid_net_dns1=" + dns}
		}
		log.Printf("red veth: Android %s/30 (eth0) <-> VM %s (%s), dns %v; puertos %v por reenvío",
			cfg.VethAndroid, cfg.VethHost, hostIf, dnsArgs, cfg.Ports)
		return nil
	}()
	if netErr != nil {
		log.Printf("aviso: red %s: %v; Android arranca con lo que haya", mode, netErr)
	}
	// La orden para el hijo: los argumentos de red, uno por línea, y "go".
	msg := strings.Join(dnsArgs, "\n")
	if _, err := io.WriteString(w, msg+"\n\x00"); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	w.Close()
	s.setState("booting")
	go s.watchBoot(pid)

	res := <-done
	s.mu.Lock()
	if s.netnsFD >= 0 {
		syscall.Close(s.netnsFD)
		s.netnsFD = -1
	}
	s.pid = 0
	s.mu.Unlock()
	return fmt.Errorf("init exited: %v", res.err)
}

// watchBoot pone state a "running" cuando Android llega a boot_completed y,
// con ANDROID_PREP=1, después de dejarlo como un teléfono de automatización
// (prepScript). La sonda de listo espera a las dos cosas (prepared).
func (s *supervisor) watchBoot(pid int) {
	_ = os.Remove(prepMark)
	for i := 0; i < 1500; i++ {
		time.Sleep(200 * time.Millisecond)
		s.mu.Lock()
		cur := s.pid
		s.mu.Unlock()
		if cur != pid {
			return
		}
		if v, err := getProp(pid, "sys.boot_completed"); err != nil || v != "1" {
			continue
		}
		log.Printf("android: boot_completed")
		if s.cfg.Prep {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			t0 := time.Now()
			_, err := runIn(ctx, pid, zygoteEnv(pid), nil, prepScript)
			cancel()
			if err != nil {
				log.Printf("aviso: preparar la pantalla: %v", err)
			} else {
				log.Printf("android: pantalla preparada en %.1fs", time.Since(t0).Seconds())
			}
		}
		_ = os.WriteFile(prepMark, []byte(strconv.Itoa(pid)+"\n"), 0o644)
		s.setState("running")
		return
	}
}

// prepScript: pantalla encendida siempre, sin bloqueo, sin animaciones y en
// la pantalla de inicio (lo que hacía phone.sh por kling exec). Persiste en
// los ajustes de /data: un dorado lo lleva a todos sus clones.
const prepScript = `svc power stayon true; settings put system screen_off_timeout 2147483647
locksettings set-disabled true; input keyevent KEYCODE_WAKEUP; wm dismiss-keyguard
settings put global window_animation_scale 0; settings put global transition_animation_scale 0
settings put global animator_duration_scale 0; input keyevent KEYCODE_HOME; true`

func firstNameserver() string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "nameserver" && net.ParseIP(f[1]).To4() != nil {
			return f[1]
		}
	}
	return ""
}

// forward reenvía el puerto p de la VM al mismo puerto dentro del espacio de
// red de Android: el socket de salida se abre en ese espacio (setns en un hilo
// bloqueado) y se conecta a 127.0.0.1. Sin DNAT: Android no necesita ninguna
// ruta hacia la VM, y con ANDROID_NET=isolated adb sigue funcionando.
func (s *supervisor) forward(p int) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
	if err != nil {
		log.Printf("aviso: reenvío del %d: %v", p, err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("reenvío del %d: %v", p, err)
			time.Sleep(time.Second)
			continue
		}
		go s.pipe(c, p)
	}
}

func (s *supervisor) pipe(c net.Conn, p int) {
	defer c.Close()
	up, err := s.dialAndroid(p)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(up, c)
	go cp(c, up)
	<-done
	<-done
}

// dialAndroid conecta con 127.0.0.1:p dentro del espacio de red de Android.
// El socket se crea y se conecta en un hilo metido en ese espacio; en
// loopback el connect bloqueante contesta al instante (o ECONNREFUSED).
func (s *supervisor) dialAndroid(p int) (net.Conn, error) {
	s.mu.Lock()
	fd := s.netnsFD
	s.mu.Unlock()
	if fd < 0 {
		return nil, errNotRunning
	}
	sfd := -1
	err := inNetns(fd, func() error {
		var e error
		if sfd, e = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); e != nil {
			return e
		}
		return syscall.Connect(sfd, &syscall.SockaddrInet4{Port: p, Addr: [4]byte{127, 0, 0, 1}})
	})
	if err != nil {
		if sfd >= 0 {
			syscall.Close(sfd)
		}
		return nil, err
	}
	f := os.NewFile(uintptr(sfd), "android")
	defer f.Close()
	return net.FileConn(f) // duplica el descriptor
}
