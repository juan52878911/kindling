//go:build darwin

// kling-vz es el VMM de kindling en macOS: un proceso por microVM que habla el
// API HTTP de Firecracker por un socket unix y por debajo usa
// Virtualization.framework. El contrato está en docs/backend-vz.md del núcleo.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/footprint"
	"github.com/juan52878911/kindling/vz/internal/server"
	"github.com/juan52878911/kindling/vz/internal/vnet"
	"github.com/juan52878911/kindling/vz/internal/vzvm"
)

// version la fija el build con -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// syncWriter serializa las escrituras en stdout: la consola del invitado y los
// diagnósticos del ayudante van al mismo fichero (firecracker.log) y no deben
// mezclarse a mitad de línea.
type syncWriter struct {
	mu   sync.Mutex
	w    io.Writer
	last byte // último byte escrito, para saber si la consola dejó una línea abierta
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > 0 {
		s.last = p[len(p)-1]
	}
	return s.w.Write(p)
}

// line escribe una línea de diagnóstico empezando siempre en columna 0: el
// prompt del invitado no acaba en salto de línea, y "kling-vz:" pegado a él no
// lo encontraría quien busque el prefijo.
func (s *syncWriter) line(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != 0 && s.last != '\n' {
		msg = "\n" + msg
	}
	s.last = '\n'
	_, _ = io.WriteString(s.w, msg+"\n")
}

var stdout = &syncWriter{w: os.Stdout}

func logf(format string, args ...any) {
	stdout.line("kling-vz: " + fmt.Sprintf(format, args...))
}

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("kling-vz", flag.ContinueOnError)
	sock := fs.String("api-sock", "", "unix socket for the Firecracker-compatible API")
	showVersion := fs.Bool("version", false, "print the version and exit")
	// --id lo pasa Firecracker (y el jailer); se acepta para no romper a quien
	// lance el ayudante con los mismos argumentos.
	_ = fs.String("id", "", "instance id (ignored)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("kling-vz", version)
		return 0
	}
	if *sock == "" {
		fmt.Fprintln(os.Stderr, "kling-vz: --api-sock is required")
		return 2
	}

	meter := footprint.NewMeter()
	policy := egress.NewPolicy()
	resolver := egress.NewResolver(policy)
	srv := server.New(server.Deps{
		Factory: &vzvm.Factory{Console: stdout, Logf: logf, OnCreate: meter.Track},
		NewNet: func(c server.NetConfig) (server.Network, error) {
			return vnet.New(vnet.Config{
				GuestMAC: c.GuestMAC,
				MMDS:     c.MMDS,
				MMDSAddr: c.MMDSAddr,
				Policy:   c.Policy,
				Resolver: c.Resolver,
				Logf:     logf,
			})
		},
		Footprint: meter.Bytes,
		Version:   version,
		Logf:      logf,
		Policy:    policy,
		Resolver:  resolver,
	})

	// Un socket que sobra de un proceso muerto impediría escuchar. Solo se
	// borra si es un socket: una ruta equivocada no debe llevarse un fichero.
	if fi, err := os.Lstat(*sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(*sock)
	}
	// sun_path admite 104 bytes en macOS, y las rutas bajo el directorio del
	// usuario se pasan con facilidad. Con una ruta larga se escucha por nombre
	// relativo desde su directorio, que el kernel sí acepta; quien conecte
	// tendrá que hacer lo mismo.
	bindPath := *sock
	if len(bindPath) > 103 {
		if err := os.Chdir(filepath.Dir(bindPath)); err == nil {
			bindPath = filepath.Base(bindPath)
		}
	}
	l, err := net.Listen("unix", bindPath)
	if err != nil {
		logf("cannot listen on %s: %v", *sock, err)
		return 1
	}
	_ = os.Chmod(bindPath, 0o600)
	defer os.Remove(bindPath)

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(l) }()
	logf("version %s listening on %s", version, *sock)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	code := 0
	select {
	case sig := <-sigs:
		logf("received %s: stopping the VM", sig)
		srv.Shutdown()
	case err := <-srv.Done():
		if err != nil {
			code = 1
		}
		srv.Shutdown()
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logf("API server failed: %v", err)
			code = 1
		}
		srv.Shutdown()
	}
	_ = hs.Close()
	return code
}
