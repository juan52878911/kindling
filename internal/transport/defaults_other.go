//go:build !darwin

package transport

// DefaultRoot es donde guarda sus datos el daemon local.
func DefaultRoot() string { return "/var/lib/kindling" }

// DefaultSocketPath es el socket del daemon local.
func DefaultSocketPath() string { return DefaultSocket }
