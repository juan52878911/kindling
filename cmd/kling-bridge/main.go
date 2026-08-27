// kling-bridge convierte un servidor MCP de stdio en uno de Streamable HTTP.
//
// EL PROBLEMA. La mayoría de servidores MCP open source solo hablan `stdio`: un
// proceso hijo persistente con el que se habla por tuberías. Eso es lo contrario
// de invocable bajo demanda — no hay puerto al que llamar, y el ciclo de vida lo
// impone el cliente, no el servidor.
//
// LA SOLUCIÓN. Este puente corre DENTRO de la microVM como /entrypoint, lanza el
// servidor MCP como hijo y expone su protocolo por HTTP en :8080, que es donde el
// gateway busca las herramientas. Desde fuera, cualquier servidor stdio parece
// nativo de HTTP.
//
//	gateway ──HTTP──> kling-bridge ──stdin/stdout──> servidor MCP
//
// SESIONES. MCP identifica conversaciones con la cabecera Mcp-Session-Id. Un
// servidor stdio es de sesión única por naturaleza: su estado vive en el proceso.
// Por eso el puente lanza UN PROCESO HIJO POR SESIÓN, y así varias conversaciones
// concurrentes no se pisan el estado.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/panico"
)

// SessionHeader es la cabecera del protocolo MCP que identifica la conversación.
const SessionHeader = "Mcp-Session-Id"

