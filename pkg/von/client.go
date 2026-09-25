package von

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// API COMPATIBLE CON OPENAI, LO JUSTO.
//
// Solo los campos que usan el CLI y el calentamiento. El gateway que venga
// después reenvía el cuerpo tal cual y no necesita más tipos que estos para
// leer lo que le interese (uso de tokens, tiempos).

// Message es un mensaje de chat.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest es el cuerpo de POST /v1/chat/completions.
type ChatRequest struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	// Seed fija la semilla. Sin ella llama-server saca una nueva por petición
	// (ver docs/von.md sobre réplicas restauradas del mismo dorado).
	Seed   *int64 `json:"seed,omitempty"`
	Stream bool   `json:"stream,omitempty"`
}

// Timings es la extensión de llama-server con los tiempos de la petición: la
// forma más honrada de medir, porque no incluye red ni proxy.
type Timings struct {
	PromptN            int     `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedN         int     `json:"predicted_n"`
	PredictedMS        float64 `json:"predicted_ms"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
	CacheN             int     `json:"cache_n"`
}

// ChatResponse es la respuesta (sin streaming).
type ChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Timings *Timings `json:"timings,omitempty"`
}

// Text es el contenido de la primera elección.
func (r *ChatResponse) Text() string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Message.Content
}

// ErrLoading es la respuesta de /health mientras llama-server carga el modelo.
var ErrLoading = errors.New("llama-server is still loading the model")

// Health pregunta a llama-server, por el proxy del daemon, si está listo.
// Devuelve nil (200), ErrLoading (503: cargando) u otro error.
func Health(ctx context.Context, c *api.Client, ref string) error {
	resp, err := c.Guest(ctx, ref, api.GuestRequest{Port: Port, Path: "/health", Method: http.MethodGet})
	if err != nil {
		return err
	}
	switch resp.Status {
	case http.StatusOK:
		return nil
	case http.StatusServiceUnavailable:
		return ErrLoading
	default:
		return fmt.Errorf("/health answered %d: %s", resp.Status, recortar(resp.Body))
	}
}

// WaitReady espera a que llama-server conteste 200 en /health: primero a que
// el puerto abra (lo espera el daemon, en la red que ve al invitado) y luego a
// que termine de cargar el modelo. Sondea cada 50 ms porque lo que se mide
// después (arranque en frío hasta el primer token) no debe llevar la mitad de
// un intervalo de sondeo de propina.
func WaitReady(ctx context.Context, c *api.Client, ref string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if _, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: Port, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	}); err != nil {
		return fmt.Errorf("llama-server did not open port %d: %w", Port, err)
	}
	var last error
	for time.Now().Before(deadline) {
		last = Health(ctx, c, ref)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("llama-server not ready after %s: %w", timeout, last)
}

// Chat manda una petición de chat por el proxy del daemon y devuelve la
// respuesta y lo que tardó de punta a punta vista desde aquí.
func Chat(ctx context.Context, c *api.Client, ref string, req ChatRequest) (*ChatResponse, time.Duration, error) {
	if req.Stream {
		return nil, 0, fmt.Errorf("streaming does not go through the daemon proxy: talk to the machine's address directly")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}
	t0 := time.Now()
	resp, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: Port, Path: "/v1/chat/completions", Method: http.MethodPost, Body: string(body),
	})
	dur := time.Since(t0)
	if err != nil {
		return nil, dur, err
	}
	if resp.Status != http.StatusOK {
		return nil, dur, fmt.Errorf("chat completion answered %d: %s", resp.Status, recortar(resp.Body))
	}
	var out ChatResponse
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return nil, dur, fmt.Errorf("chat completion: %w", err)
	}
	return &out, dur, nil
}

// Warm calienta un llama-server recién cargado con una respuesta corta.
//
// Cargar no basta para un dorado bueno: llama-server hace una pasada vacía al
// arrancar, pero la primera petición de verdad todavía toca páginas de pesos
// que la pasada no leyó, crea los hilos de OpenMP y reserva los búferes de
// cálculo del tamaño real de un lote. Todo eso, hecho ANTES de congelar, queda
// dentro del snapshot y ninguna réplica lo vuelve a pagar.
func Warm(ctx context.Context, c *api.Client, ref string) (*ChatResponse, error) {
	cero := 0.0
	r, _, err := Chat(ctx, c, ref, ChatRequest{
		Messages:    []Message{{Role: "user", Content: "Say hello in one word."}},
		MaxTokens:   8,
		Temperature: &cero,
	})
	return r, err
}

