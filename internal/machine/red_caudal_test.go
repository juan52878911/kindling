package machine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

func TestCaudalRedPrecedencia(t *testing.T) {
	daemon := func(n int) func() int { return func() int { return n } }
	for _, c := range []struct {
		nombre string
		pedido int
		daemon func() int
		mib    int64
	}{
		{"nada: el de siempre", 0, nil, defaultNetMiBps},
		{"daemon sin valor", 0, daemon(0), defaultNetMiBps},
		{"el del daemon", 0, daemon(64), 64},
		{"la petición gana al daemon", 256, daemon(64), 256},
		{"daemon negativo: el de siempre", 0, daemon(-3), defaultNetMiBps},
		{"daemon por encima del tope: el de siempre, no sin límite", 0, daemon(config.MaxNetMiBps + 1), defaultNetMiBps},
	} {
		if got := caudalRed(c.pedido, c.daemon); got != c.mib<<20 {
			t.Errorf("%s: %d B/s, quería %d MiB/s", c.nombre, got, c.mib)
		}
	}
}

func TestValidarNetMiBps(t *testing.T) {
	for _, n := range []int{0, 1, 256, config.MaxNetMiBps} {
		if err := validarNetMiBps(n); err != nil {
			t.Errorf("%d: %v", n, err)
		}
	}
	for _, n := range []int{-1, config.MaxNetMiBps + 1} {
		if err := validarNetMiBps(n); err == nil {
			t.Errorf("%d: aceptado", n)
		}
	}
}

// Lo que se manda a PUT /network-interfaces/eth0 lleva el caudal de la
// máquina en los dos limitadores, y sin pedir nada, el del daemon.
func TestInterfazRedLlegaAFirecracker(t *testing.T) {
	m := newTestManager(t)
	m.SetNetMiBps(func() int { return 32 })
	m.mu.Lock()
	m.byID["aaaa000000000001"] = &api.Machine{ID: "aaaa000000000001", NetMiBps: 256}
	m.byID["aaaa000000000002"] = &api.Machine{ID: "aaaa000000000002"}
	m.mu.Unlock()
	for id, mib := range map[string]int64{"aaaa000000000001": 256, "aaaa000000000002": 32} {
		b, err := json.Marshal(m.interfazRed(id))
		if err != nil {
			t.Fatal(err)
		}
		var ni struct {
			Rx, Tx struct {
				Bandwidth struct {
					Size       int64 `json:"size"`
					RefillTime int64 `json:"refill_time"`
				} `json:"bandwidth"`
			}
		}
		// Los nombres del API de Firecracker, no los del struct de Go.
		js := strings.NewReplacer(`"rx_rate_limiter"`, `"Rx"`, `"tx_rate_limiter"`, `"Tx"`).Replace(string(b))
		if err := json.Unmarshal([]byte(js), &ni); err != nil {
			t.Fatal(err)
		}
		if ni.Rx.Bandwidth.Size != mib<<20 || ni.Tx.Bandwidth.Size != mib<<20 || ni.Rx.Bandwidth.RefillTime != 1000 {
			t.Errorf("%s: %s, quería %d MiB/s en rx y tx", id, b, mib)
		}
	}
}
