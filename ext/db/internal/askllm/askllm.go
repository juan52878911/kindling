// Package askllm es el modelo de `kling db ask`: una interfaz mínima (texto de
// sistema + mensaje -> texto) y su implementación contra la API Messages de
// Anthropic, solo con net/http.
//
// El modelo NUNCA recibe credenciales ni ejecuta nada: quien llama le manda el
// esquema y la pregunta y recibe texto. Este paquete no sabe de bases de datos.
package askllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Provider es un modelo de lenguaje. En los tests se sustituye por uno falso.
type Provider interface {
	// Complete manda system y prompt y devuelve la respuesta en texto.
	Complete(ctx context.Context, system, prompt string) (string, error)
	// Name es a quién se le manda (para avisar de qué sale de la máquina).
	Name() string
}

const (
	// DefaultModel es el modelo si no se pide otro con -model.
	DefaultModel = "claude-sonnet-5"
	// EnvKey es la variable de la que se lee la clave de la API.
	EnvKey = "ANTHROPIC_API_KEY"

	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	// maxBody acota la respuesta que se lee.
	maxBody = 1 << 20
)

// ErrNoKey: no hay clave en el entorno.
var ErrNoKey = errors.New(EnvKey + " is not set: kling db ask needs an Anthropic API key in the environment")

// Anthropic habla con la API Messages.
type Anthropic struct {
	key       string
	Model     string
	MaxTokens int
	// URL es el punto de la API; vacío, el de Anthropic. Solo los tests la cambian.
	URL    string
	Client *http.Client
}

// FromEnv lee la clave de ANTHROPIC_API_KEY. model vacío: DefaultModel.
func FromEnv(model string) (*Anthropic, error) {
	key := strings.TrimSpace(os.Getenv(EnvKey))
	if key == "" {
		return nil, ErrNoKey
	}
	return New(key, model), nil
}

// New con una clave dada.
func New(key, model string) *Anthropic {
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{
		key: key, Model: model, MaxTokens: 2048,
		Client: &http.Client{
			Timeout: 120 * time.Second,
			// Sin redirecciones: la cabecera x-api-key no es de las que Go
			// quita al saltar a otro dominio, y la API no redirige.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Name implementa Provider.
func (a *Anthropic) Name() string { return "Anthropic API (" + a.Model + ")" }

// String no enseña la clave, por si alguien imprime el valor con %v.
func (a *Anthropic) String() string { return a.Name() }

// GoString tampoco (%#v).
func (a *Anthropic) GoString() string { return a.Name() }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type response struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implementa Provider.
func (a *Anthropic) Complete(ctx context.Context, system, prompt string) (string, error) {
	body, err := json.Marshal(request{
		Model: a.Model, MaxTokens: a.MaxTokens, System: system,
		Messages: []message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	u := a.URL
	if u == "" {
		u = anthropicURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := a.Client.Do(req)
	if err != nil {
		// El error de net/http lleva la URL, no las cabeceras.
		return "", fmt.Errorf("calling the model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("reading the model's answer: %w", err)
	}
	if len(raw) > maxBody {
		return "", errors.New("the model's answer is too large")
	}
	var r response
	jerr := json.Unmarshal(raw, &r)
	if resp.StatusCode != http.StatusOK {
		msg := resp.Status
		if jerr == nil && r.Error != nil {
			msg += ": " + r.Error.Type + ": " + clip(r.Error.Message, 300)
		}
		return "", fmt.Errorf("the model API answered %s", msg)
	}
	if jerr != nil {
		return "", fmt.Errorf("the model's answer is not JSON: %w", jerr)
	}
	var sb strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("the model returned no text (stop_reason %q)", r.StopReason)
	}
	return sb.String(), nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
