package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Recortar deja rastro: cuántas líneas y bytes se descartaron, exactos, y un
// aviso legible. Por flujo, por fichero y por el tope de líneas.
func TestReadDroppedWarning(t *testing.T) {
	var b bytes.Buffer
	for i := range 5000 {
		fmt.Fprintf(&b, "%s line %04d\n", strings.Repeat("x", 90), i) // 101 bytes
	}
	raw := b.Bytes()
	check := func(name string, lg *Log, wantDropped int) {
		t.Helper()
		if !lg.Truncated || lg.DroppedLines != wantDropped || lg.DroppedLines+len(lg.Lines) != 5000 {
			t.Fatalf("%s: truncated %v, dropped %d + kept %d, want %d dropped", name, lg.Truncated, lg.DroppedLines, len(lg.Lines), wantDropped)
		}
		if lg.DroppedBytes != int64(wantDropped*101) {
			t.Errorf("%s: dropped %d bytes, want %d", name, lg.DroppedBytes, wantDropped*101)
		}
		if first := fmt.Sprintf("line %04d", wantDropped); !strings.HasSuffix(lg.Lines[0].Text, first) {
			t.Errorf("%s: first kept line %q, want …%s", name, lg.Lines[0].Text, first)
		}
		w := lg.Warning()
		for _, s := range []string{fmt.Sprintf("last %d lines", len(lg.Lines)), fmt.Sprintf("first %d lines", wantDropped), "KiB", "may be in the part"} {
			if !strings.Contains(w, s) {
				t.Errorf("%s: warning %q lacks %q", name, w, s)
			}
		}
	}
	// Tope de bytes por flujo: 10 000 bytes son 99 líneas enteras y un trozo.
	lg, err := Read(bytes.NewReader(raw), Limits{MaxBytes: 10_000, MaxLines: 1000, MaxLineBytes: 200, MaxStream: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	check("stream", lg, 5000-99)
	// El mismo tope por fichero (salta sin interpretar, pero cuenta).
	p := filepath.Join(t.TempDir(), "big.log")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	lg, err = ReadFile(p, Limits{MaxBytes: 10_000, MaxLines: 1000, MaxLineBytes: 200, MaxStream: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	check("file", lg, 5000-99)
	// Solo el tope de líneas.
	lg, err = Read(bytes.NewReader(raw), Limits{MaxBytes: 1 << 20, MaxLines: 300, MaxLineBytes: 200, MaxStream: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	check("lines", lg, 4700)
	// Un salto mayor que MaxStream no se cuenta entero: el número es un mínimo.
	lg, err = ReadFile(p, Limits{MaxBytes: 10_000, MaxLines: 1000, MaxLineBytes: 200, MaxStream: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !lg.DroppedAtLeast || lg.DroppedLines >= 5000-99 || !strings.Contains(lg.Warning(), "at least") {
		t.Errorf("uncounted skip: at least %v, %d lines, %q", lg.DroppedAtLeast, lg.DroppedLines, lg.Warning())
	}
	// Un flujo más largo que MaxStream: lo que no se leyó es el FINAL.
	lg, err = Read(bytes.NewReader(raw), Limits{MaxBytes: 1 << 20, MaxLines: 10_000, MaxLineBytes: 200, MaxStream: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !lg.Truncated || !lg.TailUnread || !strings.Contains(lg.Warning(), "end was not read") {
		t.Errorf("stream past MaxStream: truncated %v, unread %v, %q", lg.Truncated, lg.TailUnread, lg.Warning())
	}
	// Un log que cabe no avisa.
	lg, err = Read(strings.NewReader("ok\nFAIL x\n"), DefaultLimits)
	if err != nil || lg.Truncated || lg.Warning() != "" || lg.DroppedLines != 0 {
		t.Errorf("small log: %v %+v %q", err, lg, lg.Warning())
	}
}

// fakeGateway contesta /v1/classify como Chispa: el localizador puntúa por
// palabras clave y la categoría es siempre «test».
func fakeGateway(t *testing.T) *Gateway {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Task string `json:"task"`
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Task != DefaultOptions.LinesTask {
			fmt.Fprint(w, `{"label":"test","prob":0.9,"escalate":false}`)
			return
		}
		p := 0.02
		switch {
		case strings.Contains(req.Text, "BOOM"):
			p = 0.95
		case strings.Contains(req.Text, "tail error"):
			p = 0.6
		}
		fmt.Fprintf(w, `{"label":"explains","prob":%g,"chispa":{"candidates":[{"label":"explains","prob":%g}]}}`, p, p)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KLING_AI_TOKEN", "")
	g, err := NewGateway(srv.URL, "", 10*time.Second, 4)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// El caso de la matriz de trabajos: el fallo a mitad y una cola que explica
// otra cosa. Leyendo la cola, el resultado avisa del recorte (y señala la
// cola); por ventanas encuentra el tramo bueno, aunque caiga justo en el borde
// entre dos ventanas.
func TestAnalyzeWindows(t *testing.T) {
	var b bytes.Buffer
	for i := range 3000 {
		switch {
		case i == 1199 || i == 1200: // a caballo entre las ventanas 4 y 5
			fmt.Fprintf(&b, "BOOM: job %d failed\n", i)
		case i == 2900:
			b.WriteString("tail error: something unrelated\n")
		default:
			fmt.Fprintf(&b, "step %d ok\n", i)
		}
	}
	p := filepath.Join(t.TempDir(), "matrix.log")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	g := fakeGateway(t)
	o := DefaultOptions
	o.SummaryTask = ""
	ctx := context.Background()

	lim := Limits{MaxBytes: 1 << 20, MaxLines: 500, MaxLineBytes: 200, MaxStream: 1 << 20}
	lg, err := ReadFile(p, lim)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := Analyze(ctx, g, lg, o)
	if err != nil {
		t.Fatal(err)
	}
	if !tail.Truncated || tail.DroppedLines != 2500 || !strings.Contains(tail.Warning, "first 2500 lines") || strings.Contains(tail.Chunk, "BOOM") {
		t.Fatalf("tail: truncated %v, dropped %d, warning %q, chunk %q", tail.Truncated, tail.DroppedLines, tail.Warning, tail.Chunk)
	}

	wo := WindowOptions{Lines: 300, Context: 50, MaxBytes: 1 << 20}
	res, err := AnalyzeWindows(ctx, g, p, lim, wo, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.Warning != "" || res.Lines != 3000 || res.Scored != 3000 || res.Windows != 10 {
		t.Fatalf("windows: %+v", res)
	}
	if len(res.Chunks) != 1 || res.Chunks[0].From != 1200 || res.Chunks[0].To != 1201 {
		t.Fatalf("chunks %+v", res.Chunks)
	}
	if res.Chunk != "BOOM: job 1199 failed\nBOOM: job 1200 failed\n" || res.Category != CatTest {
		t.Fatalf("chunk %q, category %s", res.Chunk, res.Category)
	}
	// Las características de una ventana son las del log entero.
	full := Features(&Log{Lines: func() []Line {
		var ls []Line
		for l := range strings.SplitSeq(strings.TrimSuffix(b.String(), "\n"), "\n") {
			ls = append(ls, Line{Text: l})
		}
		return ls
	}()})
	w := &Log{Lines: make([]Line, 400)}
	for i := range w.Lines {
		w.Lines[i] = Line{Text: full[1000+i].Text}
	}
	part := features(w, 100, 300, 1000, 3000)
	for k, x := range part {
		want := full[1100+k].Fields
		if x.Fields["end"] != want["end"] || x.Fields["pos"] != want["pos"] {
			t.Fatalf("line %d: end %v pos %v, want %v %v", 1100+k, x.Fields["end"], x.Fields["pos"], want["end"], want["pos"])
		}
	}
	// Un fichero mayor que MaxBytes: la cola, con aviso.
	wo.MaxBytes = 10_000
	res, err = AnalyzeWindows(ctx, g, p, lim, wo, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.DroppedLines+res.Lines != 3000 || !strings.Contains(res.Warning, "dropped") {
		t.Fatalf("windows over MaxBytes: %d+%d lines, %q", res.DroppedLines, res.Lines, res.Warning)
	}
}
