package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
	"github.com/juan52878911/kindling/pkg/domotica"
)

// kling domotica: la decisión de la habitación de demo (pkg/domotica) desde la
// CLI, sin daemon. Plantillas de la demo → Chispa (intención) + Chispa-slots
// (huecos); lo que no es confiado dice a qué capa escalar. Ver docs/domotica.md.

const domoticaUsage = `usage: kling domotica <command> [options]

  decide [-lang es|en|auto] [-intent m.chispa]  decides one command: intent, slots, layer,
         [-slots m.chispas] [-json] "<text>"    confidence and latency (JSONL from stdin
                                                when no text is given)
  eval -data test.jsonl [-intent m.chispa]      intent accuracy / macro-F1, slot F1, exact
       [-slots m.chispas] [-challenge]          match, coverage and precision at the
                                                confident threshold, latency, per layer
  train-slots -data train.jsonl -o m.chispas    trains the slot tagger (Chispa-slots)
       [-valid valid.jsonl] [-test t.jsonl]
  embed -url http://host:port -model <encoder>  embeds the texts of JSONL files with a
       -data a.jsonl,b.jsonl -o cache.jemb      sentence encoder replica, into a cache
  train-encoder -data train.jsonl -valid v.jsonl   trains the layer-3 head on cached
       -cache c.jemb -o head.jenc [-hidden N]   encoder vectors
  templates [-lang es|en]                       the demo commands the template layer knows

decide and eval take the layer-3 encoder with -encoder head.jenc plus -embed-url
http://host:port (a replica) and/or -embed-cache c.jemb (docs/codificador.md).

Models default to $KLING_DOMOTICA_MODELS (or the user cache dir)/intent.chispa and
slots.chispas; without them only the demo templates answer. Data: go run
./tools/domotica-data fetch && go run ./tools/domotica-data build (docs/domotica-datos.md).
`

func cmdDomotica(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(domoticaUsage)
		return nil
	}
	switch args[0] {
	case "decide":
		return cmdDomoticaDecide(args[1:])
	case "eval":
		return cmdDomoticaEval(args[1:])
	case "train-slots":
		return cmdDomoticaTrainSlots(args[1:])
	case "embed":
		return cmdDomoticaEmbed(args[1:])
	case "train-encoder":
		return cmdDomoticaTrainEncoder(args[1:])
	case "templates":
		return cmdDomoticaTemplates(args[1:])
	}
	return fmt.Errorf("unknown domotica command %q\n\n%s", args[0], domoticaUsage)
}

func domoticaModelsDir() string {
	if d := os.Getenv("KLING_DOMOTICA_MODELS"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "kindling", "domotica", "models")
}

var (
	matcherOnce sync.Once
	matcherVal  *domotica.Matcher
	matcherErr  error
)

func demoMatcher() (*domotica.Matcher, error) {
	matcherOnce.Do(func() { matcherVal, matcherErr = domotica.NewMatcher(domotica.DemoTemplates) })
	return matcherVal, matcherErr
}

