package domotica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LAS CAPAS A TRAVÉS DEL GATEWAY DE IA.
//
// En producción las capas no viven en el proceso que las usa: `kling ai
// serve` sirve las 1–3 en POST /v1/decide (una tarea «domotica»: plantillas y
// modelo rápido en su proceso, el codificador en una réplica que despierta y
// congela) y la 4 en POST /v1/generate (una tarea de generación con
// LLMSystemPrompt y LLMSchema). Estos adaptadores convierten esas llamadas en
// una FastFunc y un Generator para la Cascade. La validación de la respuesta
// del LLM se hace aquí, en quien la va a ejecutar, no en el gateway: el
// gateway no sabe qué dispositivos hay.

// GatewayClient habla con el gateway de IA (kindling-domotica gateway).
type GatewayClient struct {
	// Base es la URL del gateway («http://127.0.0.1:18080»; con un socket
	// Unix, cualquier host y un Client que marque ese socket).
	Base   string
	Client *http.Client
	// Token va como Bearer si no está vacío.
	Token string
}

// Tope de lo que se lee de una respuesta del gateway.
const maxGatewayBody = 1 << 20

// GatewayError es una respuesta no 2xx del gateway.
type GatewayError struct {
	Code int
	Msg  string
}

func (e *GatewayError) Error() string { return fmt.Sprintf("gateway answered %d: %s", e.Code, e.Msg) }

// Do manda una petición JSON y decodifica la respuesta en out (si no es nil).
func (c *GatewayClient) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	cl := c.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayBody+1))
	if err != nil {
		return err
	}
	if len(b) > maxGatewayBody {
		return fmt.Errorf("gateway answer over %d bytes", maxGatewayBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return &GatewayError{Code: resp.StatusCode, Msg: clip(msg, 300)}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*[]byte); ok {
		*s = b
		return nil
	}
	return json.Unmarshal(b, out)
}

// Fast devuelve las capas 1–3 de una tarea «domotica» del gateway.
func (c *GatewayClient) Fast(task string) FastFunc {
	return func(ctx context.Context, text, lang string) (Decision, error) {
		var d Decision
		err := c.Do(ctx, http.MethodPost, "/v1/decide", map[string]string{"task": task, "text": text, "lang": lang}, &d)
		return d, err
	}
}

// Generator devuelve la capa 4 sobre una tarea de generación del gateway.
func (c *GatewayClient) Generator(task string) Generator {
	return func(ctx context.Context, input string) (string, string, error) {
		var r struct {
			Model  string `json:"model"`
			Output string `json:"output"`
		}
		err := c.Do(ctx, http.MethodPost, "/v1/generate", map[string]string{"task": task, "input": input}, &r)
		var ge *GatewayError
		if errors.As(err, &ge) && ge.Code == http.StatusBadGateway && strings.Contains(ge.Msg, "valid JSON") {
			// El gateway ya comprobó que no era JSON (respuesta cortada o sin
			// gramática): es una respuesta mala del modelo, no un fallo.
			return "", "", ErrInvalidJSON
		}
		return r.Output, r.Model, err
	}
}

// Metrics devuelve el texto de GET /metrics.
func (c *GatewayClient) Metrics(ctx context.Context) ([]byte, error) {
	var b []byte
	err := c.Do(ctx, http.MethodGet, "/metrics", nil, &b)
	return b, err
}

// WakeCount son los despertares de réplicas de un modelo, por cómo (thaw:
// estaba congelada; restore: nueva desde el dorado; adopt), según
// kling_ai_von_wake_seconds del gateway.
type WakeCount struct {
	N     int
	Sum   float64 // segundos
	State map[string]int
}

