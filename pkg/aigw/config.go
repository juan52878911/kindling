// Package aigw es el gateway de IA de kindling: muchos modelos listos bajo
// demanda y ninguno corriendo 24/7.
//
// Dos clases de modelo detrás de una misma API:
//
//   - JEV (pkg/jev): un clasificador lineal de 1–5 MB que vive DENTRO de este
//     proceso. Se carga la primera vez que se usa y se descarta por LRU cuando
//     los cargados pasan del presupuesto de memoria. Contesta en microsegundos.
//   - VON (pkg/von): un LLM pequeño en una microVM, restaurado de un dorado
//     congelado con el modelo ya cargado. pkg/scheduler lo despierta con la
//     primera petición, lo congela al quedarse ocioso (0 CPU; en Linux su
//     memoria vuelve al fichero) y añade réplicas si no da abasto.
//
// Una tarea une los dos en CASCADA: JEV contesta si está seguro (su
// probabilidad calibrada supera el umbral de la clase) y escala a VON si no.
// Lo que VON contesta en lo escalado se guarda (acotado) para recalibrar los
// umbrales de JEV con el tráfico real, a petición: `kling ai calibrate`.
package aigw

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Tipos de modelo.
const (
	KindJEV = "jev"
	KindVON = "von"
)

// Config es el registro de modelos y tareas (un fichero JSON, ai.json).
//
// Es un fichero y no el store del daemon a propósito: los .jev son rutas del
// host donde corre el gateway, que puede no ser el del daemon (el daemon puede
// estar al otro lado de un SSH), y un fichero se versiona, se revisa y se
// despliega como cualquier otra configuración.
type Config struct {
	Models map[string]*ModelConfig `json:"models"`
	Tasks  map[string]*TaskConfig  `json:"tasks"`
	// Tenants son tokens con nombre y cuota (reparto justo, no seguridad; ver
	// pkg/scheduler/quota.go). Con tokens aquí, el fichero debe ser 0600.
	Tenants []TenantConfig `json:"tenants,omitempty"`
}

// ModelConfig es un modelo con nombre.
type ModelConfig struct {
	Kind string `json:"kind"` // jev | von
	// Path es el .jev (kind jev). Relativo = relativo al fichero de config.
	Path string `json:"path,omitempty"`
	// Snapshot es el dorado de `kling models add` (kind von).
	Snapshot string `json:"snapshot,omitempty"`
	// MaxReplicas acota las réplicas de este modelo (0 = la del gateway).
	MaxReplicas int `json:"max_replicas,omitempty"`
}

