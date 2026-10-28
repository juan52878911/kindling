//go:build darwin

// kling-vz es el VMM de kindling en macOS: un proceso por microVM que habla el
// API HTTP de Firecracker por un socket unix y por debajo usa
// Virtualization.framework. El contrato está en docs/backend-vz.md del núcleo.
package main

import (
	"context"
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

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/footprint"
	"github.com/juan52878911/kindling/vz/internal/grafo"
	"github.com/juan52878911/kindling/vz/internal/peercred"
	"github.com/juan52878911/kindling/vz/internal/sandbox"
	"github.com/juan52878911/kindling/vz/internal/server"
	"github.com/juan52878911/kindling/vz/internal/spec"
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

// frenoCaido es el error de footprint.ErrFrenoCaido visto como
// server.ErrFreezeLost, sin repetir el mensaje.
type frenoCaido struct{ error }

func (e frenoCaido) Is(target error) bool { return target == server.ErrFreezeLost }
func (e frenoCaido) Unwrap() error        { return e.error }

// argFreno lanza este binario como proceso freno del tope de CPU (ver
// footprint/freno_darwin.go).
const argFreno = "--cpu-brake"

func main() {
	if len(os.Args) == 2 && os.Args[1] == argFreno {
		os.Exit(servirFreno())
	}
	if vzvm.WindowMode() {
		// AppKit necesita el hilo principal (vzvm lo fija en su init): el
		// servidor va en otra gorrutina y el proceso sale cuando acabe.
		go func() { os.Exit(run()) }()
		vzvm.RunApp()
		return
	}
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("kling-vz", flag.ContinueOnError)
	sock := fs.String("api-sock", "", "unix socket for the Firecracker-compatible API")
	showVersion := fs.Bool("version", false, "print the version and exit")
	formats := fs.Bool("snapshot-formats", false, "print the oldest and newest snapshot formats (kling_vz) this build reads, and exit")
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
	if *formats {
		min, max := spec.Formatos()
		fmt.Println(min, max)
		return 0
	}
	if *sock == "" {
		fmt.Fprintln(os.Stderr, "kling-vz: --api-sock is required")
		return 2
	}

	// Pantalla virtio-gpu para las máquinas que arranquen aquí (el daemon pasa
	// su entorno a kling-vz). Ver prototypes/android/docs/gpu.md.
	graphics, err := spec.ParseGraphics(os.Getenv("KLING_VZ_GRAPHICS"))
	if err != nil {
		logf("KLING_VZ_GRAPHICS: %v", err)
		return 2
	}

	// El canal con el agente por virtio-vsock y la MTU del enlace, para las
	// máquinas que arranquen aquí (una restauración usa los del snapshot).
	vsock, err := spec.ParseVsock(os.Getenv("KLING_VZ_VSOCK"))
	if err != nil {
		logf("KLING_VZ_VSOCK: %v", err)
		return 2
	}
	mtu, err := spec.ParseMTU(os.Getenv("KLING_VZ_MTU"))
	if err != nil {
		logf("KLING_VZ_MTU: %v", err)
		return 2
	}

	meter := footprint.NewMeter()
	// El freno del tope de CPU va en su propio proceso, lanzado antes de que
	// este se encierre. Sin él, el tope pausa la VM por el framework, con la
	// que se para también el reloj del invitado.
	freno, err := footprint.StartFreno(argFreno)
	if err != nil {
		logf("warning: no CPU brake process, the CPU ceiling will pause the VM instead: %v", err)
	}
	onCreate, freeze := meter.Track, func(bool) error { return errors.New("no CPU brake process") }
	if freno != nil {
		onCreate = func(fd uintptr) {
			meter.Track(fd)
			if err := freno.Track(fd); err != nil {
				logf("warning: %v", err)
			}
		}
		freeze = func(stop bool) error {
			err := freno.Freeze(stop)
			if errors.Is(err, footprint.ErrFrenoCaido) {
				// El servidor reconoce su centinela, no el de footprint.
				return frenoCaido{err}
			}
			return err
		}
	}
	policy := egress.NewPolicy()
	resolver := egress.NewResolver(policy)
	// El proxy de credenciales resuelve por el mismo upstream que el invitado
	// y con los mismos destinos prohibidos que la red (egress.PublicIPv4).
	// Solo atiende en allowlist: su listener está en la pasarela sea cual sea
	// el modo, y sin esta barrera un invitado sin salida (none) que conectara
	// a mano a la pasarela:80 con el Host de un dominio con credencial saldría
	// a internet a través de él. El rechazo es del proxy, así que queda en su
	// registro de auditoría, que va junto al socket: el directorio de la
	// máquina, el mismo machines/<id>/credaudit.jsonl que lee el daemon (y el
	// único sitio en el que el sandbox deja escribir).
	//
	// Las aristas de grafo y kling db attach (vz/internal/grafo): si el
	// daemon dice dónde está su broker (KLING_VZ_BROKER), cada conexión a
	// otra máquina se le pide a él, y el proxy atiende también sin allowlist
	// cuando TODAS sus credenciales van a otra máquina (aristas credential
	// de un nodo en none o internet): esas no salen a ningún otro sitio.
	var grafoMaq *grafo.Grafo
	var creds *credproxy.Proxy
	opts := credproxy.Options{
		Lookup: resolver.PublicIPv4,
		Enabled: func() bool {
			return policy.Mode() == egress.Allowlist || (grafoMaq != nil && creds.SoloMaquinas())
		},
		AuditPath: rutaAuditoria(*sock),
		Logf:      logf,
	}
	// Tamaño y generaciones del registro, si el daemon los fijó.
	if v := os.Getenv("KLING_VZ_CREDAUDIT"); v != "" {
		if c, err := credproxy.ParseAuditConfig(v); err == nil {
			opts.Audit = c
		} else {
			logf("WARNING: %v (using the defaults)", err)
		}
	}
	// Un cuerpo grande con Content-Length se derrama con la clave dentro
	// (pkg/credproxy/cuerpo.go): a credtmp/ del directorio de la máquina (0700,
	// dentro de lo que el perfil deja escribir), vaciado aquí de lo que dejó un
	// kling-vz muerto a mitad. Nunca a os.TempDir(); si no se puede, el proxy
	// no derrama y ese cuerpo sale chunked.
	if tmp := filepath.Join(filepath.Dir(rutaAuditoria(*sock)), "credtmp"); credproxy.PrepararTempDir(tmp) == nil {
		opts.TempDir = tmp
	} else {
		logf("WARNING: no credential proxy spill directory at %s: large request bodies will be sent chunked", tmp)
	}
	broker := rutaBroker()
	if broker != "" {
		opts.DialMachine = func(ctx context.Context, id, owner string, port int) (net.Conn, error) {
			return grafoMaq.DialMachine(ctx, id, owner, port)
		}
	}
	creds = credproxy.New(opts)
	if broker != "" {
		grafoMaq = grafo.New(broker, creds.Auditor(), logf)
	}
	// Nada de lo que crea este proceso (el socket de la API, en particular)
	// debe nacer legible por otros usuarios, ni siquiera el instante entre
	// crearlo y el chmod.
	syscall.Umask(0o077)

	peers := peercred.New()
	// Encerrado, este proceso solo escribe en el directorio de su máquina; ver
	// internal/sandbox y kling-vz.sb.
	confine, destino := sandbox.Preparar(sandbox.Opciones{Nombre: "kling-vz", Perfil: perfil,
		Sock: *sock, Broker: rutaBroker(), Logf: logf})
	srv := server.New(server.Deps{
		Factory: &vzvm.Factory{Console: stdout, Logf: logf, OnCreate: onCreate,
			Window: vzvm.WindowMode(), Title: "kling " + filepath.Base(filepath.Dir(*sock))},
		Graphics: graphics,
		Vsock:    vsock,
		MTU:      mtu,
		NewNet: func(c server.NetConfig) (server.Network, error) {
			// Sin proxy, la interfaz queda nil de verdad (no un puntero nil
			// dentro de ella, que vnet tomaría por un proxy).
			var pg vnet.PGProxy
			if c.CredentialsPG != nil {
				pg = c.CredentialsPG
			}
			return vnet.New(vnet.Config{
				GuestMAC: c.GuestMAC,
				MTU:      c.MTU,
				MMDS:     c.MMDS,
				MMDSAddr: c.MMDSAddr,
				Policy:   c.Policy,
				Resolver: c.Resolver,
				// Credentials: el proxy en la pasarela (ver vnet.Config).
				Credentials:   c.Credentials,
				CredentialsPG: pg,
				Graph:         grafoDeRed(c.Graph),
				Logf:          logf,
				// Solo los procesos de este usuario llegan al agente del
				// invitado por el reenvío (ver internal/peercred).
				PeerAllowed: peers.Allowed,
			})
		},
		Footprint: meter.Bytes,
		Version:   version,
		Logf:      logf,
		Policy:    policy,
		Resolver:  resolver,
		// Las claves del proxy viven en la memoria de este proceso, que
		// corre como el usuario; ver la sección 7 de SECURITY.md.
		Credentials:   creds,
		CredIP:        vnet.GatewayIP,
		Confine:       confine,
		Destino:       destino,
		CPUTime:       meter.CPUTime,
		Freeze:        freeze,
		HelperStopped: meter.HelperStopped,
		HelperPID:     meter.HelperPID,
		Graph:         grafoMaq,
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
		if errors.Is(err, server.ErrFreezeLost) {
			// El auxiliar se quedó parado sin freno: no contestaría a Stop
			// (Shutdown esperaría para siempre). Salir ya, que al irse este
			// proceso el sistema se lo lleva.
			_ = hs.Close()
			_ = creds.Close()
			return code
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
	// Después de parar la VM (y con ella su red y el servidor del proxy): lo
	// que quede en la cola del registro llega al disco.
	_ = creds.Close()
	return code
}

// rutaAuditoria es el registro de auditoría del proxy: junto al socket de la
// API, en el directorio de la máquina. Absoluta y sin enlaces antes de nada:
// con una ruta de socket larga el proceso cambia de directorio (ver más abajo),
// y el sandbox compara rutas reales.
func rutaAuditoria(sock string) string {
	dir := filepath.Dir(sock)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Join(dir, credproxy.AuditFile)
}

// grafoDeRed pasa las aristas a la red sin el puntero nil dentro de una
// interfaz (que vnet tomaría por unas aristas).
func grafoDeRed(g *grafo.Grafo) vnet.GraphLinks {
	if g == nil {
		return nil
	}
	return g
}

// rutaBroker es el socket del daemon por el que se piden las conexiones a
// otras máquinas (pkg/linkbroker), o "" si el daemon no lo da. Real y
// absoluta: el sandbox la compara con la ruta real (en macOS /tmp es
// /private/tmp) y el perfil solo deja conectar a esa.
func rutaBroker() string {
	p := os.Getenv("KLING_VZ_BROKER")
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	dir := filepath.Dir(p)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return filepath.Join(dir, filepath.Base(p))
}

// servirFreno es el proceso freno: se encierra y atiende a su kling-vz por el
// fd 3 hasta que este muere. Ignora las señales de terminal y SIGTERM: si
// muriera con el auxiliar parado, la VM se quedaría parada mientras kling-vz
// viva; lo que lo termina es que se cierre el socket (o un SIGKILL).
func servirFreno() int {
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	if err := confinarFreno(); err != nil {
		fmt.Fprintf(os.Stderr, "kling-vz: CPU brake: %v\n", err)
		return 1
	}
	return footprint.ServeFreno(3)
}
