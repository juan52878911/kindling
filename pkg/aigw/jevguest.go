package aigw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/juan52878911/kindling/pkg/jev"
)

// TAREAS JEV CON BACKEND "microvm": la misma predicción que en proceso, pero
// servida por una réplica de kling-jev (cmd/kling-jev) en una microVM que
// pkg/scheduler despierta con la primera petición y congela al quedarse
// ociosa —igual que a un VON—. docs/jev-serverless.md mide cuánto cuesta ese
// salto de red frente a los µs de JEV en proceso, y por qué compensa igual
// para muchas tareas distintas (aislamiento por tarea/inquilino, un paquete
// por tarea, coste cero mientras nadie la usa).

// jevGuestRequest es el cuerpo de POST /v1/classify contra kling-jev: el mismo
// esquema que ClassifyRequest usa para hablar con JEV en proceso.
type jevGuestRequest struct {
	Text    string         `json:"text"`
	Fields  map[string]any `json:"fields,omitempty"`
	Explain bool           `json:"explain,omitempty"`
}

// jevGuestResponse es la respuesta de kling-jev. Candidates trae SIEMPRE la
// distribución entera (no un top-N) cuando JEV duda o se pidió explain: así
// esta réplica se comporta exactamente como jev.Model.PredictFull en proceso,
// y el resto de Classify (top_k, la plantilla de la cascada) no tiene que
// saber de dónde vino la predicción.
type jevGuestResponse struct {
	Label      string          `json:"label"`
	Prob       float64         `json:"prob"`
	Threshold  float64         `json:"threshold"`
	Confident  bool            `json:"confident"`
	Decision   string          `json:"decision"`
	Candidates []jev.ClassProb `json:"candidates,omitempty"`
	Evidence   []jev.Evidence  `json:"evidence,omitempty"`
}

// maxJEVGuestAnswerBytes acota la respuesta de una réplica JEV: unas cuantas
// decenas de etiquetas con su evidencia caben de sobra, y un invitado hostil
// no puede hinchar la memoria del gateway.
const maxJEVGuestAnswerBytes = 256 << 10

// classifyGuest pregunta a una réplica del dorado snap (kling jev deploy) y
// devuelve la predicción en la misma forma que jev.Model.Predict/PredictFull,
// para que Classify no tenga que distinguir después de dónde vino.
func (g *Gateway) classifyGuest(ctx context.Context, snap string, in jev.Input, explain bool) (jev.Prediction, error) {
	body, err := json.Marshal(jevGuestRequest{Text: in.Text, Fields: in.Fields, Explain: explain})
	if err != nil {
		return jev.Prediction{}, err
	}
	resp, rep, err := g.postGuest(ctx, snap, "/v1/classify", body)
	if err != nil {
		return jev.Prediction{}, err
	}
	defer rep.Release()
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJEVGuestAnswerBytes+1))
	if err != nil {
		return jev.Prediction{}, err
	}
	if len(b) > maxJEVGuestAnswerBytes {
		return jev.Prediction{}, fmt.Errorf("replica answer larger than %d bytes", maxJEVGuestAnswerBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return jev.Prediction{}, fmt.Errorf("replica answered %d: %s", resp.StatusCode, truncUTF8(string(b), 200))
	}
	var gr jevGuestResponse
	if err := json.Unmarshal(b, &gr); err != nil {
		return jev.Prediction{}, fmt.Errorf("replica answer is not a classify response: %w", err)
	}
	return jev.Prediction{
		Label: gr.Label, Prob: gr.Prob, Threshold: gr.Threshold,
		Confident: gr.Confident, Decision: gr.Decision,
		Probs: gr.Candidates, Evidence: gr.Evidence,
	}, nil
}

// candidateLabels saca las etiquetas de una distribución ya completa (todas
// las del modelo, como la manda una réplica microvm o PredictFull en
// proceso): es lo que escalation usa cuando top_k no acota más.
func candidateLabels(probs []jev.ClassProb) []string {
	out := make([]string, len(probs))
	for i, c := range probs {
		out[i] = c.Label
	}
	return out
}
