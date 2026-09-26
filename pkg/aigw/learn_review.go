package aigw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// QUÉ ENTRA EN UN REENTRENO, Y QUÉ NECESITA OJOS HUMANOS.
//
// Cada caso capturado acaba en uno de cuatro sitios:
//
//   - etiqueta humana (confirmar o corregir): verdad, peso 1, siempre entra;
//   - descartado por una persona: no entra nunca;
//   - aceptado sin revisión: solo si pasa el FILTRO DE ACUERDO con maestros
//     VALIDADOS (abajo), con peso teacher_weight y un tope por clase;
//   - pendiente: lo demás, que `kling ai review` enseña a una persona.
//
// La cola mezcla dos cosas a propósito. Una parte es lo más informativo para
// entrenar (maestros que discrepan, una etiqueta que Chispa ni consideraba).
// La otra es una AUDITORÍA al azar: un 20 % fijo de las capturas, elegido por
// el hash del texto (independiente de todo lo demás), que solo se revisa por
// este camino y en orden de hash. Solo la auditoría mide a los maestros: si
// se midieran en los casos elegidos por discrepar, cada maestro parecería peor
// de lo que es (una discrepancia entre dos maestros tiene siempre uno que se
// equivoca). Medido en la simulación de docs/mejora-continua.md: con la
// muestra sesgada, un maestro del 20 % de error entraba y salía de la
// validación de una ronda a otra.
//
// Un maestro está validado si (a) un registro de `kling ai eval` de la tarea
// dice que su capa gana a Chispa sobre etiquetas de verdad (la cascada con ese
// VON, o la capa del codificador en una tarea de intención), o (b) en los casos auditados
// con su voto acierta más que Chispa (McNemar de una cola, p < 0,05, con al
// menos min_checks casos) Y lo que el filtro de acuerdo habría aceptado con su
// voto acierta al menos accept_precision (estimado como aciertos/(n+1), con al
// menos 10). Las dos cosas se miden contra la persona, nunca contra otro
// maestro: la lección de `kling ai calibrate`.

// learnCase es un caso con todo lo que se sabe de él.
type learnCase struct {
	ID        string
	Kind      string
	TS        time.Time
	Text      string
	TextSHA   string
	Fields    map[string]any
	Chispa    CaptureChispa
	HasChispa bool
	Votes     []TeacherVote
	Human     string // etiqueta humana ("" = ninguna)
	Discarded bool
	Captured  bool // false = llegó solo por /v1/feedback, con su texto
}

// auditPct es el tanto por ciento de capturas que forman la auditoría.
const auditPct = 20

// auditKey es la posición de un caso en la auditoría (su hash) y si está en
// ella: solo las capturas, por el sha256 de su texto, que no depende de nada
// que el bucle mire.
func auditKey(c *learnCase) (uint64, bool) {
	if !c.Captured || len(c.TextSHA) < 16 {
		return 0, false
	}
	h, err := strconv.ParseUint(c.TextSHA[:16], 16, 64)
	if err != nil {
		return 0, false
	}
	return h, h%100 < auditPct
}

func textKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// buildCases junta capturas y feedback. Dos capturas del mismo texto son un
// caso (se suman sus votos); lo último que dice una persona de un caso manda.
// predict rellena lo que dice Chispa de los casos que no lo traen (los que
// llegaron por feedback con su texto); puede ser nil.
func buildCases(caps []CaptureRecord, fb []FeedbackRecord, predict func(text string, fields map[string]any) (CaptureChispa, bool)) []*learnCase {
	var out []*learnCase
	byID := map[string]*learnCase{}
	byText := map[string]*learnCase{}
	for _, r := range caps {
		if c := byText[r.TextSHA256]; c != nil && r.TextSHA256 != "" {
			c.Votes = append(c.Votes, r.Teachers...)
			byID[r.ID] = c
			continue
		}
		c := &learnCase{ID: r.ID, Kind: r.Kind, TS: r.TS, Text: r.Text, TextSHA: r.TextSHA256, Fields: r.Fields,
			Chispa: r.Chispa, HasChispa: r.Chispa.Label != "", Votes: append([]TeacherVote(nil), r.Teachers...), Captured: true}
		out = append(out, c)
		byID[r.ID] = c
		if r.TextSHA256 != "" {
			byText[r.TextSHA256] = c
		}
	}
	for _, f := range fb {
		c := byID[f.ID]
		if c == nil && f.Text != "" {
			k := textKey(f.Text)
			if c = byText[k]; c == nil {
				id := f.ID
				if id == "" {
					id = "fb-" + k[:12]
				}
				c = &learnCase{ID: id, Kind: "feedback", TS: f.TS, Text: f.Text, TextSHA: k, Fields: f.Fields}
				out = append(out, c)
				byText[k] = c
			}
			if f.ID != "" {
				byID[f.ID] = c
			}
		}
		if c == nil {
			continue // un id que no se capturó y sin texto: nada que aprender
		}
		if c.Text == "" && f.Text != "" {
			// Captura en modo hash: la persona trae el texto.
			c.Text, c.Fields = f.Text, f.Fields
		}
		switch f.Source {
		case "teacher":
			c.Votes = append(c.Votes, TeacherVote{Name: f.Teacher, Label: f.Label, Conf: f.Conf})
		case "human":
			if f.Action == "discard" {
				c.Discarded, c.Human = true, ""
			} else {
				c.Discarded, c.Human = false, f.Label
			}
		}
	}
	if predict != nil {
		for _, c := range out {
			if !c.HasChispa && c.Text != "" {
				c.Chispa, c.HasChispa = predict(c.Text, c.Fields)
			}
		}
	}
	return out
}

// Motivos por los que un caso no entra solo.
const (
	reasonNoTeacher   = "no teacher answer"
	reasonDisagree    = "teachers disagree"
	reasonFewVotes    = "not enough agreeing votes"
	reasonNotTopK     = "teacher label not in Chispa's top-k"
	reasonUnknownLbl  = "label unknown to the model"
	reasonNoText      = "hash only: no text to learn from"
	reasonNotVerified = "teacher not validated"
)

// agreement aplica el filtro de acuerdo a un caso: todos los votos (de
// cualquier maestro) dicen lo mismo, al menos MinVotes vienen de maestros que
// cuentan, la etiqueta es del modelo y está en el top-k de Chispa. counts dice
// qué maestros cuentan (nil = todos: «¿pasaría si estuvieran validados?»).
func agreement(c *learnCase, lc LearnConfig, labels map[string]bool, counts func(string) bool) (string, bool, string) {
	if c.Text == "" {
		return "", false, reasonNoText
	}
	if len(c.Votes) == 0 {
		return "", false, reasonNoTeacher
	}
	label := c.Votes[0].Label
	n := 0
	anyCounted := false
	for _, v := range c.Votes {
		if v.Label != label {
			return majority(c.Votes), false, reasonDisagree
		}
		if counts == nil || counts(v.Name) {
			n++
			anyCounted = true
		}
	}
	if !labels[label] {
		return label, false, reasonUnknownLbl
	}
	if !inTopK(c, label, lc.TopK) {
		return label, false, reasonNotTopK
	}
	if n < lc.MinVotes {
		if !anyCounted {
			return label, false, reasonNotVerified
		}
		return label, false, reasonFewVotes
	}
	return label, true, ""
}

func inTopK(c *learnCase, label string, k int) bool {
	if !c.HasChispa {
		return false
	}
	for i, cp := range c.Chispa.Top {
		if i >= k {
			break
		}
		if cp.Label == label {
			return true
		}
	}
	return false
}

// majority es la etiqueta más votada (empate: la primera que llegó).
func majority(vs []TeacherVote) string {
	n := map[string]int{}
	best := ""
	for _, v := range vs {
		n[v.Label]++
		if best == "" || n[v.Label] > n[best] {
			best = v.Label
		}
	}
	return best
}

