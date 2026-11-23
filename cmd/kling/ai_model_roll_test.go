package main

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const registroRoll = `{
  "models": {
    "srv": {"kind": "http", "snapshot": "srv-v1", "port": 8090, "contract": "chispa-decide", "tenants": ["acme"]},
    "otro": {"kind": "von", "snapshot": "von-smol"}
  }
}
`

func daemonRoll() *daemonModelos {
	return &daemonModelos{
		snaps: []*api.Snapshot{
			{Name: "srv-v1", Labels: map[string]string{api.LabelPorts: "8090"}},
			{Name: "srv-v2", Labels: map[string]string{api.LabelPorts: "8091"}},
			{Name: "srv-v10x"}, {Name: "von-smol"},
		},
		maquinas: []*api.Machine{
			{ID: "r1", Name: "srv-v1-a", From: "srv-v1", State: api.StateWarm},
			{ID: "r2", Name: "srv-v1-b", From: "srv-v1", State: api.StateRunning},
			{ID: "r3", Name: "srv-v2-a", From: "srv-v2", State: api.StateRunning},
			{ID: "r4", Name: "von-1", From: "von-smol", State: api.StateWarm},
			{ID: "r5", Name: "suelta", State: api.StateRunning, Labels: map[string]string{api.LabelService: "srv-v1"}},
		},
		// r2 sigue con una petición en vuelo durante dos consultas y luego el
		// gateway la congela por ociosa.
		congelarTras: map[string]int{"r2": 2},
	}
}

