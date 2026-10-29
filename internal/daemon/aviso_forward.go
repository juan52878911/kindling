package daemon

import knet "github.com/juan52878911/kindling/internal/net"

// avisoForward es el aviso de una política del FORWARD en DROP sin salida para
// las máquinas (internal/net/forward.go), o "" si no hay problema o no se
// pudieron leer las reglas. Se mira una vez, al arrancar: con las barreras ya
// puestas, que es lo que el arreglo necesita para quedar detrás.
func avisoForward(leer func() (string, error)) string {
	reglas, err := leer()
	if err != nil {
		return ""
	}
	if a := knet.AvisoForward(reglas); a != "" {
		return "WARNING: " + a
	}
	return ""
}
