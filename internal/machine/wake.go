package machine

import (
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// cronoFases cronometra un despertar por fases (api.WakePhases). Cada marca
// apunta el tiempo transcurrido desde la marca anterior en el campo que se le
// da, así que las fases no se solapan y su suma es el total salvo redondeo.
type cronoFases struct {
	t0, ultima time.Time
	p          api.WakePhases
}

func nuevoCrono(tier string) *cronoFases {
	now := time.Now()
	return &cronoFases{t0: now, ultima: now, p: api.WakePhases{Tier: tier}}
}

func msDesde(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// marca suma a *campo lo transcurrido desde la marca anterior.
func (c *cronoFases) marca(campo *float64) {
	now := time.Now()
	*campo += msDesde(now.Sub(c.ultima))
	c.ultima = now
}

// cerrar apunta lo que queda en FinishMS y el total, y devuelve una copia.
func (c *cronoFases) cerrar() *api.WakePhases {
	c.marca(&c.p.FinishMS)
	c.p.TotalMS = msDesde(time.Since(c.t0))
	p := c.p
	return &p
}

// notaFases es el desglose para el mensaje del evento: solo las fases que
// cuestan algo, para que `kling events` se lea de un vistazo.
func notaFases(p *api.WakePhases) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	add := func(nombre string, v float64) {
		if v < 0.05 {
			return
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %.1f", nombre, v)
	}
	add("wait", p.WaitMS)
	add("check", p.CheckMS)
	add("net", p.NetMS)
	add("spawn", p.SpawnMS)
	add("socket", p.SocketMS)
	add("load", p.LoadMS)
	add("forwards", p.ForwardsMS)
	add("resync", p.ResyncMS)
	add("cgroup", p.CgroupMS)
	add("finish", p.FinishMS)
	return fmt.Sprintf(" [%s %.1f ms: %s]", p.Tier, p.TotalMS, b.String())
}
