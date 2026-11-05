package main

import (
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// hostPortFlag acumula -host-port PUERTO o HOST:GUEST (api.ParseHostPort):
// puertos TCP del loopback del Mac que el invitado alcanza en
// host.kindling.internal. Admite también varios separados por comas.
type hostPortFlag []api.HostPort

func (h *hostPortFlag) String() string {
	var out []string
	for _, p := range *h {
		out = append(out, p.String())
	}
	return strings.Join(out, ",")
}

func (h *hostPortFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if strings.TrimSpace(s) == "" {
			continue
		}
		p, err := api.ParseHostPort(s)
		if err != nil {
			return err
		}
		*h = append(*h, p)
	}
	return api.ValidateHostPorts(*h)
}
