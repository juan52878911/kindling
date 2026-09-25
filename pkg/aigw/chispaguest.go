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
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
	"github.com/juan52878911/kindling/pkg/scheduler"
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
	// Slots son los huecos, si el dorado se desplegó con -slots.
	Slots []slots.Span `json:"slots,omitempty"`
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
	// Slots son los huecos del .chispas horneado con -slots (vacío: la
	// réplica no lleva modelo de huecos y no puede mandar ninguno), y
	// SlotsSha256 su sha256.
	Slots       []string `json:"slots,omitempty"`
	SlotsSha256 string   `json:"slots_sha256,omitempty"`
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

// deployRecord da el registro de despliegue de un modelo chispa backend
// microvm (etiquetas y huecos válidos), cacheado deployCacheTTL. Es la única
// fuente de verdad que askChispaGuest usa para validar lo que manda la réplica.
func (g *Gateway) deployRecord(ctx context.Context, snapshot string) (ChispaDeployRecord, error) {
	g.deployMu.Lock()
	if e, ok := g.deployCache[snapshot]; ok && time.Since(e.at) < deployCacheTTL {
		g.deployMu.Unlock()
		return e.rec, nil
	}
	g.deployMu.Unlock()

	rec, err := g.deploy.chispaLabels(ctx, snapshot)
	if err != nil {
		return ChispaDeployRecord{}, &deployLookupError{err}
	}

	g.deployMu.Lock()
	if g.deployCache == nil {
		g.deployCache = map[string]deployCacheEntry{}
	}
	g.deployCache[snapshot] = deployCacheEntry{rec: rec, at: time.Now()}
	g.deployMu.Unlock()
	return rec, nil
}

