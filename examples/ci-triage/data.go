package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/examples/ci-triage/logchunks"
	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

// LogRecord es un log del conjunto con su oro: una línea del manifiesto
// logs-<conjunto>.jsonl. Es lo que lee `eval`; los logs se vuelven a leer de
// Path con el mismo lector, así que los índices de línea casan.
type LogRecord struct {
	ID       string   `json:"id"`
	Repo     string   `json:"repo"`
	Lang     string   `json:"lang,omitempty"`
	Path     string   `json:"path"`
	Lines    int      `json:"lines"`
	Gold     [][2]int `json:"gold"`     // tramos [inicio, fin] (índices desde 0) que explican el fallo
	Category string   `json:"category"` // la categoría con la que se evalúa
	// RuleCategory es la de las reglas; Hand dice si Category la revisó una
	// persona (el conjunto de prueba entero).
	RuleCategory string `json:"rule_category"`
	Hand         bool   `json:"hand,omitempty"`
}

// Negativos por log en el conjunto de líneas: todos los positivos, todos los
// negativos difíciles (cerca del trozo, con marcas de error o en rojo) y como
// mucho estos al azar. Sin submuestreo, 1,3 M de líneas de las que el 0,5 %
// son positivas: entrenar costaría 20× más para aprender lo mismo.
// Igual con los positivos: un log cuyo «trozo» es un diff de 1 600 líneas no
// puede pesar más que cien logs normales.
const (
	randomNegatives = 200
	maxPositives    = 60
	nearWindow      = 10
)

