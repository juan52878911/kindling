//go:build darwin && arm64

package hvvm

import (
	"math"
	"testing"
	"time"
)

// TestRTCOffsetSaturado: el offset del RTC viene del snapshot, y con uno
// extremo time(NULL) + offset desbordaba con signo (UBSan lo para con
// SANITIZE=undefined ./scripts/sanitizers-hv.sh). Se satura a lo que el
// invitado puede escribir; un offset normal no cambia.
func TestRTCOffsetSaturado(t *testing.T) {
	for _, off := range []int64{math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, 1 << 50} {
		_, g := rtcRestaurado(off)
		if g > 1<<40 || g < -(1<<40) {
			t.Errorf("restored offset %d stayed %d, want it within ±2^40", off, g)
		}
	}
	dr, g := rtcRestaurado(-3600)
	if g != -3600 {
		t.Errorf("restored offset -3600 became %d", g)
	}
	if d := int64(dr) - (time.Now().Unix() - 3600); d < -5 || d > 5 {
		t.Errorf("DR with offset -3600: %d, %d s off", dr, d)
	}
}
