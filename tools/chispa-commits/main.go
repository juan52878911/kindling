// chispa-commits construye un conjunto de datos de evaluación para Chispa a partir
// del historial de git de repositorios locales, usando el prefijo de
// Conventional Commits (feat, fix, docs…) como etiqueta débil, y calcula las
// líneas base con las que compararlo. Nada se descarga: los datos salen de los
// repos que ya hay en la máquina y no se guardan en este repositorio.
//
//	go run ./tools/chispa-commits build -out DIR REPO...
//	go run ./tools/chispa-commits baseline -train DIR/train.jsonl -test DIR/test.jsonl
//
// El prefijo (con su ámbito) se QUITA del texto: si no, el problema es leer las
// cuatro primeras letras. También se quitan de los cuerpos las líneas que
// empiezan por un prefijo (los squash merges listan los commits que juntan) y
// los trailers (Signed-off-by, Co-authored-by…).
//
// Reparto sin fugas: por repo, en orden temporal, el 70 % más antiguo entrena,
// el 10 % siguiente valida y el 20 % más reciente se evalúa. Además un reparto
// entre repos: uno entero (-holdout) solo en test. Los textos duplicados se
// quedan en su primera aparición, para que no crucen de un lado a otro.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/chispa"
)

var types = []string{"build", "chore", "ci", "docs", "feat", "fix", "perf", "refactor", "style", "test"}

var (
	// feat(scope)!: resto — el tipo sin distinguir mayúsculas.
	prefixRe  = regexp.MustCompile(`(?i)^\s*(feat|fix|docs|refactor|test|chore|perf|ci|build|style)(\([^)]*\))?!?:\s*`)
	bulletRe  = regexp.MustCompile(`(?i)^\s*[-*]?\s*(feat|fix|docs|refactor|test|chore|perf|ci|build|style)(\([^)]*\))?!?:`)
	trailerRe = regexp.MustCompile(`(?i)^\s*(signed-off-by|co-authored-by|reviewed-by|acked-by|tested-by|reported-by|cc|change-id|fixes|closes|refs?)\s*:`)
)

// maxGitOutput acota lo que se lee de `git log`: el historial de bun con
// numstat son ~decenas de MB; un repo patológico no se come la memoria.
const maxGitOutput = 1 << 30

type row struct {
	Text   string         `json:"text"`
	Label  string         `json:"label"`
	Fields map[string]any `json:"fields,omitempty"`
	Repo   string         `json:"repo"`
	Time   int64          `json:"time"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = cmdBuild(os.Args[2:])
	case "baseline":
		err = cmdBaseline(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  chispa-commits build -out DIR [-min 50] [-body 300] [-holdout NAME] REPO...
  chispa-commits baseline -train train.jsonl -test test.jsonl [-json]
`)
	os.Exit(2)
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	out := fs.String("out", "", "output directory (required)")
	minConv := fs.Int("min", 50, "skip repos with fewer conventional commits than this")
	body := fs.Int("body", 300, "bytes of the commit body kept after the subject")
	holdout := fs.String("holdout", "", "repo (directory name) kept entirely for the cross-repo test split")
	trainPct := fs.Int("train-pct", 70, "oldest percent of each repo used for training")
	validPct := fs.Int("valid-pct", 10, "next percent used for validation (early stopping, calibration, τ); the rest is test")
	fs.Parse(args)
	if *out == "" || fs.NArg() == 0 {
		return errors.New("usage: chispa-commits build -out DIR REPO...")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	seen := map[string]bool{}
	byRepo := map[string][]row{}
	var repos []string
	for _, dir := range fs.Args() {
		name := filepath.Base(dir)
		rows, total, err := readRepo(dir, *body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", name, err)
			continue
		}
		if len(rows) < *minConv {
			fmt.Fprintf(os.Stderr, "skip %s: %d conventional of %d commits\n", name, len(rows), total)
			continue
		}
		// Del más antiguo al más reciente; los duplicados se quedan en el primero.
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time < rows[j].Time })
		kept := rows[:0]
		for _, r := range rows {
			k := strings.ToLower(strings.Join(strings.Fields(r.Text), " "))
			if seen[k] {
				continue
			}
			seen[k] = true
			kept = append(kept, r)
		}
		fmt.Fprintf(os.Stderr, "%s: %d conventional of %d commits, %d after dedup\n", name, len(rows), total, len(kept))
		byRepo[name] = kept
		repos = append(repos, name)
	}
	sort.Strings(repos)

	var tr, va, te, xtr, xva, xte []row
	for _, name := range repos {
		rows := byRepo[name]
		n := len(rows)
		a, b := n**trainPct/100, n*(*trainPct+*validPct)/100
		tr, va, te = append(tr, rows[:a]...), append(va, rows[a:b]...), append(te, rows[b:]...)
		if name == *holdout {
			xte = append(xte, rows...)
		} else {
			c := n * 90 / 100
			xtr, xva = append(xtr, rows[:c]...), append(xva, rows[c:]...)
		}
	}
	files := map[string][]row{"train": tr, "valid": va, "test": te}
	if *holdout != "" {
		if len(xte) == 0 {
			return fmt.Errorf("holdout repo %q not found among the kept repos", *holdout)
		}
		files["xrepo-train"], files["xrepo-valid"], files["xrepo-test"] = xtr, xva, xte
	}
	for name, rows := range files {
		if err := writeJSONL(filepath.Join(*out, name+".jsonl"), rows); err != nil {
			return err
		}
	}
	printStats(os.Stdout, files)
	return nil
}

