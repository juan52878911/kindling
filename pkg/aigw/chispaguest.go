package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/chispa"
)

// TAREAS Chispa CON BACKEND "microvm": la misma predicción que en proceso, pero
// servida por una réplica de kling-chispa (cmd/kling-chispa) en una microVM que
// pkg/scheduler despierta con la primera petición y congela al quedarse
// ociosa —igual que a un VON—. docs/chispa-serverless.md mide cuánto cuesta ese
// salto de red frente a los µs de Chispa en proceso, y por qué compensa igual
// para muchas tareas distintas (aislamiento por tarea/inquilino, un paquete
// por tarea, coste cero mientras nadie la usa).
//
// EL INVITADO NO ES DE FIAR. kling-chispa corre dentro de una microVM que sirvió
// una imagen que alguien construyó (`kling chispa deploy`); nada impide que esa
// imagen esté corrompida, desincronizada con el registro del gateway tras un
// redeploy, o (si algún día una tarea la sirve un tercero) directamente sea
// hostil. Antes esto solo acotaba el TAMAÑO de la respuesta
// (maxChispaGuestAnswerBytes); el CONTENIDO pasaba tal cual a gateway.go, que
// decide "confiado" o "escalar" a partir de Label/Prob/Threshold/Probs. Ahora
// validateGuestReply comprueba cada campo contra el conjunto de etiquetas del
// modelo (chispaDeployLookup, más abajo) antes de que nada de esto llegue a
// Classify: una réplica que miente da un error, nunca una decisión.

// chispaGuestRequest es el cuerpo de POST /v1/classify contra kling-chispa: el mismo
// esquema que ClassifyRequest usa para hablar con Chispa en proceso.
type chispaGuestRequest struct {
	Text    string         `json:"text"`
	Fields  map[string]any `json:"fields,omitempty"`
	Explain bool           `json:"explain,omitempty"`
}

// chispaGuestResponse es la respuesta de kling-chispa. Candidates trae SIEMPRE la
// distribución entera (no un top-N) cuando Chispa duda o se pidió explain: así
// esta réplica se comporta exactamente como chispa.Model.PredictFull en proceso,
// y el resto de Classify (top_k, la plantilla de la cascada) no tiene que
// saber de dónde vino la predicción.
//
// Nada de esto es de fiar hasta que validateGuestReply lo comprueba: el tipo
// solo describe la forma del JSON, no lo que promete.
type chispaGuestResponse struct {
	Label      string             `json:"label"`
	Prob       float64            `json:"prob"`
	Threshold  float64            `json:"threshold"`
	Confident  bool               `json:"confident"`
	Decision   string             `json:"decision"`
	Candidates []chispa.ClassProb `json:"candidates,omitempty"`
	Evidence   []chispa.Evidence  `json:"evidence,omitempty"`
}

// maxChispaGuestAnswerBytes acota la respuesta de una réplica Chispa: unas cuantas
// decenas de etiquetas con su evidencia caben de sobra, y un invitado hostil
// no puede hinchar la memoria del gateway.
const maxChispaGuestAnswerBytes = 256 << 10

// maxGuestEvidence acota cuántas pistas de evidencia acepta el gateway de una
// réplica. En proceso, PredictFull solo pide un puñado (chispaDecide pide 5); un
// invitado hostil no debería poder colar miles de entradas minúsculas dentro
// de los 256 KiB que ya acota maxChispaGuestAnswerBytes.
const maxGuestEvidence = 64

// ChispaDeployAnnotation es la anotación (pkg/api/annotations.go) que
// `kling chispa deploy` graba en el dorado de una tarea backend microvm: las
// etiquetas del .chispa horneado dentro y su sha256. Es la fuente de verdad de
// esas etiquetas para el gateway, que no tiene el .chispa de origen a mano (puede
// vivir en otra máquina, o el daemon estar al otro lado de un SSH) y que, sobre
// todo, no puede fiarse de las que le mande el propio invitado.
const ChispaDeployAnnotation = "chispa.deploy"

// ChispaDeployRecord es el valor de ChispaDeployAnnotation.
type ChispaDeployRecord struct {
	Labels []string `json:"labels"`
	Sha256 string   `json:"sha256"`
}

// chispaDeployLookup consulta ChispaDeployAnnotation de un dorado. La de verdad
// (clientDeployLookup) pregunta al daemon; los tests ponen una de mentira.
type chispaDeployLookup interface {
	chispaLabels(ctx context.Context, snapshot string) (ChispaDeployRecord, error)
}

// clientDeployLookup es chispaDeployLookup sobre el daemon de verdad.
type clientDeployLookup struct{ c *api.Client }

