package von

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// CODIFICADORES DE FRASES (kind "embed").
//
// La capa 3 de la cascada de intención (pkg/intent, docs/codificador.md) es un codificador
// de frases: un BERT pequeño que convierte una frase en un vector. Se sirve
// con la misma maquinaria que un VON —el constructor llm, llama-server y un
// dorado congelado ya caliente— y solo cambia cómo arranca llama-server
// (--embeddings, --pooling) y cómo se calienta. Por eso es una entrada del
// catálogo con Kind "embed" y no un constructor aparte.

// KindEmbed marca un codificador de frases en el catálogo, en Spec y en la
// etiqueta LabelKind de sus máquinas.
const KindEmbed = "embed"

// LabelKind es la etiqueta de una máquina VON que dice qué sirve: "embed"
// para un codificador; ausente en los modelos instruct (así los dorados de
// antes siguen igual).
const LabelKind = "von.kind"

// DefaultEmbedCtx es el contexto de un codificador: sus 512 posiciones. Una
// orden son 10–30 tokens, pero llama-server exige que cada entrada quepa
// entera en un lote, así que el lote es el contexto entero.
const DefaultEmbedCtx = 512

// embedCatalog son los codificadores. Ni intfloat ni sentence-transformers
// publican GGUF, y los de terceros no se pueden comprobar, así que kindling
// los convierte con el conversor de llama.cpp de la misma versión que los
// sirve (scripts/encoder-gguf.sh: pesos, conversor y paquetes fijados, salida
// reproducible bit a bit). La conversión se validó contra transformers:
// coseno medio 0,9999 en Q8_0 (1,00000 en F16) sobre 255 órdenes
// (docs/codificador.md).
//
// Licencias leídas en la ficha de cada modelo, en la revisión fijada: e5-small
// es MIT y MiniLM Apache-2.0; las dos permiten usar y redistribuir, también
// con fines comerciales.
var embedCatalog = []Model{
	{
		ID: "multilingual-e5-small", Quant: "q8_0",
		File:   "multilingual-e5-small-q8_0.gguf",
		SHA256: "9a2039af4b03dccd1d7e59a915679bc7997b61714a2b9174f43512ed1b0095c5",
		Size:   132440544, MemMiB: 512, VCPUs: 2, License: "mit",
		LicenseURL: "https://huggingface.co/intfloat/multilingual-e5-small/blob/614241f622f53c4eeff9890bdc4f31cfecc418b3/README.md",
		Kind:       KindEmbed, Pooling: "mean", Prefix: "query: ", Dim: 384,
		Source: "intfloat/multilingual-e5-small", SourceRevision: "614241f622f53c4eeff9890bdc4f31cfecc418b3",
	},
	{
		ID: "paraphrase-multilingual-minilm-l12-v2", Quant: "q8_0",
		File:   "paraphrase-multilingual-minilm-l12-v2-q8_0.gguf",
		SHA256: "865c7909a0ba5e2f0361ce3c900b9a7958d7b4270ef227beb5ace0f8e2c6f757",
		Size:   132440640, MemMiB: 512, VCPUs: 2, License: "apache-2.0",
		LicenseURL: "https://huggingface.co/sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2/blob/e8f8c211226b894fcb81acc59f3b34ba3efd5f42/README.md",
		Kind:       KindEmbed, Pooling: "mean", Dim: 384,
		Source: "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2", SourceRevision: "e8f8c211226b894fcb81acc59f3b34ba3efd5f42",
	},
}

// Los codificadores entran en el catálogo común: Find, IDs y `kling models`
// los tratan como a cualquier otro modelo.
func init() { Catalog = append(Catalog, embedCatalog...) }

var poolings = map[string]bool{"mean": true, "cls": true, "last": true}

