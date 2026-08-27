// Package panico contiene los panicos de los bucles de fondo.
package panico

import (
	"log"
	"runtime/debug"
)

// Contener ejecuta fn y absorbe un panico, dejandolo en el log con su traza.
//
// El motivo es concreto: en Go, un panico en CUALQUIER goroutine se lleva por
// delante el proceso entero. Un nil-pointer en el reconciliador, en el
// persistidor de estado o en el segador del gateway mataria el daemon — y las
// microVM seguirian corriendo, ahora huerfanas, sin nadie que las congele, las
// recoja ni sepa que existen. El fallo mas pequeno posible se convierte en el
// mas grande.
//
// Se envuelve CADA ITERACION del bucle, no el bucle entero. Envolver el bucle
// contendria el panico pero mataria el bucle, y un daemon vivo que ha dejado de
// reconciliar es peor que uno muerto: nada lo delata, y el sistema se degrada en
// silencio hasta que alguien mira. Con esto, una vuelta mala se registra y la
// siguiente vuelve a intentarlo.
//
// No es una excusa para no arreglar el panico: el log lleva la traza entera
// justamente para que se pueda.
func Contener(donde string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic contained in %s: %v\n%s", donde, r, debug.Stack())
		}
	}()
	fn()
}
