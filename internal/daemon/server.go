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
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	knet "github.com/juan52878911/kindling/internal/net"
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
var Capabilities = []string{"annotations", "store", "builders", "image-files", "exec", "sandboxes", "shell", "resize"}

// guestClient reenvía peticiones al servidor dentro de la microVM. Es un
// singleton a nivel de paquete para que http.Client reúse sus conexiones
// entre llamadas al mismo invitado — recrearlo en cada request() obligaba a
// hacer un handshake TCP nuevo con cada tools/call.
// Timeout global NO: acota la petición entera, y al otro lado hay una microVM
// que puede estar descongelándose y una herramienta que puede tardar lo suyo
// —un escaneo de semgrep sobre un repo, por ejemplo—. Se acota la espera a las
// CABECERAS, que es lo que separa "está trabajando" de "no hay nadie".
var guestClient = &http.Client{Transport: &http.Transport{
	ResponseHeaderTimeout: 5 * time.Minute,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       90 * time.Second,
}}

type Server struct {
	socket     string
	bus        *events.Bus
	mgr        *machine.Manager
	root       string
	fcBin      string
	socketUser string // a quién se cede el socket (vacío = a quien invocó sudo)

	store *store
}

func New(socket, root, fcBin, socketUser, runAs string) (*Server, error) {
	bus := events.New()
	mgr, err := machine.NewManager(root, fcBin, runAs, bus)
	if err != nil {
		return nil, err
	}
	st := &store{dir: filepath.Join(root, "store")}
	migrateLinks(root, st)
	return &Server{socket: socket, bus: bus, mgr: mgr, root: root, fcBin: fcBin, socketUser: socketUser, store: st}, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("GET /machines", s.handleList)
	mux.HandleFunc("POST /machines", s.handleRun)
	mux.HandleFunc("GET /machines/{ref}", s.handleGet)
	mux.HandleFunc("POST /machines/{ref}/freeze", s.handleFreeze)
	mux.HandleFunc("POST /machines/{ref}/thaw", s.handleThaw)
	mux.HandleFunc("POST /machines/{ref}/squeeze", s.handleSqueeze)
	mux.HandleFunc("POST /machines/{ref}/resize", s.handleResize)
	mux.HandleFunc("POST /machines/{ref}/mmds", s.handleMMDS)
	mux.HandleFunc("POST /machines/{ref}/stop", s.handleStop)
	mux.HandleFunc("DELETE /machines/{ref}", s.handleRemove)
	mux.HandleFunc("PUT /machines/{ref}/labels", s.handleLabels)
	mux.HandleFunc("POST /machines/{ref}/commit", s.handleCommit)
	mux.HandleFunc("POST /images", s.handleBuildImage)
	mux.HandleFunc("GET /images", s.handleImages)
	mux.HandleFunc("DELETE /images/{name}", s.handleRemoveImage)
	mux.HandleFunc("GET /volumes", s.handleVolumes)
	mux.HandleFunc("POST /volumes", s.handleCreateVolume)
	mux.HandleFunc("DELETE /volumes/{name}", s.handleRemoveVolume)
	mux.HandleFunc("POST /volumes/{name}/populate", s.handlePopulateVolume)
	mux.HandleFunc("GET /images/{name}/recipe", s.handleImageRecipe)
	mux.HandleFunc("GET /images/{name}/files", s.handleGetImageFile)
	mux.HandleFunc("PUT /images/{name}/files", s.handlePutImageFile)
	mux.HandleFunc("GET /snapshots", s.handleSnapshots)
	mux.HandleFunc("GET /snapshots/{name}", s.handleSnapshot)
	mux.HandleFunc("PUT /snapshots/{name}/annotations/{key}", s.handleSetAnnotation)
	mux.HandleFunc("DELETE /snapshots/{name}/annotations/{key}", s.handleRemoveAnnotation)
	mux.HandleFunc("GET /store/{ns}", s.handleStoreKeys)
	mux.HandleFunc("GET /store/{ns}/{key}", s.handleStoreGet)
	mux.HandleFunc("PUT /store/{ns}/{key}", s.handleStorePut)
	mux.HandleFunc("DELETE /store/{ns}/{key}", s.handleStoreDelete)
	mux.HandleFunc("DELETE /snapshots/{name}", s.handleRemoveSnapshot)
	mux.HandleFunc("GET /machines/{ref}/logs", s.handleLogs)
	mux.HandleFunc("POST /machines/{ref}/guest", s.handleGuest)

	// Ejecución y ficheros (solo máquinas con allow_exec) y sandboxes.
	mux.HandleFunc("POST /machines/{ref}/exec", s.handleExec)
	mux.HandleFunc("POST /machines/{ref}/shell", s.handleShell)
	mux.HandleFunc("GET /machines/{ref}/files", s.handleFiles)
	mux.HandleFunc("PUT /machines/{ref}/files", s.handleFiles)
	mux.HandleFunc("DELETE /machines/{ref}/files", s.handleFiles)
	mux.HandleFunc("POST /sandboxes", s.handleCreateSandbox)
	mux.HandleFunc("GET /sandboxes", s.handleListSandboxes)
	mux.HandleFunc("GET /sandboxes/{ref}", s.handleGetSandbox)
	mux.HandleFunc("POST /sandboxes/{ref}/renew", s.handleRenewSandbox)
	mux.HandleFunc("DELETE /sandboxes/{ref}", s.handleRemoveSandbox)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /procstats", s.handleProcStats)
	return mux
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
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return err
	}
	// 0660: el acceso al daemon equivale a root en este host.
	if err := os.Chmod(s.socket, 0o660); err != nil {
		return err
	}
	// El daemon necesita root por KVM, pero el CLI entra como usuario normal
	// (por SSH, `kling dial-stdio`). Cedemos el socket a ese usuario en lugar de
	// obligar a que todo el CLI vaya con sudo.
	if uid, gid, ok := s.socketOwner(); ok {
		if err := os.Chown(s.socket, uid, gid); err != nil {
			log.Printf("warning: couldn't hand off the socket to uid %d: %v", uid, err)
		} else {
			log.Printf("socket handed off to uid %d gid %d", uid, gid)
		}
	}

	// Vigilancia de vida: una microVM puede morir sola (pánico del invitado, OOM
	// del host). Decir "running" sobre algo muerto haría que el gateway enrutara
	// peticiones a la nada.
	s.mgr.Watch(ctx, 10*time.Second)

	srv := &http.Server{
		Handler: s.routes(),
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
	if missing := missingBinaries(); len(missing) > 0 {
		log.Printf("WARNING: missing binaries (%s): microVMs cannot be started on this host",
			strings.Join(missing, ", "))
	}

	if err := knet.Available(); err != nil {
		log.Printf("WARNING: network unavailable (%v): microVMs will boot without connectivity", err)
	} else if err := knet.SetupHost(); err != nil {
		log.Printf("WARNING: couldn't install the host barrier rules: %v", err)
	}

	if s.mgr.PrivWarning != "" {
		log.Printf("SECURITY WARNING: %s", s.mgr.PrivWarning)
	}
	if s.mgr.CgroupWarning != "" {
		log.Printf("WARNING: no CPU limit per microVM: %s", s.mgr.CgroupWarning)
	}
	log.Printf("kling daemon %s listening on %s (root=%s)", Version, s.socket, s.root)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	// Serve retorna en cuanto se cierran los listeners, pero Shutdown sigue
	// drenando las peticiones en vuelo. Esperarlo antes de tocar nada garantiza
	// que ninguna petición cambie el estado después de la limpieza.
	<-shutdownDone
	// Con las peticiones ya drenadas, esta es la última escritura del estado y
	// nadie va a cambiarlo por detrás. Se espera de verdad: perder la última
	// transición hace que el arranque siguiente reconstruya algo que no es.
	s.mgr.Close()
	_ = os.Remove(s.socket)
	return nil
}

