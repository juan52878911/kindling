package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Gateway habla con `kling ai serve` por HTTP: su socket Unix (0600, el
// propio usuario, sin token) o http://host:puerto con token.
type Gateway struct {
	base  string
	http  *http.Client
	token string
}

// maxResponse acota lo que se lee de una respuesta del gateway.
const maxResponse = 1 << 20

// NewGateway conecta con addr: la ruta de un socket Unix o una URL http(s).
// El token (solo para TCP) sale de $KLING_AI_TOKEN o de tokenFile, que no
// puede leer nadie más; nunca de la línea de órdenes.
func NewGateway(addr, tokenFile string, timeout time.Duration, conns int) (*Gateway, error) {
	tr := &http.Transport{MaxIdleConns: conns, MaxIdleConnsPerHost: conns, IdleConnTimeout: 90 * time.Second}
	g := &Gateway{base: strings.TrimRight(addr, "/"), http: &http.Client{Timeout: timeout, Transport: tr}}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		sock := addr
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		g.base = "http://ai"
		return g, nil
	}
	tok, err := readToken(tokenFile)
	if err != nil {
		return nil, err
	}
	g.token = tok
	return g, nil
}

func readToken(path string) (string, error) {
	if t := os.Getenv("KLING_AI_TOKEN"); t != "" {
		return t, nil
	}
	if path == "" {
		return "", nil
	}
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (%v): chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// APIError es un error del gateway con su código HTTP.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return fmt.Sprintf("gateway: %d %s", e.Code, e.Msg) }

func (g *Gateway) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return &APIError{Code: resp.StatusCode, Msg: truncUTF8(msg, 300)}
	}
	return json.Unmarshal(b, out)
}

// ClassProb es una etiqueta con su probabilidad.
type ClassProb struct {
	Label string  `json:"label"`
	Prob  float64 `json:"prob"`
}

// Classification es la respuesta de /v1/classify (lo que usa el ejemplo).
type Classification struct {
	Label     string  `json:"label"`
	Prob      float64 `json:"prob"`
	Escalate  bool    `json:"escalate"`
	Source    string  `json:"source"`
	LatencyMS float64 `json:"latency_ms"`
	Chispa    *struct {
		Label      string      `json:"label"`
		Prob       float64     `json:"prob"`
		Threshold  float64     `json:"threshold"`
		Decision   string      `json:"decision"`
		Candidates []ClassProb `json:"candidates"`
	} `json:"chispa"`
}

// Classify pide a Chispa (y solo a Chispa: mode "chispa") una decisión.
func (g *Gateway) Classify(ctx context.Context, task, text string, fields map[string]any) (*Classification, error) {
	var out Classification
	err := g.do(ctx, http.MethodPost, "/v1/classify", map[string]any{"task": task, "text": text, "fields": fields, "mode": "chispa"}, &out)
	return &out, err
}

// Generation es la respuesta de /v1/generate.
type Generation struct {
	Model        string  `json:"model"`
	Output       string  `json:"output"`
	FinishReason string  `json:"finish_reason"`
	LatencyMS    float64 `json:"latency_ms"`
	Usage        struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Generate pide a VON una generación de una tarea.
func (g *Gateway) Generate(ctx context.Context, task, input string, vars map[string]string) (*Generation, error) {
	var out Generation
	err := g.do(ctx, http.MethodPost, "/v1/generate", map[string]any{"task": task, "input": input, "vars": vars}, &out)
	return &out, err
}

// TaskInfo es una tarea del registro según GET /v1/tasks.
type TaskInfo struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Chispa string   `json:"chispa"`
	VON    string   `json:"von"`
	Labels []string `json:"labels"`
}

// Tasks lista las tareas del gateway.
func (g *Gateway) Tasks(ctx context.Context) ([]TaskInfo, error) {
	var out struct {
		Tasks []TaskInfo `json:"tasks"`
	}
	err := g.do(ctx, http.MethodGet, "/v1/tasks", nil, &out)
	return out.Tasks, err
}

// Explains es la etiqueta positiva del localizador.
const Explains = "explains"

// explainsProb es P(explains) de una respuesta del modelo binario del
// localizador, conteste lo que conteste.
func explainsProb(c *Classification) float64 {
	if c.Chispa != nil {
		for _, cp := range c.Chispa.Candidates {
			if cp.Label == Explains {
				return cp.Prob
			}
		}
	}
	if c.Label == Explains {
		return c.Prob
	}
	return 1 - c.Prob
}

// ScoreLines puntúa cada línea con P(explains) preguntando al gateway con
// `workers` peticiones en vuelo (conexiones reutilizadas: por el socket Unix
// cada decisión cuesta decenas de µs, casi todo JSON y HTTP). Devuelve un
// valor por línea del log (0 las que no se clasificaron) y el tiempo que tardó
// Chispa según el gateway, sumado.
func (g *Gateway) ScoreLines(ctx context.Context, task string, n int, in []LineInput, workers int) ([]float64, float64, error) {
	score := make([]float64, n)
	if len(in) == 0 {
		return score, 0, nil
	}
	workers = max(1, min(workers, len(in)))
	var (
		mu       sync.Mutex
		firstErr error
		chispaMS float64
		wg       sync.WaitGroup
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan int)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local float64
			for k := range next {
				c, err := g.Classify(ctx, task, in[k].Text, in[k].Fields)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					continue
				}
				score[in[k].Index] = explainsProb(c)
				local += c.LatencyMS
			}
			mu.Lock()
			chispaMS += local
			mu.Unlock()
		}()
	}
feed:
	for k := range in {
		select {
		case next <- k:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return nil, 0, firstErr
	}
	return score, chispaMS, ctx.Err()
}
