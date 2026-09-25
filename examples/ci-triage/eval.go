package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

// La evaluación: el localizador contra el oro de LogChunks (o de cualquier
// manifiesto con el mismo formato, como los logs privados de GitHub Actions),
// la categoría de punta a punta y las líneas base, todo por el gateway.

// ranker es una forma de puntuar las líneas de un log: Chispa o una línea base.
type ranker struct {
	name  string
	score func(lg *triage.Log) []float64
}

// locMetrics acumula las cifras del localizador de un método.
type locMetrics struct {
	n                    int
	hit1, hit5, hit10    float64
	p5, p10, r10, r50    float64
	located, cp, cr, cf1 float64
	cover50              float64
	chunkLines, chunkB   float64
}

func (m *locMetrics) add(score []float64, gold []bool, lg *triage.Log, o triage.ChunkOptions) []triage.Chunk {
	ng := 0
	for _, g := range gold {
		if g {
			ng++
		}
	}
	rank := make([]int, 0, len(score))
	for i, s := range score {
		if s > 0 {
			rank = append(rank, i)
		}
	}
	sort.SliceStable(rank, func(a, b int) bool {
		if score[rank[a]] != score[rank[b]] {
			return score[rank[a]] > score[rank[b]]
		}
		return rank[a] > rank[b]
	})
	hitsAt := func(k int) int {
		h := 0
		for _, i := range rank[:min(k, len(rank))] {
			if gold[i] {
				h++
			}
		}
		return h
	}
	m.n++
	b := func(x bool) float64 {
		if x {
			return 1
		}
		return 0
	}
	m.hit1 += b(hitsAt(1) > 0)
	m.hit5 += b(hitsAt(5) > 0)
	m.hit10 += b(hitsAt(10) > 0)
	m.p5 += float64(hitsAt(5)) / 5
	m.p10 += float64(hitsAt(10)) / 10
	m.r10 += float64(hitsAt(10)) / float64(ng)
	m.r50 += float64(hitsAt(50)) / float64(ng)

	cs := triage.Locate(lg, score, o)
	in, tot := 0, 0
	for _, c := range cs {
		for j := c.Start; j <= c.End; j++ {
			tot++
			if gold[j] {
				in++
			}
		}
	}
	m.located += b(in > 0)
	if tot > 0 {
		p, r := float64(in)/float64(tot), float64(in)/float64(ng)
		m.cp += p
		m.cr += r
		if in > 0 {
			m.cf1 += 2 * p * r / (p + r)
		}
		m.cover50 += b(r >= 0.5)
	}
	m.chunkLines += float64(tot)
	m.chunkB += float64(len(triage.ChunkText(lg, cs, triage.MaxChunkBytes)))
	return cs
}

func (m *locMetrics) row(name string) string {
	n := float64(max(1, m.n))
	return fmt.Sprintf("| %s | %.3f | %.3f | %.3f | %.3f | %.3f | %.3f | %.3f | **%.3f** | %.3f | %.3f | %.3f | %.3f | %.0f | %.0f |",
		name, m.hit1/n, m.hit5/n, m.hit10/n, m.p5/n, m.p10/n, m.r10/n, m.r50/n, m.located/n, m.cover50/n, m.cp/n, m.cr/n, m.cf1/n, m.chunkLines/n, m.chunkB/n/4)
}

const locHeader = "| method | hit@1 | hit@5 | hit@10 | P@5 | P@10 | R@10 | R@50 | located | covers ≥50 % | chunk P | chunk R | chunk F1 | lines | ~tokens |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"

// catMetrics: exactitud, macro-F1 y cobertura/precisión con umbral.
type catMetrics struct {
	gold, pred    []string
	conf          []bool
	confN, confOK int
}

func (c *catMetrics) add(gold, pred string, confident bool) {
	c.gold = append(c.gold, gold)
	c.pred = append(c.pred, pred)
	c.conf = append(c.conf, confident)
	if confident {
		c.confN++
		if gold == pred {
			c.confOK++
		}
	}
}

func (c *catMetrics) acc() float64 {
	ok := 0
	for i := range c.gold {
		if c.gold[i] == c.pred[i] {
			ok++
		}
	}
	return float64(ok) / float64(max(1, len(c.gold)))
}

// macroF1 promedia sobre las clases presentes en el oro.
func (c *catMetrics) macroF1() float64 {
	classes := map[string]bool{}
	for _, g := range c.gold {
		classes[g] = true
	}
	var sum float64
	for k := range classes {
		tp, fp, fn := 0, 0, 0
		for i := range c.gold {
			switch {
			case c.pred[i] == k && c.gold[i] == k:
				tp++
			case c.pred[i] == k:
				fp++
			case c.gold[i] == k:
				fn++
			}
		}
		if tp > 0 {
			p, r := float64(tp)/float64(tp+fp), float64(tp)/float64(tp+fn)
			sum += 2 * p * r / (p + r)
		}
	}
	return sum / float64(max(1, len(classes)))
}