// deployLabels son las etiquetas válidas del registro de despliegue.
func (g *Gateway) deployLabels(ctx context.Context, snapshot string) ([]string, error) {
	rec, err := g.deployRecord(ctx, snapshot)
	return rec.Labels, err
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

// guestErrReason clasifica un fallo de una réplica de Chispa: el motivo para
// las métricas, el código HTTP y cómo decirlo.
func guestErrReason(err error) (reason string, code int, verb string) {
	var we *wakeError
	var dle *deployLookupError
	var gie *guestInvalidError
	reason, code, verb = "request", http.StatusServiceUnavailable, "unavailable"
	switch {
	case errors.As(err, &we):
		reason = "wake"
	case errors.As(err, &dle):
		reason = "labels"
	case errors.As(err, &gie):
		// El invitado SÍ contestó, pero con algo que no es de fiar: no es que
		// no haya réplica, es que mintió o se desincronizó con el registro de
		// despliegue. 502, no 503: reintentar no arregla una respuesta que no
		// pasa validación.
		reason, code, verb = "invalid", http.StatusBadGateway, "sent an invalid answer"
	}
	return reason, code, verb
}

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

// maxGuestSpans acota los huecos que acepta el gateway de una réplica: una
// orden tiene unos pocos, y un invitado hostil no debería poder mandar miles.
const maxGuestSpans = 64

// validateGuestSpans comprueba los huecos de una réplica contra los del
// registro de despliegue (allowed, no vacío) y el texto que se le mandó:
// nombre conocido, posiciones dentro del texto y en orden. El Text de cada
// hueco se rehace del texto de la petición: el del invitado no se usa.
func validateGuestSpans(allowed []string, text string, sp []slots.Span) ([]slots.Span, error) {
	if len(sp) == 0 {
		return nil, nil
	}
	if len(sp) > maxGuestSpans {
		return nil, fmt.Errorf("%d slots, more than %d", len(sp), maxGuestSpans)
	}
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	out := make([]slots.Span, len(sp))
	prev := 0
	for i, s := range sp {
		if !ok[s.Slot] {
			return nil, fmt.Errorf("unknown slot %q", truncUTF8(s.Slot, 64))
		}
		if s.Start < prev || s.End <= s.Start || s.End > len(text) {
			return nil, fmt.Errorf("slot %q at [%d,%d) is outside the text or out of order", s.Slot, s.Start, s.End)
		}
		// Los límites vienen del invitado y son bytes: uno que caiga a mitad de
		// un carácter multibyte daría un hueco con texto roto ("sal\xc3").
		if !utf8.RuneStart(text[s.Start]) || (s.End < len(text) && !utf8.RuneStart(text[s.End])) {
			return nil, fmt.Errorf("slot %q at [%d,%d) splits a UTF-8 character", s.Slot, s.Start, s.End)
		}
		prev = s.End
		out[i] = slots.Span{Slot: s.Slot, Start: s.Start, End: s.End, Text: text[s.Start:s.End]}
	}
	return out, nil
}

// guestAnswer es lo que contestó una réplica de Chispa, ya validado.
type guestAnswer struct {
	Pred   chispa.Prediction
	Labels []string
	// HasSlots: el dorado lleva modelo de huecos; Spans son los suyos.
	HasSlots bool
	Spans    []slots.Span
	// Wake es el despertar que pagó esta petición (nil: ya estaba despierta)
	// y Request lo que tardó la petición a la réplica ya lista.
	Wake    *scheduler.WakeTrace
	Request time.Duration
}

// guestConfident decide del lado del gateway si una predicción de réplica es
// confiada: el umbral de la clase (el del modelo, o el de la tarea si lo
// sobrescribe) contra la probabilidad ya validada. El "confident" que mande
// el invitado no se mira nunca.
func guestConfident(p chispa.Prediction, thresholds map[string]float64) (tau float64, confident bool) {
	tau = p.Threshold
	if v, ok := thresholds[p.Label]; ok {
		tau = v
	}
	return tau, p.Prob >= tau
}

// classifyGuest pregunta a una réplica del dorado snap (kling chispa deploy),
// valida su respuesta contra el registro de despliegue y devuelve la
// predicción en la misma forma que chispa.Model.Predict/PredictFull, más las
// etiquetas válidas del modelo (para escalar), para que Classify no tenga que
// distinguir después de dónde vino ni volver a mirar el registro.
func (g *Gateway) classifyGuest(ctx context.Context, snap string, in chispa.Input, explain bool) (chispa.Prediction, []string, error) {
	a, err := g.askChispaGuest(ctx, snap, in, explain)
	return a.Pred, a.Labels, err
}

// askChispaGuest es classifyGuest con todo lo que trae la réplica: también
// sus huecos (validados) y cómo estaba (el despertar que pagó la petición).
// Es el camino de /v1/classify y de la capa 2 de /v1/decide.
func (g *Gateway) askChispaGuest(ctx context.Context, snap string, in chispa.Input, explain bool) (guestAnswer, error) {
	rec, err := g.deployRecord(ctx, snap)
	if err != nil {
		return guestAnswer{}, err
	}

	body, err := json.Marshal(chispaGuestRequest{Text: in.Text, Fields: in.Fields, Explain: explain})
	if err != nil {
		return guestAnswer{}, err
	}
	t0 := time.Now()
	resp, rep, err := g.postGuest(ctx, snap, "/v1/classify", body)
	if err != nil {
		return guestAnswer{}, err
	}
	defer rep.Release()
	defer resp.Body.Close()
	a := guestAnswer{Wake: rep.Wake, HasSlots: len(rec.Slots) > 0}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxChispaGuestAnswerBytes+1))
	a.Request = time.Since(t0)
	if rep.Wake != nil {
		// Lo que tardó despertarla va aparte: aquí solo la petición.
		a.Request -= rep.Wake.Total
		if a.Request < 0 {
			a.Request = 0
		}
	}
	if err != nil {
		return a, err
	}
	if len(b) > maxChispaGuestAnswerBytes {
		return a, fmt.Errorf("replica answer larger than %d bytes", maxChispaGuestAnswerBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return a, fmt.Errorf("replica answered %d: %s", resp.StatusCode, truncUTF8(string(b), 200))
	}
	var gr chispaGuestResponse
	if err := json.Unmarshal(b, &gr); err != nil {
		return a, fmt.Errorf("replica answer is not a classify response: %w", err)
	}
	p, err := validateGuestReply(rec.Labels, gr)
	if err != nil {
		return a, &guestInvalidError{err}
	}
	// Sin huecos en el registro (desplegado sin -slots, o antes de que el
	// registro los guardara) lo que mande la réplica no tiene con qué
	// validarse: se ignora, como si no los hubiera.
	if a.HasSlots {
		if a.Spans, err = validateGuestSpans(rec.Slots, in.Text, gr.Slots); err != nil {
			return a, &guestInvalidError{err}
		}
	}
	a.Pred, a.Labels = p, rec.Labels
	return a, nil
}