func (d clientDeployLookup) chispaLabels(ctx context.Context, snapshot string) (ChispaDeployRecord, error) {
	if d.c == nil {
		return ChispaDeployRecord{}, errors.New("no daemon client configured")
	}
	snap, err := d.c.Snapshot(ctx, snapshot)
	if err != nil {
		return ChispaDeployRecord{}, err
	}
	var rec ChispaDeployRecord
	ok, err := snap.Annotation(ChispaDeployAnnotation, &rec)
	if err != nil {
		return ChispaDeployRecord{}, fmt.Errorf("decoding %s annotation: %w", ChispaDeployAnnotation, err)
	}
	if !ok || len(rec.Labels) == 0 {
		return ChispaDeployRecord{}, fmt.Errorf(
			"snapshot %q has no %s annotation with labels (deployed before this gateway could pin them; redeploy with kling chispa deploy)",
			snapshot, ChispaDeployAnnotation)
	}
	return rec, nil
}

// deployCacheEntry es una entrada de la caché de deployLabels.
type deployCacheEntry struct {
	rec ChispaDeployRecord
	at  time.Time
}

// deployCacheTTL es cuánto se confía en una entrada de la caché antes de
// volver a preguntar al daemon. El dorado no cambia salvo un redeploy
// (`kling chispa deploy -replace`), así que no hace falta preguntar en cada
// clasificación; este plazo acota cuánto tarda el gateway en enterarse de uno.
const deployCacheTTL = 30 * time.Second

// deployLabels da las etiquetas válidas de un modelo chispa backend microvm,
// cacheadas deployCacheTTL. Es la única fuente de verdad que classifyGuest usa
// para validar lo que manda la réplica.
func (g *Gateway) deployLabels(ctx context.Context, snapshot string) ([]string, error) {
	g.deployMu.Lock()
	if e, ok := g.deployCache[snapshot]; ok && time.Since(e.at) < deployCacheTTL {
		g.deployMu.Unlock()
		return e.rec.Labels, nil
	}
	g.deployMu.Unlock()

	rec, err := g.deploy.chispaLabels(ctx, snapshot)
	if err != nil {
		return nil, &deployLookupError{err}
	}

	g.deployMu.Lock()
	if g.deployCache == nil {
		g.deployCache = map[string]deployCacheEntry{}
	}
	g.deployCache[snapshot] = deployCacheEntry{rec: rec, at: time.Now()}
	g.deployMu.Unlock()
	return rec.Labels, nil
}

// deployLookupError es no haber podido conseguir el registro de despliegue
// (ChispaDeployAnnotation): sin él, el gateway no tiene con qué validar lo que
// mande la réplica, así que la tarea falla como si no hubiera réplica
// disponible (503), no como una respuesta inválida (502, guestInvalidError).
type deployLookupError struct{ err error }

func (e *deployLookupError) Error() string { return "deploy record: " + e.err.Error() }
func (e *deployLookupError) Unwrap() error { return e.err }

// guestInvalidError marca una respuesta de réplica que SÍ llegó pero no pasó
// validateGuestReply: a diferencia de un fallo de red o de despertar
// (wakeError) o de no encontrar el registro de despliegue
// (deployLookupError), aquí el invitado contestó con algo que no es de fiar
// —una etiqueta que no existe, una probabilidad fuera de rango, más
// candidatos de los que el modelo tiene—. gateway.go lo trata como 502 (bad
// gateway): reintentar no arregla una réplica que manda datos inválidos.
type guestInvalidError struct{ err error }

func (e *guestInvalidError) Error() string { return "invalid reply: " + e.err.Error() }
func (e *guestInvalidError) Unwrap() error { return e.err }

// finite01 dice si f es un número (no NaN/Inf) en [0,1]: el rango válido de
// toda probabilidad que Chispa calcula, calibrada o no.
func finite01(f float64) bool { return finiteFloat(f) && f >= 0 && f <= 1 }

