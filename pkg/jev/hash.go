// Package jev es un clasificador lineal diminuto para decisiones pequeñas:
// clasificar eventos, enrutar peticiones de agentes, filtrar entradas. Se
// entrena fuera (pkg/jev/train, `kling jev train`) y se sirve aquí con
// inferencia entera en microsegundos. Ver docs/jev.md.
//
// Lo que este paquete garantiza y por qué importa:
//   - Determinismo: el mismo fichero .jev y la misma entrada dan los mismos bits
//     en amd64 y arm64, macOS y Linux. Por eso el hash es propio (no el de los
//     mapas de Go, que es aleatorio por proceso), los pesos son int16 con
//     acumulación entera, y la poca aritmética en coma flotante del final está
//     escrita para que el compilador no la funda en FMA (ver detExp).
//   - Cascada: cada predicción trae etiqueta, probabilidad calibrada, la
//     decisión frente al umbral de su clase (confident / escalate) y, si se
//     pide, la evidencia. Un gateway responde con JEV cuando está seguro y
//     escala a un modelo mayor cuando no.
//   - Cargar un fichero hostil no revienta ni reserva memoria sin tope.
//
// Solo depende de la biblioteca estándar: sin cgo ni módulos externos.
package jev

// FNV-1a de 64 bits con un finalizador de murmur3. FNV-1a es trivial de
// reimplementar igual en cualquier lenguaje (el entrenador y un cliente futuro
// deben coincidir bit a bit), pero sus bits bajos mezclan mal con claves
// cortas; el finalizador los reparte antes de quedarnos con los bits bajos
// para el cubo y el alto para el signo.

const (
	fnvOffset uint64 = 0xcbf29ce484222325
	fnvPrime  uint64 = 0x100000001b3
)

// FNV1a64 es el hash FNV-1a de 64 bits de s. Exportado para los tests de
// valores dorados y para herramientas que quieran reproducir el hashing.
func FNV1a64(s string) uint64 {
	return fnvStr(fnvOffset, s)
}

func fnvByte(h uint64, b byte) uint64 {
	return (h ^ uint64(b)) * fnvPrime
}

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

// fnvLowerStr mete s en minúsculas ASCII sin reservar: los valores de campos
// estructurados («ERROR» y «error» son lo mismo) se hashean así.
func fnvLowerStr(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		h = (h ^ uint64(c)) * fnvPrime
	}
	return h
}

// fmix64 es el finalizador de murmur3: una biyección que reparte la entropía de
// todos los bits del FNV entre los bits bajos.
func fmix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// bucketSign convierte un hash crudo en (cubo, signo). El signo del hashing
// trick hace que las colisiones se cancelen en media en vez de sumarse siempre
// en la misma dirección.
func bucketSign(h uint64, mask uint32) (uint32, int32) {
	h = fmix64(h)
	sign := int32(1)
	if h>>63 != 0 {
		sign = -1
	}
	return uint32(h) & mask, sign
}
