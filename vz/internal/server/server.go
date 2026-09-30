// Package server implementa el API de Firecracker que habla el núcleo, más las
// rutas propias /kling/*, sobre una VM que queda detrás de una interfaz.
//
// La VM es una interfaz para poder probar toda la máquina de estados (qué
// petición vale en qué momento, qué se escribe en el snapshot, cuándo se crea la
// VM en una restauración diferida) sin Virtualization.framework, que solo existe
// en un Mac con el entitlement firmado.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/grafo"
	"github.com/juan52878911/kindling/vz/internal/mmds"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// VM es lo que el servidor necesita de una máquina virtual.
type VM interface {
	Start() error
	Pause() error
	Resume() error
	// Stop la para en seco. La VM avisa por Stopped.
	Stop() error
	// SaveState vuelca el estado (con la VM en pausa) a path, que no debe existir.
	SaveState(path string) error
	// RestoreState carga el estado en una VM recién creada y la deja en pausa.
	RestoreState(path string) error
	SetBalloonTargetMiB(mib int) error
	// Stopped recibe un valor cuando la VM se para: nil si el invitado se apagó,
	// un error si el framework la dio por rota.
	Stopped() <-chan error
}

// Network es la red de espacio de usuario de la máquina.
type Network interface {
	VMFile() *os.File
	Forward(port int) (string, error)
	// Probe dice si algo escucha en ese puerto del invitado (ver getProbe).
	Probe(ctx context.Context, port int) bool
	Close()
}

// Factory crea VMs a partir de un Spec.
type Factory interface {
	// NewMachineID devuelve un identificador de máquina nuevo, en base64.
	NewMachineID() (string, error)
	// Create construye la VM sin arrancarla. bootArgs ya viene traducida.
	Create(s *spec.Spec, bootArgs string, net Network) (VM, error)
}

// NetConfig es lo que la red necesita del servidor para montarse.
type NetConfig struct {
	GuestMAC string
	MMDS     http.Handler // nil si MMDS no está configurado
	MMDSAddr netip.Addr
	Policy   *egress.Policy
	Resolver *egress.Resolver
	// Credentials es el proxy de credenciales que la red sirve en la pasarela;
	// nil si este proceso no lo tiene. CredentialsPG es el mismo proxy en su
	// papel de Postgres y MySQL (el resto de puertos TCP de la pasarela).
	Credentials   http.Handler
	CredentialsPG *credproxy.Proxy
	// Graph son las aristas link del nodo; nil si este proceso no tiene a
	// quién pedir las conexiones (sin KLING_VZ_BROKER).
	Graph *grafo.Grafo
}

type Deps struct {
	Factory Factory
	NewNet  func(NetConfig) (Network, error)
	// Footprint devuelve la memoria que ocupa la máquina en el host, en bytes.
	Footprint func() (uint64, error)
	Version   string
	Logf      func(format string, args ...any)
	// Resolver, si no es nil, siembra la allowlist al recibir la política.
	Resolver *egress.Resolver
	Policy   *egress.Policy
	// Credentials es el proxy de credenciales de la máquina (pkg/credproxy) y
	// CredIP la IP donde la red lo sirve, con la que el DNS contesta sus
	// dominios. Con Credentials nil, PUT /kling/credentials se rechaza.
	Credentials *credproxy.Proxy
	CredIP      netip.Addr
	// Confine, si no es nil, encierra el proceso en su perfil de sandbox con
	// lo que necesita la VM que va a crear (ver Confinamiento). Se llama UNA
	// vez, justo antes de crear o restaurar la VM, que es cuando ya se saben
	// la política de salida y los ficheros de la VM. Un fallo impide crear la
	// VM: preferimos no arrancar a arrancar sin la barrera.
	Confine func(Confinamiento) error
	// Destino, si no es nil, dice dónde escribir de verdad un fichero de
	// snapshot pedido en path: confinado, este proceso solo escribe en el
	// directorio de su máquina, así que un destino de fuera (el dorado de
	// kling commit en snapshots/<nombre>/) se escribe en un temporal suyo y
	// publicar lo deja en path (vz/internal/custodio). publicar nil: se
	// escribe en path directamente. Nil = siempre directamente.
	Destino func(path string) (escribir string, publicar func() error, err error)
	// CPUTime es la CPU que lleva gastada la VM (el auxiliar de Apple donde
	// corren sus vCPU). Sin ella no hay tope de CPU (ver cpu.go).
	CPUTime func() (time.Duration, error)
	// Freeze para (true) o reanuda (false) el auxiliar de Apple entero con
	// SIGSTOP/SIGCONT: el freno del tope de CPU. Sin él (o si falla), el
	// regulador pausa la VM por el framework, que para también el reloj del
	// invitado (ver cpu.go).
	Freeze func(stop bool) error
	// Graphics, si no es nil, añade una pantalla virtio-gpu a las máquinas que
	// ARRANCAN en este proceso. Una restauración usa la del snapshot, tenga o
	// no: el framework exige los mismos dispositivos que al guardar.
	Graphics *spec.Graphics

	// Graph, si no es nil, son las aristas del nodo (vz/internal/grafo): pide
	// al daemon cada conexión a otra máquina. Sin él, este kling-vz no
	// anuncia credproxy.CapGraphLink y rechaza PUT /kling/graph y las
	// credenciales con upstream_machine.
	Graph *grafo.Grafo
}

// Confinamiento es lo que el sandbox deja tocar a kling-vz, sacado de la VM
// que va a crear: solo sus ficheros, no los de las demás máquinas.
type Confinamiento struct {
	// ConRed: puede abrir conexiones al exterior (egress distinto de none).
	ConRed bool
	// Loopback: puede marcar al loopback del Mac, fuera del rango de los
	// reenvíos (allowlist: el upstream -upstream 127.0.0.1:5432 de una
	// credencial de base de datos).
	Loopback bool
	Graphics bool
	// Lectura: ficheros que solo lee (kernel, discos de solo lectura, el
	// estado que restaura). Escritura: los que lee y escribe (discos de
	// lectura y escritura).
	Lectura, Escritura []string
}

