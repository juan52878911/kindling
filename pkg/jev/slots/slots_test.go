package slots

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	got := Tokenize("Pon el salón a 21,5°C y la luz al 50%!")
	var norms []string
	for _, tk := range got {
		norms = append(norms, tk.Norm)
	}
	want := "pon el salon a 21.5 ° c y la luz al 50 %"
	if s := strings.Join(norms, " "); s != want {
		t.Fatalf("tokens %q, want %q", s, want)
	}
	text := "Pon el salón a 21,5°C"
	for _, tk := range TokenizeN(text, 100, 100) {
		if tk.Norm == "salon" && text[tk.Start:tk.End] != "salón" {
			t.Fatalf("offsets point to %q", text[tk.Start:tk.End])
		}
	}
	if n := len(TokenizeN(strings.Repeat("a ", 1000), 1<<14, 7)); n != 7 {
		t.Fatalf("max tokens not applied: %d", n)
	}
}

// toy genera frases sintéticas con dos huecos, deterministas.
func toy(n int) []Sentence {
	areas := []string{"cocina", "salón", "baño", "dormitorio", "kitchen", "living room"}
	verbs := []string{"enciende la luz de", "apaga la luz del", "turn on the light in the", "pon la temperatura de"}
	var out []Sentence
	for i := 0; i < n; i++ {
		v := verbs[i%len(verbs)]
		a := areas[(i/3)%len(areas)]
		text := v + " " + a
		sp := []Span{{Slot: "area", Start: len(v) + 1, End: len(text)}}
		if i%2 == 0 {
			num := fmt.Sprint(10 + i%40)
			text += " a " + num + " grados"
			st := len(v) + 1 + len(a) + 3
			sp = append(sp, Span{Slot: "value", Start: st, End: st + len(num)})
		}
		out = append(out, Sentence{Text: text, Spans: sp})
	}
	return out
}

func trainToy(t testing.TB) *Model {
	spec := DefaultSpec()
	spec.Buckets = 1 << 12
	res, err := Train(toy(200), toy(40), TrainConfig{Spec: spec, Lexicon: map[string]string{"cocina": "area", "kitchen": "area"}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Model
}

func TestTrainTagRoundTrip(t *testing.T) {
	m := trainToy(t)
	text := "enciende la luz de cocina a 25 grados"
	spans := m.Tag(text, nil)
	got := map[string]string{}
	for _, s := range spans {
		got[s.Slot] = s.Text
	}
	if got["area"] != "cocina" || got["value"] != "25" {
		t.Fatalf("spans %+v", spans)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := m2.Marshal()
	if !bytes.Equal(b, b2) {
		t.Fatal("marshal is not stable")
	}
	if fmt.Sprint(m2.Tag(text, nil)) != fmt.Sprint(spans) {
		t.Fatal("loaded model tags differently")
	}
	// Determinismo del entrenamiento.
	b3, _ := trainToy(t).Marshal()
	if !bytes.Equal(b, b3) {
		t.Fatal("training is not deterministic")
	}
}

func TestTagNoAlloc(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	m := trainToy(t)
	dst := make([]Span, 0, 8)
	text := "turn on the light in the kitchen"
	m.Tag(text, dst)
	if n := testing.AllocsPerRun(100, func() { m.Tag(text, dst[:0]) }); n != 0 {
		t.Fatalf("Tag allocates %v times", n)
	}
}

func TestLoadRejects(t *testing.T) {
	m := trainToy(t)
	good, _ := m.Marshal()
	bad := append([]byte(nil), good...)
	bad[20] ^= 1
	if _, err := Unmarshal(bad); err == nil {
		t.Fatal("corrupt file accepted")
	}
	if _, err := Unmarshal(good[:len(good)-7]); err == nil {
		t.Fatal("truncated file accepted")
	}
}

func fixCRC(b []byte) []byte {
	if len(b) < 4 {
		return b
	}
	binary.LittleEndian.PutUint32(b[len(b)-4:], crc32.Checksum(b[:len(b)-4], crcTable))
	return b
}

// FuzzLoad: ningún fichero hace entrar en pánico al cargador, y lo que carga
// etiqueta sin pánico.
func FuzzLoad(f *testing.F) {
	spec := DefaultSpec()
	spec.Buckets = 16
	tags := TagsFor([]string{"area"})
	m := &Model{Spec: spec, Tags: tags, Lexicon: map[string]string{"cocina": "area"}, Scale: 1,
		W: make([]int16, 16*len(tags)), Trans: make([]int16, (len(tags)+1)*len(tags))}
	m.W[5] = 300
	m.SpecHash = Hash(spec, tags, m.Lexicon)
	good, err := m.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add(good[:len(good)/2])
	f.Add([]byte("\x89JVS\r\n\x1a\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, d := range [][]byte{data, fixCRC(append(append([]byte(nil), data...), 0, 0, 0, 0))} {
			if len(d) > 1<<20 {
				continue
			}
			m, err := Unmarshal(d)
			if err != nil {
				continue
			}
			for _, s := range []string{"", "enciende la cocina", strings.Repeat("x", 5000), "50 % °"} {
				for _, sp := range m.Tag(s, nil) {
					if sp.Start < 0 || sp.End > len(s) || sp.Start > sp.End {
						t.Fatalf("bad span %+v", sp)
					}
				}
			}
		}
	})
}

func BenchmarkTag(b *testing.B) {
	m := trainToy(b)
	dst := make([]Span, 0, 8)
	text := "pon la temperatura del salón a 22 grados"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = m.Tag(text, dst[:0])
	}
}
