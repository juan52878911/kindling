package aigw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/train"
)

// El vocabulario de corpus() partido en dos mitades: el modelo inicial solo ve
// las palabras 0-2 de cada clase; el tráfico trae las 3-5, que no conoce. Es
// el caso que el bucle tiene que resolver: Chispa duda, alguien contesta, el
// reentreno aprende y la puerta lo comprueba en el conjunto de confianza.
var learnVocab = map[string][]string{
	"bug":   {"crash", "panic", "null", "overflow", "segfault", "broken"},
	"feat":  {"add", "support", "new", "implement", "introduce", "option"},
	"docs":  {"readme", "typo", "documentation", "guide", "example", "wording"},
	"chore": {"bump", "deps", "release", "version", "lockfile", "cleanup"},
}

var learnLabels = []string{"bug", "chore", "docs", "feat"}

func halfCorpus(n int, seed uint64, lo int) []chispa.Example {
	s := seed
	next := func() uint64 { s = s*6364136223846793005 + 1442695040888963407; return s >> 33 }
	out := make([]chispa.Example, n)
	for i := range out {
		l := learnLabels[next()%4]
		var b strings.Builder
		for j := 0; j < 3; j++ {
			b.WriteString(learnVocab[l][lo+int(next()%3)] + " in module " + string(rune('a'+next()%26)) + " ")
		}
		out[i] = chispa.Example{Text: b.String(), Label: l}
	}
	return out
}

func writeJSONL(t *testing.T, path string, exs []chispa.Example) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, ex := range exs {
		if err := enc.Encode(ex); err != nil {
			t.Fatal(err)
		}
	}
}

type learnFixture struct {
	g       *Gateway
	dir     string
	model   string
	ll      *fakeLlama
	traffic []chispa.Example // palabras 3-5: lo que v1 no sabe
}

// newLearnGateway arma una tarea "kind" con learn, oro, validación y conjunto
// de confianza en disco, y un v1 entrenado solo con la mitad del vocabulario.
func newLearnGateway(t *testing.T, mut func(*Config)) *learnFixture {
	t.Helper()
	dir := t.TempDir()
	gold, valid := halfCorpus(160, 1, 0), halfCorpus(240, 2, 0)
	held := append(halfCorpus(300, 3, 0), halfCorpus(300, 4, 3)...)
	writeJSONL(t, filepath.Join(dir, "gold.jsonl"), gold)
	writeJSONL(t, filepath.Join(dir, "valid.jsonl"), valid)
	writeJSONL(t, filepath.Join(dir, "heldout.jsonl"), held)
	cfg := train.Config{Spec: chispa.DefaultSpec(), Seed: 1, TargetPrecision: 0.9}
	cfg.Spec.Buckets = 1 << 12
	res, err := train.Train(gold, valid, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, "m.chispa")
	if err := res.Model.Save(mp); err != nil {
		t.Fatal(err)
	}
	ll := newFakeLlama(t)
	reps := &fakeReplicas{addr: strings.TrimPrefix(ll.srv.URL, "http://")}
	c := &Config{
		Models: map[string]*ModelConfig{
			"commits": {Kind: KindChispa, Path: mp},
			"smol":    {Kind: KindVON, Snapshot: "von-smol"},
		},
		Tasks: map[string]*TaskConfig{
			"kind": {Chispa: "commits", Learn: &LearnConfig{Capture: CaptureText,
				Gold: filepath.Join(dir, "gold.jsonl"), Valid: filepath.Join(dir, "valid.jsonl"), Heldout: filepath.Join(dir, "heldout.jsonl")}},
		},
	}
	if mut != nil {
		mut(c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: c, Replicas: reps, EvalDir: filepath.Join(dir, "ai-evals"), DataDir: filepath.Join(dir, "ai-data")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close(context.Background()) })
	return &learnFixture{g: g, dir: dir, model: mp, ll: ll, traffic: halfCorpus(400, 5, 3)}
}

// stream manda el tráfico y devuelve id -> etiqueta de verdad de lo escalado.
func (f *learnFixture) stream(t *testing.T, exs []chispa.Example) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ex := range exs {
		r, err := f.g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: ex.Text})
		if err != nil {
			t.Fatal(err)
		}
		if r.ID == "" {
			t.Fatal("a task with learn must answer with an id")
		}
		if r.Escalate {
			out[r.ID] = ex.Label
		}
	}
	f.g.learn.flush()
	return out
}

