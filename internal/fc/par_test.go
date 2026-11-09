package fc

import (
	"errors"
	"testing"
)

// M3: quien atiende el socket de una jaula tiene que estar enjaulado en ESA
// jaula. La decisión es pura; dial_linux_test.go la prueba con sockets.
func TestComprobarPar(t *testing.T) {
	host := idFichero{1, 2}
	jaulaA, jaulaB := idFichero{1, 100}, idFichero{1, 200}
	const sockA = "/var/lib/kindling/jails/firecracker/aa/root/run/firecracker.socket"

	if err := comprobarPar(sockA, jaulaA, host, jaulaA, nil); err != nil {
		t.Fatalf("su propio VMM: %v", err)
	}
	if err := comprobarPar(sockA, jaulaB, host, jaulaA, nil); err == nil {
		t.Fatal("el socket de A atendido por el VMM de B pasó")
	}
	if err := comprobarPar(sockA, host, host, jaulaA, nil); err == nil {
		t.Fatal("el socket de una jaula atendido por un proceso sin jaula pasó")
	}
	if err := comprobarPar(sockA, jaulaA, host, idFichero{}, errors.New("no existe")); err == nil {
		t.Fatal("sin poder mirar la jaula pasó")
	}
	// Sin jailer (fc.sock) no hay jaula que comprobar.
	if err := comprobarPar("/var/lib/kindling/machines/aa/fc.sock", host, host, idFichero{}, errors.New("x")); err != nil {
		t.Fatalf("un socket sin jaula: %v", err)
	}
}

func TestEsSocketDeJaula(t *testing.T) {
	for ruta, quiero := range map[string]bool{
		"/r/jails/firecracker/aa/root/run/firecracker.socket": true,
		"/r/machines/aa/fc.sock":                              false,
		"/r/jails/firecracker/aa/root/firecracker.socket":     false,
	} {
		if got := esSocketDeJaula(ruta); got != quiero {
			t.Errorf("esSocketDeJaula(%q) = %v", ruta, got)
		}
	}
}
