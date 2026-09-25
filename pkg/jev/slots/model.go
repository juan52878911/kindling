// Package slots es el extractor de huecos de JEV: un etiquetador de
// secuencias lineal (perceptrón estructurado promediado con Viterbi) sobre
// características de token hasheadas, con pesos int16. Es a los huecos
// («device», «area», «value»…) lo que pkg/jev es a la intención: pequeño,
// determinista, de microsegundos y sin dependencias. Ver docs/domotica.md.
//
// Garantías, como en pkg/jev:
//   - Determinismo: hash propio, pesos enteros, suma entera; el mismo .jevs y
//     el mismo texto dan los mismos huecos en amd64 y arm64.
//   - La etiqueta es BIO restringida: Viterbi nunca produce un I-x que no siga
//     a B-x o I-x, así que todo hueco devuelto está bien formado.
//   - Cargar un fichero hostil no revienta ni reserva sin tope (Load).
package slots

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Topes del modelo.
const (
	DefaultBuckets = 1 << 17
	MinBuckets     = 1 << 4
	MaxBuckets     = 1 << 22
	MaxTags        = 64
	MaxTagBytes    = 64
	MaxLexicon     = 1 << 16
	MaxLexKey      = 64
	MaxWindow      = 3
	MaxAffix       = 5
	specPrefix     = "jevs-spec-v1;tok=t1"
)

// Spec dice cómo se convierte cada token en características. Va dentro del
// .jevs y su hash (junto con las etiquetas y el léxico) se comprueba al cargar.
type Spec struct {
	Buckets      uint32 `json:"buckets"`
	Window       int    `json:"window"`  // palabras vecinas a cada lado
	Affix        int    `json:"affix"`   // runas de prefijo y sufijo; 0 = no
	Lexicon      bool   `json:"lexicon"` // clase del token en el léxico
	MaxTextBytes int    `json:"max_text_bytes"`
	MaxTokens    int    `json:"max_tokens"`
}

// DefaultSpec es la especificación con la que se evaluó el etiquetador de
// domótica (docs/DOMOTICA-EVAL.md).
func DefaultSpec() Spec {
	return Spec{Buckets: DefaultBuckets, Window: 2, Affix: 3, Lexicon: true,
		MaxTextBytes: DefaultMaxTextBytes, MaxTokens: DefaultMaxTokens}
}

// Validate comprueba topes y coherencia.
func (s Spec) Validate() error {
	if s.Buckets < MinBuckets || s.Buckets > MaxBuckets || s.Buckets&(s.Buckets-1) != 0 {
		return fmt.Errorf("buckets must be a power of two in [%d, %d], got %d", MinBuckets, MaxBuckets, s.Buckets)
	}
	if s.Window < 0 || s.Window > MaxWindow {
		return fmt.Errorf("window must be in [0, %d]", MaxWindow)
	}
	if s.Affix < 0 || s.Affix > MaxAffix {
		return fmt.Errorf("affix must be in [0, %d]", MaxAffix)
	}
	if s.MaxTextBytes < 1 || s.MaxTextBytes > MaxTextBytesLimit {
		return fmt.Errorf("max_text_bytes must be in [1, %d]", MaxTextBytesLimit)
	}
	if s.MaxTokens < 1 || s.MaxTokens > MaxTokensLimit {
		return fmt.Errorf("max_tokens must be in [1, %d]", MaxTokensLimit)
	}
	return nil
}

// Hash identifica la extracción completa: especificación, etiquetas y léxico.
// Si cambia cualquiera de los tres, un modelo viejo se rechaza en vez de dar
// huecos basura en silencio.
func Hash(s Spec, tags []string, lex map[string]string) uint64 {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	h := fnvStr(fnvOffset, fmt.Sprintf("%s;buckets=%d;win=%d;affix=%d;lex=%s;maxtext=%d;maxtok=%d;tags=%s",
		specPrefix, s.Buckets, s.Window, s.Affix, b(s.Lexicon), s.MaxTextBytes, s.MaxTokens, strings.Join(tags, ",")))
	keys := make([]string, 0, len(lex))
	for k := range lex {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h = fnvByte(fnvStr(h, k), '=')
		h = fnvByte(fnvStr(h, lex[k]), ';')
	}
	return fmix64(h)
}

