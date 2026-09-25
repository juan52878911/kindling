package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/train"
)

// kling chispa: el clasificador lineal diminuto (pkg/chispa). Es del núcleo porque no
// necesita daemon ni microVM: entrena y predice en la máquina donde corre el
// CLI, y el gateway futuro lo usará como primer escalón de la cascada Chispa → VON.
// Ver docs/chispa.md.

const chispaUsage = `usage: kling chispa <command> [options]

  train   -data train.jsonl -o model.chispa  trains, quantizes, calibrates and
          [-valid v.jsonl] [-test t.jsonl]   picks per-class thresholds
  eval    -model m.chispa -data test.jsonl   accuracy, macro-F1, per-class metrics,
          [-json]                            confusion, ECE, coverage at τ
  predict -model m.chispa [-text T]          one prediction, or JSONL from stdin
          [-fields JSON] [-top N] [-json]    (one JSON result per line)
  inspect <model.chispa> [-json]             format, feature spec, labels, τ, metadata

  deploy  <task> -model m.chispa [-slots s.chispas]  serverless: packages the model as a
          [-mem 64] [-vcpus 1]               microVM image and freezes a golden
                                              snapshot (needs a daemon; docs/chispa-serverless.md)
  ls      [-json]                            deployed chispa tasks (golden snapshots)
  rm      <task> [-keep-image]               removes a deployed task's snapshot (and image)

Data is JSONL: {"text": "...", "label": "...", "fields": {"service": "api"}}
Run 'kling chispa <command> -h' for the options of each command.

deploy/ls/rm need a kindling daemon (a golden snapshot lives in a microVM);
train/eval/predict/inspect never do: Chispa runs in this process alone.
`

func cmdChispa(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(chispaUsage)
		return nil
	}
	switch args[0] {
	case "train":
		return cmdChispaTrain(args[1:])
	case "eval":
		return cmdChispaEval(args[1:])
	case "predict":
		return cmdChispaPredict(args[1:])
	case "inspect":
		return cmdChispaInspect(args[1:])
	case "deploy":
		return cmdChispaDeploy(args[1:])
	case "ls", "list":
		return cmdChispaLs(args[1:])
	case "rm", "remove":
		return cmdChispaRm(args[1:])
	}
	return fmt.Errorf("unknown chispa command %q\n\n%s", args[0], chispaUsage)
}

// readJSONL lee ejemplos de un fichero (con topes, ver chispa.ReadExamples) y
// devuelve también el SHA-256 de lo leído, que va a los metadatos del modelo.
func readJSONL(path string, needLabel bool) ([]chispa.Example, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	h := sha256.New()
	exs, err := chispa.ReadExamples(io.TeeReader(f, h), 0, needLabel)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return exs, hex.EncodeToString(h.Sum(nil)), nil
}

