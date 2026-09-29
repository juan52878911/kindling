//go:build darwin

package machine

import (
	"strconv"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// direccionEsperada es por dónde llega el daemon al puerto port de mc: en
// macOS, su reenvío en el loopback (del rango reservado).
func direccionEsperada(mc *api.Machine, port int) string {
	return mc.Forwards[strconv.Itoa(port)]
}

// ponerReenvio da a mc un reenvío para port: addr, o uno inventado del rango
// reservado si addr es "". Sustituye el mapa (como abrirReenvios).
func ponerReenvio(mc *api.Machine, port int, addr string) {
	if addr == "" {
		addr = "127.0.0.1:" + strconv.Itoa(credproxy.ForwardPortMin+port%1000)
	}
	fwd := make(map[string]string, len(mc.Forwards)+1)
	for k, v := range mc.Forwards {
		fwd[k] = v
	}
	fwd[strconv.Itoa(port)] = addr
	mc.Forwards = fwd
}
