package domotica

import (
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/jev/slots"
)

// Números escritos con palabras, en español e inglés, sobre tokens ya
// plegados (sin acentos). Cubre lo que se dice a un termostato o a una
// persiana: 0..9999, «y medio», «coma cinco», «máximo». No pretende ser un
// analizador general de numerales.

const (
	kUnit = iota + 1
	kTen
	kHundred
)

type numWord struct {
	val  int
	kind int
}

var numberWordsES = map[string]numWord{
	"cero": {0, kUnit}, "uno": {1, kUnit}, "dos": {2, kUnit}, "tres": {3, kUnit}, "cuatro": {4, kUnit},
	"cinco": {5, kUnit}, "seis": {6, kUnit}, "siete": {7, kUnit}, "ocho": {8, kUnit}, "nueve": {9, kUnit},
	"diez": {10, kUnit}, "once": {11, kUnit}, "doce": {12, kUnit}, "trece": {13, kUnit}, "catorce": {14, kUnit},
	"quince": {15, kUnit}, "dieciseis": {16, kUnit}, "diecisiete": {17, kUnit}, "dieciocho": {18, kUnit},
	"diecinueve": {19, kUnit}, "veinte": {20, kTen}, "veintiuno": {21, kUnit}, "veintiun": {21, kUnit},
	"veintiuna": {21, kUnit}, "veintidos": {22, kUnit}, "veintitres": {23, kUnit}, "veinticuatro": {24, kUnit},
	"veinticinco": {25, kUnit}, "veintiseis": {26, kUnit}, "veintisiete": {27, kUnit}, "veintiocho": {28, kUnit},
	"veintinueve": {29, kUnit}, "treinta": {30, kTen}, "cuarenta": {40, kTen}, "cincuenta": {50, kTen},
	"sesenta": {60, kTen}, "setenta": {70, kTen}, "ochenta": {80, kTen}, "noventa": {90, kTen},
	"cien": {100, kHundred}, "ciento": {100, kHundred}, "doscientos": {200, kHundred}, "doscientas": {200, kHundred},
	"trescientos": {300, kHundred}, "cuatrocientos": {400, kHundred}, "quinientos": {500, kHundred},
	"seiscientos": {600, kHundred}, "setecientos": {700, kHundred}, "ochocientos": {800, kHundred},
	"novecientos": {900, kHundred},
}

var numberWordsEN = map[string]numWord{
	"zero": {0, kUnit}, "one": {1, kUnit}, "two": {2, kUnit}, "three": {3, kUnit}, "four": {4, kUnit},
	"five": {5, kUnit}, "six": {6, kUnit}, "seven": {7, kUnit}, "eight": {8, kUnit}, "nine": {9, kUnit},
	"ten": {10, kUnit}, "eleven": {11, kUnit}, "twelve": {12, kUnit}, "thirteen": {13, kUnit},
	"fourteen": {14, kUnit}, "fifteen": {15, kUnit}, "sixteen": {16, kUnit}, "seventeen": {17, kUnit},
	"eighteen": {18, kUnit}, "nineteen": {19, kUnit}, "twenty": {20, kTen}, "thirty": {30, kTen},
	"forty": {40, kTen}, "fifty": {50, kTen}, "sixty": {60, kTen}, "seventy": {70, kTen},
	"eighty": {80, kTen}, "ninety": {90, kTen},
}

// Palabras que valen un número por sí solas («al máximo», «to max», «a la mitad»).
var numberSpecial = map[string]float64{
	"maximo": 100, "maxima": 100, "max": 100, "maximum": 100, "tope": 100, "full": 100,
	"minimo": 0, "minima": 0, "min": 0, "minimum": 0,
	"mitad": 50, "half": 50, "halfway": 50,
}

func lookupNum(w []byte) (numWord, bool) {
	if v, ok := numberWordsES[string(w)]; ok {
		return v, true
	}
	v, ok := numberWordsEN[string(w)]
	return v, ok
}

func eq(b []byte, s string) bool { return string(b) == s }

// parseDigits interpreta «21» o «21.5» sin reservar.
func parseDigits(b []byte) float64 {
	v, frac, div := 0.0, false, 1.0
	for _, c := range b {
		if c == '.' {
			frac = true
			continue
		}
		d := float64(c - '0')
		if frac {
			div *= 10
			v += d / div
		} else {
			v = v*10 + d
		}
	}
	return v
}