func cmdChispaTrain(args []string) error {
	fs := flag.NewFlagSet("chispa train", flag.ExitOnError)
	data := fs.String("data", "", "training JSONL (required)")
	valid := fs.String("valid", "", "validation JSONL for early stopping, calibration and τ (default: hold out -valid-frac of -data)")
	test := fs.String("test", "", "optional test JSONL: evaluated after training")
	out := fs.String("o", "", "output model file (required)")
	bucketsLog := fs.Int("buckets", 18, "log2 of the number of hash buckets")
	uni := fs.Bool("unigrams", true, "word unigrams")
	bi := fs.Bool("bigrams", true, "word bigrams")
	char := fs.String("char", "", "character n-grams as MIN-MAX (e.g. 3-5); empty = off")
	fields := fs.Bool("fields", true, "structured field features")
	maxText := fs.Int("max-text", chispa.DefaultMaxTextBytes, "bytes of text considered")
	epochs := fs.Int("epochs", 30, "maximum epochs")
	patience := fs.Int("patience", 3, "epochs without validation improvement before stopping")
	lr := fs.Float64("lr", 0.05, "AdaGrad learning rate")
	l2 := fs.Float64("l2", 1e-4, "L2 regularization per update (0 = off)")
	cw := fs.String("class-weight", "balanced", "class weighting: balanced, sqrt or none")
	seed := fs.Uint64("seed", 1, "random seed (training is deterministic given it)")
	validFrac := fs.Float64("valid-frac", 0.1, "fraction held out for validation when -valid is not given")
	precision := fs.Float64("precision", 0.95, "target precision of the confident subset, per class")
	minSupport := fs.Int("min-support", 10, "confident validation predictions needed to trust a class threshold")
	ovr := fs.String("one-vs-rest", "", "train a binary LABEL-vs-rest model from multi-class data")
	verbose := fs.Bool("v", false, "print the loss of every epoch")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *data == "" || *out == "" {
		return errors.New("usage: kling chispa train -data train.jsonl -o model.chispa [options]")
	}
	if *bucketsLog < 4 || *bucketsLog > 22 {
		return errors.New("-buckets must be between 4 and 22 (log2)")
	}
	spec := chispa.FeatureSpec{Buckets: 1 << *bucketsLog, Unigrams: *uni, Bigrams: *bi, Fields: *fields, MaxTextBytes: *maxText}
	if *char != "" {
		lo, hi, ok := strings.Cut(*char, "-")
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if !ok || err1 != nil || err2 != nil {
			return fmt.Errorf("-char wants MIN-MAX, got %q", *char)
		}
		spec.CharMin, spec.CharMax = a, b
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	l2v := *l2
	if l2v == 0 {
		l2v = -1 // en Config, 0 es «por defecto»; aquí 0 es «sin L2»
	}

	trainEx, sum, err := readJSONL(*data, true)
	if err != nil {
		return err
	}
	var validEx, testEx []chispa.Example
	if *valid != "" {
		if validEx, _, err = readJSONL(*valid, true); err != nil {
			return err
		}
	}
	if *test != "" {
		if testEx, _, err = readJSONL(*test, true); err != nil {
			return err
		}
	}
	// SOURCE_DATE_EPOCH: la convención de builds reproducibles. Con ella el
	// .chispa sale idéntico byte a byte en cada entrenamiento con la misma semilla.
	created := time.Now().UTC().Format(time.RFC3339)
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			created = time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	}
	cfg := train.Config{
		Spec: spec, MaxEpochs: *epochs, Patience: *patience, LearningRate: *lr, L2: l2v,
		ClassWeight: *cw, Seed: *seed, ValidFraction: *validFrac, TargetPrecision: *precision,
		MinSupport: *minSupport, DatasetSHA256: sum, CreatedAt: created, OneVsRest: *ovr,
	}
	if *verbose {
		cfg.Log = os.Stderr
	}
	t0 := time.Now()
	res, err := train.Train(trainEx, validEx, cfg)
	if err != nil {
		return err
	}
	took := time.Since(t0)
	if err := res.Model.Save(*out); err != nil {
		return err
	}
	st, err := os.Stat(*out)
	if err != nil {
		return err
	}
	m := res.Model
	fmt.Printf("trained %s in %s: %d labels, %d train / %d valid examples, best epoch %d\n",
		*out, took.Round(time.Millisecond), len(m.Labels), m.Meta.TrainExamples, m.Meta.ValidExamples, res.BestEpoch)
	fmt.Printf("file: %s   temperature: %.3f   int16 vs float agreement (valid): %.4f\n",
		human(st.Size()), m.Temperature, res.QuantAgreement)
	v := res.Valid
	fmt.Printf("valid: accuracy %.3f  macro-F1 %.3f  ECE %.3f  confident %.1f%% at precision %.3f\n",
		v.Accuracy, v.MacroF1, v.ECE, 100*v.Coverage, v.ConfidentPrecision)
	if len(testEx) > 0 {
		fmt.Printf("\n== test (%s) ==\n", *test)
		chispa.Evaluate(m, testEx).WriteText(os.Stdout)
		fmt.Printf("\nint16 vs float agreement (test): %.4f\n", res.Agreement(testEx))
	}
	return nil
}

