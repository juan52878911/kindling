package chispa

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Topes de la extracción. Existen para que una entrada hostil (un texto de
// megas, mil campos) cueste lo mismo que una normal y no reserve sin límite.
const (
	DefaultBuckets      = 1 << 18
	MinBuckets          = 1 << 4
	MaxBuckets          = 1 << 22
	DefaultMaxTextBytes = 4096
	MaxTextBytesLimit   = 1 << 16
	MaxFields           = 64   // campos por entrada; el resto se ignora
	MaxFieldKey         = 64   // bytes de un nombre de campo
	MaxFieldValue       = 128  // bytes de un valor de texto
	MaxFieldList        = 16   // elementos de una lista de valores
	maxTokenBytes       = 40   // un token más largo es ruido (base64, rutas…)
	numTokenBytes       = 5    // un token con dígitos de esta longitud es un número
	normVersion         = "n1" // sube si cambia la normalización: cambia el hash
	specPrefix          = "chispa-spec-v1"
	charNgramMax        = 6
)

// FeatureSpec dice cómo se convierte una entrada en características. Va dentro
// del fichero .chispa junto a su hash: un modelo solo tiene sentido con la misma
// extracción con la que se entrenó.
type FeatureSpec struct {
	Buckets      uint32 `json:"buckets"`        // potencia de dos
	Unigrams     bool   `json:"unigrams"`       // palabras sueltas
	Bigrams      bool   `json:"bigrams"`        // pares de palabras consecutivas
	CharMin      int    `json:"char_min"`       // n-gramas de caracteres; 0 = no
	CharMax      int    `json:"char_max"`       //
	Fields       bool   `json:"fields"`         // campos estructurados campo=valor
	MaxTextBytes int    `json:"max_text_bytes"` // se trunca el texto aquí
}

// DefaultSpec es el punto de partida: palabras y bigramas, campos, sin
// n-gramas de caracteres (más lentos y, en la evaluación de commits, sin
// ganancia clara; ver docs/CHISPA-EVAL.md).
func DefaultSpec() FeatureSpec {
	return FeatureSpec{
		Buckets:      DefaultBuckets,
		Unigrams:     true,
		Bigrams:      true,
		Fields:       true,
		MaxTextBytes: DefaultMaxTextBytes,
	}
}

// Validate comprueba que la especificación es coherente y está dentro de los
// topes. La llama el cargador: un .chispa manipulado no pasa de aquí.
func (s FeatureSpec) Validate() error {
	if s.Buckets < MinBuckets || s.Buckets > MaxBuckets || s.Buckets&(s.Buckets-1) != 0 {
		return fmt.Errorf("buckets must be a power of two in [%d, %d], got %d", MinBuckets, MaxBuckets, s.Buckets)
	}
	if s.CharMin != 0 || s.CharMax != 0 {
		if s.CharMin < 1 || s.CharMax < s.CharMin || s.CharMax > charNgramMax {
			return fmt.Errorf("char n-grams must satisfy 1 <= min <= max <= %d, got %d-%d", charNgramMax, s.CharMin, s.CharMax)
		}
	}
	if s.MaxTextBytes < 1 || s.MaxTextBytes > MaxTextBytesLimit {
		return fmt.Errorf("max_text_bytes must be in [1, %d], got %d", MaxTextBytesLimit, s.MaxTextBytes)
	}
	if !s.Unigrams && !s.Bigrams && s.CharMin == 0 && !s.Fields {
		return errors.New("feature spec enables no features")
	}
	return nil
}

// Canonical es la forma textual estable de la especificación, la que se hashea.
// Incluye la versión de la normalización: si cambia el tokenizador, cambia el
// hash y los modelos viejos se rechazan en vez de dar basura en silencio.
func (s FeatureSpec) Canonical() string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return fmt.Sprintf("%s;norm=%s;hash=fnv1a64+fmix64;buckets=%d;uni=%s;bi=%s;char=%d-%d;fields=%s;maxtext=%d",
		specPrefix, normVersion, s.Buckets, b(s.Unigrams), b(s.Bigrams), s.CharMin, s.CharMax, b(s.Fields), s.MaxTextBytes)
}

// Hash identifica la especificación. Un cliente que extraiga características
// por su cuenta compara este valor antes de fiarse del modelo.
func (s FeatureSpec) Hash() uint64 {
	return fmix64(FNV1a64(s.Canonical()))
}

