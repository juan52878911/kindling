package zstd

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gen hace entradas deterministas: los vectores de testdata son estas
// entradas comprimidas con zstd 1.5.7 (ver TestGenerate), y la prueba las
// vuelve a generar para comparar byte a byte.
type gen uint64

func (g *gen) next() uint64 {
	*g ^= *g << 13
	*g ^= *g >> 7
	*g ^= *g << 17
	return uint64(*g)
}

var vocab = strings.Fields(`the layer image kindling microVM tar gzip zstd window block
	literal sequence offset match huffman table state frame checksum registry
	manifest digest ext4 snapshot daemon guest kernel memory disk volume a of to
	and in is it that for on with as by from at`)

// words es texto de mentira: comprime como un texto, con frases repetidas.
func words(n int, seed uint64) []byte {
	g := gen(seed | 1)
	var b bytes.Buffer
	var lines []string
	for b.Len() < n {
		if len(lines) > 8 && g.next()%4 == 0 {
			b.WriteString(lines[g.next()%uint64(len(lines))])
			continue
		}
		var l strings.Builder
		for k := 3 + g.next()%12; k > 0; k-- {
			l.WriteString(vocab[g.next()%uint64(len(vocab))])
			l.WriteByte(' ')
		}
		fmt.Fprintf(&l, "%d\n", g.next()%100000)
		lines = append(lines, l.String())
		b.WriteString(l.String())
	}
	return b.Bytes()[:n]
}

func random(n int, seed uint64) []byte {
	g := gen(seed | 1)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(g.next() >> 32)
	}
	return b
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// inputs son las entradas de los vectores de testdata, por nombre.
var inputs = map[string]func() []byte{
	"words":  func() []byte { return words(200_000, 1) },
	"random": func() []byte { return random(40_000, 2) },
	// Bloques RLE (ceros) y uno raw en medio.
	"rle": func() []byte { return cat(make([]byte, 1<<20), random(5000, 3), bytes.Repeat([]byte{'x'}, 300_000)) },
	// Una repetición a 3 MiB: solo la encuentra --long.
	"long":  func() []byte { return cat(random(64<<10, 4), make([]byte, 3<<20), random(64<<10, 4), words(10_000, 5)) },
	"multi": func() []byte { return cat(words(10_000, 6), random(1000, 7)) },
	"empty": func() []byte { return nil },
}

var vectors = []struct{ file, input string }{
	{"words-19.zst", "words"},        // -19, con xxhash64
	{"words-1-nocheck.zst", "words"}, // -1 --no-check
	{"words-stdin.zst", "words"},     // -3 por la entrada estándar: sin tamaño
	{"random.zst", "random"},         // bloques raw
	{"rle.zst", "rle"},               // bloques RLE
	{"long.zst", "long"},             // --long=27 -1: ventana de 128 MiB
	{"multi.zst", "multi"},           // dos marcos con uno saltable en medio
	{"empty.zst", "empty"},
}

