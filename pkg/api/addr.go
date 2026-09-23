package api

import (
	"net"
	"sort"
	"strconv"
	"strings"
)

// Addr devuelve la dirección host:puerto por la que el HOST alcanza el puerto
// port del invitado.
//
// Existe porque "IP + puerto" solo es verdad en Linux, donde cada invitado es
// alcanzable por la IP del veth de su namespace. En macOS todos los invitados
// tienen la misma IP y viven en redes de espacio de usuario separadas: se
// llega a ellos por un puerto de loopback que abre su ayudante (Forwards).
// Quien construía la dirección a mano pasa por aquí y no tiene que saber en
// qué sistema corre el daemon.
func (m *Machine) Addr(port int) string {
	if a, ok := m.Forwards[strconv.Itoa(port)]; ok && a != "" {
		return a
	}
	return net.JoinHostPort(m.IP, strconv.Itoa(port))
}

// Reachable dice si el host tiene por dónde llegar a la máquina: una IP (Linux)
// o algún reenvío (macOS). Sustituye a la comprobación "IP != vacía", que en
// macOS no distingue nada porque la IP del invitado es siempre la misma.
func (m *Machine) Reachable() bool {
	return m.IP != "" || len(m.Forwards) > 0
}

// ExposedPorts son los puertos del invitado a los que el host puede llegar:
// GuestPort más los de la etiqueta kling.ports, sin repetir y ordenados. Es la
// misma lista que el proxy del daemon deja pasar, y la que el backend de macOS
// pide reenviar.
func (m *Machine) ExposedPorts() []int {
	visto := map[int]bool{GuestPort: true}
	out := []int{GuestPort}
	for _, p := range strings.Split(m.Labels[LabelPorts], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 65535 || visto[n] {
			continue
		}
		visto[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
