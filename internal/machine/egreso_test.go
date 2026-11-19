package machine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

type llamadaEgreso struct {
	viejo, nuevo knet.Egress
	viejos, doms []string
}

func falsoCambioEgreso(t *testing.T, res knet.ResultadoEgreso, err error) *[]llamadaEgreso {
	t.Helper()
	var llamadas []llamadaEgreso
	prev := cambiarEgresoRed
	t.Cleanup(func() { cambiarEgresoRed = prev })
	cambiarEgresoRed = func(_ *knet.Net, v knet.Egress, vd []string, n knet.Egress, d []string) (knet.ResultadoEgreso, error) {
		llamadas = append(llamadas, llamadaEgreso{v, n, vd, d})
		return res, err
	}
	return &llamadas
}

func TestSetEgressVivoPersisteYAvisa(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	sub, cancel := m.bus.Subscribe()
	defer cancel()
	llamadas := falsoCambioEgreso(t, knet.ResultadoEgreso{Estrecha: true, ConexionesCortadas: true}, nil)
	mc := m.addForTest("m1")
	m.mu.Lock()
	mc.Egress, mc.AllowDomains, mc.NetIndex = "allowlist", []string{"a.com", "b.com"}, 3
	m.mu.Unlock()

	out, err := m.SetEgress(context.Background(), "m1", api.EgressRequest{AllowDomains: []string{"a.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Applied != api.EgressAppliedLive || !out.Narrowed || !out.ConnectionsClosed || out.Egress != "allowlist" {
		t.Fatalf("out = %+v", out)
	}
	want := llamadaEgreso{knet.EgressAllowlist, knet.EgressAllowlist, []string{"a.com", "b.com"}, []string{"a.com"}}
	if len(*llamadas) != 1 || !reflect.DeepEqual((*llamadas)[0], want) {
		t.Fatalf("llamadas = %+v", *llamadas)
	}
	got, _ := m.Get("m1")
	if got.Egress != "allowlist" || !reflect.DeepEqual(got.AllowDomains, []string{"a.com"}) {
		t.Fatalf("máquina = %+v", got)
	}
	// Persistido: el estado en disco lo lleva (sobrevive al reinicio del daemon).
	m.persist()
	m.Close()
	var enDisco *api.Machine
	for _, x := range readState(t, m) {
		if x.ID == "m1" {
			x := x
			enDisco = &x
		}
	}
	if enDisco == nil || !reflect.DeepEqual(enDisco.AllowDomains, []string{"a.com"}) {
		t.Fatalf("estado en disco = %+v", enDisco)
	}
	select {
	case ev := <-sub:
		if ev.Type != api.EvEgressChanged || ev.ID != "m1" {
			t.Fatalf("evento %+v", ev)
		}
	default:
		t.Fatal("sin evento de auditoría")
	}
}

func TestSetEgressSinRedSoloAnota(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	llamadas := falsoCambioEgreso(t, knet.ResultadoEgreso{}, nil)
	mc := m.addForTest("m1")
	m.mu.Lock()
	mc.State = api.StateStopped
	m.mu.Unlock()
	out, err := m.SetEgress(context.Background(), "m1", api.EgressRequest{Egress: "internet"})
	if err != nil || out.Applied != api.EgressAppliedNextStart || len(*llamadas) != 0 {
		t.Fatalf("out=%+v err=%v llamadas=%v", out, err, *llamadas)
	}
	if got, _ := m.Get("m1"); got.Egress != "internet" {
		t.Fatalf("egress = %q", got.Egress)
	}
}

func TestSetEgressRechaza(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	llamadas := falsoCambioEgreso(t, knet.ResultadoEgreso{}, nil)
	ctx := context.Background()
	mc := m.addForTest("m1") // egress none

	casos := []struct {
		nombre string
		req    api.EgressRequest
		antes  func()
		sent   error
	}{
		{"modo inventado", api.EgressRequest{Egress: "todo"}, nil, ErrRunRequest},
		{"vacía", api.EgressRequest{}, nil, ErrRunRequest},
		{"allowlist sin dominios", api.EgressRequest{Egress: "allowlist"}, nil, ErrRunRequest},
		{"dominios con internet", api.EgressRequest{Egress: "internet", AllowDomains: []string{"a.com"}}, nil, ErrRunRequest},
		{"dominio inválido", api.EgressRequest{Egress: "allowlist", AllowDomains: []string{"no es un dominio"}}, nil, ErrRunRequest},
		{"con credenciales", api.EgressRequest{Egress: "none"}, func() {
			m.mu.Lock()
			mc.Egress, mc.CredentialDomains = "allowlist", []string{"api.x.com"}
			m.mu.Unlock()
		}, ErrWrongState},
	}
	for _, c := range casos {
		if c.antes != nil {
			c.antes()
		}
		if _, err := m.SetEgress(ctx, "m1", c.req); !errors.Is(err, c.sent) {
			t.Errorf("%s: err = %v, quería %v", c.nombre, err, c.sent)
		}
	}
	if _, err := m.SetEgress(ctx, "nadie", api.EgressRequest{Egress: "none"}); !errors.Is(err, ErrNoMachine) {
		t.Errorf("máquina inexistente: %v", err)
	}
	if len(*llamadas) != 0 {
		t.Fatalf("no debía tocar la red: %+v", *llamadas)
	}
}

func TestSetEgressSinCambioYFalloDeRed(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	llamadas := falsoCambioEgreso(t, knet.ResultadoEgreso{}, errors.New("iptables-restore: boom"))
	m.addForTest("m1")
	out, err := m.SetEgress(context.Background(), "m1", api.EgressRequest{Egress: "none"})
	if err != nil || out.Applied != api.EgressAppliedUnchanged || len(*llamadas) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if _, err := m.SetEgress(context.Background(), "m1", api.EgressRequest{Egress: "internet"}); err == nil {
		t.Fatal("un fallo de red debía devolver error")
	}
	if got, _ := m.Get("m1"); got.Egress != "" && got.Egress != "none" {
		t.Fatalf("no debía persistir si la red falló: %q", got.Egress)
	}
}