func TestLearnConfigValidation(t *testing.T) {
	bad := []*Config{
		{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Path: "x"}}, Tasks: map[string]*TaskConfig{"t": {Chispa: "m", Learn: &LearnConfig{Capture: "all"}}}},
		{Models: map[string]*ModelConfig{"v": {Kind: KindVON, Snapshot: "s"}}, Tasks: map[string]*TaskConfig{"t": {VON: "v", Learn: &LearnConfig{}}}},
		{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Path: "x"}}, Tasks: map[string]*TaskConfig{"t": {Chispa: "m", Learn: &LearnConfig{VONVotes: 1}}}},
		{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Path: "x"}}, Tasks: map[string]*TaskConfig{"t": {Chispa: "m", Learn: &LearnConfig{TeacherWeight: 2}}}},
		{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Path: "x"}}, Tasks: map[string]*TaskConfig{"t": {Chispa: "m", Learn: &LearnConfig{TrustTeachers: []string{"Bad Name"}}}}},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("config %d: want an error", i)
		}
	}
	ok := &Config{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Path: "x"}},
		Tasks: map[string]*TaskConfig{"t": {Chispa: "m", Learn: &LearnConfig{Capture: CaptureHash, TrustTeachers: []string{"ext:ci"}}}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Lo escalado se captura con texto filtrado, top-k y versión; lo confiado no.
func TestLearnCapture(t *testing.T) {
	f := newLearnGateway(t, nil)
	conf := halfCorpus(1, 9, 0)[0].Text
	r, err := f.g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: conf})
	if err != nil || r.Escalate {
		t.Fatalf("a known text should be confident: %+v %v", r, err)
	}
	secret := "bump deps token=abcd1234efgh with key sk-ABCDEFGHIJKLMNOPQRSTUV mail juan@example.com"
	for i := 0; i < 2; i++ { // el segundo es un duplicado: no se escribe
		if _, err := f.g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "lockfile cleanup " + secret}); err != nil {
			t.Fatal(err)
		}
	}
	f.g.learn.flush()
	caps, err := f.g.learn.readCaptures("kind", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 {
		t.Fatalf("captures = %d, want 1 (confident answers and duplicates are not captured)", len(caps))
	}
	c := caps[0]
	if strings.Contains(c.Text, "abcd1234") || strings.Contains(c.Text, "sk-ABC") || strings.Contains(c.Text, "juan@") || !strings.Contains(c.Text, "lockfile") {
		t.Errorf("text not redacted: %q", c.Text)
	}
	if c.TextSHA256 == "" || len(c.Chispa.Top) == 0 || c.Chispa.SHA256 == "" || c.Kind != "escalated" {
		t.Errorf("record = %+v", c)
	}
	if n := f.g.learn.counts["kind|dedup"]; n != 1 {
		t.Errorf("dedup count = %d", n)
	}
	st, err := os.Stat(filepath.Join(f.dir, "ai-data", "kind", "captures"))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("captures dir: %v %v", st, err)
	}
}

// Modo hash: nada legible en disco.
func TestLearnCaptureHashOnly(t *testing.T) {
	f := newLearnGateway(t, func(c *Config) { c.Tasks["kind"].Learn.Capture = CaptureHash })
	f.stream(t, f.traffic[:5])
	caps, _ := f.g.learn.readCaptures("kind", 1<<20)
	if len(caps) == 0 {
		t.Fatal("nothing captured")
	}
	for _, c := range caps {
		if c.Text != "" || c.TextSHA256 == "" {
			t.Fatalf("hash mode stored text: %+v", c)
		}
	}
}

