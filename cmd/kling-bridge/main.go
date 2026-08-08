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
//	   gateway ──HTTP──> kling-bridge ──stdin/stdout──> servidor MCP
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
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SessionHeader es la cabecera del protocolo MCP que identifica la conversación.
const SessionHeader = "Mcp-Session-Id"

func main() {
	listen := flag.String("listen", ":8080", "dónde escuchar")
	idle := flag.Duration("session-idle", 10*time.Minute, "inactividad antes de cerrar una sesión")
	maxSessions := flag.Int("max-sessions", 32, "sesiones concurrentes como máximo")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `kling-bridge — expone un servidor MCP de stdio por HTTP

  kling-bridge [opciones] -- <comando del servidor MCP> [args...]

Ejemplos:
  kling-bridge -- npx -y @modelcontextprotocol/server-filesystem /data
  kling-bridge -- python3 -m mi_servidor_mcp

Opciones:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	argv := flag.Args()
	if len(argv) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	b := &bridge{
		argv:        argv,
		idle:        *idle,
		maxSessions: *maxSessions,
		sessions:    map[string]*session{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.reapIdle(ctx)

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

	log.Printf("kling-bridge escuchando en %s -> %s", *listen, strings.Join(argv, " "))
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

type bridge struct {
	argv        []string
	idle        time.Duration
	maxSessions int

	mu       sync.Mutex
	sessions map[string]*session
}

// session es una conversación MCP: un proceso hijo y su estado.
type session struct {
	id      string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lastUse time.Time

	mu      sync.Mutex
	pending map[string]chan json.RawMessage // id JSON-RPC -> quien espera
	notes   chan json.RawMessage            // mensajes que el servidor inicia
	closed  bool
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func (b *bridge) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		b.handlePost(w, r)
	case http.MethodGet:
		b.handleGet(w, r)
	case http.MethodDelete:
		b.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
	}
}

func (b *bridge) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "no pude leer el cuerpo", http.StatusBadRequest)
		return
	}

	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "JSON inválido: "+err.Error(), http.StatusBadRequest)
		return
	}

	sid := r.Header.Get(SessionHeader)
	isInit := msg.Method == "initialize"

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
		http.Error(w, "sesión desconocida", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming no soportado", http.StatusInternalServerError)
		return
	}

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
		case m, ok := <-s.notes:
			if !ok {
				return
			}
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
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
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
	log.Printf("reset: %d sesión(es) cerrada(s)", len(dead))
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
			return nil, false, fmt.Errorf("sesión %q desconocida o expirada", sid)
		}
	}
	if !isInit {
		return nil, false, fmt.Errorf("falta la cabecera %s (envía initialize primero)", SessionHeader)
	}
	if len(b.sessions) >= b.maxSessions {
		return nil, false, fmt.Errorf("límite de %d sesiones alcanzado", b.maxSessions)
	}

	s, err := b.spawn()
	if err != nil {
		return nil, false, err
	}
	b.sessions[s.id] = s
	return s, true, nil
}

// spawn lanza un proceso del servidor MCP para una sesión nueva.
func (b *bridge) spawn() (*session, error) {
	cmd := exec.Command(b.argv[0], b.argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // el log del servidor va a la consola serie de la microVM

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("no pude lanzar el servidor MCP: %w", err)
	}

	s := &session{
		id: newID(), cmd: cmd, stdin: stdin, lastUse: time.Now(),
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 32),
	}
	go s.readLoop(stdout)
	log.Printf("sesión %s: servidor MCP arrancado (pid %d)", s.id[:8], cmd.Process.Pid)
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
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("la sesión está cerrada")
	}
	if _, err := s.stdin.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("escribiendo al servidor MCP: %w", err)
	}
	return nil
}

// request envía un mensaje y espera la respuesta con el mismo id JSON-RPC.
func (s *session) request(ctx context.Context, id string, msg []byte) (json.RawMessage, error) {
	ch := make(chan json.RawMessage, 1)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("la sesión está cerrada")
	}
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.send(msg); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-time.After(120 * time.Second):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("el servidor MCP no respondió al mensaje %s", id)
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
	close(s.notes)
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	log.Printf("sesión %s: cerrada", s.id[:8])
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
			var dead []*session
			b.mu.Lock()
			for id, s := range b.sessions {
				if time.Since(s.lastUse) > b.idle {
					dead = append(dead, s)
					delete(b.sessions, id)
				}
			}
			b.mu.Unlock()
			for _, s := range dead {
				s.close()
			}
		}
	}
}
