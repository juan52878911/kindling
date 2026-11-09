//go:build darwin && arm64

package hvvm

import "testing"

// Una VM ociosa no despierta al vigilante de QoS por reloj (era cada 2 s, medio
// despertar por segundo por VM): la vuelta de reposo es solo la red de
// seguridad y no da vueltas mientras el vCPU duerme sin tics.
func TestQoSReposoNoDespierta(t *testing.T) {
	if ms := qosLentaMs(); ms < 10000 {
		t.Fatalf("QOS_LENTA_MS = %d, quiero >= 10000", ms)
	}
	n := pruebaQoS(false, 600)
	if n == -2 {
		t.Skip("no se pudo preparar el vigilante sin VM")
	}
	if n != 0 {
		t.Fatalf("el vigilante dio %d vueltas en 600 ms con el vCPU ocioso", n)
	}
}

// Un vCPU que gira por su cuenta (un cron, un proceso que ya corría) sube
// pronto aunque no haya vuelta lenta: lo despiertan sus tics del timer virtual.
// Con la vuelta fija de 2 s tardaba al menos 2 s.
func TestQoSSubeConLosTics(t *testing.T) {
	ms := pruebaQoS(true, 3000)
	if ms == -2 {
		t.Skip("no se pudo preparar el vigilante sin VM")
	}
	if ms < 0 || ms > 1200 {
		t.Fatalf("el vCPU ocupado subió a los %d ms, quiero <= 1200", ms)
	}
}
