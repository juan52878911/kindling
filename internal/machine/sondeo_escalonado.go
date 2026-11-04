package machine

import (
	"context"
	"errors"
	"time"
)

// SONDEO ESCALONADO del puerto de un invitado (ver sondearPuertoVMM).
//
// Un intento que se cuelga es uno lanzado antes de que el invitado tuviera su
// vsock (o su red): la petición se pierde y no contesta nunca, aunque el
// agente empiece a escuchar a mitad. Esperarlo entero era lo que más costaba
// del primer exec tras un arranque en frío (medido en el M4 con kling-hv: el
// agente escuchaba a los 108 ms del arranque del kernel y el sondeo llegaba a
// los 161). Por eso, mientras ninguno contesta, cada escalon sale otro nuevo,
// dentro del mismo plazo, y el primero que diga "abierto" gana.
//
// Con pasoCerrado > 0, un "cerrado" (el invitado ya contesta, pero aún no
// escucha nadie: el agente arrancando) tampoco es la respuesta: se vuelve a
// preguntar a los pasoCerrado, hasta el plazo. Es lo que piden quienes
// esperan a que el agente escuche: entre una pregunta y la siguiente, el
// bucle de fuera dormía hasta 50 ms. Con pasoCerrado 0, "cerrado" se devuelve
// en el acto (quien solo quiere saber si escucha ya).
//
// Devuelve (true, nil) si alguno dijo "abierto"; (false, nil) si en el plazo
// nadie lo dijo; (false, err) con el error del VMM (uno sin /kling/probe) o el
// de ctx. Con un invitado vivo contesta el primero y no sale ninguno más.
func sondearEscalonado(ctx context.Context, plazo, escalon, pasoCerrado time.Duration,
	intento func(context.Context) (bool, error)) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, plazo)
	defer cancel()
	type res struct {
		open bool
		err  error
	}
	paso := escalon
	if pasoCerrado > 0 && pasoCerrado < paso {
		paso = pasoCerrado
	}
	// Con hueco para todos: los que acaben después de devolver no se quedan
	// bloqueados.
	maxi := int(plazo/paso) + 2
	out := make(chan res, maxi)
	lanzados, pendientes := 0, 0
	lanzar := func() {
		lanzados++
		pendientes++
		go func() {
			open, err := intento(pctx)
			out <- res{open, err}
		}()
	}
	lanzar()
	sig := time.NewTimer(escalon)
	defer sig.Stop()
	fin := pctx.Done()
	for {
		select {
		case r := <-out:
			pendientes--
			switch {
			case r.err == nil && r.open:
				return true, nil
			case r.err == nil:
				if pasoCerrado <= 0 {
					return false, nil
				}
				// Si no queda ninguno en vuelo, el siguiente sale a los
				// pasoCerrado; si queda alguno colgado, al ritmo de siempre.
				if pendientes == 0 {
					sig.Reset(pasoCerrado)
				}
			case ctx.Err() != nil:
				return false, ctx.Err()
			case !errors.Is(pctx.Err(), context.DeadlineExceeded):
				// Un error del VMM, no un intento agotado: la respuesta.
				return false, r.err
			}
			if pendientes == 0 && pctx.Err() != nil {
				return false, nil
			}
		case <-sig.C:
			if pctx.Err() == nil && lanzados < maxi {
				lanzar()
				sig.Reset(escalon)
			}
		case <-fin:
			fin = nil
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if pendientes == 0 {
				return false, nil
			}
		}
	}
}