// socketOwner resuelve a quién ceder el socket: al usuario indicado por
// configuración o, si no lo hay, a quien haya invocado sudo.
func (s *Server) socketOwner() (uid, gid int, ok bool) {
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

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	_, kvmErr := os.Stat("/dev/kvm")
	info := api.Info{
		Version:      Version,
		Root:         s.root,
		KVM:          kvmErr == nil,
		Machines:     s.mgr.Count(),
		Capabilities: Capabilities,
	}
	if cifrado, conocido := machine.CifradoEnReposo(s.root); conocido {
		info.EncryptedAtRest = &cifrado
	}
	if out, err := exec.Command(s.fcBin, "--version").Output(); err == nil {
		if line, _, _ := bytes.Cut(out, []byte{'\n'}); len(line) > 0 {
			info.Firecrack = string(line)
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
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
	var req api.RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.Run(r.Context(), req)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, machine.ErrExecNotInSnapshot) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusCreated, mc)
}

func (s *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Freeze(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleThaw(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Thaw(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
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
	res, err := s.mgr.Squeeze(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleMMDS inyecta el store MMDS (un secreto de sesión) en una microVM viva. El
// cuerpo es el documento JSON del store tal cual; se pasa opaco al manager, que lo
// entrega a Firecracker. No se interpreta aquí: el esquema lo entiende el bridge.
func (s *Server) handleMMDS(w http.ResponseWriter, r *http.Request) {
	var data json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&data); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.PutMMDS(r.Context(), r.PathValue("ref"), data)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	mc, err := s.mgr.Stop(r.PathValue("ref"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, mc)
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Remove(r.PathValue("ref")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	out, err := s.mgr.Logs(r.PathValue("ref"), tail)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	var labels map[string]string
	if err := json.NewDecoder(r.Body).Decode(&labels); err != nil {
		fail(w, http.StatusBadRequest, err)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	snap, err := s.mgr.Commit(r.Context(), r.PathValue("ref"), req.Name, req.Replace)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Snapshots())
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RemoveImage(r.PathValue("name")); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, ok := s.mgr.Get(r.PathValue("ref"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("that machine doesn't exist"))
		return
	}
	if mc.State != api.StateRunning || mc.IP == "" {
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
	addr := net.JoinHostPort(mc.IP, strconv.Itoa(port))
	out, code, err := proxyGuest(r.Context(), addr, req)
	if err != nil {
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// proxyGuest manda req al servidor que escucha en addr dentro del invitado y
// devuelve su respuesta. Si falla, code es el estado HTTP con el que contestar.
// Está separado del handler para poder probarlo sin una microVM.
func proxyGuest(ctx context.Context, addr string, req api.GuestRequest) (api.GuestResponse, int, error) {
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
		if err := waitPort(ctx, addr, time.Duration(req.WaitMS)*time.Millisecond); err != nil {
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
	body, err := api.LeerCuerpo(resp.Body, maxBody)
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
func waitPort(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		time.Sleep(200 * time.Millisecond)
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