func cmdData(args []string) error {
	fs := flag.NewFlagSet("data", flag.ExitOnError)
	root := fs.String("logchunks", "", "the unzipped LogChunks folder (build-data.sh downloads and checks it)")
	out := fs.String("out", "", "output folder")
	seed := fs.String("seed", "ci-triage-1", "seed of the split by repository")
	validPct := fs.Int("valid-pct", 20, "repositories for validation (%)")
	testPct := fs.Int("test-pct", 20, "repositories for test (%)")
	labels := fs.String("labels", "", "hand-checked categories of the test logs (TSV: id, category, note)")
	_ = fs.Parse(args)
	if *root == "" || *out == "" {
		return errors.New("usage: ci-triage data -logchunks DIR -out DIR [-labels test-categories.tsv]")
	}
	hand := map[string]string{}
	if *labels != "" {
		var err error
		if hand, err = readHandLabels(*labels); err != nil {
			return err
		}
	}
	bs, err := logchunks.Load(*root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	train, valid, test := logchunks.Split(bs, *seed, *validPct, *testPct)
	sets := []struct {
		name string
		bs   []logchunks.Build
	}{{"train", train}, {"valid", valid}, {"test", test}}
	for _, s := range sets {
		st, err := writeSet(*out, s.name, s.bs, hand)
		if err != nil {
			return err
		}
		fmt.Printf("%-5s %3d logs from %2d repos, %d without gold lines; %d lines (%d explain); categories %v\n",
			s.name, len(s.bs), st.repos, st.noGold, st.lines, st.pos, st.cats)
		if s.name == "test" && len(hand) > 0 {
			fmt.Printf("      hand-checked categories: %d of %d; the rules agree on %d (%.1f %%)\n",
				st.hand, len(s.bs), st.agree, 100*float64(st.agree)/float64(max(1, st.hand)))
		}
	}
	return nil
}

type setStats struct {
	repos, noGold, lines, pos, hand, agree int
	cats                                   map[string]int
}

func writeSet(dir, name string, bs []logchunks.Build, hand map[string]string) (setStats, error) {
	st := setStats{cats: map[string]int{}}
	files := map[string]*bufio.Writer{}
	var closers []*os.File
	open := func(kind string) (*bufio.Writer, error) {
		if w := files[kind]; w != nil {
			return w, nil
		}
		f, err := os.Create(filepath.Join(dir, kind+"-"+name+".jsonl"))
		if err != nil {
			return nil, err
		}
		closers = append(closers, f)
		w := bufio.NewWriterSize(f, 1<<20)
		files[kind] = w
		return w, nil
	}
	defer func() {
		for _, f := range closers {
			f.Close()
		}
	}()
	emit := func(kind string, v any) error {
		w, err := open(kind)
		if err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		w.Write(b)
		return w.WriteByte('\n')
	}
	repos := map[string]bool{}
	for _, b := range bs {
		repos[b.Repo] = true
		lg, err := triage.ReadFile(b.LogPath, triage.DefaultLimits)
		if err != nil {
			return st, fmt.Errorf("%s: %w", b.LogPath, err)
		}
		gold, found := logchunks.Gold(lg.Lines, b.Chunk)
		ranges := goldRanges(gold)
		rec := LogRecord{ID: b.ID, Repo: b.Repo, Lang: b.Lang, Path: b.LogPath, Lines: len(lg.Lines), Gold: ranges}

		// El texto de la categoría sale de las líneas del log (sin ANSI), igual
		// que en producción; si el trozo no se encontró, del XML normalizado.
		var text string
		var fields map[string]any
		if found > 0 {
			cs := make([]triage.Chunk, 0, len(ranges))
			for _, r := range ranges {
				cs = append(cs, triage.Chunk{Start: r[0], End: r[1]})
			}
			text = triage.ChunkText(lg, cs, triage.MaxChunkBytes)
			fields = triage.CategoryFields(lg, cs)
		} else {
			st.noGold++
			var lines []string
			for l := range strings.SplitSeq(b.Chunk, "\n") {
				lines = append(lines, logchunks.Norm(l))
			}
			text = truncate(strings.Join(lines, "\n"), triage.MaxChunkBytes)
		}
		rec.RuleCategory = triage.RuleCategory(text)
		rec.Category = rec.RuleCategory
		if h, ok := hand[b.ID]; ok {
			rec.Category, rec.Hand = h, true
			st.hand++
			if h == rec.RuleCategory {
				st.agree++
			}
		}
		st.cats[rec.Category]++
		if err := emit("logs", rec); err != nil {
			return st, err
		}
		if err := emit("category", map[string]any{"text": text, "label": rec.Category, "fields": fields, "id": b.ID, "repo": b.Repo}); err != nil {
			return st, err
		}

		// Líneas para el localizador (solo en entrenamiento y validación; la
		// prueba se mide sobre los logs enteros con `eval`).
		if name == "test" || found == 0 {
			continue
		}
		in := triage.Features(lg)
		near := make([]bool, len(gold))
		for i, g := range gold {
			if g {
				for j := max(0, i-nearWindow); j <= min(len(gold)-1, i+nearWindow); j++ {
					near[j] = true
				}
			}
		}
		others, positives := 0, 0
		for _, x := range in {
			if gold[x.Index] {
				positives++
			}
			if !gold[x.Index] && !near[x.Index] && !hard(x) {
				others++
			}
		}
		for _, x := range in {
			label := "noise"
			switch {
			case gold[x.Index]:
				if positives > maxPositives && keep(b.ID, x.Index) >= float64(maxPositives)/float64(positives) {
					continue
				}
				label = triage.Explains
				st.pos++
			case near[x.Index], hard(x):
			default:
				if others > randomNegatives && keep(b.ID, x.Index) >= float64(randomNegatives)/float64(others) {
					continue
				}
			}
			st.lines++
			if err := emit("lines", map[string]any{"text": x.Text, "label": label, "fields": x.Fields, "id": b.ID}); err != nil {
				return st, err
			}
		}
	}
	st.repos = len(repos)
	for _, w := range files {
		if err := w.Flush(); err != nil {
			return st, err
		}
	}
	return st, nil
}

// hard: un negativo que se parece a un positivo (marcas de error o rojo).
func hard(x triage.LineInput) bool {
	if r, _ := x.Fields["red"].(bool); r {
		return true
	}
	mk, _ := x.Fields["mk"].([]string)
	for _, m := range mk {
		if m == "error" || m == "fail" || m == "exception" || m == "assert" {
			return true
		}
	}
	return false
}

// keep da un número estable en [0,1) por línea: el submuestreo no depende del
// orden ni de la máquina.
func keep(id string, i int) float64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%d", id, i)
	return float64(h.Sum64()>>11) / float64(1<<53)
}

func goldRanges(gold []bool) [][2]int {
	var out [][2]int
	for i := 0; i < len(gold); i++ {
		if !gold[i] {
			continue
		}
		j := i
		for j+1 < len(gold) && gold[j+1] {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// readHandLabels lee el TSV de categorías revisadas a mano: id, categoría y
// una nota opcional; # empieza un comentario.
func readHandLabels(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			return nil, fmt.Errorf("%s:%d: want id<TAB>category[<TAB>note]", path, n)
		}
		id, c := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !triage.ValidHumanCategory(c) || c == triage.CatFlaky {
			return nil, fmt.Errorf("%s:%d: unknown category %q", path, n, c)
		}
		out[id] = c
	}
	return out, sc.Err()
}

// readRecords lee un manifiesto logs-*.jsonl.
func readRecords(path string) ([]LogRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []LogRecord
	sc := bufio.NewScanner(io.LimitReader(f, 64<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var r LogRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out, sc.Err()
}
