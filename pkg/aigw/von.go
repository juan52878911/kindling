package aigw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/pkg/scheduler"
	"github.com/juan52878911/kindling/pkg/von"
)

// Replica es una réplica VON reservada para una petición: su dirección y cómo
// devolverla. Release marca el fin de la petición (el segador vuelve a contar
// su inactividad desde ahí); Drop la olvida porque no respondía.
type Replica struct {
	Addr    string
	Release func()
	Drop    func()
}

// Replicas reparte réplicas de un dorado VON. La de verdad es pkg/scheduler;
// los tests ponen una de mentira que apunta a un llama-server falso.
type Replicas interface {
	Acquire(ctx context.Context, snapshot string) (*Replica, error)
}

// schedReplicas es Replicas sobre pkg/scheduler: la primera petición despierta
// (thaw) o restaura una réplica, las concurrentes provocan réplicas nuevas
// hasta el tope del modelo, y el segador congela lo ocioso.
type schedReplicas struct{ s *scheduler.Scheduler }

func (r schedReplicas) Acquire(ctx context.Context, snap string) (*Replica, error) {
	r.s.Observe(snap) // guía el keepwarm por popularidad
	e, err := r.s.PickInstance(ctx, snap, scheduler.TenantFrom(ctx))
	if err != nil {
		return nil, err
	}
	r.s.Begin(e)
	id := e.MachineID()
	return &Replica{
		Addr:    e.Addr(von.Port),
		Release: func() { r.s.End(e) },
		Drop:    func() { r.s.DropInstance(snap, id) },
	}, nil
}

// guestClient habla con las réplicas. El invitado no es de fiar: conectar
// tiene plazo corto, las cabeceras tienen tope, y el cuerpo lo acota quien lee.
// Sin keep-alive: una conexión ociosa hacia una réplica que el segador congela
// y otra petición descongela es una conexión muerta que el transporte
// reutilizaría; abrir una nueva hacia el host local cuesta microsegundos.
var guestClient = &http.Client{Transport: &http.Transport{
	DialContext:            (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	MaxResponseHeaderBytes: 64 << 10,
	DisableCompression:     true,
	DisableKeepAlives:      true,
}}

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

// postGuest manda body a path de una réplica del dorado snap. Si no se pudo
// conectar, olvida esa réplica y lo intenta una vez más con otra: es lo que
// pasa cuando el daemon congeló o retiró la máquina por debajo.
func (g *Gateway) postGuest(ctx context.Context, snap, path string, body []byte) (*http.Response, *Replica, error) {
	for intento := 0; ; intento++ {
		rep, err := g.replicas.Acquire(ctx, snap)
		if err != nil {
			return nil, nil, &wakeError{err}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+rep.Addr+path, bytes.NewReader(body))
		if err != nil {
			rep.Release()
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := guestClient.Do(req)
		if err == nil {
			return resp, rep, nil
		}
		rep.Release()
		if isDialError(err) && intento == 0 {
			g.met.vonErr(snap, "dial")
			rep.Drop()
			continue
		}
		return nil, nil, err
	}
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
	Stream      bool          `json:"stream"`
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
		return nil, fmt.Errorf("replica answered %d: %s", resp.StatusCode, truncUTF8(string(b), 200))
	}
	var out von.ChatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("replica answer is not a chat completion: %w", err)
	}
	return &out, nil
}
