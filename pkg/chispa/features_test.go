package chispa

import (
	"crypto/sha256"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Valores dorados: si cambian, cambia el hashing y todo .chispa existente deja de
// servir. Los de FNV-1a son los de la especificación de referencia.
func TestHashGolden(t *testing.T) {
	for s, want := range map[string]uint64{
		"":       0xcbf29ce484222325,
		"a":      0xaf63dc4c8601ec8c,
		"foobar": 0x85944171f73967e8,
	} {
		if got := FNV1a64(s); got != want {
			t.Errorf("FNV1a64(%q) = %#x, want %#x", s, got, want)
		}
	}
	if got, want := DefaultSpec().Hash(), uint64(SpecHashGolden); got != want {
		t.Errorf("DefaultSpec().Hash() = %#x, want %#x (did the canonical form change?)", got, want)
	}
	b, s := bucketSign(FNV1a64("w:crash"), DefaultBuckets-1)
	if b != BucketGolden || s != SignGolden {
		t.Errorf("bucketSign(w:crash) = (%d, %d), want (%d, %d)", b, s, BucketGolden, SignGolden)
	}
}

// Los dorados de arriba, en constantes para poder regenerarlos a mano.
const (
	SpecHashGolden = 0xd1833c12899d948f
	BucketGolden   = 136605
	SignGolden     = -1
)

func tokens(text string) []string {
	var e extractor
	e.tokenize(text)
	out := make([]string, len(e.toks))
	for i := range e.toks {
		out[i] = e.tokString(i)
	}
	return out
}

func TestNormalization(t *testing.T) {
	cases := map[string][]string{
		"Fix the CRASH in parser":          {"fix", "the", "crash", "in", "parser"},
		"Canción ÉXITO Ñandú straße":       {"cancion", "exito", "nandu", "straße"},
		"ＦＩＸ　ｂｕｇ":                          {"fix", "bug"},
		"éte":                             {"ete"}, // marca combinante fuera
		"日本語のテスト":                          {"日", "本", "語", "の", "テ", "ス", "ト"},
		"bump to v1.2.3 (#39754) a1b2c3d4": {"bump", "to", "v1", "2", "3", tokNum, tokNum},
		"don't-stop__now":                  {"don", "t", "stop", "now"},
		strings.Repeat("x", 50) + " ok":    {tokLong, "ok"},
		"":                                 {},
		"¡¿!?":                             {},
	}
	for in, want := range cases {
		if got := tokens(in); !reflect.DeepEqual(got, want) {
			t.Errorf("tokens(%q) = %q, want %q", in, got, want)
		}
	}
	if len(latinFold) != 0x180-0xC0 {
		t.Fatalf("latinFold has %d entries, want %d", len(latinFold), 0x180-0xC0)
	}
}

func featureDigest(spec FeatureSpec, in Input) string {
	// El orden de extracción de los campos sigue al del mapa; el multiconjunto
	// de características es lo que debe ser estable.
	names, n := spec.FeatureNames(in)
	lines := make([]string, len(names))
	for i, f := range names {
		lines[i] = fmt.Sprintf("%s|%d|%d|%v", f.Name, f.Bucket, f.Sign, f.Text)
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		fmt.Fprintln(h, l)
	}
	return fmt.Sprintf("%x/%d/%d", h.Sum(nil)[:8], len(names), n)
}

func TestExtractionDeterministic(t *testing.T) {
	spec := DefaultSpec()
	spec.CharMin, spec.CharMax = 3, 5
	fields := map[string]any{"service": "API", "level": "error", "latency_ms": 180.0, "retry": true,
		"tags": []any{"db", "timeout", map[string]any{"nested": 1}}, "ignored": map[string]any{"x": 1}}
	in := Input{Text: "Fix: null pointer crash when the cache is cold (#12345)", Fields: fields}
	first := featureDigest(spec, in)
	for i := 0; i < 20; i++ { // el orden de los mapas cambia en cada vuelta
		if got := featureDigest(spec, in); got != first {
			t.Fatalf("extraction not deterministic: %s vs %s", got, first)
		}
	}
	if first != ExtractGolden {
		t.Errorf("feature digest %s, golden %s", first, ExtractGolden)
	}

	names, nText := spec.FeatureNames(in)
	has := map[string]bool{}
	for _, f := range names {
		has[f.Name] = true
	}
	for _, want := range []string{"w:crash", "b:null pointer", "c:<cr", "c:sh>", "w:" + tokNum,
		"f:service=api", "f:level=error", "f:latency_ms#+[128,256)", "f:retry=true", "f:tags=db", "f:tags=timeout"} {
		if !has[want] {
			t.Errorf("missing feature %q", want)
		}
	}
	if has["f:ignored=x"] || has["f:tags=nested"] {
		t.Error("nested objects must be ignored")
	}
	if nText == 0 || nText == len(names) {
		t.Errorf("text count %d of %d features", nText, len(names))
	}
	// Extract agrega por cubo y conserva la suma de signos.
	feats, n2 := spec.Extract(in)
	if n2 != nText {
		t.Errorf("Extract text count %d, FeatureNames %d", n2, nText)
	}
	key := func(b uint32, text bool) [2]uint32 {
		if text {
			return [2]uint32{b, 1}
		}
		return [2]uint32{b, 0}
	}
	sum := map[[2]uint32]int32{}
	for _, f := range names {
		sum[key(f.Bucket, f.Text)] += f.Sign
	}
	for _, f := range feats {
		if sum[key(f.Bucket, f.Text)] != f.Value || f.Value == 0 {
			t.Errorf("aggregated feature %+v does not match the sum of its occurrences", f)
		}
	}
}

const ExtractGolden = "ab802a79dca89ed5/109/103"

func TestManyFieldsDeterministic(t *testing.T) {
	fields := map[string]any{}
	for i := 0; i < 3*MaxFields; i++ {
		fields[fmt.Sprintf("k%03d", i)] = "v"
	}
	spec := DefaultSpec()
	in := Input{Fields: fields}
	first := featureDigest(spec, in)
	for i := 0; i < 20; i++ {
		if featureDigest(spec, in) != first {
			t.Fatal("over MaxFields the chosen subset must not depend on map order")
		}
	}
	names, _ := spec.FeatureNames(in)
	if len(names) != MaxFields {
		t.Errorf("%d field features, want %d", len(names), MaxFields)
	}
}

func TestTextTruncation(t *testing.T) {
	spec := DefaultSpec()
	spec.MaxTextBytes = 10
	// Cortar en medio de una runa no debe dejar UTF-8 roto ni un token de más.
	names, _ := spec.FeatureNames(Input{Text: "abcdefghiñjk"})
	for _, f := range names {
		if strings.ContainsRune(f.Name, '�') {
			t.Errorf("broken rune in %q", f.Name)
		}
	}
	if len(names) != 1 || names[0].Name != "w:abcdefghi" {
		t.Errorf("got %+v", names)
	}
}

func TestNumBucket(t *testing.T) {
	for v, want := range map[float64]string{0: "0\x00", 0.5: "+\x00", 1: "+\x01", 7: "+\x03", 8: "+\x04", -3: "-\x02",
		1e300: "+\x3f"} {
		s, m := numBucket(v)
		if string([]byte{s, m}) != want {
			t.Errorf("numBucket(%v) = %q%d, want %q", v, s, m, want)
		}
	}
	if s, _ := numBucket(math.NaN()); s != 'n' {
		t.Error("NaN bucket")
	}
}

func TestSpecValidate(t *testing.T) {
	bad := []FeatureSpec{
		{Buckets: 1000, Unigrams: true, MaxTextBytes: 10},
		{Buckets: 1 << 30, Unigrams: true, MaxTextBytes: 10},
		{Buckets: 1 << 10, MaxTextBytes: 10},
		{Buckets: 1 << 10, Unigrams: true, CharMin: 5, CharMax: 3, MaxTextBytes: 10},
		{Buckets: 1 << 10, Unigrams: true, MaxTextBytes: 0},
	}
	for _, s := range bad {
		if s.Validate() == nil {
			t.Errorf("spec %+v should be invalid", s)
		}
	}
	if err := DefaultSpec().Validate(); err != nil {
		t.Error(err)
	}
}

func TestDetMath(t *testing.T) {
	worst := 0.0
	for x := -700.0; x <= 700; x += 0.37 {
		got, want := detExp(x), math.Exp(x)
		if rel := math.Abs(got-want) / want; rel > worst {
			worst = rel
		}
	}
	if worst > 1e-15 {
		t.Errorf("detExp max relative error %g", worst)
	}
	worst = 0
	for x := 1e-300; x < 1e300; x *= 3.7 {
		got, want := detLog(x), math.Log(x)
		if d := math.Abs(got - want); d > 1e-15*math.Max(1, math.Abs(want)) && d > worst {
			worst = d
		}
	}
	if worst > 0 {
		t.Errorf("detLog max error %g", worst)
	}
	if detExp(-1000) != 0 || !math.IsInf(detExp(1000), 1) || detExp(0) != 1 {
		t.Error("detExp edge cases")
	}
}
