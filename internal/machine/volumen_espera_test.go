package machine

import (
	"errors"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestValidarVolumeWait(t *testing.T) {
	vol := []api.VolumeAttachment{{Name: "pgdata", Mount: "/var/lib/postgresql/data"}}
	if err := validarVolumeWait(api.RunRequest{Image: "x", Volumes: vol, VolumeWait: true}); err != nil {
		t.Fatal(err)
	}
	if err := validarVolumeWait(api.RunRequest{Image: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := validarVolumeWait(api.RunRequest{Image: "x", VolumeWait: true}); !errors.Is(err, ErrShareRequest) {
		t.Fatalf("sin volúmenes: %v", err)
	}
	if err := validarVolumeWait(api.RunRequest{From: "t", Volumes: vol, VolumeWait: true}); !errors.Is(err, ErrShareRequest) {
		t.Fatalf("con From: %v", err)
	}
	if volWaitBootArg(false) != "" || volWaitBootArg(true) != " kling.volwait=1" {
		t.Fatal("boot arg")
	}
	m := &Manager{byID: map[string]*api.Machine{"a": {VolumeWait: true}, "b": {}}}
	if !m.esperaVolumenDe("a") || m.esperaVolumenDe("b") || m.esperaVolumenDe("c") {
		t.Fatal("esperaVolumenDe")
	}
}
