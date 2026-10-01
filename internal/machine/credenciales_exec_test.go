package machine

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// exec y shell reciben el marcador de cada credencial (nunca la clave): tras
// `kling db attach`, PGPASSWORD está en el entorno del comando sin leer MMDS.
func TestEnvCredencialesSoloMarcadores(t *testing.T) {
	m := newTestManager(t)
	if got := m.EnvCredenciales("m1"); got != nil {
		t.Fatalf("sin credenciales: %v", got)
	}
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := []credproxy.Credential{
		{Env: "PGPASSWORD", Domain: "copia.db.internal", Placeholder: "kling-cred-pg", Secret: credSecreto},
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-aa", Secret: credSecreto},
	}
	if err := m.guardarCredenciales("m1", creds); err != nil {
		t.Fatal(err)
	}
	got := m.EnvCredenciales("m1")
	slices.Sort(got)
	if want := []string{"KEY=kling-cred-aa", "PGPASSWORD=kling-cred-pg"}; !slices.Equal(got, want) {
		t.Fatalf("env = %v, quería %v", got, want)
	}
	if strings.Contains(strings.Join(got, " "), credSecreto) {
		t.Fatal("la clave llegó al entorno del exec")
	}
}
