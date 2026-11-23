//go:build !darwin

package net

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func stubsSetup(t *testing.T) {
	t.Helper()
	oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA, oldC, oldP := ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost, hayConntrack, existeRuta
	t.Cleanup(func() {
		ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost, hayConntrack, existeRuta = oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA, oldC, oldP
	})
	ordenEnNetns = func(string, ...string) error { return nil }
	ejecutar = func(...string) error { return nil }
	ipBatch = func(string, ...string) error { return nil }
	ejecutarRestore = func(string, string) ([]byte, error) { return nil, nil }
	hayRestore = func() bool { return true }
	hayIp6tables = func() bool { return true }
	hayConntrack = func() bool { return false }
	existeRed = func(*Net) bool { return false }
	existeRuta = func(string) bool { return false }
}

// A-1 paso 2: la comprobación del host corre a la vez que el montaje.
func TestSetupAparteComprobacionEnParalelo(t *testing.T) {
	stubsSetup(t)
	empezo := make(chan struct{})
	asegurarHost = func() error { close(empezo); return nil }
	ejecutar = func(args ...string) error {
		if strings.Join(args, " ") == "ip netns add kl-abcd1234" {
			select {
			case <-empezo:
			case <-time.After(2 * time.Second):
				return errors.New("la comprobación del host no arrancó con el montaje: van en fila")
			}
		}
		return nil
	}
	esperar, err := Plan(7, "abcd1234ef").SetupAparte(EgressInternet, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := esperar(); err != nil {
		t.Fatal(err)
	}
}

// Si la comprobación falla, el montaje no la oculta: la espera devuelve el
// error (quien llama aborta antes de reanudar al invitado).
func TestSetupAparteFalloDeLaComprobacionSaleEnLaEspera(t *testing.T) {
	stubsSetup(t)
	asegurarHost = func() error { return errors.New("no se pudo") }
	esperar, err := Plan(7, "abcd1234ef").SetupAparte(EgressInternet, nil, 1000)
	if err != nil {
		t.Fatalf("el montaje no depende de la comprobación: %v", err)
	}
	if err := esperar(); err == nil || !strings.Contains(err.Error(), "host INPUT rules") {
		t.Fatalf("esperar() = %v, quería el fallo de la comprobación", err)
	}
	// Setup (sincrónico) sigue fallando igual que antes.
	if err := Plan(8, "abcd1234ef").Setup(EgressInternet, nil, 1000); err == nil {
		t.Fatal("Setup debe fallar si la comprobación falla")
	}
}
