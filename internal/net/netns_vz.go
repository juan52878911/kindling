//go:build darwin

package net

// En macOS no hay red en el host que montar: cada kling-vz lleva su propia red
// de espacio de usuario, con la misma forma que el namespace de Linux, y la
// política de salida se le manda por su API (PUT /kling/network) desde
// internal/machine. Estas funciones existen para que el gestor de máquinas no
// tenga que distinguir sistemas en cada arranque: aquí no hacen nada.

// nsIP en macOS es la IP del propio invitado. Es informativa: el host no tiene
// ruta hacia ella y alcanza a cada invitado por los reenvíos de su ayudante
// (api.Machine.Addr).
func nsIP(third, fourth int) string { return GuestIP }

// Setup no monta nada: la red la crea kling-vz al arrancar la máquina.
func (n *Net) Setup(egress Egress, domains []string, owner int) error { return nil }

// Teardown no desmonta nada: la red muere con el proceso del ayudante.
func (n *Net) Teardown() {}

// StartAllowlistResolver no hace falta: el DNS de la lista lo sirve kling-vz.
func (n *Net) StartAllowlistResolver(domains []string) error { return nil }

// Wrap no antepone nada: sin namespace, el VMM se lanza tal cual.
func (n *Net) Wrap(cmd string, args ...string) []string {
	return append([]string{cmd}, args...)
}

// ListNamespaces no encuentra nada que barrer.
func ListNamespaces() []string { return nil }

// TeardownNamespace no tiene nada que borrar.
func TeardownNamespace(ns string) {}

// Available siempre vale: la red no depende de herramientas del host.
func Available() error { return nil }
