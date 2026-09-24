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
// Cada uno a lo suyo: JEV clasifica, enruta y filtra; VON genera (resume,
// redacta, contesta). Una tarea de clasificación puede además escalar lo que
// JEV duda a un VON (la cascada), pero solo si una evaluación con datos de la
// tarea muestra que acierta más que JEV solo (ver eval.go). Lo que VON contesta
// en lo escalado se guarda (acotado) para recalibrar los umbrales de JEV con el
// tráfico real, a petición: `kling ai calibrate`.
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

// TaskConfig es una tarea con nombre, de una de dos clases:
//
//   - CLASIFICACIÓN (jev): JEV decide —clasificar, enrutar, filtrar—. Cuando
//     duda, la respuesta sale igual con escalate: true y quien llama decide.
//     Con escalate_to, la duda la resuelve un modelo VON (la cascada), pero
//     solo si un registro de evaluación (`kling ai eval`) muestra que la
//     cascada acierta más que JEV solo en los datos de esa tarea; si no, el
//     gateway se niega a activarla salvo escalate_force.
//   - GENERACIÓN (von): VON resume, redacta, contesta. La pregunta sale de una
//     plantilla con {input} y las variables que mande el cliente.
//
// Por qué la cascada no va sola: medido en la clasificación de commits
// (docs/ai-gateway.md), la cascada JEV → LLM de 0,5B acertaba MENOS que JEV
// solo (0,43 frente a 0,64). Un LLM pequeño no mejora a un clasificador
// entrenado por serlo; hay que demostrarlo tarea a tarea.
type TaskConfig struct {
	// JEV es el modelo de una tarea de clasificación.
	JEV string `json:"jev,omitempty"`
	// EscalateTo es el modelo VON al que la cascada manda lo que JEV duda.
	EscalateTo string `json:"escalate_to,omitempty"`
	// EscalateForce activa la cascada aunque su evaluación no la respalde (o
	// no la haya). Es el -force de la decisión: queda escrito en el registro.
	EscalateForce bool `json:"escalate_force,omitempty"`
	// VON es el modelo de una tarea de generación.
	VON string `json:"von,omitempty"`

	// Labels son las etiquetas válidas; salen del modelo JEV y, si se dan
	// aquí también, tienen que ser las mismas.
	Labels []string `json:"labels,omitempty"`
	// System y Prompt son la pregunta a VON. En una escalada, variables
	// {labels}, {text}, {fields} y {candidates} (el top-3 de JEV con su
	// probabilidad); en una generación, {input} y las de "vars".
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
	// umbrales prometen de más. Solo con la cascada activa.
	Audit float64 `json:"audit,omitempty"`
	// Samples es el tamaño del anillo de muestras (0 = 2000).
	Samples int `json:"samples,omitempty"`
	// MaxTokens de la respuesta de VON: 16 por defecto en una escalada (una
	// etiqueta), 256 en una generación, que es también el tope que puede
	// pedir un cliente.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature de una generación (nil = 0,7; un cliente puede cambiarla).
	// Las escaladas van siempre a 0.
	Temperature *float64 `json:"temperature,omitempty"`
	// Grammar restringe la salida de VON a exactamente una etiqueta con una
	// gramática de llama-server (nil = sí). Un modelo de 360M parámetros
	// divaga; con la gramática no puede.
	Grammar *bool `json:"grammar,omitempty"`
	// OnVONError: "jev" (por defecto) contesta con la etiqueta de JEV marcada
	// como degradada si VON no responde; "error" devuelve 503.
	OnVONError string `json:"on_von_error,omitempty"`
}

// IsGenerate dice si la tarea es de generación (VON) y no de clasificación.
func (t *TaskConfig) IsGenerate() bool { return t.VON != "" }

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
	maxTokensCap   = 256  // de una escalada: una etiqueta
	maxGenTokens   = 4096 // de una generación
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
		gen := t.VON != ""
		switch {
		case t.JEV == "" && t.VON == "":
			errs = append(errs, fmt.Errorf("task %q: needs a jev model (classification) or a von model (generation)", n))
		case t.JEV != "" && t.VON != "":
			errs = append(errs, fmt.Errorf("task %q: jev and von together: a classification task escalates with \"escalate_to\", and \"von\" is for generation tasks", n))
		}
		if t.JEV != "" && (c.Models[t.JEV] == nil || c.Models[t.JEV].Kind != KindJEV) {
			errs = append(errs, fmt.Errorf("task %q: %q is not a jev model", n, t.JEV))
		}
		if t.VON != "" && (c.Models[t.VON] == nil || c.Models[t.VON].Kind != KindVON) {
			errs = append(errs, fmt.Errorf("task %q: %q is not a von model", n, t.VON))
		}
		if t.EscalateTo != "" && (t.JEV == "" || c.Models[t.EscalateTo] == nil || c.Models[t.EscalateTo].Kind != KindVON) {
			errs = append(errs, fmt.Errorf("task %q: escalate_to needs a jev task and a von model, and %q is not one", n, t.EscalateTo))
		}
		if t.EscalateForce && t.EscalateTo == "" {
			errs = append(errs, fmt.Errorf("task %q: escalate_force without escalate_to", n))
		}
		if gen {
			// Lo que solo tiene sentido al clasificar no se acepta en una
			// generación: una clave que no hace nada es un error que no se ve.
			if len(t.Labels) > 0 || t.TopK != 0 || len(t.Thresholds) > 0 || t.Precision != 0 || t.Audit != 0 ||
				t.Samples != 0 || t.Grammar != nil || t.OnVONError != "" {
				errs = append(errs, fmt.Errorf("task %q: labels, top_k, thresholds, precision, audit, samples, grammar and on_von_error are for classification tasks", n))
			}
			if t.MaxTokens < 0 || t.MaxTokens > maxGenTokens {
				errs = append(errs, fmt.Errorf("task %q: max_tokens must be 0..%d", n, maxGenTokens))
			}
			if t.Temperature != nil && !(*t.Temperature >= 0 && *t.Temperature <= 2) {
				errs = append(errs, fmt.Errorf("task %q: temperature must be in [0,2]", n))
			}
		} else {
			if t.Temperature != nil {
				errs = append(errs, fmt.Errorf("task %q: temperature is for generation tasks (escalations answer at 0)", n))
			}
			if t.MaxTokens < 0 || t.MaxTokens > maxTokensCap {
				errs = append(errs, fmt.Errorf("task %q: max_tokens must be 0..%d", n, maxTokensCap))
			}
			if t.Audit > 0 && t.EscalateTo == "" {
				errs = append(errs, fmt.Errorf("task %q: audit asks the escalate_to model; set it or drop audit", n))
			}
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
		if t.TopK < 0 || t.TopK > maxLabels {
			errs = append(errs, fmt.Errorf("task %q: top_k must be 0..%d", n, maxLabels))
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
