//go:build !darwin

package machine

import (
	"net"
	"strconv"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// direccionEsperada es por dónde llega el daemon al puerto port de mc: en
// Linux, la IP de su netns.
func direccionEsperada(mc *api.Machine, port int) string {
	return net.JoinHostPort(knet.Plan(mc.NetIndex, mc.ID).NSIP, strconv.Itoa(port))
}

// ponerReenvio no hace nada en Linux: no hay reenvíos, la dirección sale del
// índice de red.
func ponerReenvio(*api.Machine, int, string) {}
