//go:build darwin

package machine

import (
	"context"
	"errors"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// En macOS una credencial con UpstreamMachine no llega nunca a kling-vz, ni
// sin cliente (un almacén copiado de Linux, un reinicio del daemon).
func TestVZRechazaUpstreamMachine(t *testing.T) {
	cred := credproxy.Credential{Env: "PGPASSWORD", Domain: "copia.db.internal", Placeholder: "kling-cred-a", Secret: "s",
		Kind: credproxy.KindPostgres, Port: 5432, User: "app", Database: "appdb",
		UpstreamMachine: idCopia, UpstreamOwner: "local", UpstreamTLS: credproxy.UpstreamTLSDisable}
	err := registrarCredencialesPlataforma(context.Background(), nil, nil, []credproxy.Credential{cred}, "", nil)
	if !errors.Is(err, errModeloASoloLinux) {
		t.Fatalf("esperaba el error de solo Linux: %v", err)
	}
}
