package aigw

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/internal/von"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// Replica es una réplica VON reservada para una petición: su dirección y cómo
// devolverla. Release marca el fin de la petición (el segador vuelve a contar
// su inactividad desde ahí); Drop la olvida porque no respondía.
type Replica struct {
	Addr    string
	Release func()
	Drop    func()
	// Wake es el desglose del despertar que la dejó lista si esta es la
	// primera petición que la usa desde entonces; nil si ya estaba despierta.
	Wake *scheduler.WakeTrace
}

// Replicas reparte réplicas de un dorado VON. La de verdad es pkg/scheduler;
// los tests ponen una de mentira que apunta a un llama-server falso.
type Replicas interface {
	Acquire(ctx context.Context, snapshot string) (*Replica, error)
}

// replicasConPuerto es Replicas que sabe dar la dirección de otro puerto que
// el de VON: un backend HTTP escucha donde diga su bloque ("port"). Una
// Replicas que no lo implementa da siempre la misma dirección (las de los
// tests).
type replicasConPuerto interface {
	AcquirePort(ctx context.Context, snapshot string, port int) (*Replica, error)
}

// schedReplicas es Replicas sobre pkg/scheduler: la primera petición despierta
// (thaw) o restaura una réplica, las concurrentes provocan réplicas nuevas
// hasta el tope del modelo, y el segador congela lo ocioso.
type schedReplicas struct{ s *scheduler.Scheduler }

func (r schedReplicas) Acquire(ctx context.Context, snap string) (*Replica, error) {
	return r.AcquirePort(ctx, snap, von.Port)
}

func (r schedReplicas) AcquirePort(ctx context.Context, snap string, port int) (*Replica, error) {
	r.s.Observe(snap) // guía el keepwarm por popularidad
	e, err := r.s.PickInstance(ctx, snap, scheduler.TenantFrom(ctx))
	if err != nil {
		return nil, err
	}
	r.s.Begin(e)
	id := e.MachineID()
	return &Replica{
		Addr:    e.Addr(port),
		Release: func() { r.s.End(e) },
		Drop:    func() { r.s.DropInstance(snap, id) },
		Wake:    e.TakeWake(),
	}, nil
}

// adquirir pide una réplica de snap con su dirección en port.
func (g *Gateway) adquirir(ctx context.Context, snap string, port int) (*Replica, error) {
	if rp, ok := g.replicas.(replicasConPuerto); ok {
		return rp.AcquirePort(ctx, snap, port)
	}
	return g.replicas.Acquire(ctx, snap)
}

// isDialError dice si err es no haber podido conectar: lo único que se
// reintenta, porque la petición no llegó a salir y repetirla no duplica nada.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// seed es una semilla nueva del host por petición. Las réplicas nacen del mismo
// volcado de memoria; aunque llama-server ya pide una nueva por petición (ver
// docs/von.md), el gateway no depende de lo que haga el invitado.
func seed() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1)
}

// postGuest manda body (JSON) a path de una réplica del dorado snap, en el
// puerto de su backend. Es aReplica con un cuerpo en memoria, que siempre se
// puede repetir.
func (g *Gateway) postGuest(ctx context.Context, snap, path string, body []byte) (*http.Response, *Replica, error) {
	s := &salida{method: http.MethodPost, path: path, bytes: body, hdr: http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json, text/event-stream"},
	}}
	return g.aReplica(ctx, g.nombreModelo(snap), snap, g.catalogo().puertoDe(snap), s)
}

