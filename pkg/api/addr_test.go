package api

import (
	"reflect"
	"testing"
)

func TestAddrSinReenviosUsaLaIP(t *testing.T) {
	m := &Machine{IP: "172.30.0.2"}
	if got := m.Addr(8080); got != "172.30.0.2:8080" {
		t.Fatalf("Addr = %q", got)
	}
	if !m.Reachable() {
		t.Fatal("una máquina con IP es alcanzable")
	}
}

func TestAddrConReenvioGanaAlaIP(t *testing.T) {
	m := &Machine{IP: "172.16.0.2", Forwards: map[string]string{"8080": "127.0.0.1:61234"}}
	if got := m.Addr(8080); got != "127.0.0.1:61234" {
		t.Fatalf("Addr(8080) = %q", got)
	}
	// Un puerto sin reenvío cae a la IP: no inventa una dirección.
	if got := m.Addr(9000); got != "172.16.0.2:9000" {
		t.Fatalf("Addr(9000) = %q", got)
	}
}

func TestAddrIPv6(t *testing.T) {
	m := &Machine{IP: "fd00::2"}
	if got := m.Addr(80); got != "[fd00::2]:80" {
		t.Fatalf("Addr = %q", got)
	}
}

func TestReachableSoloConReenvios(t *testing.T) {
	if (&Machine{}).Reachable() {
		t.Fatal("sin IP ni reenvíos no es alcanzable")
	}
	if !(&Machine{Forwards: map[string]string{"8080": "127.0.0.1:1"}}).Reachable() {
		t.Fatal("con reenvíos es alcanzable")
	}
}

func TestExposedPorts(t *testing.T) {
	m := &Machine{Labels: map[string]string{LabelPorts: " 9000, 8080,abc,70000,9000,3000"}}
	want := []int{3000, 8080, 9000}
	if got := m.ExposedPorts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ExposedPorts = %v, quiero %v", got, want)
	}
	if got := (&Machine{}).ExposedPorts(); !reflect.DeepEqual(got, []int{GuestPort}) {
		t.Fatalf("sin etiqueta = %v", got)
	}
}