func cmdChispaEval(args []string) error {
	fs := flag.NewFlagSet("chispa eval", flag.ExitOnError)
	model := fs.String("model", "", "model file (required)")
	data := fs.String("data", "", "labelled JSONL (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *model == "" || *data == "" {
		return errors.New("usage: kling chispa eval -model m.chispa -data test.jsonl [-json]")
	}
	m, err := chispa.LoadFile(*model)
	if err != nil {
		return err
	}
	exs, _, err := readJSONL(*data, true)
	if err != nil {
		return err
	}
	t0 := time.Now()
	rep := chispa.Evaluate(m, exs)
	took := time.Since(t0)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	rep.WriteText(os.Stdout)
	if len(exs) > 0 {
		fmt.Printf("\n%d predictions in %s (%.1f µs each, including evaluation)\n",
			len(exs), took.Round(time.Microsecond), float64(took.Microseconds())/float64(len(exs)))
	}
	return nil
}

func cmdChispaPredict(args []string) error {
	fs := flag.NewFlagSet("chispa predict", flag.ExitOnError)
	model := fs.String("model", "", "model file (required)")
	text := fs.String("text", "", "text to classify; without it, JSONL inputs are read from stdin")
	fieldsJSON := fs.String("fields", "", `structured fields as a JSON object, e.g. '{"level":"error"}'`)
	top := fs.Int("top", 5, "evidence: features that push the predicted label the most (0 = none)")
	asJSON := fs.Bool("json", false, "JSON output for -text (stdin mode always writes JSON lines)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *model == "" {
		return errors.New("usage: kling chispa predict -model m.chispa [-text T] [-fields JSON] [-top N] [-json]")
	}
	m, err := chispa.LoadFile(*model)
	if err != nil {
		return err
	}
	if *text == "" && *fieldsJSON == "" {
		return predictStream(m, os.Stdin, os.Stdout, *top)
	}
	in := chispa.Input{Text: *text}
	if *fieldsJSON != "" {
		if err := json.Unmarshal([]byte(*fieldsJSON), &in.Fields); err != nil {
			return fmt.Errorf("-fields: %w", err)
		}
	}
	p := m.PredictFull(in, *top)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(p)
	}
	fmt.Printf("%s  p=%.3f  τ=%s  -> %s\n", p.Label, p.Prob, fmtTau(p.Threshold), p.Decision)
	for _, c := range p.Probs[1:min(4, len(p.Probs))] {
		fmt.Printf("  %-12s %.3f\n", c.Label, c.Prob)
	}
	if len(p.Evidence) > 0 {
		fmt.Println("evidence:")
		for _, e := range p.Evidence {
			fmt.Printf("  %+.3f  %s\n", e.Weight, e.Feature)
		}
	}
	return nil
}

