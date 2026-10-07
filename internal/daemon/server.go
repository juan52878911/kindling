// Package daemon expone el gestor de microVMs por HTTP sobre un socket Unix.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// Version es la versión del daemon que devuelve GET /info. La fija el binario
// al arrancar (cmd/kling: daemon.Version = main.Version); antes era una
// constante "0.1.0" que nunca se actualizaba, así que /info mentía y nadie
// podía comprobar compatibilidad contra ella.
var Version = "dev"

// Capabilities son las capacidades del API que este daemon sirve. Una extensión
// (p. ej. kindling-mcp) las consulta en GET /info antes de usar una ruta, en vez
// de deducirlas de la versión. Solo se añaden nombres; nunca se reutilizan.
var Capabilities = []string{"annotations", "store", "builders", "image-files", "exec", "sandboxes", "shell", "resize", "image-blobs", "guest-resync", "shares-copy", "shares-live", "renew", "pause", "fork", "credaudit", "db-attach", "graphs", "authz", "cow-grow", "ready", api.CapabilityMachineEnv, api.CapabilityStart}

// guestProgressTimeout es el plazo de INACTIVIDAD al leer el CUERPO de una
// respuesta del invitado: se renueva con cada Read que devuelve datos, así
// que una respuesta larga pero que progresa (el streaming de /exec, una
// descarga real) no expira, y un invitado que calla a medio cuerpo —o que
// gotea 1 byte cada minuto solo para mantener viva una goroutine, un FD y
// hasta GuestMaxBodyCap de búfer del daemon— sí (D-01).
//
// Deliberadamente NO toca la espera de las CABECERAS: eso sigue siendo
// ResponseHeaderTimeout en guestClient, generoso a propósito porque una
// herramienta dentro del invitado (un escaneo de semgrep, por ejemplo) puede
// tardar lo suyo en tener algo que contestar. Es la lectura del cuerpo, una
// vez que ya se sabe que hay alguien al otro lado, la que no debe poder
// colgarse para siempre.
//
// Variable y no const solo para que los tests puedan acortarla; nada más la
// cambia en el proceso real.
var guestProgressTimeout = 60 * time.Second

// guestProxyIdleTimeout es el plazo de inactividad del cuerpo en POST
// /machines/{ref}/guest. No puede ser guestProgressTimeout: un servidor MCP
// Streamable HTTP contesta con cabeceras de text/event-stream enseguida y manda
// el evento con el resultado cuando la herramienta acaba, y una respuesta de
// chat/completions sin streaming (pkg/von) puede tardar minutos en un modelo en
// CPU. Se le da el mismo margen que a las cabeceras (ResponseHeaderTimeout de
// guestClient): lo que sí sigue acotado es el goteo infinito.
var guestProxyIdleTimeout = 5 * time.Minute

// guestRequestMaxBody es el tope de la petición a /guest, que lleva dentro el
// cuerpo entero que se reenvía al invitado (una llamada JSON-RPC con un fichero
// o una imagen en base64, un prompt largo). Va a la escala de la respuesta que
// se admite (GuestMaxBodyCap), no al MiB de los demás handlers JSON; el doble
// porque el cuerpo viaja como cadena JSON escapada.
const guestRequestMaxBody = 2 * api.GuestMaxBodyCap

// progressBody envuelve el cuerpo de una respuesta del invitado (resp.Body) y
// corta la lectura si un solo Read no vuelve en su plazo. Cerrar el cuerpo
// desde el lado del daemon hace que el Read bloqueado en la conexión real
// también se destrabe con un error, así que la goroutine que lo espera no se
// queda huérfana.
type progressBody struct {
	body  io.ReadCloser
	plazo time.Duration
	// buf es donde lee la goroutine; se copia a b solo si vuelve a tiempo.
	// Leer directamente en b dejaba que un Read tardío, tras el plazo,
	// escribiera en un buffer que el llamador ya había reutilizado. Solo hay
	// una goroutine a la vez: tras un plazo vencido, err queda fijado y no se
	// lanza ninguna más, así que buf no se comparte nunca.
	buf []byte
	err error
}

// wrapGuestBody es cómo se usa progressBody: se llama justo tras
// guestClient.Do, sobre resp.Body, antes de pasarlo a quien vaya a leerlo
// (LeerCuerpo, io.Copy). Usa guestProgressTimeout como plazo de inactividad.
func wrapGuestBody(body io.ReadCloser) io.ReadCloser {
	return wrapGuestBodyCon(body, guestProgressTimeout)
}

// wrapGuestBodyCon es wrapGuestBody con un plazo de inactividad propio. Lo usa
// el flujo de /exec/stream, donde un comando puede pasar legítimamente mucho
// más de guestProgressTimeout sin escribir nada (`sleep 120`, una compilación,
// `npm ci`): allí el plazo sale del timeout del propio comando.
func wrapGuestBodyCon(body io.ReadCloser, d time.Duration) io.ReadCloser {
	return &progressBody{body: body, plazo: d}
}

func (p *progressBody) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	if len(b) == 0 {
		return 0, nil
	}
	if cap(p.buf) < len(b) {
		p.buf = make([]byte, len(b))
	}
	buf := p.buf[:len(b)]
	type resultado struct {
		n   int
		err error
	}
	ch := make(chan resultado, 1)
	go func() { n, err := p.body.Read(buf); ch <- resultado{n, err} }()
	select {
	case r := <-ch:
		return copy(b, buf[:r.n]), r.err
	case <-time.After(p.plazo):
		_ = p.body.Close() // destraba el Read de la goroutine de arriba
		p.err = fmt.Errorf("the guest agent stopped answering (no data for %s)", p.plazo)
		return 0, p.err
	}
}

func (p *progressBody) Close() error { return p.body.Close() }