// resolveEmbed completa lo propio de un codificador en un Spec ya resuelto.
func resolveEmbed(r *Resolved, s Spec) error {
	if r.Model != nil {
		if s.Kind != "" && s.Kind != r.Model.Kind || s.Pooling != "" {
			return fmt.Errorf("kind and pooling come from the catalog for %s", r.Ref)
		}
		r.Kind, r.Pooling = r.Model.Kind, r.Model.Pooling
	} else {
		r.Kind, r.Pooling = s.Kind, s.Pooling
	}
	switch r.Kind {
	case "":
		if r.Pooling != "" {
			return fmt.Errorf("pooling only goes with kind %q", KindEmbed)
		}
		return nil
	case KindEmbed:
	default:
		return fmt.Errorf("kind must be empty (instruct) or %q, not %q", KindEmbed, r.Kind)
	}
	if r.Pooling == "" {
		r.Pooling = "mean"
	}
	if !poolings[r.Pooling] {
		return fmt.Errorf("pooling must be mean, cls or last, not %q", r.Pooling)
	}
	if s.Ctx == 0 {
		r.Ctx = DefaultEmbedCtx
	}
	return nil
}

// EmbedArgs son los argumentos de llama-server que añade un codificador:
// --embeddings (sirve /v1/embeddings y no genera), el resumen de la frase, y
// lotes del tamaño del contexto por ranura (un modelo no causal procesa cada
// entrada en un solo lote: más larga que el lote, llama-server la rechaza).
func (r Resolved) EmbedArgs() []string {
	if r.Kind != KindEmbed {
		return nil
	}
	per := strconv.Itoa(r.Ctx / max(r.Parallel, 1))
	return []string{"--embeddings", "--pooling", r.Pooling, "--ubatch-size", per, "--batch-size", strconv.Itoa(r.Ctx)}
}

// EmbedResponse es la respuesta de POST /v1/embeddings.
type EmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// Embed pide los vectores de inputs a una réplica por el proxy del daemon
// (`kling models embed`, el calentamiento). El gateway y el entrenamiento
// hablan con la réplica directamente (pkg/codificador).
func Embed(ctx context.Context, c *api.Client, ref string, inputs []string) (*EmbedResponse, time.Duration, error) {
	body, err := json.Marshal(map[string]any{"input": inputs})
	if err != nil {
		return nil, 0, err
	}
	t0 := time.Now()
	resp, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: Port, Path: "/v1/embeddings", Method: http.MethodPost, Body: string(body),
	})
	dur := time.Since(t0)
	if err != nil {
		return nil, dur, err
	}
	if resp.Status != http.StatusOK {
		return nil, dur, fmt.Errorf("embeddings answered %d: %s", resp.Status, recortar(resp.Body))
	}
	var out EmbedResponse
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return nil, dur, fmt.Errorf("embeddings: %w", err)
	}
	if len(out.Data) != len(inputs) {
		return nil, dur, fmt.Errorf("embeddings: %d vectors for %d inputs", len(out.Data), len(inputs))
	}
	return &out, dur, nil
}

// warm calienta según lo que sirve la máquina. Un codificador se calienta con
// frases de verdad, en los dos idiomas y de largos distintos: la primera
// petición toca las páginas de los pesos y reserva los búferes de cálculo, y
// eso tiene que quedar dentro del dorado.
func warm(ctx context.Context, c *api.Client, ref, kind string) (*ChatResponse, error) {
	if kind != KindEmbed {
		return Warm(ctx, c, ref)
	}
	r, _, err := Embed(ctx, c, ref, []string{
		"open a ticket for the billing team",
		"cancela mi último pedido y devuélveme el dinero, por favor",
		"the invoice from last month looks wrong, can you check what happened with the March payment",
	})
	if err != nil {
		return nil, err
	}
	w := &ChatResponse{Model: r.Model}
	w.Choices = append(w.Choices, struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{Message: Message{Role: "assistant", Content: fmt.Sprintf("%d vectors of %d dimensions", len(r.Data), len(r.Data[0].Embedding))}})
	return w, nil
}