// aReplica manda s a una réplica del dorado snap (name es el modelo, para las
// métricas), por las conexiones reutilizables de g.guests (guestpool.go). Dos
// fallos se repiten, cada uno una vez y solo si el cuerpo no llegó a salir: no
// haber podido conectar (olvida esa réplica y prueba con otra: el daemon la
// congeló o la retiró por debajo) y una conexión reutilizada que muere sin
// respuesta (la dejó muerta un congelar/despertar; se repite por una conexión
// nueva). En ninguno de los dos la réplica llegó a contestar.
func (g *Gateway) aReplica(ctx context.Context, name, snap string, port int, s *salida) (*http.Response, *Replica, error) {
	var dialRetried, staleRetried bool
	for {
		rep, err := g.adquirir(ctx, snap, port)
		if err != nil {
			return nil, nil, &wakeError{err}
		}
		req, err := s.request(ctx, "http://"+rep.Addr)
		if err != nil {
			rep.Release()
			return nil, nil, err
		}
		if rep.Wake != nil {
			// Se acaba de despertar: lo que hubiera en el pool hacia esta
			// dirección es de antes de dormirse (ver guestpool.go).
			g.guests.closeIdle(rep.Addr)
		}
		t0 := time.Now()
		resp, reused, err := g.guests.do(rep.Addr, g.cabecerasDe(snap), req)
		if err == nil {
			if rep.Wake != nil {
				g.primeraPeticion(snap, rep.Wake, time.Since(t0))
			}
			return resp, rep, nil
		}
		rep.Release()
		if staleConn(ctx, reused, err) && !staleRetried && s.reintentable() {
			staleRetried = true
			g.met.vonErr(name, "stale")
			g.guests.closeIdle(rep.Addr)
			continue
		}
		if isDialError(err) && !dialRetried && s.reintentable() {
			dialRetried = true
			g.met.vonErr(name, "dial")
			g.guests.forget(rep.Addr)
			rep.Drop()
			continue
		}
		return nil, nil, err
	}
}

// cabecerasDe es el tope de espera a las cabeceras de una réplica del dorado
// snap: el limits.timeout de su backend (un chat sin stream manda las
// cabeceras al acabar), o 0 para el del pool (-proxy-timeout).
func (g *Gateway) cabecerasDe(snap string) time.Duration {
	if b := g.backendDe(snap); b != nil && b.Limits.Timeout != "" {
		if d, err := time.ParseDuration(b.Limits.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// wakeError es no haber conseguido réplica (no cabe, no hay dorado...): el
// cliente recibe 503 con Retry-After, no un 502 de "el modelo falló".
type wakeError struct{ err error }

func (e *wakeError) Error() string { return "no replica available: " + e.err.Error() }
func (e *wakeError) Unwrap() error { return e.err }

// chatReq es lo que el gateway pregunta a VON: una escalada o una generación.
type chatReq struct {
	Messages    []von.Message `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Seed        int64         `json:"seed"`
	Grammar     string        `json:"grammar,omitempty"`
	// JSONSchema es la extensión de llama-server que convierte un esquema en
	// una gramática: la salida solo puede ser JSON que lo cumple.
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
	Stream     bool            `json:"stream"`
}

// maxChatAnswerBytes acota la respuesta (sin streaming) de una escalada o una
// generación: 4096 tokens caben de sobra, y un invitado hostil no puede
// hinchar la memoria.
const maxChatAnswerBytes = 1 << 20

// askVON pregunta a una réplica y devuelve el texto de la respuesta.
func (g *Gateway) askVON(ctx context.Context, snap string, req chatReq) (string, error) {
	out, err := g.chatVON(ctx, snap, req)
	if err != nil {
		return "", err
	}
	return out.Text(), nil
}

// chatVON manda una petición de chat (sin streaming) a una réplica del dorado
// snap y devuelve la respuesta entera, acotada.
func (g *Gateway) chatVON(ctx context.Context, snap string, req chatReq) (*von.ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, rep, err := g.postGuest(ctx, snap, "/v1/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer rep.Release()
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxChatAnswerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxChatAnswerBytes {
		return nil, fmt.Errorf("replica answer larger than %d bytes", maxChatAnswerBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("replica answered %d: %q", resp.StatusCode, truncUTF8(string(b), 200))
	}
	var out von.ChatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("replica answer is not a chat completion: %w", err)
	}
	return &out, nil
}

// nombreModelo es el modelo del registro que sirve el dorado snap, que es con
// lo que se etiquetan las métricas (R8-11: el dorado abría otra serie para lo
// mismo); el dorado si ningún modelo lo nombra.
func (g *Gateway) nombreModelo(snap string) string {
	if name, _ := g.config().replicaModel(snap); name != "" {
		return name
	}
	return snap
}

// primeraPeticion cierra el desglose de un despertar con la petición que lo
// pagó: la registra en las métricas y deja una línea de log con todas las
// fases, que es de donde sale la tabla de docs/despertar.md.
func (g *Gateway) primeraPeticion(snap string, t *scheduler.WakeTrace, first time.Duration) {
	name := g.nombreModelo(snap)
	g.met.wakePhases(name, t, first)
	log.Printf("%s: replica ready (%s), first request %.1f ms, total %.1f ms", name, t,
		float64(first.Microseconds())/1000, float64((t.Total+first).Microseconds())/1000)
}