func main() {
	listen := flag.String("listen", ":8080", "where to listen")
	idle := flag.Duration("session-idle", 10*time.Minute, "idle time before closing a session")
	// 0 = derivarlo de la memoria del invitado. El flag sigue existiendo para
	// forzarlo, porque la estimación por sesión es eso, una estimación.
	maxSessions := flag.Int("max-sessions", 0,
		"maximum concurrent sessions (0 = based on microVM memory)")
	// Tope de seguridad para una respuesta, NO el plazo real: quien manda es el
	// contexto de quien llama, que se cancela si el cliente se va. Esto solo
	// existe para que un servidor MCP colgado no retenga la entrada pendiente
	// para siempre cuando el cliente tampoco se desconecta.
	//
	// El defecto son 5 minutos, no 2: tiene que ser MÁS generoso que el plazo
	// del gateway que hay delante, o el puente corta trabajo que el gateway
	// estaba dispuesto a esperar. Un analizador estático sobre un árbol grande
	// pasa de dos minutos sin estar roto en absoluto.
	reqTimeout := flag.Duration("request-timeout", defaultRequestTimeout,
		"safety cap for an MCP server response")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `kling-bridge — exposes a stdio MCP server over HTTP

  kling-bridge [options] -- <MCP server command> [args...]

Examples:
  kling-bridge -- npx -y @modelcontextprotocol/server-filesystem /data
  kling-bridge -- python3 -m my_mcp_server

Options:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	argv := flag.Args()
	if len(argv) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	if *maxSessions <= 0 {
		mem := guestMemMiB()
		if mem <= 0 {
			// Sin poder leer la memoria, el valor histórico. Mejor un tope
			// conocido que ninguno.
			*maxSessions = maxSessionsCap
		} else {
			*maxSessions = deriveMaxSessions(mem)
			log.Printf("session limit: %d (%d MiB memory, ~%d MiB per session)",
				*maxSessions, mem, sessionMiB)
		}
	}

	b := &bridge{
		argv:        argv,
		idle:        *idle,
		maxSessions: *maxSessions,
		reqTimeout:  *reqTimeout,
		sessions:    map[string]*session{},
	}
	// El puente es PID 1 dentro de la microVM. Si muere sin recoger, deja
	// procesos del servidor MCP sueltos que el snapshot siguiente se lleva
	// puestos: por eso atiende a SIGTERM en vez de dejarse matar.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go procReaper.run()
	go b.reapIdle(ctx)

	// Ruta a 169.254.169.254 (MMDS) por eth0. Best-effort: si el servicio no usa
	// secretos por MMDS es inocuo, y si los usa esta es la ruta que los hace
	// legibles. Es la pieza a validar en hardware; ver mmds.go.
	setupMMDSRoute()

	mux := http.NewServeMux()
	// El mismo manejador en / y en /mcp: distintos clientes asumen distinta ruta
	// y no merece la pena que falle un handshake por una barra.
	mux.HandleFunc("/", b.handle)
	mux.HandleFunc("/mcp", b.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	// /reset cierra TODAS las sesiones y mata los procesos hijos. Lo usa el
	// import tras capturar el catálogo, para que el snapshot dorado no se
	// congele con estado de sesión abierto (que es lo que rompe los restores
	// posteriores).
	mux.HandleFunc("/reset", b.handleReset)

	// /exec solo existe si el kernel la enciende. En una microVM de servicio no
	// está registrada siquiera: una capacidad de ejecutar comandos que solo
	// depende de no ser alcanzable es una capacidad que alguien acaba
	// alcanzando, y el gateway reenvía peticiones a los invitados.
	if execEnabled() {
		mux.HandleFunc("/exec", b.handleExec)
		log.Printf("command execution enabled (%s=1): this microVM is single-use",
			execBootParam)
	}

	// El volumen se monta ANTES de servir nada. Si el kernel pidió uno y no se
	// puede montar, es mejor morir aquí —donde se ve en la consola serie— que
	// arrancar el servidor MCP y dejarle escribir en un directorio del overlay
	// que va a desaparecer con la máquina.
	if err := volumeState.acquire(); err != nil {
		log.Fatalf("volume: %v", err)
	}
	// Después de montar, no antes: hay que mirar dentro de los volúmenes para
	// saber cuáles traen paquetes.
	b.env = libraryEnv(os.Environ(), volumeState.specs())
	for _, kv := range b.env {
		if strings.HasPrefix(kv, "NODE_PATH=") || strings.HasPrefix(kv, "PYTHONPATH=") {
			log.Printf("library: %s", kv)
		}
	}

	// Modo navegador compartido: si la imagen dejó el marcador, se guarda para
	// arrancar el Chromium común de forma PEREZOSA (en la primera sesión que lo
	// use, no aquí), y se preparan los args que conectan cada sesión por CDP. Así
	// el snapshot dorado se congela sin navegador dentro. Ver browser.go.
	if spec := loadBrowserSpec(); spec != nil {
		log.Printf("browser: shared mode detected (%s); lazy start", spec.Sidecar[0])
		b.browser = spec
		b.sessionArgs = spec.SessionArgs
	}

	// Modo proxy HTTP: si la imagen declara transport:http, el servidor MCP hijo
	// habla Streamable HTTP nativo. El puente lo arranca UNA vez, espera a su
	// puerto y le reversa las peticiones, en vez de lanzar un proceso por sesión.
	// Se pierde el aislamiento proceso-por-sesión del modo stdio: el hijo es
	// compartido por todas las sesiones (ver proxy.go). A diferencia del
	// navegador, esto NO es perezoso: no hay handshake que traducir, el servidor
	// tiene que estar en pie antes de servir la primera petición.
	if svc := loadServiceSpec(); svc != nil {
		b.service = svc
		log.Printf("http mode: MCP server on :%d (shared by all sessions, no per-session isolation)", svc.Port)
		if err := b.startProxy(); err != nil {
			log.Fatalf("http mode: %v", err)
		}
	}

	// /volume/sync vacía la caché del invitado al disco.
	//
	// El daemon lo llama antes de matar la microVM. Sin esto lo último que
	// escribió la herramienta se queda en la caché de páginas del invitado y
	// muere con él: el volumen "persistente" perdería justo lo más reciente,
	// que es lo que a nadie se le ocurre comprobar.
	mux.HandleFunc("/volume/sync", func(w http.ResponseWriter, r *http.Request) {
		volumeState.sync()
		w.WriteHeader(http.StatusNoContent)
	})

	// /volume/release DESMONTA, y existe por el snapshot dorado.
	//
	// Un snapshot congela la memoria del invitado, y ahí dentro va la caché de
	// ext4: superbloque, mapas de bloques, posición del journal. El fichero del
	// volumen NO se copia al snapshot — sigue siendo el mismo del anfitrión y
	// sigue cambiando. Así que cada instancia restaurada arranca con metadatos
	// de la época del import sobre un disco que ya divergió, y escribe encima.
	//
	// El daemon llama aquí ANTES de congelar la plantilla, para que la memoria
	// que se vuelca no lleve ningún ext4 montado dentro.
	mux.HandleFunc("/volume/release", func(w http.ResponseWriter, r *http.Request) {
		volumeState.release()
		w.WriteHeader(http.StatusNoContent)
	})

	// /volume/acquire vuelve a montar. Lo llama el daemon tras restaurar, cuando
	// los discos ya apuntan a los ficheros de ESTA instancia.
	mux.HandleFunc("/volume/acquire", func(w http.ResponseWriter, r *http.Request) {
		if err := volumeState.acquire(); err != nil {
			// 500 y no un log: si el volumen no se monta, la herramienta
			// escribiría en un directorio del overlay que muere con la máquina,
			// y eso no da ni un error hasta que alguien busca lo que guardó.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// El puente es el PID 1 del invitado: una goroutine y un descriptor
		// retenidos para siempre por un cliente que no termina su cabecera se
		// pagan contra las ~12 sesiones que caben en 1 GiB, no contra un
		// servidor holgado. El daemon y el gateway ya llevaban estos plazos;
		// aqui faltaban.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout va a CERO a proposito, y es la diferencia con los otros
		// dos: aqui al otro lado hay una herramienta MCP ejecutandose. Un escaneo
		// de semgrep sobre un repo grande, o un navegador cargando una pagina
		// lenta, pasan del minuto con toda legitimidad. Un WriteTimeout cortaria
		// esa respuesta a medias y el cliente veria una conexion caida en vez de
		// un resultado — un fallo peor que la espera que pretendia evitar.
	}

	// El apagado se ORDENA aquí y se COMPLETA abajo. Shutdown hace que
	// ListenAndServe retorne de inmediato, así que cerrar las sesiones dentro
	// de esta goroutine sería una carrera contra la salida del proceso: los
	// hijos quedarían tan huérfanos como sin manejar la señal.
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(shutdownDone)
	}()

	log.Printf("kling-bridge listening on %s -> %s", *listen, strings.Join(argv, " "))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}

	<-shutdownDone
	b.closeAll()
	// En modo proxy no hay sesiones-proceso, pero sí el hijo HTTP compartido:
	// somos PID 1 y no debemos dejarlo suelto al morir la microVM.
	b.stopProxyChild()
	// Después de closeAll, no antes: mientras los servidores MCP vivan pueden
	// seguir escribiendo, y desmontar por debajo perdería esas escrituras.
	volumeState.release()
}

// closeAll cierra todas las sesiones y mata sus procesos hijo, incluido el
// caliente sin ligar: somos PID 1 y nada debe quedar suelto al apagar.
func (b *bridge) closeAll() {
	b.mu.Lock()
	all := b.sessions
	b.sessions = map[string]*session{}
	warm := b.warm
	b.warm = nil
	b.mu.Unlock()
	if warm != nil {
		warm.discard()
	}

	// Aquí SÍ se espera, al contrario que en el cosechador de ociosas: después
	// de esto se sueltan los volúmenes y el proceso sale. En paralelo para que
	// una sesión lenta no retrase el apagado de la microVM entera.
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func(s *session) { defer wg.Done(); s.close() }(s)
	}
	wg.Wait()
	if len(all) > 0 {
		log.Printf("kling-bridge: %d session(s) closed on exit", len(all))
	}
}

// defaultRequestTimeout es el tope de seguridad para una respuesta del servidor
// MCP. Más generoso que el plazo del gateway que hay delante, a propósito: si
// fuera más corto, el puente cortaría trabajo que el gateway estaba dispuesto a
// esperar, y el fallo aparecería como un servidor roto en vez de como un límite.
const defaultRequestTimeout = 6 * time.Minute

type bridge struct {
	argv        []string
	idle        time.Duration
	maxSessions int
	reqTimeout  time.Duration
	// env es el entorno con el que se lanza cada servidor MCP, ya con
	// NODE_PATH y PYTHONPATH apuntando a los volúmenes que traen paquetes. Se
	// calcula UNA vez, tras montar: recorrer el disco en cada sesión nueva
	// sería trabajo repetido sobre algo que no cambia.
	env []string

	// sessionArgs se AÑADEN al comando de CADA sesión. Los usa el modo navegador
	// compartido (ver browser.go): apuntan cada sesión al mismo Chromium común
	// por CDP, de modo que N sesiones usan un solo navegador y cada una se lleva
	// su propio contexto y sus páginas, atados a su Mcp-Session-Id.
	sessionArgs []string

	// Modo navegador compartido con arranque perezoso. browser es el marcador
	// (nil si el servicio no es de navegador); bproc es el Chromium común mientras
	// vive; bmu los protege. Ver browser.go.
	browser *browserSpec
	bmu     sync.Mutex
	bproc   *exec.Cmd

	// Modo proxy HTTP (ver proxy.go). service es el marcador (nil = modo stdio);
	// proxy reversa hacia el hijo ÚNICO y compartido; proxyChild es ese hijo y
	// pmu lo protege. proxySeen rastrea la última actividad por Mcp-Session-Id
	// SOLO para el reaper de ociosas: en este modo no hay proceso por sesión.
	service    *serviceSpec
	proxy      *httputil.ReverseProxy
	pmu        sync.Mutex
	proxyChild *exec.Cmd

	mu        sync.Mutex
	sessions  map[string]*session
	proxySeen map[string]time.Time
	// warm es el hijo caliente sin ligar que deja /reset para que el primer
	// initialize tras restaurar el dorado no pague el arranque del runtime
	// (ver warm.go). Protegido por mu. A propósito NO vive en sessions: no
	// cuenta para maxSessions ni puede cosecharlo el reaper de ociosas.
	warm *warmChild
}

// session es una conversación MCP: un proceso hijo y su estado.
type session struct {
	id      string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lastUse time.Time
	// reqTimeout se copia del puente al nacer la sesión en vez de consultarlo
	// allí: call() lo lee sin tomar el mutex del puente, y esa dependencia entre
	// candados es justo la que sale cara de descubrir.
	reqTimeout time.Duration
	// exitCh recibe el estado de salida si el cosechador se adelanta a Wait.
	exitCh chan syscall.WaitStatus

	mu sync.Mutex
	// wmu serializa las escrituras a stdin: el protocolo es una línea por
	// mensaje, y dos Write entrelazados lo corromperían. Va APARTE de mu porque
	// una escritura puede bloquear indefinidamente, y quien la desbloquea
	// —close(), cerrando stdin— no debe necesitar un candado que la escritura
	// tenga cogido. Nunca se toma uno teniendo el otro.
	wmu     sync.Mutex
	pending map[string]chan json.RawMessage // id JSON-RPC -> quien espera
	notes   chan json.RawMessage            // mensajes que el servidor inicia
	closed  bool
	// done se cierra al terminar la sesión. Es la señal para el flujo SSE, y
	// sustituye a cerrar notes: un canal que solo se cierra no puede provocar
	// el pánico de "send on closed channel" en el readLoop.
	done chan struct{}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func (b *bridge) handle(w http.ResponseWriter, r *http.Request) {
	// Modo proxy HTTP: el hijo es único y habla HTTP nativo; se le reversa la
	// petición tal cual, sin parsear JSON-RPC ni lanzar procesos. Ver proxy.go.
	// Las rutas de infraestructura (/healthz, /reset, /volume/*, /exec) tienen su
	// propio manejador en el mux y no pasan por aquí, así que siguen siendo del
	// puente y no del servidor de detrás.
	if b.proxy != nil {
		b.handleProxy(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		b.handlePost(w, r)
	case http.MethodGet:
		b.handleGet(w, r)
	case http.MethodDelete:
		b.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *bridge) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	sid := r.Header.Get(SessionHeader)
	isInit := msg.Method == "initialize"

	// Arranque perezoso del navegador compartido: la primera sesión lo levanta,
	// antes de crear la sesión (que conectará a él por CDP). Va fuera del candado
	// de sesiones para no bloquear todo durante los ~2 s que tarda Chromium.
	if isInit {
		if err := b.ensureBrowser(); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}

	// `initialize` abre sesión; el resto la exige. Es lo que permite al gateway
	// enrutar cada conversación a la misma instancia.
	s, created, err := b.resolve(sid, isInit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if created {
		w.Header().Set(SessionHeader, s.id)
	}

	// Una notificación no lleva id y no espera respuesta: se entrega y se acepta.
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		if err := s.send(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp, err := s.request(r.Context(), string(msg.ID), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// handleGet abre el flujo SSE por el que el servidor envía mensajes propios.
func (b *bridge) handleGet(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get(SessionHeader)
	b.mu.Lock()
	s, ok := b.sessions[sid]
	b.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	// Tener el stream SSE abierto ES actividad. Sin esto, reapIdle mira solo el
	// lastUse de las peticiones POST y cosecha a un cliente que está conectado y
	// a la espera de notificaciones: su stream muere y su proceso con él.
	b.mu.Lock()
	s.lastUse = time.Now()
	b.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set(SessionHeader, s.id)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			// La sesión terminó: se corta el flujo. Antes esto se detectaba
			// porque notes se cerraba, que es justo lo que provocaba el pánico
			// en el readLoop.
			return
		case m := <-s.notes:
			fmt.Fprintf(w, "data: %s\n\n", m)
			flusher.Flush()
		case <-ping.C:
			// Comentario SSE: mantiene viva la conexión sin ensuciar el protocolo.
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (b *bridge) handleDelete(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get(SessionHeader)
	b.mu.Lock()
	s, ok := b.sessions[sid]
	delete(b.sessions, sid)
	b.mu.Unlock()
	if ok {
		s.close()
	}
	// Si era la última sesión, apaga el navegador compartido: en reposo no debe
	// retener ~500 MB.
	b.stopBrowserIfIdle()
	w.WriteHeader(http.StatusNoContent)
}

// handleReset cierra TODAS las sesiones vivas, liberando los procesos hijo
// y dejando al bridge en un estado equivalente al primer arranque: sin
// sesiones, listo para recibir el primer initialize de un cliente nuevo.
//
// Sirve al ciclo de import: tras capturar el catálogo, mcpImport llama a
// /reset antes de hacer commit del snapshot. Sin esto, el snapshot dorado se
// congela con el bridge teniendo sesiones activas (y sus procesos hijo
// hablando por pipes), y al restaurar la microVM queda en un estado donde
// los handshakes posteriores fallan (HTTP 400/406).
func (b *bridge) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Modo proxy: no hay sesiones-proceso que cerrar, pero el snapshot dorado no
	// debe congelarse con el servidor HTTP en estado post-handshake (rechazaría
	// los initialize que lleguen tras restaurar). Reiniciar el hijo compartido lo
	// deja limpio, y de paso se olvida el rastreo de sesiones. Devolver 204 hace
	// que el import tome la vía rápida "bridge" en vez de esperar un auto-reset.
	if b.proxy != nil {
		b.mu.Lock()
		b.proxySeen = map[string]time.Time{}
		b.mu.Unlock()
		if err := b.restartProxyChild(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("reset: HTTP MCP server restarted")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b.mu.Lock()
	dead := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		dead = append(dead, s)
	}
	b.sessions = map[string]*session{}
	b.mu.Unlock()
	for _, s := range dead {
		s.close()
	}
	// El reset deja la microVM como recién arrancada; también apaga el navegador
	// compartido, para que el commit del snapshot dorado se congele sin él.
	b.stopBrowser()
	// Caliente pero sin estado: se deja (o repone) un hijo arrancado y SIN ligar
	// antes de contestar, para que el dorado que se congele tras este 204 lleve
	// el runtime ya arrancado y el primer initialize tras restaurar lo adopte.
	// Si no se puede, el reset sigue siendo válido: solo se pierde la velocidad,
	// nunca la corrección. Ver warm.go.
	//
	// Se puede pedir que NO se precaliente. El hijo caliente vive dentro del
	// dorado, asi que lo engorda: medido, 39 MB -> 120 MB en un servicio de node.
	// A 150 servicios son 12 GB de diferencia, y quien los tenga puede preferir
	// el disco al medio segundo de despertar. Por defecto SI se precalienta,
	// porque el caso comun es tener pocos servicios y usarlos a menudo.
	if r.URL.Query().Get("warm") == "0" {
		log.Printf("reset: prewarm skipped on request (smaller snapshot, slower first wake)")
	} else if err := b.replenishWarm(); err != nil {
		log.Printf("reset: no prewarmed MCP server (the first session will pay the full start): %v", err)
	}
	log.Printf("reset: %d session(s) closed", len(dead))
	w.WriteHeader(http.StatusNoContent)
}

// resolve devuelve la sesión pedida, creándola si el mensaje es un initialize.
func (b *bridge) resolve(sid string, isInit bool) (*session, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if sid != "" {
		if s, ok := b.sessions[sid]; ok {
			s.lastUse = time.Now()
			return s, false, nil
		}
		if !isInit {
			return nil, false, fmt.Errorf("session %q unknown or expired", sid)
		}
	}
	if !isInit {
		return nil, false, fmt.Errorf("missing header %s (send initialize first)", SessionHeader)
	}
	if len(b.sessions) >= b.maxSessions {
		// Al tope. Antes de rechazar, intenta hacer sitio reciclando la sesión más
		// OCIOSA si lleva parada más que una gracia corta. Es lo que permite que un
		// cliente que se RECONECTA (p. ej. opencode reiniciado) entre aunque su
		// sesión anterior siga contada: el reaper de inactividad espera b.idle
		// —minutos—, y sin esto un servicio con tope 1 rechaza toda reconexión
		// durante todo ese rato. Solo se reciclan ociosas: una sesión con su stream
		// SSE abierto se marca activa y nunca es candidata.
		v := b.oldestReclaimableLocked()
		if v == nil {
			return nil, false, fmt.Errorf("session limit of %d reached", b.maxSessions)
		}
		delete(b.sessions, v.id)
		go v.close()
		log.Printf("session %s reclaimed (idle %s) to make room for a new one",
			v.id, time.Since(v.lastUse).Round(time.Second))
	}

	s, err := b.spawn()
	if err != nil {
		return nil, false, err
	}
	b.sessions[s.id] = s
	return s, true, nil
}

// sessionReclaimGrace: cuánto debe llevar OCIOSA una sesión para poder
// reciclarla y hacer sitio a una nueva cuando se alcanza el tope. Corto —una
// reconexión no debe esperar el idle completo, que son minutos— pero no cero:
// entre dos mensajes de una conversación viva pasan segundos, y no queremos
// matar una que solo está pensando entre herramientas.
const sessionReclaimGrace = 45 * time.Second

// oldestReclaimableLocked devuelve la sesión ociosa más antigua que ya supera la
// gracia de reciclado, o nil si ninguna cumple. Se llama con b.mu tomado.
func (b *bridge) oldestReclaimableLocked() *session {
	var oldest *session
	for _, s := range b.sessions {
		if time.Since(s.lastUse) < sessionReclaimGrace {
			continue
		}
		if oldest == nil || s.lastUse.Before(oldest.lastUse) {
			oldest = s
		}
	}
	return oldest
}

// spawn lanza un proceso del servidor MCP para una sesión nueva.
func (b *bridge) spawn() (*session, error) {
	// argv del servidor + los sessionArgs del modo navegador (vacío en el resto
	// de servicios): apuntan esta sesión al Chromium compartido por CDP.
	// El id se genera ANTES del comando: sessionEnv lo necesita para buscar los
	// secretos de MMDS de ESTA sesión (sessions[<id>]) y añadirlos a su entorno.
	id := newID()

	// Entorno base del puente MÁS los secretos de sesión que MMDS traiga (comunes
	// + los de esta sesión). Sin MMDS, es exactamente b.env.
	env, secrets := b.sessionEnv(id)

	// Si /reset dejó un hijo caliente y esta sesión puede adoptarlo —vivo y sin
	// secretos que él no tenga—, se ahorra el arranque del runtime, que es lo
	// que anulaba la ventaja del snapshot dorado. Ver warm.go.
	if s := b.adoptWarmLocked(id, secrets); s != nil {
		return s, nil
	}

	args := append(append([]string(nil), b.argv[1:]...), b.sessionArgs...)
	cmd := exec.Command(b.argv[0], args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // el log del servidor va a la consola serie de la microVM

	// Registrado en el cosechador: el puente es PID 1 y recoge huérfanos con
	// wait4(-1), que no distingue. Esto le permite devolvernos el estado si se
	// adelanta a nuestro Wait.
	exitCh, err := procReaper.startTracked(cmd)
	if err != nil {
		return nil, fmt.Errorf("could not start MCP server: %w", err)
	}

	s := &session{
		id: id, cmd: cmd, stdin: stdin, lastUse: time.Now(),
		reqTimeout: b.reqTimeout,
		exitCh:     exitCh,
		pending:    map[string]chan json.RawMessage{},
		notes:      make(chan json.RawMessage, 32),
		done:       make(chan struct{}),
	}
	go s.readLoop(stdout)
	log.Printf("session %s: MCP server started (pid %d)", s.id[:8], cmd.Process.Pid)
	return s, nil
}

// ── sesión ────────────────────────────────────────────────────────────────────

// readLoop lee la salida del servidor y reparte cada mensaje.
//
// stdio en MCP es JSON delimitado por saltos de línea: un mensaje por línea.
func (s *session) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20) // respuestas grandes son normales

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		raw := json.RawMessage(append([]byte(nil), line...))

		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(raw, &probe)

		if len(probe.ID) == 0 || string(probe.ID) == "null" {
			// Sin id: el servidor habla por iniciativa propia. Va al flujo SSE, y
			// si nadie escucha se descarta antes que bloquear al servidor.
			select {
			case s.notes <- raw:
			default:
			}
			continue
		}

		s.mu.Lock()
		ch, ok := s.pending[string(probe.ID)]
		delete(s.pending, string(probe.ID))
		s.mu.Unlock()
		if ok {
			ch <- raw
		}
	}
	s.close()
}

func (s *session) send(msg []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("session is closed")
	}

	// La escritura va bajo wmu, NO bajo mu.
	//
	// Escribir a un pipe puede bloquear para siempre: si el servidor MCP deja
	// de leer su stdin, a los ~64 KiB encolados el Write se para. Hacerlo con el
	// candado de estado tomado colgaba la sesión entera —y con ella el
	// cosechador de ociosas, que las cierra en serie, y el apagado, que como
	// somos PID 1 impide que la microVM muera limpia—, porque close(), que es
	// justo quien desatasca cerrando stdin, esperaba ese mismo candado.
	//
	// La carrera que esto abre es benigna: si close() cierra stdin entre el
	// chequeo y el Write, el Write devuelve ErrClosed y el cliente ve un error,
	// que es exactamente lo que debe ver.
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.stdin.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("writing to MCP server: %w", err)
	}
	return nil
}

// request envía un mensaje y espera la respuesta con el mismo id JSON-RPC.
func (s *session) request(ctx context.Context, id string, msg []byte) (json.RawMessage, error) {
	ch := make(chan json.RawMessage, 1)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("session is closed")
	}
	// Un id ya en vuelo se RECHAZA, no se pisa.
	//
	// Antes se sobrescribía la entrada: el primero que esperaba quedaba
	// huérfano —nadie volvería a escribir en su canal— y se colgaba hasta el
	// tope de respuesta, minutos después. Y peor: la primera respuesta que
	// llegara con ese id se entregaba al SEGUNDO, que recibía la contestación
	// de una petición que no era la suya, y las siguientes se descartaban por
	// no encontrar a nadie esperando.
	//
	// JSON-RPC exige que los id en vuelo sean únicos; el que los repite tiene
	// un fallo, y decírselo con un error inmediato es infinitamente mejor que
	// colgarlo y contestarle otra cosa.
	if _, repetido := s.pending[id]; repetido {
		s.mu.Unlock()
		return nil, fmt.Errorf("a request with id %s is already in flight: "+
			"JSON-RPC requires unique ids among unanswered requests", id)
	}
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.send(msg); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	// Un cero significa "sin configurar", no "vence ya". Sin esto, cualquier
	// sesión construida sin el campo —un test, un refactor futuro— haría que
	// time.After(0) disparase en el acto y toda llamada fallara al instante.
	timeout := s.reqTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	select {
	case resp, ok := <-ch:
		// Recepción de DOS valores, no de uno. close() cierra los canales
		// pendientes cuando muere el servidor MCP, y un canal cerrado entrega
		// el valor cero sin bloquear: con `resp := <-ch` esto devolvía
		// (nil, nil) y el cliente recibía una respuesta vacía en lugar de un
		// error, que es la forma más cara de diagnosticar un proceso muerto.
		if !ok {
			return nil, fmt.Errorf("MCP server died while handling message %s", id)
		}
		return resp, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-time.After(timeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		// Se dice que SIGUE trabajando, no que esté roto: el proceso no se mata
		// aquí, y confundir "tarda mucho" con "muerto" manda a quien depura al
		// sitio equivocado. Ya pasó con un escaneo que topaba contra este plazo
		// exacto y parecía un servidor caído.
		return nil, fmt.Errorf("MCP server did not respond to message %s within %s; "+
			"it's still working, this is the bridge's cap. Raise it with -request-timeout",
			id, timeout)
	}
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pending := s.pending
	s.pending = map[string]chan json.RawMessage{}
	s.mu.Unlock()

	// Nadie debe quedarse esperando una respuesta que ya no va a llegar.
	for _, ch := range pending {
		close(ch)
	}

	// s.notes NO se cierra: el readLoop sigue vivo hasta que el hijo suelte su
	// stdout, y enviar por un canal cerrado entra en pánico incluso dentro de
	// un select con default. Bastaba con que el servidor MCP emitiera una
	// notificación en esta ventana para tumbar el puente entero. La señal de
	// fin va por s.done, que solo se cierra, nunca se escribe.
	close(s.done)

	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	// waitFor y no Wait a secas: el cosechador puede haberse llevado a este hijo
	// con wait4(-1), y entonces Wait devuelve ECHILD sin código de salida. El
	// estado real llega por el canal, y sin eso se perdería justo la
	// distinción que el log de abajo necesita.
	err := waitFor(s.cmd, s.exitCh)
	if s.cmd.Process != nil {
		procReaper.forget(s.cmd.Process.Pid)
	}

	// El motivo importa: la consola serie es la única ventana al interior de la
	// microVM, y un hijo al que se llevó el oom-killer sale por señal. Esa
	// distinción es la diferencia entre "se cerró la sesión", que no explica
	// nada, y saber que el invitado se quedó sin memoria.
	switch {
	case err == nil:
		log.Printf("session %s: closed", s.id[:8])
	case len(pending) > 0:
		log.Printf("session %s: MCP server exited (%v) with %d request(s) in flight",
			s.id[:8], err, len(pending))
	default:
		log.Printf("session %s: closed, MCP server exited with %v", s.id[:8], err)
	}
}

// reapIdle cierra las sesiones abandonadas. Cada una es un proceso vivo dentro de
// la microVM: sin esto, un cliente que se va sin despedirse las acumula.
func (b *bridge) reapIdle(ctx context.Context) {
	t := time.NewTicker(b.idle / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// El puente es el PID 1 del invitado: si muere, la microVM entra en
			// panico del kernel. Contener aqui es lo que separa "una cosecha
			// salio mal" de "el invitado se cayo".
			panico.Contener("bridge.reapIdle", func() {
				var dead []*session
				b.mu.Lock()
				for id, s := range b.sessions {
					if time.Since(s.lastUse) > b.idle {
						dead = append(dead, s)
						delete(b.sessions, id)
					}
				}
				b.mu.Unlock()
				// En paralelo: cerrarlas en serie hacía que una sesión lenta
				// retuviera la cosecha de todas las demás. close() es idempotente.
				for _, s := range dead {
					go s.close()
				}
				// Si el segador se llevó la última sesión, apaga el navegador
				// compartido: una microVM viva pero ociosa no debe retenerlo.
				if len(dead) > 0 {
					b.stopBrowserIfIdle()
				}
				// Modo proxy: no hay sesiones-proceso en b.sessions, pero sí un
				// rastro de Mcp-Session-Id que limpiar cerrándolos en el servidor
				// compartido. Ver reapIdleProxy.
				if b.proxy != nil {
					b.reapIdleProxy()
				}
			})
		}
	}
}