// La cascada deja la respuesta de VON como voto, y von_votes suma votos de
// autoconsistencia.
func TestLearnCaptureVONVotes(t *testing.T) {
	f := newLearnGateway(t, func(c *Config) {
		c.Tasks["kind"].EscalateTo, c.Tasks["kind"].EscalateForce = "smol", true
		c.Tasks["kind"].Learn.VONVotes = 2
	})
	f.ll.set("chore")
	f.stream(t, f.traffic[:1])
	deadline := time.Now().Add(5 * time.Second)
	var caps []CaptureRecord
	for time.Now().Before(deadline) {
		f.g.learn.flush()
		caps, _ = f.g.learn.readCaptures("kind", 1<<20)
		if len(caps) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(caps) != 1 || len(caps[0].Teachers) != 3 || caps[0].Teachers[2].Name != "smol" || caps[0].Teachers[0].Label != "chore" {
		t.Fatalf("captures = %+v", caps)
	}
	if b := f.ll.last(); b["temperature"] != 0.4 {
		t.Errorf("extra vote temperature = %v", b["temperature"])
	}
}

func TestLearnFeedback(t *testing.T) {
	f := newLearnGateway(t, nil)
	ids := f.stream(t, f.traffic[:3])
	var id string
	for k := range ids {
		id = k
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", FeedbackItem: FeedbackItem{ID: id, Label: "bug"}}, false, "ci"); err == nil {
		t.Fatal("a tenant token must not speak as a person")
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", FeedbackItem: FeedbackItem{ID: "nope", Label: "bug"}}, true, "default"); err == nil {
		t.Fatal("invalid id accepted")
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", FeedbackItem: FeedbackItem{Label: "bug"}}, true, "default"); err == nil {
		t.Fatal("no id and no text accepted")
	}
	r, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: []FeedbackItem{
		{ID: id, Label: "bug", By: "juan"},
		{ID: id, Teacher: "triage", Label: "docs", Conf: 0.8},
		{Text: "flaky test in the scheduler", Label: "chore"},
	}}, true, "default")
	if err != nil || r.Recorded != 3 {
		t.Fatalf("feedback = %+v %v", r, err)
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", FeedbackItem: FeedbackItem{ID: id, Teacher: "triage", Label: "docs"}}, false, "ci"); err != nil {
		t.Fatal(err)
	}
	fb, _ := f.g.learn.readFeedback("kind")
	if len(fb) != 4 || fb[1].Teacher != "ext:triage" || fb[3].Teacher != "ext:ci.triage" || fb[0].Source != "human" || fb[0].Action != "correct" {
		t.Fatalf("feedback records = %+v", fb)
	}
}

