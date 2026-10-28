//go:build darwin && arm64

// kling-hv es el tercer VMM de kindling: un proceso por microVM, como kling-vz,
// que habla el MISMO API (el de Firecracker más /kling/*, docs/backend-vz.md)
// con el mismo servidor, la misma red de espacio de usuario, MMDS, egress y
// proxy de credenciales. Lo que cambia es la VM: en vez de
// Virtualization.framework, un VMM propio sobre Hypervisor.framework
// (vz/internal/hvvm) cuya RAM es un fichero mapeado, así que restaurar un
// snapshot cuesta milisegundos y casi nada de memoria. Ver docs/backend-hv.md.
package main

/*
#cgo CFLAGS: -mmacosx-version-min=15.2
#cgo LDFLAGS: -framework Hypervisor
#include <Hypervisor/Hypervisor.h>

// khv_sondear crea y destruye una VM con el GIC del framework, sin vCPU ni
// RAM: lo mínimo que dice si este proceso puede ser un kling-hv aquí (el
// entitlement, un hipervisor disponible, el GIC de macOS 15). *paso es lo que
// llegó a hacer: 0 = hv_vm_create, 1 = el GIC.
static hv_return_t khv_sondear(int *paso) {
	*paso = 0;
	hv_return_t r = hv_vm_create(NULL);
	if (r != HV_SUCCESS) return r;
	*paso = 1;
	hv_gic_config_t g = hv_gic_config_create();
	r = hv_gic_config_set_distributor_base(g, 0x08000000ULL);
	if (r == HV_SUCCESS) r = hv_gic_config_set_redistributor_base(g, 0x0A000000ULL);
	if (r == HV_SUCCESS) r = hv_gic_create(g);
	os_release(g);
	hv_vm_destroy();
	return r;
}
*/
import "C"

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/energia"
	"github.com/juan52878911/kindling/vz/internal/grafo"
	"github.com/juan52878911/kindling/vz/internal/hvvm"
	"github.com/juan52878911/kindling/vz/internal/peercred"
	"github.com/juan52878911/kindling/vz/internal/sandbox"
	"github.com/juan52878911/kindling/vz/internal/server"
	"github.com/juan52878911/kindling/vz/internal/spec"
	"github.com/juan52878911/kindling/vz/internal/vnet"
)

// sondear es --probe: lo que mira el daemon con daemon.vmm auto antes de
// elegir hv. Cuesta unos ms y no deja nada (la VM es del proceso, que sale).
func sondear() int {
	var paso C.int
	r := C.khv_sondear(&paso)
	if r == C.HV_SUCCESS {
		fmt.Println("ok")
		return 0
	}
	que := "hv_vm_create"
	if paso == 1 {
		que = "hv_gic_create"
	}
	por := ""
	switch r {
	case C.HV_DENIED:
		por = " (denied: not signed with com.apple.security.hypervisor?)"
	case C.HV_UNSUPPORTED:
		por = " (unsupported: no hypervisor here, e.g. a VM without nested virtualization)"
	case C.HV_BUSY:
		por = " (busy)"
	case C.HV_NO_RESOURCES:
		por = " (no resources)"
	}
	fmt.Fprintf(os.Stderr, "kling-hv: %s failed: 0x%x%s\n", que, uint32(r), por)
	return 1
}

// version la fija el build con -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// syncWriter serializa las escrituras en stdout (firecracker.log): la consola
// del invitado y los diagnósticos no deben mezclarse a mitad de línea.
type syncWriter struct {
	mu   sync.Mutex
	w    io.Writer
	last byte
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > 0 {
		s.last = p[len(p)-1]
	}
	return s.w.Write(p)
}

// line escribe una línea de diagnóstico empezando siempre en columna 0.
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
	stdout.line("kling-hv: " + fmt.Sprintf(format, args...))
}

// perfil es el sandbox de kling-hv (ver internal/sandbox).
//
//go:embed kling-hv.sb
var perfil string