type state int

const (
	stConfiguring state = iota
	stPendingLoad
	stRunning
	stPaused
	stStopped
)

func (s state) String() string {
	switch s {
	case stConfiguring:
		return "Not started"
	case stPendingLoad:
		return "Paused" // cargada y sin reanudar: para el núcleo es una VM en pausa
	case stRunning:
		return "Running"
	case stPaused:
		return "Paused"
	}
	return "Stopped"
}

// Límites de lo que se lee de un cuerpo: la configuración cabe de sobra en
// 64 KiB; el almacén de MMDS puede llevar secretos de muchas sesiones.
const (
	maxConfigBody = 64 << 10
	maxMMDSBody   = 1 << 20
	// maxCredBody: 16 claves de hasta 4 KiB que el JSON puede escapar con
	// \uXXXX (seis bytes por uno) caben de sobra.
	maxCredBody = 1 << 20
)

type Server struct {
	d     Deps
	store *mmds.Store

	mu        sync.Mutex
	st        state
	spec      *spec.Spec
	vm        VM
	net       Network
	statePath string

	done     chan error
	doneOnce sync.Once

	globoEn []time.Duration // ver reaplicarGlobo; campo para las pruebas

	// confinado: ya se aplicó Deps.Confine; confinadoConRed, con qué red.
	confinado, confinadoConRed bool

	// Tope de CPU (cpu.go). regulando: la VM está en una pausa del regulador,
	// que el núcleo no ve (para él sigue running). cpuListo: el agente ya
	// escucha (o pasó la gracia) y rige el techo configurado.
	cpuPct                           int
	cpuEnMarcha, regulando, cpuListo bool
	cpuPausado                       time.Duration
	cpuPeriodo, cpuGracia            time.Duration // para las pruebas; 0 = por defecto
	cpuReloj                         reloj         // para las pruebas; nil = el de verdad

	// frenoMu protege congelado: el auxiliar está parado por el regulador
	// (Deps.Freeze). Va aparte de mu para que reanudarlo nunca espere a quien
	// tiene mu, que puede estar esperando justo a que el auxiliar conteste.
	frenoMu   sync.Mutex
	congelado bool
	// sueltaAviso: ya se avisó de que reanudar el auxiliar falla (con
	// frenoMu); se rearma al conseguirlo.
	sueltaAviso bool
	frenoAviso  bool // ya se avisó de que Freeze falla (con mu)
}

func New(d Deps) *Server {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Policy == nil {
		d.Policy = egress.NewPolicy()
	}
	return &Server{
		d:     d,
		store: mmds.NewStore(),
		spec:  &spec.Spec{},
		done:  make(chan error, 1),

		globoEn: []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second,
			4 * time.Second, 8 * time.Second, 15 * time.Second},
	}
}

// Done recibe cuando la VM se ha parado y el proceso debe terminar.
func (s *Server) Done() <-chan error { return s.done }

func (s *Server) finish(err error) {
	s.doneOnce.Do(func() { s.done <- err })
}

// Shutdown para la VM y la red. Es lo que se hace ante SIGTERM.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.soltarFreno()
	vm, n := s.vm, s.net
	s.vm, s.net = nil, nil
	s.st = stStopped
	s.mu.Unlock()
	if vm != nil {
		_ = vm.Stop()
	}
	if n != nil {
		n.Close()
	}
}

// Handler devuelve el mux del API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.getRoot)
	mux.HandleFunc("PUT /boot-source", s.putBootSource)
	mux.HandleFunc("PUT /machine-config", s.putMachineConfig)
	mux.HandleFunc("PUT /drives/{id}", s.putDrive)
	mux.HandleFunc("PATCH /drives/{id}", s.patchDrive)
	mux.HandleFunc("PUT /network-interfaces/{id}", s.putNetwork)
	mux.HandleFunc("PUT /entropy", s.putEntropy)
	mux.HandleFunc("PUT /balloon", s.putBalloon)
	mux.HandleFunc("PATCH /balloon", s.patchBalloon)
	mux.HandleFunc("GET /balloon/statistics", s.getBalloonStats)
	mux.HandleFunc("PUT /mmds/config", s.putMMDSConfig)
	mux.HandleFunc("PUT /mmds", s.putMMDS)
	mux.HandleFunc("PATCH /mmds", s.patchMMDS)
	mux.HandleFunc("GET /mmds", s.getMMDS)
	mux.HandleFunc("PUT /actions", s.putActions)
	mux.HandleFunc("PATCH /vm", s.patchVM)
	mux.HandleFunc("PUT /snapshot/create", s.putSnapshotCreate)
	mux.HandleFunc("PUT /snapshot/load", s.putSnapshotLoad)
	mux.HandleFunc("GET /kling/info", s.getInfo)
	mux.HandleFunc("PUT /kling/network", s.putKlingNetwork)
	mux.HandleFunc("PUT /kling/credentials", s.putKlingCredentials)
	mux.HandleFunc("PUT /kling/graph", s.putKlingGraph)
	mux.HandleFunc("PUT /kling/forwards", s.putForwards)
	mux.HandleFunc("GET /kling/probe", s.getProbe)
	mux.HandleFunc("GET /kling/stats", s.getStats)
	mux.HandleFunc("PUT /kling/cpu", s.putKlingCPU)
	mux.HandleFunc("GET /kling/cpu", s.getKlingCPU)
	mux.HandleFunc("GET /kling/screenshot", s.getScreenshot)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fault(w, fmt.Errorf("kling-vz does not implement %s %s", r.Method, r.URL.Path))
	})
	// Las peticiones rechazadas se registran: el núcleo solo ve el error en su
	// propio log, y quien lea `kling logs` de la máquina debe poder saber qué
	// le pidieron al VMM y por qué no lo hizo.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &faultRecorder{ResponseWriter: w}
		mux.ServeHTTP(rec, r)
		if rec.status >= 400 {
			s.d.Logf("%s %s -> %d %s", r.Method, r.URL.Path, rec.status, rec.body.String())
		}
	})
}

