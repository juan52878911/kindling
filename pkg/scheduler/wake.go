package scheduler

import (
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// WakeTrace desglosa lo que costó tener lista una instancia, del lado del
// planificador: cada fase es tiempo de pared, y Daemon es el desglose del thaw
// que devolvió el daemon (nil si no hubo thaw). Ver docs/despertar.md.
type WakeTrace struct {
	// How es "adopt", "thaw" o "restore", como en OnAcquire.
	How string
	// List es leer la flota del daemon para elegir máquina.
	List time.Duration
	// Renew es renovar su TTL antes de despertarla.
	Renew time.Duration
	// Wake es la llamada que la despierta (thaw o run -from), vista desde aquí.
	Wake time.Duration
	// Ready es esperar a que el puerto del invitado acepte conexiones.
	Ready time.Duration
	// Total va de la entrada en buildEntry a la instancia lista.
	Total time.Duration
	// Daemon es el desglose del daemon (api.Machine.Wake).
	Daemon *api.WakePhases
}

// String es la línea de log de un despertar.
func (t *WakeTrace) String() string {
	if t == nil {
		return ""
	}
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s ms: list %s, renew %s, %s %s, ready %s", t.How, ms(t.Total), ms(t.List), ms(t.Renew),
		t.How, ms(t.Wake), ms(t.Ready))
	if p := t.Daemon; p != nil {
		fmt.Fprintf(&b, " (daemon %.1f: wait %.1f, check %.1f, net %.1f, spawn %.1f, socket %.1f, load %.1f, resync %.1f, cgroup %.1f, finish %.1f)",
			p.TotalMS, p.WaitMS, p.CheckMS, p.NetMS, p.SpawnMS, p.SocketMS, p.LoadMS, p.ResyncMS, p.CgroupMS, p.FinishMS)
	}
	return b.String()
}

// TakeWake devuelve el desglose del despertar que dejó lista esta instancia y
// lo olvida: solo la primera petición servida por ella lo recoge, que es la
// que pagó el despertar (el gateway de IA mide ahí su "primera petición").
func (e *entry) TakeWake() *WakeTrace { return e.wake.Swap(nil) }