// finiteFloat dice si f no es NaN ni ±Inf.
func finiteFloat(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// validateGuestReply comprueba TODOS los campos de gr contra labels —las
// etiquetas del registro de despliegue, nunca las que la propia réplica
// declare— y devuelve la predicción ya limpia, o un error que describe la
// violación concreta. El invitado no es de fiar (ver el comentario de
// paquete arriba): nada de gr pasa a gateway.go sin pasar por aquí.
func validateGuestReply(labels []string, gr chispaGuestResponse) (chispa.Prediction, error) {
	if len(labels) == 0 {
		return chispa.Prediction{}, errors.New("no label set to validate against")
	}
	if len(labels) > chispa.MaxLabels {
		return chispa.Prediction{}, fmt.Errorf("deploy record has %d labels, more than %d", len(labels), chispa.MaxLabels)
	}
	allowed := make(map[string]bool, len(labels))
	for _, l := range labels {
		allowed[l] = true
	}

	if gr.Label == "" || !allowed[gr.Label] {
		return chispa.Prediction{}, fmt.Errorf("unknown label %q", truncUTF8(gr.Label, 128))
	}
	if !finite01(gr.Prob) {
		return chispa.Prediction{}, fmt.Errorf("prob %v is not finite in [0,1]", gr.Prob)
	}
	// El umbral de una clase puede llegar a chispa.NeverConfident (2.0, "nunca
	// confiado": ver pkg/chispa.Model.Init y TaskConfig.Thresholds en config.go,
	// que aceptan el mismo rango), no solo [0,1] como una probabilidad: un
	// umbral de 1 le seguiría prohibiendo a esa clase el escalón que promete.
	if !finiteFloat(gr.Threshold) || gr.Threshold < 0 || gr.Threshold > chispa.NeverConfident {
		return chispa.Prediction{}, fmt.Errorf("threshold %v is not finite in [0,%v]", gr.Threshold, chispa.NeverConfident)
	}
	switch gr.Decision {
	case "", chispa.DecisionConfident, chispa.DecisionEscalate:
	default:
		return chispa.Prediction{}, fmt.Errorf("unknown decision %q", truncUTF8(gr.Decision, 64))
	}

	if len(gr.Candidates) > len(labels) {
		return chispa.Prediction{}, fmt.Errorf("%d candidates for a %d-label model", len(gr.Candidates), len(labels))
	}
	seen := make(map[string]bool, len(gr.Candidates))
	for _, c := range gr.Candidates {
		if c.Label == "" || !allowed[c.Label] {
			return chispa.Prediction{}, fmt.Errorf("candidate with unknown label %q", truncUTF8(c.Label, 128))
		}
		if seen[c.Label] {
			return chispa.Prediction{}, fmt.Errorf("duplicate candidate label %q", c.Label)
		}
		seen[c.Label] = true
		if !finite01(c.Prob) {
			return chispa.Prediction{}, fmt.Errorf("candidate %q: prob %v is not finite in [0,1]", c.Label, c.Prob)
		}
	}

	if len(gr.Evidence) > maxGuestEvidence {
		return chispa.Prediction{}, fmt.Errorf("%d evidence entries, more than %d", len(gr.Evidence), maxGuestEvidence)
	}
	for _, e := range gr.Evidence {
		if e.Feature == "" || len(e.Feature) > chispa.MaxLabelBytes || !finiteFloat(e.Weight) {
			return chispa.Prediction{}, fmt.Errorf("invalid evidence entry %q", truncUTF8(e.Feature, 64))
		}
	}

	// Confident y Decision NO se copian: gateway.go los recalcula siempre del
	// prob ya validado y el umbral del lado del gateway (Classify, backend
	// microvm), así que ni siquiera queda un booleano del invitado por ahí
	// para que un futuro cambio se fíe de él por descuido.
	return chispa.Prediction{
		Label: gr.Label, Prob: gr.Prob, Threshold: gr.Threshold,
		Probs: gr.Candidates, Evidence: gr.Evidence,
	}, nil
}

// classifyGuest pregunta a una réplica del dorado snap (kling chispa deploy),
// valida su respuesta contra el registro de despliegue y devuelve la
// predicción en la misma forma que chispa.Model.Predict/PredictFull, más las
// etiquetas válidas del modelo (para escalar), para que Classify no tenga que
// distinguir después de dónde vino ni volver a mirar el registro.
func (g *Gateway) classifyGuest(ctx context.Context, snap string, in chispa.Input, explain bool) (chispa.Prediction, []string, error) {
	labels, err := g.deployLabels(ctx, snap)
	if err != nil {
		return chispa.Prediction{}, nil, err
	}

	body, err := json.Marshal(chispaGuestRequest{Text: in.Text, Fields: in.Fields, Explain: explain})
	if err != nil {
		return chispa.Prediction{}, nil, err
	}
	resp, rep, err := g.postGuest(ctx, snap, "/v1/classify", body)
	if err != nil {
		return chispa.Prediction{}, nil, err
	}
	defer rep.Release()
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxChispaGuestAnswerBytes+1))
	if err != nil {
		return chispa.Prediction{}, nil, err
	}
	if len(b) > maxChispaGuestAnswerBytes {
		return chispa.Prediction{}, nil, fmt.Errorf("replica answer larger than %d bytes", maxChispaGuestAnswerBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return chispa.Prediction{}, nil, fmt.Errorf("replica answered %d: %s", resp.StatusCode, truncUTF8(string(b), 200))
	}
	var gr chispaGuestResponse
	if err := json.Unmarshal(b, &gr); err != nil {
		return chispa.Prediction{}, nil, fmt.Errorf("replica answer is not a classify response: %w", err)
	}
	p, err := validateGuestReply(labels, gr)
	if err != nil {
		return chispa.Prediction{}, nil, &guestInvalidError{err}
	}
	return p, labels, nil
}
