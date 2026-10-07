package main

import (
	"net"
	"strings"
	"testing"
)

// El puente no autentica: sin -listen, fuera de la microVM, tiene que
// quedarse en loopback.
func TestDefaultListenIsLoopback(t *testing.T) {
	host, port, err := net.SplitHostPort(defaultListenFor(false))
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("default listen %q is not loopback", defaultListen)
	}
	if port != "8080" {
		t.Fatalf("default port = %s, want 8080", port)
	}
	if w := exposedWarning(defaultListen, false); w != "" {
		t.Fatalf("the default must not warn: %q", w)
	}
}

func TestExposedWarning(t *testing.T) {
	for _, tc := range []struct {
		listen string
		pid1   bool
		warn   bool
	}{
		{"127.0.0.1:9100", false, false},
		{"[::1]:8080", false, false},
		{"localhost:8080", false, false},
		{":8080", false, true},
		{"0.0.0.0:9100", false, true},
		{"[::]:9100", false, true},
		{"192.168.2.3:9100", false, true},
		// Como /entrypoint de la microVM (PID 1) escuchar en todas es el diseño.
		{":8080", true, false},
		{"garbage", false, false},
	} {
		w := exposedWarning(tc.listen, tc.pid1)
		if (w != "") != tc.warn {
			t.Errorf("exposedWarning(%q, pid1=%v) = %q, want warning=%v", tc.listen, tc.pid1, w, tc.warn)
		}
		if w != "" && !strings.Contains(w, "NO authentication") {
			t.Errorf("warning for %q does not say why: %q", tc.listen, w)
		}
	}
}

// Como PID 1 (el /entrypoint de una microVM) sin -listen escucha en :8080:
// el gateway llega por la tap, y una imagen propia sin -listen que recibe el
// puente nuevo con refresh-bridge no puede quedarse en loopback. Y sin aviso.
func TestDefaultListenInsideMicroVM(t *testing.T) {
	got := defaultListenFor(true)
	if got != ":8080" {
		t.Fatalf("default listen as PID 1 = %q, want :8080", got)
	}
	if w := exposedWarning(got, true); w != "" {
		t.Fatalf("the microVM default must not warn: %q", w)
	}
}
