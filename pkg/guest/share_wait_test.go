package guest

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestParseShareWait(t *testing.T) {
	for v, want := range map[string]int{"2": 2, "0": 0, "x": 0, "-1": 0} {
		if got := parseShareWait(map[string]string{api.ShareWaitBootParam: v}); got != want {
			t.Errorf("%s=%s: %d, want %d", api.ShareWaitBootParam, v, got, want)
		}
	}
	if got := parseShareWait(map[string]string{}); got != 0 {
		t.Errorf("without the parameter: %d", got)
	}
	if p := parseBootParams("console=hvc0 kling.shares=1"); len(p.unknown) != 0 || p.values[api.ShareWaitBootParam] != "1" {
		t.Errorf("kling.shares is not a known parameter: %+v", p)
	}
}

func TestStartAfterSharesEsperaASusCarpetas(t *testing.T) {
	sh := newShares()
	started := make(chan struct{})
	old, oldN := shareWaitTick, shareWaitTicks
	shareWaitTick, shareWaitTicks = time.Millisecond, 5000
	defer func() { shareWaitTick, shareWaitTicks = old, oldN }()
	startAfterShares(sh, 2, func() { close(started) })
	sh.noteAttached(0)
	sh.noteAttached(0) // la misma carpeta reconectada no cuenta dos veces
	select {
	case <-started:
		t.Fatal("started with 1 of 2 shares")
	case <-time.After(30 * time.Millisecond):
	}
	sh.noteAttached(1)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("did not start with its 2 shares")
	}
}

func TestStartAfterSharesArrancaAlVencerElPlazo(t *testing.T) {
	sh := newShares()
	if sh.waitAttached(1, time.Millisecond, 3) {
		t.Fatal("waitAttached without shares said they arrived")
	}
	sh.noteAttached(3)
	if !sh.waitAttached(1, time.Millisecond, 3) {
		t.Fatal("waitAttached with the share attached said it did not arrive")
	}
}
