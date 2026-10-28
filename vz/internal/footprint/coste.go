package footprint

import "time"

// La CPU que cobra el tope de CPU, en tiempo de núcleo de rendimiento (P).
//
// Un Mac con Apple Silicon tiene núcleos P y núcleos de eficiencia (E), y el
// mismo trabajo cuesta en un E bastante más tiempo de CPU. El tope de CPU
// (server/cpu.go) es "pct % de un núcleo" contado en tiempo, y quien elige el
// núcleo es el planificador, no nosotros: medido en un M4 con kling-vz lanzado
// por launchd como el daemon, con 1 vCPU y techo 25 % el hilo de vCPU pasaba el
// 97 % del tiempo en núcleos E (ri_user_ptime: 30 ms en P de 1170), donde las
// mismas 6.600 millones de instrucciones costaban 3,3 veces más tiempo; al 50 %
// y sin techo iba todo en P. Contado en tiempo a secas, el techo al 25 % frenaba
// ×14 en vez de ×4. Lanzado desde una terminal (la coalición de una app) se
// quedaba en P y no se notaba, y subir la QoS de los hilos de kling-vz no lo
// cambia: los hilos de vCPU son del auxiliar.
//
// Así que el tiempo en núcleos E se cobra a lo que vale en P: por la velocidad
// relativa que se mide en la propia VM (instrucciones por nanosegundo en E
// frente a en P, acumuladas desde que arrancó su auxiliar). Sin bastante
// muestra en P (o en un Mac sin núcleos P/E, donde ri_user_ptime es 0) se cobra
// el tiempo tal cual, que es lo de antes.

// lectura es lo que da proc_pid_rusage del auxiliar: CPU total y en núcleos
// P, e instrucciones totales y en P. Todo acumulado desde que arrancó.
type lectura struct {
	total, p      time.Duration
	instr, instrP uint64
}

const (
	// muestraMinP es lo menos que tiene que haber corrido en P para fiarse de
	// su velocidad.
	muestraMinP = 20 * time.Millisecond
	// muestraMinE, lo mismo en E: por debajo, la proporción es ruido.
	muestraMinE = 5 * time.Millisecond
	// relMin acota lo poco que puede valer un ms en E: ni un contador raro
	// debe dejar a una VM sin techo.
	relMin = 0.15
)

// relativaE es lo que vale un ns en E en ns de P (entre relMin y 1), o 1 si no
// hay con qué medirlo.
func relativaE(l lectura) float64 {
	tE := l.total - l.p
	if l.p < muestraMinP || tE < muestraMinE || l.instrP == 0 || l.instr <= l.instrP {
		return 1
	}
	ipsP := float64(l.instrP) / float64(l.p)
	ipsE := float64(l.instr-l.instrP) / float64(tE)
	return min(max(ipsE/ipsP, relMin), 1)
}

// contador lleva la CPU equivalente de un auxiliar. Va por incrementos para
// que no baje nunca aunque cambie la proporción: el regulador toma una bajada
// por un contador reiniciado.
type contador struct {
	ult   lectura
	equiv time.Duration
	listo bool
}

// suma incorpora una lectura nueva y devuelve la CPU equivalente acumulada.
func (c *contador) suma(l lectura) time.Duration {
	if !c.listo || l.total < c.ult.total || l.p < c.ult.p {
		// Primera lectura (o un auxiliar nuevo): se parte de su total.
		c.ult, c.equiv, c.listo = l, l.total, true
		return c.equiv
	}
	dP := l.p - c.ult.p
	dE := (l.total - l.p) - (c.ult.total - c.ult.p)
	if dE < 0 {
		dE = 0
	}
	c.equiv += dP + time.Duration(float64(dE)*relativaE(l))
	c.ult = l
	return c.equiv
}