// loadDecider carga lo que haya. Un modelo pedido explícitamente que no carga
// es un error; uno por defecto que no existe, no (queda solo la capa 1).
func loadDecider(intentPath, slotsPath string) (*domotica.Decider, error) {
	m, err := demoMatcher()
	if err != nil {
		return nil, err
	}
	d := &domotica.Decider{Matcher: m}
	dir := domoticaModelsDir()
	if intentPath == "" && dir != "" {
		if p := filepath.Join(dir, "intent.chispa"); regularFile(p) {
			intentPath = p
		}
	}
	if slotsPath == "" && dir != "" {
		if p := filepath.Join(dir, "slots.chispas"); regularFile(p) {
			slotsPath = p
		}
	}
	if intentPath != "" {
		if d.Intent, err = chispa.LoadFile(intentPath); err != nil {
			return nil, err
		}
	}
	if slotsPath != "" {
		if d.Slots, err = slots.LoadFile(slotsPath); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func regularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

type decideOut struct {
	Text string `json:"text"`
	domotica.Decision
}

func cmdDomoticaDecide(args []string) error {
	fs := flag.NewFlagSet("domotica decide", flag.ExitOnError)
	lang := fs.String("lang", "auto", "language: es, en or auto")
	intentPath := fs.String("intent", "", "intent model (.chispa)")
	slotsPath := fs.String("slots", "", "slot model (.chispas)")
	asJSON := fs.Bool("json", false, "one JSON object per line")
	encPath := fs.String("encoder", "", "layer-3 head (.jenc)")
	embedURL := fs.String("embed-url", "", "the head's encoder: http://host:port of a replica")
	embedCache := fs.String("embed-cache", "", "embedding cache (.jemb) to look up first")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	d, err := loadDecider(*intentPath, *slotsPath)
	if err != nil {
		return err
	}
	if enc, err := loadEncoder(*encPath, *embedCache, *embedURL); err != nil {
		return err
	} else if enc != nil {
		d.Encoder = enc
	}
	show := func(text string) {
		dec := d.Decide(text, *lang)
		if *asJSON {
			b, _ := json.Marshal(decideOut{Text: text, Decision: dec})
			fmt.Println(string(b))
			return
		}
		slotsJSON, _ := json.Marshal(dec.Slots)
		status := "confident"
		if !dec.Confident {
			status = "escalate → " + dec.Escalate + " (" + dec.Reason + ")"
		}
		fmt.Printf("intent: %s  slots: %s\nlayer: %s  lang: %s  p=%.3f  %s  %.1f µs\n",
			orDash(dec.Intent), slotsJSON, dec.Layer, dec.Lang, dec.Prob, status, dec.LatencyUS)
	}
	if fs.NArg() > 0 {
		show(strings.Join(fs.Args(), " "))
		return nil
	}
	rows, err := chispa.ReadExamples(os.Stdin, 0, false)
	if err != nil {
		return err
	}
	*asJSON = true
	for _, r := range rows {
		show(r.Text)
	}
	return nil
}

func readRowsFile(path string) ([]domotica.Row, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	h := sha256.New()
	rows, err := domotica.ReadRows(io.TeeReader(f, h))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return rows, hex.EncodeToString(h.Sum(nil)), nil
}

func cmdDomoticaEval(args []string) error {
	fs := flag.NewFlagSet("domotica eval", flag.ExitOnError)
	data := fs.String("data", "", "unified JSONL test data (tools/domotica-data build)")
	intentPath := fs.String("intent", "", "intent model (.chispa)")
	slotsPath := fs.String("slots", "", "slot model (.chispas)")
	challenge := fs.Bool("challenge", true, "also score the built-in challenge set (indirect, multi-command, near out-of-scope)")
	errs := fs.Int("errors", 0, "print this many confident errors per system")
	encPath := fs.String("encoder", "", "layer-3 head (.jenc): adds the encoder alone and the cascade with it")
	embedURL := fs.String("embed-url", "", "the head's encoder: http://host:port of a replica")
	embedCache := fs.String("embed-cache", "", "embedding cache (.jemb) to look up first (reproducible, no replica needed)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	d, err := loadDecider(*intentPath, *slotsPath)
	if err != nil {
		return err
	}
	enc, err := loadEncoder(*encPath, *embedCache, *embedURL)
	if err != nil {
		return err
	}
	var rows []domotica.Row
	if *data != "" {
		if rows, _, err = readRowsFile(*data); err != nil {
			return err
		}
	}
	systems := []struct {
		name string
		sys  domotica.System
	}{
		{"template", func(text, lang string) domotica.Decision {
			t0 := time.Now()
			m := d.Matcher.Match(text)
			out := domotica.Decision{Layer: domotica.LayerNone}
			if m.OK {
				out = domotica.Decision{Intent: m.Intent, Slots: m.Slots, Layer: domotica.LayerTemplate, Confident: true}
			}
			out.LatencyUS = float64(time.Since(t0).Nanoseconds()) / 1e3
			return out
		}},
		{"keywords", func(text, lang string) domotica.Decision {
			t0 := time.Now()
			intent, s := domotica.Keywords(text)
			return domotica.Decision{Intent: intent, Slots: s, Layer: "keywords", Confident: true,
				LatencyUS: float64(time.Since(t0).Nanoseconds()) / 1e3}
		}},
	}
	if d.Intent != nil {
		// «chispa» es el clasificador solo, con su «fuera de ámbito» como
		// respuesta final; la cascada es la política del producto: primero
		// las plantillas y lo fuera de ámbito escala. «cascade, OOS final»
		// muestra lo que cambia esa política.
		chispaOnly, final := *d, *d
		chispaOnly.NoTemplate, chispaOnly.FinalOOS = true, true
		final.FinalOOS = true
		type named = struct {
			name string
			sys  domotica.System
		}
		systems = append(systems, named{"chispa", chispaOnly.Decide}, named{"cascade", d.Decide},
			named{"cascade, OOS final", final.Decide})
		systems = append(systems, encoderSystems(d, enc)...)
	} else {
		fmt.Println("(no intent model: only template and keyword baselines)")
	}
	if len(rows) > 0 {
		var reps []*domotica.Report
		for _, s := range systems {
			// Calentamiento: la primera llamada paga pools y cachés.
			for i := 0; i < min(200, len(rows)); i++ {
				s.sys(rows[i].Text, rows[i].Lang)
			}
			reps = append(reps, domotica.Evaluate(s.name, s.sys, rows))
		}
		groups := []string{"all", "es", "en", "inscope", "inscope/es", "inscope/en", "oos",
			"inscope/massive/es", "inscope/massive/en", "inscope/ha", "inscope/demo"}
		fmt.Printf("== %s (%d rows) ==\n", *data, len(rows))
		fmt.Println("intent = intent accuracy, mF1 = intent macro-F1, slotF1 = normalized slot=value F1 (in-scope gold),")
		fmt.Println("exact = intent and every slot right, cover = confident share, prec@c = exact accuracy among confident")
		domotica.WriteTable(os.Stdout, reps, groups)
		fmt.Println("\nlatency per command (µs): p50 / p99, and layer that answered")
		for _, r := range reps {
			var ls []string
			for _, k := range sortedKeysInt(r.Layers) {
				ls = append(ls, fmt.Sprintf("%s=%d", k, r.Layers[k]))
			}
			fmt.Printf("  %-22s %8.2f / %8.2f   %s\n", r.Name, r.Percentile(0.5), r.Percentile(0.99), strings.Join(ls, " "))
		}
		if d.Slots != nil {
			var sents []slots.Sentence
			for _, r := range rows {
				if r.Intent != domotica.OutOfScope {
					sents = append(sents, slots.Sentence{Text: r.Text, Spans: r.Spans})
				}
			}
			sr := d.Slots.Evaluate(sents)
			fmt.Printf("\nslot tagger alone, exact span F1 on in-scope rows (%d): P %.3f  R %.3f  F1 %.3f\n", len(sents), sr.MicroP, sr.MicroR, sr.MicroF1)
			for _, name := range sr.SlotNamesSorted() {
				s := sr.PerSlot[name]
				fmt.Printf("  %-8s P %.3f  R %.3f  F1 %.3f  (gold %d)\n", name, s.P, s.R, s.F1, s.TP+s.FN)
			}
		}
		if *errs > 0 {
			for _, r := range reps {
				fmt.Printf("\nconfident errors, %s:\n", r.Name)
				for i, e := range r.Errors {
					if i == *errs {
						break
					}
					fmt.Println("  " + e)
				}
			}
		}
	}
	if *challenge {
		ind := domotica.Indirect("test")
		fmt.Printf("\n== indirect commands, held-out split (%d hand-written rows; see docs/codificador.md) ==\n", len(ind))
		fmt.Printf("%-22s %4s %10s %10s %10s\n", "system", "n", "right", "escalated", "WRONG")
		for _, s := range systems {
			res, wrong := domotica.EvaluateChallenge(s.sys, ind)
			x := res["indirect"]
			fmt.Printf("%-22s %4d %10d %10d %10d\n", s.name, x.N, x.ConfidentRight, x.Escalated, x.ConfidentWrong)
			if *errs > 0 {
				for _, w := range wrong {
					fmt.Println("    " + w)
				}
			}
		}
		ch := domotica.Challenge()
		fmt.Printf("\n== challenge set (%d hand-written rows) ==\n", len(ch))
		fmt.Printf("%-22s %-11s %4s %10s %10s %10s\n", "system", "class", "n", "right", "escalated", "WRONG")
		for _, s := range systems {
			res, wrong := domotica.EvaluateChallenge(s.sys, ch)
			for _, c := range []string{"indirect", "multi", "oos_near", "paraphrase"} {
				x := res[c]
				if x == nil {
					continue
				}
				fmt.Printf("%-22s %-11s %4d %10d %10d %10d\n", s.name, c, x.N, x.ConfidentRight, x.Escalated, x.ConfidentWrong)
			}
			if *errs > 0 {
				for _, w := range wrong {
					fmt.Println("    " + w)
				}
			}
		}
	}
	return nil
}

func sortedKeysInt(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmdDomoticaTrainSlots(args []string) error {
	fs := flag.NewFlagSet("domotica train-slots", flag.ExitOnError)
	data := fs.String("data", "", "unified JSONL training data (required)")
	valid := fs.String("valid", "", "unified JSONL validation data (early stopping)")
	test := fs.String("test", "", "optional unified JSONL test data")
	out := fs.String("o", "", "output model (.chispas, required)")
	bucketsLog := fs.Int("buckets", 17, "log2 of the number of hash buckets")
	window := fs.Int("window", 2, "neighbour words on each side")
	affix := fs.Int("affix", 3, "prefix/suffix runes (0 = off)")
	lexicon := fs.Bool("lexicon", true, "lexicon class features (areas, devices, colors, numbers, units)")
	epochs := fs.Int("epochs", 15, "maximum epochs")
	seed := fs.Uint64("seed", 1, "random seed")
	verbose := fs.Bool("v", false, "print every epoch")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *data == "" || *out == "" {
		return errors.New("usage: kling domotica train-slots -data train.jsonl -o slots.chispas [-valid v.jsonl]")
	}
	if *bucketsLog < 4 || *bucketsLog > 22 {
		return errors.New("-buckets must be between 4 and 22 (log2)")
	}
	load := func(path string) ([]slots.Sentence, string, error) {
		if path == "" {
			return nil, "", nil
		}
		rows, sum, err := readRowsFile(path)
		if err != nil {
			return nil, "", err
		}
		var out []slots.Sentence
		for _, r := range rows {
			// Solo las órdenes de la habitación: en lo fuera de ámbito no hay
			// huecos anotados, y enseñarle que «cocina» no es zona en «limpia
			// la cocina» solo confunde al etiquetador.
			if r.Intent != domotica.OutOfScope {
				out = append(out, slots.Sentence{Text: r.Text, Spans: r.Spans})
			}
		}
		return out, sum, nil
	}
	tr, sum, err := load(*data)
	if err != nil {
		return err
	}
	va, _, err := load(*valid)
	if err != nil {
		return err
	}
	te, _, err := load(*test)
	if err != nil {
		return err
	}
	spec := slots.DefaultSpec()
	spec.Buckets, spec.Window, spec.Affix, spec.Lexicon = 1<<*bucketsLog, *window, *affix, *lexicon
	created := time.Now().UTC().Format(time.RFC3339)
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			created = time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	}
	cfg := slots.TrainConfig{Spec: spec, Lexicon: domotica.Lexicon(), MaxEpochs: *epochs, Seed: *seed,
		CreatedAt: created, DatasetSHA256: sum}
	if *verbose {
		cfg.Log = os.Stderr
	}
	t0 := time.Now()
	res, err := slots.Train(tr, va, cfg)
	if err != nil {
		return err
	}
	if err := res.Model.Save(*out); err != nil {
		return err
	}
	st, err := os.Stat(*out)
	if err != nil {
		return err
	}
	fmt.Printf("trained %s in %s: slots %s, %d train / %d valid sentences, best epoch %d\n",
		*out, time.Since(t0).Round(time.Millisecond), strings.Join(res.Model.SlotNames(), ","),
		res.Model.Meta.TrainSentences, res.Model.Meta.ValidSentences, res.BestEpoch)
	fmt.Printf("file: %s   int16 vs float agreement (valid): %.4f\n", human(st.Size()), res.QuantAgreement)
	fmt.Printf("valid: exact span P %.3f  R %.3f  F1 %.3f\n", res.Valid.MicroP, res.Valid.MicroR, res.Valid.MicroF1)
	if len(te) > 0 {
		r := res.Model.Evaluate(te)
		fmt.Printf("test:  exact span P %.3f  R %.3f  F1 %.3f\n", r.MicroP, r.MicroR, r.MicroF1)
	}
	return nil
}

func cmdDomoticaTemplates(args []string) error {
	fs := flag.NewFlagSet("domotica templates", flag.ExitOnError)
	lang := fs.String("lang", "", "only this language")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	m, err := demoMatcher()
	if err != nil {
		return err
	}
	for _, t := range domotica.DemoTemplates {
		if *lang != "" && t.Lang != *lang {
			continue
		}
		dev := t.Device
		if dev == "" {
			dev = "-"
		}
		fmt.Printf("%s  %-18s %-10s %s\n", t.Lang, t.Intent, dev, t.Template)
	}
	fmt.Printf("\n%d templates, %d distinct normalized phrases ({area}, {value} and {color} are filled at match time)\n",
		len(domotica.DemoTemplates), m.Size())
	return nil
}
