package machine

import (
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Fases de una creación desde un snapshot (runFromCon), en el orden en que
// ocurren. Son las claves del histograma kling_create_phase_ms.
const (
	fcChecks = iota
	fcDisk
	fcNet
	fcPersist
	fcGateWait
	fcVMM
	fcLoad
	fcResume
	fcResync
	fcStart
	fcFinish
	numFasesCreacion
)

// FasesCreacion son los nombres de las fases de api.CreatePhases, en el orden
// de las constantes fc*.
var FasesCreacion = [numFasesCreacion]string{"checks", "disk", "net", "persist", "gate_wait", "vmm", "load",
	"resume", "resync", "start", "finish"}

// cronoCreacion cronometra una creación por fases. Cada marca apunta lo
// transcurrido desde la anterior en su fase, así que las fases no se solapan y
// su suma es el total (marcando "finish" al cerrar) salvo redondeo.
type cronoCreacion struct {
	t0, ultima time.Time
	d          [numFasesCreacion]time.Duration
}

func nuevoCronoCreacion() *cronoCreacion {
	now := time.Now()
	return &cronoCreacion{t0: now, ultima: now}
}

func (c *cronoCreacion) marca(fase int) {
	now := time.Now()
	c.d[fase] += now.Sub(c.ultima)
	c.ultima = now
}

// cerrar marca "finish" y devuelve el desglose, con el total desde t0.
func (c *cronoCreacion) cerrar() *api.CreatePhases {
	c.marca(fcFinish)
	ms := func(i int) float64 { return msDesde(c.d[i]) }
	return &api.CreatePhases{
		ChecksMS: ms(fcChecks), DiskMS: ms(fcDisk), NetMS: ms(fcNet), PersistMS: ms(fcPersist),
		GateWaitMS: ms(fcGateWait), VMMMS: ms(fcVMM), LoadMS: ms(fcLoad), ResumeMS: ms(fcResume),
		ResyncMS: ms(fcResync), StartMS: ms(fcStart), FinishMS: ms(fcFinish),
		TotalMS: msDesde(c.ultima.Sub(c.t0)),
	}
}

// msFasesCreacion son los campos de p en el orden de FasesCreacion.
func msFasesCreacion(p *api.CreatePhases) [numFasesCreacion]float64 {
	return [...]float64{p.ChecksMS, p.DiskMS, p.NetMS, p.PersistMS, p.GateWaitMS, p.VMMMS, p.LoadMS,
		p.ResumeMS, p.ResyncMS, p.StartMS, p.FinishMS}
}

// creacion anota una creación que salió bien: el total y cada fase en su
// histograma. No toca kling_operation_duration_ms{restore}, que solo cubre la
// carga (ver docs/observabilidad.md).
func (t *telemetria) creacion(p *api.CreatePhases) {
	t.durCreacion.observar(int64(p.TotalMS))
	for i, ms := range msFasesCreacion(p) {
		t.fasesCreacion[i].observar(int64(ms))
	}
}

// notaCreacion es el desglose para el registro: "total 52.1 ms: net 3.0, ...".
func notaCreacion(p *api.CreatePhases) string {
	var b strings.Builder
	fmt.Fprintf(&b, "total %.1f ms:", p.TotalMS)
	for i, ms := range msFasesCreacion(p) {
		if ms >= 0.05 {
			fmt.Fprintf(&b, " %s %.1f,", FasesCreacion[i], ms)
		}
	}
	return strings.TrimSuffix(b.String(), ",")
}