// guestClient reenvía peticiones al servidor dentro de la microVM. Es un
// singleton a nivel de paquete para que http.Client reúse sus conexiones
// entre llamadas al mismo invitado — recrearlo en cada request() obligaba a
// hacer un handshake TCP nuevo con cada tools/call.
// Timeout global NO: acota la petición entera, y al otro lado hay una microVM
// que puede estar descongelándose y una herramienta que puede tardar lo suyo
// —un escaneo de semgrep sobre un repo, por ejemplo—. Se acota la espera a las
// CABECERAS, que es lo que separa "está trabajando" de "no hay nadie"; una vez
// que llegan, wrapGuestBody acota por su cuenta el progreso del CUERPO.
var guestClient = &http.Client{Transport: guestTransports}

// guestTransports da un transporte por dirección de invitado, para poder
// soltar las conexiones de UNO cuando su red se desmonta (forgetIP) sin tocar
// las de los demás: con uno compartido, cada stop, rm o sandbox retirado
// cerraba las ociosas hacia TODAS las máquinas, y en un host con muchas altas
// y bajas cada exec volvía a abrir conexión.
var guestTransports = &transportesInvitado{}

// maxTransportesInvitado acota el mapa. En Linux cada transporte se olvida al
// desmontar la red de su máquina; en macOS la dirección es un reenvío de
// 127.0.0.1 con un puerto nuevo por VMM que nadie olvida (al morir kling-vz
// el reenvío se cierra y sus conexiones con él). Pasado el tope se empieza de
// cero: cuesta una conexión nueva por invitado, nada más.
const maxTransportesInvitado = 1024

type transportesInvitado struct {
	mu    sync.Mutex
	porID map[string]*http.Transport // host:puerto -> transporte
}

func (g *transportesInvitado) de(host string) *http.Transport {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.porID[host]; t != nil {
		return t
	}
	if g.porID == nil || len(g.porID) >= maxTransportesInvitado {
		for _, t := range g.porID {
			t.CloseIdleConnections()
		}
		g.porID = map[string]*http.Transport{}
	}
	t := &http.Transport{
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	g.porID[host] = t
	return t
}

func (g *transportesInvitado) RoundTrip(req *http.Request) (*http.Response, error) {
	return g.de(req.URL.Host).RoundTrip(req)
}

// CloseIdleConnections cierra las ociosas de todos (http.Client lo reenvía).
func (g *transportesInvitado) CloseIdleConnections() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, t := range g.porID {
		t.CloseIdleConnections()
	}
}

// forgetIP cierra las ociosas hacia ip (en cualquier puerto) y olvida sus
// transportes. Las peticiones en curso siguen en el suyo hasta acabar.
func (g *transportesInvitado) forgetIP(ip string) {
	if ip == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for host, t := range g.porID {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			h = host
		}
		if h == ip {
			t.CloseIdleConnections()
			delete(g.porID, host)
		}
	}
}

type Server struct {
	socket     string
	bus        *events.Bus
	mgr        *machine.Manager
	root       string
	fcBin      string
	socketUser string // a quién se cede el socket (vacío = a quien invocó sudo)

	store *store
	lock  *os.File // cerrojo de la raíz (ver bloquearRaiz); abierto mientras viva

	// fcVersion es la primera línea de `firecracker --version`, calculada una
	// vez al arrancar (D-02): GET /info la recalculaba en cada llamada con un
	// exec de más, y nada la cambia dentro de la vida del proceso —un binario
	// distinto en disco necesita un daemon nuevo para tenerse en cuenta.
	fcVersion string

	// authz es la política de autorización (authz.go). nil = sin política:
	// quien alcanza el socket manda sobre todo, como siempre.
	authz *Politica
	// identificar lee quién está al otro lado de una conexión. nil =
	// credencialesPar (SO_PEERCRED / LOCAL_PEERCRED); los tests ponen uno falso.
	identificar func(net.Conn) (Llamante, error)

	// constructor es el usuario sin privilegios de los constructores
	// aislados (builders_sinroot.go); nil = corren con el uid del daemon.
	// muConstructor los pone en fila: ver barrerProcesos.
	constructor   *usuarioConstructor
	muConstructor sync.Mutex

	// construyendo es un cerrojo por nombre de imagen: dos construcciones
	// del mismo nombre van en fila (bloquearNombreImagen). Bajo muConstruyendo.
	muConstruyendo sync.Mutex
	construyendo   map[string]*cerrojoImagen
}

// SetAuthz fija la política de autorización (nil = ninguna). Se llama antes de
// Listen: la política no cambia con el daemon en marcha.
func (s *Server) SetAuthz(p *Politica) { s.authz = p }

// SetShareConfig fija de dónde lee el daemon su configuración de carpetas
// compartidas (daemon.share_roots y daemon.share_copy_max_mib). Se consulta en
// cada petición: cambiarla no pide reiniciar.
func (s *Server) SetShareConfig(f func() machine.ShareConfig) { s.mgr.SetShareConfig(f) }

// SetCoW fija el modo de copia de discos (daemon.cow). Se lee una vez, al
// arrancar: cambiarlo pide reiniciar el daemon. Ver docs/cow.md.
func (s *Server) SetCoW(c machine.CoWConfig) { s.mgr.SetCoW(c) }

func New(socket, root, fcBin, socketUser, runAs string) (*Server, error) {
	lock, err := bloquearRaiz(root)
	if err != nil {
		return nil, err
	}
	// Antes del manager: al cargar reconcilia las máquinas vivas y levanta
	// sus proxies de credenciales, que toman ya el directorio de derrame.
	prepararRaizPlataforma(root)
	bus := events.New()
	mgr, err := machine.NewManager(root, fcBin, runAs, bus)
	if err != nil {
		lock.Close()
		return nil, err
	}
	st := &store{dir: filepath.Join(root, "store")}
	migrateLinks(root, st)
	fcVersion := firecrackerVersion(fcBin)
	// Lo que se graba en cada dorado y contra lo que se comparan los que hay:
	// un dorado de otro VMM sale obsoleto en vez de fallar al despertar.
	mgr.FijarOrigen(Version, fcVersion)
	// Una máquina parada o borrada deja su IP a la siguiente (kling start
	// conserva la suya): lo que guardara guestClient contra ella es un TCP
	// muerto, y el primer exec del VMM nuevo se comía un RST. Solo las de esa
	// IP: las de las demás máquinas siguen valiendo.
	mgr.OnGuestGone(guestTransports.forgetIP)
	return &Server{socket: socket, bus: bus, mgr: mgr, root: root, fcBin: fcBin, socketUser: socketUser, store: st, lock: lock,
		fcVersion: fcVersion}, nil
}