// faultRecorder guarda el código y el principio del cuerpo de los errores.
type faultRecorder struct {
	http.ResponseWriter
	status int
	body   strings.Builder
}

func (f *faultRecorder) WriteHeader(code int) {
	f.status = code
	f.ResponseWriter.WriteHeader(code)
}

func (f *faultRecorder) Write(b []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	if f.status >= 400 && f.body.Len() < 512 {
		f.body.WriteString(strings.TrimSpace(string(b)))
	}
	return f.ResponseWriter.Write(b)
}

// --- utilidades ---

func fault(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": err.Error()})
}

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, limit int64, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if int64(len(b)) > limit {
		return fmt.Errorf("request body larger than %d bytes", limit)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// preBoot comprueba que la configuración aún se pueda tocar. Como Firecracker:
// una vez creada la VM, los dispositivos no cambian.
func (s *Server) preBoot() error {
	if s.st != stConfiguring {
		return fmt.Errorf("the microVM is %s: this can only be configured before InstanceStart", s.st)
	}
	return nil
}

// --- API de Firecracker ---

func (s *Server) getRoot(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	writeJSON(w, map[string]string{
		"id": "anonymous-instance", "state": st.String(),
		"vmm_version": s.d.Version, "app_name": "kling-vz",
	})
}

func (s *Server) putBootSource(w http.ResponseWriter, r *http.Request) {
	var b spec.BootSource
	if err := decode(r, maxConfigBody, &b); err != nil {
		fault(w, err)
		return
	}
	if b.KernelImagePath == "" {
		fault(w, errors.New("kernel_image_path is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	s.spec.BootSource = &b
	noContent(w)
}

func (s *Server) putMachineConfig(w http.ResponseWriter, r *http.Request) {
	var m spec.MachineConfig
	if err := decode(r, maxConfigBody, &m); err != nil {
		fault(w, err)
		return
	}
	if m.VCPUCount < 1 || m.MemSizeMiB < 1 {
		fault(w, errors.New("vcpu_count and mem_size_mib must be positive"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	s.spec.MachineConfig = &m
	noContent(w)
}

func (s *Server) putDrive(w http.ResponseWriter, r *http.Request) {
	var d spec.Drive
	if err := decode(r, maxConfigBody, &d); err != nil {
		fault(w, err)
		return
	}
	id := r.PathValue("id")
	if d.DriveID == "" {
		d.DriveID = id
	}
	if d.DriveID != id {
		fault(w, fmt.Errorf("drive_id %q does not match the path (%q)", d.DriveID, id))
		return
	}
	if d.PathOnHost == "" {
		fault(w, errors.New("path_on_host is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	s.spec.PutDrive(d)
	noContent(w)
}

// patchDrive cambia la ruta REGISTRADA. Antes de crear la VM es la que se usa al
// crearla; con la VM creada, solo lo que se grabará en el próximo snapshot. El
// framework no deja cambiar el fichero de un disco en caliente, y el núcleo solo
// parchea con la VM en pausa alrededor de un snapshot (Commit).
func (s *Server) patchDrive(w http.ResponseWriter, r *http.Request) {
	var d struct {
		DriveID    string `json:"drive_id"`
		PathOnHost string `json:"path_on_host"`
	}
	if err := decode(r, maxConfigBody, &d); err != nil {
		fault(w, err)
		return
	}
	if d.PathOnHost == "" {
		fault(w, errors.New("path_on_host is required (kling-vz only patches the path)"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == stStopped {
		fault(w, errors.New("the microVM has stopped"))
		return
	}
	if err := s.spec.PatchDrivePath(r.PathValue("id"), d.PathOnHost); err != nil {
		fault(w, err)
		return
	}
	noContent(w)
}

func (s *Server) putNetwork(w http.ResponseWriter, r *http.Request) {
	var n spec.NetworkInterface
	if err := decode(r, maxConfigBody, &n); err != nil {
		fault(w, err)
		return
	}
	id := r.PathValue("id")
	if n.IfaceID == "" {
		n.IfaceID = id
	}
	if n.IfaceID != id {
		fault(w, fmt.Errorf("iface_id %q does not match the path (%q)", n.IfaceID, id))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	if s.spec.Network != nil && s.spec.Network.IfaceID != n.IfaceID {
		fault(w, errors.New("kling-vz supports a single network interface"))
		return
	}
	s.spec.Network = &n
	noContent(w)
}

func (s *Server) putEntropy(w http.ResponseWriter, r *http.Request) {
	// El cuerpo (rate_limiter) se ignora, pero se lee acotado igualmente.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxConfigBody))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	s.spec.Entropy = true
	noContent(w)
}

func (s *Server) putBalloon(w http.ResponseWriter, r *http.Request) {
	var b spec.Balloon
	if err := decode(r, maxConfigBody, &b); err != nil {
		fault(w, err)
		return
	}
	if b.AmountMiB < 0 {
		fault(w, errors.New("amount_mib cannot be negative"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	s.spec.Balloon = &b
	noContent(w)
}

func (s *Server) patchBalloon(w http.ResponseWriter, r *http.Request) {
	var b struct {
		AmountMiB *int `json:"amount_mib"`
	}
	if err := decode(r, maxConfigBody, &b); err != nil {
		fault(w, err)
		return
	}
	if b.AmountMiB == nil || *b.AmountMiB < 0 {
		fault(w, errors.New("amount_mib is required and cannot be negative"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spec.Balloon == nil {
		fault(w, errors.New("the balloon device is not configured"))
		return
	}
	if mc := s.spec.MachineConfig; mc != nil && *b.AmountMiB >= mc.MemSizeMiB {
		fault(w, fmt.Errorf("amount_mib (%d) must be below mem_size_mib (%d)", *b.AmountMiB, mc.MemSizeMiB))
		return
	}
	s.spec.Balloon.AmountMiB = *b.AmountMiB
	if s.vm != nil {
		s.soltarFreno()
		if err := s.vm.SetBalloonTargetMiB(s.spec.BalloonTargetMiB()); err != nil {
			fault(w, fmt.Errorf("moving the balloon: %w", err))
			return
		}
	}
	noContent(w)
}

// getBalloonStats: el framework no da estadísticas del invitado, así que la
// memoria va a 0, que el núcleo lee como "desconocido".
func (s *Server) getBalloonStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	b := s.spec.Balloon
	var amount int
	if b != nil {
		amount = b.AmountMiB
	}
	s.mu.Unlock()
	if b == nil {
		fault(w, errors.New("the balloon device is not configured"))
		return
	}
	writeJSON(w, map[string]any{
		"target_mib": amount, "actual_mib": amount,
		"free_memory": 0, "available_memory": 0, "total_memory": 0,
	})
}

func (s *Server) putMMDSConfig(w http.ResponseWriter, r *http.Request) {
	var c spec.MMDSConfig
	if err := decode(r, maxConfigBody, &c); err != nil {
		fault(w, err)
		return
	}
	if c.Version == "" {
		c.Version = "V2"
	}
	if c.Version != "V2" {
		fault(w, fmt.Errorf("kling-vz only implements MMDS V2 (got %q)", c.Version))
		return
	}
	if c.IPv4Address != "" {
		ip, err := netip.ParseAddr(c.IPv4Address)
		if err != nil || !ip.Is4() || !netip.MustParsePrefix("169.254.0.0/16").Contains(ip) {
			fault(w, fmt.Errorf("ipv4_address %q must be an IPv4 link-local address", c.IPv4Address))
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	if s.spec.Network == nil {
		fault(w, errors.New("MMDS needs a network interface configured first"))
		return
	}
	s.spec.MMDSConfig = &c
	noContent(w)
}

func (s *Server) putMMDS(w http.ResponseWriter, r *http.Request) {
	var v any
	if err := decode(r, maxMMDSBody, &v); err != nil {
		fault(w, err)
		return
	}
	s.store.Put(v)
	noContent(w)
}

func (s *Server) patchMMDS(w http.ResponseWriter, r *http.Request) {
	var v any
	if err := decode(r, maxMMDSBody, &v); err != nil {
		fault(w, err)
		return
	}
	s.store.Patch(v)
	noContent(w)
}

func (s *Server) getMMDS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.store.Get())
}

func (s *Server) putActions(w http.ResponseWriter, r *http.Request) {
	var a struct {
		ActionType string `json:"action_type"`
	}
	if err := decode(r, maxConfigBody, &a); err != nil {
		fault(w, err)
		return
	}
	if a.ActionType != "InstanceStart" {
		fault(w, fmt.Errorf("kling-vz does not implement action %q", a.ActionType))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.preBoot(); err != nil {
		fault(w, err)
		return
	}
	if err := s.start(); err != nil {
		fault(w, err)
		return
	}
	noContent(w)
}

func (s *Server) patchVM(w http.ResponseWriter, r *http.Request) {
	var v struct {
		State string `json:"state"`
	}
	if err := decode(r, maxConfigBody, &v); err != nil {
		fault(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Si el regulador tiene parado el auxiliar, el framework no contestaría.
	s.soltarFreno()
	var err error
	switch v.State {
	case "Paused":
		switch s.st {
		case stRunning:
			if s.regulando {
				// Ya está en pausa por el regulador de CPU: pasa a ser del
				// núcleo, y el regulador no la reanudará.
				s.regulando = false
				s.st = stPaused
				break
			}
			if err = s.vm.Pause(); err == nil {
				s.st = stPaused
			}
		case stPaused, stPendingLoad:
			// Idempotente, como en Firecracker.
		default:
			err = fmt.Errorf("cannot pause: the microVM is %s", s.st)
		}
	case "Resumed":
		switch s.st {
		case stPaused:
			if err = s.vm.Resume(); err == nil {
				s.st = stRunning
			}
		case stPendingLoad:
			err = s.restore()
		case stRunning:
		default:
			err = fmt.Errorf("cannot resume: the microVM is %s", s.st)
		}
	default:
		err = fmt.Errorf("invalid state %q (use Paused or Resumed)", v.State)
	}
	if err != nil {
		fault(w, err)
		return
	}
	noContent(w)
}

func (s *Server) putSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var c struct {
		SnapshotType string `json:"snapshot_type"`
		SnapshotPath string `json:"snapshot_path"`
		MemFilePath  string `json:"mem_file_path"`
	}
	if err := decode(r, maxConfigBody, &c); err != nil {
		fault(w, err)
		return
	}
	if c.SnapshotType != "" && c.SnapshotType != "Full" {
		fault(w, fmt.Errorf("kling-vz only implements Full snapshots (got %q)", c.SnapshotType))
		return
	}
	if c.SnapshotPath == "" || c.MemFilePath == "" {
		fault(w, errors.New("snapshot_path and mem_file_path are required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != stPaused {
		fault(w, fmt.Errorf("the microVM must be paused to create a snapshot (it is %s)", s.st))
		return
	}
	t0 := time.Now()
	destino := s.d.Destino
	if destino == nil {
		destino = func(p string) (string, func() error, error) { return p, nil, nil }
	}
	memW, pubMem, err := destino(c.MemFilePath)
	if err != nil {
		fault(w, fmt.Errorf("state file: %w", err))
		return
	}
	snapW, pubSnap, err := destino(c.SnapshotPath)
	if err != nil {
		fault(w, fmt.Errorf("snapshot file: %w", err))
		return
	}
	// Los temporales de un destino de fuera no deben quedarse en el
	// directorio de la máquina, salga bien o mal.
	defer func() {
		if pubMem != nil {
			_ = os.Remove(memW)
		}
		if pubSnap != nil {
			_ = os.Remove(snapW)
		}
	}()
	// El framework se niega a escribir sobre un fichero existente, y Firecracker
	// sí lo pisa: el núcleo reutiliza las rutas al volver a congelar.
	if err := os.Remove(memW); err != nil && !errors.Is(err, os.ErrNotExist) {
		fault(w, fmt.Errorf("removing the old state file: %w", err))
		return
	}
	if err := s.vm.SaveState(memW); err != nil {
		fault(w, fmt.Errorf("saving the machine state: %w", err))
		return
	}
	if err := s.spec.WriteFile(snapW); err != nil {
		fault(w, fmt.Errorf("writing the snapshot file: %w", err))
		return
	}
	var size int64
	if fi, err := os.Stat(memW); err == nil {
		size = fi.Size()
	}
	for _, pub := range []func() error{pubMem, pubSnap} {
		if pub == nil {
			continue
		}
		if err := pub(); err != nil {
			fault(w, fmt.Errorf("placing the snapshot: %w", err))
			return
		}
	}
	s.d.Logf("snapshot created in %d ms (state %.1f MiB)", time.Since(t0).Milliseconds(), float64(size)/(1<<20))
	noContent(w)
}

func (s *Server) putSnapshotLoad(w http.ResponseWriter, r *http.Request) {
	var l struct {
		SnapshotPath string `json:"snapshot_path"`
		MemFilePath  string `json:"mem_file_path"`
		MemBackend   *struct {
			BackendPath string `json:"backend_path"`
			BackendType string `json:"backend_type"`
		} `json:"mem_backend"`
		ResumeVM bool `json:"resume_vm"`
	}
	if err := decode(r, maxConfigBody, &l); err != nil {
		fault(w, err)
		return
	}
	statePath := l.MemFilePath
	if l.MemBackend != nil {
		if l.MemBackend.BackendType != "" && l.MemBackend.BackendType != "File" {
			fault(w, fmt.Errorf("kling-vz only implements the File memory backend (got %q)", l.MemBackend.BackendType))
			return
		}
		statePath = l.MemBackend.BackendPath
	}
	if l.SnapshotPath == "" || statePath == "" {
		fault(w, errors.New("snapshot_path and mem_backend.backend_path are required"))
		return
	}
	loaded, err := spec.ReadFile(l.SnapshotPath)
	if err != nil {
		fault(w, fmt.Errorf("loading snapshot: %w", err))
		return
	}
	if _, err := os.Stat(statePath); err != nil {
		fault(w, fmt.Errorf("loading snapshot: state file: %w", err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != stConfiguring || s.spec.BootSource != nil || len(s.spec.Drives) > 0 {
		fault(w, errors.New("a snapshot can only be loaded into a fresh, unconfigured kling-vz"))
		return
	}
	s.spec = loaded
	s.statePath = statePath
	s.st = stPendingLoad
	// La red se monta ya, y no al reanudar: así /kling/forwards funciona también
	// entre la carga y la reanudación.
	if err := s.ensureNet(); err != nil {
		s.st = stConfiguring
		s.spec = &spec.Spec{}
		fault(w, err)
		return
	}
	if l.ResumeVM {
		if err := s.restore(); err != nil {
			fault(w, err)
			return
		}
	}
	noContent(w)
}

// --- rutas propias ---

// credentialKinds son los tipos de credencial que entiende este kling-vz. El
// daemon lo pregunta antes de mandarle una credencial Postgres o MySQL: uno
// anterior ignoraría el tipo y la serviría como HTTP. "postgres-upstream"
// dice que entiende además Upstream, UpstreamTLS y TLSServerName (también en
// las MySQL, que nacieron con ellos).
var credentialKinds = []string{credproxy.KindHTTP, credproxy.KindPostgres, credproxy.CapPostgresUpstream, credproxy.KindMySQL, credproxy.CapHTTPPlaces}

func (s *Server) getInfo(w http.ResponseWriter, _ *http.Request) {
	info := map[string]any{"backend": "vz", "version": s.d.Version}
	if s.d.Credentials != nil {
		kinds := credentialKinds
		// Aristas de grafo y kling db attach: solo con el daemon al otro lado.
		if s.d.Graph != nil {
			kinds = append(append([]string(nil), kinds...), credproxy.CapGraphLink)
		}
		info["credential_kinds"] = kinds
	}
	writeJSON(w, info)
}

// putKlingGraph fija las aristas salientes del nodo (sustituye las
// anteriores): las link, que la red atiende en la pasarela, y los nombres
// <nodo>.graph que el DNS contesta (los de las link y los de las credential).
// No lleva direcciones: cada conexión se pide al daemon (vz/internal/grafo).
func (s *Server) putKlingGraph(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Links []grafo.Enlace `json:"links"`
		Hosts []string       `json:"hosts"`
	}
	if err := decode(r, maxConfigBody, &body); err != nil {
		fault(w, err)
		return
	}
	if s.d.Graph == nil || !s.d.CredIP.IsValid() {
		fault(w, errors.New("this kling-vz has no link broker: the daemon did not set KLING_VZ_BROKER"))
		return
	}
	if len(body.Hosts) > 2*grafo.MaxEnlaces {
		fault(w, fmt.Errorf("%d graph hosts; the limit is %d", len(body.Hosts), 2*grafo.MaxEnlaces))
		return
	}
	hosts := map[string]bool{}
	for _, h := range body.Hosts {
		if _, err := linkbroker.NodoDeHost(h); err != nil {
			fault(w, err)
			return
		}
		hosts[h] = true
	}
	for _, l := range body.Links {
		if !hosts[l.Host] {
			fault(w, fmt.Errorf("link %s:%d: its host is not in hosts", l.Host, l.Port))
			return
		}
	}
	if err := s.d.Graph.Set(body.Links); err != nil {
		fault(w, err)
		return
	}
	s.d.Policy.SetGraphHosts(body.Hosts, s.d.CredIP)
	noContent(w)
}

func (s *Server) putKlingNetwork(w http.ResponseWriter, r *http.Request) {
	var n struct {
		Egress       string   `json:"egress"`
		AllowDomains []string `json:"allow_domains"`
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
	// Confinado sin red, el sandbox ya no deja abrir conexiones al exterior:
	// aceptar ahora internet o allowlist sería decir que sí y que el invitado
	// no llegara a ninguna parte. Estrechar (o repetir none) sí vale.
	s.mu.Lock()
	sinRed := s.confinado && !s.confinadoConRed
	s.mu.Unlock()
	if sinRed && mode != egress.None {
		fault(w, fmt.Errorf("egress %s can't be enabled after the VM was created without network: "+
			"kling-vz is confined and can't open outside connections; set it before InstanceStart or snapshot/load", mode))
		return
	}
	s.d.Policy.Set(mode, n.AllowDomains)
	if mode == egress.Allowlist && s.d.Resolver != nil {
		// En segundo plano: resolver N dominios puede tardar segundos y el
		// núcleo espera a esta respuesta para arrancar. El invitado resuelve por
		// nuestro DNS antes de conectar, y eso siembra la IP en el acto.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s.d.Resolver.SeedStatic(ctx)
		}()
	}
	noContent(w)
}

// putKlingCredentials fija el juego COMPLETO de credenciales del proxy de esta
// máquina (sustituye el anterior; la lista vacía las quita todas) y desvía sus
// dominios a la pasarela en el DNS. Las claves se quedan en la memoria de este
// proceso: ni se escriben a disco ni se registran, y nada del invitado las ve
// (su memoria vive en otro proceso, el auxiliar de Apple). Es la ruta que en
// Linux no hace falta porque el proxy es una goroutine del propio daemon.
func (s *Server) putKlingCredentials(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Credentials []struct {
			Env         string   `json:"env"`
			Domain      string   `json:"domain"`
			Placeholder string   `json:"placeholder"`
			Secret      string   `json:"secret"`
			Allow       []string `json:"allow,omitempty"`
			Headers     []string `json:"headers,omitempty"`
			Query       bool     `json:"query,omitempty"`
			Body        bool     `json:"body,omitempty"`
			Kind        string   `json:"kind,omitempty"`
			Port        int      `json:"port,omitempty"`
			User        string   `json:"user,omitempty"`
			Database    string   `json:"database,omitempty"`
			AnyDatabase bool     `json:"any_database,omitempty"`
			CAPEM       string   `json:"ca_pem,omitempty"`
			// Upstream fijado por el operador (ver pkg/credproxy/upstream.go):
			// el proxy marca desde la pila de red del Mac, así que 127.0.0.1
			// es el loopback del Mac, donde Docker Desktop publica.
			Upstream      string `json:"upstream,omitempty"`
			UpstreamTLS   string `json:"upstream_tls,omitempty"`
			TLSServerName string `json:"tls_server_name,omitempty"`
			// Otra máquina de kindling (arista credential, kling db attach):
			// la conexión se pide al daemon en cada sesión (vz/internal/grafo).
			UpstreamMachine string `json:"upstream_machine,omitempty"`
			UpstreamOwner   string `json:"upstream_owner,omitempty"`
		} `json:"credentials"`
	}
	if err := decode(r, maxCredBody, &body); err != nil {
		fault(w, err)
		return
	}
	if s.d.Credentials == nil || !s.d.CredIP.IsValid() {
		fault(w, errors.New("this kling-vz has no credential proxy"))
		return
	}
	creds := make([]credproxy.Credential, 0, len(body.Credentials))
	soloMaquinas := len(body.Credentials) > 0
	for _, c := range body.Credentials {
		if c.UpstreamMachine != "" && s.d.Graph == nil {
			fault(w, fmt.Errorf("credential for %s: an upstream machine needs the link broker (the daemon did not set KLING_VZ_BROKER)", c.Domain))
			return
		}
		soloMaquinas = soloMaquinas && c.UpstreamMachine != ""
		creds = append(creds, credproxy.Credential{
			Env: c.Env, Domain: c.Domain, Placeholder: c.Placeholder, Secret: c.Secret,
			Allow:   c.Allow,
			Headers: c.Headers, Query: c.Query, Body: c.Body,
			Kind: c.Kind, Port: c.Port, User: c.User, Database: c.Database, AnyDatabase: c.AnyDatabase, CAPEM: c.CAPEM,
			Upstream: c.Upstream, UpstreamTLS: c.UpstreamTLS, TLSServerName: c.TLSServerName,
			UpstreamMachine: c.UpstreamMachine, UpstreamOwner: c.UpstreamOwner,
		})
	}
	// Como en Linux: el desvío lo hace el DNS propio de allowlist. En otro
	// modo el invitado resolvería por su cuenta (internet) o no tendría salida
	// que el proxy pudiera romper (none). La excepción son las aristas
	// credential de un grafo (todas hacia otra máquina): no salen a ningún
	// sitio más que a la máquina que el daemon entrega, y su nombre
	// (<nodo>.graph) lo contesta el DNS en todos los modos.
	if mode := s.d.Policy.Mode(); len(body.Credentials) > 0 && mode != egress.Allowlist && !soloMaquinas {
		fault(w, fmt.Errorf("credentials need egress allowlist (this machine has %s)", mode))
		return
	}
	doms, err := s.d.Credentials.SetCredentials(creds)
	if err != nil {
		fault(w, err)
		return
	}
	s.d.Policy.SetCredHosts(doms, s.d.CredIP)
	writeJSON(w, map[string]any{"domains": doms})
}

func (s *Server) putForwards(w http.ResponseWriter, r *http.Request) {
	var f struct {
		Ports []int `json:"ports"`
	}
	if err := decode(r, maxConfigBody, &f); err != nil {
		fault(w, err)
		return
	}
	s.mu.Lock()
	n := s.net
	s.mu.Unlock()
	if n == nil {
		fault(w, errors.New("the network is not up yet (configure a network interface and start or load the microVM first)"))
		return
	}
	out := map[string]string{}
	for _, p := range f.Ports {
		addr, err := n.Forward(p)
		if err != nil {
			fault(w, fmt.Errorf("forwarding port %d: %w", p, err))
			return
		}
		out[strconv.Itoa(p)] = addr
	}
	writeJSON(w, map[string]any{"forwards": out})
}

// probeTimeout es cuánto espera un sondeo a que el invitado conteste al SYN.
// Mientras su red no ha subido, el SYN se pierde y la pila lo reintenta; más
// que esto y quien sondea en bucle se queda esperando entero cada vuelta.
const probeTimeout = time.Second

// getProbe sirve GET /kling/probe?port=N: {"open": bool}, si algo escucha en
// ese puerto DENTRO del invitado.
//
// Hace falta porque el reenvío no lo dice: el puerto de loopback lo abre este
// proceso y acepta siempre, escuche el invitado o no (solo después, al no
// poder conectar dentro, cierra). Sondear la dirección del reenvío con un
// dial daba "abierto" al instante para un servidor que tardaba segundos en
// arrancar, o que no existía: probe_only contestaba 200 y wait_ms no esperaba.
func (s *Server) getProbe(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		fault(w, fmt.Errorf("invalid port %q", r.URL.Query().Get("port")))
		return
	}
	s.mu.Lock()
	n := s.net
	s.mu.Unlock()
	if n == nil {
		fault(w, errors.New("the network is not up yet"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	writeJSON(w, map[string]any{"open": n.Probe(ctx, port)})
}

func (s *Server) getStats(w http.ResponseWriter, _ *http.Request) {
	if s.d.Footprint == nil {
		fault(w, errors.New("footprint is not available on this platform"))
		return
	}
	b, err := s.d.Footprint()
	if err != nil {
		fault(w, err)
		return
	}
	writeJSON(w, map[string]any{"footprint_mib": int((b + (1<<20 - 1)) >> 20)})
}

// Screenshotter lo implementa una VM con ventana (vzvm con KLING_VZ_WINDOW=1).
type Screenshotter interface {
	Screenshot() ([]byte, error)
}

// getScreenshot devuelve la pantalla virtio-gpu tal y como la pinta la
// ventana, en PNG. Sin ventana no hay de dónde sacarla: el framework no expone
// el scanout de otro modo.
func (s *Server) getScreenshot(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	vm := s.vm
	s.mu.Unlock()
	sc, ok := vm.(Screenshotter)
	if vm == nil || !ok {
		fault(w, errors.New("this VM cannot take screenshots"))
		return
	}
	// Con el auxiliar parado por el regulador el framework no contesta, y la
	// ventana tampoco pinta: se suelta antes de pedirle la pantalla, como
	// hacen los demás caminos que le piden algo (patchVM, putBalloon).
	s.soltarFreno()
	png, err := sc.Screenshot()
	if err != nil {
		fault(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

// --- ciclo de vida ---

func (s *Server) ensureNet() error {
	if s.net != nil || s.spec.Network == nil || s.d.NewNet == nil {
		return nil
	}
	cfg := NetConfig{
		GuestMAC: s.spec.Network.GuestMAC,
		Policy:   s.d.Policy,
		Resolver: s.d.Resolver,
	}
	if s.d.Credentials != nil {
		cfg.Credentials = s.d.Credentials
		cfg.CredentialsPG = s.d.Credentials
	}
	cfg.Graph = s.d.Graph
	if c := s.spec.MMDSConfig; c != nil {
		cfg.MMDS = s.store.Handler()
		if ip, err := netip.ParseAddr(c.IPv4Address); err == nil {
			cfg.MMDSAddr = ip
		}
	}
	n, err := s.d.NewNet(cfg)
	if err != nil {
		return fmt.Errorf("setting up the network: %w", err)
	}
	s.net = n
	return nil
}

func (s *Server) create() error {
	if err := s.spec.Validate(); err != nil {
		return err
	}
	if err := s.ensureNet(); err != nil {
		return err
	}
	if err := s.confinar(); err != nil {
		return err
	}
	vm, err := s.d.Factory.Create(s.spec.Clone(), spec.TranslateBootArgs(s.spec.BootSource.BootArgs), s.net)
	if err != nil {
		return fmt.Errorf("creating the VM: %w", err)
	}
	s.vm = vm
	go s.watch(vm)
	return nil
}

// confinar aplica Deps.Confine la primera vez, con la red que pide la política
// de salida en ese momento. Se llama con s.mu tomado.
func (s *Server) confinar() error {
	if s.d.Confine == nil || s.confinado {
		return nil
	}
	c := s.confinamiento()
	if err := s.d.Confine(c); err != nil {
		return fmt.Errorf("confining kling-vz in its sandbox: %w", err)
	}
	s.confinado, s.confinadoConRed = true, c.ConRed
	return nil
}

// confinamiento es lo que la VM que se va a crear necesita tocar. Se llama
// con s.mu tomado y la spec ya definitiva (tras los PATCH de discos de una
// restauración).
func (s *Server) confinamiento() Confinamiento {
	modo := s.d.Policy.Mode()
	c := Confinamiento{
		ConRed:   modo != egress.None,
		Loopback: modo == egress.Allowlist,
		Graphics: s.spec.Graphics != nil,
	}
	if s.spec.BootSource != nil && s.spec.BootSource.KernelImagePath != "" {
		c.Lectura = append(c.Lectura, s.spec.BootSource.KernelImagePath)
	}
	for _, d := range s.spec.Drives {
		if d.IsReadOnly {
			c.Lectura = append(c.Lectura, d.PathOnHost)
		} else {
			c.Escritura = append(c.Escritura, d.PathOnHost)
		}
	}
	if s.st == stPendingLoad && s.statePath != "" {
		c.Lectura = append(c.Lectura, s.statePath)
	}
	return c
}

// abandon deshace una creación fallida. La red se conserva: sigue siendo válida
// para un reintento, y cerrar sus puertos rompería los que el núcleo ya tiene.
func (s *Server) abandon() {
	if s.vm != nil {
		vm := s.vm
		s.vm = nil
		_ = vm.Stop()
	}
}

func (s *Server) start() error {
	if s.spec.MachineIdentifier == "" {
		id, err := s.d.Factory.NewMachineID()
		if err != nil {
			return fmt.Errorf("machine identifier: %w", err)
		}
		s.spec.MachineIdentifier = id
	}
	if s.spec.Graphics == nil && s.d.Graphics != nil {
		g := *s.d.Graphics
		s.spec.Graphics = &g
	}
	t0 := time.Now()
	if err := s.create(); err != nil {
		return err
	}
	if err := s.vm.Start(); err != nil {
		s.abandon()
		return fmt.Errorf("starting the VM: %w", err)
	}
	s.st = stRunning
	s.applyBalloon()
	if s.spec.Balloon != nil && s.spec.Balloon.AmountMiB > 0 {
		go s.reaplicarGlobo(s.vm, s.globoEn)
	}
	s.d.Logf("instance started in %d ms", time.Since(t0).Milliseconds())
	return nil
}

func (s *Server) restore() error {
	t0 := time.Now()
	if err := s.create(); err != nil {
		return err
	}
	if err := s.vm.RestoreState(s.statePath); err != nil {
		s.abandon()
		return fmt.Errorf("restoring the machine state: %w", err)
	}
	tRestore := time.Since(t0)
	if err := s.vm.Resume(); err != nil {
		s.abandon()
		return fmt.Errorf("resuming the restored VM: %w", err)
	}
	s.st = stRunning
	s.applyBalloon()
	s.d.Logf("snapshot restored in %d ms (resumed after %d ms)", tRestore.Milliseconds(), time.Since(t0).Milliseconds())
	return nil
}

// applyBalloon lleva el globo a lo configurado. Al arrancar el framework deja
// al invitado con toda su memoria; con amount_mib>0 (techo de memoria del
// núcleo) hay que retener la diferencia desde el principio.
func (s *Server) applyBalloon() {
	if s.spec.Balloon == nil || s.spec.Balloon.AmountMiB == 0 || s.vm == nil {
		return
	}
	if err := s.vm.SetBalloonTargetMiB(s.spec.BalloonTargetMiB()); err != nil {
		s.d.Logf("warning: could not set the balloon target: %v", err)
	}
}

// reaplicarGlobo repite el objetivo del globo mientras el invitado arranca, en
// los momentos de en (contados desde el arranque).
//
// El objetivo que se fija justo tras Start() se pierde: el driver
// virtio-balloon del invitado aún no existe, y Virtualization.framework no lo
// hace llegar cuando aparece. Medido: con mem 512 de techo 1024 el invitado
// veía los 1024 enteros indefinidamente, y el mismo PATCH repetido a los 3 s
// sí inflaba. El framework no dice cuándo está listo el driver (no hay
// estadísticas del invitado), así que se repite unas veces en los primeros
// segundos; fijar el mismo objetivo otra vez no mueve nada si ya se cumplía.
// Se para si la VM cambia o deja de correr, o si ya no hay nada inflado.
func (s *Server) reaplicarGlobo(vm VM, en []time.Duration) {
	t0 := time.Now()
	for _, d := range en {
		time.Sleep(time.Until(t0.Add(d)))
		s.mu.Lock()
		if s.vm != vm || s.st != stRunning || s.spec.Balloon == nil || s.spec.Balloon.AmountMiB == 0 {
			s.mu.Unlock()
			return
		}
		// Lo mismo que putBalloon: parado por el regulador, el framework
		// no atendería al globo hasta el final de la parada.
		s.soltarFreno()
		err := vm.SetBalloonTargetMiB(s.spec.BalloonTargetMiB())
		s.mu.Unlock()
		if err != nil {
			s.d.Logf("warning: could not set the balloon target: %v", err)
			return
		}
	}
}

func (s *Server) watch(vm VM) {
	err := <-vm.Stopped()
	s.mu.Lock()
	current := s.vm == vm
	if current {
		s.st = stStopped
	}
	s.mu.Unlock()
	if !current {
		return // una VM abandonada tras un fallo: no es el fin del proceso
	}
	if err != nil {
		s.d.Logf("the VM stopped with an error: %v", err)
	} else {
		s.d.Logf("the guest stopped")
	}
	s.finish(err)
}