// TeacherTrust es la validación de un maestro en una tarea.
type TeacherTrust struct {
	Name      string `json:"name"`
	Validated bool   `json:"validated"`
	// Source: "reviews" (casos revisados por personas), "eval" (un registro
	// de kling ai eval que lo respalda), "forced" (trust_teachers) o "".
	Source string `json:"source,omitempty"`
	// En los casos que una persona revisó y en los que votó: cuántos, cuántos
	// acertó él y cuántos Chispa, y sus discrepancias.
	Checks       int     `json:"checks"`
	TeacherRight int     `json:"teacher_right"`
	ChispaRight  int     `json:"chispa_right"`
	TeacherOnly  int     `json:"teacher_only_right"`
	ChispaOnly   int     `json:"chispa_only_right"`
	PValue       float64 `json:"p_value"`
	// Lo que el filtro de acuerdo habría aceptado con su voto, revisado.
	AcceptChecks    int     `json:"accept_checks"`
	AcceptRight     int     `json:"accept_right"`
	AcceptPrecision float64 `json:"accept_precision"`
	Reason          string  `json:"reason"`
}

// minAcceptChecks son las comprobaciones al azar de lo aceptable que hacen
// falta para creerse su precisión.
const minAcceptChecks = 10

// validateTeachers valida cada maestro que aparece en los votos. evalOK son
// los maestros que un registro de evaluación respalda; trust, los forzados.
func validateTeachers(cases []*learnCase, lc LearnConfig, labels map[string]bool, evalOK map[string]string, trust []string) []TeacherTrust {
	forced := map[string]bool{}
	for _, t := range trust {
		forced[t] = true
	}
	stats := map[string]*TeacherTrust{}
	get := func(n string) *TeacherTrust {
		if stats[n] == nil {
			stats[n] = &TeacherTrust{Name: n}
		}
		return stats[n]
	}
	for n := range evalOK {
		get(n)
	}
	for n := range forced {
		get(n)
	}
	for _, c := range cases {
		for _, v := range c.Votes {
			get(v.Name)
		}
		if _, audit := auditKey(c); c.Human == "" || !c.HasChispa || !audit {
			continue
		}
		label, pass, _ := agreement(c, lc, labels, nil)
		seen := map[string]bool{}
		for _, v := range c.Votes {
			if seen[v.Name] {
				continue
			}
			seen[v.Name] = true
			var mine []TeacherVote
			for _, w := range c.Votes {
				if w.Name == v.Name {
					mine = append(mine, w)
				}
			}
			t := get(v.Name)
			t.Checks++
			tr, cr := majority(mine) == c.Human, c.Chispa.Label == c.Human
			if tr {
				t.TeacherRight++
			}
			if cr {
				t.ChispaRight++
			}
			switch {
			case tr && !cr:
				t.TeacherOnly++
			case cr && !tr:
				t.ChispaOnly++
			}
			if pass {
				t.AcceptChecks++
				if label == c.Human {
					t.AcceptRight++
				}
			}
		}
	}
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]TeacherTrust, 0, len(names))
	for _, n := range names {
		t := stats[n]
		t.PValue = mcnemarOneSided(t.ChispaOnly, t.TeacherOnly)
		if t.AcceptChecks > 0 {
			t.AcceptPrecision = ratio(t.AcceptRight, t.AcceptChecks+1)
		}
		byReviews := t.Checks >= lc.MinChecks && t.TeacherOnly > t.ChispaOnly && t.PValue < 0.05 &&
			t.AcceptChecks >= minAcceptChecks && t.AcceptPrecision >= lc.AcceptPrecision
		var why string
		switch {
		case byReviews:
			why = fmt.Sprintf("beats Chispa on %d audited cases (%d vs %d, McNemar p=%.2g); what the agreement filter accepts is right %.3f (%d checks)",
				t.Checks, t.TeacherOnly, t.ChispaOnly, t.PValue, t.AcceptPrecision, t.AcceptChecks)
		case t.Checks < lc.MinChecks:
			why = fmt.Sprintf("only %d audited cases with its answer (need %d): review more with kling ai review", t.Checks, lc.MinChecks)
		case !(t.TeacherOnly > t.ChispaOnly && t.PValue < 0.05):
			why = fmt.Sprintf("does not beat Chispa on the audited cases (right %d vs Chispa %d; %d vs %d where they differ, McNemar p=%.2g)",
				t.TeacherRight, t.ChispaRight, t.TeacherOnly, t.ChispaOnly, t.PValue)
		case t.AcceptChecks < minAcceptChecks:
			why = fmt.Sprintf("only %d audited cases of what it would add (need %d)", t.AcceptChecks, minAcceptChecks)
		default:
			why = fmt.Sprintf("what the agreement filter would accept from it is right only %.3f (need %.2f)", t.AcceptPrecision, lc.AcceptPrecision)
		}
		switch {
		case forced[n]:
			// Forzado se usa igual, pero el informe dice lo que las revisiones
			// opinan de él: forzar no es a ciegas.
			t.Validated, t.Source = true, "forced"
			if !byReviews {
				t.Reason = "forced (trust_teachers) without validation; reviews say: " + why
			} else {
				t.Reason = "forced (trust_teachers); reviews would validate it anyway: " + why
			}
		case evalOK[n] != "":
			t.Validated, t.Source = true, "eval"
			t.Reason = "backed by the task's eval record: " + evalOK[n]
		case byReviews:
			t.Validated, t.Source, t.Reason = true, "reviews", why
		default:
			t.Reason = why
		}
		out = append(out, *t)
	}
	return out
}