// firecrackerVersion ejecuta `firecracker --version` una vez. Si el binario
// no está (daemon con AVISO de host, ver comprobarHost) o no contesta lo que
// se espera, devuelve "": GET /info sencillamente omite el campo, igual que
// antes.
func firecrackerVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := bytes.Cut(out, []byte{'\n'})
	return string(line)
}

// rutas es la tabla del API: cada ruta con su acción de autorización (ver
// authz.go). Es el ÚNICO sitio donde se registra una ruta, así que ninguna se
// queda sin decidir quién puede usarla.
func (s *Server) rutas() []ruta {
	return []ruta{
		{"GET /info", AccionInfo, nil, s.handleInfo},
		{"GET /machines", AccionListar, nil, s.handleList},
		{"POST /machines", AccionCrear, revisarRun, s.handleRun},
		{"GET /machines/{ref}", AccionMaquina, nil, s.handleGet},
		{"POST /machines/{ref}/freeze", AccionMaquina, nil, s.handleFreeze},
		{"POST /machines/{ref}/thaw", AccionMaquina, nil, s.handleThaw},
		{"POST /machines/{ref}/pause", AccionMaquina, nil, s.handlePause},
		{"POST /machines/{ref}/squeeze", AccionMaquina, nil, s.handleSqueeze},
		{"POST /machines/{ref}/resize", AccionMaquina, nil, s.handleResize},
		{"POST /machines/{ref}/mmds", AccionMaquina, nil, s.handleMMDS},
		{"POST /machines/{ref}/credentials", AccionMaquina, revisarCredenciales, s.handleCredentials},
		{"DELETE /machines/{ref}/credentials/{env}", AccionMaquina, nil, s.handleRemoveCredential},
		{"POST /machines/{ref}/stop", AccionMaquina, nil, s.handleStop},
		{"POST /machines/{ref}/start", AccionMaquina, nil, s.handleStart},
		{"DELETE /machines/{ref}", AccionMaquina, nil, s.handleRemove},
		{"PUT /machines/{ref}/labels", AccionMaquina, revisarEtiquetas, s.handleLabels},
		{"POST /machines/{ref}/commit", AccionMaquina, revisarCommit, s.handleCommit},
		{"POST /images", AccionAdmin, nil, s.handleBuildImage},
		{"GET /images", AccionImagenes, nil, s.handleImages},
		{"DELETE /images/{name}", AccionAdmin, nil, s.handleRemoveImage},
		{"GET /volumes", AccionAdmin, nil, s.handleVolumes},
		{"POST /volumes", AccionAdmin, nil, s.handleCreateVolume},
		{"DELETE /volumes/{name}", AccionAdmin, nil, s.handleRemoveVolume},
		{"POST /volumes/{name}/populate", AccionAdmin, nil, s.handlePopulateVolume},
		{"GET /volumes/{name}/snapshots", AccionAdmin, nil, s.handleVolumeSnapshots},
		{"POST /volumes/{name}/snapshots", AccionAdmin, nil, s.handleSnapshotVolume},
		{"POST /volumes/{name}/restore", AccionAdmin, nil, s.handleRestoreVolume},
		{"DELETE /volumes/{name}/snapshots/{snap}", AccionAdmin, nil, s.handleRemoveVolumeSnapshot},
		{"GET /images/{name}/recipe", AccionAdmin, nil, s.handleImageRecipe},
		{"GET /images/{name}/files", AccionAdmin, nil, s.handleGetImageFile},
		{"PUT /images/{name}/files", AccionAdmin, nil, s.handlePutImageFile},
		{"GET /images/{name}/blob", AccionAdmin, nil, s.handleGetImageBlob},
		{"PUT /images/{name}/blob", AccionAdmin, nil, s.handlePutImageBlob},
		{"GET /snapshots", AccionListar, nil, s.handleSnapshots},
		{"GET /snapshots/{name}", AccionSnapLeer, nil, s.handleSnapshot},
		{"PUT /snapshots/{name}/annotations/{key}", AccionSnapEscribir, nil, s.handleSetAnnotation},
		{"PUT /snapshots/{name}/credentials", AccionSnapEscribir, revisarCredenciales, s.handleSnapshotCredentials},
		{"DELETE /snapshots/{name}/annotations/{key}", AccionSnapEscribir, nil, s.handleRemoveAnnotation},
		{"GET /store/{ns}", AccionAdmin, nil, s.handleStoreKeys},
		{"GET /store/{ns}/{key}", AccionAdmin, nil, s.handleStoreGet},
		{"PUT /store/{ns}/{key}", AccionAdmin, nil, s.handleStorePut},
		{"DELETE /store/{ns}/{key}", AccionAdmin, nil, s.handleStoreDelete},
		{"DELETE /snapshots/{name}", AccionSnapEscribir, nil, s.handleRemoveSnapshot},
		{"GET /machines/{ref}/logs", AccionMaquina, nil, s.handleLogs},
		{"GET /machines/{ref}/credaudit", AccionMaquina, nil, s.handleCredAudit},
		{"POST /machines/{ref}/guest", AccionMaquina, nil, s.handleGuest},
		{"POST /machines/{ref}/renew", AccionMaquina, nil, s.handleRenew},
		{"GET /machines/{ref}/ready", AccionMaquina, nil, s.handleReady},
		{"POST /machines/{ref}/hooks", AccionMaquina, nil, s.handleHooks},
		{"POST /machines/{ref}/exec", AccionMaquina, nil, s.handleExec},
		{"POST /machines/{ref}/shell", AccionMaquina, nil, s.handleShell},
		{"GET /machines/{ref}/files", AccionMaquina, nil, s.handleFiles},
		{"PUT /machines/{ref}/files", AccionMaquina, nil, s.handleFiles},
		{"DELETE /machines/{ref}/files", AccionMaquina, nil, s.handleFiles},
		{"POST /shares/uploads", AccionAdmin, nil, s.handleShareUpload},
		{"POST /sandboxes", AccionCrear, revisarSandbox, s.handleCreateSandbox},
		{"GET /sandboxes", AccionListar, nil, s.handleListSandboxes},
		{"GET /sandboxes/{ref}", AccionMaquina, nil, s.handleGetSandbox},
		{"POST /sandboxes/{ref}/renew", AccionMaquina, nil, s.handleRenewSandbox},
		{"POST /sandboxes/{ref}/fork", AccionMaquina, revisarFork, s.handleForkSandbox},
		{"DELETE /sandboxes/{ref}", AccionMaquina, nil, s.handleRemoveSandbox},
		{"POST /graphs", AccionCrear, revisarGrafo, s.handleGraphUp},
		{"GET /graphs", AccionListar, nil, s.handleGraphs},
		{"GET /graphs/{ref}", AccionGrafo, nil, s.handleGraph},
		{"POST /graphs/{ref}/freeze", AccionGrafo, nil, s.handleGraphFreeze},
		{"POST /graphs/{ref}/thaw", AccionGrafo, nil, s.handleGraphThaw},
		{"POST /graphs/{ref}/snapshot", AccionGrafo, nil, s.handleGraphSnapshot},
		{"POST /graphs/{ref}/fork", AccionGrafo, nil, s.handleGraphFork},
		{"DELETE /graphs/{ref}", AccionGrafo, nil, s.handleGraphRemove},
		{"GET /events", AccionListar, nil, s.handleEvents},
		{"GET /metrics", AccionAdmin, nil, s.handleMetrics},
		{"GET /procstats", AccionAdmin, nil, s.handleProcStats},
		{"POST /cow/store/grow", AccionAdmin, nil, s.handleGrowCoWStore},
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.rutas() {
		mux.HandleFunc(rt.patron, s.autorizar(rt))
	}
	return conVersionAPI(sinBarrasEscapadas(mux))
}

// conVersionAPI pone en cada respuesta el API y la versión del daemon: el
// cliente (pkg/api) los compara sin tener que preguntar /info en cada orden
// (ver api.APIVersion).
func conVersionAPI(h http.Handler) http.Handler {
	apiVer := strconv.Itoa(api.APIVersion)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderAPI, apiVer)
		w.Header().Set(api.HeaderVersion, Version)
		h.ServeHTTP(w, r)
	})
}

