//go:build !darwin

package machine

import (
	"context"
	"errors"
)

// El modo batería es de macOS: en Linux "auto" es no estar nunca en batería, y
// "on" solo espacia el vigilante (sin la CPU del invitado no se sabe si una
// máquina está ociosa, así que no se pausa ninguna).

func qosProceso(int, bool) error { return nil }

func enBateriaSO() (bool, error) { return false, nil }

func cpuInvitado(context.Context, string) (int64, int, error) {
	return -1, 0, errors.New("the guest CPU time is only known on macOS")
}