// selection es lo que un reentreno saca de los casos.
type selection struct {
	Human     []chispa.Example
	Accepted  []chispa.Example
	Pending   int
	Discarded int
	Capped    int
	Reasons   map[string]int
}

// selectCases reparte los casos. gold son las cuentas por clase del oro, para
// el tope por clase.
func selectCases(cases []*learnCase, lc LearnConfig, labels map[string]bool, validated map[string]bool, gold map[string]int) selection {
	s := selection{Reasons: map[string]int{}}
	byLabel := map[string][]*learnCase{}
	for _, c := range cases {
		switch {
		case c.Discarded:
			s.Discarded++
		case c.Human != "" && c.Text != "":
			s.Human = append(s.Human, chispa.Example{Text: c.Text, Label: c.Human, Fields: c.Fields})
		case c.Human != "":
			s.Reasons[reasonNoText]++
		default:
			label, ok, why := agreement(c, lc, labels, func(n string) bool { return validated[n] })
			if !ok {
				s.Pending++
				s.Reasons[why]++
				continue
			}
			byLabel[label] = append(byLabel[label], c)
		}
	}
	for _, l := range sortedKeys(byLabel) {
		cs := byLabel[l]
		limit := lc.MaxPerClass
		if limit == 0 {
			limit = max(50, gold[l])
		}
		if len(cs) > limit {
			// Lo más reciente se parece más al tráfico de mañana.
			sort.SliceStable(cs, func(i, j int) bool { return cs[i].TS.After(cs[j].TS) })
			s.Capped += len(cs) - limit
			cs = cs[:limit]
		}
		for _, c := range cs {
			s.Accepted = append(s.Accepted, chispa.Example{Text: c.Text, Label: l, Fields: c.Fields, Weight: lc.TeacherWeight})
		}
	}
	return s
}

// ---- kling ai review

// ReviewRequest pide la cola de revisión de una tarea.
type ReviewRequest struct {
	Task string `json:"task"`
	N    int    `json:"n,omitempty"` // 20; como mucho 500
}

// ReviewCase es un caso para que una persona lo confirme o corrija.
type ReviewCase struct {
	ID        string         `json:"id"`
	TS        time.Time      `json:"ts"`
	Text      string         `json:"text"`
	Fields    map[string]any `json:"fields,omitempty"`
	Chispa    CaptureChispa  `json:"chispa"`
	Teachers  []TeacherVote  `json:"teachers,omitempty"`
	Proposed  string         `json:"proposed"`
	Reason    string         `json:"reason"`
	Audit     bool           `json:"audit,omitempty"`      // de la auditoría al azar: mide a los maestros
	RareClass bool           `json:"rare_class,omitempty"` // la clase propuesta no tiene umbral (siempre escala)
}