// sinBarrasEscapadas rechaza las rutas con una barra escapada (%2F). El mux
// desescapa cada segmento ANTES de casarlo con el patrón, así que
// DELETE /volumes/..%2Fimages%2Fmin llegaba al handler con el nombre
// "../images/min" y borraba la imagen base, y lo mismo con cualquier {name} o
// {ref} que acabe en una ruta del disco. Ningún nombre legítimo (máquina,
// imagen, volumen, snapshot) lleva una barra, así que se corta aquí la clase
// entera; los nombres se validan además donde se construye la ruta.
func sinBarrasEscapadas(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// La contrabarra se mira también ya desescapada: %5C es como Go la
		// escapa por defecto, así que con %5C (mayúscula) RawPath queda vacío
		// y mirar solo RawPath no la veía.
		if raw := r.URL.RawPath; strings.ContainsRune(r.URL.Path, '\\') || raw != "" && (strings.Contains(raw, "%2F") || strings.Contains(raw, "%2f") ||
			strings.Contains(raw, "%5C") || strings.Contains(raw, "%5c")) {
			fail(w, http.StatusBadRequest, errors.New("escaped slashes are not allowed in the path: no name contains one"))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Listen sirve hasta que se cancele el contexto.
func (s *Server) Listen(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o755); err != nil {
		return err
	}
	// Un socket huérfano de una ejecución anterior impediría escuchar.
	if err := os.Remove(s.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// El limite de sun_path son 104 bytes en macOS y 108 en Linux, y pasarse da
	// "bind: invalid argument", que no menciona ni la longitud ni el socket.
	if n := len(s.socket); n >= 104 {
		return fmt.Errorf("socket path is %d bytes and a unix socket allows about 104: %s", n, s.socket)
	}
	// 0660, y el daemon necesita root por KVM, pero el CLI entra como usuario
	// normal (por SSH, `kling dial-stdio`): el socket se cede a ese usuario y a
	// su grupo en lugar de obligar a que todo el CLI vaya con sudo. Sin seguir
	// enlaces: ver escucharSocket.
	uid, gid, ceder := s.socketOwner()
	ln, err := escucharSocket(s.socket, ceder, uid, gid)
	if err != nil {
		return err
	}

	// Vigilancia de vida: una microVM puede morir sola (pánico del invitado, OOM
	// del host). Decir "running" sobre algo muerto haría que el gateway enrutara
	// peticiones a la nada.
	s.mgr.Watch(ctx, 10*time.Second)

	srv := &http.Server{
		Handler:     s.routes(),
		ConnContext: s.conContexto,
		// Sin timeouts, un cliente que abre la conexión y nunca termina
		// el header mantiene una goroutine y un FD indefinidamente. El
		// daemon controla root: este es el tipo de superficie que no
		// debe existir.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	// El apagado se ORDENA desde una goroutine pero se COMPLETA en el camino
	// principal. Es la diferencia entre limpiar y creer que se limpió:
	// srv.Shutdown hace que Serve retorne de inmediato, así que todo lo que se
	// ponga detrás del Shutdown dentro de esta goroutine corre contra la salida
	// del proceso y normalmente la pierde.
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(shutdownDone)
	}()

	// Los externos que hacen falta para arrancar una microVM. Solo se comprobaba la
	// red (`ip`, `iptables`), asi que el daemon se quedaba escuchando y aceptando
	// peticiones sin `firecracker`, sin `setpriv` y sin `mkfs.ext4`. `setpriv` era el
	// peor: no lo cubria ninguna comprobacion y fallaba por microVM en pleno arranque.
	//
	// Sigue siendo AVISO y no error: el daemon vale para inspeccionar estado aunque no
	// pueda arrancar nada. Pero ahora los nombra TODOS de golpe.
	s.comprobarHost()

	if s.mgr.PrivWarning != "" {
		log.Printf("SECURITY WARNING: %s", s.mgr.PrivWarning)
	}
	if s.mgr.CgroupWarning != "" {
		log.Printf("WARNING: no CPU limit per microVM: %s", s.mgr.CgroupWarning)
	}
	if s.mgr.JailerWarning != "" {
		log.Printf("SECURITY WARNING: %s", s.mgr.JailerWarning)
	}
	if s.mgr.JailerBlocked != "" {
		log.Printf("WARNING: %s", s.mgr.JailerBlocked)
	}
	if s.authz != nil {
		log.Printf("authz policy %s: operations are authorized per caller (docs/authz.md)", s.authz.Ruta)
	} else {
		log.Printf("no authz policy (%s): whoever reaches the socket controls everything", RutaAuthzPorDefecto)
	}
	log.Printf("kling daemon %s listening on %s (root=%s)", Version, s.socket, s.root)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	// Serve retorna en cuanto se cierran los listeners, pero Shutdown sigue
	// drenando las peticiones en vuelo. Esperarlo antes de tocar nada garantiza
	// que ninguna petición cambie el estado después de la limpieza.
	<-shutdownDone
	// Con las peticiones ya drenadas, esta es la última escritura del estado.
	// Shutdown se rinde a los 5 s, y una petición lenta (un freeze de varios
	// GiB) o una operación del vigilante puede seguir en marcha: Close espera
	// a las operaciones de ciclo de vida en curso antes de cerrar la escritura
	// (con plazo), y la que acabe aún más tarde escribe su propia foto. Perder
	// la última transición hace que el arranque siguiente reconstruya algo
	// que no es.
	s.mgr.Close()
	_ = os.Remove(s.socket)
	return nil
}

// socketOwner resuelve a quién ceder el socket: al usuario indicado por
// configuración o, si no lo hay, a quien haya invocado sudo.
func (s *Server) socketOwner() (uid, gid int, ok bool) {
	if !cederSocket {
		return 0, 0, false // macOS: el daemon ya es el usuario; no hay a quién cederlo
	}
	if s.socketUser != "" {
		u, err := user.Lookup(s.socketUser)
		if err != nil {
			log.Printf("warning: unknown user %q: %v", s.socketUser, err)
			return 0, 0, false
		}
		uid, _ = strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
		return uid, gid, uid != 0
	}
	u, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	g, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil || u == 0 {
		return 0, 0, false
	}
	return u, g, true
}

// ── handlers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail escribe el error con su código.
//
// Si el error YA trae un código propio, ese manda: es información que el manager
// puso a propósito para que quien llama pueda reaccionar —el gateway hace sitio
// cuando ve un 507— y aplastarla con un 500 genérico la perdería justo donde
// hace falta.
func fail(w http.ResponseWriter, code int, err error) {
	var se *api.StatusError
	if errors.As(err, &se) && se.Code != 0 {
		code = se.Code
	}
	writeJSON(w, code, api.Error{Message: err.Error()})
}

// jsonMaxBody es el tope de los handlers que decodifican JSON directo del
// cuerpo (D-05). El socket ya equivale a root en este host, así que el riesgo
// es bajo, pero varios handlers no tenían NINGÚN tope mientras otros (MMDS,
// resize, ficheros) sí lo llevan: la inconsistencia es la que se corrige.
const jsonMaxBody = 1 << 20 // 1 MiB

// decodeJSON decodifica el cuerpo de r como JSON en v, acotado a jsonMaxBody.
// Pasarse el tope no trunca en silencio: http.MaxBytesReader hace que el
// Decode falle con un *http.MaxBytesError, que jsonBodyStatus traduce a 413.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeJSONCon(w, r, v, jsonMaxBody)
}

// decodeJSONCon es decodeJSON con un tope propio.
func decodeJSONCon(w http.ResponseWriter, r *http.Request, v any, max int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	return json.NewDecoder(r.Body).Decode(v)
}

// jsonBodyStatus traduce un error de decodeJSON a su código HTTP: 413 si fue
// el tope de tamaño el que lo cortó, 400 para cualquier otro (JSON inválido,
// campo con el tipo que no toca, etc).
func jsonBodyStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	_, kvmErr := os.Stat("/dev/kvm")
	info := api.Info{
		Version:      Version,
		API:          api.APIVersion,
		Root:         s.root,
		KVM:          kvmErr == nil,
		Machines:     s.contarMaquinas(r),
		Capabilities: Capabilities,
		Backend:      s.mgr.Backend(),
		Arch:         runtime.GOARCH,
		ShareRoots:   s.mgr.ShareRoots(),
		CoW:          s.mgr.CoWInfo(),
		Authz:        s.infoAuthz(r),
	}
	if cifrado, conocido := machine.CifradoEnReposo(s.root); conocido {
		info.EncryptedAtRest = &cifrado
	}
	// Los ajustes del host no son de ningún inquilino.
	if _, filtra := inquilinoDe(r); !filtra {
		info.Tuning = s.ajustes()
	}
	if s.fcVersion != "" {
		info.Firecrack = s.fcVersion
	}
	// Quien no tiene rol solo recibe lo que `kling doctor` necesita para
	// explicarle por qué se le niega todo (versión, capacidades, su authz):
	// nada de rutas ni del almacén del host.
	if rol, ok := rolDe(r); ok && !rol.Valido() {
		info = api.Info{Version: Version, API: api.APIVersion, Capabilities: Capabilities, Backend: info.Backend, Authz: info.Authz}
	}
	writeJSON(w, http.StatusOK, info)
}

