// Command mejora-continua simula el bucle de mejora continua de Chispa
// (docs/mejora-continua.md) con el gateway de verdad, en proceso y sin daemon:
// el mismo pkg/aigw que sirve `kling ai serve`, con sus capturas, su
// /v1/feedback, su cola de revisión y su `retrain` con puerta.
//
// Lo único simulado son los que contestan cuando Chispa duda:
//
//   - dos MAESTROS con una tasa de error fija (por defecto 8 % y 20 %), que se
//     equivocan sobre todo entre las etiquetas que el propio Chispa duda (el
//     error típico: confundir lo confundible), y que mandan su respuesta por
//     /v1/feedback como haría un cliente con su capa lenta (un codificador, un
//     LLM). Correr VON de verdad sobre decenas de miles de escaladas en CPU
//     no cabe en una sesión; el mecanismo es el mismo.
//   - una PERSONA que cada ronda revisa N casos de la cola de `kling ai
//     review` y contesta con la etiqueta de verdad del conjunto.
//
// Uso:
//
//	go run ./tools/mejora-continua prepare -data <dir con train/valid/test.jsonl> -out <dir>
//	kling chispa train -data <out>/gold.jsonl -valid <out>/valid.jsonl -o <out>/intent.chispa
//	go run ./tools/mejora-continua run -out <dir>
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/chispa"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: mejora-continua prepare|run [flags]")
	}
	var err error
	switch os.Args[1] {
	case "prepare":
		err = prepare(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

// splitmix64: el mismo generador que el entrenador; reproducible.
type rng struct{ s uint64 }

func (r *rng) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng) float() float64 { return float64(r.next()>>11) / (1 << 53) }

func readJSONL(path string) ([]chispa.Example, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return chispa.ReadExamples(f, 0, true)
}

func writeJSONL(path string, exs []chispa.Example) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, ex := range exs {
		if err := enc.Encode(ex); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// prepare parte los datos: un ORO pequeño (lo único que ve el v1), una
// validación pequeña, el TRÁFICO (el resto del entrenamiento, barajado) y el
// conjunto de CONFIANZA (test entero), que nada entrena.
func prepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	data := fs.String("data", "", "directory with train.jsonl, valid.jsonl and test.jsonl ({text, label, fields})")
	out := fs.String("out", "", "output directory")
	gold := fs.Int("gold", 2000, "gold examples (the only thing v1 sees)")
	valid := fs.Int("valid", 1000, "validation examples for calibration")
	seed := fs.Uint64("seed", 1, "shuffle seed")
	_ = fs.Parse(args)
	if *data == "" || *out == "" {
		return fmt.Errorf("-data and -out are required")
	}
	tr, err := readJSONL(filepath.Join(*data, "train.jsonl"))
	if err != nil {
		return err
	}
	va, err := readJSONL(filepath.Join(*data, "valid.jsonl"))
	if err != nil {
		return err
	}
	te, err := readJSONL(filepath.Join(*data, "test.jsonl"))
	if err != nil {
		return err
	}
	r := rng{s: *seed}
	shuffle := func(x []chispa.Example) {
		for i := len(x) - 1; i > 0; i-- {
			j := int(r.next() % uint64(i+1))
			x[i], x[j] = x[j], x[i]
		}
	}
	shuffle(tr)
	shuffle(va)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	for name, exs := range map[string][]chispa.Example{
		"gold.jsonl": tr[:*gold], "traffic.jsonl": tr[*gold:], "valid.jsonl": va[:*valid], "heldout.jsonl": te,
	} {
		if err := writeJSONL(filepath.Join(*out, name), exs); err != nil {
			return err
		}
	}
	fmt.Printf("gold %d, traffic %d, valid %d, held-out %d in %s\n", *gold, len(tr)-*gold, *valid, len(te), *out)
	return nil
}

// teacher es un maestro simulado: acierta con probabilidad 1-errRate; cuando
// falla, el 70 % de las veces elige una de las etiquetas que Chispa también
// consideraba (su top-3) y si no, una cualquiera.
type teacher struct {
	name    string
	errRate float64
}

func (t teacher) answer(r *rng, gold string, top []chispa.ClassProb, labels []string) string {
	if r.float() >= t.errRate {
		return gold
	}
	var conf []string
	for i, c := range top {
		if i < 3 && c.Label != gold {
			conf = append(conf, c.Label)
		}
	}
	if len(conf) > 0 && r.float() < 0.7 {
		return conf[r.next()%uint64(len(conf))]
	}
	for {
		if l := labels[r.next()%uint64(len(labels))]; l != gold {
			return l
		}
	}
}

// Round es una fila de la tabla.
type Round struct {
	Round         int     `json:"round"`
	Served        string  `json:"served"`
	Traffic       int     `json:"traffic"`
	LiveCoverage  float64 `json:"live_coverage"` // tráfico de la ronda contestado confiado
	Escalated     int     `json:"escalated"`
	Teachers      string  `json:"teachers"`
	Reviewed      int     `json:"reviewed"`
	HumanTotal    int     `json:"human_total"`
	Accepted      int     `json:"accepted"`
	Pending       int     `json:"pending"`
	Validated     string  `json:"validated"`
	CurCoverage   float64 `json:"current_coverage"`
	CurPrecision  float64 `json:"current_precision"`
	CurAnswered   float64 `json:"current_answered_right"`
	CurAccuracy   float64 `json:"current_accuracy"`
	NewCoverage   float64 `json:"new_coverage"`
	NewPrecision  float64 `json:"new_precision"`
	NewAnswered   float64 `json:"new_answered_right"`
	NewAccuracy   float64 `json:"new_accuracy"`
	ShadowOnly    int     `json:"shadow_only_right"`
	CurrentOnly   int     `json:"current_only_right"`
	PValue        float64 `json:"p_value"`
	Promoted      string  `json:"promoted"`
	Verdict       string  `json:"verdict"`
	TrainSeconds  float64 `json:"train_seconds"`
	RetrainSecond float64 `json:"retrain_seconds"`
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	out := fs.String("out", "", "directory made by prepare, with intent.chispa trained from gold.jsonl")
	rounds := fs.Int("rounds", 4, "rounds with good teachers")
	perRound := fs.Int("traffic", 5000, "requests per round")
	review := fs.Int("review", 150, "cases a person reviews per round")
	errA := fs.Float64("err-a", 0.08, "error rate of teacher A (encoder-like)")
	errB := fs.Float64("err-b", 0.20, "error rate of teacher B (LLM-like)")
	noisy := fs.Float64("noisy", 0.45, "error rate of the noisy teachers of the last round (0 = no noisy round)")
	seed := fs.Uint64("seed", 7, "seed of the simulated teachers and person")
	_ = fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	abs, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	for _, d := range []string{"ai-data", "ai-evals"} {
		_ = os.RemoveAll(filepath.Join(abs, d))
	}
	reg := map[string]any{
		"models": map[string]any{"intent": map[string]any{"kind": "chispa", "path": "live.chispa"}},
		"tasks": map[string]any{"home-intent": map[string]any{"chispa": "intent", "learn": map[string]any{
			"capture": "text", "gold": "gold.jsonl", "valid": "valid.jsonl", "heldout": "heldout.jsonl",
		}}},
	}
	b, _ := json.MarshalIndent(reg, "", "  ")
	cfgPath := filepath.Join(abs, "ai.json")
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		return err
	}
	v1, err := os.ReadFile(filepath.Join(abs, "intent.chispa"))
	if err != nil {
		return fmt.Errorf("train v1 first (kling chispa train ... -o %s/intent.chispa): %w", abs, err)
	}
	if err := os.WriteFile(filepath.Join(abs, "live.chispa"), v1, 0o644); err != nil {
		return err
	}
	g, err := aigw.New(aigw.Options{ConfigPath: cfgPath})
	if err != nil {
		return err
	}
	defer g.Close(context.Background())
	ctx := context.Background()
	m, err := chispa.Unmarshal(v1)
	if err != nil {
		return err
	}
	traffic, err := readJSONL(filepath.Join(abs, "traffic.jsonl"))
	if err != nil {
		return err
	}
	const task = "home-intent"
	r := rng{s: *seed}
	goldOf := map[string]string{} // id -> etiqueta de verdad (solo la persona simulada la mira)
	var rows []Round
	served := "v1"
	pos := 0
	total := *rounds
	if *noisy > 0 {
		total++
	}
	humanTotal := 0
	for round := 1; round <= total; round++ {
		isNoisy := *noisy > 0 && round == total
		ta, tb := teacher{"encoder", *errA}, teacher{"llm", *errB}
		if isNoisy {
			ta, tb = teacher{"noisy-a", *noisy}, teacher{"noisy-b", *noisy}
		}
		row := Round{Round: round, Served: served, Teachers: fmt.Sprintf("%s %.0f%% + %s %.0f%%", ta.name, ta.errRate*100, tb.name, tb.errRate*100)}
		if isNoisy {
			row.Teachers += " (forced)"
		}
		// Tráfico: lo que Chispa escala se captura; los maestros contestan
		// (como haría el cliente con su capa lenta) por /v1/feedback.
		var items []aigw.FeedbackItem
		conf := 0
		for i := 0; i < *perRound && pos < len(traffic); i, pos = i+1, pos+1 {
			ex := traffic[pos]
			resp, err := g.Classify(ctx, "classify", aigw.ClassifyRequest{Task: task, Text: ex.Text, Fields: ex.Fields})
			if err != nil {
				return err
			}
			row.Traffic++
			if !resp.Escalate {
				conf++
				continue
			}
			row.Escalated++
			goldOf[resp.ID] = ex.Label
			var top []chispa.ClassProb
			if resp.Chispa != nil {
				top = resp.Chispa.Candidates
			}
			items = append(items,
				aigw.FeedbackItem{ID: resp.ID, Teacher: ta.name, Label: ta.answer(&r, ex.Label, top, m.Labels)},
				aigw.FeedbackItem{ID: resp.ID, Teacher: tb.name, Label: tb.answer(&r, ex.Label, top, m.Labels)})
		}
		row.LiveCoverage = ratio(conf, row.Traffic)
		for len(items) > 0 {
			n := min(len(items), 1000)
			if _, err := g.Feedback(aigw.FeedbackRequest{Task: task, Items: items[:n]}, true, "default"); err != nil {
				return err
			}
			items = items[n:]
		}
		// La persona: los N primeros de la cola de revisión, con la verdad.
		rv, err := g.Review(ctx, aigw.ReviewRequest{Task: task, N: *review})
		if err != nil {
			return err
		}
		var human []aigw.FeedbackItem
		for _, c := range rv.Cases {
			if l, ok := goldOf[c.ID]; ok {
				act := "correct"
				if l == c.Proposed {
					act = "confirm"
				}
				human = append(human, aigw.FeedbackItem{ID: c.ID, Action: act, Label: l, By: "sim"})
			}
		}
		if len(human) > 0 {
			if _, err := g.Feedback(aigw.FeedbackRequest{Task: task, Items: human}, true, "default"); err != nil {
				return err
			}
		}
		row.Reviewed = len(human)
		humanTotal += len(human)
		row.HumanTotal = humanTotal
		req := aigw.RetrainRequest{Task: task}
		if isNoisy {
			req.Trust = []string{"ext:" + ta.name, "ext:" + tb.name}
		}
		t0 := time.Now()
		rep, err := g.Retrain(ctx, req)
		if err != nil {
			return err
		}
		row.RetrainSecond = math.Round(time.Since(t0).Seconds()*10) / 10
		var val []string
		for _, t := range rep.Teachers {
			if t.Validated {
				val = append(val, fmt.Sprintf("%s (%s)", t.Name, t.Source))
			}
		}
		row.Validated = strings.Join(val, ", ")
		if row.Validated == "" {
			row.Validated = "-"
		}
		row.Accepted, row.Pending = rep.Data.Accepted, rep.Data.Pending
		c, s := rep.Current, rep.Shadow
		row.CurCoverage, row.CurPrecision, row.CurAnswered, row.CurAccuracy = c.Coverage, c.ConfidentPrecision, c.AnsweredRight, c.Accuracy
		row.NewCoverage, row.NewPrecision, row.NewAnswered, row.NewAccuracy = s.Coverage, s.ConfidentPrecision, s.AnsweredRight, s.Accuracy
		row.ShadowOnly, row.CurrentOnly, row.PValue = rep.Gate.ShadowOnlyRight, rep.Gate.CurrentOnlyRight, rep.Gate.PValue
		row.Verdict, row.TrainSeconds = rep.Gate.Verdict, rep.TrainSeconds
		row.Promoted = "no"
		if rep.Promoted {
			row.Promoted = rep.Version
			served = rep.Version
		}
		rows = append(rows, row)
		fmt.Fprintf(os.Stderr, "round %d: served %s, live coverage %.3f, accepted %d, human %d, %s -> %s\n",
			round, row.Served, row.LiveCoverage, row.Accepted, row.HumanTotal, row.Promoted, rep.Gate.Verdict)
		for _, t := range rep.Teachers {
			fmt.Fprintf(os.Stderr, "   teacher %s: %s\n", t.Name, t.Reason)
		}
	}
	// Una última pasada de tráfico con lo que quedó servido, para ver la
	// cobertura en vivo de la versión final.
	final := Round{Round: total + 1, Served: served}
	conf := 0
	for i := 0; i < *perRound && pos < len(traffic); i, pos = i+1, pos+1 {
		resp, err := g.Classify(ctx, "classify", aigw.ClassifyRequest{Task: task, Text: traffic[pos].Text, Fields: traffic[pos].Fields})
		if err != nil {
			return err
		}
		final.Traffic++
		if !resp.Escalate {
			conf++
		} else {
			final.Escalated++
		}
	}
	final.LiveCoverage = ratio(conf, final.Traffic)
	rows = append(rows, final)

	jb, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(filepath.Join(abs, "rounds.json"), jb, 0o644); err != nil {
		return err
	}
	printTable(os.Stdout, rows)
	// Lo que enseña /metrics al final: las series del bucle.
	rec := httptest.NewRecorder()
	g.Handler("").ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	fmt.Println()
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		for _, p := range []string{"kling_ai_chispa_version_coverage{", "kling_ai_heldout_", "kling_ai_escalation_rate_window{", "kling_ai_requests_window{", "kling_ai_learn_"} {
			if strings.HasPrefix(l, p) {
				fmt.Println(l)
			}
		}
	}
	return nil
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10000) / 10000
}

func printTable(w io.Writer, rows []Round) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "round\tserved\ttraffic\tlive cov.\tteachers\thuman (total)\taccepted\tvalidated\theld-out cov. cur→new\tprec. cur→new\tanswered right cur→new\t+/-\tp\tpromoted")
	for _, r := range rows {
		if r.Teachers == "" {
			fmt.Fprintf(tw, "%d\t%s\t%d\t%.3f\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\n", r.Round, r.Served, r.Traffic, r.LiveCoverage)
			continue
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%.3f\t%s\t%d (%d)\t%d\t%s\t%.3f→%.3f\t%.3f→%.3f\t%.3f→%.3f\t+%d/-%d\t%.1g\t%s\n",
			r.Round, r.Served, r.Traffic, r.LiveCoverage, r.Teachers, r.Reviewed, r.HumanTotal, r.Accepted, r.Validated,
			r.CurCoverage, r.NewCoverage, r.CurPrecision, r.NewPrecision, r.CurAnswered, r.NewAnswered, r.ShadowOnly, r.CurrentOnly, r.PValue, r.Promoted)
	}
	_ = tw.Flush()
}