// Prefix es el principio fijo de las peticiones de una tarea: su system prompt
// y, si la plantilla del usuario empieza con texto fijo, ese texto. Es lo que
// el dorado deja ya evaluado (en la ranura y en la caché de prompts) para que
// una réplica recién restaurada solo evalúe lo que cambia en cada petición.
type Prefix struct {
	System string `json:"system,omitempty"`
	User   string `json:"user,omitempty"`
}

// Messages es la conversación con la que se calienta el prefijo. Sin texto de
// usuario va uno neutro: la plantilla de chat pide un turno del usuario, y lo
// que se aprovecha luego es lo común con la petición real (el system prompt y
// el principio del turno), no la respuesta.
func (p Prefix) Messages() []Message {
	var out []Message
	if p.System != "" {
		out = append(out, Message{Role: "system", Content: p.System})
	}
	u := p.User
	if u == "" {
		u = "Hi"
	}
	return append(out, Message{Role: "user", Content: u})
}

// LabelPrefixes es la etiqueta de un dorado con prefijos precalculados: su
// PrefixesHash. Dice si el dorado está al día con las tareas que lo usan.
const LabelPrefixes = "von.prefixes"

// PrefixesHash identifica un conjunto de prefijos, sin importar el orden (12
// hex del sha256). Vacío si no hay ninguno.
func PrefixesHash(ps []Prefix) string {
	if len(ps) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ps))
	for _, p := range ps {
		b, _ := json.Marshal(p)
		keys = append(keys, string(b))
	}
	sort.Strings(keys)
	h := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(h[:6])
}

// GoldenOptions es cómo crear el dorado de un modelo.
type GoldenOptions struct {
	Image    string // imagen construida con el constructor llm
	Snapshot string // nombre del dorado
	Ref      string // valor de von.model
	VCPUs    int
	MemMiB   int
	// CPUPct es el techo de CPU (% de un core) que el dorado graba y sus
	// réplicas heredan. 0 = un core entero por vCPU. El del daemon por defecto
	// (50 %, pensado para herramientas MCP que esperan casi siempre) deja a un
	// modelo de 2 vCPU a una cuarta parte de su velocidad.
	CPUPct int
	// AllowExec deja exec y cp en el dorado y en sus réplicas (depurar).
	AllowExec bool
	Replace   bool
	// Wait es cuánto esperar a que el modelo cargue.
	Wait time.Duration
	// Prefixes son los prefijos de las tareas que se dejan evaluados en el
	// dorado, en este orden, después del calentamiento. El último queda en la
	// ranura; los demás, en la caché de prompts de llama-server (Spec.CacheRAM:
	// sin ella solo sobrevive el último).
	Prefixes []Prefix
	// Labels extra; las de VON (von.model, kling.ports, service) se añaden.
	Labels map[string]string
	// Log recibe el progreso, línea a línea. Puede ser nil.
	Log func(format string, args ...any)
	// Kind es el de la imagen (KindEmbed para un codificador): decide cómo se
	// calienta. Quien llama añade LabelKind a Labels.
	Kind string
}

// GoldenResult es lo que costó cada paso, para contarlo.
type GoldenResult struct {
	Snapshot *api.Snapshot
	BootMS   int64         // arranque de la microVM (lo dice el daemon)
	LoadTime time.Duration // de arrancada a /health 200
	Warm     *ChatResponse
	// PrefixTokens son los tokens de cada prefijo precalculado.
	PrefixTokens []int
}

// Labels son las etiquetas de una máquina VON: el modelo, el puerto que el
// proxy del daemon y el backend de macOS tienen que dejar pasar, y el servicio.
func Labels(ref, service string, extra map[string]string) map[string]string {
	out := api.MergeLabels(extra, map[string]string{
		LabelModel:     ref,
		api.LabelPorts: strconv.Itoa(Port),
	})
	if service != "" {
		out[api.LabelService] = service
	}
	return out
}