// Span es un hueco encontrado: nombre y posición en bytes del texto original.
type Span struct {
	Slot  string `json:"slot"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text,omitempty"`
}

// Meta: de dónde sale el modelo. No afecta a la inferencia.
type Meta struct {
	CreatedAt      string             `json:"created_at,omitempty"`
	DatasetSHA256  string             `json:"dataset_sha256,omitempty"`
	TrainSentences int                `json:"train_sentences,omitempty"`
	ValidSentences int                `json:"valid_sentences,omitempty"`
	Epochs         int                `json:"epochs,omitempty"`
	QuantAgreement float64            `json:"quant_agreement,omitempty"`
	ValidMetrics   map[string]float64 `json:"valid_metrics,omitempty"`
	Notes          string             `json:"notes,omitempty"`
}

// Model es un etiquetador cuantizado, inmutable tras Init: Tag es seguro desde
// muchas goroutines.
type Model struct {
	Spec     Spec
	SpecHash uint64
	// Tags[0] es "O"; el resto, pares B-x / I-x.
	Tags    []string
	Lexicon map[string]string
	Meta    Meta
	// Scale convierte las puntuaciones enteras a las del perceptrón (solo
	// informativo: Viterbi trabaja en enteros).
	Scale float64
	// W[b*T+t]: peso de la característica del cubo b para la etiqueta t.
	W []int16
	// Trans[(p+1)*T+t]: transición de p a t; la fila 0 es el inicio.
	Trans []int16

	nT      int
	allowed []bool    // (T+1)*T
	transF  []float64 // Trans en float64 (enteros exactos)
	slotOf  []string  // nombre del hueco de cada etiqueta ("" para O)
	begin   []bool
	pool    sync.Pool
}

// SlotNames devuelve los huecos que conoce el modelo, en orden.
func (m *Model) SlotNames() []string {
	var out []string
	for i, t := range m.Tags {
		if m.begin[i] {
			out = append(out, t[2:])
		}
	}
	return out
}

// Init valida y prepara el modelo. La llaman el cargador y el entrenador.
func (m *Model) Init() error {
	if err := m.Spec.Validate(); err != nil {
		return err
	}
	if err := validateTags(m.Tags); err != nil {
		return err
	}
	if len(m.Lexicon) > MaxLexicon {
		return fmt.Errorf("lexicon larger than %d entries", MaxLexicon)
	}
	for k, v := range m.Lexicon {
		if k == "" || len(k) > MaxLexKey || v == "" || len(v) > MaxLexKey {
			return errors.New("invalid lexicon entry")
		}
	}
	if h := Hash(m.Spec, m.Tags, m.Lexicon); h != m.SpecHash {
		return fmt.Errorf("spec hash mismatch: file says %016x, spec hashes to %016x", m.SpecHash, h)
	}
	T := len(m.Tags)
	if len(m.W) != int(m.Spec.Buckets)*T || len(m.Trans) != (T+1)*T {
		return errors.New("weights length does not match buckets × tags")
	}
	if math.IsNaN(m.Scale) || math.IsInf(m.Scale, 0) || m.Scale < 0 {
		return errors.New("invalid scale")
	}
	m.nT = T
	m.slotOf = make([]string, T)
	m.begin = make([]bool, T)
	for i, t := range m.Tags {
		if i > 0 {
			m.slotOf[i] = t[2:]
			m.begin[i] = t[0] == 'B'
		}
	}
	m.allowed = allowedTransitions(m.Tags)
	m.transF = make([]float64, len(m.Trans))
	for i, v := range m.Trans {
		m.transF[i] = float64(v)
	}
	return nil
}

