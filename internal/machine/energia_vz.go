//go:build darwin

package machine

import (
	"context"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// argVigiaEnergia es el modo vigía del binario del VMM (vz/internal/energia).
const argVigiaEnergia = "--power-events"

// vigilarEnergia lanza el vigía de energía (energia.go) con el binario del
// VMM y lo atiende mientras viva el daemon. Si muere, se relanza tras una
// pausa; si ni arranca (un VMM anterior sin el modo), se deja: queda la
// resincronización al despertar.
func (m *Manager) vigilarEnergia(ctx context.Context) {
	if !strings.HasPrefix(filepath.Base(m.fcBin), "kling-") {
		return // tests y binarios de prueba
	}
	for ctx.Err() == nil {
		t0 := time.Now()
		cmd := exec.CommandContext(ctx, m.fcBin, argVigiaEnergia)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		if err := cmd.Start(); err != nil {
			log.Printf("host sleep: no power events (%v); machines are resynced after the host wakes", err)
			return
		}
		m.atenderEnergia(ctx, stdout, stdin)
		stdin.Close()
		err = cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		// Un vigía que no aguanta ni un minuto no se relanza en bucle: un
		// VMM sin el modo sale al momento, siempre.
		if time.Since(t0) < time.Minute {
			log.Printf("host sleep: the power events helper exited (%v); machines are resynced after the host wakes", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}