// MakeGolden arranca una microVM de la imagen, espera a que el modelo cargue,
// la calienta, la congela como dorado y la borra. Es lo que hace `kling models
// add` tras construir la imagen, y lo que hará un gateway que (re)cree
// dorados: por ejemplo tras reiniciar el host, que invalida los snapshots.
func MakeGolden(ctx context.Context, c *api.Client, o GoldenOptions) (*GoldenResult, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Wait == 0 {
		o.Wait = 5 * time.Minute
	}
	if o.CPUPct == 0 {
		o.CPUPct = 100 * max(o.VCPUs, 1)
	}
	// La plantilla lleva nombre propio y aleatorio: dos `models add` a la vez
	// del mismo modelo no deben pisarse la máquina.
	name := fmt.Sprintf("%s-golden-%s", o.Snapshot, strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36))
	labels := Labels(o.Ref, o.Snapshot, o.Labels)
	delete(labels, LabelPrefixes)
	if h := PrefixesHash(o.Prefixes); h != "" {
		labels[LabelPrefixes] = h
	}
	mc, err := c.Run(ctx, api.RunRequest{
		Name: name, Image: o.Image, VCPUs: o.VCPUs, MemMiB: o.MemMiB,
		CPUPct:    o.CPUPct,
		Egress:    "none",
		Labels:    labels,
		AllowExec: o.AllowExec,
	})
	if err != nil {
		return nil, fmt.Errorf("booting %s: %w", o.Image, err)
	}
	res := &GoldenResult{BootMS: mc.BootMS}
	// Pase lo que pase, la plantilla no se queda: lo que vale es el dorado.
	defer func() {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.Remove(rc, mc.ID)
	}()
	logf("booted %s in %d ms; loading the model...", mc.Name, mc.BootMS)

	t0 := time.Now()
	if err := WaitReady(ctx, c, mc.ID, o.Wait); err != nil {
		return nil, fmt.Errorf("%w\nsee the server log with:  kling exec %s -- tail -50 /var/log/service.log", err, mc.Name)
	}
	res.LoadTime = time.Since(t0)
	logf("model loaded in %s; warming up...", res.LoadTime.Round(time.Millisecond))

	// La primera respuesta puede tardar más que el plazo de cabeceras del
	// proxy del daemon (5 min): en el laboratorio anidado, un 1,5B o un 3B
	// trae sus pesos a base de fallos de página de ~1 ms. llama-server sigue
	// con ella aunque el proxy se canse, así que se reintenta mientras quede
	// Wait: el reintento espera en su cola y sale en cuanto la primera acaba.
	warmHasta := time.Now().Add(o.Wait)
	var w *ChatResponse
	for {
		if w, err = warm(ctx, c, mc.ID, o.Kind); err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(warmHasta) {
			return nil, fmt.Errorf("warm-up: %w", err)
		}
		logf("warm-up not answered yet (%v); retrying", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	res.Warm = w
	logf("warm-up answered %q", w.Text())

	// Los prefijos de las tareas, evaluados antes de congelar: una respuesta de
	// un token a temperatura 0 basta para que su caché KV quede en la ranura
	// (y, al llegar el siguiente, en la caché de prompts). Medido en
	// docs/von-cpu.md: la primera petición de una tarea con ~800 tokens de
	// system prompt pasa de 4,7 s a 0,4 s en Qwen2.5-1.5B.
	cero := 0.0
	for i, p := range o.Prefixes {
		r, _, err := Chat(ctx, c, mc.ID, ChatRequest{Messages: p.Messages(), MaxTokens: 1, Temperature: &cero})
		if err != nil {
			return nil, fmt.Errorf("prefix %d: %w", i+1, err)
		}
		res.PrefixTokens = append(res.PrefixTokens, r.Usage.PromptTokens)
		if r.Timings != nil {
			logf("prefix %d of %d: %d tokens evaluated in %.0f ms", i+1, len(o.Prefixes), r.Timings.PromptN, r.Timings.PromptMS)
		}
	}

	// Devolver al host lo reclamable (memoria libre y caché de páginas limpia)
	// ANTES de congelar: el globo deja esas páginas a cero, el commit las
	// convierte en huecos del mem.file y el dorado ocupa solo lo que el modelo
	// usa de verdad. Si la máquina no tiene globo, se congela igual.
	if sq, err := c.Squeeze(ctx, mc.ID); err == nil {
		logf("returned %d MiB of free memory to the host before freezing", sq.ReclaimedMiB)
	}

	snap, err := c.Commit(ctx, mc.ID, o.Snapshot, o.Replace)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	res.Snapshot = snap
	return res, nil
}

func recortar(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