// ajustes son los del manager más los que viven en el daemon: de dónde salen
// los constructores y con qué usuario corren los que lo admiten.
func (s *Server) ajustes() map[string]string {
	a := s.mgr.Ajustes()
	a["KLING_BUILDERS_DIR"] = ajusteBuildersDir()
	a["builder_user"] = "daemon"
	if s.constructor != nil {
		a["builder_user"] = s.constructor.Nombre
	}
	return a
}

// ajusteBuildersDir es lo que se dice de KLING_BUILDERS_DIR. Sin la variable,
// los constructores sin root son el propio binario del daemon
// (resolverConstructor), y decir solo el directorio por defecto hacía creer
// que se usaban los instalados ahí.
func ajusteBuildersDir() string {
	if d := os.Getenv("KLING_BUILDERS_DIR"); d != "" {
		return d
	}
	return "unset (oci, debian, android: the daemon's own binary; others: " + buildersDirPorDefecto + ")"
}

// contarMaquinas es cuántas máquinas ve quien pregunta.
func (s *Server) contarMaquinas(r *http.Request) int {
	if _, filtra := inquilinoDe(r); filtra {
		return len(filtrarMaquinas(r, s.mgr.List()))
	}
	return s.mgr.Count()
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, filtrarMaquinas(r, s.mgr.List()))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	mc, ok := s.mgr.Get(r.PathValue("ref"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("that machine doesn't exist"))
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req api.RunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	decodeMS := time.Since(start).Milliseconds()
	// kling.graph y kling.graph.* las pone solo el daemon (grafos).
	if err := api.ValidateNoGraphLabels(req.Labels); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.Run(r.Context(), req)
	if err != nil {
		fail(w, runStatus(err), err)
		return
	}
	// Un log barato por creación (A4): el daemon solo puede medir, sin abrir
	// el manager, cuánto costó decodificar el cuerpo y cuánto la llamada a
	// Run() entera. BootMS, si lo hay, es el desglose de esa llamada para el
	// arranque en frío (runFrom, al restaurar un snapshot, no lo rellena).
	total := time.Since(start).Milliseconds()
	if mc.BootMS > 0 {
		log.Printf("created %s in %d ms (decode %d ms, boot %d ms)", mc.Name, total, decodeMS, mc.BootMS)
	} else {
		log.Printf("created %s in %d ms (decode %d ms)", mc.Name, total, decodeMS)
	}
	writeJSON(w, http.StatusCreated, mc)
}

