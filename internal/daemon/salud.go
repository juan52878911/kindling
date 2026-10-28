package daemon

// La sonda de los healthchecks de las aristas depends (machine/grafo_salud.go):
// el mismo exec que POST /machines/{ref}/exec?wait=1, sin pasar por HTTP.

import (
	"context"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// sondaSalud ejecuta argv dentro de mc y devuelve su código de salida. La
// salida se descarta (como mucho 4 KiB): solo cuenta el código.
func (s *Server) sondaSalud(ctx context.Context, mc *api.Machine, argv []string, plazo time.Duration) (int, error) {
	if err := s.waitAgent(ctx, mc); err != nil {
		return 0, err
	}
	seg := max(int(plazo/time.Second), 1)
	req := api.ExecRequest{Cmd: argv, TimeoutSeconds: seg, MaxOutputBytes: 4 << 10}
	body, code, err := openGuestExec(ctx, guestBase(mc), req)
	if err != nil {
		if code == http.StatusNotImplemented {
			err = tooOldAgent(mc)
		}
		return 0, err
	}
	defer body.Close()
	res, err := aggregateExec(readExecEvents(body, req.MaxOutputBytes))
	if err != nil {
		return 0, err
	}
	if res.TimedOut {
		return -1, nil
	}
	return res.ExitCode, nil
}