func validateTags(tags []string) error {
	if len(tags) < 3 || len(tags) > MaxTags || tags[0] != "O" {
		return fmt.Errorf("tags must start with \"O\" and have 3..%d entries", MaxTags)
	}
	seen := map[string]bool{}
	for _, t := range tags[1:] {
		if len(t) < 3 || len(t) > MaxTagBytes || (t[:2] != "B-" && t[:2] != "I-") || seen[t] {
			return fmt.Errorf("invalid or duplicate tag %q", t)
		}
		seen[t] = true
	}
	for _, t := range tags[1:] {
		other := "I-" + t[2:]
		if t[0] == 'I' {
			other = "B-" + t[2:]
		}
		if !seen[other] {
			return fmt.Errorf("tag %q has no %q", t, other)
		}
	}
	return nil
}

// allowedTransitions: I-x solo tras B-x o I-x (también al principio no).
func allowedTransitions(tags []string) []bool {
	T := len(tags)
	a := make([]bool, (T+1)*T)
	for p := -1; p < T; p++ {
		for t := 0; t < T; t++ {
			ok := true
			if t > 0 && tags[t][0] == 'I' {
				ok = p > 0 && tags[p][2:] == tags[t][2:]
			}
			a[(p+1)*T+t] = ok
		}
	}
	return a
}

// TagsFor construye la lista de etiquetas BIO para unos huecos.
func TagsFor(slots []string) []string {
	s := append([]string(nil), slots...)
	sort.Strings(s)
	tags := []string{"O"}
	for _, x := range s {
		tags = append(tags, "B-"+x, "I-"+x)
	}
	return tags
}

// scratch: búferes de una llamada, en un sync.Pool.
type scratch struct {
	tk    tokenizer
	feats []uint32
	foff  []int32
	emis  []float64
	dp    []float64
	back  []int16
	path  []int16
}

func (m *Model) getScratch() *scratch {
	if s, ok := m.pool.Get().(*scratch); ok {
		return s
	}
	return &scratch{}
}

// Tag devuelve los huecos de text, añadidos a dst. Con dst con capacidad y el
// pool caliente no reserva memoria: Span.Text es un trozo de text.
func (m *Model) Tag(text string, dst []Span) []Span {
	s := m.getScratch()
	if path := m.tags(s, text); len(path) > 0 {
		dst = decodeSpans(text, &s.tk, path, m.slotOf, m.begin, dst)
	}
	m.pool.Put(s)
	return dst
}

// tags tokeniza text en s y devuelve la mejor secuencia de etiquetas.
func (m *Model) tags(s *scratch, text string) []int16 {
	s.tk.run(text, m.Spec.MaxTextBytes, m.Spec.MaxTokens)
	n := len(s.tk.toks)
	if n == 0 {
		return nil
	}
	m.Spec.features(&s.tk, m.Lexicon, &s.feats, &s.foff)
	T := m.nT
	s.emis = growF(s.emis, n*T)
	W := m.W
	for i := 0; i < n; i++ {
		e := s.emis[i*T : i*T+T]
		var acc [MaxTags]int64
		for _, b := range s.feats[s.foff[i]:s.foff[i+1]] {
			row := W[int(b)*T : int(b)*T+T]
			for t, w := range row {
				acc[t] += int64(w)
			}
		}
		for t := range e {
			e[t] = float64(acc[t]) // entero exacto en float64: determinista
		}
	}
	return m.viterbiInt(s, n)
}

// Evaluate mide el F1 de huecos exactos (mismo hueco, mismos tokens) del
// modelo sobre frases con huecos de oro, alineados a tokens como al entrenar.
func (m *Model) Evaluate(sents []Sentence) *SpanReport {
	tagIdx := map[string]int{}
	for i, t := range m.Tags {
		tagIdx[t] = i
	}
	r := newSpanReport(m.Tags)
	s := &scratch{}
	for _, st := range sents {
		path := m.tags(s, st.Text)
		if len(path) == 0 {
			continue
		}
		gold := Align(&s.tk, st.Spans, tagIdx)
		r.add(gold, append([]int16(nil), path...))
	}
	return r.finish()
}

func (m *Model) viterbiInt(s *scratch, n int) []int16 {
	T := m.nT
	s.dp = growF(s.dp, n*T)
	s.back = growI16(s.back, n*T)
	s.path = growI16(s.path, n)
	return viterbi(s.emis, m.transF, m.allowed, n, T, s.dp, s.back, s.path)
}