// ReviewResponse es la cola.
type ReviewResponse struct {
	Task string `json:"task"`
	// Pending son los casos que solo entran con una persona; Acceptable, los
	// que entrarían solos con los maestros validados de hoy.
	Pending    int            `json:"pending"`
	Acceptable int            `json:"acceptable"`
	Reviewed   int            `json:"reviewed"`
	Reasons    map[string]int `json:"reasons"`
	Cases      []ReviewCase   `json:"cases"`
	Teachers   []TeacherTrust `json:"teachers"`
}

const maxReviewCases = 500

// reviewPriority ordena lo pendiente: lo más informativo primero.
func reviewPriority(why string) int {
	switch why {
	case reasonDisagree:
		return 0
	case reasonNotTopK:
		return 1
	case reasonNotVerified, reasonFewVotes, reasonUnknownLbl:
		return 2
	default:
		return 3
	}
}

// Review devuelve los casos que más merece la pena que mire una persona.
func (g *Gateway) Review(ctx context.Context, req ReviewRequest) (*ReviewResponse, error) {
	n := req.N
	if n <= 0 {
		n = 20
	}
	if n > maxReviewCases {
		return nil, statusf(http.StatusBadRequest, "n must be 1..%d", maxReviewCases)
	}
	lt, err := g.loadLearnTask(ctx, req.Task, nil, nil)
	if err != nil {
		return nil, err
	}
	resp := &ReviewResponse{Task: req.Task, Reasons: map[string]int{}, Teachers: lt.trust, Cases: []ReviewCase{}}
	type cand struct {
		c    *learnCase
		why  string
		prop string
		rare bool
		pri  int
		key  uint64
	}
	var pend, audit []cand
	for _, c := range lt.cases {
		if c.Human != "" || c.Discarded {
			resp.Reviewed++
			continue
		}
		if c.Text == "" {
			resp.Reasons[reasonNoText]++
			continue
		}
		_, okNow, whyNow := agreement(c, lt.lc, lt.labels, func(n string) bool { return lt.validated[n] })
		label, would, why := agreement(c, lt.lc, lt.labels, nil)
		prop := label
		if prop == "" {
			prop = c.Chispa.Label
		}
		k := cand{c: c, why: why, prop: prop, rare: lt.never[prop], pri: reviewPriority(why)}
		if would {
			k.why, k.pri = "the agreement filter would accept it once its teachers are validated", 4
			if okNow {
				k.why = "accepted without review (validated teachers agree)"
			}
		}
		if okNow {
			resp.Acceptable++
		} else {
			resp.Pending++
			resp.Reasons[whyNow]++
		}
		if h, in := auditKey(c); in {
			k.key, k.why = h, "random audit (measures the teachers): "+k.why
			audit = append(audit, k)
		} else if !okNow {
			pend = append(pend, k)
		}
	}
	sort.SliceStable(pend, func(i, j int) bool {
		a, b := pend[i], pend[j]
		if a.pri != b.pri {
			return a.pri < b.pri
		}
		if a.rare != b.rare {
			return a.rare
		}
		if a.c.Chispa.Prob != b.c.Chispa.Prob {
			return a.c.Chispa.Prob < b.c.Chispa.Prob
		}
		return a.c.TS.After(b.c.TS)
	})
	// La auditoría va en orden de hash: al azar, pero estable (dos llamadas
	// seguidas enseñan lo mismo hasta que se revise) y sin mirar el contenido.
	sort.SliceStable(audit, func(i, j int) bool { return audit[i].key < audit[j].key })
	nAudit := min(len(audit), max(1, n*2/5))
	if len(pend) < n-nAudit {
		nAudit = min(len(audit), n-len(pend))
	}
	pick := append(append([]cand(nil), audit[:nAudit]...), pend[:min(len(pend), n-nAudit)]...)
	for _, k := range pick {
		_, in := auditKey(k.c)
		resp.Cases = append(resp.Cases, ReviewCase{
			ID: k.c.ID, TS: k.c.TS, Text: k.c.Text, Fields: k.c.Fields, Chispa: k.c.Chispa, Teachers: k.c.Votes,
			Proposed: k.prop, Reason: k.why, Audit: in, RareClass: k.rare,
		})
	}
	g.learn.mu.Lock()
	g.learn.pending[req.Task] = resp.Pending
	g.learn.mu.Unlock()
	return resp, nil
}
