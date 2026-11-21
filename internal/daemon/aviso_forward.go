package daemon

import knet "github.com/juan52878911/kindling/internal/net"

// faltaAllowlist dice qué falta en el host para el modo allowlist ("" si
// nada). Variable para los tests: depende de lo instalado.
var faltaAllowlist = knet.FaltaParaAllowlist

// avisoForward es el aviso de una política del FORWARD en DROP sin salida para
// las máquinas (internal/net/forward.go), seguido del de un host sin ipset
// (sin él, -egress allowlist falla y la máquina se queda en none), o "" si no
// hay nada que avisar. El primero, si no se pudieron leer las reglas, no sale.
// Se mira una vez, al arrancar: con las barreras ya puestas, que es lo que el
// arreglo del FORWARD necesita para quedar detrás.
func avisoForward(leer func() (string, error)) string {
	var avisos string
	if reglas, err := leer(); err == nil {
		if a := knet.AvisoForward(reglas); a != "" {
			avisos = "WARNING: " + a
		}
	}
	if f := faltaAllowlist(); f != "" {
		if avisos != "" {
			avisos += "\n"
		}
		avisos += "WARNING: " + f
	}
	return avisos
}
