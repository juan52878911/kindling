package api

import (
	"reflect"
	"strconv"
	"strings"
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

func TestExposes(t *testing.T) {
	m := &Machine{Labels: map[string]string{LabelPorts: "5432, 80,x"}}
	for port, want := range map[int]bool{GuestPort: true, 5432: true, 80: true, 81: false, 0: false} {
		if got := m.Exposes(port); got != want {
			t.Errorf("Exposes(%d) = %v, quería %v", port, got, want)
		}
	}
	if (&Machine{}).Exposes(5432) {
		t.Error("sin etiqueta solo se expone el puerto del agente")
	}
}

func TestValidateForkLabels(t *testing.T) {
	if err := ValidateForkLabels(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateForkLabels(map[string]string{"kling.db.state": "preparing"}); err != nil {
		t.Fatal(err)
	}
	malas := []map[string]string{
		{"Mal": "x"}, {"": "x"}, {LabelKind: "x"}, {LabelForkOf: "x"},
		{"k": strings.Repeat("v", 257)},
	}
	many := map[string]string{}
	for i := 0; i < 33; i++ {
		many["k"+strconv.Itoa(i)] = "v"
	}
	malas = append(malas, many)
	for _, l := range malas {
		if ValidateForkLabels(l) == nil {
			t.Errorf("se aceptó %v", l)
		}
	}
}