// runStatus traduce los errores de arrancar una máquina a códigos HTTP.
func runStatus(err error) int {
	switch {
	case errors.Is(err, machine.ErrExecNotInSnapshot), errors.Is(err, machine.ErrDoradoObsoleto):
		return http.StatusConflict
	case errors.Is(err, machine.ErrShareRequest), errors.Is(err, machine.ErrEnvRequest):
		return http.StatusBadRequest
	case errors.Is(err, machine.ErrNameTaken):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// cicloStatus traduce los errores de las operaciones sobre una máquina que ya
// existe (freeze, pause, thaw, stop, rm...): 404 si no existe, 409 si su
// estado no admite la operación. Lo demás sigue siendo 400, como siempre; un
// api.StatusError del manager (507, 503...) manda sobre todo esto (ver fail).
func cicloStatus(err error) int {
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		return http.StatusNotFound
	case errors.Is(err, machine.ErrWrongState):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (s *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Freeze(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Pause(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleThaw(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Thaw(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	var req api.ResizeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.Resize(r.Context(), r.PathValue("ref"), req.MemMiB)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, machine.ErrNoMachine):
			code = http.StatusNotFound
		case errors.Is(err, machine.ErrNotRunning):
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleSqueeze(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	res, err := s.mgr.SqueezeWith(r.Context(), r.PathValue("ref"), force)
	if err != nil {
		code := cicloStatus(err)
		if errors.Is(err, machine.ErrSqueezeShared) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleMMDS inyecta el store MMDS (un secreto de sesión) en una microVM viva. El
// cuerpo es el documento JSON del store tal cual; se pasa opaco al manager, que lo
// entrega a Firecracker. No se interpreta aquí: el esquema lo entiende el bridge.
func (s *Server) handleMMDS(w http.ResponseWriter, r *http.Request) {
	var data json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, api.MaxMMDSBytes)).Decode(&data); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.PutMMDS(r.Context(), r.PathValue("ref"), data)
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request) {
	var req api.CredentialsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.SetCredentials(r.Context(), r.PathValue("ref"), req.Credentials)
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

// handleRemoveCredential quita una credencial de una máquina por su variable.
// ?upstream_machine=<id> exige que vaya a esa máquina (kling db detach).
func (s *Server) handleRemoveCredential(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.RemoveCredential(r.Context(), r.PathValue("ref"), r.PathValue("env"), r.URL.Query().Get("upstream_machine"))
	if err != nil {
		code := cicloStatus(err)
		if strings.Contains(err.Error(), "has no credential") {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleSnapshotCredentials(w http.ResponseWriter, r *http.Request) {
	var req api.CredentialsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	snap, err := s.mgr.SetSnapshotCredentials(r.PathValue("name"), req.Credentials, req.Clear)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "does not exist") {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Stop(r.PathValue("ref"))
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

// handleStart arranca otra vez una máquina parada. El cuerpo (StartRequest)
// es opcional: sin entorno no hace falta mandarlo.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req api.StartRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	mc, err := s.mgr.Start(r.Context(), r.PathValue("ref"), req.Env)
	if err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Remove(r.PathValue("ref")); err != nil {
		fail(w, cicloStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	tail := 200
	// Un tail que no se puede parsear no debe caer callado al defecto: eso
	// esconde un error de quien llama (una `-tail` mal pasada) detrás de una
	// respuesta 200 que no es la que se pidió.
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid tail %q: must be a non-negative integer", v))
			return
		}
		tail = n
	}
	out, err := s.mgr.Logs(r.PathValue("ref"), tail)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// handleCredAudit sirve el registro de auditoría del proxy de credenciales de
// una máquina como NDJSON (una api.CredAuditRecord por línea). Funciona con la
// máquina corriendo, congelada o parada: es un fichero de su directorio.
func (s *Server) handleCredAudit(w http.ResponseWriter, r *http.Request) {
	q, err := parseCredAuditQuery(r.URL.Query(), time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	recs, err := s.mgr.CredAudit(r.PathValue("ref"), q)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	for _, rec := range recs {
		_ = enc.Encode(rec)
	}
}

// parseCredAuditQuery lee tail (defecto 200, 0 = todo), denied (booleano) y
// since (RFC 3339 o una duración hacia atrás desde now, "10m"). Como en
// handleLogs, un valor que no se entiende es un 400, no el defecto callado.
func parseCredAuditQuery(v url.Values, now time.Time) (api.CredAuditQuery, error) {
	q := api.CredAuditQuery{Tail: 200}
	if t := v.Get("tail"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return q, fmt.Errorf("invalid tail %q: must be a non-negative integer", t)
		}
		q.Tail = n
	}
	if d := v.Get("denied"); d != "" {
		b, err := strconv.ParseBool(d)
		if err != nil {
			return q, fmt.Errorf("invalid denied %q: must be 1, 0, true or false", d)
		}
		q.Denied = b
	}
	if since := v.Get("since"); since != "" {
		if ts, err := time.Parse(time.RFC3339, since); err == nil {
			q.Since = ts
		} else if d, err := time.ParseDuration(since); err == nil && d > 0 {
			q.Since = now.Add(-d)
		} else {
			return q, fmt.Errorf("invalid since %q: must be an RFC 3339 time or a positive duration like 10m", since)
		}
	}
	return q, nil
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	var labels map[string]string
	if err := decodeJSON(w, r, &labels); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	if err := s.mgr.SetLabels(r.PathValue("ref"), labels); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req api.CommitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	if req.ReadyTimeoutSeconds < 0 || req.ReadyTimeoutSeconds > api.ReadyMaxWaitSeconds {
		fail(w, http.StatusBadRequest, fmt.Errorf("ready_timeout_seconds must be between 0 and %d", api.ReadyMaxWaitSeconds))
		return
	}
	snap, err := s.mgr.CommitWith(r.Context(), r.PathValue("ref"), req.Name, machine.CommitOptions{
		Replace: req.Replace, SkipReady: req.SkipReady,
		ReadyWait: time.Duration(req.ReadyTimeoutSeconds) * time.Second,
	})
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, machine.ErrSharesCommit) || errors.Is(err, machine.ErrNotReady) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.filtrarSnapshots(r, s.mgr.Snapshots()))
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RemoveImage(r.PathValue("name")); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	// De paso, los temporales de sidecar que dejó un daemon muerto a medias.
	barrerSidecarsHuerfanos(filepath.Join(s.root, "images"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RemoveSnapshot(r.PathValue("name")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents emite NDJSON: un evento por línea, vaciando el buffer en cada uno
// para que el CLI los vea llegar en tiempo real.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("streaming not supported"))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsub := s.bus.Subscribe()
	defer unsub()

	enc := json.NewEncoder(w)
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Type == api.EvDropped {
				// El aviso de descartes no es de ninguna máquina. A un
				// inquilino se le dice que perdió eventos, pero no cuántos: el
				// recuento incluye los de las máquinas de los demás.
				if _, filtra := inquilinoDe(r); filtra {
					ev.Dropped, ev.Message = 0, "events dropped: this subscriber did not read them in time"
				}
			} else if !s.eventoVisible(r, ev) {
				continue
			}
			if enc.Encode(ev) != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			// Línea vacía como latido: detecta clientes muertos al otro lado del SSH.
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleGuest habla con el servidor que corre dentro de una microVM y devuelve
// su respuesta tal cual.
//
// Existe porque las IP de los invitados solo son alcanzables desde el host: un
// CLI conectado por SSH no tiene ruta hasta ellas, y sondearlas directamente se
// queda colgado hasta agotar el plazo. El daemon sí está en la red buena.
func (s *Server) handleGuest(w http.ResponseWriter, r *http.Request) {
	var req api.GuestRequest
	if err := decodeJSONCon(w, r, &req, guestRequestMaxBody); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	mc, ok := s.mgr.Get(r.PathValue("ref"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("that machine doesn't exist"))
		return
	}
	if mc.State != api.StateRunning || !mc.Reachable() {
		fail(w, http.StatusConflict, fmt.Errorf("the machine is %s, not accepting calls", mc.State))
		return
	}

	port := req.Port
	if port == 0 {
		port = api.GuestPort
	}
	// Solo el puerto del agente, salvo que la máquina declare otros. Antes el
	// proxy llegaba a cualquier puerto del invitado, y con él cualquiera con
	// acceso al socket alcanzaba servicios internos de la microVM que nadie
	// había decidido exponer. Los puertos extra se declaran con la etiqueta
	// kling.ports (lista separada por comas), que un snapshot hereda.
	if !puertoPermitido(mc, port) {
		fail(w, http.StatusForbidden, fmt.Errorf("port %d is not exposed by %s: only %d, or those listed in its %s label",
			port, mc.Name, api.GuestPort, api.LabelPorts))
		return
	}
	addr := mc.Addr(port)
	esperar := func(ctx context.Context, timeout time.Duration) error {
		return s.esperarPuerto(ctx, mc, port, timeout)
	}
	out, code, err := proxyGuest(r.Context(), addr, req, esperar)
	if err != nil {
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// proxyGuest manda req al servidor que escucha en addr dentro del invitado y
// devuelve su respuesta. Si falla, code es el estado HTTP con el que contestar.
// Está separado del handler para poder probarlo sin una microVM.
//
// esperar es cómo se espera a que el puerto abra (wait_ms, probe_only): la
// dirección no basta en macOS, donde el reenvío acepta siempre (esperarPuerto).
func proxyGuest(ctx context.Context, addr string, req api.GuestRequest,
	esperar func(context.Context, time.Duration) error) (api.GuestResponse, int, error) {
	// El daemon no añade nada de ningún protocolo: ruta, cabeceras de ida y
	// cuáles devolver las decide quien llama.
	path := req.Path
	respHeaders := req.ResponseHeaders
	if len(respHeaders) == 0 {
		respHeaders = []string{"Content-Type"}
	}
	if path == "" && !req.ProbeOnly {
		return api.GuestResponse{}, http.StatusBadRequest, fmt.Errorf("missing path")
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		return api.GuestResponse{}, http.StatusBadRequest, fmt.Errorf("path must start with '/': %q", path)
	}
	maxBody := req.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = api.GuestMaxBody
	}
	if maxBody > api.GuestMaxBodyCap {
		return api.GuestResponse{}, http.StatusBadRequest,
			fmt.Errorf("max_body_bytes %d is over the %d cap", maxBody, api.GuestMaxBodyCap)
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}

	// Un servidor recién arrancado tarda en escuchar. Esperar aquí, y no en el
	// cliente, mantiene el sondeo en la red que puede verlo.
	if req.WaitMS > 0 {
		if err := esperar(ctx, time.Duration(req.WaitMS)*time.Millisecond); err != nil {
			return api.GuestResponse{}, http.StatusGatewayTimeout, err
		}
	}
	if req.ProbeOnly {
		return api.GuestResponse{Status: http.StatusOK}, 0, nil
	}

	greq, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, strings.NewReader(req.Body))
	if err != nil {
		return api.GuestResponse{}, http.StatusBadRequest, err
	}
	greq.Header.Set("Content-Type", "application/json")
	for k, v := range req.Headers {
		greq.Header.Set(k, v)
	}

	resp, err := guestClient.Do(greq)
	if err != nil {
		return api.GuestResponse{}, http.StatusBadGateway, err
	}
	defer resp.Body.Close()

	// El límite evita que un invitado que se desmadre agote la memoria del
	// daemon. Se falla en vez de truncar: una respuesta a medias parece buena.
	// wrapGuestBodyCon (D-01) acota, además, el PROGRESO: un cuerpo que gotea
	// un byte cada minuto no debe poder tener esto leyendo para siempre. Con
	// guestProxyIdleTimeout y no guestProgressTimeout: ver su comentario.
	body, err := api.LeerCuerpo(wrapGuestBodyCon(resp.Body, guestProxyIdleTimeout), maxBody)
	if err != nil {
		return api.GuestResponse{}, http.StatusBadGateway, err
	}
	out := api.GuestResponse{Status: resp.StatusCode, Body: string(body), Headers: map[string]string{}}
	for _, h := range respHeaders {
		if v := resp.Header.Get(h); v != "" {
			out.Headers[h] = v
		}
	}
	return out, 0, nil
}

// waitPort espera a que algo escuche en addr, o se rinde al agotar el plazo.
// esperarPuerto espera a que algo escuche en ese puerto del invitado de mc.
//
// En Linux es waitPort sobre su IP. En macOS se pregunta a kling-vz
// (/kling/probe): el reenvío de loopback lo abre el ayudante y acepta la
// conexión escuche el invitado o no, así que waitPort daba por abierto al
// instante un servidor que aún no escuchaba, o que no existía —probe_only
// contestaba 200 y wait_ms no esperaba nada—.
func (s *Server) esperarPuerto(ctx context.Context, mc *api.Machine, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		open, ok := s.mgr.ProbeGuestPort(ctx, mc.ID, port)
		if !ok {
			return waitPort(ctx, mc.Addr(port), time.Until(deadline))
		}
		if open {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nobody is listening on port %d inside %s", port, mc.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func waitPort(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	// Backoff corto (A4): 200 ms fijos entre sondeos son 200 ms de más en el
	// caso común, un agente que ya escucha para la segunda o tercera vuelta.
	// Arranca en 5 ms y dobla hasta un tope de 50 ms, para no convertir la
	// espera larga (un host cargado, el invitado aún descongelándose) en un
	// busy-loop.
	wait := 5 * time.Millisecond
	const maxWait = 50 * time.Millisecond
	// Cada intento, con su propio plazo corto que crece. El primero sale
	// cuando el invitado aún no tiene red: su SYN se pierde, y el kernel no lo
	// retransmite hasta el RTO inicial de TCP, 1 s. Con un plazo fijo de 1 s,
	// cada arranque en frío esperaba ese segundo entero aunque el agente ya
	// escuchara a los ~290 ms. Cortando el intento, el siguiente manda un SYN
	// nuevo (y el reintento ARP de 50 ms del tap0, en internal/net, evita la
	// otra espera de 1 s). Crece hasta 1 s para que un host muy cargado, donde
	// el SYN-ACK tarda de verdad, no se quede reintentando para siempre.
	intento := 50 * time.Millisecond
	const maxIntento = time.Second
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, intento)
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		if intento *= 2; intento > maxIntento {
			intento = maxIntento
		}
		time.Sleep(wait)
		if wait *= 2; wait > maxWait {
			wait = maxWait
		}
	}
	return fmt.Errorf("nobody opened %s: %w", addr, last)
}

// missingBinaries devuelve los externos que el daemon necesita y no encuentra.
//
// `cp` y `chmod` no estan: son coreutils y su ausencia significa que el sistema esta
// roto de una forma que esta lista no arregla. `fallocate` tampoco, que solo compacta
// el fichero de memoria y degrada sin romper nada.
func missingBinaries() []string {
	var missing []string
	for _, b := range []string{"firecracker", "setpriv", "mkfs.ext4"} {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	return missing
}

// puertoPermitido dice si el proxy puede llegar a ese puerto de la máquina.
func puertoPermitido(mc *api.Machine, port int) bool {
	if port == api.GuestPort {
		return true
	}
	for _, p := range strings.Split(mc.Labels[api.LabelPorts], ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n == port {
			return true
		}
	}
	return false
}
