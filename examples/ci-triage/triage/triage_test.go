package triage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTravis(t *testing.T) {
	raw := "travis_fold:start:install.1\r\x1b[0K\x1b[33;1mInstalling\x1b[0m\n" +
		"Downloading 10%\rDownloading 100%\n" +
		"travis_fold:end:install.1\r\x1b[0K\n" +
		"\x1b[31mFAIL\x1b[0m test/foo.js\n" +
		"travis_time:end:abc:start=1,finish=2\r\x1b[0K\x1b[31;1mThe command \"npm test\" exited with 1.\x1b[0m\n"
	lg, err := Read(strings.NewReader(raw), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if lg.Format != "travis" {
		t.Fatalf("format %q", lg.Format)
	}
	want := []string{"Installing", "Downloading 100%", "", "FAIL test/foo.js", `The command "npm test" exited with 1.`}
	if len(lg.Lines) != len(want) {
		t.Fatalf("got %d lines: %+v", len(lg.Lines), lg.Lines)
	}
	for i, w := range want {
		if lg.Lines[i].Text != w {
			t.Errorf("line %d: %q, want %q", i, lg.Lines[i].Text, w)
		}
	}
	if lg.Lines[1].Section != "install" || lg.Lines[3].Section != "" {
		t.Errorf("sections: %q %q", lg.Lines[1].Section, lg.Lines[3].Section)
	}
	if !lg.Lines[3].Red || lg.Lines[1].Red {
		t.Error("red detection")
	}
}

func TestReadGitHub(t *testing.T) {
	raw := "build\tUNKNOWN STEP\t\ufeff2026-09-01T18:52:03.4695845Z ##[group]Run npm test\n" +
		"build\tUNKNOWN STEP\t2026-09-01T18:52:03.4697106Z > jest\n" +
		"build\tUNKNOWN STEP\t2026-09-01T18:52:04.0000000Z ##[endgroup]\n" +
		"build\tUNKNOWN STEP\t2026-09-01T18:52:05.0000000Z ##[error]Process completed with exit code 1.\n"
	lg, err := Read(strings.NewReader(raw), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if lg.Format != "github" || len(lg.Lines) != 3 {
		t.Fatalf("format %q, %d lines", lg.Format, len(lg.Lines))
	}
	if l := lg.Lines[1]; l.Text != "> jest" || l.Section != "Run npm test" || l.Job != "build" {
		t.Errorf("%+v", l)
	}
	if l := lg.Lines[2]; !l.Marked || l.Text != "Process completed with exit code 1." {
		t.Errorf("%+v", l)
	}
}

// Un log más grande que el tope se queda con su cola, sin la línea cortada.
func TestReadTail(t *testing.T) {
	var b bytes.Buffer
	for i := range 5000 {
		b.WriteString(strings.Repeat("x", 90))
		b.WriteString(" line ")
		b.WriteString(strings.Repeat("0", 4-len(itoa(i))) + itoa(i))
		b.WriteByte('\n')
	}
	lim := Limits{MaxBytes: 10_000, MaxLines: 50, MaxLineBytes: 64, MaxStream: 1 << 20}
	lg, err := Read(bytes.NewReader(b.Bytes()), lim)
	if err != nil {
		t.Fatal(err)
	}
	if !lg.Truncated || len(lg.Lines) != 50 {
		t.Fatalf("truncated %v, %d lines", lg.Truncated, len(lg.Lines))
	}
	if len(lg.Lines[0].Text) != 64 {
		t.Errorf("line not clipped: %d bytes", len(lg.Lines[0].Text))
	}
	// Por fichero: salta al final sin leer el principio.
	p := filepath.Join(t.TempDir(), "big.log")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	lg2, err := ReadFile(p, Limits{MaxBytes: 10_000, MaxLines: 1000, MaxLineBytes: 200})
	if err != nil {
		t.Fatal(err)
	}
	if !lg2.Truncated || !strings.HasSuffix(lg2.Lines[len(lg2.Lines)-1].Text, "4999") || !strings.HasPrefix(lg2.Lines[0].Text, "xxx") {
		t.Errorf("tail: first %q last %q", lg2.Lines[0].Text, lg2.Lines[len(lg2.Lines)-1].Text)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var s []byte
	for ; i > 0; i /= 10 {
		s = append([]byte{byte('0' + i%10)}, s...)
	}
	return string(s)
}

func TestLocate(t *testing.T) {
	score := make([]float64, 100)
	for i := range score {
		score[i] = 0.01
	}
	// Un tramo en 40-44 con un hueco flojo en 42, y un segundo casi tan bueno
	// en 90.
	score[40], score[41], score[42], score[43], score[44] = 0.9, 0.8, 0.05, 0.7, 0.95
	score[90] = 0.85
	cs := Locate(nil, score, DefaultChunkOptions)
	if len(cs) != 2 || cs[0].Start != 40 || cs[0].End != 44 || cs[1].Start != 90 {
		t.Fatalf("%+v", cs)
	}
	// Sin nada puntuado, nada.
	if cs := Locate(nil, make([]float64, 10), DefaultChunkOptions); len(cs) != 0 {
		t.Fatalf("%+v", cs)
	}
}

func TestFeaturesContext(t *testing.T) {
	lg := &Log{Lines: []Line{
		{Text: "Downloading deps"}, {Text: ""}, {Text: "AssertionError: expected 1 to equal 2", Red: true},
		{Text: "    at Context.<anonymous> (test/a.js:3:7)"}, {Text: `The command "npm test" exited with 1.`},
	}}
	in := Features(lg)
	if len(in) != 4 {
		t.Fatalf("%d inputs (blank lines are skipped)", len(in))
	}
	frame := in[2]
	if frame.Index != 3 || frame.Fields["frame"] != true || frame.Fields["dist"] != 1 || frame.Fields["toexit"] != 1 {
		t.Errorf("%+v", frame.Fields)
	}
	if prev, _ := frame.Fields["prev"].([]string); !contains(prev, "assert") {
		t.Errorf("prev marks %v", prev)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestRuleCategory(t *testing.T) {
	cases := map[string]string{
		"FAIL test/autoprefixer.test.js\nexpect(received).toEqual(expected)":                   CatTest,
		"src/a.rs:3:5: error[E0425]: cannot find value `x`":                                    CatBuild,
		"npm ERR! code E404\nnpm ERR! 404 Not Found - GET https://registry":                    CatDependency,
		"src/x.js:1 Replace `\"a\"` with `'a'` (prettier/prettier)":                            CatLint,
		"fatal: unable to access 'https://github.com/x/': Could not resolve host: github.com":  CatInfra,
		"No output has been received in the last 10m0s":                                        CatTimeout,
		"FATAL ERROR: Ineffective mark-compacts near heap limit JavaScript heap out of memory": CatResources,
		"install-jdk.sh: No such file or directory":                                            CatConfig,
		"all good": CatOther,
	}
	for in, want := range cases {
		if got := RuleCategory(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// La confirmación es un ejemplo de entrenamiento válido: text, label y fields
// con esos nombres, y nunca el log.
func TestFeedback(t *testing.T) {
	r := &Result{Chunk: "FAIL x", Category: CatTest, ChispaLabel: CatTest, Prob: 0.4, Layer: "chispa-unsure", Lines: 10,
		Chunks: []ChunkOut{{From: 3, To: 4}}, VON: &VONAnswer{Category: CatTest}}
	if _, err := NewFeedback(r, nil, "h", "nope", ""); err == nil {
		t.Fatal("unknown category accepted")
	}
	fb, err := NewFeedback(r, map[string]any{"cmd": []string{"npm"}}, "h", CatFlaky, strings.Repeat("n", 900))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fb.jsonl")
	fl, err := NewFeedbackLog(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(fb); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["text"] != "FAIL x" || m["label"] != CatFlaky || m["agreed"] != false || m["schema"] != FeedbackSchema || len(m["note"].(string)) != maxNote {
		t.Errorf("%v", m)
	}
	small, _ := NewFeedbackLog(filepath.Join(t.TempDir(), "s.jsonl"), 10)
	if err := small.Append(fb); err != ErrFeedbackFull {
		t.Errorf("cap: %v", err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}

// BenchmarkFeatures mide lo que cuesta preparar las líneas para Chispa (el
// resto del camino es Chispa, ~1 µs por línea, y el transporte).
func BenchmarkFeatures(b *testing.B) {
	var sb strings.Builder
	for i := range 2000 {
		switch i % 50 {
		case 7:
			sb.WriteString("\x1b[31mAssertionError: expected 1 to equal 2\x1b[0m\n")
		case 8:
			sb.WriteString("    at Context.<anonymous> (test/unit/parser.test.js:42:17)\n")
		default:
			sb.WriteString("Downloading https://registry.npmjs.org/some-package/-/some-package-1.2.3.tgz (12 kB)\n")
		}
	}
	lg, err := Read(strings.NewReader(sb.String()), DefaultLimits)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		Features(lg)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(lg.Lines)), "ns/line")
}
