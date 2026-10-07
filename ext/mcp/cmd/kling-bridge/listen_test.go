package main

import (
	"net"
	"strings"
	"testing"
)

// El puente no autentica: sin -listen tiene que quedarse en loopback.
func TestDefaultListenIsLoopback(t *testing.T) {
	host, port, err := net.SplitHostPort(defaultListen)
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
