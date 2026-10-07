package machine

import (
	"context"
	"log"
	"os"
	"time"
)

// Apretar el globo cuando la máquina termina de arrancar.
//
// Un servicio que arranca toca mucha más memoria de la que usa después: el
// kernel del invitado lee la imagen a su caché de página, el proceso carga y
// suelta, y Firecracker no devuelve nada de eso al host por sí solo. Medido en
// el laboratorio: un Postgres recién arrancado en frío ocupaba en el host 169
// MiB de RSS, frente a 63 MiB de una copia restaurada de un dorado (que solo
// faultea lo que toca). Es lo que hace `kling squeeze` a mano: inflar el globo
// hasta dejar al invitado con su margen, que el driver entregue lo libre y la
// caché limpia, y desinflar a la línea base. El invitado conserva todo su
// presupuesto; solo vuelve a entrar lo que de verdad lea otra vez.
//
// Se hace UNA vez, al pasar la sonda de listo de un arranque en frío (la vigía
// de vigilarListo lo ve pasar a "ready"): antes, el servicio aún está
// cargando y lo soltado volvería a entrar enseguida; y solo si la imagen
// declara sonda, que es la única forma de saber que ya terminó. No tras un
// thaw o un run -from: esas comparten su memoria con el dorado (MemShared) y
// el globo la volvería privada (ver squeezeLocked). Solo en Firecracker: en
// macOS el framework vuelve a poblar las páginas al desinflar.
// KLING_SQUEEZE_ON_READY=0 lo apaga.

// plazoApretonListo acota el apretón entero: el inflado espera al driver como
// mucho 3 s (waitBalloon).
const plazoApretonListo = 15 * time.Second

// esperaCerrojoApretonListo es cuánto se reintenta tomar el cerrojo de la
// máquina: Run lo tiene hasta devolverla, y la sonda puede pasar justo antes.
// Si alguien lo tiene más (un save, un freeze), no se aprieta: esas
// operaciones aprietan ellas mismas antes de volcar.
const esperaCerrojoApretonListo = 2 * time.Second

// apretarAlEstarListaActivo dice si el apretón al estar lista corre aquí.
func apretarAlEstarListaActivo() bool {
	return !globoSinEstadisticas && os.Getenv("KLING_SQUEEZE_ON_READY") != "0"
}

// apretarAlEstarLista aprieta en segundo plano el globo de la máquina id, que
// acaba de pasar su sonda de listo tras un arranque en frío. Nunca falla: sin
// globo, sin nada que reclamar o con la máquina ocupada, no hace nada.
func (m *Manager) apretarAlEstarLista(id string) {
	if !apretarAlEstarListaActivo() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), plazoApretonListo)
		defer cancel()
		go func() {
			select {
			case <-m.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		soltar, ok := m.tryLock(id)
		for limite := time.Now().Add(esperaCerrojoApretonListo); !ok && time.Now().Before(limite); {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			soltar, ok = m.tryLock(id)
		}
		if !ok {
			return
		}
		defer soltar()
		res, err := m.squeezeLocked(ctx, id, id, false)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("%s: no squeeze after the ready probe: %v", shortID(id), err)
			}
			return
		}
		log.Printf("%s: squeezed after the ready probe: ~%d MiB returned to the host (RSS now %d MiB)",
			shortID(id), res.ReclaimedMiB, res.RSSMiB)
	}()
}
