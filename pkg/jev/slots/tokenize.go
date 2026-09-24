package slots

import (
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/jev"
)

// Topes del tokenizador: una entrada hostil cuesta lo mismo que una normal.
const (
	DefaultMaxTextBytes = 1024
	MaxTextBytesLimit   = 1 << 14
	DefaultMaxTokens    = 64
	MaxTokensLimit      = 512
	maxTokenBytes       = 40 // más largo es ruido; se sustituye por <long>
)

// Token es un token con su forma normalizada y su posición (en bytes) en el
// texto original: los huecos se devuelven sobre el texto que escribió el
// usuario, no sobre la versión plegada.
type Token struct {
	Norm       string
	Start, End int
}

type tokSpan struct {
	ns, ne int32 // en norm
	os, oe int32 // en el texto original
	digit  bool  // solo dígitos (con un separador decimal como mucho)
}

// tokenizer guarda los búferes de una tokenización; reutilizado, no reserva.
type tokenizer struct {
	norm []byte
	toks []tokSpan
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// run normaliza y parte text. Es el plegado de JEV (jev.FoldRune) con tres
// diferencias que importan a los huecos y no al clasificador: «%» y «°» son
// tokens propios (son la unidad del valor), un decimal «21,5» o «21.5» es un
// solo token (normalizado a «21.5»), y se recuerda la posición original.
func (t *tokenizer) run(text string, maxBytes, maxToks int) {
	t.norm = t.norm[:0]
	t.toks = t.toks[:0]
	if len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	in := false
	var cur tokSpan
	allDigit := true
	closeTok := func(end int) {
		if !in {
			return
		}
		in = false
		cur.ne = int32(len(t.norm))
		cur.oe = int32(end)
		cur.digit = allDigit
		if len(t.toks) < maxToks {
			t.toks = append(t.toks, cur)
		} else {
			t.norm = t.norm[:cur.ns]
		}
	}
	solo := func(s string, start, end int) {
		closeTok(start)
		if len(t.toks) >= maxToks {
			return
		}
		ns := len(t.norm)
		t.norm = append(t.norm, s...)
		t.toks = append(t.toks, tokSpan{ns: int32(ns), ne: int32(len(t.norm)), os: int32(start), oe: int32(end)})
	}
	for i := 0; i < len(text); {
		start := i
		r, size := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		i += size
		switch r {
		case '%':
			solo("%", start, i)
			continue
		case '°', 'º':
			solo("°", start, i)
			continue
		case '.', ',':
			// Separador decimal: el token abierto es todo dígitos y lo que
			// sigue es un dígito.
			if in && allDigit && i < len(text) && isDigit(text[i]) {
				t.norm = append(t.norm, '.')
				continue
			}
		}
		c := jev.FoldRune(r)
		switch {
		case c == jev.FoldSkip:
			continue
		case c == jev.FoldSep:
			closeTok(start)
			continue
		}
		if !in {
			in = true
			cur = tokSpan{ns: int32(len(t.norm)), os: int32(start)}
			allDigit = true
		}
		if c < '0' || c > '9' {
			allDigit = false
		}
		if c < utf8.RuneSelf {
			t.norm = append(t.norm, byte(c))
		} else {
			t.norm = utf8.AppendRune(t.norm, c)
		}
	}
	closeTok(len(text))
}

func (t *tokenizer) tok(i int) []byte {
	s := t.toks[i]
	return t.norm[s.ns:s.ne]
}

// Tokenize parte text con los topes por defecto. Reserva: es para
// herramientas y tests; el camino caliente usa el tokenizador del modelo.
func Tokenize(text string) []Token {
	return TokenizeN(text, DefaultMaxTextBytes, DefaultMaxTokens)
}

// TokenizeN es Tokenize con topes propios (los de la Spec de un modelo).
func TokenizeN(text string, maxBytes, maxToks int) []Token {
	var t tokenizer
	t.run(text, maxBytes, maxToks)
	out := make([]Token, len(t.toks))
	for i, s := range t.toks {
		out[i] = Token{Norm: string(t.norm[s.ns:s.ne]), Start: int(s.os), End: int(s.oe)}
	}
	return out
}

// Tokenizer es el tokenizador reutilizable, para quien quiera trabajar sobre
// los mismos tokens que el etiquetador (el emparejador de plantillas de
// pkg/domotica). Reutilizado, no reserva. No es seguro para uso concurrente.
type Tokenizer struct{ t tokenizer }

// Run tokeniza text con los topes por defecto.
func (t *Tokenizer) Run(text string) { t.t.run(text, DefaultMaxTextBytes, DefaultMaxTokens) }

// Len es el número de tokens.
func (t *Tokenizer) Len() int { return len(t.t.toks) }

// Tok es la forma normalizada del token i (válida hasta el siguiente Run).
func (t *Tokenizer) Tok(i int) []byte { return t.t.tok(i) }

// Pos es la posición en bytes del token i en el texto original.
func (t *Tokenizer) Pos(i int) (start, end int) {
	s := t.t.toks[i]
	return int(s.os), int(s.oe)
}

// Digit dice si el token i es un número escrito con cifras.
func (t *Tokenizer) Digit(i int) bool { return t.t.toks[i].digit }