// El bucle entero: capturas + etiquetas humanas -> v2 promocionado, porque
// en el conjunto de confianza contesta bien lo que v1 escalaba; la versión
// sale en las capturas y en /metrics; rollback vuelve a v1 byte a byte.
func TestLearnRetrainPromotesAndRollsBack(t *testing.T) {
	f := newLearnGateway(t, nil)
	v1, _ := fileSHA256(f.model)
	ids := f.stream(t, f.traffic[:200])
	if len(ids) < 100 {
		t.Fatalf("only %d of 200 unknown texts escalated", len(ids))
	}
	var items []FeedbackItem
	for id, l := range ids {
		items = append(items, FeedbackItem{ID: id, Label: l, By: "test"})
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	rep, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Promoted || rep.Version != "v2" || !rep.Gate.Pass || rep.Shadow.Coverage <= rep.Current.Coverage || rep.Data.Human != len(ids) {
		t.Fatalf("retrain = %+v", rep)
	}
	if rep.Current.Version != "v1" || rep.Stored == "" || rep.Backup != f.model+".prev" {
		t.Fatalf("report bookkeeping = %+v", rep)
	}
	if prev, _ := fileSHA256(f.model + ".prev"); prev != v1 {
		t.Error(".prev is not v1")
	}
	// v2 ya sirve: lo que antes escalaba, ahora contesta.
	r, _ := f.g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: f.traffic[300].Text})
	if r.Escalate {
		t.Errorf("v2 still unsure on a text like the ones it learned: %+v", r)
	}
	if st, _ := f.g.learnFor("kind"); st.version != "v2" {
		t.Errorf("served version = %q", st.version)
	}
	rec := do(t, f.g.Handler(""), "GET", "/metrics", "", nil).Body.String()
	for _, want := range []string{`kling_ai_heldout_coverage{task="kind",version="v2"}`, `kling_ai_heldout_coverage{task="kind",version="v1"}`,
		`kling_ai_chispa_version_coverage{task="kind",version="v2"}`, `kling_ai_escalation_rate_window{task="kind"}`,
		`kling_ai_learn_escalations_avoided_estimate{task="kind"}`, `kling_ai_learn_captures_total{task="kind",outcome="written"}`} {
		if !strings.Contains(rec, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	tasks := decode[struct{ Tasks []TaskInfo }](t, do(t, f.g.Handler(""), "GET", "/v1/tasks", "", nil))
	if li := tasks.Tasks[0].Learn; li == nil || li.Version != "v2" || len(li.Versions) != 2 || li.LastRetrain == nil {
		t.Errorf("/v1/tasks learn = %+v", li)
	}

	// Sin nada nuevo, no se entrena.
	rb, err := f.g.Rollback(RollbackRequest{Task: "kind"})
	if err != nil || rb.To != "v1" || rb.From != "v2" {
		t.Fatalf("rollback = %+v %v", rb, err)
	}
	if now, _ := fileSHA256(f.model); now != v1 {
		t.Error("rollback did not restore v1 byte for byte")
	}
	if st, _ := f.g.learnFor("kind"); st.version != "v1" {
		t.Errorf("after rollback serving %q", st.version)
	}
	if _, err := f.g.Rollback(RollbackRequest{Task: "kind", To: "v1"}); err == nil {
		t.Error("rolling back to the served version should fail")
	}
	if _, err := f.g.Rollback(RollbackRequest{Task: "kind", To: "v2"}); err != nil {
		t.Fatal(err)
	}
}

// Un maestro ruidoso forzado (trust_teachers) mete etiquetas malas: la sombra
// es peor en el conjunto de confianza y la puerta la rechaza; lo que se sirve
// no cambia. Y la validación, sin forzar, no lo habría dejado pasar.
func TestLearnGateRejectsNoisyTeacher(t *testing.T) {
	f := newLearnGateway(t, nil)
	before, _ := fileSHA256(f.model)
	ids := f.stream(t, f.traffic[:200])
	wrong := map[string]string{"bug": "chore", "chore": "docs", "docs": "feat", "feat": "bug"}
	var items []FeedbackItem
	for id, l := range ids {
		items = append(items, FeedbackItem{ID: id, Teacher: "a", Label: wrong[l]}, FeedbackItem{ID: id, Teacher: "b", Label: wrong[l]})
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	rep, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Data.Accepted != 0 || !strings.Contains(rep.Gate.Verdict, "nothing new") {
		t.Fatalf("unvalidated teachers must not train: %+v", rep)
	}
	rep, err = f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", Trust: []string{"ext:a", "ext:b"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Data.Accepted == 0 || rep.Gate.Pass || rep.Promoted {
		t.Fatalf("the gate should reject a model taught by a noisy teacher: %+v", rep.Gate)
	}
	if after, _ := fileSHA256(f.model); after != before {
		t.Error("a rejected retrain changed the served model")
	}
	if _, err := os.Stat(rep.Candidate); err != nil {
		t.Errorf("the rejected shadow should stay on disk for inspection: %v", err)
	}
}

// Un maestro que acierta, con suficientes revisiones humanas, se valida solo
// y lo suyo entra sin revisión (con su peso y dentro del tope por clase).
func TestLearnTeacherValidatedByReviews(t *testing.T) {
	f := newLearnGateway(t, nil)
	ids := f.stream(t, f.traffic[:400])
	var items []FeedbackItem
	n := 0
	caps, _ := f.g.learn.readCaptures("kind", 1<<24)
	audited := map[string]bool{}
	for _, c := range caps {
		if _, in := auditKey(&learnCase{Captured: true, TextSHA: c.TextSHA256}); in {
			audited[c.ID] = true
		}
	}
	for id, l := range ids {
		items = append(items, FeedbackItem{ID: id, Teacher: "a", Label: l}, FeedbackItem{ID: id, Teacher: "b", Label: l})
		if audited[id] {
			items = append(items, FeedbackItem{ID: id, Label: l})
			n++
		}
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	rv, err := f.g.Review(context.Background(), ReviewRequest{Task: "kind", N: 10})
	if err != nil {
		t.Fatal(err)
	}
	var a TeacherTrust
	for _, tt := range rv.Teachers {
		if tt.Name == "ext:a" {
			a = tt
		}
	}
	if n < 30 || !a.Validated || a.Source != "reviews" || a.Checks != n {
		t.Fatalf("teacher a = %+v", a)
	}
	if rv.Acceptable == 0 || rv.Reviewed != n {
		t.Fatalf("review = pending %d acceptable %d reviewed %d", rv.Pending, rv.Acceptable, rv.Reviewed)
	}
	rep, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Data.Accepted == 0 || rep.Data.Human != n || rep.Promoted {
		t.Fatalf("dry run = %+v", rep.Data)
	}
}

// La cola enseña primero lo que discrepa, y reserva sitio para comprobar al
// azar lo que se aceptaría.
func TestLearnReviewOrder(t *testing.T) {
	f := newLearnGateway(t, nil)
	ids := f.stream(t, f.traffic[:60])
	var items []FeedbackItem
	i := 0
	var disagree string
	for id, l := range ids {
		switch i % 3 {
		case 0:
			items = append(items, FeedbackItem{ID: id, Teacher: "a", Label: l}, FeedbackItem{ID: id, Teacher: "b", Label: l})
		case 1:
			items = append(items, FeedbackItem{ID: id, Teacher: "a", Label: "bug"}, FeedbackItem{ID: id, Teacher: "b", Label: "docs"})
			disagree = id
		}
		i++
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	rv, err := f.g.Review(context.Background(), ReviewRequest{Task: "kind", N: 10})
	if err != nil {
		t.Fatal(err)
	}
	audits := 0
	firstPriority := ""
	for _, c := range rv.Cases {
		if c.Audit {
			audits++
		} else if firstPriority == "" {
			firstPriority = c.Reason
		}
	}
	if len(rv.Cases) != 10 || firstPriority != reasonDisagree || disagree == "" {
		t.Fatalf("cases = %+v", rv.Cases)
	}
	if audits < 1 || audits > 4 {
		t.Errorf("audit cases = %d, want 1..4 of 10", audits)
	}
	if _, err := f.g.Review(context.Background(), ReviewRequest{Task: "kind", N: 501}); err == nil {
		t.Error("n over the cap accepted")
	}
}

// Nada del conjunto de confianza entra a entrenar, aunque una persona lo
// etiquete.
func TestLearnLeakGuard(t *testing.T) {
	f := newLearnGateway(t, nil)
	held, _ := readLabelled(filepath.Join(f.dir, "heldout.jsonl"))
	var items []FeedbackItem
	for _, ex := range held[300:340] {
		items = append(items, FeedbackItem{Text: "  " + strings.ToUpper(ex.Text), Label: ex.Label})
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	rep, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Data.Leaked < 40 || rep.Data.Human != 0 {
		t.Fatalf("leak guard: %+v", rep.Data)
	}
}

// Un modelo nuevo apaga la cascada respaldada (su evaluación era del
// anterior) y el reentreno lo dice; con los datos, la vuelve a evaluar.
func TestLearnRetrainReevaluatesCascade(t *testing.T) {
	f := newLearnGateway(t, func(c *Config) { c.Tasks["kind"].EscalateTo = "smol" })
	sum, _ := fileSHA256(f.model)
	rec := &EvalRecord{Task: "kind", Settings: settingsHash(f.g.config().Tasks["kind"]), BeatsChispa: true, Verdict: "test"}
	rec.Chispa.Model, rec.Chispa.SHA256 = "commits", sum
	rec.VON.Model, rec.VON.Snapshot = "smol", "von-smol"
	if err := f.g.saveEval(rec); err != nil {
		t.Fatal(err)
	}
	f.g.regate()
	if !f.g.cascade("kind").On() {
		t.Fatalf("cascade = %+v", f.g.cascade("kind"))
	}
	f.ll.set("bug")
	ids := f.stream(t, f.traffic[:200])
	var items []FeedbackItem
	for id, l := range ids {
		items = append(items, FeedbackItem{ID: id, Label: l})
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	// Capturas con la cascada activa: el voto de VON viene con ellas.
	caps, _ := f.g.learn.readCaptures("kind", 1<<20)
	if len(caps) == 0 || len(caps[0].Teachers) != 1 || caps[0].Teachers[0].Name != "smol" {
		t.Fatalf("captures under the cascade = %+v", caps[:min(len(caps), 1)])
	}
	rep, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", NoEval: true})
	if err != nil || !rep.Promoted {
		t.Fatalf("retrain = %+v %v", rep, err)
	}
	if !strings.Contains(rep.Cascade, "off until it is evaluated again") || f.g.cascade("kind").On() {
		t.Fatalf("cascade note = %q, state %+v", rep.Cascade, f.g.cascade("kind"))
	}
	// Volver a v1 vuelve a encender la cascada: su evaluación era de v1.
	if _, err := f.g.Rollback(RollbackRequest{Task: "kind"}); err != nil {
		t.Fatal(err)
	}
	if !f.g.cascade("kind").On() {
		t.Error("rolling back to the evaluated model should re-enable its cascade")
	}
	// Otra vez a v2, y ahora sí se re-evalúa (el VON falso contesta "bug":
	// pierde, y la cascada queda rechazada con un registro nuevo).
	if _, err := f.g.Rollback(RollbackRequest{Task: "kind", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	lt, _ := f.g.loadLearnTask(context.Background(), "kind", nil, nil)
	note := f.g.reEvaluate(context.Background(), lt, CascadeState{Status: "on"}, RetrainRequest{})
	if !strings.Contains(note, "re-evaluated the cascade") {
		t.Errorf("re-eval note = %q", note)
	}
}

// Backend microvm: la versión queda pendiente hasta que su dorado existe con
// el mismo sha256; entonces la tarea apunta a él, y rollback vuelve al dorado
// anterior.
func TestLearnMicroVMPromoteAndRollback(t *testing.T) {
	f := newLearnGateway(t, func(c *Config) {
		c.Models["commits"] = &ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "commits"}
	})
	b, _ := os.ReadFile(f.model)
	m, _ := chispa.Unmarshal(b)
	dep := fakeDeployLookup{"commits": {Labels: m.Labels, Sha256: sha256Hex(b)}}
	f.g.deploy = dep
	var items []FeedbackItem
	for _, ex := range f.traffic[:200] {
		items = append(items, FeedbackItem{Text: ex.Text, Label: ex.Label})
	}
	if _, err := f.g.Feedback(FeedbackRequest{Task: "kind", Items: items}, true, "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind"}); err == nil || !strings.Contains(err.Error(), "-current") {
		t.Fatalf("without a local copy it must ask for -current: %v", err)
	}
	if _, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", CurrentModel: []byte("not it")}); err == nil {
		t.Fatal("a current model that is not the deployed one was accepted")
	}
	rep, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", CurrentModel: b})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Pending || rep.Promoted || rep.Snapshot != "commits-v2" || len(rep.CandidateModel) == 0 {
		t.Fatalf("retrain = %+v", rep)
	}
	if _, err := f.g.Promote(context.Background(), PromoteRequest{Task: "kind", Version: "v2", Snapshot: "commits-v2"}); err == nil {
		t.Fatal("promoted without the golden snapshot")
	}
	dep["commits-v2"] = ChispaDeployRecord{Labels: m.Labels, Sha256: "0000"}
	if _, err := f.g.Promote(context.Background(), PromoteRequest{Task: "kind", Version: "v2", Snapshot: "commits-v2"}); err == nil {
		t.Fatal("promoted a golden that serves another model")
	}
	dep["commits-v2"] = ChispaDeployRecord{Labels: m.Labels, Sha256: sha256Hex(rep.CandidateModel)}
	pc, err := f.g.Promote(context.Background(), PromoteRequest{Task: "kind", Version: "v2", Snapshot: "commits-v2"})
	if err != nil || pc.To != "v2" {
		t.Fatalf("promote = %+v %v", pc, err)
	}
	if s := f.g.config().Models["commits"].Snapshot; s != "commits-v2" {
		t.Fatalf("task serves snapshot %q", s)
	}
	if s := f.g.rawConfig().Models["commits"].Snapshot; s != "commits" {
		t.Fatalf("the registry itself must not change: %q", s)
	}
	rb, err := f.g.Rollback(RollbackRequest{Task: "kind"})
	if err != nil || rb.Snapshot != "commits" || f.g.config().Models["commits"].Snapshot != "commits" {
		t.Fatalf("rollback = %+v %v", rb, err)
	}
	// Un reentreno siguiente ya no necesita -current: usa la copia local.
	if _, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind", DryRun: true}); err != nil {
		t.Fatal(err)
	}
}

// Un modelo compartido por dos tareas no se reentrena por una de ellas.
func TestLearnRefusesSharedModel(t *testing.T) {
	f := newLearnGateway(t, func(c *Config) { c.Tasks["other"] = &TaskConfig{Chispa: "commits"} })
	_, err := f.g.Retrain(context.Background(), RetrainRequest{Task: "kind"})
	if err == nil || !strings.Contains(err.Error(), "also used by task") {
		t.Fatalf("err = %v", err)
	}
}

func TestLearnGateRules(t *testing.T) {
	cur := HeldoutMetrics{Examples: 4, Accuracy: 0.5, Coverage: 0.5, ConfidentPrecision: 1, AnsweredRight: 0.5}
	same := []bool{true, true, false, false}
	g := decideGate(cur, cur, same, same, "mcnemar", 0.005)
	if g.Pass || g.Significant {
		t.Errorf("identical models passed: %+v", g)
	}
	if g := decideGate(cur, cur, same, same, "no-regression", 0.005); !g.Pass {
		t.Errorf("no-regression should pass identical models: %+v", g)
	}
	worse := cur
	worse.ConfidentPrecision = 0.9
	if g := decideGate(cur, worse, same, same, "no-regression", 0.005); g.Pass || g.PrecisionOK {
		t.Errorf("a precision drop passed: %+v", g)
	}
	none := HeldoutMetrics{Examples: 4}
	if g := decideGate(cur, none, same, []bool{false, false, false, false}, "no-regression", 0.005); g.Pass {
		t.Errorf("a model that never answers passed: %+v", g)
	}
}

func TestRedact(t *testing.T) {
	for in, bad := range map[string]string{
		"password: hunter22 please":                 "hunter22",
		"Authorization: Bearer abc.def.ghi":         "abc.def",
		"key AKIAABCDEFGHIJKLMNOP here":             "AKIAABCD",
		"ghp_" + strings.Repeat("a1", 20):           "ghp_",
		"token 3f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3": "3f9a8b",
		"write to ops@example.org":                  "ops@",
	} {
		if out := redact(in); strings.Contains(out, bad) || !strings.Contains(out, "[redacted]") {
			t.Errorf("redact(%q) = %q", in, out)
		}
	}
	for _, keep := range []string{"open the quarterly report", "internationalization", "mueve la fila 50"} {
		if out := redact(keep); out != keep {
			t.Errorf("redact(%q) = %q", keep, out)
		}
	}
}

// El almacén de capturas no pasa de su tope: primero se borra lo más viejo y,
// si el día de hoy ya lo llena, lo nuevo se pierde y se cuenta.
func TestLearnCaptureCaps(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"2026-01-01", "2026-09-01", "2026-09-20"} {
		if err := os.WriteFile(filepath.Join(dir, d+".jsonl"), make([]byte, 1000), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if total := pruneCaptures(dir, 1<<20, 30, "2026-09-24"); total != 2000 {
		t.Fatalf("after age pruning = %d", total)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-01-01.jsonl")); !os.IsNotExist(err) {
		t.Error("old day kept")
	}
	if total := pruneCaptures(dir, 1500, 0, "2026-09-20"); total != 1000 {
		t.Fatalf("after size pruning = %d", total)
	}
	f := newLearnGateway(t, func(c *Config) { c.Tasks["kind"].Learn.MaxMB = 0 })
	f.g.cfgMu.Lock()
	st := f.g.learnStates["kind"]
	st.cfg.MaxMB = 0 // tope 0: nada cabe
	f.g.learnStates["kind"] = st
	f.g.cfgMu.Unlock()
	f.stream(t, f.traffic[:5])
	if n := f.g.learn.counts["kind|cap"]; n == 0 {
		t.Errorf("cap drops = %d", n)
	}
}