func (c *catMetrics) row(name string, withConf bool) string {
	cov, prec := "—", "—"
	if withConf {
		cov = fmt.Sprintf("%.1f %%", 100*float64(c.confN)/float64(max(1, len(c.gold))))
		if c.confN > 0 {
			prec = fmt.Sprintf("%.3f", float64(c.confOK)/float64(c.confN))
		}
	}
	return fmt.Sprintf("| %s | %.3f | %.3f | %s | %s |", name, c.acc(), c.macroF1(), cov, prec)
}

const catHeader = "| method | accuracy | macro-F1 | answers confidently | precision when confident |\n|---|---|---|---|---|"

// logResult es la línea de -out por log (para mirar después los fallos).
type logResult struct {
	ID        string            `json:"id"`
	Repo      string            `json:"repo"`
	Lines     int               `json:"lines"`
	Scored    int               `json:"scored"`
	Gold      [][2]int          `json:"gold"`
	Chunks    [][2]int          `json:"chunks"`
	Located   bool              `json:"located"`
	Category  string            `json:"category"`
	Pred      string            `json:"pred"`
	Prob      float64           `json:"prob"`
	Confident bool              `json:"confident"`
	VON       *triage.VONAnswer `json:"von,omitempty"`
	MS        struct {
		Read, Features, Lines, Locate, Category, VON float64
	} `json:"ms"`
}

