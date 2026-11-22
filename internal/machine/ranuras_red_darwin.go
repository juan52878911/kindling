package machine

import knet "github.com/juan52878911/kindling/internal/net"

// En macOS la red es de espacio de usuario dentro de kling-vz: no hay netns
// que preparar y no hay fondo de ranuras.

func (m *Manager) ranuraTomar() (string, bool)     { return "", false }
func (m *Manager) ranuraEntregar(string) *knet.Net { return nil }
func (m *Manager) ranuraPropia(string) bool        { return false }
func (m *Manager) ranuraPrefijoEnUso(string) bool  { return false }
func (m *Manager) cerrarFondoRed()                 {}
