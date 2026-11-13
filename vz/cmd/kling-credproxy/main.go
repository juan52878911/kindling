//go:build darwin

// kling-credproxy es el proxy de credenciales de UNA máquina en macOS, en un
// proceso aparte del VMM (docs/proxy-macos-separado.md del núcleo). Es el
// único proceso con las claves reales de esa máquina: el VMM (kling-vz o
// kling-hv), que procesa la red del invitado, solo sabe qué dominios desviar
// en su DNS y le pasa cada conexión del invitado a la pasarela por un socket.
//
// Lo lanza el daemon al entregar las credenciales de una máquina (no lo hace
// el VMM, que está confinado y no puede lanzar procesos), con su directorio
// (MDIR/credproxy) y el PID del VMM. En ese directorio abre dos sockets 0600:
//
//   - api.sock: el daemon, y solo él (LOCAL_PEERCRED + la ruta de su
//     ejecutable), le da el modo de salida y las credenciales;
//   - fwd.sock: el VMM de la máquina, y solo él, le pasa cada conexión con
//     una cabecera fija (credproxy.SplitHeader).
//
// Se encierra en kling-credproxy.sb antes de atender nada y termina en
// cuanto muere el VMM (kqueue, NOTE_EXIT): stop, freeze y rm se lo llevan
// con él, también con el daemon caído. Sobrevive a un reinicio del daemon,
// que lo vuelve a encontrar por su socket.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/credapi"
	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/grafo"
	"github.com/juan52878911/kindling/vz/internal/peercred"
	"github.com/juan52878911/kindling/vz/internal/sbinit"
)

// version la fija el build con -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// perfil es su sandbox.
//
//go:embed kling-credproxy.sb
var perfil string

// sinBroker es la ruta que el perfil recibe cuando no hay broker: no existe
// ni puede crearse, así que la regla no abre nada.
const sinBroker = "/dev/null/kling-credproxy-no-broker"