func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	dataDir := fs.String("data", "", "folder written by `ci-triage data`")
	set := fs.String("set", "test", "which set: train, valid or test")
	manifest := fs.String("manifest", "", "a logs-*.jsonl manifest instead of -data/-set (e.g. private logs annotated by hand)")
	gw := gatewayFlags(fs)
	workers := fs.Int("workers", 8, "classify requests in flight per log")
	vonMode := fs.String("von", "off", "VON on the located chunk: off, escalated (only when Chispa doubts) or all")
	vonMax := fs.Int("von-max", 0, "call VON on at most this many logs (0 = no cap)")
	out := fs.String("out", "", "write one JSON line per log here")
	quiet := fs.Bool("q", false, "no per-log progress")
	maxLogs := fs.Int("max-logs", 0, "evaluate only the first N logs (0 = all)")
	sweep := fs.Bool("sweep", false, "also try a grid of chunk options on the same scores (how the defaults were chosen, on valid)")
	misses := fs.Bool("misses", false, "print the gold and the chosen lines of every log the locator missed")
	_ = fs.Parse(args)

	path := *manifest
	if path == "" {
		if *dataDir == "" {
			return errors.New("usage: ci-triage eval -data DIR [-set test] | -manifest logs.jsonl")
		}
		path = filepath.Join(*dataDir, "logs-"+*set+".jsonl")
	}
	recs, err := readRecords(path)
	if err != nil {
		return err
	}
	if *maxLogs > 0 && *maxLogs < len(recs) {
		recs = recs[:*maxLogs]
	}
	g, err := gw.dial(*workers)
	if err != nil {
		return err
	}
	ctx := context.Background()
	opts := gw.options()
	opts.Workers = *workers

	var w *json.Encoder
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = json.NewEncoder(f)
	}

	rankers := []ranker{
		{"last 10 lines", lastN(10)},
		{"last 30 lines", lastN(30)},
		{"regex/keywords", keywordScores},
	}
	loc := map[string]*locMetrics{"Chispa": {}}
	for _, r := range rankers {
		loc[r.name] = &locMetrics{}
	}
	var (
		catE2E, catGold, rulesE2E, rulesGold, cascade, vonOnly catMetrics
		perLog, linesMS                                        []float64
		totalLines, scoredLines, vonCalls, vonTokens           int
		vonMS                                                  []float64
		t0                                                     = time.Now()
	)
	type kept struct {
		lg    *triage.Log
		score []float64
		gold  []bool
	}
	var sweepSet []kept
	withGold := 0
	for k, rec := range recs {
		var res logResult
		res.ID, res.Repo, res.Gold, res.Category = rec.ID, rec.Repo, rec.Gold, rec.Category
		tr := time.Now()
		lg, err := triage.ReadFile(rec.Path, triage.DefaultLimits)
		if err != nil {
			return fmt.Errorf("%s: %w", rec.Path, err)
		}
		res.MS.Read = ms(time.Since(tr))
		if rec.Lines != 0 && rec.Lines != len(lg.Lines) {
			return fmt.Errorf("%s: %d lines, the manifest says %d (was it built with another reader?)", rec.Path, len(lg.Lines), rec.Lines)
		}
		res.Lines = len(lg.Lines)
		gold := make([]bool, len(lg.Lines))
		for _, r := range rec.Gold {
			for j := r[0]; j <= r[1] && j < len(gold); j++ {
				gold[j] = true
			}
		}

		// Capa 1: Chispa por línea, por el gateway.
		tf := time.Now()
		in := triage.Features(lg)
		res.MS.Features = ms(time.Since(tf))
		tl := time.Now()
		score, _, err := g.ScoreLines(ctx, opts.LinesTask, len(lg.Lines), in, *workers)
		if err != nil {
			return fmt.Errorf("%s: %w", rec.ID, err)
		}
		res.MS.Lines = ms(time.Since(tl))
		linesMS = append(linesMS, res.MS.Lines)
		totalLines += len(lg.Lines)
		scoredLines += len(in)
		res.Scored = len(in)
		tc := time.Now()
		var cs []triage.Chunk
		if len(rec.Gold) > 0 {
			withGold++
			cs = loc["Chispa"].add(score, gold, lg, opts.Chunk)
			for _, r := range rankers {
				loc[r.name].add(r.score(lg), gold, lg, opts.Chunk)
			}
		} else {
			cs = triage.Locate(lg, score, opts.Chunk)
		}
		res.MS.Locate = ms(time.Since(tc))
		if *sweep && len(rec.Gold) > 0 {
			sweepSet = append(sweepSet, kept{lg, score, gold})
		}
		for _, c := range cs {
			res.Chunks = append(res.Chunks, [2]int{c.Start, c.End})
			for j := c.Start; j <= c.End; j++ {
				res.Located = res.Located || gold[j]
			}
		}
		if *misses && len(rec.Gold) > 0 && !res.Located {
			fmt.Printf("── missed %s %s (%d lines)\n", rec.Repo, rec.ID, len(lg.Lines))
			for _, r := range rec.Gold[:min(2, len(rec.Gold))] {
				for j := r[0]; j <= min(r[1], r[0]+2); j++ {
					fmt.Printf("   gold %5d  %s\n", j+1, triage.Clip(lg.Lines[j].Text, 150))
				}
			}
			for _, c := range cs {
				for j := c.Start; j <= min(c.End, c.Start+2); j++ {
					fmt.Printf("   got  %5d  %.2f %s\n", j+1, score[j], triage.Clip(lg.Lines[j].Text, 140))
				}
			}
		}
		perLog = append(perLog, res.MS.Read+res.MS.Features+res.MS.Lines+res.MS.Locate)

		// Capa 2: la categoría del trozo localizado (de punta a punta) y, para
		// separar errores, la del trozo de oro.
		text := triage.ChunkText(lg, cs, triage.MaxChunkBytes)
		tk := time.Now()
		c, err := g.Classify(ctx, opts.CategoryTask, text, triage.CategoryFields(lg, cs))
		if err != nil {
			return fmt.Errorf("%s: category: %w", rec.ID, err)
		}
		res.MS.Category = ms(time.Since(tk))
		res.Pred, res.Prob, res.Confident = c.Label, c.Prob, !c.Escalate
		catE2E.add(rec.Category, c.Label, !c.Escalate)
		rulesE2E.add(rec.Category, triage.RuleCategory(text), true)
		if len(rec.Gold) > 0 {
			gcs := make([]triage.Chunk, 0, len(rec.Gold))
			for _, r := range rec.Gold {
				gcs = append(gcs, triage.Chunk{Start: r[0], End: r[1]})
			}
			gtext := triage.ChunkText(lg, gcs, triage.MaxChunkBytes)
			gc, err := g.Classify(ctx, opts.CategoryTask, gtext, triage.CategoryFields(lg, gcs))
			if err != nil {
				return fmt.Errorf("%s: category: %w", rec.ID, err)
			}
			catGold.add(rec.Category, gc.Label, !gc.Escalate)
			rulesGold.add(rec.Category, triage.RuleCategory(gtext), true)
		}

		// Capa 3: VON, si se pide, sobre el mismo trozo.
		final := c.Label
		ask := *vonMode == "all" || *vonMode == "escalated" && c.Escalate
		if ask && opts.SummaryTask != "" && (*vonMax == 0 || vonCalls < *vonMax) {
			tv := time.Now()
			ans, err := triage.AskVON(ctx, g, opts.SummaryTask, text, c)
			res.MS.VON = ms(time.Since(tv))
			vonCalls++
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: von: %v\n", rec.ID, err)
			} else {
				res.VON = ans
				vonMS = append(vonMS, res.MS.VON)
				vonTokens += ans.PromptTokens + ans.CompletionTokens
				vonOnly.add(rec.Category, ans.Category, true)
				if c.Escalate {
					final = ans.Category
				}
			}
		}
		cascade.add(rec.Category, final, true)
		if w != nil {
			_ = w.Encode(res)
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "\r%d/%d logs", k+1, len(recs))
		}
	}
	wall := time.Since(t0)
	if !*quiet {
		fmt.Fprintln(os.Stderr)
	}

	fmt.Printf("## %s: %d logs, %d with gold lines, %d lines (%d classified)\n\n", filepath.Base(path), len(recs), withGold, totalLines, scoredLines)
	if withGold > 0 {
		fmt.Println("### Locator (logs with gold lines)")
		fmt.Println()
		fmt.Println(locHeader)
		fmt.Println(loc["Chispa"].row("**Chispa**"))
		for _, r := range rankers {
			fmt.Println(loc[r.name].row(r.name))
		}
		fmt.Println()
	}
	if *sweep {
		fmt.Println("### Chunk options (same Chispa scores)")
		fmt.Println()
		fmt.Println("| chunks | rel | max lines | second | gap | located | covers ≥50 % | chunk F1 | ~tokens |\n|---|---|---|---|---|---|---|---|---|")
		for _, mc := range []int{2, 3} {
			for _, rel := range []float64{0.2, 0.35, 0.5} {
				for _, ml := range []int{20, 30} {
					for _, sec := range []float64{0.5, 0.65, 0.8} {
						for _, gap := range []int{3, 5} {
							o := triage.ChunkOptions{MaxChunks: mc, MaxLines: ml, MaxBytes: triage.MaxChunkBytes, Rel: rel, Second: sec, Gap: gap}
							var m locMetrics
							for _, x := range sweepSet {
								m.add(x.score, x.gold, x.lg, o)
							}
							n := float64(max(1, m.n))
							fmt.Printf("| %d | %.2f | %d | %.2f | %d | %.3f | %.3f | %.3f | %.0f |\n", mc, rel, ml, sec, gap, m.located/n, m.cover50/n, m.cf1/n, m.chunkB/n/4)
						}
					}
				}
			}
		}
		fmt.Println()
	}
	fmt.Println("### Category")
	fmt.Println()
	fmt.Println(catHeader)
	fmt.Println(catE2E.row("**Chispa**, located chunk (end to end)", true))
	fmt.Println(rulesE2E.row("rules, located chunk", false))
	if len(catGold.gold) > 0 {
		fmt.Println(catGold.row("Chispa, gold chunk", true))
		fmt.Println(rulesGold.row("rules, gold chunk", false))
	}
	if vonCalls > 0 {
		fmt.Println(vonOnly.row(fmt.Sprintf("VON alone on the %d logs it saw", len(vonOnly.gold)), false))
		fmt.Println(cascade.row("Chispa → VON (VON answers what Chispa doubts)", false))
	}
	fmt.Println()
	fmt.Printf("### Latency (%d requests in flight per log)\n\n", *workers)
	sort.Float64s(perLog)
	fmt.Printf("- per log, read + features + Chispa on every line + chunk: p50 %.1f ms, p90 %.1f ms, p99 %.1f ms, max %.1f ms\n",
		pct(perLog, 50), pct(perLog, 90), pct(perLog, 99), pct(perLog, 100))
	var lsum float64
	for _, x := range linesMS {
		lsum += x
	}
	fmt.Printf("- lines through the gateway: %.0f lines/s (%d lines in %.1f s of scoring)\n", float64(scoredLines)/(lsum/1000), scoredLines, lsum/1000)
	fmt.Printf("- wall time of the whole evaluation: %.1f s\n", wall.Seconds())
	if len(vonMS) > 0 {
		sort.Float64s(vonMS)
		fmt.Printf("- VON: %d calls (%.1f %% of logs), p50 %.0f ms, p90 %.0f ms, max %.0f ms; %.0f tokens per call\n",
			vonCalls, 100*float64(vonCalls)/float64(len(recs)), pct(vonMS, 50), pct(vonMS, 90), pct(vonMS, 100), float64(vonTokens)/float64(len(vonMS)))
	}
	return nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

// lastN: la línea base «mira el final del log»: las N últimas líneas no
// vacías, más puntuación cuanto más al final.
func lastN(n int) func(lg *triage.Log) []float64 {
	return func(lg *triage.Log) []float64 {
		s := make([]float64, len(lg.Lines))
		k := 0
		for i := len(lg.Lines) - 1; i >= 0 && k < n; i-- {
			if strings.TrimSpace(lg.Lines[i].Text) == "" {
				continue
			}
			s[i] = 1 - float64(k)/float64(2*n)
			k++
		}
		return s
	}
}

// keywordScores: la línea base de expresiones regulares (error, fail,
// exception…), la más tardía primero.
func keywordScores(lg *triage.Log) []float64 {
	s := make([]float64, len(lg.Lines))
	for i, l := range lg.Lines {
		s[i] = triage.KeywordScore(l, i, len(lg.Lines))
	}
	return s
}