// predictStream clasifica JSONL de r, una respuesta JSON por línea. Las
// líneas se leen con tope (chispa.MaxLineBytes): una entrada sin saltos de línea
// no se come la memoria.
func predictStream(m *chispa.Model, r io.Reader, w io.Writer, top int) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), chispa.MaxLineBytes)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	enc := json.NewEncoder(bw)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		var in chispa.Input
		if err := json.Unmarshal(b, &in); err != nil {
			return fmt.Errorf("stdin line %d: %w", line, err)
		}
		if err := enc.Encode(m.PredictFull(in, top)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func fmtTau(t float64) string {
	if t >= chispa.NeverConfident {
		return "never"
	}
	return fmt.Sprintf("%.3f", t)
}

func cmdChispaInspect(args []string) error {
	fs := flag.NewFlagSet("chispa inspect", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling chispa inspect <model.chispa> [-json]")
	}
	path := fs.Arg(0)
	m, err := chispa.LoadFile(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	K := m.NumOutputs()
	rows := 0
	for b := 0; b < int(m.Spec.Buckets); b++ {
		for k := 0; k < K; k++ {
			if m.W[b*K+k] != 0 {
				rows++
				break
			}
		}
	}
	type labelInfo struct {
		Label     string  `json:"label"`
		Threshold float64 `json:"threshold"`
		TrainN    int     `json:"train_examples"`
	}
	info := struct {
		File          string             `json:"file"`
		Bytes         int64              `json:"bytes"`
		FormatVersion int                `json:"format_version"`
		Spec          chispa.FeatureSpec `json:"spec"`
		SpecHash      string             `json:"spec_hash"`
		Binary        bool               `json:"binary"`
		Outputs       int                `json:"outputs"`
		NonzeroRows   int                `json:"nonzero_buckets"`
		Temperature   float64            `json:"temperature"`
		Labels        []labelInfo        `json:"labels"`
		Meta          chispa.Meta        `json:"meta"`
	}{path, st.Size(), chispa.FormatVersion, m.Spec, fmt.Sprintf("%016x", m.SpecHash), m.Binary, K, rows, m.Temperature, nil, m.Meta}
	for c, l := range m.Labels {
		info.Labels = append(info.Labels, labelInfo{l, m.Thresholds[c], m.Meta.ClassCounts[l]})
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	mode := "multinomial (softmax)"
	if m.Binary {
		mode = "binary (logistic)"
		if m.Meta.OneVsRest != "" {
			mode += ", one-vs-rest for " + m.Meta.OneVsRest
		}
	}
	s := m.Spec
	char := "off"
	if s.CharMin > 0 {
		char = fmt.Sprintf("%d-%d", s.CharMin, s.CharMax)
	}
	fmt.Printf("file:        %s (%s, format v%d)\n", path, human(st.Size()), chispa.FormatVersion)
	fmt.Printf("model:       %s, %d outputs, temperature %.3f\n", mode, K, m.Temperature)
	fmt.Printf("features:    2^%d buckets (%d non-zero), unigrams=%v bigrams=%v char=%s fields=%v max-text=%d\n",
		log2(s.Buckets), rows, s.Unigrams, s.Bigrams, char, s.Fields, s.MaxTextBytes)
	fmt.Printf("spec hash:   %016x\n", m.SpecHash)
	mt := m.Meta
	if mt.CreatedAt != "" {
		fmt.Printf("created:     %s\n", mt.CreatedAt)
	}
	if mt.DatasetSHA256 != "" {
		fmt.Printf("dataset:     sha256 %s…, %d train / %d valid examples\n", mt.DatasetSHA256[:min(16, len(mt.DatasetSHA256))], mt.TrainExamples, mt.ValidExamples)
	}
	if mt.Optimizer != "" {
		keys := make([]string, 0, len(mt.Params))
		for k := range mt.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var ps []string
		for _, k := range keys {
			ps = append(ps, k+"="+mt.Params[k])
		}
		fmt.Printf("training:    %s, best epoch %d, %s\n", mt.Optimizer, mt.Epochs, strings.Join(ps, " "))
	}
	if len(mt.ValidMetrics) > 0 {
		v := mt.ValidMetrics
		fmt.Printf("validation:  accuracy %.3f  macro-F1 %.3f  ECE %.3f  confident %.1f%% at precision %.3f (target %.2f)\n",
			v["accuracy"], v["macro_f1"], v["ece"], 100*v["coverage"], v["confident_precision"], mt.TargetPrecision)
		fmt.Printf("int16/float: %.4f agreement on validation\n", mt.QuantAgreement)
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tτ\tTRAIN EXAMPLES")
	for _, l := range info.Labels {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", l.Label, fmtTau(l.Threshold), l.TrainN)
	}
	return tw.Flush()
}

func log2(n uint32) int {
	l := 0
	for n > 1 {
		n >>= 1
		l++
	}
	return l
}