func readRepo(dir string, bodyBytes int) ([]row, int, error) {
	cmd := exec.Command("git", "-C", dir, "log", "--no-merges", "--no-renames",
		"--format=%x1e%H%x1f%at%x1f%s%x1f%b%x1f", "--numstat")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	data, rerr := io.ReadAll(io.LimitReader(stdout, maxGitOutput))
	werr := cmd.Wait()
	if rerr != nil {
		return nil, 0, rerr
	}
	if werr != nil {
		return nil, 0, fmt.Errorf("git log: %w", werr)
	}
	name := filepath.Base(dir)
	var out []row
	total := 0
	for _, rec := range bytes.Split(data, []byte{0x1e}) {
		parts := strings.SplitN(string(rec), "\x1f", 5)
		if len(parts) < 5 {
			continue
		}
		total++
		m := prefixRe.FindStringSubmatch(parts[2])
		if m == nil {
			continue
		}
		label := strings.ToLower(m[1])
		subject := strings.TrimSpace(parts[2][len(m[0]):])
		if subject == "" {
			continue
		}
		ts, _ := strconv.ParseInt(parts[1], 10, 64)
		text := subject
		if b := cleanBody(parts[3], bodyBytes); b != "" {
			text += "\n" + b
		}
		out = append(out, row{Text: text, Label: label, Fields: numstatFields(parts[4]), Repo: name, Time: ts})
	}
	return out, total, nil
}

// cleanBody quita trailers y líneas con prefijo convencional (fuga de la
// etiqueta en los squash merges) y recorta a n bytes sin partir una runa.
func cleanBody(b string, n int) string {
	var keep []string
	for _, l := range strings.Split(b, "\n") {
		if trailerRe.MatchString(l) || bulletRe.MatchString(l) {
			continue
		}
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	s := strings.Join(keep, "\n")
	if len(s) > n {
		s = s[:n]
		for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 {
			s = s[:len(s)-1]
		}
		if len(s) > 0 && s[len(s)-1] >= 0xC0 {
			s = s[:len(s)-1]
		}
	}
	return s
}

// numstatFields resume los ficheros tocados: cuántos, cuántas líneas, qué
// extensiones y qué directorios de primer nivel (los ocho más frecuentes).
func numstatFields(s string) map[string]any {
	files, churn := 0, 0
	exts, dirs := map[string]int{}, map[string]int{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.SplitN(l, "\t", 3)
		if len(f) != 3 {
			continue
		}
		files++
		a, _ := strconv.Atoi(f[0]) // "-" en binarios: 0
		d, _ := strconv.Atoi(f[1])
		churn += a + d
		p := f[2]
		ext := strings.ToLower(filepath.Ext(p))
		if ext == "" {
			ext = "(none)"
		}
		exts[ext]++
		dir := "(root)"
		if i := strings.IndexByte(p, '/'); i > 0 {
			dir = p[:i]
		}
		dirs[dir]++
	}
	if files == 0 {
		return nil
	}
	return map[string]any{"files": files, "churn": churn, "ext": top(exts, 8), "dir": top(dirs, 8)}
}

func top(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys[:min(n, len(keys))]
}

func writeJSONL(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func printStats(w io.Writer, files map[string][]row) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "%-12s %6s", "split", "n")
	for _, t := range types {
		fmt.Fprintf(w, " %6s", t)
	}
	fmt.Fprintln(w)
	for _, n := range names {
		c := map[string]int{}
		for _, r := range files[n] {
			c[r.Label]++
		}
		fmt.Fprintf(w, "%-12s %6d", n, len(files[n]))
		for _, t := range types {
			fmt.Fprintf(w, " %6d", c[t])
		}
		fmt.Fprintln(w)
	}
}

