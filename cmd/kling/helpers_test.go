package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestHelpDeComandoDesconocido(t *testing.T) {
	err := cmdHelp([]string{"definitely-not-a-command"})
	if err == nil || hintFor(err) != "kling help all" || codigoDeSalida(err) != 2 {
		t.Fatalf("%v / %q", err, hintFor(err))
	}
}

func TestWriteVersion(t *testing.T) {
	var b bytes.Buffer
	_ = writeVersion(&b, "v0.13.0", "/run/kling.sock", &api.Info{Version: "0.13.0", Firecrack: "Firecracker v1.17.0"}, []string{"v0.20.0"}, nil, false)
	if b.String() != "kling v0.13.0\ndaemon: 0.13.0 (/run/kling.sock)\napi:    1 (daemon 1)\nvmm:    firecracker Firecracker v1.17.0\nagent:  v0.20.0 (as announced by its machines)\n" {
		t.Fatalf("%q", b.String())
	}
	b.Reset()
	_ = writeVersion(&b, "v0.13.0", "/run/kling.sock", &api.Info{Version: "0.12.0", Backend: "vz"}, nil, nil, false)
	if !strings.Contains(b.String(), "note: CLI and daemon differ") || !strings.Contains(b.String(), "vmm:    vz\n") || strings.Contains(b.String(), "agent:") {
		t.Fatalf("desfase sin avisar: %q", b.String())
	}
	b.Reset()
	_ = writeVersion(&b, "v0.13.0", "ssh://x", nil, nil, errors.New("down"), false)
	if !strings.HasPrefix(b.String(), "kling v0.13.0\n") || !strings.Contains(b.String(), "not reachable at ssh://x") {
		t.Fatalf("%q", b.String())
	}
	b.Reset()
	_ = writeVersion(&b, "v0.13.0", "s", &api.Info{Version: "0.13.0", API: 1, Backend: "hv", Firecrack: "kling-hv 0.20"}, []string{"a", "b"}, nil, true)
	var v versionJSON
	if err := json.Unmarshal(b.Bytes(), &v); err != nil || v.CLI != "v0.13.0" || v.Daemon != "0.13.0" || v.Endpoint != "s" ||
		v.API != api.APIVersion || v.DaemonAPI != 1 || v.VMM != "hv" || v.VMMVersion != "kling-hv 0.20" || len(v.Agents) != 2 {
		t.Fatalf("%+v %v", v, err)
	}
}