const (
	maxConfigBody = 64 << 10
	// maxCredBody: el mismo que el de PUT /kling/credentials de kling-vz.
	maxCredBody = 1 << 20
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s kling-credproxy: %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("kling-credproxy", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	vmmPID := fs.Int("vmm-pid", 0, "pid of the machine's VMM: this process exits when it does")
	// --dir va el último: el daemon lo busca así en la línea de órdenes, y
	// la ruta puede llevar espacios ("Application Support").
	dirFlag := fs.String("dir", "", "the proxy's directory (<machine dir>/credproxy)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("kling-credproxy", version)
		return 0
	}
	if *dirFlag == "" || *vmmPID <= 0 {
		fmt.Fprintln(os.Stderr, "kling-credproxy: --dir and --vmm-pid are required")
		return 2
	}
	daemonBin, vmmBin := os.Getenv("KLING_CREDPROXY_DAEMON"), os.Getenv("KLING_CREDPROXY_VMM")
	if !filepath.IsAbs(daemonBin) || !filepath.IsAbs(vmmBin) {
		logf("KLING_CREDPROXY_DAEMON and KLING_CREDPROXY_VMM must be absolute paths (the daemon sets them)")
		return 2
	}
	// Nada de lo que crea (sockets, registro, derramados) nace legible por
	// otros, ni el instante entre crearlo y el chmod.
	syscall.Umask(0o077)
	dir, err := dirPropio(*dirFlag)
	if err != nil {
		logf("%v", err)
		return 2
	}
	// Desde su directorio y por nombre relativo: sun_path admite 104 bytes y
	// la ruta del directorio de una máquina se los come con facilidad.
	if err := os.Chdir(dir); err != nil {
		logf("cannot chdir to %s: %v", dir, err)
		return 1
	}
	// El VMM, antes de encerrarse: el kqueue se registra ahora y avisa
	// después, ya dentro del sandbox. Uno que ya no existe es salir.
	vmmFin, err := vigilarVMM(*vmmPID)
	if err != nil {
		logf("the VMM (pid %d) is gone: %v", *vmmPID, err)
		return 1
	}

	policy := egress.NewPolicy()
	resolver := egress.NewResolver(policy)
	broker := rutaBroker()
	var grafoMaq *grafo.Grafo
	var p *credproxy.Proxy
	opts := credproxy.Options{
		// El mismo upstream y los mismos destinos prohibidos que la red del
		// invitado (egress.PublicIPv4), como cuando el proxy vivía en el VMM.
		Lookup: resolver.PublicIPv4,
		// Solo en allowlist (lo dice el daemon, no el VMM), o con todas las
		// credenciales hacia otra máquina (aristas credential, que el daemon
		// entrega por el broker).
		Enabled: func() bool {
			return policy.Mode() == egress.Allowlist || (grafoMaq != nil && p.SoloMaquinas())
		},
		AuditPath: filepath.Join(dir, credproxy.AuditFile),
		Logf:      logf,
	}
	if v := os.Getenv("KLING_VZ_CREDAUDIT"); v != "" {
		if c, err := credproxy.ParseAuditConfig(v); err == nil {
			opts.Audit = c
		} else {
			logf("WARNING: %v (using the defaults)", err)
		}
	}
	if tmp := filepath.Join(dir, credproxy.SplitTempDir); credproxy.PrepararTempDir(tmp) == nil {
		opts.TempDir = tmp
	} else {
		logf("WARNING: no spill directory at %s: large request bodies will be sent chunked", tmp)
	}
	if broker != "" {
		opts.DialMachine = func(ctx context.Context, id, owner string, port int) (net.Conn, error) {
			return grafoMaq.DialMachine(ctx, id, owner, port)
		}
		// El loopback del Mac (-upstream 127.0.0.1:5432) lo marca el daemon:
		// el perfil no abre el loopback a este proceso.
		opts.DialLoopback = func(ctx context.Context, port int) (net.Conn, error) {
			return grafoMaq.DialLoopback(ctx, port)
		}
	}
	p = credproxy.New(opts)
	if broker != "" {
		grafoMaq = grafo.New(broker, p.Auditor(), logf)
	}

	apiL, err := escuchar(credproxy.SplitAPISock)
	if err != nil {
		logf("%v", err)
		return 1
	}
	defer os.Remove(credproxy.SplitAPISock)
	fwdL, err := escuchar(credproxy.SplitFwdSock)
	if err != nil {
		logf("%v", err)
		return 1
	}
	defer os.Remove(credproxy.SplitFwdSock)

	if os.Getenv("KLING_VZ_NO_SANDBOX") == "1" {
		logf("WARNING: not confined, KLING_VZ_NO_SANDBOX=1")
	} else {
		b := broker
		if b == "" {
			b = sinBroker
		}
		t0 := time.Now()
		if err := sbinit.Aplicar(perfil, []string{"DIR", dir, "BROKER", b}); err != nil {
			logf("cannot confine itself: %v", err)
			return 1
		}
		logf("confined in %v: writes only %s", time.Since(t0).Round(time.Microsecond), dir)
	}

	// Solo el daemon habla con el API, y solo el VMM de la máquina con el
	// reenvío: un VMM tomado por su invitado no puede ni cambiar las
	// credenciales, ni leerlas, ni decir que la máquina está en allowlist.
	api := &apiServer{p: p, policy: policy, grafo: grafoMaq}
	hs := &http.Server{Handler: api.handler(), ReadHeaderTimeout: 10 * time.Second}
	apiErr := make(chan error, 1)
	go func() { apiErr <- hs.Serve(peercred.Solo(apiL, "API socket", []string{daemonBin}, logf)) }()
	split := credproxy.NewSplitServer(p)
	fwdErr := make(chan error, 1)
	go func() {
		fwdErr <- split.Serve(peercred.SoloPID(fwdL, "forward socket", []string{vmmBin}, *vmmPID, logf))
	}()
	logf("version %s serving %s (VMM pid %d)", version, dir, *vmmPID)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	code := 0
	select {
	case sig := <-sigs:
		logf("received %s: stopping", sig)
	case <-vmmFin:
		logf("the VMM (pid %d) exited: stopping", *vmmPID)
	case err := <-apiErr:
		logf("API server failed: %v", err)
		code = 1
	case err := <-fwdErr:
		logf("forward server failed: %v", err)
		code = 1
	}
	_ = hs.Close()
	_ = split.Close()
	// Después de cortar las conexiones: lo que quede en la cola del registro
	// llega al disco.
	_ = p.Close()
	return code
}

// dirPropio comprueba que dir es un directorio de este usuario que nadie más
// puede tocar (lo crea el daemon, 0700) y lo devuelve absoluto y real: el
// sandbox compara rutas reales.
func dirPropio(dir string) (string, error) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory of this user (the daemon creates it 0700)", dir)
	}
	return dir, nil
}

