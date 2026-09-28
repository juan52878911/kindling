package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionHeader es la cabecera de sesión de MCP (Streamable HTTP).
const sessionHeader = "Mcp-Session-Id"

// maxBody acota lo que se lee de cada respuesta. El eco devuelve unos bytes;
// más de 1 MiB es que algo va mal, y no se guarda entero.
const maxBody = 1 << 20

// mcpClient es un cliente MCP mínimo contra el gateway: POST JSON-RPC y
// DELETE de sesión. Acepta respuesta JSON o SSE, como pide la especificación.
type mcpClient struct {
	http    *http.Client
	gateway string // sin barra final
	token   string
	timeout time.Duration
}

// stageError es un fallo en una fase concreta de la sesión, con un código
// corto y estable para agrupar (http_503, timeout, rpc_-32601...).
type stageError struct {
	Stage  string
	Code   string
	Detail string
}

func (e *stageError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Stage, e.Code, e.Detail)
}

// classify da el código corto de un error de transporte.
func classify(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case strings.Contains(err.Error(), "connection refused"):
		return "conn_refused"
	case strings.Contains(err.Error(), "connection reset"), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "conn_reset"
	default:
		return "transport"
	}
}

// post manda un mensaje JSON-RPC. Devuelve el código HTTP, el id de sesión que
// venga en la respuesta y el cuerpo JSON (ya sacado del SSE si venía así).
func (c *mcpClient) post(ctx context.Context, service, sid string, msg []byte) (int, string, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.gateway+"/mcp/"+service, bytes.NewReader(msg))
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if sid != "" {
		req.Header.Set(sessionHeader, sid)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, "", nil, err
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		body = lastSSEData(body)
	}
	return resp.StatusCode, resp.Header.Get(sessionHeader), body, nil
}

// lastSSEData saca el último `data:` de un cuerpo SSE: la respuesta a la
// petición llega la última (antes pueden venir notificaciones).
func lastSSEData(b []byte) []byte {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxBody)
	for sc.Scan() {
		if d, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:")); ok {
			last = append(last[:0], bytes.TrimSpace(d)...)
		}
	}
	return last
}

func (c *mcpClient) delete(ctx context.Context, service, sid string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.gateway+"/mcp/"+service, nil)
	if err != nil {
		return 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set(sessionHeader, sid)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// rpcCheck mira una respuesta JSON-RPC: error del protocolo, o resultado de
// herramienta con isError. Un cuerpo que no es JSON-RPC también es un fallo.
func rpcCheck(body []byte) (code, detail string) {
	var r struct {
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "bad_json", trunc(string(body))
	}
	if r.Error != nil {
		return fmt.Sprintf("rpc_%d", r.Error.Code), trunc(r.Error.Message)
	}
	if r.Result == nil {
		return "no_result", trunc(string(body))
	}
	if r.Result.IsError {
		return "tool_error", trunc(string(body))
	}
	return "", ""
}

func trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// sessionResult es lo medido en una sesión. Todas las duraciones en ms.
type sessionResult struct {
	Service string
	Err     *stageError
	// InitMS: initialize (incluye colocar la sesión y, si hace falta,
	// descongelar o restaurar la microVM). FirstCallMS: el primer tools/call.
	// TTFRMS: del initialize al primer resultado de herramienta, lo que ve un
	// agente que abre una sesión en frío.
	InitMS, FirstCallMS, TTFRMS float64
	Steady                      []float64
	DeleteMS                    float64
}

// callSpec es la llamada que se repite.
type callSpec struct {
	Tool string
	Args json.RawMessage
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// runSession hace una sesión completa: initialize, notifications/initialized,
// tools/call, `steady` llamadas más y DELETE. El primer fallo la termina.
//
// El resultado va con nombre: el DELETE diferido tiene que poder anotar su
// fallo en lo que se devuelve.
func (c *mcpClient) runSession(ctx context.Context, service string, call callSpec, steady int) (res sessionResult) {
	res.Service = service
	fail := func(stage, code, detail string) sessionResult {
		res.Err = &stageError{Stage: stage, Code: code, Detail: detail}
		return res
	}

	t0 := time.Now()
	init := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling-mcpbench","version":"1"}}}`)
	st, sid, body, err := c.post(ctx, service, "", init)
	if err != nil {
		return fail("init", classify(err), err.Error())
	}
	if st != http.StatusOK {
		return fail("init", fmt.Sprintf("http_%d", st), trunc(string(body)))
	}
	if code, d := rpcCheck(body); code != "" {
		return fail("init", code, d)
	}
	if sid == "" {
		return fail("init", "no_session", "no "+sessionHeader+" header")
	}
	res.InitMS = ms(time.Since(t0))

	// La sesión ya existe: pase lo que pase a partir de aquí, se cierra, para
	// no dejar procesos en el puente que falseen la celda siguiente.
	defer func() {
		t := time.Now()
		st, err := c.delete(context.WithoutCancel(ctx), service, sid)
		res.DeleteMS = ms(time.Since(t))
		if res.Err != nil {
			return
		}
		if err != nil {
			res.Err = &stageError{Stage: "delete", Code: classify(err), Detail: err.Error()}
		} else if st >= 300 {
			res.Err = &stageError{Stage: "delete", Code: fmt.Sprintf("http_%d", st)}
		}
	}()

	st, _, body, err = c.post(ctx, service, sid, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		res.Err = &stageError{Stage: "initialized", Code: classify(err), Detail: err.Error()}
		return res
	}
	if st != http.StatusAccepted && st != http.StatusOK {
		res.Err = &stageError{Stage: "initialized", Code: fmt.Sprintf("http_%d", st), Detail: trunc(string(body))}
		return res
	}

	for i := 0; i <= steady; i++ {
		msg := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
			i+2, call.Tool, call.Args)
		stage := "call"
		if i > 0 {
			stage = "steady"
		}
		t := time.Now()
		st, _, body, err := c.post(ctx, service, sid, msg)
		d := ms(time.Since(t))
		if err != nil {
			res.Err = &stageError{Stage: stage, Code: classify(err), Detail: err.Error()}
			return res
		}
		if st != http.StatusOK {
			res.Err = &stageError{Stage: stage, Code: fmt.Sprintf("http_%d", st), Detail: trunc(string(body))}
			return res
		}
		if code, det := rpcCheck(body); code != "" {
			res.Err = &stageError{Stage: stage, Code: code, Detail: det}
			return res
		}
		if i == 0 {
			res.FirstCallMS = d
			res.TTFRMS = ms(time.Since(t0))
		} else {
			res.Steady = append(res.Steady, d)
		}
	}
	return res
}

// runLoad lanza `sessions` sesiones a la vez, repartidas en turno rotatorio
// entre los servicios, tras una barrera: ninguna empieza hasta que todas las
// goroutines están listas, para que la ráfaga sea una ráfaga y no una rampa
// del ritmo al que Go crea goroutines.
func runLoad(ctx context.Context, c *mcpClient, services []string, sessions int, call callSpec, steady int) []sessionResult {
	out := make([]sessionResult, sessions)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i := range sessions {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			svc := services[i%len(services)]
			ready.Done()
			<-start
			out[i] = c.runSession(ctx, svc, call, steady)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return out
}
