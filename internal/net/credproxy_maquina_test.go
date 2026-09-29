package net

import (
	"strconv"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// El proxy de cada netns pregunta la dirección de una copia al resolvedor de
// la ÚLTIMA entrega (el del manager, atado a esa máquina); sin él, error.
func TestCredProxyResolverMaquina(t *testing.T) {
	p := newCredProxy("")
	t.Cleanup(func() { _ = p.proxy.Close() })
	if _, err := p.resolverMaquina("0123456789abcdef", "local", 5432); err == nil {
		t.Fatal("sin resolvedor debe fallar")
	}
	var ninguno credproxy.ResolveMachineFunc
	p.resolve.Store(&ninguno)
	if _, err := p.resolverMaquina("0123456789abcdef", "local", 5432); err == nil {
		t.Fatal("con un resolvedor nil debe fallar")
	}
	f := credproxy.ResolveMachineFunc(func(id, owner string, port int) (string, error) {
		return id + "|" + owner + "|" + strconv.Itoa(port), nil
	})
	p.resolve.Store(&f)
	if got, err := p.resolverMaquina("0123456789abcdef", "local", 5432); err != nil || got != "0123456789abcdef|local|5432" {
		t.Fatalf("%q %v", got, err)
	}
	// Sin proxies registrados, invalidar no hace nada (ni se cuelga).
	if n := InvalidarMaquina("0123456789abcdef"); n != 0 {
		t.Fatalf("invalidó %d", n)
	}
	if n := InvalidarAgente(Plan(1, "0123456789abcdef")); n != 0 {
		t.Fatalf("invalidó %d", n)
	}
}