// TestGenerate rehace los vectores de testdata con la herramienta zstd:
// ZSTD_GEN=1 go test -run Generate ./internal/zstd
func TestGenerate(t *testing.T) {
	if os.Getenv("ZSTD_GEN") == "" {
		t.Skip("ZSTD_GEN not set")
	}
	dir := t.TempDir()
	in := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, inputs[name](), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	z := func(out string, args ...string) {
		cmd := exec.Command("zstd", append([]string{"-q", "-f", "-o", "testdata/" + out}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("zstd %v: %v\n%s", args, err, b)
		}
	}
	z("words-19.zst", "-19", in("words"))
	z("words-1-nocheck.zst", "-1", "--no-check", in("words"))
	z("random.zst", "-3", in("random"))
	z("rle.zst", "-3", in("rle"))
	z("long.zst", "-1", "--long=27", in("long"))
	z("empty.zst", in("empty"))
	cmd := exec.Command("zstd", "-q", "-3", "-c")
	cmd.Stdin = bytes.NewReader(inputs["words"]())
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/words-stdin.zst", b, 0o644); err != nil {
		t.Fatal(err)
	}
	m := inputs["multi"]()
	f1, f2 := filepath.Join(dir, "m1"), filepath.Join(dir, "m2")
	os.WriteFile(f1, m[:10_000], 0o644)
	os.WriteFile(f2, m[10_000:], 0o644)
	z("m1.zst", "-3", f1)
	z("m2.zst", "-3", f2)
	a, _ := os.ReadFile("testdata/m1.zst")
	c, _ := os.ReadFile("testdata/m2.zst")
	os.Remove("testdata/m1.zst")
	os.Remove("testdata/m2.zst")
	skip := []byte{0x5A, 0x2A, 0x4D, 0x18, 5, 0, 0, 0, 'h', 'e', 'l', 'l', 'o'}
	if err := os.WriteFile("testdata/multi.zst", cat(a, skip, c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decode(b []byte) ([]byte, error) {
	return io.ReadAll(NewReader(bytes.NewReader(b)))
}

func TestVectors(t *testing.T) {
	for _, v := range vectors {
		b, err := os.ReadFile("testdata/" + v.file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decode(b)
		if err != nil {
			t.Fatalf("%s: %v", v.file, err)
		}
		if want := inputs[v.input](); !bytes.Equal(got, want) {
			t.Errorf("%s: got %d bytes, want %d (differ)", v.file, len(got), len(want))
		}
	}
}

// Leer de a poco (un byte cada vez) da lo mismo que de golpe.
func TestSmallReads(t *testing.T) {
	b, _ := os.ReadFile("testdata/multi.zst")
	r := NewReader(bytes.NewReader(b))
	var got []byte
	p := make([]byte, 1)
	for {
		n, err := r.Read(p)
		got = append(got, p[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, inputs["multi"]()) {
		t.Fatal("byte-at-a-time read differs")
	}
}

// Cada truncado es un error, nunca un final limpio, un pánico ni un bucle.
func TestTruncated(t *testing.T) {
	for _, f := range []string{"words-19.zst", "multi.zst", "rle.zst", "long.zst"} {
		b, _ := os.ReadFile("testdata/" + f)
		for n := 0; n < len(b); n += 1 + n/64 {
			if _, err := decode(b[:n]); err == nil {
				t.Fatalf("%s truncated to %d of %d bytes decoded without error", f, n, len(b))
			}
		}
	}
}

// Con xxhash64, un bit cambiado tras la cabecera nunca da datos malos: lo
// caza el descompresor o la suma. Algunos bits no cambian nada (el de
// relleno de una descripción de tabla, por ejemplo; zstd tampoco se queja):
// esos dan la salida buena.
func TestCorrupt(t *testing.T) {
	b, _ := os.ReadFile("testdata/words-19.zst")
	want := inputs["words"]()
	errs := 0
	for i := 6; i < len(b); i += 7 {
		c := bytes.Clone(b)
		c[i] ^= 1 << (i % 8)
		got, err := decode(c)
		if err != nil {
			errs++
		} else if !bytes.Equal(got, want) {
			t.Fatalf("bit %d of byte %d flipped: wrong output without error", i%8, i)
		}
	}
	if errs < len(b)/7*9/10 {
		t.Fatalf("only %d of %d flips detected", errs, len(b)/7)
	}
}

func TestBadFrames(t *testing.T) {
	block := []byte{1, 0, 0} // último bloque, raw, vacío
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty input", nil, "unexpected EOF"},
		{"magic", []byte{1, 2, 3, 4, 5}, "invalid magic"},
		{"window", cat([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 18 << 3}, block), "over the 128 MiB limit"},
		{"window fcs", cat([]byte{0x28, 0xB5, 0x2F, 0xFD, 0xE0}, binary.LittleEndian.AppendUint64(nil, 1<<40), block), "over the 128 MiB limit"},
		{"dictionary", cat([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x01, 0, 7}, block), "dictionaries"},
		{"reserved bit", cat([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x08, 0}, block), "corrupt"},
		{"reserved block", []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0, 7, 0, 0}, "corrupt"},
		{"huge block", []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0, 0xF9, 0xFF, 0xFF}, "corrupt"},
		{"size", []byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 5, 0x19, 0, 0, 'a', 'b', 'c'}, "shorter than"},
		{"trailing", cat([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0}, block, []byte{0}), "unexpected EOF"},
	}
	for _, c := range cases {
		_, err := decode(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", c.name, err, c.want)
		}
	}
}

// Un marco mínimo bien formado: cabecera de un solo segmento y un bloque
// RLE.
func TestTinyFrame(t *testing.T) {
	got, err := decode([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 5, 0x2B, 0, 0, 'z'})
	if err != nil || string(got) != "zzzzz" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestXXH64(t *testing.T) {
	for in, want := range map[string]uint64{"": 0xEF46DB3751D8E999, "abc": 0x44BC2CF5AD770999} {
		var x xxh64
		x.reset()
		x.write([]byte(in))
		if got := x.sum(); got != want {
			t.Errorf("xxh64(%q) = %x, want %x", in, got, want)
		}
	}
	// A trozos da lo mismo que de una vez.
	b := random(1000, 9)
	var a, c xxh64
	a.reset()
	a.write(b)
	c.reset()
	for i := 0; i < len(b); i += 13 {
		c.write(b[i:min(i+13, len(b))])
	}
	if a.sum() != c.sum() {
		t.Fatal("chunked xxh64 differs")
	}
}

// TestCLI compara con la herramienta zstd, si está, con más niveles,
// ventanas y tamaños de los que caben en testdata. Con una ventana pequeña
// comprueba además que la historia guardada no pasa del tope.
func TestCLI(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil || testing.Short() {
		t.Skip("no zstd tool or -short")
	}
	big := cat(words(3<<20, 11), random(300_000, 12), make([]byte, 500_000), words(1<<20, 11))
	cases := []struct {
		in   []byte
		args []string
	}{
		{big, []string{"-1"}},
		{big, []string{"-3"}},
		{big[:1<<20], []string{"-19"}},
		{big[:1<<20], []string{"--ultra", "-22"}},
		{big, []string{"-3", "--long=27"}},
		{big, []string{"-3", "--zstd=wlog=10"}},
		{big, []string{"-1", "--zstd=wlog=20"}},
		{big, []string{"-3", "--no-check", "--no-content-size"}},
		{big, []string{"--fast=5"}},
		{big, []string{"-6", "-B100000"}},
		{random(200_000, 13), []string{"-3"}},
		{[]byte("a"), []string{"-3"}},
	}
	for _, c := range cases {
		cmd := exec.Command("zstd", append([]string{"-q", "-c"}, c.args...)...)
		cmd.Stdin = bytes.NewReader(c.in)
		comp, err := cmd.Output()
		if err != nil {
			t.Fatalf("zstd %v: %v", c.args, err)
		}
		z := NewReader(bytes.NewReader(comp))
		t0 := time.Now()
		got, err := io.ReadAll(z)
		if err != nil {
			t.Fatalf("zstd %v: %v", c.args, err)
		}
		if !bytes.Equal(got, c.in) {
			t.Fatalf("zstd %v: output differs", c.args)
		}
		limit := z.window + max(z.window/2, 1<<20) + maxBlock
		if cap(z.hist) > limit {
			t.Errorf("zstd %v: history of %d bytes, over %d", c.args, cap(z.hist), limit)
		}
		t.Logf("zstd %v: %d → %d bytes, window %d, history %d, %v", c.args, len(comp), len(got), z.window, cap(z.hist), time.Since(t0))
	}
}

// Un lector que falla a mitad no se convierte en un final limpio.
func TestReadError(t *testing.T) {
	b, _ := os.ReadFile("testdata/words-19.zst")
	boom := errors.New("boom")
	r := io.MultiReader(bytes.NewReader(b[:len(b)/2]), iotestErr{boom})
	if _, err := io.ReadAll(NewReader(r)); err == nil {
		t.Fatal("read error swallowed")
	}
}

type iotestErr struct{ err error }

func (e iotestErr) Read([]byte) (int, error) { return 0, e.err }

func FuzzReader(f *testing.F) {
	// Semillas pequeñas: el fuzzer minimiza cada entrada interesante, y con
	// las de decenas de KiB se pasa el rato minimizando.
	for _, v := range vectors {
		if b, err := os.ReadFile("testdata/" + v.file); err == nil {
			f.Add(b[:min(len(b), 6<<10)])
		}
	}
	f.Add([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 5, 0x2B, 0, 0, 'z'})
	f.Fuzz(func(t *testing.T, b []byte) {
		// Un bloque RLE de 4 bytes da 128 KiB: se corta la salida.
		io.Copy(io.Discard, io.LimitReader(NewReader(bytes.NewReader(b)), 16<<20))
	})
}

// stored hace un marco de bloques raw con ventana de 1 KiB.
func stored(b []byte) []byte {
	out := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00}
	for {
		n, last := min(len(b), maxBlock), 0
		if n == len(b) {
			last = 1
		}
		h := n<<3 | last
		out = append(append(out, byte(h), byte(h>>8), byte(h>>16)), b[:n]...)
		if b = b[n:]; last == 1 {
			return out
		}
	}
}

// La historia guardada no crece con la salida: se queda en la ventana más
// el margen (aquí, 1 KiB + 1 MiB + un bloque para 5 MiB de salida).
func TestHistoryBound(t *testing.T) {
	in := random(5<<20, 31)
	z := NewReader(bytes.NewReader(stored(in)))
	got, err := io.ReadAll(z)
	if err != nil || !bytes.Equal(got, in) {
		t.Fatalf("stored frame: %v", err)
	}
	if limit := 1024 + 1<<20 + maxBlock; cap(z.hist) > limit {
		t.Fatalf("history of %d bytes, over %d", cap(z.hist), limit)
	}
}

// benchInput es una capa de verdad si ZSTD_BENCH apunta a un .tar (en el
// lab, la de una imagen), y si no, texto de mentira.
func benchInput(b *testing.B) []byte {
	if p := os.Getenv("ZSTD_BENCH"); p != "" {
		in, err := os.ReadFile(p)
		if err != nil {
			b.Fatal(err)
		}
		return in
	}
	return words(16<<20, 21)
}

// BenchmarkDecode frente a BenchmarkGzip: la misma entrada, zstd -3 (el
// nivel por defecto) y gzip -6 (el de docker push).
func BenchmarkDecode(b *testing.B) {
	in := benchInput(b)
	cmd := exec.Command("zstd", "-q", "-c", "-3")
	cmd.Stdin = bytes.NewReader(in)
	comp, err := cmd.Output()
	if err != nil {
		b.Skip("no zstd tool")
	}
	b.SetBytes(int64(len(in)))
	b.ReportMetric(float64(len(comp))/float64(len(in)), "ratio")
	for i := 0; i < b.N; i++ {
		io.Copy(io.Discard, NewReader(bytes.NewReader(comp)))
	}
}

func BenchmarkGzip(b *testing.B) {
	in := benchInput(b)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(in)
	zw.Close()
	b.SetBytes(int64(len(in)))
	b.ReportMetric(float64(buf.Len())/float64(len(in)), "ratio")
	for i := 0; i < b.N; i++ {
		zr, _ := gzip.NewReader(bytes.NewReader(buf.Bytes()))
		io.Copy(io.Discard, zr)
	}
}
