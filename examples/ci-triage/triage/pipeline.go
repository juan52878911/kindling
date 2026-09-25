package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Options son las tareas del gateway y cómo se usan.
type Options struct {
	LinesTask    string // Chispa binario por línea: explains / noise
	CategoryTask string // Chispa multiclase sobre el trozo
	SummaryTask  string // VON con json_schema; vacío = sin capa 3
	// VON: "escalated" (solo cuando Chispa duda de la categoría; por defecto),
	// "always" (también para resumir lo que Chispa ya sabe) u "off".
	VON     string
	Workers int
	Chunk   ChunkOptions
}

// DefaultOptions casan con examples/ci-triage/ai.json.
var DefaultOptions = Options{LinesTask: "ci-lines", CategoryTask: "ci-category", SummaryTask: "ci-summary",
	VON: "escalated", Workers: 8, Chunk: DefaultChunkOptions}

// Timing es lo que tardó cada capa, en milisegundos, visto por este proceso.
type Timing struct {
	Read     float64 `json:"read_ms"`
	Features float64 `json:"features_ms"`
	Lines    float64 `json:"lines_ms"`        // Chispa en todas las líneas, por el gateway
	LinesIn  float64 `json:"lines_chispa_ms"` // lo mismo según el gateway (suma de latency_ms)
	Locate   float64 `json:"locate_ms"`
	Category float64 `json:"category_ms"`
	VON      float64 `json:"von_ms,omitempty"`
	Total    float64 `json:"total_ms"`
}

// ChunkOut es un tramo en la salida, con líneas numeradas desde 1.
type ChunkOut struct {
	From  int     `json:"from"`
	To    int     `json:"to"`
	Score float64 `json:"score"`
}

// VONAnswer es lo que contestó VON, ya validado.
type VONAnswer struct {
	Category         string  `json:"category"`
	Summary          string  `json:"summary"`
	NextStep         string  `json:"next_step"`
	Model            string  `json:"model"`
	LatencyMS        float64 `json:"latency_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
}

// Result es el triaje de un log.
type Result struct {
	Format     string      `json:"format"`
	Lines      int         `json:"lines"`
	Scored     int         `json:"lines_classified"`
	Truncated  bool        `json:"truncated,omitempty"`
	Chunks     []ChunkOut  `json:"chunks"`
	Chunk      string      `json:"chunk"`
	Category   string      `json:"category"`
	Prob       float64     `json:"confidence"`       // probabilidad calibrada de Chispa para Category
	Confident  bool        `json:"chispa_confident"` // Chispa llegó a su umbral
	Layer      string      `json:"decided_by"`       // chispa | von | chispa-unsure
	Candidates []ClassProb `json:"candidates,omitempty"`
	VON        *VONAnswer  `json:"von,omitempty"`
	VONError   string      `json:"von_error,omitempty"`
	Timing     Timing      `json:"timing"`
	// Scores es P(explains) de cada línea (0 las vacías): la página los usa
	// para resaltar. No va en el JSON de analyze.
	Scores []float64 `json:"-"`
}

// Analyze hace el triaje de un log ya leído: las tres capas.
func Analyze(ctx context.Context, g *Gateway, lg *Log, o Options) (*Result, error) {
	t0 := time.Now()
	r := &Result{Format: lg.Format, Lines: len(lg.Lines), Truncated: lg.Truncated}

	tf := time.Now()
	in := Features(lg)
	r.Timing.Features = ms(time.Since(tf))
	r.Scored = len(in)

	tl := time.Now()
	score, chispaMS, err := g.ScoreLines(ctx, o.LinesTask, len(lg.Lines), in, o.Workers)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", o.LinesTask, err)
	}
	r.Timing.Lines, r.Timing.LinesIn = ms(time.Since(tl)), chispaMS
	r.Scores = score

	tc := time.Now()
	cs := Locate(lg, score, o.Chunk)
	for _, c := range cs {
		r.Chunks = append(r.Chunks, ChunkOut{From: c.Start + 1, To: c.End + 1, Score: round3(c.Score)})
	}
	r.Chunk = ChunkText(lg, cs, MaxChunkBytes)
	r.Timing.Locate = ms(time.Since(tc))

	tk := time.Now()
	c, err := g.Classify(ctx, o.CategoryTask, r.Chunk, CategoryFields(lg, cs))
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", o.CategoryTask, err)
	}
	r.Timing.Category = ms(time.Since(tk))
	r.Category, r.Prob, r.Confident, r.Layer = c.Label, round3(c.Prob), !c.Escalate, "chispa"
	if c.Chispa != nil {
		r.Candidates = c.Chispa.Candidates
	}
	if c.Escalate {
		r.Layer = "chispa-unsure"
	}

	ask := o.SummaryTask != "" && r.Chunk != "" && (o.VON == "always" || o.VON == "escalated" && c.Escalate)
	if ask {
		tv := time.Now()
		ans, err := AskVON(ctx, g, o.SummaryTask, r.Chunk, c)
		r.Timing.VON = ms(time.Since(tv))
		if err != nil {
			// Sin VON la respuesta sigue valiendo: la categoría de Chispa,
			// marcada como insegura. Un LLM caído no tumba el triaje.
			r.VONError = truncUTF8(err.Error(), 300)
		} else {
			r.VON = ans
			if c.Escalate {
				r.Category, r.Layer = ans.Category, "von"
			}
		}
	}
	r.Timing.Total = ms(time.Since(t0))
	return r, nil
}

// maxVONText acota lo que se acepta de VON en cada campo: el invitado no es de
// fiar y la página lo pinta.
const maxVONText = 400

// AskVON pregunta a la tarea de generación por la categoría, un resumen y el
// siguiente paso. VON solo ve el trozo y la duda de Chispa, nunca el log.
func AskVON(ctx context.Context, g *Gateway, task, chunk string, c *Classification) (*VONAnswer, error) {
	hint := c.Label
	if c.Chispa != nil && len(c.Chispa.Candidates) > 0 {
		var parts []string
		for _, cp := range c.Chispa.Candidates {
			parts = append(parts, fmt.Sprintf("%s %.2f", cp.Label, cp.Prob))
		}
		hint = strings.Join(parts, ", ")
	}
	gen, err := g.Generate(ctx, task, chunk, map[string]string{"hint": hint})
	if err != nil {
		return nil, err
	}
	var raw struct {
		Category string `json:"category"`
		Summary  string `json:"summary"`
		NextStep string `json:"next_step"`
	}
	if err := json.Unmarshal([]byte(gen.Output), &raw); err != nil {
		return nil, fmt.Errorf("VON did not answer JSON: %w", err)
	}
	ok := false
	for _, k := range Categories {
		ok = ok || raw.Category == k
	}
	if !ok {
		return nil, errors.New("VON answered an unknown category " + truncUTF8(fmt.Sprintf("%q", raw.Category), 60))
	}
	return &VONAnswer{
		Category: raw.Category, Summary: truncUTF8(strings.TrimSpace(raw.Summary), maxVONText),
		NextStep: truncUTF8(strings.TrimSpace(raw.NextStep), maxVONText), Model: gen.Model,
		LatencyMS: gen.LatencyMS, PromptTokens: gen.Usage.PromptTokens, CompletionTokens: gen.Usage.CompletionTokens,
	}, nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func round3(x float64) float64 { return float64(int(x*1000+0.5)) / 1000 }