// viterbi es el decodificador común al modelo cuantizado y al entrenamiento
// (que puntúa en float64). Los empates se rompen por el índice de etiqueta
// menor: determinista.
func viterbi(emis, trans []float64, allowed []bool, n, T int, dp []float64, back, path []int16) []int16 {
	negInf := math.Inf(-1)
	for t := 0; t < T; t++ {
		dp[t] = negInf
		if allowed[t] {
			dp[t] = trans[t] + emis[t]
		}
		back[t] = -1
	}
	for i := 1; i < n; i++ {
		for t := 0; t < T; t++ {
			best, arg := negInf, -1
			for p := 0; p < T; p++ {
				if !allowed[(p+1)*T+t] || dp[(i-1)*T+p] == negInf {
					continue
				}
				v := dp[(i-1)*T+p] + trans[(p+1)*T+t]
				if v > best {
					best, arg = v, p
				}
			}
			if arg < 0 {
				dp[i*T+t] = negInf
			} else {
				dp[i*T+t] = best + emis[i*T+t]
			}
			back[i*T+t] = int16(arg)
		}
	}
	best, arg := negInf, 0
	for t := 0; t < T; t++ {
		if v := dp[(n-1)*T+t]; v > best {
			best, arg = v, t
		}
	}
	for i := n - 1; i >= 0; i-- {
		path[i] = int16(arg)
		if i > 0 {
			arg = int(back[i*T+arg])
			if arg < 0 {
				arg = 0
			}
		}
	}
	return path
}

// decodeSpans convierte la secuencia BIO en huecos sobre el texto original.
func decodeSpans(text string, tk *tokenizer, path []int16, slotOf []string, begin []bool, dst []Span) []Span {
	open := -1
	for i, t := range path {
		tag := int(t)
		switch {
		case tag == 0:
			open = -1
		case begin[tag]:
			dst = append(dst, Span{Slot: slotOf[tag], Start: int(tk.toks[i].os), End: int(tk.toks[i].oe)})
			open = len(dst) - 1
		default: // I-x: Viterbi garantiza que sigue a B-x o I-x del mismo hueco
			if open >= 0 {
				dst[open].End = int(tk.toks[i].oe)
			}
		}
	}
	for i := range dst {
		if dst[i].Text == "" && dst[i].End <= len(text) {
			dst[i].Text = text[dst[i].Start:dst[i].End]
		}
	}
	return dst
}

func growF(b []float64, n int) []float64 {
	if cap(b) < n {
		return make([]float64, n)
	}
	return b[:n]
}

func growI16(b []int16, n int) []int16 {
	if cap(b) < n {
		return make([]int16, n)
	}
	return b[:n]
}

// Características de un token. Las palabras vecinas fuera de la frase son
// <s> y </s>; los números son <d> (el valor exacto no dice nada del hueco);
// un token larguísimo es <long>.
var (
	wStart = []byte("<s>")
	wEnd   = []byte("</s>")
	wNum   = []byte("<d>")
	wLong  = []byte("<long>")
	lNone  = "-"
)

func word(tk *tokenizer, j int) []byte {
	if j < 0 {
		return wStart
	}
	if j >= len(tk.toks) {
		return wEnd
	}
	if tk.toks[j].digit {
		return wNum
	}
	b := tk.tok(j)
	if len(b) > maxTokenBytes {
		return wLong
	}
	return b
}

func lexClass(tk *tokenizer, lex map[string]string, j int) string {
	if j < 0 || j >= len(tk.toks) {
		return lNone
	}
	if tk.toks[j].digit {
		return "<d>"
	}
	if c, ok := lex[string(tk.tok(j))]; ok { // sin reserva: índice con string(bytes)
		return c
	}
	return lNone
}

func shape(tk *tokenizer, j int) byte {
	if tk.toks[j].digit {
		return 'd'
	}
	b := tk.tok(j)
	switch {
	case len(b) == 1 && b[0] == '%':
		return '%'
	case len(b) == 2 && b[0] == 0xc2: // «°» en UTF-8
		return 'o'
	}
	return 'a'
}