// ParseWakes lee de /metrics los despertares por modelo y cómo
// ("modelo|how") y las réplicas por modelo y estado (en State de la clave
// "modelo|").
func ParseWakes(metrics []byte) map[string]*WakeCount {
	out := map[string]*WakeCount{}
	get := func(k string) *WakeCount {
		w := out[k]
		if w == nil {
			w = &WakeCount{State: map[string]int{}}
			out[k] = w
		}
		return w
	}
	for _, line := range strings.Split(string(metrics), "\n") {
		name, labels, val, ok := promLine(line)
		if !ok {
			continue
		}
		switch name {
		case "kling_ai_von_wake_seconds_count":
			get(labels["model"] + "|" + labels["how"]).N = int(val)
		case "kling_ai_von_wake_seconds_sum":
			get(labels["model"] + "|" + labels["how"]).Sum = val
		case "kling_ai_von_replicas":
			get(labels["model"] + "|").State[labels["state"]] = int(val)
		}
	}
	return out
}

// promLine parte una línea de la exposición de Prometheus: nombre{a="b",…} valor.
func promLine(line string) (name string, labels map[string]string, val float64, ok bool) {
	if line == "" || line[0] == '#' {
		return
	}
	i := strings.IndexByte(line, '{')
	j := strings.LastIndexByte(line, '}')
	if i < 0 || j < i {
		return
	}
	name = line[:i]
	labels = map[string]string{}
	for _, kv := range strings.Split(line[i+1:j], ",") {
		k, v, found := strings.Cut(kv, "=")
		if found {
			labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	if _, err := fmt.Sscan(strings.TrimSpace(line[j+1:]), &val); err != nil {
		return
	}
	return name, labels, val, true
}

// Wake son los despertares de réplicas de un modelo entre dos lecturas de
// /metrics: con una orden a la vez, los de esa orden.
type Wake struct {
	Model string  `json:"model"`
	How   string  `json:"how"` // thaw | restore | adopt
	N     int     `json:"n"`
	MS    float64 `json:"ms"` // media
}

// WakesBetween compara dos lecturas de ParseWakes.
func WakesBetween(before, after map[string]*WakeCount) []Wake {
	var out []Wake
	for k, a := range after {
		model, how, _ := strings.Cut(k, "|")
		if how == "" {
			continue
		}
		var n int
		var sum float64
		if b := before[k]; b != nil {
			n, sum = b.N, b.Sum
		}
		if dn := a.N - n; dn > 0 {
			out = append(out, Wake{Model: model, How: how, N: dn, MS: 1000 * (a.Sum - sum) / float64(dn)})
		}
	}
	return out
}

// TaskInfo es lo que se usa de GET /v1/tasks.
type TaskInfo struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`   // classify | generate | domotica
	Chispa string `json:"chispa"` // modelo rápido de una tarea domotica
	// ChispaBackend: inprocess (en el proceso del gateway) o microvm
	// (serverless, kling chispa deploy).
	ChispaBackend string `json:"chispa_backend"`
	VON           string `json:"von"` // LLM de una generación
	Cascade       *struct {
		Status string `json:"status"` // capa 3 de una tarea domotica: on | forced | refused | off
		To     string `json:"to"`     // su codificador
		Reason string `json:"reason"`
	} `json:"cascade"`
}

// Tasks lista las tareas del gateway.
func (c *GatewayClient) Tasks(ctx context.Context) ([]TaskInfo, error) {
	var r struct {
		Tasks []TaskInfo `json:"tasks"`
	}
	err := c.Do(ctx, http.MethodGet, "/v1/tasks", nil, &r)
	return r.Tasks, err
}

// FastID identifica las capas 1–3 de una tarea domotica del gateway: su
// modelo rápido y su codificador, y si este está encendido. Es lo que decide
// qué llega a la capa 4, así que la evaluación de esta queda atada a él.
func FastID(t TaskInfo) string {
	enc := "none"
	if t.Cascade != nil && t.Cascade.To != "" {
		enc = t.Cascade.To + ":" + t.Cascade.Status
	}
	return "gateway:" + t.Name + "|fast=" + t.Chispa + "|encoder=" + enc
}