// Línea base de palabras clave: la regla que escribiría alguien en diez
// minutos. Se evalúa en orden y la primera que casa gana; si ninguna casa, se
// predice la clase mayoritaria y cuenta como «escalado» (así se compara la
// cobertura de las reglas con la de Chispa).
var keywordRules = []struct {
	label string
	words []string
}{
	{"docs", strings.Fields("doc docs documentation readme typo typos comment comments guide changelog wording")},
	{"test", strings.Fields("test tests testing spec specs flaky coverage snapshot")},
	{"ci", strings.Fields("ci workflow workflows actions pipeline gha")},
	{"perf", strings.Fields("perf performance faster speed speedup optimize optimise optimization slow")},
	{"refactor", strings.Fields("refactor refactoring rename move cleanup simplify restructure extract reorganize")},
	{"style", strings.Fields("format formatting lint prettier whitespace style fmt")},
	{"build", strings.Fields("build cmake makefile deps dependency dependencies bump compile compiler toolchain docker")},
	{"fix", strings.Fields("fix fixes fixed bug crash error issue broken wrong regression panic incorrect handle")},
	{"feat", strings.Fields("add adds added implement support new introduce allow enable")},
	{"chore", strings.Fields("release version update chore upgrade")},
}

func keywordPredict(text string) (string, bool) {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		words[w] = true
	}
	for _, r := range keywordRules {
		for _, w := range r.words {
			if words[w] {
				return r.label, true
			}
		}
	}
	return "", false
}

func cmdBaseline(args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	trainPath := fs.String("train", "", "training JSONL (for the majority class)")
	testPath := fs.String("test", "", "test JSONL")
	asJSON := fs.Bool("json", false, "JSON output")
	fs.Parse(args)
	if *trainPath == "" || *testPath == "" {
		return errors.New("usage: chispa-commits baseline -train train.jsonl -test test.jsonl")
	}
	trainEx, err := readFile(*trainPath)
	if err != nil {
		return err
	}
	testEx, err := readFile(*testPath)
	if err != nil {
		return err
	}
	labels := append([]string(nil), types...)
	idx := map[string]int{}
	for i, l := range labels {
		idx[l] = i
	}
	counts := make([]int, len(labels))
	for _, ex := range trainEx {
		if i, ok := idx[ex.Label]; ok {
			counts[i]++
		}
	}
	maj := 0
	for i := range counts {
		if counts[i] > counts[maj] {
			maj = i
		}
	}
	gold := func(l string) int {
		if i, ok := idx[l]; ok {
			return i
		}
		return -1
	}
	var majRows, kwRows []chispa.Scored
	for _, ex := range testEx {
		g := gold(ex.Label)
		majRows = append(majRows, chispa.Scored{Gold: g, Pred: maj, Confident: true})
		p, ok := maj, false
		if l, hit := keywordPredict(ex.Text); hit {
			p, ok = idx[l], true
		}
		kwRows = append(kwRows, chispa.Scored{Gold: g, Pred: p, Confident: ok})
	}
	reports := map[string]*chispa.Report{
		"majority": chispa.Score(labels, majRows, nil, false),
		"keywords": chispa.Score(labels, kwRows, nil, false),
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(reports)
	}
	fmt.Printf("== majority class (%s) ==\n", labels[maj])
	reports["majority"].WriteText(os.Stdout)
	fmt.Println("\n== keyword rules (confident = a rule fired) ==")
	reports["keywords"].WriteText(os.Stdout)
	return nil
}

func readFile(path string) ([]chispa.Example, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return chispa.ReadExamples(f, 0, true)
}