// Input es lo que se clasifica: un texto y, opcionalmente, campos
// estructurados tal como salen de encoding/json (string, float64, bool,
// json.Number o listas de ellos). Los objetos anidados se ignoran.
type Input struct {
	Text   string         `json:"text"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Feature es una característica ya hasheada y agregada: la suma de los signos
// de todas sus apariciones en el cubo. Text distingue las del texto (que se
// normalizan por la longitud) de las de campos (que valen ±1 cada una).
type Feature struct {
	Bucket uint32
	Value  int32
	Text   bool
}

// feat es una aparición sin agregar: lo que produce la extracción en caliente.
type feat struct {
	bucket uint32
	sign   int32
	text   bool
}

type span struct {
	start, end int32
	num        bool // se sustituye por el marcador numérico
	long       bool // se sustituye por el marcador de token largo
}

// extractor guarda los búferes de una extracción para reutilizarlos: con ellos
// la inferencia no reserva memoria en el camino caliente.
type extractor struct {
	norm  []byte
	toks  []span
	offs  []int32
	feats []feat
	names []string // solo cuando se pide evidencia
	keys  []string // solo cuando hay más de MaxFields campos
	nText int
}

const (
	tokNum  = "<num>"
	tokLong = "<long>"
)

func (e *extractor) tok(i int) []byte {
	t := e.toks[i]
	return e.norm[t.start:t.end]
}

// tokString es el texto de un token para la evidencia (reserva; no es caliente).
func (e *extractor) tokString(i int) string {
	switch t := e.toks[i]; {
	case t.num:
		return tokNum
	case t.long:
		return tokLong
	}
	return string(e.tok(i))
}

func (e *extractor) tokHash(h uint64, i int) uint64 {
	switch t := e.toks[i]; {
	case t.num:
		return fnvStr(h, tokNum)
	case t.long:
		return fnvStr(h, tokLong)
	}
	return fnvBytes(h, e.tok(i))
}

// Extract devuelve las características de in agregadas por cubo y ordenadas, y
// cuántas apariciones de texto hubo (el divisor de la normalización). La usa
// el entrenador; la inferencia usa la versión sin reservas.
func (s FeatureSpec) Extract(in Input) (feats []Feature, textCount int) {
	var e extractor
	s.extract(&e, in.Text, in.Fields, false)
	type key struct {
		b uint32
		t bool
	}
	agg := make(map[key]int32, len(e.feats))
	for _, f := range e.feats {
		agg[key{f.bucket, f.text}] += f.sign
	}
	out := make([]Feature, 0, len(agg))
	for k, v := range agg {
		if v != 0 {
			out = append(out, Feature{Bucket: k.b, Value: v, Text: k.t})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bucket != out[j].Bucket {
			return out[i].Bucket < out[j].Bucket
		}
		return !out[i].Text && out[j].Text
	})
	return out, e.nText
}

// NamedFeature es una aparición con su nombre legible («w:crash», «f:level=error»).
type NamedFeature struct {
	Name   string `json:"name"`
	Bucket uint32 `json:"bucket"`
	Sign   int32  `json:"sign"`
	Text   bool   `json:"text"`
}

// FeatureNames devuelve cada aparición con su nombre, en orden de extracción.
// Sirve para depurar y para la evidencia; reserva memoria.
func (s FeatureSpec) FeatureNames(in Input) (out []NamedFeature, textCount int) {
	var e extractor
	s.extract(&e, in.Text, in.Fields, true)
	out = make([]NamedFeature, len(e.feats))
	for i, f := range e.feats {
		out[i] = NamedFeature{Name: e.names[i], Bucket: f.bucket, Sign: f.sign, Text: f.text}
	}
	return out, e.nText
}

func (e *extractor) emit(h uint64, mask uint32, text bool, name func() string) {
	b, sg := bucketSign(h, mask)
	e.feats = append(e.feats, feat{bucket: b, sign: sg, text: text})
	if text {
		e.nText++
	}
	if e.names != nil {
		e.names = append(e.names, name())
	}
}

// extract llena e.feats. Con withNames también e.names (reserva; solo para
// evidencia). Sin nombres, y con los búferes ya crecidos, no reserva nada.
func (s *FeatureSpec) extract(e *extractor, text string, fields map[string]any, withNames bool) {
	e.feats = e.feats[:0]
	e.nText = 0
	e.names = nil
	if withNames {
		e.names = make([]string, 0, 64)
	}
	mask := s.Buckets - 1
	if len(text) > s.MaxTextBytes {
		cut := s.MaxTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	e.tokenize(text)

	for i := range e.toks {
		if s.Unigrams {
			h := e.tokHash(fnvStr(fnvOffset, "w:"), i)
			e.emit(h, mask, true, func() string { return "w:" + e.tokString(i) })
		}
		if s.Bigrams && i > 0 {
			h := fnvStr(fnvOffset, "b:")
			h = e.tokHash(h, i-1)
			h = fnvByte(h, ' ')
			h = e.tokHash(h, i)
			e.emit(h, mask, true, func() string { return "b:" + e.tokString(i-1) + " " + e.tokString(i) })
		}
		if s.CharMin > 0 && !e.toks[i].num && !e.toks[i].long {
			e.charNgrams(i, s.CharMin, s.CharMax, mask)
		}
	}
	if s.Fields && len(fields) > 0 {
		e.fieldFeatures(fields, mask)
	}
}

// tokenize normaliza el texto a e.norm y parte los tokens. La normalización es
// deliberadamente pequeña y sin tablas externas (no hay NFKC en la biblioteca
// estándar): minúsculas Unicode, acentos latinos plegados («canción» =
// «cancion»), marcas combinantes fuera, ancho completo a ASCII, y cada
// ideograma CJK como token propio porque esos idiomas no separan con espacios.
func (e *extractor) tokenize(text string) {
	e.norm = e.norm[:0]
	e.toks = e.toks[:0]
	in := false
	start := 0
	digits := false
	closeTok := func() {
		if !in {
			return
		}
		in = false
		n := len(e.norm) - start
		e.toks = append(e.toks, span{
			start: int32(start), end: int32(len(e.norm)),
			num:  digits && n >= numTokenBytes,
			long: !digits && n > maxTokenBytes,
		})
	}
	for i := 0; i < len(text); {
		r, size := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		i += size
		c := foldRune(r)
		switch {
		case c == foldSkip:
			continue
		case c == foldSep:
			closeTok()
			continue
		case c >= 0x2E80 && isSolo(c):
			closeTok()
			start = len(e.norm)
			e.norm = utf8.AppendRune(e.norm, c)
			in, digits = true, false
			closeTok()
			continue
		}
		if !in {
			in, start, digits = true, len(e.norm), false
		}
		if c >= '0' && c <= '9' {
			digits = true
		}
		if c < utf8.RuneSelf {
			e.norm = append(e.norm, byte(c))
		} else {
			e.norm = utf8.AppendRune(e.norm, c)
		}
	}
	closeTok()
}

const (
	foldSep  rune = -1
	foldSkip rune = -2
)

// latinFold pliega U+00C0..U+017F a su letra base. '.' = sin plegado (se deja a
// unicode.ToLower), '_' = separador (× y ÷).
const latinFold = "" +
	"aaaaaa.ceeeeiiii" + // C0-CF
	".nooooo_ouuuuy.." + // D0-DF
	"aaaaaa.ceeeeiiii" + // E0-EF
	".nooooo_ouuuuy.y" + // F0-FF
	"aaaaaaccccccccdd" + // 100-10F
	"ddeeeeeeeeeegggg" + // 110-11F
	"gggghhhhiiiiiiii" + // 120-12F
	"ii..jjkk.lllllll" + // 130-13F
	"lllnnnnnnn..oooo" + // 140-14F
	"oo..rrrrrrssssss" + // 150-15F
	"ssttttttuuuuuuuu" + // 160-16F
	"uuuuwwyyyzzzzzzs" // 170-17F

func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return foldSep
	}
	if r >= 0xFF01 && r <= 0xFF5E { // ancho completo
		return foldRune(r - 0xFEE0)
	}
	if r >= 0xC0 && r <= 0x17F {
		switch c := latinFold[r-0xC0]; c {
		case '_':
			return foldSep
		case '.':
		default:
			return rune(c)
		}
	}
	if unicode.Is(unicode.Mn, r) {
		return foldSkip
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return unicode.ToLower(r)
	}
	return foldSep
}

// isSolo dice si un carácter forma token por sí solo: escrituras sin espacios.
func isSolo(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Thai, r)
}

// charNgrams emite los n-gramas de caracteres (en runas, no bytes) del token i
// rodeado de '<' y '>', como fastText: los bordes distinguen «fix» al principio
// de una palabra de «fix» en medio.
func (e *extractor) charNgrams(i, lo, hi int, mask uint32) {
	t := e.tok(i)
	e.offs = e.offs[:0]
	for j := 0; j < len(t); {
		e.offs = append(e.offs, int32(j))
		_, sz := utf8.DecodeRune(t[j:])
		j += sz
	}
	e.offs = append(e.offs, int32(len(t)))
	R := len(e.offs) - 1 // runas del token
	P := R + 2           // con los dos bordes
	for n := lo; n <= hi; n++ {
		for a := 0; a+n <= P; a++ {
			b := a + n // [a, b) en coordenadas con bordes
			h := fnvStr(fnvOffset, "c:")
			if a == 0 {
				h = fnvByte(h, '<')
			}
			ra, rb := max(a-1, 0), min(b-1, R) // runas del token cubiertas
			if ra < rb {
				h = fnvBytes(h, t[e.offs[ra]:e.offs[rb]])
			}
			if b == P {
				h = fnvByte(h, '>')
			}
			e.emit(h, mask, true, func() string {
				s := "c:"
				if a == 0 {
					s += "<"
				}
				if ra < rb {
					s += string(t[e.offs[ra]:e.offs[rb]])
				}
				if b == P {
					s += ">"
				}
				return s
			})
		}
	}
}

// fieldFeatures emite campo=valor. Los números se agrupan por orden de magnitud
// (potencias de dos, calculadas con enteros para no depender de math.Log2).
// Si hay más de MaxFields campos se toman los primeros por orden alfabético:
// recorrer el mapa tal cual daría un subconjunto distinto en cada llamada.
func (e *extractor) fieldFeatures(fields map[string]any, mask uint32) {
	if len(fields) <= MaxFields {
		for k, v := range fields {
			e.field(k, v, mask)
		}
		return
	}
	e.keys = e.keys[:0]
	for k := range fields {
		e.keys = append(e.keys, k)
	}
	sort.Strings(e.keys)
	for _, k := range e.keys[:MaxFields] {
		e.field(k, fields[k], mask)
	}
}

func (e *extractor) field(k string, v any, mask uint32) {
	if len(k) > MaxFieldKey {
		k = k[:MaxFieldKey]
	}
	switch x := v.(type) {
	case []any:
		for i, el := range x {
			if i == MaxFieldList {
				break
			}
			switch el.(type) {
			case []any, map[string]any:
				continue // sin anidar: acota el trabajo
			}
			e.field(k, el, mask)
		}
		return
	case []string:
		for i, el := range x {
			if i == MaxFieldList {
				break
			}
			e.field(k, el, mask)
		}
		return
	case map[string]any, nil:
		return
	}
	h := fnvStr(fnvOffset, "f:")
	h = fnvStr(h, k)
	switch x := v.(type) {
	case string:
		if len(x) > MaxFieldValue {
			x = x[:MaxFieldValue]
		}
		h = fnvByte(h, '=')
		h = fnvLowerStr(h, x)
		e.emit(h, mask, false, func() string { return "f:" + k + "=" + asciiLower(x) })
	case bool:
		h = fnvByte(h, '=')
		val := "false"
		if x {
			val = "true"
		}
		h = fnvStr(h, val)
		e.emit(h, mask, false, func() string { return "f:" + k + "=" + val })
	case float64:
		e.numField(h, k, x, mask)
	case interface{ Float64() (float64, error) }: // json.Number
		f, err := x.Float64()
		if err != nil {
			f = math.NaN()
		}
		e.numField(h, k, f, mask)
	case int:
		e.numField(h, k, float64(x), mask)
	}
}

func (e *extractor) numField(h uint64, k string, v float64, mask uint32) {
	sign, mag := numBucket(v)
	h = fnvByte(h, '#')
	h = fnvByte(h, sign)
	h = fnvByte(h, mag)
	e.emit(h, mask, false, func() string {
		switch sign {
		case 'n':
			return "f:" + k + "#nan"
		case '0':
			return "f:" + k + "#0"
		}
		s := "f:" + k + "#" + string(sign)
		if mag == 0 {
			return s + "<1"
		}
		return s + "[" + strconv.FormatUint(1<<(mag-1), 10) + "," + strconv.FormatUint(1<<mag, 10) + ")"
	})
}

// numBucket agrupa un número por su magnitud en potencias de dos: 5 y 7 caen
// juntos, 5 y 500 no. Solo usa conversiones y bits.Len64: el mismo resultado
// en cualquier arquitectura.
func numBucket(v float64) (sign byte, mag byte) {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return 'n', 0
	case v == 0:
		return '0', 0
	}
	sign = '+'
	if v < 0 {
		sign, v = '-', -v
	}
	if v >= 1<<62 {
		return sign, 63
	}
	return sign, byte(bits.Len64(uint64(v)))
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// Resultados especiales de FoldRune.
const (
	FoldSep  = foldSep  // la runa separa tokens
	FoldSkip = foldSkip // la runa desaparece (marca combinante)
)

// FoldRune es el plegado de caracteres del tokenizador de Chispa: minúscula,
// acentos latinos fuera, ancho completo a ASCII. Devuelve FoldSep o FoldSkip
// para separadores y marcas. Se exporta para que otros extractores (el
// etiquetador de pkg/chispa/slots) normalicen exactamente igual que el clasificador.
func FoldRune(r rune) rune { return foldRune(r) }
