//go:build !darwin

package machine

import (
	"slices"
	"strconv"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Firecracker limita el almacén MMDS (y el cuerpo de cada petición a su API)
// a 50 KiB si no se le dice otra cosa: un entorno de 32 KiB más los secretos
// de sesión no cabía. Se le pasa el tope de api.MaxMMDSBytes, con y sin
// jailer (tras "--", que es lo que jailer entrega a Firecracker).
func TestFirecrackerArrancaConElTopeDeMMDS(t *testing.T) {
	tope := strconv.Itoa(api.MaxMMDSBytes)
	quiere := []string{"--http-api-max-payload-size", tope, "--mmds-size-limit", tope}
	if got := argsVMM(); !slices.Equal(got, quiere) {
		t.Fatalf("argsVMM = %v; quería %v", got, quiere)
	}
	if api.MaxMMDSBytes < 4*api.MaxMachineEnvBytes {
		t.Fatalf("MaxMMDSBytes (%d) no deja sitio al entorno escapado (%d) y a los secretos",
			api.MaxMMDSBytes, api.MaxMachineEnvBytes)
	}

	m := newTestManager(t)
	m.fcBin = "/bin/true"
	m.priv = &Privileges{}
	argv, err := m.jailerArgv("abcdef0123456789", "")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(argv, "--")
	if i < 0 {
		t.Fatalf("sin \"--\" en %v", argv)
	}
	if !slices.Equal(argv[len(argv)-len(quiere):], quiere) || slices.Index(argv[i:], "--mmds-size-limit") < 0 {
		t.Fatalf("jailer no le pasa el tope a Firecracker: %v", argv)
	}
}
