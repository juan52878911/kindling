//go:build !darwin

package machine

// Umbrales de PSI "some avg10" (% del tiempo con alguna tarea esperando
// memoria en los últimos 10 s) que equivalen a los niveles de macOS. El
// crítico va por encima del tope de admisión (defaultMaxMemPressure, 20): ahí
// ya no se arranca nada y lo que queda es hacer sitio.
const (
	psiAviso   = 10.0
	psiCritico = 40.0
)

// nivelPresionSO traduce la PSI de memoria de Linux a los niveles de macOS.
// Sin PSI (kernel sin CONFIG_PSI, o psi=0) no hay señal y no se hace nada.
func nivelPresionSO() (int, error) {
	return nivelPresionPSI(memPressure())
}

func nivelPresionPSI(avg10 float64) (int, error) {
	switch {
	case avg10 < 0:
		return 0, errSinPresion
	case avg10 >= psiCritico:
		return NivelPresionCritico, nil
	case avg10 >= psiAviso:
		return NivelPresionAviso, nil
	}
	return NivelPresionNormal, nil
}