// TaskConfig es una decisión con nombre: qué JEV contesta primero, a qué VON se
// escala y cómo se le pregunta.
type TaskConfig struct {
	JEV string `json:"jev,omitempty"`
	VON string `json:"von,omitempty"`
	// Labels son las etiquetas válidas. Con JEV salen del modelo y, si se dan
	// aquí también, tienen que ser las mismas; sin JEV son obligatorias.
	Labels []string `json:"labels,omitempty"`
	// System y Prompt son la pregunta a VON. Variables: {labels}, {text},
	// {fields} y {candidates} (el top-3 de JEV con su probabilidad).
	System string `json:"system,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// TopK > 0 convierte a VON en un reordenador: en una escalada solo puede
	// elegir entre las K etiquetas más probables según JEV (la pregunta y la
	// gramática llevan solo esas). Un LLM diminuto elige mejor entre tres que
	// entre diez, y el top-3 de JEV suele contener la buena aunque la primera
	// no lo sea. 0 = todas las etiquetas.
	TopK int `json:"top_k,omitempty"`
	// Thresholds sustituye el umbral τ de JEV para las clases que nombra
	// (p. ej. subirlo en una clase que se equivoca más de lo prometido).
	Thresholds map[string]float64 `json:"thresholds,omitempty"`
	// Precision es la precisión objetivo al recalibrar (0 = la del modelo, o
	// 0,95).
	Precision float64 `json:"precision,omitempty"`
	// Audit es la fracción de respuestas CONFIADAS de JEV que se preguntan
	// también a VON en segundo plano, solo para la recalibración: sin ellas la
	// muestra solo tendría lo que JEV escaló y no se podría saber si los
	// umbrales prometen de más.
	Audit float64 `json:"audit,omitempty"`
	// Samples es el tamaño del anillo de muestras (0 = 2000).
	Samples int `json:"samples,omitempty"`
	// MaxTokens de la respuesta de VON (0 = 16: una etiqueta).
	MaxTokens int `json:"max_tokens,omitempty"`
	// Grammar restringe la salida de VON a exactamente una etiqueta con una
	// gramática de llama-server (nil = sí). Un modelo de 360M parámetros
	// divaga; con la gramática no puede.
	Grammar *bool `json:"grammar,omitempty"`
	// OnVONError: "jev" (por defecto) contesta con la etiqueta de JEV marcada
	// como degradada si VON no responde; "error" devuelve 503.
	OnVONError string `json:"on_von_error,omitempty"`
}

// TenantConfig es un token con nombre y sus cuotas.
type TenantConfig struct {
	Name         string `json:"name"`
	Token        string `json:"token"`
	MaxInflight  int    `json:"max_inflight,omitempty"`
	MaxInstances int    `json:"max_instances,omitempty"`
}

// Límites del registro: el fichero lo escribe una persona, pero lo lee un
// servicio que escucha en la red, y un error de bulto no debe tumbarlo.
const (
	maxConfigBytes = 1 << 20
	maxModels      = 256
	maxTasks       = 256
	maxPromptBytes = 16 << 10
	maxLabels      = 256
	maxSamples     = 100_000
	maxTokensCap   = 256
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// LoadConfig lee y valida el registro.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := ParseConfig(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, m := range c.Models {
		if m.Kind == KindJEV && !filepath.IsAbs(m.Path) {
			m.Path = filepath.Join(filepath.Dir(path), m.Path)
		}
	}
	return c, nil
}

// ParseConfig lee y valida un registro de r (como mucho 1 MiB).
func ParseConfig(r io.Reader) (*Config, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigBytes {
		return nil, fmt.Errorf("config larger than %d bytes", maxConfigBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // una clave mal escrita no puede pasar en silencio
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// Validate comprueba nombres, referencias y rangos.
func (c *Config) Validate() error {
	if c.Models == nil {
		c.Models = map[string]*ModelConfig{}
	}
	if c.Tasks == nil {
		c.Tasks = map[string]*TaskConfig{}
	}
	if len(c.Models) > maxModels || len(c.Tasks) > maxTasks {
		return fmt.Errorf("at most %d models and %d tasks", maxModels, maxTasks)
	}
	var errs []error
	for _, n := range sortedKeys(c.Models) {
		m := c.Models[n]
		if !nameRE.MatchString(n) {
			errs = append(errs, fmt.Errorf("model %q: invalid name (lowercase letters, digits, . _ -)", n))
		}
		if m == nil {
			errs = append(errs, fmt.Errorf("model %q: empty", n))
			continue
		}
		switch m.Kind {
		case KindJEV:
			if m.Path == "" || m.Snapshot != "" {
				errs = append(errs, fmt.Errorf("model %q: a jev model needs path (and no snapshot)", n))
			}
		case KindVON:
			if m.Snapshot == "" || m.Path != "" {
				errs = append(errs, fmt.Errorf("model %q: a von model needs snapshot (and no path)", n))
			}
		default:
			errs = append(errs, fmt.Errorf("model %q: kind must be jev or von, not %q", n, m.Kind))
		}
		if m.MaxReplicas < 0 || m.MaxReplicas > 64 {
			errs = append(errs, fmt.Errorf("model %q: max_replicas must be 0..64", n))
		}
	}
	for _, n := range sortedKeys(c.Tasks) {
		t := c.Tasks[n]
		if !nameRE.MatchString(n) {
			errs = append(errs, fmt.Errorf("task %q: invalid name", n))
		}
		if t == nil {
			errs = append(errs, fmt.Errorf("task %q: empty", n))
			continue
		}
		if t.JEV == "" && t.VON == "" {
			errs = append(errs, fmt.Errorf("task %q: needs a jev model, a von model or both", n))
		}
		if t.JEV != "" && (c.Models[t.JEV] == nil || c.Models[t.JEV].Kind != KindJEV) {
			errs = append(errs, fmt.Errorf("task %q: %q is not a jev model", n, t.JEV))
		}
		if t.VON != "" && (c.Models[t.VON] == nil || c.Models[t.VON].Kind != KindVON) {
			errs = append(errs, fmt.Errorf("task %q: %q is not a von model", n, t.VON))
		}
		if t.JEV == "" && len(t.Labels) == 0 {
			errs = append(errs, fmt.Errorf("task %q: without a jev model, labels are required", n))
		}
		if len(t.Labels) > maxLabels {
			errs = append(errs, fmt.Errorf("task %q: at most %d labels", n, maxLabels))
		}
		seen := map[string]bool{}
		for _, l := range t.Labels {
			if l == "" || len(l) > 128 || strings.ContainsAny(l, "\n\r\"\\") || seen[l] || l == Unknown {
				errs = append(errs, fmt.Errorf("task %q: invalid or repeated label %q", n, l))
			}
			seen[l] = true
		}
		if len(t.System) > maxPromptBytes || len(t.Prompt) > maxPromptBytes {
			errs = append(errs, fmt.Errorf("task %q: system/prompt larger than %d bytes", n, maxPromptBytes))
		}
		for l, v := range t.Thresholds {
			if !(v >= 0 && v <= 2) { // 2 = jev.NeverConfident: la clase escala siempre
				errs = append(errs, fmt.Errorf("task %q: threshold for %q must be in [0,2]", n, l))
			}
		}
		if !(t.Precision >= 0 && t.Precision < 1) {
			errs = append(errs, fmt.Errorf("task %q: precision must be in [0,1)", n))
		}
		if !(t.Audit >= 0 && t.Audit <= 1) {
			errs = append(errs, fmt.Errorf("task %q: audit must be in [0,1]", n))
		}
		if t.Samples < 0 || t.Samples > maxSamples {
			errs = append(errs, fmt.Errorf("task %q: samples must be 0..%d", n, maxSamples))
		}
		if t.TopK < 0 || t.TopK > maxLabels || (t.TopK > 0 && t.JEV == "") {
			errs = append(errs, fmt.Errorf("task %q: top_k needs a jev model and must be 0..%d", n, maxLabels))
		}
		if t.MaxTokens < 0 || t.MaxTokens > maxTokensCap {
			errs = append(errs, fmt.Errorf("task %q: max_tokens must be 0..%d", n, maxTokensCap))
		}
		switch t.OnVONError {
		case "", "jev", "error":
		default:
			errs = append(errs, fmt.Errorf("task %q: on_von_error must be jev or error", n))
		}
	}
	for i, tn := range c.Tenants {
		// "default" es el token principal (el que administra): un tenant con
		// ese nombre heredaría las rutas de administración.
		if !nameRE.MatchString(tn.Name) || tn.Name == "default" || len(tn.Token) < 16 {
			errs = append(errs, fmt.Errorf("tenant %d: needs a valid name (not \"default\") and a token of at least 16 characters", i))
		}
	}
	return errors.Join(errs...)
}

// vonModel busca un modelo VON por su nombre en el registro o por el de su
// dorado: un cliente OpenAI pone en "model" lo que ve en /v1/models.
func (c *Config) vonModel(name string) (string, *ModelConfig) {
	if m := c.Models[name]; m != nil && m.Kind == KindVON {
		return name, m
	}
	for _, n := range sortedKeys(c.Models) {
		if m := c.Models[n]; m.Kind == KindVON && m.Snapshot == name {
			return n, m
		}
	}
	return "", nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