func main() {
	// El Go de kling-hv es el servidor y la red de usuario: los vCPU son hilos
	// de C, fuera de los P del planificador. Con los 10 P por defecto de un M4
	// cada copia de un dorado carga con sus cachés y su recolección sin
	// ganar nada; con 2, ~0,3 MiB menos por copia (medido, 10 copias) y la
	// misma latencia. GOMAXPROCS en el entorno sigue mandando.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(2)
	}
	if len(os.Args) == 2 && os.Args[1] == energia.Arg {
		os.Exit(energia.Main(sandbox.Aplicar))
	}
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("kling-hv", flag.ContinueOnError)
	sock := fs.String("api-sock", "", "unix socket for the Firecracker-compatible API")
	showVersion := fs.Bool("version", false, "print the version and exit")
	formats := fs.Bool("snapshot-formats", false, "print the oldest and newest snapshot formats (kling_vz) this build reads, and exit")
	probe := fs.Bool("probe", false, "create and destroy an empty VM with its GIC to check Hypervisor.framework works here, and exit (0 = ok)")
	_ = fs.String("id", "", "instance id (ignored)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("kling-hv", version)
		return 0
	}
	if *formats {
		min, max := spec.Formatos()
		fmt.Println(min, max)
		return 0
	}
	if *probe {
		return sondear()
	}
	if *sock == "" {
		fmt.Fprintln(os.Stderr, "kling-hv: --api-sock is required")
		return 2
	}

	// El canal con el agente por virtio-vsock (vsock.c), como en kling-vz y
	// con la misma variable: las máquinas que arrancan aquí lo llevan salvo
	// KLING_VZ_VSOCK=0; una restauración usa lo que diga su snapshot.
	vsock, err := spec.ParseVsock(os.Getenv("KLING_VZ_VSOCK"))
	if err != nil {
		logf("KLING_VZ_VSOCK: %v", err)
		return 2
	}

	policy := egress.NewPolicy()
	resolver := egress.NewResolver(policy)
	// El proxy de credenciales y las aristas de grafo, como en kling-vz (ver
	// vz/cmd/kling-vz/main.go): mismo proxy, misma barrera de allowlist.
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
	// El daemon pasa a kling-hv el mismo entorno que a kling-vz.
	if v := os.Getenv("KLING_VZ_CREDAUDIT"); v != "" {
		if c, err := credproxy.ParseAuditConfig(v); err == nil {
			opts.Audit = c
		} else {
			logf("WARNING: %v (using the defaults)", err)
		}
	}
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
		// El loopback del Mac (-upstream 127.0.0.1:5432) también lo marca el
		// daemon: el perfil no abre el loopback a este proceso.
		opts.DialLoopback = func(ctx context.Context, port int) (net.Conn, error) {
			return grafoMaq.DialLoopback(ctx, port)
		}
	}
	creds = credproxy.New(opts)
	if broker != "" {
		grafoMaq = grafo.New(broker, creds.Auditor(), logf)
	}
	syscall.Umask(0o077)

	// Como kling-vz: al crear o restaurar la VM se encierra en kling-hv.sb y
	// solo escribe en el directorio de su máquina; los dorados los publica el
	// daemon (internal/sandbox).
	confine, destino := sandbox.Preparar(sandbox.Opciones{Nombre: "kling-hv", Perfil: perfil,
		Sock: *sock, Broker: broker, Logf: logf})

	peers := peercred.New()
	srv := server.New(server.Deps{
		Factory: &hvvm.Factory{Console: stdout, Logf: logf, RAMDir: dirMaquina(*sock), Inservible: salirInservible},
		Vsock:   vsock,
		NewNet: func(c server.NetConfig) (server.Network, error) {
			var pg vnet.PGProxy
			if c.CredentialsPG != nil {
				pg = c.CredentialsPG
			}
			return vnet.New(vnet.Config{
				GuestMAC:      c.GuestMAC,
				MMDS:          c.MMDS,
				MMDSAddr:      c.MMDSAddr,
				Policy:        c.Policy,
				Resolver:      c.Resolver,
				Credentials:   c.Credentials,
				CredentialsPG: pg,
				Graph:         grafoDeRed(c.Graph),
				Logf:          logf,
				PeerAllowed:   peers.Allowed,
			})
		},
		Footprint:   hvvm.Footprint,
		Version:     version,
		Backend:     "hv",
		Logf:        logf,
		Policy:      policy,
		Resolver:    resolver,
		Credentials: creds,
		CredIP:      vnet.GatewayIP,
		CPUTime:     hvvm.CPUTime,
		Freeze:      hvvm.Freeze,
		Graph:       grafoMaq,
		Confine:     confine,
		Destino:     destino,
	})

	if fi, err := os.Lstat(*sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(*sock)
	}
	// sun_path admite 104 bytes: con una ruta larga se escucha por nombre
	// relativo desde su directorio (el núcleo conecta por un enlace corto).
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
	_ = creds.Close()
	return code
}

// salirInservible termina el proceso cuando ya no puede alojar una VM (ver
// hvvm.Factory.Inservible): con un margen para que la respuesta con el error
// llegue al núcleo, que verá morir al VMM y lanzará uno nuevo.
func salirInservible(err error) {
	logf("this process cannot host another VM (%v): exiting so the daemon starts a new kling-hv", err)
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Exit(1)
	}()
}

// dirMaquina es el directorio de la máquina (el del socket), absoluto y real.
func dirMaquina(sock string) string {
	dir := filepath.Dir(sock)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir
}

// rutaAuditoria es el registro de auditoría del proxy, junto al socket.
func rutaAuditoria(sock string) string {
	return filepath.Join(dirMaquina(sock), credproxy.AuditFile)
}

func grafoDeRed(g *grafo.Grafo) vnet.GraphLinks {
	if g == nil {
		return nil
	}
	return g
}

// rutaBroker es el socket del daemon para las conexiones a otras máquinas
// (pkg/linkbroker), o "" si el daemon no lo da.
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