// ParseNumberAt lee un número que empiece en el token i: cifras («22»,
// «21.5») o palabras («veintidós», «treinta y cinco», «twenty five», «cien»,
// «y medio»). Devuelve el valor y cuántos tokens consume. No reserva.
func ParseNumberAt(tk *slots.Tokenizer, i int) (float64, int, bool) {
	n := tk.Len()
	if i >= n {
		return 0, 0, false
	}
	j := i
	var v float64
	switch w := tk.Tok(i); {
	case tk.Digit(i):
		v = parseDigits(w)
		j++
	default:
		if sv, ok := numberSpecial[string(w)]; ok {
			return sv, 1, true
		}
		total, cur, last, any := 0, 0, 0, false
	loop:
		for j < n {
			w := tk.Tok(j)
			if nw, ok := lookupNum(w); ok {
				if eq(w, "ciento") {
					// «ciento» solo es número delante de otro («ciento
					// veinte»); suelto es la unidad de «por ciento».
					if j+1 >= n {
						break loop
					}
					if _, next := lookupNum(tk.Tok(j + 1)); !next {
						break loop
					}
				}
				switch nw.kind {
				case kUnit:
					if last == kUnit {
						break loop
					}
				case kTen:
					if last == kUnit || last == kTen {
						break loop
					}
				case kHundred:
					if last != 0 {
						break loop
					}
				}
				cur += nw.val
				last, any = nw.kind, true
				j++
				continue
			}
			switch {
			case (eq(w, "y") || eq(w, "and")) && any && j+1 < n:
				// «treinta y cinco», «one hundred and five»: solo si sigue un
				// número (si no, la «y» une dos órdenes y no es nuestra).
				if nw, ok := lookupNum(tk.Tok(j + 1)); ok && nw.kind == kUnit && last != kUnit {
					j++
					continue
				}
				break loop
			case eq(w, "hundred") && (last == kUnit || !any):
				if cur == 0 {
					cur = 1
				}
				cur *= 100
				last, any = kHundred, true
				j++
			case eq(w, "mil") || eq(w, "thousand"):
				if cur == 0 {
					cur = 1
				}
				total += cur * 1000
				cur, last, any = 0, 0, true
				j++
			default:
				break loop
			}
		}
		if !any {
			return 0, 0, false
		}
		v = float64(total + cur)
	}
	// Fracciones: «y medio», «y media», «and a half», «coma cinco», «point five».
	if j+1 < n && (eq(tk.Tok(j), "y") || eq(tk.Tok(j), "and")) {
		k := j + 1
		if eq(tk.Tok(k), "a") && k+1 < n {
			k++
		}
		if w := tk.Tok(k); eq(w, "medio") || eq(w, "media") || eq(w, "half") {
			return v + 0.5, k + 1 - i, true
		}
	}
	if j+1 < n && (eq(tk.Tok(j), "coma") || eq(tk.Tok(j), "point")) {
		if nw, ok := lookupNum(tk.Tok(j + 1)); ok && nw.val < 10 {
			return v + float64(nw.val)/10, j + 2 - i, true
		}
		if tk.Digit(j + 1) {
			d := parseDigits(tk.Tok(j + 1))
			for d >= 1 {
				d /= 10
			}
			return v + d, j + 2 - i, true
		}
	}
	return v, j - i, true
}

// ParseValue busca el primer número de un texto (el de un hueco «value»).
func ParseValue(text string) (float64, bool) {
	var tk slots.Tokenizer
	tk.Run(text)
	for i := 0; i < tk.Len(); i++ {
		if v, _, ok := ParseNumberAt(&tk, i); ok {
			return v, true
		}
	}
	return 0, false
}

// UnitAfter mira los tokens desde i buscando una unidad: «%», «por ciento»,
// «grados», «°», «fahrenheit»… Devuelve la unidad canónica o "".
func UnitAfter(tk *slots.Tokenizer, i int) string {
	for k := i; k < tk.Len() && k < i+3; k++ {
		w := tk.Tok(k)
		switch {
		case eq(w, "%") || eq(w, "porciento") || eq(w, "percent") || eq(w, "pct"):
			return UnitPercent
		case eq(w, "por") && k+1 < tk.Len() && eq(tk.Tok(k+1), "ciento"),
			eq(w, "per") && k+1 < tk.Len() && eq(tk.Tok(k+1), "cent"):
			return UnitPercent
		case eq(w, "fahrenheit") || eq(w, "f") && k > i:
			return UnitFahrenheit
		case eq(w, "grados") || eq(w, "grado") || eq(w, "degrees") || eq(w, "degree") || eq(w, "°") ||
			eq(w, "celsius") || eq(w, "centigrados"):
			// «grados fahrenheit»: mira un token más.
			if k+1 < tk.Len() && eq(tk.Tok(k+1), "fahrenheit") {
				return UnitFahrenheit
			}
			return UnitCelsius
		}
	}
	return ""
}

// Unidades canónicas.
const (
	UnitPercent    = "%"
	UnitCelsius    = "°C"
	UnitFahrenheit = "°F"
)

var unitsES = [...]string{"cero", "uno", "dos", "tres", "cuatro", "cinco", "seis", "siete", "ocho", "nueve", "diez",
	"once", "doce", "trece", "catorce", "quince", "dieciséis", "diecisiete", "dieciocho", "diecinueve", "veinte",
	"veintiuno", "veintidós", "veintitrés", "veinticuatro", "veinticinco", "veintiséis", "veintisiete", "veintiocho", "veintinueve"}
var tensES = [...]string{"", "", "", "treinta", "cuarenta", "cincuenta", "sesenta", "setenta", "ochenta", "noventa"}
var unitsEN = [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
	"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
var tensEN = [...]string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}

// NumberWords escribe n (0..100) con palabras; fuera de rango, con cifras.
// Lo usa el generador de datos para que el etiquetador vea números escritos.
func NumberWords(n int, lang string) string {
	if n < 0 || n > 100 {
		return strconv.Itoa(n)
	}
	if lang == "es" {
		switch {
		case n == 100:
			return "cien"
		case n < 30:
			return unitsES[n]
		case n%10 == 0:
			return tensES[n/10]
		}
		return tensES[n/10] + " y " + unitsES[n%10]
	}
	switch {
	case n == 100:
		return "one hundred"
	case n < 20:
		return unitsEN[n]
	case n%10 == 0:
		return tensEN[n/10]
	}
	return tensEN[n/10] + " " + unitsEN[n%10]
}

// FormatValue escribe un valor sin ceros sobrantes («22», «21.5»).
func FormatValue(v float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', -1, 64), ".0")
}
