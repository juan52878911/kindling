package main

import (
	"reflect"
	"testing"
)

// Los dos ficheros reales del caso: el que deja systemd-resolved en el
// anfitrion y el que debe quedar dentro del invitado.
func TestNameserversDeLeeLosCasosReales(t *testing.T) {
	casos := []struct {
		nombre string
		texto  string
		quiero []string
	}{
		{
			"el que deja systemd-resolved (y que rompia la navegacion)",
			"# This is /run/systemd/resolve/stub-resolv.conf managed by man:systemd-resolved(8).\n" +
				"# Do not edit.\n#\nnameserver 127.0.0.53\noptions edns0 trust-ad\nsearch proxmox.home\n",
			[]string{"127.0.0.53"},
		},
		{
			"el resolver real del anfitrion, que el cortafuegos bloquea",
			"nameserver 192.168.2.1\nsearch proxmox.home\n",
			[]string{"192.168.2.1"},
		},
		{
			"el que se pone ahora",
			"nameserver 1.1.1.1\nnameserver 8.8.8.8\n",
			[]string{"1.1.1.1", "8.8.8.8"},
		},
		{"vacio", "", nil},
		{"solo comentarios", "# nada\n; tampoco\n", nil},
		{"con espacios de sobra", "   nameserver    9.9.9.9   \n", []string{"9.9.9.9"}},
		{"una linea que solo dice nameserver", "nameserver\n", nil},
	}
	for _, c := range casos {
		got := nameserversDe(c.texto)
		if !reflect.DeepEqual(got, c.quiero) {
			t.Errorf("%s: %v, esperaba %v", c.nombre, got, c.quiero)
		}
	}
}
