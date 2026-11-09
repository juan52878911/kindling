package machine

import (
	"testing"

	hostshare "github.com/juan52878911/kindling/internal/share"
)

// 0 es el valor por defecto y un negativo quita el tope.
func TestShareLimits(t *testing.T) {
	got := ShareConfig{}.shareLimits()
	want := hostshare.Limits{MaxBytes: DefaultShareMaxBytes, MaxFiles: DefaultShareMaxFiles, MinFree: DefaultShareMinFree}
	if got != want {
		t.Fatalf("defaults %+v, want %+v", got, want)
	}
	got = ShareConfig{MaxBytes: -1, MaxFiles: 10, MinFreeBytes: -1}.shareLimits()
	if want := (hostshare.Limits{MaxFiles: 10}); got != want {
		t.Fatalf("%+v, want %+v", got, want)
	}
}