var winPrefix = [2][MaxWindow + 1]string{
	{"w0=", "w-1=", "w-2=", "w-3="},
	{"w0=", "w+1=", "w+2=", "w+3="},
}

// features llena feats (cubos, planos) y foff (inicio de cada token; n+1).
func (s *Spec) features(tk *tokenizer, lex map[string]string, feats *[]uint32, foff *[]int32) {
	mask := s.Buckets - 1
	f := (*feats)[:0]
	off := (*foff)[:0]
	emit := func(h uint64) { f = append(f, uint32(fmix64(h))&mask) }
	n := len(tk.toks)
	for i := 0; i < n; i++ {
		off = append(off, int32(len(f)))
		w0 := word(tk, i)
		emit(fnvStr(fnvOffset, "bias"))
		emit(fnvBytes(fnvStr(fnvOffset, "w0="), w0))
		for k := 1; k <= s.Window; k++ {
			emit(fnvBytes(fnvStr(fnvOffset, winPrefix[0][k]), word(tk, i-k)))
			emit(fnvBytes(fnvStr(fnvOffset, winPrefix[1][k]), word(tk, i+k)))
		}
		if s.Window > 0 {
			h := fnvBytes(fnvStr(fnvOffset, "b-="), word(tk, i-1))
			emit(fnvBytes(fnvByte(h, ' '), w0))
			h = fnvBytes(fnvStr(fnvOffset, "b+="), w0)
			emit(fnvBytes(fnvByte(h, ' '), word(tk, i+1)))
		}
		emit(fnvByte(fnvStr(fnvOffset, "sh="), shape(tk, i)))
		if s.Affix > 0 && !tk.toks[i].digit {
			b := tk.tok(i)
			if p := runePrefix(b, s.Affix); p < len(b) {
				emit(fnvBytes(fnvStr(fnvOffset, "p="), b[:p]))
				emit(fnvBytes(fnvStr(fnvOffset, "s="), b[runeSuffix(b, s.Affix):]))
			}
		}
		if s.Lexicon {
			l0 := lexClass(tk, lex, i)
			emit(fnvStr(fnvStr(fnvOffset, "l0="), l0))
			emit(fnvStr(fnvStr(fnvOffset, "l-1="), lexClass(tk, lex, i-1)))
			emit(fnvStr(fnvStr(fnvOffset, "l+1="), lexClass(tk, lex, i+1)))
			h := fnvByte(fnvStr(fnvStr(fnvOffset, "l0w-1="), l0), ' ')
			emit(fnvBytes(h, word(tk, i-1)))
		}
	}
	off = append(off, int32(len(f)))
	*feats, *foff = f, off
}

// runePrefix: bytes de las primeras n runas de b.
func runePrefix(b []byte, n int) int {
	i := 0
	for k := 0; k < n && i < len(b); k++ {
		i++
		for i < len(b) && b[i]&0xC0 == 0x80 {
			i++
		}
	}
	return i
}

// runeSuffix: índice donde empiezan las últimas n runas de b.
func runeSuffix(b []byte, n int) int {
	i := len(b)
	for k := 0; k < n && i > 0; k++ {
		i--
		for i > 0 && b[i]&0xC0 == 0x80 {
			i--
		}
	}
	return i
}

// FNV-1a + finalizador de murmur3, como pkg/jev (ver su hash.go): trivial de
// reproducir en otro lenguaje y con los bits bajos bien mezclados.
const (
	fnvOffset uint64 = 0xcbf29ce484222325
	fnvPrime  uint64 = 0x100000001b3
)

func fnvByte(h uint64, b byte) uint64 { return (h ^ uint64(b)) * fnvPrime }

func fnvStr(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * fnvPrime
	}
	return h
}

func fnvBytes(h uint64, s []byte) uint64 {
	for _, c := range s {
		h = (h ^ uint64(c)) * fnvPrime
	}
	return h
}

func fmix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// String resume el modelo para la CLI.
func (m *Model) String() string {
	return "slots model: " + strconv.Itoa(len(m.SlotNames())) + " slots " + strings.Join(m.SlotNames(), ",") +
		", " + strconv.Itoa(int(m.Spec.Buckets)) + " buckets"
}
