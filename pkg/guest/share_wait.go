package guest

// kling.shares=N: el servicio espera a sus carpetas vivas.
//
// El daemon conecta las carpetas vivas (POST /share/attach) cuando el agente ya
// escucha, y el agente lanzaba el servicio antes de escuchar: `node
// /app/server.js` fallaba una vez con «Cannot find module» y la política de
// reinicio lo levantaba un segundo después; un `node --watch` que arrancaba sin
// el fichero no se recuperaba. Con kling.shares=N el servicio arranca cuando N
// carpetas tienen sesión, o al vencer el plazo (y entonces con un aviso: mejor
// un servicio que arranca sin su carpeta que uno que no arranca nunca).
//
// El plazo cuenta tiempo EN MARCHA, por tics: una plantilla guardada mientras
// su agente espera (RunRequest.ShareWait) se restaura horas después con el
// reloj adelantado, y un temporizador de pared vencería al despertar, antes de
// que el daemon le conecte nada.

import (
	"log"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// shareWaitTick y shareWaitTicks: el plazo, 30 s en marcha.
var (
	shareWaitTick  = 100 * time.Millisecond
	shareWaitTicks = 300
)

// shareWaitWanted es N de kling.shares=N; 0 sin el parámetro o con un valor
// que no se entiende (se avisa: no se espera a medias).
func shareWaitWanted() int {
	return parseShareWait(cmdlineParams().values)
}

func parseShareWait(values map[string]string) int {
	v, ok := values[api.ShareWaitBootParam]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("warning: ignoring %s=%s: not a count", api.ShareWaitBootParam, v)
		return 0
	}
	return n
}

// noteAttached apunta que la carpeta tag tiene sesión y avisa a quien espera.
func (s *Shares) noteAttached(tag int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.withSession == nil {
		s.withSession = map[int]bool{}
	}
	s.withSession[tag] = true
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// waitAttached espera a que n carpetas hayan tenido sesión, como mucho ticks
// tics de tick. Dice si llegaron.
func (s *Shares) waitAttached(n int, tick time.Duration, ticks int) bool {
	t := time.NewTicker(tick)
	defer t.Stop()
	for i := 0; ; {
		s.mu.Lock()
		if len(s.withSession) >= n {
			s.mu.Unlock()
			return true
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			// Tras restaurar, el Ticker entrega UN tic atrasado, no los de
			// todo el tiempo congelado.
			if i++; i >= ticks {
				return false
			}
		}
	}
}

// startAfterShares lanza start cuando lleguen n carpetas (o venza el plazo),
// sin bloquear: las carpetas llegan por el servidor HTTP del agente, que
// arranca después del servicio.
func startAfterShares(sh *Shares, n int, start func()) {
	go func() {
		t0 := time.Now()
		if sh.waitAttached(n, shareWaitTick, shareWaitTicks) {
			log.Printf("service: %d shared folder(s) attached after %s; starting it", n, time.Since(t0).Round(time.Millisecond))
		} else {
			log.Printf("warning: service: %d shared folder(s) did not arrive in %s; starting it without them",
				n, time.Duration(shareWaitTicks)*shareWaitTick)
		}
		start()
	}()
}
