package logchunks

import (
	"fmt"
	"testing"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

func lines(ss ...string) []triage.Line {
	out := make([]triage.Line, len(ss))
	for i, s := range ss {
		out[i] = triage.Line{Text: s}
	}
	return out
}

// El trozo del XML perdió los '<', lleva colores como "[31m" sueltos y una
// línea recortada; aun así casa con las líneas del log, vacías incluidas.
func TestGold(t *testing.T) {
	lg := lines("setup", "1) test foo", "", "   Expected <a> got <b>  ", "   at main (x.js:1)", "done")
	chunk := "1) test foo\n[31m Expected a> got b>[0m\n at main"
	g, n := Gold(lg, chunk)
	if n != 1 || fmt.Sprint(g) != "[false true true true true false]" {
		t.Fatalf("%d %v", n, g)
	}
	if _, n := Gold(lg, "not in the log"); n != 0 {
		t.Fatal("matched a chunk that is not there")
	}
}

func TestSplitByRepo(t *testing.T) {
	var bs []Build
	for r := range 20 {
		for i := range 3 {
			bs = append(bs, Build{Repo: fmt.Sprintf("o@r%d", r), ID: fmt.Sprint(r*10 + i)})
		}
	}
	tr, va, te := Split(bs, "s", 20, 20)
	if len(tr)+len(va)+len(te) != 60 || len(te) != 12 || len(va) != 12 {
		t.Fatalf("%d %d %d", len(tr), len(va), len(te))
	}
	seen := map[string]int{}
	for k, set := range [][]Build{tr, va, te} {
		for _, b := range set {
			if s, ok := seen[b.Repo]; ok && s != k {
				t.Fatalf("repo %s in two sets", b.Repo)
			}
			seen[b.Repo] = k
		}
	}
}