// escuchar abre el socket nombre en el directorio de trabajo, quitando antes
// uno que sobre de un proceso muerto (solo si es un socket).
func escuchar(nombre string) (net.Listener, error) {
	if fi, err := os.Lstat(nombre); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(nombre)
	}
	l, err := net.Listen("unix", nombre)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", nombre, err)
	}
	_ = os.Chmod(nombre, 0o600)
	return l, nil
}

// vigilarVMM devuelve un canal que se cierra cuando el proceso pid termina.
func vigilarVMM(pid int) (<-chan struct{}, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		unix.Close(kq)
		return nil, err
	}
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		out := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(kq, nil, out, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				logf("waiting for the VMM to exit: %v", err)
				return
			}
			if n > 0 {
				return
			}
		}
	}()
	return fin, nil
}

// rutaBroker es el socket del broker del daemon, real y absoluto, o "".
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

// apiServer es el API del daemon: GET /kling/info, PUT /kling/network y PUT
// /kling/credentials, con los mismos cuerpos que las de kling-vz para que el
// daemon use el mismo cliente.
type apiServer struct {
	p      *credproxy.Proxy
	policy *egress.Policy
	grafo  *grafo.Grafo
}

func (a *apiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /kling/info", a.info)
	mux.HandleFunc("PUT /kling/network", a.network)
	mux.HandleFunc("PUT /kling/credentials", a.credentials)
	return mux
}

func (a *apiServer) info(w http.ResponseWriter, _ *http.Request) {
	kinds := credapi.Kinds
	if a.grafo != nil {
		kinds = append(append([]string(nil), kinds...), credproxy.CapGraphLink)
	}
	writeJSON(w, map[string]any{"backend": "credproxy", "version": version, "pid": os.Getpid(), "credential_kinds": kinds})
}

// network fija el modo de salida de la máquina: el proxy solo atiende en
// allowlist (o con todas las credenciales hacia otra máquina).
func (a *apiServer) network(w http.ResponseWriter, r *http.Request) {
	var n struct {
		Egress       string   `json:"egress"`
		AllowDomains []string `json:"allow_domains,omitempty"`
	}
	if err := decode(r, maxConfigBody, &n); err != nil {
		fault(w, err)
		return
	}
	mode, err := egress.ParseMode(n.Egress)
	if err != nil {
		fault(w, err)
		return
	}
	a.policy.Set(mode, n.AllowDomains)
	w.WriteHeader(http.StatusNoContent)
}

// credentials fija el juego COMPLETO de credenciales (sustituye el anterior;
// la lista vacía las quita). Las claves se quedan en la memoria de este
// proceso: ni a disco ni al registro.
func (a *apiServer) credentials(w http.ResponseWriter, r *http.Request) {
	var body credapi.Cuerpo
	if err := decode(r, maxCredBody, &body); err != nil {
		fault(w, err)
		return
	}
	creds := make([]credproxy.Credential, 0, len(body.Credentials))
	soloMaquinas := len(body.Credentials) > 0
	for _, c := range body.Credentials {
		if c.UpstreamMachine != "" && a.grafo == nil {
			fault(w, fmt.Errorf("credential for %s: an upstream machine needs the link broker (the daemon did not set KLING_VZ_BROKER)", c.Domain))
			return
		}
		soloMaquinas = soloMaquinas && c.UpstreamMachine != ""
		creds = append(creds, c.Credential())
	}
	if mode := a.policy.Mode(); len(creds) > 0 && mode != egress.Allowlist && !soloMaquinas {
		fault(w, fmt.Errorf("credentials need egress allowlist (this machine has %s)", mode))
		return
	}
	doms, err := a.p.SetCredentials(creds)
	if err != nil {
		fault(w, err)
		return
	}
	writeJSON(w, map[string]any{"domains": doms})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fault(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": err.Error()})
}

func decode(r *http.Request, limit int64, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if int64(len(b)) > limit {
		return fmt.Errorf("request body larger than %s bytes", strconv.FormatInt(limit, 10))
	}
	return json.Unmarshal(b, v)
}
