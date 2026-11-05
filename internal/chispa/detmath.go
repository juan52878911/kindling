package chispa

import "math"

// Aritmética en coma flotante reproducible bit a bit.
//
// La especificación de Go permite fundir x*y+z en una sola instrucción FMA, y el
// compilador lo hace en arm64 (no en amd64 por defecto): el mismo código da
// resultados distintos en el último bit según la máquina. Una conversión
// explícita float64(x*y) obliga a redondear el producto e impide la fusión; por
// eso todos los productos que se suman van envueltos así. Además math.Exp y
// math.Log tienen implementaciones en ensamblador por arquitectura que no
// prometen el mismo redondeo, así que aquí hay versiones propias hechas solo de
// + - * / (que IEEE 754 redondea igual en todas partes), math.Floor,
// math.Frexp y math.Ldexp (exactas).

const (
	ln2Hi  = 6.93147180369123816490e-01 // ln2 partido en dos para la reducción
	ln2Lo  = 1.90821492927058770002e-10
	invLn2 = 1.44269504088896338700e+00
)

// detExp es e^x con error relativo < 2e-16 y el mismo resultado en todas las
// plataformas. Reducción x = k·ln2 + r con |r| <= ln2/2 y Taylor de grado 13
// en r por Horner.
func detExp(x float64) float64 {
	switch {
	case x != x:
		return x
	case x > 709.7:
		return math.Inf(1)
	case x < -745.2:
		return 0
	}
	k := math.Floor(float64(x*invLn2) + 0.5)
	r := float64(x - float64(k*ln2Hi))
	r = float64(r - float64(k*ln2Lo))
	// Horner: 1 + r(1 + r/2(1 + r/3(...))) escrito con coeficientes 1/n!.
	p := 1.0 / 6227020800 // 1/13!
	for _, c := range expCoef {
		p = float64(p*r) + c
	}
	return math.Ldexp(p, int(k))
}

// expCoef son 1/n! para n = 12..0, en el orden de Horner.
var expCoef = [...]float64{
	1.0 / 479001600, 1.0 / 39916800, 1.0 / 3628800, 1.0 / 362880, 1.0 / 40320,
	1.0 / 5040, 1.0 / 720, 1.0 / 120, 1.0 / 24, 1.0 / 6, 1.0 / 2, 1, 1,
}

// detLog es ln(x) reproducible, para la pérdida y la calibración del
// entrenador (que deben elegir lo mismo en cualquier máquina). x = m·2^e con
// m en [√½, √2); ln m = 2·atanh(s), s = (m-1)/(m+1), |s| <= 0,1716.
func detLog(x float64) float64 {
	switch {
	case x != x || x < 0:
		return math.NaN()
	case x == 0:
		return math.Inf(-1)
	case math.IsInf(x, 1):
		return x
	}
	m, e := math.Frexp(x) // m en [0.5, 1)
	if m < math.Sqrt2/2 {
		m *= 2 // exacto
		e--
	}
	s := float64(m-1) / float64(m+1)
	s2 := float64(s * s)
	// 2·(s + s³/3 + s⁵/5 + ... + s²³/23)
	p := 1.0 / 23
	for n := 21; n >= 1; n -= 2 {
		p = float64(p*s2) + 1/float64(n)
	}
	lm := float64(2 * float64(s*p))
	fe := float64(e)
	return float64(fe*ln2Hi) + float64(float64(fe*ln2Lo)+lm)
}

// softmaxInto escribe en p la softmax de z con el máximo restado (estable) y
// la suma en orden fijo. Devuelve el índice del máximo; en empate gana el
// índice menor, para que la etiqueta no dependa de nada más que de los bits.
func softmaxInto(z, p []float64) int {
	best := 0
	for k := 1; k < len(z); k++ {
		if z[k] > z[best] {
			best = k
		}
	}
	m := z[best]
	sum := 0.0
	for k, v := range z {
		p[k] = detExp(float64(v - m))
		sum += p[k]
	}
	for k := range p {
		p[k] /= sum
	}
	return best
}

// DetExp y DetLog exponen las versiones reproducibles para el entrenador, que
// debe dar los mismos pesos en cualquier arquitectura.
func DetExp(x float64) float64 { return detExp(x) }

// DetLog: ver DetExp.
func DetLog(x float64) float64 { return detLog(x) }