func idsQuedan(d *daemonModelos) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, m := range d.maquinas {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// R8-06a: roll apunta ai.json al dorado nuevo, recarga el gateway y borra las
// réplicas del viejo cuando dejan de trabajar; las del nuevo y las de otros
// modelos no se tocan.
func TestRollCambiaVersion(t *testing.T) {
	cfg := escribeRegistro(t, registroRoll)
	d := daemonRoll()
	c := d.cliente(t)
	recargas, quiero := 0, "srv-v2"
	reload := func(context.Context) error {
		recargas++
		// Cuando el gateway relee, ai.json ya dice la versión nueva.
		if leeModelos(t, cfg)["srv"]["snapshot"] != quiero {
			t.Error("recargó antes de reescribir ai.json")
		}
		return nil
	}
	r, err := modelRoll(context.Background(), c, reload, rollOptions{Model: "srv", Config: cfg, Drain: 5 * time.Second, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if r.From != "srv-v1" || r.To != "srv-v2" || recargas != 1 {
		t.Fatalf("resultado %+v, recargas %d", r, recargas)
	}
	ms := leeModelos(t, cfg)
	b := ms["srv"]
	// El puerto lo trae el dorado nuevo; el resto del bloque se queda.
	if b["snapshot"] != "srv-v2" || b["port"] != float64(8091) || b["contract"] != "chispa-decide" || b["kind"] != "http" {
		t.Fatalf("bloque: %v", b)
	}
	if ts, _ := b["tenants"].([]any); len(ts) != 1 || ts[0] != "acme" {
		t.Fatalf("tenants: %v", b["tenants"])
	}
	if ms["otro"]["snapshot"] != "von-smol" {
		t.Fatalf("otro: %v", ms["otro"])
	}
	// r2 corría: se esperó a que quedara ociosa, no se cortó.
	if got := idsQuedan(d); got != "r3,r4,r5" {
		t.Fatalf("quedan %s (borradas %v)", got, r.Removed)
	}
	sort.Strings(r.Removed)
	if strings.Join(r.Removed, ",") != "srv-v1-a,srv-v1-b" {
		t.Fatalf("borradas: %v", r.Removed)
	}

	// Y de vuelta con -to: la v1 se conservó.
	quiero = "srv-v1"
	r, err = modelRoll(context.Background(), c, reload, rollOptions{Model: "srv", To: 1, Config: cfg, Drain: time.Second, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if r.To != "srv-v1" || leeModelos(t, cfg)["srv"]["port"] != float64(8090) || idsQuedan(d) != "r4,r5" {
		t.Fatalf("vuelta: %+v, quedan %s", r, idsQuedan(d))
	}
}

// Si nada cambia, nada se recarga ni se borra.
func TestRollYaSirve(t *testing.T) {
	cfg := escribeRegistro(t, registroRoll)
	d := daemonRoll()
	r, err := modelRoll(context.Background(), d.cliente(t), func(context.Context) error {
		t.Error("no debía recargar")
		return nil
	}, rollOptions{Model: "srv", To: 1, Config: cfg})
	if err != nil || r.From != r.To {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != registroRoll || idsQuedan(d) != "r1,r2,r3,r4,r5" {
		t.Fatal("no debía tocar nada")
	}
}

// Una recarga que falla deja ai.json como estaba y no borra réplicas: el
// gateway sigue sirviendo el dorado viejo.
func TestRollRecargaFallaRestaura(t *testing.T) {
	cfg := escribeRegistro(t, registroRoll)
	d := daemonRoll()
	_, err := modelRoll(context.Background(), d.cliente(t), func(context.Context) error {
		return errors.New("connection refused")
	}, rollOptions{Model: "srv", Config: cfg, Drain: time.Second, Poll: 10 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "left as it was") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != registroRoll {
		t.Fatalf("ai.json no se restauró:\n%s", b)
	}
	if idsQuedan(d) != "r1,r2,r3,r4,r5" {
		t.Fatalf("borró réplicas: %s", idsQuedan(d))
	}
}

// Una réplica vieja que no deja de trabajar se borra al agotar -drain.
func TestRollDrainAgota(t *testing.T) {
	cfg := escribeRegistro(t, registroRoll)
	d := daemonRoll()
	d.congelarTras = nil
	t0 := time.Now()
	r, err := modelRoll(context.Background(), d.cliente(t), func(context.Context) error { return nil },
		rollOptions{Model: "srv", Config: cfg, Drain: 150 * time.Millisecond, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) < 150*time.Millisecond || len(r.Removed) != 2 || idsQuedan(d) != "r3,r4,r5" {
		t.Fatalf("%v, %+v, quedan %s", time.Since(t0), r, idsQuedan(d))
	}
}

// -no-reload solo reescribe el fichero.
func TestRollSinRecarga(t *testing.T) {
	cfg := escribeRegistro(t, registroRoll)
	d := daemonRoll()
	r, err := modelRoll(context.Background(), d.cliente(t), nil, rollOptions{Model: "srv", Config: cfg, NoReload: true})
	if err != nil || r.To != "srv-v2" || leeModelos(t, cfg)["srv"]["snapshot"] != "srv-v2" || idsQuedan(d) != "r1,r2,r3,r4,r5" {
		t.Fatalf("%+v %v, quedan %s", r, err, idsQuedan(d))
	}
}

func TestRollErrores(t *testing.T) {
	cfg := escribeRegistro(t, `{"models": {"srv": {"kind": "http", "upstream": "http://10.0.0.1:8000", "contract": "openai"}}}`)
	d := daemonRoll()
	c := d.cliente(t)
	for _, caso := range []struct {
		o   rollOptions
		msg string
	}{
		{rollOptions{Model: "srv", Config: cfg}, "upstream"},
		{rollOptions{Model: "nadie", Config: cfg}, "no model"},
		{rollOptions{Model: "srv", Config: cfg + ".no"}, "does not exist"},
	} {
		if _, err := modelRoll(context.Background(), c, nil, caso.o); err == nil || !strings.Contains(err.Error(), caso.msg) {
			t.Fatalf("%+v: %v", caso.o, err)
		}
	}
	cfg = escribeRegistro(t, registroRoll)
	if _, err := modelRoll(context.Background(), c, nil, rollOptions{Model: "srv", To: 7, Config: cfg}); err == nil || !strings.Contains(err.Error(), "srv-v7") {
		t.Fatalf("-to inexistente: %v", err)
	}
}
