//go:build !darwin

package machine

import "context"

// vigilarEnergia: solo en macOS hay vigía de energía (energia.go).
func (m *Manager) vigilarEnergia(context.Context) {}
